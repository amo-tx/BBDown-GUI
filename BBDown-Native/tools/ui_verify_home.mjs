#!/usr/bin/env node
/* ===== 首页（分区浏览 + 播放层）端到端自检 =====
 *
 * 前提：本地服务已跑（dist/BBDown原生版.exe -ui browser -no-open）。
 *
 * 验的是这一页是「我们自己的界面」而不是嵌了第三方页面：
 *   1) ?embed=home 自动展开首页，分类导航与封面瀑布流都渲染出来；
 *   2) 点卡片 → 播放层出现，MSE 真的出画（readyState>=2 且 currentTime 在走）；
 *   3) 画质/UP/标题回填，「接下来」可切换，Esc 能关掉；
 *   4) 换分类后列表确实换了内容。
 *
 * 用法：node tools/ui_verify_home.mjs [地址]
 */
import { spawn, execSync } from "node:child_process";
import { mkdirSync, writeFileSync } from "node:fs";
import path from "node:path";

const BASE = (process.argv[2] || "http://127.0.0.1:18230/").replace(/\/?$/, "/");
const PORT = 9337;
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
const profile = path.resolve(".devdata/home-profile");
mkdirSync(profile, { recursive: true });
const child = spawn(browserPath, [
  "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
  "--autoplay-policy=no-user-gesture-required",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`,
  "--window-size=1500,1000", "about:blank",
], { stdio: "ignore" });
const cleanup = () => { try { execSync(`taskkill /F /T /PID ${child.pid}`, { stdio: "ignore" }); } catch {} };
process.on("exit", cleanup);
const fail = (m) => { console.error("✗ " + m); cleanup(); process.exit(1); };
const ok = (m) => console.log("✓ " + m);

async function waitFor(cdp, expr, ms, label) {
  const end = Date.now() + ms;
  for (;;) {
    if (await cdp.eval(expr)) return true;
    if (Date.now() > end) return false;
    await sleep(300);
  }
}

let cdp;
try {
  await sleep(1500);
  const targets = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
  const page = targets.find((t) => t.type === "page");
  cdp = await CDP.connect(page.webSocketDebuggerUrl);
  await cdp.send("Page.enable");
  await cdp.send("Page.navigate", { url: BASE + "?embed=home" });
  await sleep(3000);

  // ---------- 页面骨架 ----------
  if (!(await cdp.eval(`!document.getElementById('home-view').classList.contains('hidden')`)))
    fail("首页视图没自动展开");

  const tabs = await cdp.eval(`document.querySelectorAll('#home-tabs .home-tab').length`);
  if (tabs < 8) fail(`分类导航只有 ${tabs} 个`);
  ok(`首页自动展开，分类 ${tabs} 个`);

  const firstLoad = await waitFor(cdp, `document.querySelectorAll('#home-grid .home-card').length >= 20`, 25000, "卡片");
  if (!firstLoad) {
    const hint = await cdp.eval(`document.getElementById('home-hint').textContent`);
    fail("首屏没有卡片：" + (hint || "（无提示）"));
  }
  const cards = await cdp.eval(`document.querySelectorAll('#home-grid .home-card').length`);
  ok(`封面瀑布流渲染 ${cards} 张`);

  // 卡片必须是自己的 DOM（不是 iframe / 外站）
  if (await cdp.eval(`!!document.querySelector('#home-view iframe')`)) fail("首页里出现了 iframe");
  const cover = await cdp.eval(`document.querySelector('#home-grid .home-card img')?.src || ''`);
  if (!/^https:\/\/i\d\.hdslb\.com\//.test(cover)) fail("封面地址不对：" + cover);
  ok("卡片是自绘 DOM，封面直连图床");

  // ---------- 点卡片 → 播放层 ----------
  const firstBvid = await cdp.eval(`home.items[0].bvid`);
  await cdp.eval(`document.querySelector('#home-grid .home-card').click()`);
  if (await cdp.eval(`document.getElementById('play-layer').classList.contains('hidden')`))
    fail("点卡片没打开播放层");
  ok("点卡片打开播放层");

  const started = await waitFor(
    cdp,
    `(() => { const v = document.getElementById('play-video'); return v.readyState >= 2 && v.currentTime > 0.2; })()`,
    45000, "首帧",
  );
  const st = await cdp.eval(`(() => { const v = document.getElementById('play-video'); return {ready:v.readyState,w:v.videoWidth,h:v.videoHeight,t:v.currentTime}; })()`);
  if (!started) fail("播放层没出画面：" + JSON.stringify(st));
  ok(`播放层出画 ${st.w}x${st.h}，已播 ${st.t.toFixed(2)}s`);
  if (!st.w || !st.h) fail("videoWidth/Height 为 0");

  const q = await cdp.eval(`document.getElementById('play-quality').textContent`);
  if (!q || q === "取流中…" || q === "—") fail("画质没回填：" + q);
  ok("画质：" + q);
  if (await cdp.eval(`document.getElementById('play-download').disabled`))
    fail("播放层的下载按钮没启用");

  const upnext = await cdp.eval(`document.querySelectorAll('#play-upnext .upnext-item').length`);
  if (upnext === 0) fail("「接下来」是空的");
  ok(`「接下来」${upnext} 条`);

  // ---------- 切换到下一支 ----------
  const secondBvid = await cdp.eval(`home.items[1].bvid`);
  await cdp.eval(`document.getElementById('play-next').click()`);
  const switched = await waitFor(
    cdp,
    `(() => { const v = document.getElementById('play-video'); return v.readyState >= 2 && v.currentTime > 0.1; })()`,
    45000, "下一支",
  );
  if (!switched) fail("点「下一个」没播起来");
  ok("「下一个」切换后正常播放（" + firstBvid + " → " + secondBvid + "）");

  // 播放层观感：留一张图方便肉眼核对布局
  {
    await sleep(600);
    const shot = await cdp.send("Page.captureScreenshot", { format: "png" });
    const out = path.resolve("_shots", "home_play.png");
    mkdirSync(path.dirname(out), { recursive: true });
    writeFileSync(out, Buffer.from(shot.data, "base64"));
    console.log("  截图 → " + out);
  }

  // ---------- Esc 关闭 ----------
  await cdp.send("Input.dispatchKeyEvent", { type: "keyDown", key: "Escape", code: "Escape", windowsVirtualKeyCode: 27 });
  await sleep(400);
  if (!(await cdp.eval(`document.getElementById('play-layer').classList.contains('hidden')`)))
    fail("Esc 没关掉播放层");
  const stopped = await cdp.eval(`document.getElementById('play-video').getAttribute('src') === null`);
  if (!stopped) fail("关闭播放层后 video 还挂着流");
  ok("Esc 关闭播放层并释放流");

  // ---------- 换分类 ----------
  const before = await cdp.eval(`home.items.slice(0,5).map(i => i.bvid).join(',')`);
  await cdp.eval(`[...document.querySelectorAll('#home-tabs .home-tab')].find(b => b.textContent === '动画').click()`);
  const tabLoaded = await waitFor(
    cdp,
    `(() => { const b = [...document.querySelectorAll('#home-tabs .home-tab')].find(x => x.textContent === '动画'); return b.classList.contains('active') && home.items.length > 0 && !home.loading; })()`,
    30000, "换分类",
  );
  if (!tabLoaded) fail("切到「动画」没加载出内容");
  await sleep(800);
  const after = await cdp.eval(`home.items.slice(0,5).map(i => i.bvid).join(',')`);
  if (before === after) fail("换分类后内容没变");
  ok("换分类生效（内容已更换）");

  const shot = await cdp.send("Page.captureScreenshot", { format: "png" });
  const out = path.resolve("_shots", "home_page.png");
  mkdirSync(path.dirname(out), { recursive: true });
  writeFileSync(out, Buffer.from(shot.data, "base64"));
  console.log("  截图 → " + out);

  console.log("ALL PASS");
  cdp.close();
  cleanup();
  process.exit(0);
} catch (e) {
  fail(e.message);
}
