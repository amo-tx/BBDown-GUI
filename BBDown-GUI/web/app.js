/* ===== BBDown 图形界面 · 前端逻辑 ===== */
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const TEXT_FIELDS = ["dfn_priority", "encoding_priority", "language", "select_page",
    "file_pattern", "extra_args", "aria2c_path", "download_dir", "ffmpeg_path",
    "cookie", "access_token", "delay_per_page"];
  // 凭证只有在「校验并保存」时才写入配置，避免把没粘完的内容存进去
  const CRED_FIELDS = ["cookie", "access_token"];
  const CHECK_FIELDS = ["video_only", "audio_only", "danmaku_only", "sub_only", "cover_only",
    "download_danmaku", "skip_subtitle", "skip_cover", "skip_mux", "interactive",
    "save_archives_to_file", "use_aria2c",
    "compat_mode", "force_http", "force_replace_host", "allow_pcdn"];
  const BLANK_IMG = "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7";

  let taskTag = 0;
  let taskActive = false;
  let taskId = 0;                 // 当前任务序号（后端换任务时用于重置日志）
  let pollTimer = null;
  let logCount = 0;

  /* ---------- 工具 ---------- */
  function fmtDur(sec) {
    sec = Math.round(Number(sec) || 0);
    if (sec <= 0) return "";
    const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    const p = (n) => String(n).padStart(2, "0");
    return (h ? h + ":" + p(m) : m) + ":" + p(s);
  }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, (c) => (
      { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  }
  function toast(msg, type = "", ms = 3600) {
    const el = document.createElement("div");
    el.className = "toast " + type;
    el.textContent = msg;
    $("toasts").appendChild(el);
    setTimeout(() => { el.style.opacity = "0"; setTimeout(() => el.remove(), 250); }, ms);
  }
  const api = async (path, data) => {
    const opt = data
      ? { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(data) }
      : {};
    const r = await fetch(path, opt);
    if (!r.ok) throw new Error("HTTP " + r.status);
    return r.json();
  };
  const busy = (btn, on) => btn && btn.classList.toggle("loading", on);

  /* ================= 主题（普通 / 暗黑） ================= */
  const THEME_KEY = "bbdown-theme";
  let themeSynced = false;

  function currentTheme() {
    return document.documentElement.getAttribute("data-theme") === "dark" ? "dark" : "light";
  }
  function applyTheme(theme, persist) {
    const t = theme === "dark" ? "dark" : "light";
    document.documentElement.setAttribute("data-theme", t);
    try { localStorage.setItem(THEME_KEY, t); } catch (e) { /* 忽略 */ }
    if ($("theme-name")) $("theme-name").textContent = t === "dark" ? "暗黑" : "普通";
    if (persist) api("/api/config", { theme: t }).catch(() => {});
  }
  $("btn-theme").addEventListener("click", () => {
    applyTheme(currentTheme() === "dark" ? "light" : "dark", true);
    themeSynced = true;
    toast("已切换到" + (currentTheme() === "dark" ? "暗黑" : "普通") + "主题", "", 1500);
  });
  $("theme-name").textContent = currentTheme() === "dark" ? "暗黑" : "普通";
  function syncThemeFromServer(theme) {
    if (themeSynced) return;
    let has = null;
    try { has = localStorage.getItem(THEME_KEY); } catch (e) { /* 忽略 */ }
    if (!has && theme) applyTheme(theme, false);
    themeSynced = true;
  }

  /* ---------- 配置绑定 ---------- */
  function collect() {
    const c = {};
    TEXT_FIELDS.forEach((k) => {
      if (CRED_FIELDS.indexOf(k) < 0) c[k] = $(k).value.trim();
    });
    CHECK_FIELDS.forEach((k) => (c[k] = $(k).checked));
    c.api = $("api").value;
    return c;
  }
  function apply(cfg) {
    if (!cfg) return;
    TEXT_FIELDS.forEach((k) => { if (cfg[k] !== undefined) $(k).value = cfg[k] ?? ""; });
    CHECK_FIELDS.forEach((k) => { if (cfg[k] !== undefined) $(k).checked = !!cfg[k]; });
    if (cfg.api) $("api").value = cfg.api;
  }
  let saveTimer = null;
  function pushConfig() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => api("/api/config", collect()).catch(() => {}), 400);
  }
  TEXT_FIELDS.forEach((k) => {
    if ($(k) && CRED_FIELDS.indexOf(k) < 0) $(k).addEventListener("input", pushConfig);
  });
  CHECK_FIELDS.forEach((k) => { if ($(k)) $(k).addEventListener("change", pushConfig); });
  $("api").addEventListener("change", pushConfig);

  /* ---------- 环境状态 ---------- */
  async function refreshStatus(quiet) {
    try {
      const s = await api("/api/status");
      const cv = $("chip-version");
      cv.className = "chip " + (s.bbdownExeExists ? "ok" : "bad");
      cv.querySelector("b").textContent = s.bbdownExeExists ? ("v" + (s.version || "?")) : "未找到";

      const cf = $("chip-ffmpeg");
      cf.className = "chip " + (s.ffmpeg.found ? "ok" : "bad");
      cf.querySelector("b").textContent = s.ffmpeg.found ? "已就绪" : "缺失";

      const L = s.login || {};
      const cl = $("chip-login");
      cl.className = "chip " + (L.ok ? "ok" : "warn");
      cl.innerHTML = '<i class="dot"></i>' + (L.ok ? "已登录" : "未登录")
        + (L.ok && L.uname ? "·" + esc(L.uname) : "");
      cl.title = L.ok
        ? ("登录状态：" + (L.uname || "已登录") + (L.mid ? "（UID " + L.mid + "）" : ""))
        : "未登录，仅能下载公开内容";

      const ls = $("login-status");
      ls.className = "login-status " + (L.ok ? "ok" : "warn");
      $("login-text").textContent = L.ok ? (L.uname ? "已登录：" + L.uname : "已登录") : "未登录";
      const det = [];
      if (L.ok) {
        if (L.mid) det.push("UID " + L.mid);
        if (L.sessdata) det.push("SESSDATA " + L.sessdata);
        if (L.hasToken) det.push("已配置 access_token");
        if (L.cookieFile) det.push("已写入 " + L.cookieFile.split(/[\\/]/).pop());
        if (L.tvTokenFile) det.push("已写入 " + L.tvTokenFile.split(/[\\/]/).pop());
      } else {
        det.push("登录后可下载会员清晰度内容");
      }
      $("login-detail").textContent = det.join(" · ");

      const badge = $("ffmpeg-badge");
      badge.textContent = s.ffmpeg.found ? "已检测到" : "未找到";
      badge.className = "badge " + (s.ffmpeg.found ? "ok" : "bad");
      if (s.ffmpeg.found) $("ffmpeg_path").value = s.ffmpeg.path;

      syncThemeFromServer(s.theme);
      if (!quiet) apply(s.config);
      const bq = $("btn-quit"), bb = $("btn-browser");
      if (bq) bq.classList.remove("hidden");
      if (bb) bb.classList.toggle("hidden", !s.desktop);
      return s;
    } catch (e) {
      if (!quiet) toast("无法连接本地服务：" + e.message, "err");
      return null;
    }
  }

  /* ---------- 日志渲染 ---------- */
  function lineClass(m) {
    if (/错误|失败|异常|error|报错|Exception|找不到/i.test(m)) return "ln-err";
    if (/完成|成功|已保存|下载完毕|合并/i.test(m)) return "ln-ok";
    if (/警告|warn|跳过/i.test(m)) return "ln-warn";
    return "";
  }
  function appendLogs(lines, replace) {
    const box = $("console");
    if (replace) { box.textContent = ""; logCount = 0; }
    if (!lines || !lines.length) return;
    const frag = document.createDocumentFragment();
    lines.forEach((l) => {
      const d = document.createElement("div");
      if (l.q) {
        // 控制台字符画二维码：不换行、保留原始宽度，否则图形会错位
        d.className = "ln-qr";
        d.textContent = l.m;
      } else {
        const cls = lineClass(l.m);
        if (cls) d.className = cls;
        const t = document.createElement("span");
        t.className = "ln-time";
        t.textContent = "[" + l.t.toFixed(1).padStart(6) + "s] ";
        d.appendChild(t);
        d.appendChild(document.createTextNode(l.m));
      }
      frag.appendChild(d);
      logCount++;
    });
    box.appendChild(frag);
    $("log-count").textContent = logCount + " 行";
    if ($("autoscroll").checked) box.scrollTop = box.scrollHeight;
  }

  /* ================= 下载进度 =================
   * 服务端在 /api/task 的 prog 字段里给出：percent / speed / eta /
   * downloaded / total / stage / stageText / page / pages / outputs。
   * 这些数据来自「磁盘分片增长」，因为 BBDown 在输出被重定向时什么进度都不打印。
   */
  const STEP_ORDER = ["parse", "video", "audio", "mux"];

  function fmtBytes(n) {
    n = Number(n) || 0;
    if (n < 1024) return Math.round(n) + " B";
    const unit = ["KB", "MB", "GB", "TB"];
    let i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < unit.length - 1);
    return (n >= 100 ? n.toFixed(0) : n.toFixed(1)) + " " + unit[i];
  }
  function fmtSpeed(n) {
    n = Number(n) || 0;
    return n > 0 ? fmtBytes(n) + "/s" : "--";
  }
  function fmtPair(done, total) {
    if (!total) return done > 0 ? fmtBytes(done) : "--";
    const u = ["B", "KB", "MB", "GB", "TB"];
    let i = 0;
    while (i < u.length - 1 && total >= 1024) { total /= 1024; done /= 1024; i++; }
    const d = done >= 100 ? done.toFixed(0) : done.toFixed(1);
    return d + "/" + (total >= 100 ? total.toFixed(0) : total.toFixed(1)) + " " + u[i];
  }
  function fmtEta(sec) {
    sec = Math.round(Number(sec) || 0);
    if (sec <= 0) return "--";
    if (sec < 60) return sec + " 秒";
    const m = Math.floor(sec / 60);
    if (m < 60) return m + " 分 " + (sec % 60) + " 秒";
    return Math.floor(m / 60) + " 时 " + (m % 60) + " 分";
  }
  function stepIndex(stage) {
    if (stage === "parse") return 0;
    if (stage === "video") return 1;
    if (stage === "audio" || stage === "extra") return 2;
    if (stage === "page_done" || stage === "mux") return 3;
    if (stage === "done") return 4;
    return 0;
  }

  let sparkData = [];
  function paintSpark(speed, running) {
    const wrap = $("ps-spark-wrap");
    if (!running) {
      sparkData = [];
      wrap.classList.add("hidden");
      return;
    }
    sparkData.push(Math.max(0, Number(speed) || 0));
    if (sparkData.length > 60) sparkData.shift();
    if (sparkData.length < 2) { wrap.classList.add("hidden"); return; }
    const mx = Math.max(1, ...sparkData);
    const n = sparkData.length;
    const pts = sparkData.map((v, i) => {
      const x = (i / (n - 1)) * 100;
      const y = 20 - (v / mx) * 18;
      return x.toFixed(1) + "," + y.toFixed(1);
    });
    $("ps-spark-line").setAttribute("points", pts.join(" "));
    $("ps-spark-area").setAttribute("points", "0,22 " + pts.join(" ") + " 100,22");
    wrap.classList.remove("hidden");
  }

  function paintOutputs(snap) {
    const box = $("task-outputs");
    const list = (snap.prog && snap.prog.outputs) || [];
    if (snap.running || !list.length) {
      box.classList.add("hidden");
      box.textContent = "";
      return;
    }
    box.classList.remove("hidden");
    box.textContent = "";
    const head = document.createElement("div");
    head.className = "outputs-head";
    head.textContent = "已保存 " + list.length + " 个文件";
    box.appendChild(head);
    list.slice(0, 4).forEach((o) => {
      const row = document.createElement("div");
      row.className = "output-row";
      const nm = document.createElement("span");
      nm.className = "oname";
      nm.textContent = o.name;
      nm.title = o.name;
      const sz = document.createElement("span");
      sz.className = "osize";
      sz.textContent = fmtBytes(o.size);
      row.append(nm, sz);
      box.appendChild(row);
    });
    if (list.length > 4) {
      const more = document.createElement("div");
      more.className = "output-row hint";
      more.textContent = "…还有 " + (list.length - 4) + " 个文件";
      box.appendChild(more);
    }
  }

  function paintProgress(snap) {
    const box = $("task-stats"), steps = $("task-steps"), phase = $("ps-phase");
    const prog = snap.prog;
    if (!prog || !prog.stage) {
      box.classList.add("hidden");
      steps.classList.add("hidden");
      phase.classList.add("hidden");
      $("ps-spark-wrap").classList.add("hidden");
      return;
    }
    box.classList.remove("hidden");
    steps.classList.remove("hidden");
    phase.classList.remove("hidden");

    const running = !!snap.running;
    const done = !!prog.done;
    $("ps-speed").textContent = running ? fmtSpeed(prog.speed) : "--";
    $("ps-size").textContent = fmtPair(prog.downloaded, prog.total);
    $("ps-eta").textContent = !running ? "--"
      : (prog.estimated ? fmtEta(prog.eta) : "测算中");

    // 阶段文案（分P信息并进去）
    let txt = !running && done ? "已完成" : (prog.stageText || "--");
    if (running && prog.pages > 1) {
      txt += "（第 " + prog.page + "/" + prog.pages + " P）";
    }
    phase.textContent = txt;

    // 阶段步进器：视频 / 音频 会按实际选择的流隐藏
    const idx = stepIndex(prog.stage);
    const known = !!prog.estimated;
    steps.querySelectorAll(".pstep").forEach((el) => {
      const s = el.dataset.s, i = STEP_ORDER.indexOf(s);
      if (s === "video") el.classList.toggle("hidden", known && !prog.hasVideo);
      if (s === "audio") el.classList.toggle("hidden", known && !prog.hasAudio);
      el.classList.toggle("done", idx > i);
      el.classList.toggle("active", idx === i);
    });

    paintSpark(prog.speed, running && prog.stage !== "mux");
    paintOutputs(snap);
  }

  /* ---------- 任务轮询 ---------- */
  const cleanLine = (s) => String(s).replace(/^\[\d{4}-\d{2}-\d{2}[^\]]*\]\s*-?\s*/, "").slice(0, 130);
  function setBar(state, pct, indeterminate) {
    const wrap = $("task-bar").parentElement, i = $("task-bar");
    wrap.classList.remove("indeterminate", "failed");
    if (state === "run") {
      if (indeterminate) {
        wrap.classList.add("indeterminate");
        $("task-pct").textContent = "进行中";
      } else {
        const v = Math.max(0, Math.min(100, Number(pct) || 0));
        i.style.width = v + "%";
        $("task-pct").textContent = v.toFixed(1) + "%";
      }
    } else if (state === "ok") { i.style.width = "100%"; $("task-pct").textContent = "完成"; }
    else if (state === "fail") { wrap.classList.add("failed"); i.style.width = "100%"; $("task-pct").textContent = "失败"; }
    else { i.style.width = "0"; $("task-pct").textContent = "就绪"; }
  }
  function setTaskUI(running, title) {
    taskActive = running;
    $("btn-stop").disabled = !running;
    $("btn-download").disabled = running;
    $("btn-download-sel").disabled = running;
    $("btn-parse").disabled = running;
    $("task-state").textContent = running ? (title || "进行中…") : "空闲";
  }
  async function pollTask() {
    let snap;
    try { snap = await api("/api/task?since=" + taskTag); }
    catch (e) { return; }
    if (!snap || !snap.exists) return;

    // 多地址批量下载时后端会换一个新任务对象：重置日志与曲线，从头拉取
    if (snap.id !== taskId) {
      taskId = snap.id;
      taskTag = 0;
      sparkData = [];
      appendLogs([], true);
      try { snap = await api("/api/task?since=0"); } catch (e) { /* 用旧快照 */ }
    }

    if (snap.logs && snap.logs.length) {
      appendLogs(snap.logs, false);
      taskTag = snap.tag;
    }
    const prog = snap.prog;
    if (snap.running) {
      if (prog && prog.estimated && prog.stage !== "parse") setBar("run", prog.percent || 0);
      else setBar("run", 0, true);
    }
    paintProgress(snap);
    if (snap.last) $("task-stage").textContent = cleanLine(snap.last);

    const wasActive = taskActive;
    setTaskUI(snap.running, snap.title);

    if (wasActive && !snap.running) {
      const ok = snap.exitCode === 0;
      setBar(ok ? "ok" : "fail");
      if (!ok) $("task-stage").textContent = "任务失败，请查看下方日志";
      toast(ok ? "任务完成：" + snap.title : "任务结束（返回码 " + snap.exitCode + "）",
        ok ? "ok" : "err");
      if (snap.kind === "login") refreshStatus(false);
      // 队列里还有地址：继续轮询，别提前收工
      if (ok && (snap.queueLeft || 0) > 0) {
        $("task-stage").textContent = "还有 " + snap.queueLeft + " 个地址排队中…";
        return;
      }
      stopPolling();
    }
  }
  function startPolling() {
    if (pollTimer) return;
    pollTimer = setInterval(pollTask, 500);
  }
  function stopPolling() {
    if (pollTimer) { clearInterval(pollTimer); pollTimer = null; }
  }
  async function trackTask(btn, label) {
    taskTag = 0;
    taskId = 0;
    sparkData = [];
    appendLogs([], true);
    setBar("run", 0, true);
    $("task-stage").textContent = label || "正在启动…";
    await pollTask();
    startPolling();
    if (btn) busy(btn, false);
    $("task-state").textContent = label || "进行中…";
  }

  /* ---------- 地址识别 / 粘贴 ----------
     提取规则只在服务端实现一份（server.py:extract_targets），前端一律调 /api/extract。
     这样「界面提示的地址」与「真正交给 BBDown 的地址」不会各写一套正则而漂移。 */
  let urlNoteTimer = null;

  async function extractTargets(text) {
    if (!String(text == null ? "" : text).trim()) return [];
    try {
      const r = await api("/api/extract", { text: String(text) });
      return r.targets || [];
    } catch (e) {
      return [];                    // 只是提示，失败就当没识别到，不拦提交
    }
  }

  function showUrlNote(msg, type) {
    const el = $("url-note");
    if (!msg) {
      el.textContent = "";
      el.className = "url-note hidden";
      return;
    }
    el.textContent = msg;
    el.className = "url-note" + (type ? " " + type : "");
  }

  /** 提示而已，不改写输入框：用户还在打字时不要动他的内容。 */
  async function updateUrlNote() {
    const raw = $("urls").value;
    if (!raw.trim()) return showUrlNote("");
    const targets = await extractTargets(raw);
    if (!targets.length) return showUrlNote("没识别到 B 站地址", "warn");
    if (targets.length === 1 && raw.trim() === targets[0]) return showUrlNote("");
    showUrlNote("已从中识别出 " + targets.length + " 个地址" +
      (targets.length > 1 ? "，将依次下载" : "：" + targets[0]), "");
  }

  /** 提交前把输入框整理成「一行一个干净地址」。识别不到则原样保留并给出提示。 */
  async function normalizeUrls() {
    const raw = $("urls").value;
    const targets = await extractTargets(raw);
    if (!targets.length) {
      showUrlNote(raw.trim() ? "没识别到 B 站地址，请检查输入" : "请先粘贴或输入视频地址",
        raw.trim() ? "warn" : "");
      return [];
    }
    const clean = targets.join("\n");
    if (raw.trim() !== clean) $("urls").value = clean;
    showUrlNote(targets.length > 1
      ? "共 " + targets.length + " 个地址，将依次下载"
      : "地址：" + targets[0], "");
    return targets;
  }

  $("urls").addEventListener("input", () => {
    clearTimeout(urlNoteTimer);
    urlNoteTimer = setTimeout(updateUrlNote, 350);
  });

  /** 读剪贴板：浏览器 API 快但桌面窗口可能被拒（甚至可能一直不返回），回退到服务端。 */
  async function readClipboard() {
    try {
      if (navigator.clipboard && navigator.clipboard.readText) {
        // 加超时兜底：WebView2 里被权限挡下时若既不 resolve 也不 reject，按钮会一直转圈
        const t = await Promise.race([
          Promise.resolve(navigator.clipboard.readText()).catch(() => ""),
          new Promise((r) => setTimeout(r, 1200)),
        ]);
        if (t) return { ok: true, text: t };
      }
    } catch (e) { /* 权限被拒 → 走服务端 */ }
    try {
      const r = await api("/api/clipboard");
      return { ok: !!(r && r.ok), text: (r && r.text) || "",
        message: (r && r.message) || "" };
    } catch (e) {
      return { ok: false, text: "", message: e.message };
    }
  }

  $("btn-paste").addEventListener("click", async (e) => {
    const btn = e.currentTarget;
    busy(btn, true);
    try {
      const clip = await readClipboard();
      if (!clip.ok || !clip.text.trim()) {
        toast(clip.message || "剪贴板里没有文字", "warn");
        return;
      }
      const box = $("urls");
      const before = (await extractTargets(box.value)).length;
      box.value = (box.value.trim() ? box.value.trim() + "\n" : "") + clip.text.trim();
      const targets = await normalizeUrls();
      if (!targets.length) {
        toast("剪贴板内容里没有 B 站地址", "warn");
        return;
      }
      const added = targets.length - before;
      toast(targets.length > 1
        ? "已粘贴并提取 " + targets.length + " 个地址（新增 " + added + " 个）"
        : "已粘贴并提取地址，可直接解析或下载", "ok");
    } catch (err) {
      toast("粘贴失败：" + err.message, "err");
    } finally { busy(btn, false); }
  });

  /* ---------- 解析 ---------- */
  $("btn-parse").addEventListener("click", async () => {
    const targets = await normalizeUrls();
    if (!targets.length) return;
    const url = targets[0];
    const extra = targets.length - 1;
    busy($("btn-parse"), true);
    appendLogs([], true);
    try {
      const r = await api("/api/parse", Object.assign({ text: $("urls").value }, collect()));
      appendLogs(r.logs || [], false);
      if (!r.ok) {
        toast(r.message || "解析失败，请查看任务台日志", "err");
        return;
      }
      renderInfo(r);
      toast(extra > 0
        ? "解析成功（识别到 " + targets.length + " 个地址，当前只解析第 1 个）"
        : "解析成功", "ok");
    } catch (e) {
      toast("解析出错：" + e.message, "err");
    } finally { busy($("btn-parse"), false); }
  });

  function renderInfo(r) {
    $("card-info").classList.remove("hidden");
    $("info-title").textContent = r.title || "(未获取到标题)";
    $("info-hint").textContent = (r.url || "") +
      (r.targetCount > 1
        ? "（共识别到 " + r.targetCount + " 个地址，这里只解析了第 1 个）" : "");
    const meta = [];
    if (r.ownerName) {
      meta.push("UP主 <b>" + esc(r.ownerName) + "</b>");
    } else if (r.owner) {
      const mid = (r.owner.match(/(\d+)/) || [])[1];
      meta.push('UP主 <b>' + esc(mid ? "mid " + mid : r.owner) + "</b>");
    }
    if (r.duration) meta.push("时长 <b>" + fmtDur(r.duration) + "</b>");
    if (r.publishTime) meta.push("发布 <b>" + esc(r.publishTime) + "</b>");
    if (r.pageCount) meta.push("分P <b>" + r.pageCount + "</b>");
    meta.push("可用流 <b>" + (r.streamCount || (r.streams || []).length) + "</b>");
    $("info-meta").innerHTML = meta.join("&nbsp;&nbsp;·&nbsp;&nbsp;");

    const pages = r.pages || [];
    const wrap = $("pages-wrap");
    const box = $("pages");
    box.textContent = "";
    if (pages.length > 1) {
      wrap.classList.remove("hidden");
      $("pages-count").textContent = "共 " + pages.length + " P";
      const frag = document.createDocumentFragment();
      pages.forEach((p) => {
        const lab = document.createElement("label");
        const cb = document.createElement("input");
        cb.type = "checkbox"; cb.checked = true; cb.value = p.n;
        const no = document.createElement("span");
        no.className = "pno"; no.textContent = "P" + p.n;
        const ti = document.createElement("span");
        ti.className = "ptitle"; ti.textContent = p.title; ti.title = p.title;
        lab.append(cb, no, ti);
        if (p.duration) {
          const du = document.createElement("span");
          du.className = "pdur"; du.textContent = p.duration;
          lab.appendChild(du);
        }
        frag.appendChild(lab);
      });
      box.appendChild(frag);
    } else {
      wrap.classList.add("hidden");
    }
    $("info-raw").textContent = (r.logs || []).map((l) => l.m).join("\n");
  }

  $("pages-all").addEventListener("click", () =>
    $("pages").querySelectorAll("input").forEach((c) => (c.checked = true)));
  $("pages-none").addEventListener("click", () =>
    $("pages").querySelectorAll("input").forEach((c) => (c.checked = false)));

  /* ---------- 下载 ---------- */
  async function doDownload(button, onlySelected) {
    const targets = await normalizeUrls();
    if (!targets.length) return toast("请先输入视频地址", "warn");

    let selectPage = $("select_page").value.trim();
    if (onlySelected) {
      const picked = [...$("pages").querySelectorAll("input:checked")].map((c) => c.value);
      if (!picked.length) return toast("请至少勾选一个分P", "warn");
      selectPage = picked.join(",");
    }
    if (!$("ffmpeg_path").value.trim()) {
      toast("未检测到 ffmpeg，请先在「环境与账号」中配置或自动下载", "warn", 6000);
    }
    busy(button, true);
    try {
      const r = await api("/api/download",
        Object.assign({ text: $("urls").value, selectPage }, collect()));
      if (!r.ok) { toast(r.message || "启动失败", "err"); busy(button, false); return; }
      const n = r.targetCount || targets.length;
      toast("已开始下载" + (n > 1 ? "（共 " + n + " 个地址，依次进行）" : ""), "ok");
      await trackTask(null, "下载中…");
      busy(button, false);
    } catch (e) {
      toast("下载出错：" + e.message, "err");
      busy(button, false);
    }
  }
  $("btn-download").addEventListener("click", (e) => doDownload(e.currentTarget, false));
  $("btn-download-sel").addEventListener("click", (e) => doDownload(e.currentTarget, true));

  $("btn-stop").addEventListener("click", async () => {
    const r = await api("/api/stop", {});
    toast(r.ok ? "已发送停止指令" : (r.message || "停止失败"), r.ok ? "warn" : "err");
  });
  $("btn-clear-log").addEventListener("click", () => { appendLogs([], true); $("console").textContent = ""; });
  $("btn-clear-url").addEventListener("click", () => {
    $("urls").value = ""; $("card-info").classList.add("hidden");
    showUrlNote("");
  });
  document.querySelectorAll("#quick-examples a").forEach((a) =>
    a.addEventListener("click", () => { $("urls").value = a.dataset.u; updateUrlNote(); }));

  /* ---------- ffmpeg ---------- */
  $("btn-ffmpeg-detect").addEventListener("click", async () => {
    const r = await api("/api/ffmpeg/detect", { path: $("ffmpeg_path").value.trim() });
    toast(r.ok ? "已找到 ffmpeg" : "仍未找到 ffmpeg，请指定正确路径", r.ok ? "ok" : "err");
    refreshStatus(false);
  });
  $("btn-ffmpeg-download").addEventListener("click", async () => {
    const r = await api("/api/ffmpeg/download", { source: 0 });
    if (!r.ok) return toast(r.message || "无法开始下载", "err");
    toast("开始下载 ffmpeg（约 110MB），来源：" + r.message, "warn", 5000);
    $("ffmpeg-dl").classList.remove("hidden");
    const t = setInterval(async () => {
      let s;
      try { s = await api("/api/ffmpeg/progress"); } catch (e) { return; }
      $("ffmpeg-dl-bar").style.width = (s.progress || 0) + "%";
      $("ffmpeg-dl-msg").textContent = s.message +
        (s.total ? "  " + (s.received / 1048576).toFixed(1) + "/" + (s.total / 1048576).toFixed(1) + " MB" : "");
      if (!s.running) {
        clearInterval(t);
        if (s.ok) {
          toast("ffmpeg 安装完成", "ok");
          $("ffmpeg-dl").classList.add("hidden");
          refreshStatus(false);
        } else {
          toast(s.message, "err", 6000);
        }
      }
    }, 800);
  });

  /* ================= 登录 ================= */
  /*
   * 四种方式：
   *   web   —— 自实现 Web 扫码（官方接口 + 自己生成二维码图片，带实时状态）
   *   tv    —— 自实现电视端扫码（登录后直接拿到 access_token）
   *   cookie—— 手动粘贴 Cookie，保存前会调账号接口校验
   *   token —— 手动填写 access_token
   * 不再使用 `BBDown login`：它只在控制台画二维码，本环境下会退化成整片实心
   * 方块，既不显示图形也没有状态反馈，这正是「日志栏看不到二维码」的原因。
   */
  const QR_STATE_TEXT = {
    idle: "尚未开始", loading: "正在获取二维码…", pending: "等待扫码…",
    scanned: "已扫码，请在手机上点击「确认登录」", confirmed: "登录成功",
    expired: "二维码已失效", error: "登录失败",
  };
  const LOGIN = { mode: "web", timer: null, playing: false, lastSeq: { web: -1, tv: -1 }, dockClosed: false };

  function tabOf(mode) { return mode === "tv" ? "tv" : "web"; }
  function modeName(mode) { return mode === "tv" ? "电视端" : "Web 端"; }

  function qrSrc(mode, s) {
    return s.hasImage ? ("/api/login/qrcode.png?mode=" + mode + "&v=" + s.seq) : BLANK_IMG;
  }

  function paintLogin(s) {
    const tab = tabOf(s.mode);
    const st = $("qr-state-" + tab);
    st.className = "qr-state " + (s.state || "");
    st.textContent = s.message || QR_STATE_TEXT[s.state] || s.state || "";

    if (LOGIN.lastSeq[tab] !== s.seq) {
      LOGIN.lastSeq[tab] = s.seq;
      const src = qrSrc(tab, s);
      $("qr-img-" + tab).src = src;
      $("qr-dock-img").src = src;
    }
    const mask = $("qr-mask-" + tab);
    mask.classList.toggle("hidden", !!s.hasImage);
    mask.textContent = s.hasImage ? ""
      : (s.state === "loading" ? "正在获取…" : (QR_STATE_TEXT[s.state] || "点击「获取二维码」"));

    $("qr-remain-" + tab).textContent = s.remain > 0 ? s.remain : "--";
    $("btn-qr-" + tab + "-start").textContent = s.hasImage ? "刷新二维码" : "获取二维码";
    paintDock(s, tab);
  }

  function paintDock(s, tab) {
    const dock = $("qr-dock");
    const show = !LOGIN.dockClosed && (s.hasImage || s.state === "loading" || s.state === "scanned");
    dock.classList.toggle("hidden", !show);
    if (!show) return;
    $("qr-dock-title").textContent = (tab === "tv" ? "电视端登录二维码" : "扫码登录二维码");
    $("qr-dock-mode").textContent = modeName(s.mode);
    const st = $("qr-dock-state");
    st.className = "qr-dock-state " + (s.state || "");
    st.textContent = s.message || QR_STATE_TEXT[s.state] || "";
    $("qr-dock-msg").textContent = s.hasImage
      ? "用哔哩哔哩 App 扫描左侧二维码"
      : "正在准备二维码…";
    $("qr-dock-remain").textContent = s.remain > 0 ? s.remain : "--";
  }

  function stopLoginPoll() {
    if (LOGIN.timer) { clearInterval(LOGIN.timer); LOGIN.timer = null; }
    LOGIN.playing = false;
  }

  function startLoginPoll(mode) {
    stopLoginPoll();
    LOGIN.mode = tabOf(mode);
    LOGIN.playing = true;
    LOGIN.timer = setInterval(async () => {
      let s;
      try { s = await api("/api/login/status?mode=" + mode); }
      catch (e) { return; }
      paintLogin(s);
      if (s.state === "confirmed") {
        stopLoginPoll();
        const who = (s.result && s.result.uname) ? "：" + s.result.uname : "";
        toast("登录成功" + who + "，凭证已自动保存", "ok", 6000);
        appendLogs([{ t: 0, m: "登录成功" + who + "，Cookie / access_token 已写入程序目录。" }], false);
        refreshStatus(false);
        setTimeout(() => {
          LOGIN.dockClosed = true;
          $("qr-dock").classList.add("hidden");
        }, 3000);
      } else if (s.state === "error") {
        stopLoginPoll();
        toast(s.message || "登录失败", "err", 6000);
      } else if (!s.active) {
        stopLoginPoll();          // 已结束（取消 / 多次失效 / 超时）
      }
    }, 1200);
  }

  async function startLogin(mode, btn) {
    busy(btn, true);
    try {
      const r = await api("/api/login/start", { mode });
      if (!r.ok) { toast(r.message || "启动失败", "err"); busy(btn, false); return; }
      LOGIN.dockClosed = false;
      LOGIN.lastSeq[tabOf(mode)] = -1;
      appendLogs([{ t: 0, m: "开始" + modeName(mode) + "扫码登录，请在任务台二维码面板中扫码…" }], true);
      $("qr-dock").classList.remove("hidden");
      $("task-state").textContent = "等待扫码…";
      toast("请使用「哔哩哔哩」App 扫描二维码", "warn", 5000);
      startLoginPoll(mode);
    } catch (e) {
      toast("登录出错：" + e.message, "err");
    } finally { busy(btn, false); }
  }

  async function cancelLogin(mode) {
    await api("/api/login/cancel", { mode }).catch(() => {});
    stopLoginPoll();
    LOGIN.dockClosed = true;
    $("qr-dock").classList.add("hidden");
    $("task-state").textContent = "空闲";
    try { paintLogin(await api("/api/login/status?mode=" + mode)); } catch (e) { /* 忽略 */ }
    toast("已取消登录", "warn");
  }

  $("btn-qr-web-start").addEventListener("click", (e) => startLogin("web", e.currentTarget));
  $("btn-qr-tv-start").addEventListener("click", (e) => startLogin("tv", e.currentTarget));
  $("btn-qr-web-cancel").addEventListener("click", () => cancelLogin("web"));
  $("btn-qr-tv-cancel").addEventListener("click", () => cancelLogin("tv"));
  $("qr-dock-close").addEventListener("click", () => {
    LOGIN.dockClosed = true;
    $("qr-dock").classList.add("hidden");
  });

  $("btn-save-cookie").addEventListener("click", async (e) => {
    const val = $("cookie").value.trim();
    if (!val) return toast("请先粘贴 Cookie 字符串", "warn");
    busy(e.currentTarget, true);
    try {
      const r = await api("/api/login/cookie", { cookie: val });
      toast(r.message || (r.ok ? "已保存" : "保存失败"), r.ok ? "ok" : "err", r.ok ? 4000 : 7000);
      if (r.ok) refreshStatus(false);
    } catch (err) {
      toast("保存出错：" + err.message, "err");
    } finally { busy(e.currentTarget, false); }
  });

  $("btn-save-token").addEventListener("click", async (e) => {
    const val = $("access_token").value.trim();
    if (!val) return toast("请先填写 access_token", "warn");
    busy(e.currentTarget, true);
    try {
      const r = await api("/api/login/token", { token: val });
      toast(r.message || (r.ok ? "已保存" : "保存失败"), r.ok ? "ok" : "err");
      if (r.ok) refreshStatus(false);
    } catch (err) {
      toast("保存出错：" + err.message, "err");
    } finally { busy(e.currentTarget, false); }
  });

  $("btn-logout").addEventListener("click", async () => {
    if (!confirm("确定要退出登录吗？\n将清除已保存的 Cookie 与 access_token（BBDown.data / BBDownTV.data）。")) return;
    const r = await api("/api/login/logout", {});
    if (r.ok) {
      stopLoginPoll();
      $("qr-dock").classList.add("hidden");
      $("cookie").value = "";
      $("access_token").value = "";
      toast("已退出登录", "warn");
      refreshStatus(false);
    }
  });

  document.querySelectorAll("#login-tabs .tab").forEach((b) => {
    b.addEventListener("click", () => {
      document.querySelectorAll("#login-tabs .tab").forEach((x) =>
        x.classList.toggle("active", x === b));
      ["web", "tv", "cookie", "token"].forEach((k) =>
        $("panel-" + k).classList.toggle("hidden", k !== b.dataset.tab));
    });
  });

  /* ---------- 目录 ---------- */
  $("btn-open").addEventListener("click", async () => {
    const r = await api("/api/open", { path: $("download_dir").value.trim() });
    if (!r.ok) toast("无法打开目录：" + (r.message || ""), "err");
  });
  $("btn-pick").addEventListener("click", async () => {
    busy($("btn-pick"), true);
    try {
      const r = await api("/api/pick-folder", {});
      if (r.path) { $("download_dir").value = r.path; pushConfig(); toast("已选择目录", "ok"); }
    } catch (e) { toast("选择失败，请手动填写路径", "warn"); }
    busy($("btn-pick"), false);
  });

  $("btn-refresh").addEventListener("click", () => refreshStatus(false));

  $("btn-browser").addEventListener("click", async () => {
    await api("/api/open-browser", {});
    toast("已在系统浏览器中打开", "ok");
  });
  $("btn-quit").addEventListener("click", async () => {
    if (!confirm("确定要退出 BBDown 图形界面吗？\n正在进行的下载会被中断。")) return;
    toast("正在退出…", "warn", 1500);
    try { await api("/api/quit", {}); } catch (e) { /* 进程已退出 */ }
    setTimeout(() => { document.body.innerHTML =
      '<div style="display:flex;height:100vh;align-items:center;justify-content:center;' +
      'font:16px/1.6 system-ui;color:#64748b">程序已退出，可以关闭此窗口。</div>'; }, 400);
  });

  /* ---------- 启动 ---------- */
  refreshStatus(false).then((s) => {
    if (s && !s.ffmpeg.found) {
      toast("未检测到 ffmpeg，BBDown 无法工作。请在「环境与账号」中指定路径或点击「自动下载」。", "warn", 8000);
      $("card-env").open = true;
    }
  });
  api("/api/task?since=0").then((snap) => {
    if (snap && snap.exists && snap.running) {
      taskTag = snap.tag;
      taskId = snap.id;
      appendLogs(snap.logs || [], true);
      paintProgress(snap);
      setTaskUI(true, snap.title);
      startPolling();
    }
  }).catch(() => {});

  // 页面重载后恢复进行中的扫码登录
  ["web", "tv"].forEach((m) => {
    api("/api/login/status?mode=" + m).then((s) => {
      if (s && s.active) {
        LOGIN.dockClosed = false;
        paintLogin(s);
        startLoginPoll(m);
      }
    }).catch(() => {});
  });
})();
