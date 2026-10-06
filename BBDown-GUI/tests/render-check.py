# -*- coding: utf-8 -*-
"""下载进度面板 + 地址识别的确定性渲染自检。

用打桩数据驱动**真实前端**（真 index.html + 真 app.js + 真 style.css + 真字体），
稳定复现「多分P传输中」状态，截图并断言：速度 / 已下载 / 剩余时间 / 阶段步进 /
进度条 / 产物列表都真的渲染出来了，且普通、暗黑两种主题与矮窗口下都不会被压扁。

另外验证「粘贴分享文案」这条链路：`/api/extract`、`/api/clipboard` **不打死桩**，
直接打到真实 server.py，所以这里同时覆盖了「服务端提取规则 + 前端粘贴/提示接线」。
（提取规则本身的用例在 url-extract-check.py，两边不重复。）

为什么不用真实下载抢时机：本机带宽下几十 MB 的分P 常在一两秒内跑完，
而无头 Chrome 启动要 2~3 秒，几乎必然错过传输阶段。打桩则完全可重复。

用法（在项目根目录，或任意目录均可）：
    python BBDown-GUI/tests/render-check.py
脚本会自动在 8813 端口拉起 server.py（若已在运行则直接复用），结束后收工。
截图落在 _shots/ 下。

只在 Windows + 本机 Chrome 环境下可用；找不到 Chrome 会直接报错退出。
"""
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
GUI = os.path.dirname(HERE)                 # BBDown-GUI
ROOT = os.path.dirname(GUI)                 # 项目根（BBDown 图形界面.exe 所在目录）
WEB = os.path.join(GUI, "web")
PORT = 8813
BASE = "http://127.0.0.1:%d" % PORT
OUT = os.path.join(ROOT, "_shots")

CHROME_CANDIDATES = [
    r"C:\Program Files\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Google\Chrome\Application\chrome.exe",
    r"C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe",
]

op = urllib.request.build_opener(urllib.request.ProxyHandler({}))

# ---------------------------------------------------------------- 打桩脚本
STUB = r"""
<script>
(function () {
  /* ---- 捕获脚本错误，写进 __probe 便于定位 ---- */
  window.__errs = [];
  window.addEventListener("error", function (e) {
    window.__errs.push((e.message || "err") + " @ " + (e.filename || "") + ":" + (e.lineno || 0));
  });
  window.addEventListener("unhandledrejection", function (e) {
    window.__errs.push("reject: " + ((e.reason && (e.reason.stack || e.reason.message)) || e.reason));
  });
  var __calls = [];
  var NATIVE_FETCH = window.fetch.bind(window);
  /* 剪贴板内容由 Python 侧注入（__CLIP_*__），避免两处各写一份文案 */
  var CLIP_ONE = __CLIP_ONE__;      // 浏览器剪贴板 API 路径
  var CLIP_SERVER = __CLIP_ONE__;   // 服务端 /api/clipboard 回退路径
  var CLIP_TWO = __CLIP_TWO__;      // 含两个地址
  /* ---- 一次「多分P传输中」的固定快照；速度加正弦扰动让曲线有形状 ---- */
  var N = 0;
  /* 日志元素必须是 {t: 距开始的秒数, m: 文本} —— 与 server.py 的 Task.lines 结构一致 */
  var LOGS = [
    { t: 0.31, m: "开始解析P2: 【4K纪录片】深海之下 · 第2集 (2 of 3)" },
    { t: 2.10, m: "获取aid结束: 117386707474165" },
    { t: 2.53, m: "已选择的流: [视频] avc1.640028 [1080P 高码率] [~286.14 MB]" },
    { t: 2.54, m: "已选择的流: [音频] mp4a.40.2 [192K] [~25.72 MB]" },
    { t: 3.22, m: "开始下载P2视频..." },
    { t: 3.45, m: "开始下载P2音频..." },
    { t: 25.77, m: "正在下载P2视频分片: 00042_117386707474165.P2.117382328616825.vclip" }
  ];
  function jit(k, amp) { return 1 + amp * Math.sin(N / k) + amp * 0.45 * Math.sin(N / (k * 2.7)); }
  function snap() {
    N++;
    var total = 311.86 * 1048576;
    var pct = Math.min(63.4 + N * 0.06, 98.5);
    var downloaded = total * pct / 100;
    var speed = 4.02 * 1048576 * jit(2.3, 0.16);
    return {
      exists: true, id: 7, tag: 9, kind: "download",
      running: true, exitCode: null, queueLeft: 0,
      title: "【4K纪录片】深海之下 · 第2集",
      last: "已下载 %.2f MB / 286.14 MB".replace("%.2f", (downloaded / 1048576).toFixed(2)),
      logs: N <= 1 ? LOGS : [],
      prog: {
        active: true, stage: "video",
        stageText: "正在下载视频流 · 1080P 高码率",
        percent: pct,
        speed: speed,
        eta: Math.round((total - downloaded) / speed),
        page: 2, pages: 3,
        hasVideo: true, hasAudio: true,
        estimated: true,
        downloaded: downloaded, total: total,
        outputs: [], done: false
      }
    };
  }
  var STATUS = {
    bbdownExeExists: true, version: "1.6.3", desktop: true, theme: "__THEME__",
    ffmpeg: { found: true, path: "C:\\Users\\Admin\\AppData\\Local\\BBDownGUI\\ffmpeg\\bin\\ffmpeg.exe" },
    login: { ok: true, uname: "Up主的小粉丝", mid: "699721054", sessdata: "83897028...IIEC",
             hasToken: false, cookieFile: "BBDown.data", tvTokenFile: "BBDownTV.data" },
    config: __CONFIG__
  };
  window.fetch = function (url, opt) {
    var u = String(url), data;
    __calls.push(u);
    // 地址识别走真实服务端：这样「提取规则」也不用在前端再仿一份
    if (u.indexOf("/api/extract") >= 0) return NATIVE_FETCH(url, opt);
    if (u.indexOf("/api/clipboard") >= 0) {
      return Promise.resolve({ ok: true, status: 200,
        json: function () { return Promise.resolve({ ok: true, text: CLIP_SERVER }); } });
    }
    if (u.indexOf("/api/task") >= 0) data = snap();
    else if (u.indexOf("/api/status") >= 0) data = STATUS;
    else if (u.indexOf("/api/login/status") >= 0) data = { active: false };
    else data = { ok: true };
    return Promise.resolve({ ok: true, status: 200, json: function () { return Promise.resolve(data); } });
  };
  function sleep(ms) { return new Promise(function (r) { setTimeout(r, ms); }); }
  function fakeClip(text) {
    Object.defineProperty(navigator, "clipboard", { configurable: true,
      value: { readText: function () {
        return text === null ? Promise.reject(new Error("NotAllowedError")) : Promise.resolve(text);
      } } });
  }
  /* 逐个点「粘贴」，验证 3 条路径：浏览器剪贴板 / 服务端回退 / 多地址 */
  async function checkPaste() {
    var out = {};
    var box = document.getElementById("urls");
    var note = document.getElementById("url-note");
    var btn = document.getElementById("btn-paste");
    if (!btn) { out.error = "找不到 #btn-paste"; return out; }
    out.hasBtn = true;
    async function step(name, clip) {
      box.value = "";
      fakeClip(clip);
      btn.click();
      // 不要只 sleep 固定时长：/api/extract 是真网络请求，虚拟时钟与它的完成顺序不定
      for (var i = 0; i < 60; i++) {
        await sleep(50);
        if ((note.textContent || "").length || box.value.length) break;
      }
      await sleep(200);
      out[name] = { box: box.value, note: note.textContent,
                    cls: note.className, hidden: note.classList.contains("hidden") };
    }
    await step("browserApi", CLIP_ONE);
    await step("serverFallback", null);          // 剪贴板 API 被拒 → 走 /api/clipboard
    await step("multi", CLIP_TWO);
    return out;                                  // 保留最后一次结果，截图里能看到效果
  }
  window.addEventListener("load", function () {
    setTimeout(function () {
      var probe = {};
      ["console", "task-stats", "ps-phase", "ps-spark-wrap", "task-outputs"].forEach(function (id) {
        var el = document.getElementById(id);
        if (!el) { probe[id] = "MISSING"; return; }
        var cs = getComputedStyle(el);
        probe[id] = { bg: cs.backgroundColor, color: cs.color, disp: cs.display,
                      cls: el.className,
                      h: Math.round(el.getBoundingClientRect().height),
                      txt: (el.textContent || "").slice(0, 46) };
      });
      probe.__theme = document.documentElement.getAttribute("data-theme");
      probe.__errs = window.__errs;
      probe.__calls = __calls.slice(0, 12);
      probe.__n = __calls.length;
      probe.__text = {
        speed: (document.getElementById("ps-speed") || {}).textContent,
        size: (document.getElementById("ps-size") || {}).textContent,
        eta: (document.getElementById("ps-eta") || {}).textContent,
        phase: (document.getElementById("ps-phase") || {}).textContent
      };
      probe.__sparkPts = (document.getElementById("ps-spark-line") || {}).getAttribute
        ? document.getElementById("ps-spark-line").getAttribute("points").split(" ").length : 0;
      // 粘贴链路放在最后跑：它会改动输入框，不能影响上面进度面板的取值
      checkPaste().then(function (res) {
        probe.__addr = res;
        finish(probe);
      }).catch(function (e) {
        probe.__addr = { error: String((e && e.stack) || e) };
        finish(probe);
      });
    }, 1400);
  });
  function finish(probe) {
    var d = document.createElement("div");
    d.id = "__probe";
    d.style.display = "none";
    d.textContent = JSON.stringify(probe);
    document.body.appendChild(d);
  }
})();
</script>
"""

CONFIG_FOR_STUB = {
    "bbdown_exe": "", "ffmpeg_path": "", "download_dir": "D:\\BBDown下载",
    "api": "web", "dfn_priority": "8K 超高清,4K 超清,1080P 高码率,1080P 高清",
    "encoding_priority": "hevc,av1,avc", "language": "", "select_page": "",
    "file_pattern": "", "delay_per_page": "", "extra_args": "", "aria2c_path": "",
    "cookie": "", "access_token": "", "theme": "light",
}


SHARE_ONE = ("【Hsin let's Groove!】 https://www.bilibili.com/video/BV12eHa6RE59/"
             "?share_source=copy_web&vd_source=948b033cbf8aaa1bf41502231246ba13")
SHARE_ONE_CLEAN = SHARE_ONE[SHARE_ONE.index("https://"):]
# 第二段分享文案：一段话里两个地址
SHARE_TWO = SHARE_ONE + "\n【B】 BV16VHL6NEQe 也看看这个"
SHARE_TWO_CLEAN = SHARE_ONE_CLEAN + "\nBV16VHL6NEQe"
APPJS_PATCHED = "_preview_app.js"
_server = None


def find_chrome():
    for p in CHROME_CANDIDATES:
        if os.path.isfile(p):
            return p
    sys.exit("找不到 Chrome/Edge，无法做渲染自检（可修改 CHROME_CANDIDATES）")


CHROME = find_chrome()


def server_alive(timeout=1.0):
    try:
        op.open(BASE + "/api/status", timeout=timeout).read()
        return True
    except (urllib.error.URLError, OSError):
        return False


def ensure_server(wait=30):
    """8813 没在跑就把 server.py 拉起来；已在跑则直接复用。"""
    global _server
    if server_alive():
        print("复用已在运行的服务：%s" % BASE)
        return
    log = open(os.path.join(ROOT, "_render-check-srv.log"), "w", encoding="utf-8")
    _server = subprocess.Popen([sys.executable, "server.py", "--no-browser", "--port", str(PORT)],
                               cwd=GUI, stdout=log, stderr=subprocess.STDOUT)
    log.close()          # 子进程已拿到自己的句柄；不关掉的话 Windows 下删不掉这个日志
    for _ in range(int(wait / 0.5)):
        if server_alive():
            print("已启动测试服务：%s" % BASE)
            return
        if _server.poll() is not None:
            sys.exit("server.py 启动即退出，见 _render-check-srv.log")
        time.sleep(0.5)
    sys.exit("等待服务就绪超时（%ds）" % wait)


def stop_server():
    if _server and _server.poll() is None:
        _server.terminate()
        try:
            _server.wait(timeout=6)
        except subprocess.TimeoutExpired:
            _server.kill()


def build_appjs():
    """产出一份「摘掉启动链 catch 兜底」的 app.js。

    真实 app.js 里 `api("/api/task?since=0").then(...).catch(() => {})` 会把渲染异常
    静默吞掉（上一版就因此排查了半天）。验证时摘掉它，异常就能冒到 unhandledrejection。
    只用于验证，不影响产品代码。
    """
    src = os.path.join(WEB, "app.js")
    js = open(src, encoding="utf-8").read()
    MARK = "/* ---------- 启动 ---------- */"
    DEAD = ".catch(() => {});"
    k = js.index(MARK)
    j = js.index(DEAD, k)
    js = js[:j] + ";" + js[j + len(DEAD):]
    open(os.path.join(WEB, APPJS_PATCHED), "w", encoding="utf-8").write(js)


def build_page(theme, name):
    src = os.path.join(WEB, "index.html")
    html = open(src, encoding="utf-8").read()
    # 主题预置：直接改那个内联脚本，保证首帧就是目标主题（不闪白）
    html = html.replace(
        '    var t = localStorage.getItem("bbdown-theme");\n',
        '    try{localStorage.setItem("bbdown-theme","%s");}catch(e){}\n    var t = "%s";\n' % (theme, theme),
        1)
    assert 'localStorage.setItem("bbdown-theme","%s")' % theme in html, "主题预置注入失败"
    stub = (STUB.replace("__THEME__", theme)
                .replace("__CONFIG__", json.dumps(CONFIG_FOR_STUB, ensure_ascii=False))
                .replace("__CLIP_ONE__", json.dumps(SHARE_ONE, ensure_ascii=False))
                .replace("__CLIP_TWO__", json.dumps(SHARE_TWO, ensure_ascii=False)))
    assert "__CLIP_ONE__" not in stub and "__CLIP_TWO__" not in stub, \
        "剪贴板文案占位符没替换干净"
    tag = '<script src="/static/app.js"></script>'
    assert tag in html, "未找到 app.js 引入标签"
    html = html.replace(tag, stub + '\n<script src="/static/%s"></script>' % APPJS_PATCHED, 1)
    dst = os.path.join(WEB, name)
    open(dst, "w", encoding="utf-8").write(html)
    return dst


def shot(url, out):
    subprocess.run([CHROME, "--headless=new", "--disable-gpu", "--no-proxy-server",
                    "--hide-scrollbars", "--window-size=1460,1000",
                    "--virtual-time-budget=9000",
                    "--screenshot=" + out, url],
                   capture_output=True, timeout=120)
    return os.path.isfile(out) and os.path.getsize(out) > 0


def unescape_html(s):
    """把 --dump-dom 序列化时转义的字符还原。

    注意 `&amp;` 必须最后换：先换会把 `&amp;quot;` 二次解码成 `"`。地址里带 `&vd_source=`
    这类参数，漏了这一步会得到 `&amp;vd_source=`，断言就会假失败。
    """
    for a, b in (("&lt;", "<"), ("&gt;", ">"), ("&quot;", '"'), ("&#39;", "'"),
                 ("&amp;", "&")):
        s = s.replace(a, b)
    return s


def probe(url, size="1460,1000"):
    """注意：必须与截图用同样的 --window-size。

    之前漏了这一步，探针跑在默认 800x600 下，量到 .pstage 高 0，
    一度以为是主题配色问题——实际是矮窗口触发了 flex 压缩（已修）。
    """
    r = subprocess.run([CHROME, "--headless=new", "--disable-gpu", "--no-proxy-server",
                        "--hide-scrollbars", "--window-size=" + size,
                        "--virtual-time-budget=9000", "--dump-dom", url],
                       capture_output=True, timeout=120)
    dom = r.stdout.decode("utf-8", "replace")
    m = re.search(r'<div id="__probe"[^>]*>(.*?)</div>', dom, re.S)
    if not m:
        return None
    return json.loads(unescape_html(m.group(1)))


def check_addr(theme, pr):
    """校验「粘贴分享文案 → 提取地址 → 提示」这条链路。

    /api/extract、/api/clipboard 都打到真实 server.py，所以这里同时验证了
    服务端提取结果是否真的进了输入框、提示文案是否渲染出来。
    """
    a = pr.get("__addr") or {}
    if not a.get("hasBtn"):
        return ["%s: 找不到「粘贴」按钮（#btn-paste）" % theme]
    if a.get("error"):
        return ["%s: 粘贴流程抛错 —— %s" % (theme, a["error"])]
    fails = []
    cases = (
        ("浏览器剪贴板", "browserApi", SHARE_ONE_CLEAN, "地址"),
        ("服务端回退", "serverFallback", SHARE_ONE_CLEAN, "地址"),
        ("多地址", "multi", SHARE_TWO_CLEAN, "2 个地址"),
    )
    for label, key, want_box, want_note in cases:
        v = a.get(key) or {}
        print("  [粘贴·%s] 输入框=%r" % (label, (v.get("box") or "")[:78]))
        print("               提示=%r" % ((v.get("note") or "")[:60]))
        if v.get("box") != want_box:
            fails.append("%s: [%s] 提取结果不对（期望 %r）" % (theme, label, want_box))
        if v.get("hidden") or not (v.get("note") or "").strip():
            fails.append("%s: [%s] 没有显示识别提示" % (theme, label))
        elif want_note not in v.get("note", ""):
            fails.append("%s: [%s] 提示文案里没有 %r" % (theme, label, want_note))
    return fails


def main():
    os.makedirs(OUT, exist_ok=True)
    ensure_server()
    build_appjs()
    pages = {
        "light": build_page("light", "_preview_prog_light.html"),
        "dark": build_page("dark", "_preview_prog_dark.html"),
    }
    print("已生成打桩预览页：", ", ".join(os.path.basename(p) for p in pages.values()))

    fails = []
    for theme in ("light", "dark"):
        url = BASE + "/static/" + os.path.basename(pages[theme])
        out = os.path.join(OUT, "progress-%s.png" % theme)
        ok = shot(url, out)
        pr = probe(url)
        print("\n== %s ==" % theme)
        print("  截图 %s（%d 字节）" % ("OK" if ok else "失败",
                                      os.path.getsize(out) if ok else 0))
        if pr:
            t = pr.get("__text") or {}
            print("  主题属性        :", pr.get("__theme"))
            print("  速度/已下载/剩余 :", t.get("speed"), "|", t.get("size"), "|", t.get("eta"))
            print("  阶段文案        :", t.get("phase"))
            print("  曲线点数        :", pr.get("__sparkPts"))
            print("  fetch 调用 %d 次 : %s" % (pr.get("__n", 0), pr.get("__calls")))
            for e in (pr.get("__errs") or []):
                print("  [JS 错误]       :", e)
            for k in ("console", "task-stats", "ps-phase", "ps-spark-wrap", "task-outputs"):
                v = pr.get(k)
                if isinstance(v, dict):
                    print("  %-13s disp=%-6s h=%-4s bg=%-21s cls=%s | %s"
                          % (k, v["disp"], v["h"], v["bg"], v["cls"], v["txt"]))
                else:
                    print("  %-13s %s" % (k, v))
            fails += check_addr(theme, pr)
            # 断言：主题与关键数值必须真的落到 DOM 上
            if pr.get("__theme") != theme:
                fails.append("%s: 主题未生效" % theme)
            if t.get("speed") in (None, "--"):
                fails.append("%s: 速度未渲染" % theme)
            if not (t.get("phase") or "").startswith("正在下载"):
                fails.append("%s: 阶段文案未渲染" % theme)
            if (pr.get("__n") or 0) < 3:
                fails.append("%s: 轮询未启动" % theme)
            if pr.get("__errs"):
                fails.append("%s: 有 JS 报错" % theme)
            for k in ("task-stats", "ps-phase"):
                v = pr.get(k) or {}
                if v.get("h", 0) <= 0:
                    fails.append("%s: %s 高度为 0（内容被压扁）" % (theme, k))
        else:
            fails.append("%s: 未取到 __probe" % theme)

    # 矮窗口回归：曾因 flex 的 min-height:auto 退化为 0，把阶段文案整行压没
    print("\n== 矮窗口回归（1460x620，普通主题）==")
    short = probe(BASE + "/static/" + os.path.basename(pages["light"]), "1460,620")
    if not short:
        fails.append("矮窗口: 未取到 __probe")
    else:
        for k in ("ps-phase", "task-stats", "console", "ps-spark-wrap"):
            v = short.get(k) or {}
            print("  %-13s disp=%-6s h=%s" % (k, v.get("disp"), v.get("h")))
            if v.get("h", 0) <= 0:
                fails.append("矮窗口: %s 被压成 0 高" % k)
        print("  阶段文案        :", (short.get("__text") or {}).get("phase"))

    print("\n" + "=" * 46)
    if fails:
        print("结果：%d 项未通过" % len(fails))
        for f in fails:
            print("  x", f)
    else:
        print("结果：全部通过（进度条/速度/剩余时间/阶段/产物在两种主题与矮窗口下都正常；"
              "粘贴分享文案的提取与提示也正常）")
    return (1 if fails else 0), pages


def cleanup(pages, srv_log):
    for p in list(pages.values()) + [os.path.join(WEB, APPJS_PATCHED)]:
        try:
            os.remove(p)
        except OSError:
            pass
    # 服务是我们自己拉起的才删日志；复用了用户的服务就别动它的文件
    if _server is not None and os.path.isfile(srv_log):
        try:
            os.remove(srv_log)
        except OSError:
            pass


if __name__ == "__main__":
    srv_log = os.path.join(ROOT, "_render-check-srv.log")
    code = 1
    pages = {}
    try:
        code, pages = main()
    finally:
        stop_server()          # 先停服务，再删日志（Windows 下进程占用的文件删不掉）
        cleanup(pages, srv_log)
    sys.exit(code)
