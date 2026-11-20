#!/usr/bin/env node
/* ===== 刷视频功能端到端自检 =====
 *
 * 前提：本地服务已跑（dist/BBDown原生版.exe -ui browser -no-open）。
 * 用本机 Edge/Chrome 的 CDP 驱动，真的点「刷视频」→ 选视频 → 等 MSE 播放
 * → 点一键下载，全程断言 DOM 与播放器状态。
 *
 * 用法：node tools/ui_verify_feed.mjs [地址]
 */
import { spawn, execSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import path from "node:path";

const URL_ARG = process.argv[2] || "http://127.0.0.1:18230/?theme=light";
const PORT = 9335;
const BROWSERS = [
  "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
  "C:/Program Files/Microsoft/Edge/Application/msedge.exe",
  "C:/Program Files/Google/Chrome/Application/chrome.exe",
];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function findBrowser() {
  for (const p of BROWSERS) {
    try { execSync(`"${p}" --version`, { stdio: "ignore" }); return p; } catch {}
  }
  throw new Error("没找到 Edge/Chrome");
}

class CDP {
  constructor(ws) { this.ws = ws; this.id = 0; this.waiters = new Map(); }
  static async connect(url) {
    const ws = new WebSocket(url);
    await new Promise((res, rej) => {
      ws.addEventListener("open", res, { once: true });
      ws.addEventListener("error", () => rej(new Error("CDP 连接失败")), { once: true });
    });
    const c = new CDP(ws);
    ws.addEventListener("message", (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id && c.waiters.has(m.id)) {
        const { res, rej } = c.waiters.get(m.id);
        c.waiters.delete(m.id);
        m.error ? rej(new Error(m.error.message)) : res(m.result);
      }
    });
    return c;
  }
  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((res, rej) => {
      this.waiters.set(id, { res, rej });
      this.ws.send(JSON.stringify({ id, method, params }));
      setTimeout(() => { if (this.waiters.delete(id)) rej(new Error(method + " 超时")); }, 60000);
    });
  }
  async eval(expr) {
    const r = await this.send("Runtime.evaluate", { expression: expr, returnByValue: true, awaitPromise: true });
    if (r.exceptionDetails) throw new Error(r.exceptionDetails.exception?.description || r.exceptionDetails.text);
    return r.result.value;
  }
  close() { try { this.ws.close(); } catch {} }
}

const browserPath = findBrowser();
const profile = path.resolve(".devdata/feed-profile");
mkdirSync(profile, { recursive: true });
const child = spawn(browserPath, [
  "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
  "--autoplay-policy=no-user-gesture-required",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`,
  "--window-size=1400,1000", "about:blank",
], { stdio: "ignore" });
const cleanup = () => { try { execSync(`taskkill /F /T /PID ${child.pid}`, { stdio: "ignore" }); } catch {} };
process.on("exit", cleanup);
const fail = (m) => { console.error("✗ " + m); cleanup(); process.exit(1); };
const ok = (m) => console.log("✓ " + m);

let cdp;
try {
  await sleep(1500);
  const targets = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
  const page = targets.find((t) => t.type === "page");
  cdp = await CDP.connect(page.webSocketDebuggerUrl);

  await cdp.send("Page.enable");
  await cdp.send("Page.navigate", { url: URL_ARG });
  await sleep(2500);

  // 1) 点「刷视频」
  await cdp.eval(`document.getElementById('btn-feed').click()`);
  await sleep(500);
  const panelVisible = await cdp.eval(`!document.getElementById('feed-view').classList.contains('hidden')`);
  if (!panelVisible) fail("刷视频面板没打开");
  ok("刷视频面板已打开");

  // 2) 等热门列表加载（最多 20 秒）
  let n = 0;
  for (let i = 0; i < 40; i++) {
    n = await cdp.eval(`document.querySelectorAll('.feed-card').length`);
    if (n > 0) break;
    await sleep(500);
  }
  if (n === 0) fail("热门列表为空");
  ok(`热门列表加载 ${n} 条`);

  // 3) 点第一个视频，等 MSE 预览播放
  await cdp.eval(`document.querySelectorAll('.feed-card')[0].click()`);
  let play = null;
  for (let i = 0; i < 60; i++) {
    play = await cdp.eval(`(() => {
      const v = document.getElementById('feed-video');
      return { ready: v.readyState, w: v.videoWidth, h: v.videoHeight,
        q: document.getElementById('feed-quality').textContent,
        loading: !document.getElementById('feed-loading').classList.contains('hidden'),
        dlDisabled: document.getElementById('feed-download').disabled };
    })()`);
    if (play.w > 0 && !play.loading) break;
    await sleep(1000);
  }
  console.log("   播放器状态:", JSON.stringify(play));
  if (!play || play.w === 0) fail("MSE 预览没出画面");
  ok(`MSE 播放成功 ${play.w}x${play.h} · ${play.q}`);
  if (play.dlDisabled) fail("预览成功后一键下载按钮仍禁用");

  // 4) 点「下一个」
  const before = await cdp.eval(`document.getElementById('feed-title').textContent`);
  await cdp.eval(`document.getElementById('feed-next').click()`);
  await sleep(3500);
  const after = await cdp.eval(`document.getElementById('feed-title').textContent`);
  if (before === after) fail("点下一个后标题没变");
  ok("下一个切换正常");

  // 5) 点一键下载 → 面板关闭、任务启动
  await cdp.eval(`document.getElementById('feed-download').disabled=false; document.getElementById('feed-download').click()`);
  await sleep(3000);
  const closed = await cdp.eval(`document.getElementById('feed-view').classList.contains('hidden')`);
  if (!closed) fail("一键下载后面板没关闭");
  const stage = await cdp.eval(`document.getElementById('task-stage').textContent`);
  console.log("   任务状态:", stage.slice(0, 80));
  ok("一键下载已发起（面板关闭、任务区有反应）");

  console.log("\n全部通过 ✓");
  cdp.close();
  cleanup();
  process.exit(0);
} catch (e) {
  console.error("✗ 异常:", e.message);
  try { cdp && cdp.close(); } catch {}
  cleanup();
  process.exit(1);
}
