#!/usr/bin/env node
/* ===== 网页界面冒烟自检 =====
 *
 * 用无头 Edge/Chrome 打开本地页面，真的填地址、真的点「解析视频」，
 * 然后断言解析结果卡片渲染出了足够的内容并截图。
 *
 * 为什么不引第三方库：CDP 就是一个 WebSocket 上的 JSON-RPC，Node 22 自带
 * WebSocket 客户端，几十行就能驱动。为一次冒烟检查装 500MB 的 Chromium
 * 不划算 —— 本机本来就有 Edge。
 *
 * 用法：
 *   node tools/uitest.mjs [地址] [截图.png] [下载后截图.png] [--download]
 * 前提：本地服务已经在跑（BBDown原生版.exe -no-open 或直接双击）。
 *
 * 不带 --download 只验证解析与渲染（几秒）；带上则会真的点「开始下载」，
 * 跑完整的 取流 → 下载 → 封装 流程，等到产物列出来为止。
 */

import { spawn, execSync } from "node:child_process";
import { writeFileSync, mkdirSync } from "node:fs";
import path from "node:path";

const argv = process.argv.slice(2);
const DO_DOWNLOAD = argv.includes("--download");
const positional = argv.filter((a) => !a.startsWith("--"));
const URL_ARG = positional[0] || "http://127.0.0.1:18230/?theme=light";
const OUT_PNG = path.resolve(positional[1] || ".devdata/ui-parse.png");
const OUT_DL = path.resolve(positional[2] || ".devdata/ui-download.png");
const PORT = 9333;

const BROWSERS = [
  "C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe",
  "C:/Program Files/Microsoft/Edge/Application/msedge.exe",
  "C:/Program Files/Google/Chrome/Application/chrome.exe",
  "C:/Program Files (x86)/Google/Chrome/Application/chrome.exe",
];

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function findBrowser() {
  for (const p of BROWSERS) {
    try { execSync(`"${p}" --version`, { stdio: "ignore" }); return p; } catch { /* 试下一个 */ }
  }
  throw new Error("没找到 Edge/Chrome");
}

/* ---------- 极简 CDP 客户端 ---------- */

class CDP {
  constructor(ws) { this.ws = ws; this.id = 0; this.waiters = new Map(); this.events = new Map(); }

  static async connect(url) {
    const ws = new WebSocket(url);
    await new Promise((res, rej) => {
      ws.addEventListener("open", res, { once: true });
      ws.addEventListener("error", () => rej(new Error("CDP 连接失败")), { once: true });
    });
    const c = new CDP(ws);
    ws.addEventListener("message", (ev) => c._onMessage(JSON.parse(ev.data)));
    return c;
  }

  _onMessage(msg) {
    if (msg.id && this.waiters.has(msg.id)) {
      const { res, rej } = this.waiters.get(msg.id);
      this.waiters.delete(msg.id);
      msg.error ? rej(new Error(msg.error.message)) : res(msg.result);
      return;
    }
    if (msg.method) {
      const list = this.events.get(msg.method);
      if (list) list.forEach((fn) => fn(msg.params));
    }
  }

  send(method, params = {}) {
    const id = ++this.id;
    return new Promise((res, rej) => {
      this.waiters.set(id, { res, rej });
      this.ws.send(JSON.stringify({ id, method, params }));
      setTimeout(() => {
        if (this.waiters.delete(id)) rej(new Error(`${method} 超时`));
      }, 60000);
    });
  }

  /** 在页面里跑一段表达式并取回结果。 */
  async eval(expr) {
    const r = await this.send("Runtime.evaluate", {
      expression: expr, returnByValue: true, awaitPromise: true,
    });
    if (r.exceptionDetails) {
      throw new Error("页面内抛出异常：" + (r.exceptionDetails.exception?.description || r.exceptionDetails.text));
    }
    return r.result.value;
  }

  close() { try { this.ws.close(); } catch { /* 忽略 */ } }
}

/* ---------- 主流程 ---------- */

const browserPath = findBrowser();
const profile = path.resolve(".devdata/uitest-profile");
mkdirSync(path.dirname(OUT_PNG), { recursive: true });

const child = spawn(browserPath, [
  "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
  `--remote-debugging-port=${PORT}`, `--user-data-dir=${profile}`,
  "--window-size=1400,1000", "about:blank",
], { stdio: "ignore" });

const cleanup = () => {
  try { execSync(`taskkill /F /T /PID ${child.pid}`, { stdio: "ignore" }); } catch { /* 已退出 */ }
};
process.on("exit", cleanup);

let cdp = null;
const fail = (msg) => { console.error("✗ " + msg); cleanup(); process.exit(1); };

try {
  // 等调试端口起来
  let target = null;
  for (let i = 0; i < 60; i++) {
    try {
      const list = await (await fetch(`http://127.0.0.1:${PORT}/json/list`)).json();
      target = list.find((t) => t.type === "page");
      if (target) break;
    } catch { /* 还没起来 */ }
    await sleep(300);
  }
  if (!target) fail("浏览器调试端口没起来");

  cdp = await CDP.connect(target.webSocketDebuggerUrl);
  await cdp.send("Page.enable");
  await cdp.send("Runtime.enable");
  await cdp.send("Emulation.setDeviceMetricsOverride",
    { width: 1400, height: 1000, deviceScaleFactor: 1, mobile: false });

  await cdp.send("Page.navigate", { url: URL_ARG });
  for (let i = 0; i < 80; i++) {
    if (await cdp.eval("document.readyState") === "complete") break;
    await sleep(150);
  }
  await sleep(700); // 让 loadStatus / pollTask 先跑一轮

  // 1) 页面骨架
  const shell = await cdp.eval(`JSON.stringify({
    title: document.title,
    hasConsole: !!document.getElementById("console"),
    css: !!document.querySelector('link[href="/static/style.css"]')
      && getComputedStyle(document.documentElement).getPropertyValue("--accent").trim(),
  })`);
  const shellInfo = JSON.parse(shell);
  if (!shellInfo.hasConsole) fail("页面缺少日志框");
  if (!shellInfo.css) fail("style.css 没生效（--accent 取不到值）");

  // 2) 服务连通性：任务台应当已经拿到过 /api/task
  const alive = await cdp.eval(
    `document.getElementById("task-state").textContent + "|" +
     document.getElementById("login-detail").textContent`);
  if (!alive) fail("任务台状态没渲染");

  // 3) 真的填地址并点解析
  await cdp.eval(`(() => {
    const ta = document.getElementById("urls");
    ta.value = "BV1GJ411x7h7";
    ta.dispatchEvent(new Event("input", { bubbles: true }));
    document.getElementById("btn-parse").click();
    return true;
  })()`);

  let shown = false;
  for (let i = 0; i < 160; i++) {
    const cls = await cdp.eval(`document.getElementById("card-info").className`);
    if (!cls.includes("hidden")) { shown = true; break; }
    await sleep(250);
  }
  if (!shown) fail("点了「解析视频」之后，解析结果卡片一直没有出现");

  await sleep(400);

  // 4) 校验渲染出来的结构
  const summary = JSON.parse(await cdp.eval(`JSON.stringify({
    items: document.querySelectorAll("#info-list .item-block").length,
    titles: [...document.querySelectorAll("#info-list .media-title")].map(e => e.textContent.trim()),
    pageRows: document.querySelectorAll("#info-list .pages label").length,
    checkedRows: document.querySelectorAll('#info-list .pages input[type=checkbox]:checked').length,
    qualities: [...document.querySelectorAll("#info-list .qtag")].map(e => e.textContent),
    qualityOptions: [...document.getElementById("quality").options].map(o => o.textContent),
    hint: document.getElementById("info-hint").textContent,
    covers: [...document.querySelectorAll("#info-list .media-cover")].map(e => ({
      src: e.src.slice(0, 60), loaded: e.naturalWidth > 0, hidden: e.style.display === "none",
    })),
    consoleLines: document.getElementById("console").childNodes.length,
    lastLog: (document.getElementById("console").lastChild || {}).textContent || "",
  })`));

  const problems = [];
  if (summary.items < 1) problems.push("没有渲染出任何目标卡片");
  if (summary.pageRows < 1) problems.push("没有渲染出分P列表");
  if (summary.checkedRows !== summary.pageRows) problems.push("分P默认没有全选");
  if (summary.qualityOptions.length < 2) problems.push("画质下拉没有按解析结果填充");
  if (summary.consoleLines < 1) problems.push("日志框没有任何内容");

  // 5) 截图（captureBeyondViewport 抓整页）
  const shot = await cdp.send("Page.captureScreenshot",
    { format: "png", captureBeyondViewport: true });
  writeFileSync(OUT_PNG, Buffer.from(shot.data, "base64"));

  console.log("页面标题   :", shellInfo.title);
  console.log("解析目标数 :", summary.items);
  summary.titles.forEach((t) => console.log("  ·", t));
  console.log("分P行数    :", summary.pageRows, "（已全选", summary.checkedRows + "）");
  console.log("可选画质   :", summary.qualities.join(" / ") || "(无)");
  console.log("画质下拉   :", summary.qualityOptions.join(" | "));
  console.log("封面       :", summary.covers.length
    ? summary.covers.map((c) => (c.loaded ? "已加载" : c.hidden ? "加载失败(已隐藏)" : "加载中")).join(", ")
    : "(无封面字段)");
  console.log("结果提示   :", summary.hint);
  console.log("日志行数   :", summary.consoleLines);
  console.log("日志末行   :", summary.lastLog.trim());
  console.log("截图       :", OUT_PNG);

  // 6) 可选：真的点一次「开始下载」，走完 取流 → 下载 → 封装
  if (DO_DOWNLOAD && !problems.length) {
    console.log("\n--- 开始下载（真实链路）---");
    await cdp.eval(`document.getElementById("btn-download").click(); true`);

    const done = { 已完成: 1, 有失败: 1, 已停止: 1 };
    let state = "";
    for (let i = 0; i < 1200; i++) { // 最多等 10 分钟
      state = await cdp.eval(`document.getElementById("task-state").textContent.trim()`);
      if (done[state]) break;
      if (i % 20 === 0) {
        const pct = await cdp.eval(`document.getElementById("task-pct").textContent`);
        const stage = await cdp.eval(`document.getElementById("task-stage").textContent`);
        console.log(`  [${state}] ${pct}  ${stage}`);
      }
      await sleep(500);
    }

    await sleep(500);
    const result = JSON.parse(await cdp.eval(`JSON.stringify({
      state: document.getElementById("task-state").textContent.trim(),
      pct: document.getElementById("task-pct").textContent,
      stage: document.getElementById("task-stage").textContent,
      steps: [...document.querySelectorAll("#task-steps .pstep")]
        .map(e => e.dataset.s + "=" + (e.className.replace("pstep", "").trim() || "-")),
      outputs: [...document.querySelectorAll("#task-outputs .output-row")].map(e => e.textContent.trim()),
      tail: [...document.getElementById("console").childNodes].slice(-6).map(e => e.textContent.trim()),
    })`));

    const dlShot = await cdp.send("Page.captureScreenshot",
      { format: "png", captureBeyondViewport: true });
    writeFileSync(OUT_DL, Buffer.from(dlShot.data, "base64"));

    console.log("最终状态   :", result.state, "|", result.pct);
    console.log("阶段       :", result.stage);
    console.log("四步进度   :", result.steps.join("  "));
    result.outputs.forEach((o) => console.log("产物       :", o));
    console.log("日志末尾   :");
    result.tail.forEach((t) => console.log("   ", t));
    console.log("截图       :", OUT_DL);

    if (result.state !== "已完成") problems.push("下载没有正常完成，最终状态是 " + result.state);
    if (!result.outputs.length) problems.push("任务完成后没有列出任何产物文件");
  }

  cdp.close();
  cleanup();
  if (problems.length) {
    console.error("\n✗ 界面自检未通过：");
    problems.forEach((p) => console.error("  - " + p));
    process.exit(1);
  }
  console.log("\n✓ 界面自检通过");
  process.exit(0);
} catch (e) {
  fail(e.message);
}
