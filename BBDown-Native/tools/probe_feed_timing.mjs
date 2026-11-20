#!/usr/bin/env node
/* 刷视频首帧耗时拆解（诊断用，不参与验收）：
 * 打开刷视频 → 点第一支 → 轮询到出画面，然后打印资源瀑布，
 * 用来判断时间花在 /api/preview、/api/stream 首字节还是 MSE 解码上。
 *
 * 用法：node tools/probe_feed_timing.mjs [地址]
 */
import { spawn, execSync } from "node:child_process";
import { mkdirSync } from "node:fs";
import path from "node:path";

const URL_ARG = process.argv[2] || "http://127.0.0.1:18230/?theme=light";
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

const profile = path.resolve(".devdata/probe-profile");
mkdirSync(profile, { recursive: true });
const child = spawn(findBrowser(), [
  "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
  "--autoplay-policy=no-user-gesture-required",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`,
  "--window-size=1400,1000", "about:blank",
], { stdio: "ignore" });
const cleanup = () => { try { execSync(`taskkill /F /T /PID ${child.pid}`, { stdio: "ignore" }); } catch {} };
process.on("exit", cleanup);

let cdp;
try {
  await sleep(1500);
  const targets = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
  const page = targets.find((t) => t.type === "page");
  cdp = await CDP.connect(page.webSocketDebuggerUrl);
  await cdp.send("Page.enable");
  await cdp.send("Page.navigate", { url: URL_ARG });
  await sleep(2500);

  await cdp.eval(`document.getElementById('btn-feed').click()`);
  let n = 0;
  for (let i = 0; i < 40; i++) {
    n = await cdp.eval(`document.querySelectorAll('.feed-card').length`);
    if (n > 0) break;
    await sleep(250);
  }
  console.log("列表条数:", n);

  const t0 = Date.now();
  await cdp.eval(`document.querySelectorAll('.feed-card')[0].click()`);
  let info = null;
  for (let i = 0; i < 300; i++) {
    info = await cdp.eval(`(() => {
      const v = document.getElementById('feed-video');
      const ms = window.__ms || null;
      return { w: v.videoWidth, rs: v.readyState, ct: v.currentTime,
        loading: !document.getElementById('feed-loading').classList.contains('hidden'),
        paused: v.paused };
    })()`);
    if (info.w > 0 && !info.loading) break;
    await sleep(200);
  }
  console.log("首帧耗时:", Date.now() - t0, "ms", JSON.stringify(info));

  // 资源瀑布：只挑预览相关的请求
  const res = await cdp.eval(`performance.getEntriesByType('resource')
    .filter(e => /preview|stream/.test(e.name))
    .map(e => ({ n: e.name.replace(/^.*\\//,'').slice(0,24), start: Math.round(e.startTime),
      ttfb: Math.round(e.responseStart), dur: Math.round(e.duration),
      bytes: e.transferSize }))`);
  console.log("资源瀑布:");
  for (const r of res) console.log("  ", JSON.stringify(r));

  console.log("播放器缓冲:", JSON.stringify(await cdp.eval(`(() => {
    const v = document.getElementById('feed-video');
    const b = v.buffered;
    return { ct: +v.currentTime.toFixed(2), ranges: b.length,
      end: b.length ? +b.end(b.length-1).toFixed(2) : 0 };
  })()`)));

  // 连续播 10 秒，确认边下边播不会中途断流（背压把缓冲维持住）
  await cdp.eval(`document.getElementById('feed-video').play()`);
  await sleep(10000);
  console.log("10 秒后:", JSON.stringify(await cdp.eval(`(() => {
    const v = document.getElementById('feed-video');
    const b = v.buffered;
    return { ct: +v.currentTime.toFixed(2), paused: v.paused, ended: v.ended,
      err: v.error ? v.error.code : null, end: b.length ? +b.end(b.length-1).toFixed(2) : 0 };
  })()`)));
  cdp.close();
  cleanup();
  process.exit(0);
} catch (e) {
  console.error("异常:", e.message);
  try { cdp && cdp.close(); } catch {}
  cleanup();
  process.exit(1);
}
