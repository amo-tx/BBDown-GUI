/* ===== BBDown 原生版 · 前端 =====
   后端是本地 Go 服务（只监听 127.0.0.1），所有写操作都要带 X-BBDown-Native 头：
   跨站请求带上自定义头会触发 CORS 预检，而服务端从不返回 CORS 响应头，
   于是浏览器自己就把请求拦下来了。这就是本文件里 HEADERS 的用处。 */
"use strict";

const HEADERS = { "X-BBDown-Native": "1", "Content-Type": "application/json" };
const BLANK_IMG = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7";

const $ = (id) => document.getElementById(id);

/* ---------- 小工具 ---------- */

async function api(path, body) {
  const opt = { method: body === undefined ? "GET" : "POST", headers: HEADERS };
  if (body !== undefined) opt.body = JSON.stringify(body || {});
  const r = await fetch(path, opt);
  const txt = await r.text();
  let data = null;
  if (txt) { try { data = JSON.parse(txt); } catch (e) { data = null; } }
  if (!r.ok) throw new Error((data && data.error) || ("请求失败 HTTP " + r.status));
  return data || {};
}

function esc(s) {
  return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => (
    { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
  ));
}

function fmtBytes(n) {
  if (!n || n <= 0) return "--";
  const u = ["B", "KB", "MB", "GB", "TB"];
  let i = 0, v = n;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 || i === 0 ? 0 : 1) + " " + u[i];
}

function fmtSpeed(v) { return v > 0 ? fmtBytes(v) + "/s" : "--"; }

function fmtEta(sec) {
  if (!sec || sec <= 0 || !isFinite(sec)) return "--";
  const s = Math.round(sec);
  if (s < 60) return s + " 秒";
  if (s < 3600) return Math.floor(s / 60) + " 分 " + (s % 60) + " 秒";
  return Math.floor(s / 3600) + " 时 " + Math.floor((s % 3600) / 60) + " 分";
}

function fmtDur(sec) {
  sec = Math.round(sec || 0);
  const h = Math.floor(sec / 3600), m = Math.floor(sec / 60) % 60, s = sec % 60;
  const p = (n) => String(n).padStart(2, "0");
  return h > 0 ? h + ":" + p(m) + ":" + p(s) : m + ":" + p(s);
}

let toastSeq = 0;
function toast(msg, kind) {
  const el = document.createElement("div");
  el.className = "toast " + (kind || "");
  el.textContent = msg;
  $("toasts").appendChild(el);
  const id = ++toastSeq;
  setTimeout(() => { if (el.parentNode && toastSeq >= id) el.remove(); }, kind === "err" ? 7000 : 3600);
}

/* ---------- 全局状态 ---------- */

const state = {
  items: [],       // 解析结果
  sel: [],         // sel[i] = Set(分P序号)
  playing: false,  // 是否已有任务在跑
  logCursor: 0,
  speeds: [],
  activeTab: "web",
  login: null,
  dockClosed: false,
  quit: false,
  account: null,
};

/* ---------- 主题 ---------- */

function applyTheme(t) {
  document.documentElement.setAttribute("data-theme", t);
  $("theme-name").textContent = t === "dark" ? "暗黑" : "普通";
  try { localStorage.setItem("bbdown-native-theme", t); } catch (e) { /* 忽略 */ }
}

$("btn-theme").addEventListener("click", () => {
  const next = document.documentElement.getAttribute("data-theme") === "dark" ? "light" : "dark";
  applyTheme(next);
});

/* ---------- 状态与配置 ---------- */

function paintSettings(s) {
  $("download_dir").value = s.download_dir || "";
  $("codec_order").value = s.codec_order || "";
  $("parallel").value = s.parallel || 6;
  $("keep_temp").checked = !!s.keep_temp;
  // 画质下拉是按解析结果填的，没有结果时只留「自动」。
  const q = String(s.quality || 0);
  if ([...$("quality").options].some((o) => o.value === q)) $("quality").value = q;
}

function paintAccount(acct, cookieSet) {
  state.account = acct;
  const chip = $("chip-login");
  const head = $("login-status");
  let text, cls, detail;

  if (acct && acct.logged_in) {
    text = "已登录 · " + (acct.uname || "账号");
    cls = "ok";
    detail = "已登录，可以下载高清晰度";
  } else if (cookieSet) {
    text = acct && acct.checked ? "登录已失效" : "校验中…";
    cls = acct && acct.checked ? "bad" : "warn";
    detail = (acct && acct.message) || "正在用已保存的凭据校验…";
  } else {
    text = "未登录";
    cls = "";
    detail = "未登录最高只能拿到 480P";
  }
  chip.className = "chip " + cls;
  chip.innerHTML = '<i class="dot"></i>' + esc(text);
  head.className = "login-status " + cls;
  $("login-text").textContent = text;
  $("login-detail").textContent = detail;
}

async function loadStatus() {
  let s;
  try { s = await api("/api/status"); }
  catch (e) { return; }
  if (state.quit) return;
  $("chip-engine").title = s.engine;
  $("chip-mux").title = s.muxer;
  paintSettings(s.settings);
  paintAccount(s.account, s.cookie_set);
  if (s.warning && !state.warned) { state.warned = true; toast(s.warning, "warn"); }
}

let saveTimer = null;
function saveConfigSoon() {
  clearTimeout(saveTimer);
  saveTimer = setTimeout(async () => {
    try { await api("/api/config", collect()); } catch (e) { /* 静默 */ }
  }, 450);
}

function collect() {
  const n = parseInt($("parallel").value, 10);
  return {
    quality: parseInt($("quality").value, 10) || 0,
    codec_order: $("codec_order").value.trim(),
    parallel: isNaN(n) ? 6 : Math.min(32, Math.max(1, n)),
    keep_temp: $("keep_temp").checked,
    download_dir: $("download_dir").value.trim(),
  };
}

["quality", "codec_order", "parallel", "keep_temp", "download_dir"].forEach((id) => {
  $(id).addEventListener("change", saveConfigSoon);
});

/* ---------- 地址识别提示 ---------- */

let extractTimer = null;
async function extractPreview() {
  const text = $("urls").value.trim();
  const note = $("url-note");
  if (!text) { note.classList.add("hidden"); return; }
  let r;
  try { r = await api("/api/extract", { text }); }
  catch (e) { return; }
  if (!r.count) {
    note.className = "url-note warn";
    note.textContent = "没从这段文本里认出 B 站地址（只认 bilibili.com / b23.tv 系的链接与 BV/av/ep/ss 号）";
    note.classList.remove("hidden");
    return;
  }
  note.className = "url-note";
  note.innerHTML = "识别到 <b>" + r.count + "</b> 个地址：" +
    r.targets.map((t) => esc(t.display)).join("、");
  note.classList.remove("hidden");
}

$("urls").addEventListener("input", () => {
  clearTimeout(extractTimer);
  extractTimer = setTimeout(extractPreview, 400);
});

/* ---------- 解析 ---------- */

$("btn-parse").addEventListener("click", async () => {
  const text = $("urls").value.trim();
  if (!text) { toast("先粘贴视频地址或分享文案", "warn"); return; }

  const btn = $("btn-parse");
  btn.classList.add("loading");
  btn.disabled = true;
  try {
    const r = await api("/api/parse", { text });
    renderItems(r.items || []);
    toast("解析完成，共 " + (r.count || 0) + " 个目标", "ok");
  } catch (e) {
    toast(e.message, "err");
  } finally {
    btn.classList.remove("loading");
    btn.disabled = false;
  }
});

$("btn-clear-url").addEventListener("click", () => {
  $("urls").value = "";
  $("url-note").classList.add("hidden");
  $("card-info").classList.add("hidden");
  state.items = [];
  state.sel = [];
  $("quality").innerHTML = '<option value="0">自动（最高可用）</option>';
});

$("btn-paste").addEventListener("click", async () => {
  const btn = $("btn-paste");
  btn.classList.add("loading");
  btn.disabled = true;
  try {
    const r = await api("/api/clipboard", {});
    if (!r.text || !r.text.trim()) { toast("剪贴板里没有文本", "warn"); return; }
    $("urls").value = r.text;
    await extractPreview();
  } catch (e) {
    toast(e.message, "err");
  } finally {
    btn.classList.remove("loading");
    btn.disabled = false;
  }
});

$("quick-examples").addEventListener("click", (ev) => {
  const a = ev.target.closest("a[data-u]");
  if (!a) return;
  $("urls").value = a.dataset.u;
  extractPreview();
});

function buildQualityOptions(items) {
  const seen = new Map();
  items.forEach((it) => (it.qualities || []).forEach((q) => seen.set(q.qn, q.name)));
  const prev = $("quality").value;
  const list = [...seen.entries()].sort((a, b) => b[0] - a[0]);
  let html = '<option value="0">自动（最高可用）</option>';
  list.forEach(([qn, name]) => {
    html += '<option value="' + qn + '">' + esc(name) + "（qn=" + qn + "）</option>";
  });
  $("quality").innerHTML = html;
  if ([...$("quality").options].some((o) => o.value === prev)) $("quality").value = prev;
}

function syncSelCount(i) {
  const block = document.querySelector('[data-item="' + i + '"]');
  if (!block || !state.items[i]) return;
  const total = (state.items[i].pages || []).length;
  const n = state.sel[i].size;
  const countEl = block.querySelector(".pages-count");
  if (countEl) countEl.textContent = "已选 " + n + "/" + total;
  const all = block.querySelector('[data-act="all"]');
  const none = block.querySelector('[data-act="none"]');
  if (all) all.disabled = n === total;
  if (none) none.disabled = n === 0;
}

/** 接口返回的封面常常是 http:// 明文地址，统一升到 https —— 图床本来就支持，
 *  顺手避开以后页面搬到 https 时的混合内容拦截。 */
function httpsURL(u) {
  return String(u || "").replace(/^http:\/\//i, "https://");
}

function renderItems(items) {
  const box = $("info-list");
  box.innerHTML = "";
  state.items = items;
  state.sel = items.map(() => new Set());

  items.forEach((it, i) => {
    const sel = state.sel[i];
    (it.pages || []).forEach((p) => sel.add(p.index));

    const block = document.createElement("div");
    block.className = "item-block";
    block.dataset.item = String(i);

    const qtags = (it.qualities || []).map((q) =>
      '<span class="qtag' + (q.qn >= 80 ? " hi" : "") + '">' + esc(q.name) + "</span>"
    ).join("");

    block.innerHTML =
      '<div class="media">' +
        (it.cover
          ? '<img class="media-cover" alt="" src="' + esc(httpsURL(it.cover)) +
            '" referrerpolicy="no-referrer" onerror="this.style.display=\'none\'">'
          : "") +
        '<div class="media-body">' +
          '<div class="media-title">' + esc(it.title) +
            (it.is_bangumi ? '<span class="item-badge">番剧</span>' : "") + "</div>" +
          '<div class="media-meta">' +
            (it.owner ? "<span>UP <b>" + esc(it.owner) + "</b></span>" : "") +
            "<span>编号 <b>" + esc(it.id) + "</b></span>" +
            "<span>时长 <b>" + fmtDur(it.duration) + "</b></span>" +
            "<span>分P <b>" + (it.pages || []).length + "</b></span>" +
          "</div>" +
          (qtags ? '<div class="qlist">' + qtags + "</div>" : "") +
        "</div>" +
      "</div>";

    if ((it.pages || []).length > 0) {
      const det = document.createElement("details");
      det.className = "pages-wrap";
      det.open = (it.pages || []).length <= 60;
      det.innerHTML =
        "<summary>分P列表 <span class=\"pages-count\"></span></summary>" +
        '<div class="pages-tools">' +
          '<button class="btn tiny" data-act="all">全选</button>' +
          '<button class="btn tiny" data-act="none">全不选</button>' +
          '<button class="btn tiny" data-act="default">只选默认 P</button>' +
          '<span class="hint">不勾选的目标会被跳过</span>' +
        "</div>" +
        '<div class="pages"></div>';
      const list = det.querySelector(".pages");
      (it.pages || []).forEach((p) => {
        const lb = document.createElement("label");
        lb.innerHTML =
          '<input type="checkbox" checked data-page="' + p.index + '">' +
          '<span class="pno">P' + p.index + "</span>" +
          '<span class="ptitle">' + esc(p.title) + "</span>" +
          '<span class="pdur">' + fmtDur(p.duration) + "</span>";
        list.appendChild(lb);
      });
      block.appendChild(det);
    }

    box.appendChild(block);
    syncSelCount(i);
  });

  $("card-info").classList.remove("hidden");
  $("info-hint").textContent = items.length + " 个目标 · 编号 " +
    items.map((it) => it.id).filter(Boolean).join(" / ");
  buildQualityOptions(items);
}

// 分P勾选与全选/全不选
$("info-list").addEventListener("change", (ev) => {
  const cb = ev.target.closest('input[type="checkbox"][data-page]');
  if (!cb) return;
  const block = cb.closest("[data-item]");
  const i = Number(block.dataset.item);
  const idx = Number(cb.dataset.page);
  if (cb.checked) state.sel[i].add(idx); else state.sel[i].delete(idx);
  syncSelCount(i);
});

$("info-list").addEventListener("click", (ev) => {
  const btn = ev.target.closest("button[data-act]");
  if (!btn) return;
  const block = btn.closest("[data-item]");
  const i = Number(block.dataset.item);
  const act = btn.dataset.act;
  const pages = state.items[i].pages || [];
  const next = new Set();
  if (act === "all") pages.forEach((p) => next.add(p.index));
  else if (act === "default") {
    const d = state.items[i].default_page;
    if (d > 0) next.add(d);
  }
  state.sel[i] = next;
  block.querySelectorAll('input[data-page]').forEach((cb) => {
    cb.checked = next.has(Number(cb.dataset.page));
  });
  syncSelCount(i);
});

/* ---------- 下载 ---------- */

function specOf(i) {
  return [...state.sel[i]].sort((a, b) => a - b).join(",");
}

$("btn-download").addEventListener("click", async () => {
  if (!state.items.length) { toast("请先解析视频地址", "warn"); return; }

  const empty = state.items
    .map((_, i) => i)
    .filter((i) => state.sel[i].size === 0);
  if (empty.length) {
    toast("第 " + empty.map((i) => i + 1).join("、") + " 个目标没有勾选任何分P", "warn");
    return;
  }

  const cfg = collect();
  try {
    await api("/api/download", {
      specs: state.items.map((_, i) => specOf(i)),
      quality: cfg.quality,
      codec_order: cfg.codec_order,
      parallel: cfg.parallel,
      out_dir: cfg.download_dir,
    });
    $("console").textContent = "";
    state.logCursor = 0;
    state.speeds = [];
    toast("已开始下载", "ok");
  } catch (e) {
    toast(e.message, "err");
  }
});

$("btn-stop").addEventListener("click", async () => {
  try { await api("/api/stop", {}); toast("正在停止…", "warn"); }
  catch (e) { toast(e.message, "err"); }
});

$("btn-clear-log").addEventListener("click", () => {
  // 只清 DOM，光标不动 —— 否则下一次轮询会把整段历史又拉回来。
  $("console").textContent = "";
  $("log-count").textContent = "";
});

/* ---------- 任务轮询与渲染 ---------- */

const STATE_TEXT = {
  idle: "空闲", running: "进行中", done: "已完成",
  failed: "有失败", stopped: "已停止",
};

function applyTask(snap) {
  state.playing = !!snap.running;

  $("task-state").textContent = STATE_TEXT[snap.state] || snap.state;

  const bar = $("task-bar");
  const wrap = bar.parentNode;
  const pct = snap.percent || 0;
  if (snap.state === "running" && snap.total <= 0) {
    wrap.classList.add("indeterminate");
    $("task-pct").textContent = "准备中…";
  } else {
    wrap.classList.remove("indeterminate");
    bar.style.width = Math.max(0, Math.min(100, pct)).toFixed(1) + "%";
    $("task-pct").textContent = pct > 0
      ? pct.toFixed(1) + "%"
      : (snap.state === "idle" ? "就绪" : (STATE_TEXT[snap.state] || "就绪"));
  }
  wrap.classList.toggle("failed", snap.state === "failed");

  // 速度曲线
  if (snap.state === "running") {
    state.speeds.push(snap.speed || 0);
    if (state.speeds.length > 120) state.speeds.shift();
    drawSpark();
  } else if (state.speeds.length && snap.state !== "idle") {
    state.speeds.push(0);
    if (state.speeds.length > 120) state.speeds.shift();
    drawSpark();
  }

  // 速度 / 已下载 / 剩余
  const showStats = snap.total > 0;
  $("task-stats").classList.toggle("hidden", !showStats);
  if (showStats) {
    $("ps-speed").textContent = fmtSpeed(snap.speed);
    $("ps-size").textContent = fmtBytes(snap.done) + " / " + fmtBytes(snap.total);
    $("ps-eta").textContent = snap.running ? fmtEta(snap.eta) : "--";
  }

  // 四步进度
  const steps = snap.steps || {};
  let anyStep = false;
  $("task-steps").querySelectorAll("[data-s]").forEach((el) => {
    const v = steps[el.dataset.s] || "";
    el.className = "pstep" + (v ? " " + v : "");
    if (v) anyStep = true;
  });
  $("task-steps").classList.toggle("hidden", !anyStep);
  $("ps-phase").classList.toggle("hidden", !anyStep);
  $("ps-phase").textContent = snap.label
    ? snap.label + (snap.stage ? " · " + snap.stage : "")
    : (snap.stage || "");

  $("task-stage").textContent = snap.stage || "等待任务…";

  // 产物
  const outs = snap.outputs || [];
  const outBox = $("task-outputs");
  if (outs.length) {
    outBox.innerHTML = '<div class="outputs-head">本次产物 ' + outs.length + " 个</div>" +
      outs.map((o) =>
        '<div class="output-row" title="' + esc(o.path) + '">' +
          '<span class="oname">' + esc(o.name) + "</span>" +
          '<span class="osize">' + fmtBytes(o.size) + "</span>" +
        "</div>"
      ).join("");
    outBox.classList.remove("hidden");
  } else {
    outBox.classList.add("hidden");
  }

  // 按钮
  $("btn-download").disabled = state.playing;
  $("btn-stop").disabled = !state.playing;
  $("btn-parse").disabled = state.playing;

  appendLogs(snap.logs || []);
  if (typeof snap.log_from === "number") state.logCursor = snap.log_from;
}

function drawSpark() {
  const a = state.speeds;
  if (a.length < 2) return;
  const max = Math.max.apply(null, a.concat([1]));
  const n = a.length;
  const pts = a.map((v, i) =>
    (i / (n - 1) * 100).toFixed(2) + "," + (22 - (v / max) * 20).toFixed(2)
  );
  $("ps-spark-line").setAttribute("points", pts.join(" "));
  $("ps-spark-area").setAttribute("points", "0,22 " + pts.join(" ") + " 100,22");
  $("ps-spark-wrap").classList.remove("hidden");
}

function appendLogs(lines) {
  if (!lines.length) return;
  const box = $("console");
  const frag = document.createDocumentFragment();
  lines.forEach((l) => {
    const row = document.createElement("div");
    if (l.lv) row.className = "ln-" + l.lv;
    const t = document.createElement("span");
    t.className = "ln-time";
    t.textContent = "[" + l.t + "] ";
    row.appendChild(t);
    row.appendChild(document.createTextNode(l.s));
    frag.appendChild(row);
  });
  box.appendChild(frag);
  while (box.childNodes.length > 1500) box.removeChild(box.firstChild);
  if ($("autoscroll").checked) box.scrollTop = box.scrollHeight;
  $("log-count").textContent = state.logCursor + " 行";
}

let taskBusy = false;
async function pollTask() {
  if (taskBusy || state.quit) return;
  taskBusy = true;
  try {
    const snap = await api("/api/task?since=" + state.logCursor);
    applyTask(snap);
  } catch (e) {
    // 服务已经退出时不做任何打扰
  } finally {
    taskBusy = false;
  }
}

/* ---------- 登录 ---------- */

function paintLogin(s) {
  state.login = s;
  const modes = ["web", "tv"];
  modes.forEach((m) => {
    const mine = s.mode === m;
    const st = $("qr-state-" + m);
    const rem = $("qr-remain-" + m);
    const mask = $("qr-mask-" + m);
    const img = $("qr-img-" + m);

    st.textContent = mine ? (s.message || "准备中…") : "尚未开始";
    st.className = "qr-state " + (mine ? s.state : "");
    rem.textContent = mine && s.remain > 0 ? s.remain : "--";

    const show = mine && s.has_qr;
    mask.classList.toggle("hidden", show);
    if (show) {
      if (img.dataset.seq !== String(s.seq)) {
        img.dataset.seq = String(s.seq);
        img.src = "/api/login/qrcode.png?v=" + s.seq;
      }
    } else if (img.dataset.seq) {
      img.dataset.seq = "";
      img.src = BLANK_IMG;
    }

    $("btn-qr-" + m + "-start").disabled = mine && s.running;
    $("btn-qr-" + m + "-cancel").disabled = !(mine && s.running);
  });

  // 任务台上的二维码面板
  const dock = $("qr-dock");
  const showDock = s.has_qr && !state.dockClosed;
  dock.classList.toggle("hidden", !showDock);
  if (showDock) {
    $("qr-dock-mode").textContent = s.mode === "tv" ? "电视端" : "网页端";
    $("qr-dock-state").textContent = s.message || "";
    $("qr-dock-state").className = "qr-dock-state " + s.state;
    $("qr-dock-remain").textContent = s.remain > 0 ? s.remain : "--";
    const di = $("qr-dock-img");
    if (di.dataset.seq !== String(s.seq)) {
      di.dataset.seq = String(s.seq);
      di.src = "/api/login/qrcode.png?v=" + s.seq;
    }
    if (s.state === "confirmed") $("qr-dock-msg").textContent = "登录成功，正在保存凭据…";
    else if (s.state === "scanned") $("qr-dock-msg").textContent = "已扫码，请在手机上确认";
    else $("qr-dock-msg").textContent = "用哔哩哔哩 App 扫描二维码";
  }
}

let loginBusy = false;
async function pollLogin() {
  if (loginBusy || state.quit) return;
  loginBusy = true;
  try {
    const s = await api("/api/login/status");
    const wasConfirmed = state.login && state.login.state === "confirmed";
    paintLogin(s);
    if (s.state === "confirmed" && !wasConfirmed) {
      state.dockClosed = false;
      loadStatus();
      setTimeout(() => { state.dockClosed = true; $("qr-dock").classList.add("hidden"); }, 4000);
    }
  } catch (e) {
    // 忽略
  } finally {
    loginBusy = false;
  }
}

document.querySelectorAll("#login-tabs .tab").forEach((tab) => {
  tab.addEventListener("click", () => {
    document.querySelectorAll("#login-tabs .tab").forEach((t) => t.classList.remove("active"));
    tab.classList.add("active");
    const want = tab.dataset.tab;
    state.activeTab = want;
    ["web", "tv", "cookie"].forEach((m) => {
      $("panel-" + m).classList.toggle("hidden", m !== want);
    });
  });
});

async function startLogin(mode) {
  try {
    const r = await api("/api/login/start", { mode });
    if (!r.ok) { toast(r.message || "无法开始", "warn"); return; }
    state.dockClosed = false;
  } catch (e) {
    toast(e.message, "err");
  }
}

$("btn-qr-web-start").addEventListener("click", () => startLogin("web"));
$("btn-qr-tv-start").addEventListener("click", () => startLogin("tv"));

["web", "tv"].forEach((m) => {
  $("btn-qr-" + m + "-cancel").addEventListener("click", async () => {
    try { await api("/api/login/cancel", { mode: m }); } catch (e) { /* 忽略 */ }
  });
});

$("qr-dock-close").addEventListener("click", () => {
  state.dockClosed = true;
  $("qr-dock").classList.add("hidden");
});

$("btn-save-cookie").addEventListener("click", async () => {
  const v = $("cookie").value.trim();
  if (!v) { toast("先粘贴 cookie", "warn"); return; }
  const btn = $("btn-save-cookie");
  btn.classList.add("loading");
  btn.disabled = true;
  try {
    const r = await api("/api/login/cookie", { cookie: v });
    toast("登录成功：" + (r.uname || "账号"), "ok");
    $("cookie").value = "";
    loadStatus();
  } catch (e) {
    toast(e.message, "err");
  } finally {
    btn.classList.remove("loading");
    btn.disabled = false;
  }
});

$("btn-logout").addEventListener("click", async () => {
  try {
    await api("/api/login/logout", {});
    toast("已退出登录", "ok");
    loadStatus();
  } catch (e) {
    toast(e.message, "err");
  }
});

/* ---------- 目录与退出 ---------- */

$("btn-pick").addEventListener("click", async () => {
  try {
    const r = await api("/api/pick-folder", {});
    if (r.canceled) return;
    if (r.path) {
      $("download_dir").value = r.path;
      saveConfigSoon();
    }
  } catch (e) {
    toast(e.message, "err");
  }
});

$("btn-open").addEventListener("click", async () => {
  try { await api("/api/open", { path: $("download_dir").value.trim() }); }
  catch (e) { toast(e.message, "err"); }
});

$("btn-verify").addEventListener("click", async () => {
  const btn = $("btn-verify");
  btn.textContent = "校验中…";
  btn.disabled = true;
  try {
    const r = await api("/api/verify", {});
    toast(r.logged_in ? ("已登录：" + (r.uname || "账号")) : ("未登录：" + (r.message || "")),
      r.logged_in ? "ok" : "warn");
  } catch (e) {
    toast(e.message, "err");
  } finally {
    btn.textContent = "重新校验";
    btn.disabled = false;
    loadStatus();
  }
});

$("btn-quit").addEventListener("click", async () => {
  if (state.playing && !confirm("还有任务在跑，确定要退出吗？")) return;
  state.quit = true;
  try { await api("/api/quit", {}); } catch (e) { /* 服务可能已经关了 */ }
  document.body.innerHTML =
    '<div class="app"><div class="card" style="margin:60px auto;max-width:520px;text-align:center">' +
    "<h2>程序已退出</h2><p class=\"hint\" style=\"margin-top:10px\">" +
    "本地服务已关闭，这个页面可以直接关掉了。</p></div></div>";
});

/* ---------- 启动 ---------- */

applyTheme(document.documentElement.getAttribute("data-theme") || "light");

window.addEventListener("pagehide", () => {
  // 关掉页面就等于关掉程序：窗口化的 exe 没有窗口可关，不通知一声
  // 就会留下一个看不见的常驻进程。用 keepalive 是为了让请求在
  // 卸载过程中也能发出去，同时仍然带上那个自定义头。
  try {
    fetch("/api/bye", { method: "POST", headers: HEADERS, body: "{}", keepalive: true });
  } catch (e) { /* 忽略 */ }
});

loadStatus();
pollTask();
pollLogin();
setInterval(pollTask, 600);
setInterval(loadStatus, 5000);
setInterval(pollLogin, 1200);
