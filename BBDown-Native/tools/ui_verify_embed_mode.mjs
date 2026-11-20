#!/usr/bin/env node
/* ===== 内嵌页签模式自检 =====
 *
 * 前提：本地服务已跑（dist/BBDown原生版.exe -ui browser -no-open）。
 *
 * 验的是这轮修的两件事：
 *   1) ?embed=feed 下「✕ 关闭」必须收起来 —— 原生窗口有页签条，点了只会
 *      露出网页版下载界面，用户以为刷视频页崩了；
 *   2) 即使绕过按钮直接调 feedClose()，面板也不能塌（双重保险）。
 *
 * 用法：node tools/ui_verify_embed_mode.mjs [地址]
 */
import { spawn, execSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import path from "node:path";

const BASE = (process.argv[2] || "http://127.0.0.1:18230/").replace(/\/?$/, "/");
const PORT = 9336;
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
const profile = path.resolve(".devdata/embed-profile");
mkdirSync(profile, { recursive: true });
const child = spawn(browserPath, [
  "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
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

  // ---------- 内嵌刷视频页 ----------
  await cdp.send("Page.navigate", { url: BASE + "?embed=feed" });
  await sleep(2500);

  if (!(await cdp.eval(`document.body.classList.contains('embed-feed')`))) {
    fail("body 上没有 embed-feed 标记");
  }
  ok("内嵌刷视频页已带 embed-feed 标记");

  if (!(await cdp.eval(`!document.getElementById('feed-view').classList.contains('hidden')`))) {
    fail("刷视频面板没自动展开");
  }
  ok("刷视频面板自动展开");

  const disp = await cdp.eval(`getComputedStyle(document.getElementById('feed-close')).display`);
  if (disp !== "none") fail(`「✕ 关闭」没被收起，display=${disp}`);
  ok("「✕ 关闭」已收起");

  // 绕过按钮直接调函数：这是用户「点了关闭露出下载页」的那条老路
  await cdp.eval(`window.feedClose && window.feedClose()`);
  await sleep(300);
  if (await cdp.eval(`document.getElementById('feed-view').classList.contains('hidden')`)) {
    fail("feedClose() 在内嵌模式下仍把面板关了");
  }
  ok("feedClose() 在内嵌模式下被挡住");

  // ---------- 普通浏览器页：关闭键必须还在 ----------
  await cdp.send("Page.navigate", { url: BASE + "?theme=light" });
  await sleep(2500);
  await cdp.eval(`document.getElementById('btn-feed').click()`);
  await sleep(600);
  const disp2 = await cdp.eval(`getComputedStyle(document.getElementById('feed-close')).display`);
  if (disp2 === "none") fail("浏览器模式下「✕ 关闭」不该被收起");
  ok("浏览器模式下「✕ 关闭」仍然可用");

  await cdp.eval(`document.getElementById('feed-close').click()`);
  await sleep(300);
  if (!(await cdp.eval(`document.getElementById('feed-view').classList.contains('hidden')`))) {
    fail("浏览器模式下点关闭没关掉面板");
  }
  ok("浏览器模式下关闭键行为不变");

  console.log("ALL PASS");
  cdp.close();
  cleanup();
  process.exit(0);
} catch (e) {
  fail(e.message);
}
