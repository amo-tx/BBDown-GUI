#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
BBDown 图形界面 —— 本地服务端

纯 Python 标准库实现，无需安装任何第三方依赖。
职责：
  * 托管前端页面（web/ 目录）
  * 封装 BBDown.exe 的解析 / 下载 / 登录命令
  * 以流式方式推送实时日志与下载进度
  * 检测并（可选）自动下载 ffmpeg
  * 持久化用户配置到 config.json

运行:  python server.py [--port 8765] [--no-browser]
"""

import argparse
import ctypes
import json
import os
import re
import shutil
import socket
import subprocess
import sys
import threading
import time
import webbrowser
import zipfile
import io
import urllib.request
from http.server import ThreadingHTTPServer, BaseHTTPRequestHandler
from urllib.parse import urlparse, parse_qs

# 本地模块（与 server.py 同目录）：
#   qrcodegen  —— 纯标准库二维码编码器（生成登录二维码图片）
#   bili_auth  —— 哔哩哔哩 Web / 电视端扫码登录（不依赖 BBDown 的控制台二维码）
#   dlprogress —— 从磁盘分片增长推算下载进度（BBDown 重定向时不打印任何进度）
import qrcodegen
import bili_auth
import dlprogress

# --------------------------------------------------------------------------- #
# 路径与常量
# --------------------------------------------------------------------------- #
def _detect_paths():
    """区分「源码运行」与「PyInstaller 打包运行」两套路径。

    打包后 sys.executable 指向 exe，只读资源被解压到 sys._MEIPASS；
    配置/工具/下载等可写内容必须落在 exe 同级，不能写进临时解压目录。
    """
    if getattr(sys, "frozen", False):
        data_dir = os.path.dirname(os.path.abspath(sys.executable))
        bundle_dir = getattr(sys, "_MEIPASS", data_dir)
        return True, data_dir, bundle_dir
    here = os.path.dirname(os.path.abspath(__file__))
    return False, here, here


FROZEN, DATA_DIR, BUNDLE_DIR = _detect_paths()
APP_DIR = DATA_DIR                              # 兼容旧引用

WEB_DIR = os.path.join(BUNDLE_DIR, "web")       # 只读：前端页面（打包后位于 _MEIPASS）
TOOLS_DIR = os.path.join(DATA_DIR, "tools")     # 可写：内置工具
CONFIG_PATH = os.path.join(DATA_DIR, "config.json")
STATIC_DIR = os.path.join(DATA_DIR, "static")
ASSETS_DIR = os.path.join(BUNDLE_DIR, "assets")  # 只读：图标等资源
ICON_PATH = os.path.join(ASSETS_DIR, "icon.ico")


def _locate_workspace():
    """定位 BBDown.exe 所在目录：先看自身，再看上一级，最后回退自身。"""
    for d in (DATA_DIR, os.path.dirname(DATA_DIR)):
        if os.path.isfile(os.path.join(d, "BBDown.exe")):
            return d
    return DATA_DIR


WORKSPACE = _locate_workspace()                 # BBDown.exe 所在目录

DEFAULT_BBDOWN = os.path.join(WORKSPACE, "BBDown.exe")
DEFAULT_DOWNLOAD_DIR = os.path.join(WORKSPACE, "downloads")

# ffmpeg 自动下载源（按顺序尝试）
FFMPEG_SOURCES = [
    ("gyan.dev (essentials)",
     "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"),
    ("BtbN (GPL)",
     "https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/"
     "ffmpeg-master-latest-win64-gpl.zip"),
]

CREATE_NO_WINDOW = 0x08000000 if os.name == "nt" else 0

ANSI_RE = re.compile(r"\x1b\[[0-9;?]*[a-zA-Z]")
PERCENT_RE = re.compile(r"(\d{1,3}(?:\.\d+)?)\s*%")

DEFAULT_CONFIG = {
    "bbdown_exe": DEFAULT_BBDOWN,
    "ffmpeg_path": "",
    "download_dir": DEFAULT_DOWNLOAD_DIR,
    "api": "web",                       # web | tv | app | intl
    "dfn_priority": "8K 超高清,4K 超清,1080P 高码率,1080P 高清,720P 高清",
    "encoding_priority": "hevc,av1,avc",
    "language": "",
    "select_page": "",
    "file_pattern": "",
    "video_only": False,
    "audio_only": False,
    "danmaku_only": False,
    "sub_only": False,
    "cover_only": False,
    "download_danmaku": False,
    "skip_subtitle": False,
    "skip_cover": False,
    "skip_mux": False,
    "interactive": False,
    "use_aria2c": False,
    "aria2c_path": "",
    "save_archives_to_file": False,
    "delay_per_page": "",
    "force_http": True,                 # BBDown 默认：下载时强制 HTTP
    "force_replace_host": True,         # BBDown 默认：强制替换下载 host
    "allow_pcdn": False,
    "compat_mode": False,               # 兼容模式：关闭上面两项（下载 403 时自动启用）
    "cookie": "",
    "access_token": "",
    "extra_args": "",
    "theme": "light",                   # light（普通） | dark（暗黑）
}

_lock = threading.RLock()
_TASK_SEQ = [0]                      # 任务序号：前端据此发现「换了一个新任务」
QUEUE = {"left": 0}                  # 多地址批量下载时，还在排队的地址数


# --------------------------------------------------------------------------- #
# 配置读写
# --------------------------------------------------------------------------- #
def load_config():
    cfg = dict(DEFAULT_CONFIG)
    if os.path.isfile(CONFIG_PATH):
        try:
            with open(CONFIG_PATH, "r", encoding="utf-8") as f:
                cfg.update(json.load(f))
        except Exception:
            pass
    return cfg


def save_config(cfg):
    with open(CONFIG_PATH, "w", encoding="utf-8") as f:
        json.dump(cfg, f, ensure_ascii=False, indent=2)


CONFIG = load_config()


def sanitize_config(cfg):
    """自愈配置里失效的路径与取值。

    配置可能是在别的机器/别的目录下生成的（例如换盘后 BBDown.exe 路径、
    空的保存目录），此时程序会「明明文件就在旁边却说找不到」。这里统一纠正。
    返回是否有改动。
    """
    changed = False
    exe = (cfg.get("bbdown_exe") or "").strip()
    if not os.path.isfile(exe) and os.path.isfile(DEFAULT_BBDOWN):
        if exe != DEFAULT_BBDOWN:
            cfg["bbdown_exe"] = DEFAULT_BBDOWN
            changed = True
    if not (cfg.get("download_dir") or "").strip():
        cfg["download_dir"] = DEFAULT_DOWNLOAD_DIR
        changed = True
    if cfg.get("theme") not in ("light", "dark"):
        cfg["theme"] = "light"
        changed = True
    if not isinstance(cfg.get("cookie"), str):
        cfg["cookie"] = ""
        changed = True
    return changed


if sanitize_config(CONFIG):
    try:
        save_config(CONFIG)
    except OSError:
        pass


def cfg_set(patch):
    with _lock:
        CONFIG.update({k: v for k, v in patch.items() if k in DEFAULT_CONFIG})
        save_config(CONFIG)
    return CONFIG


# --------------------------------------------------------------------------- #
# ffmpeg 探测
# --------------------------------------------------------------------------- #
def _usable(path):
    return bool(path) and os.path.isfile(path)


def find_ffmpeg():
    """按优先级返回可用的 ffmpeg.exe 路径，找不到返回 ''。"""
    cand = []
    if CONFIG.get("ffmpeg_path"):
        cand.append(CONFIG["ffmpeg_path"])
    which = shutil.which("ffmpeg")
    if which:
        cand.append(which)
    cand += [
        os.path.join(WORKSPACE, "ffmpeg.exe"),
        os.path.join(WORKSPACE, "bin", "ffmpeg.exe"),
        os.path.join(WORKSPACE, "tools", "ffmpeg", "bin", "ffmpeg.exe"),
        os.path.join(TOOLS_DIR, "ffmpeg", "bin", "ffmpeg.exe"),
        os.path.join(APP_DIR, "ffmpeg.exe"),
    ]
    # 扫描 workspace 下形如 ffmpeg*/bin/ffmpeg.exe 的目录
    try:
        for name in os.listdir(WORKSPACE):
            p = os.path.join(WORKSPACE, name)
            if os.path.isdir(p) and name.lower().startswith("ffmpeg"):
                cand.append(os.path.join(p, "bin", "ffmpeg.exe"))
                cand.append(os.path.join(p, "ffmpeg.exe"))
    except OSError:
        pass
    for p in cand:
        if _usable(p):
            return os.path.abspath(p)
    return ""


COOKIE_FILE = os.path.join(WORKSPACE, "BBDown.data")      # Web 端 cookie
TV_TOKEN_FILE = os.path.join(WORKSPACE, "BBDownTV.data")  # 电视端 access_token


def _first_existing(names):
    for d in (WORKSPACE, APP_DIR, os.getcwd()):
        for n in names:
            p = os.path.join(d, n)
            if os.path.isfile(p):
                return p
    return ""


def cookie_file():
    """BBDown 登录后写入的 cookie 文件（探测常见位置）。"""
    return _first_existing(("BBDown.data",))


def tv_token_file():
    """电视端登录后写入的凭证文件。"""
    return _first_existing(("BBDownTV.data",))


def _read_text(path):
    try:
        with open(path, "r", encoding="utf-8", errors="replace") as f:
            return f.read().strip()
    except OSError:
        return ""


def effective_cookie():
    """界面配置优先，其次读取 BBDown.data。"""
    cfg = (CONFIG.get("cookie") or "").strip()
    if cfg:
        return cfg
    p = cookie_file()
    return _read_text(p) if p else ""


def effective_token():
    cfg = (CONFIG.get("access_token") or "").strip()
    if cfg:
        return cfg
    p = tv_token_file()
    return _read_text(p) if p else ""


def has_login():
    return bool(effective_cookie()) or bool(effective_token())


def write_text(path, text):
    with open(path, "w", encoding="utf-8") as f:
        f.write(text)


def remove_quiet(path):
    try:
        if path and os.path.isfile(path):
            os.remove(path)
    except OSError:
        pass


_ACCOUNT = {"cookie": None, "ok": False, "uname": "", "mid": 0,
            "at": 0.0, "message": ""}


def account_info(force=False, ttl=120):
    """带缓存的账号信息查询（用 cookie 调 nav 接口拿昵称）。"""
    cookie = effective_cookie()
    now = time.time()
    if (not force and _ACCOUNT["cookie"] == cookie
            and now - _ACCOUNT["at"] < ttl):
        return dict(_ACCOUNT)
    if not cookie:
        _ACCOUNT.update(cookie=cookie, ok=False, uname="", mid=0,
                        at=now, message="未登录")
        return dict(_ACCOUNT)
    ok, uname, mid, msg = bili_auth.fetch_nav(cookie)
    _ACCOUNT.update(cookie=cookie, ok=ok, uname=uname, mid=mid,
                    at=now, message=msg)
    return dict(_ACCOUNT)


def login_status(with_account=False):
    """汇总登录状态，供界面显示。"""
    cf, tf = cookie_file(), tv_token_file()
    cookie, token = effective_cookie(), effective_token()
    info = {
        "ok": bool(cookie) or bool(token),
        "hasCookie": bool(cookie),
        "hasToken": bool(token),
        "cookieFile": cf,
        "tvTokenFile": tf,
        "cookieSource": ("config" if (CONFIG.get("cookie") or "").strip()
                         else ("file" if cf and cookie else "")),
        "sessdata": "",
        "mid": "",
        "uname": "",
        "nickChecked": False,
    }
    d = bili_auth.cookie_to_dict(cookie)
    sd = d.get("SESSDATA") or ""
    if sd:
        info["sessdata"] = sd[:6] + "…" + sd[-4:] if len(sd) > 12 else "已设置"
    info["mid"] = d.get("DedeUserID") or ""
    if with_account and cookie:
        acc = account_info()
        info["uname"] = acc.get("uname") or ""
        info["nickChecked"] = True
        info["accountMessage"] = acc.get("message") or ""
        if acc.get("mid"):
            info["mid"] = acc["mid"]
    return info


# --------------------------------------------------------------------------- #
# 任务（解析 / 下载 / 登录 共用）
# --------------------------------------------------------------------------- #
class Task(object):
    """封装一次 BBDown 调用，支持流式读取输出。"""

    MAX_LINES = 20000
    # 控制台二维码用的方块字符：含这些字符的行按「字符画」原样保留，
    # 既不能截断也不能丢空行，否则图形会错位、无法扫描。
    QR_CHARS = "\u2588\u2580\u2584\u258c\u2590\u25a0\u25a1\u25cf\u25cb\u2593\u2592\u2591"

    def __init__(self, title, kind):
        self.title = title
        self.kind = kind                  # info | download | login
        with _lock:
            _TASK_SEQ[0] += 1
            self.id = _TASK_SEQ[0]
        self.lines = []
        self.tag = 0                      # 已产生日志条数
        self.progress = 0.0
        self.running = False
        self.exit_code = None
        self.started = time.time()
        self.finished = None
        self.error = ""
        self.proc = None
        self.request = None              # 发起下载时的原始请求体（用于自动降级重试）
        self.retried = False
        self._buf = b""
        self._qr_streak = 0              # 字符画二维码区域内剩余可保留的空行数
        self.tracker = None              # dlprogress.DownloadProgress（仅下载类任务）
        self.prog = None                 # 最新进度快照，供前端读取
        self._lock = threading.Lock()

    # ---- 日志写入 -------------------------------------------------------- #
    def push(self, text):
        text = ANSI_RE.sub("", text).replace("\x00", "").rstrip("\r\n")

        # 进度推算先看原始行：二维码字符画 / 空行等后面会被过滤掉，但阶段标记
        # （「开始下载P1视频…」「已选择的流」等）必须全部送进去。
        if self.tracker is not None:
            try:
                self.tracker.feed(text)
            except Exception:                          # noqa: BLE001
                pass

        has_block = bool(text) and any(ch in self.QR_CHARS for ch in text)
        if has_block:
            is_qr = True
            self._qr_streak = 6          # 紧跟其后的空行属于二维码静默区
        elif self._qr_streak > 0 and not text.strip():
            is_qr = True
            self._qr_streak -= 1
        else:
            is_qr = False
            self._qr_streak = 0

        if not text.strip() and not is_qr:
            return
        if len(text) > 400 and not is_qr:            # 避免超长流地址刷屏
            text = text[:400] + " …"

        with self._lock:
            entry = {"t": round(time.time() - self.started, 2), "m": text}
            if is_qr:
                entry["q"] = 1
            self.lines.append(entry)
            if len(self.lines) > self.MAX_LINES:
                self.lines = self.lines[-self.MAX_LINES:]
            self.tag += 1
            if not is_qr:
                m = PERCENT_RE.search(text)
                if m:
                    try:
                        v = float(m.group(1))
                        if 0 <= v <= 100:
                            self.progress = v
                    except ValueError:
                        pass

    def feed(self, chunk):
        """按 \\r / \\n 切分字节流（下载进度靠 \\r 刷新同一行）。"""
        self._buf += chunk
        *done, self._buf = re.split(rb"[\r\n]", self._buf)
        for raw in done:
            if raw:
                self.push(decode(raw))
        # 行过长保护
        if len(self._buf) > 65536:
            self.push(decode(self._buf))
            self._buf = b""

    def flush(self):
        if self._buf:
            self.push(decode(self._buf))
            self._buf = b""

    def snapshot(self, since=0):
        with self._lock:
            return {
                "id": self.id,
                "title": self.title,
                "kind": self.kind,
                "running": self.running,
                "exitCode": self.exit_code,
                "progress": round(self.progress, 2),
                "tag": self.tag,
                "logs": self.lines[since:],
                "last": self.lines[-1]["m"] if self.lines else "",
                "error": self.error,
                "elapsed": round(time.time() - self.started, 1),
                "prog": self.prog,
            }


def decode(raw):
    for enc in ("utf-8", "gbk"):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode("utf-8", "replace")


CURRENT = {"task": None}


def _watch_download_progress(task):
    """周期性把磁盘上的分片增长换算成进度，写入 task.prog。"""
    tracker = task.tracker
    if tracker is None:
        return
    interval = dlprogress.DownloadProgress.POLL
    try:
        while task.running:
            tracker.tick()
            task.prog = tracker.snapshot()
            time.sleep(interval)
    except Exception:                                   # noqa: BLE001
        pass
    # 收尾：再采一次，让界面拿到最终体积与产物清单
    try:
        tracker.finish(task.exit_code == 0)
        task.prog = tracker.snapshot()
    except Exception:                                   # noqa: BLE001
        pass


def start_task(title, kind, args, cwd=None, env=None, retry_fn=None):
    """启动一个 BBDown 进程并持续读取输出。返回 (task, err)。

    retry_fn(task, exit_code) -> 新的参数列表或 None；
    若非 None，则在同一任务内换用新参数重新拉起进程（用于 403 自动降级重试）。
    """
    with _lock:
        old = CURRENT["task"]
        if old and old.running:
            return None, "已有任务正在运行，请先停止或等待其完成。"

    exe = CONFIG.get("bbdown_exe") or DEFAULT_BBDOWN
    if not os.path.isfile(exe):
        return None, "找不到 BBDown.exe：%s" % exe

    task = Task(title, kind)
    cwd = cwd or WORKSPACE

    def spawn(a):
        return subprocess.Popen(
            [exe] + a,
            cwd=cwd,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            stdin=subprocess.DEVNULL,
            bufsize=0,
            env=env,
            creationflags=CREATE_NO_WINDOW,
        )

    try:
        proc = spawn(args)
    except Exception as exc:                       # noqa: BLE001
        return None, "启动失败: %s" % exc

    task.proc = proc
    task.running = True
    with _lock:
        CURRENT["task"] = task

    # 下载类任务：BBDown 在输出被重定向时不打印任何进度，改由磁盘分片增长推算。
    # 必须赶在输出泵启动之前把 tracker 建好，否则最早的几行会被漏掉。
    if kind == "download":
        task.tracker = dlprogress.DownloadProgress(
            CONFIG.get("download_dir") or WORKSPACE, task.started)
        task.prog = task.tracker.snapshot()
        threading.Thread(target=_watch_download_progress, args=(task,),
                         daemon=True).start()

    def pump():
        nonlocal proc
        try:
            while True:
                try:
                    fd = proc.stdout.fileno()
                    while True:
                        chunk = os.read(fd, 8192)
                        if not chunk:
                            break
                        task.feed(chunk)
                except Exception:                  # noqa: BLE001
                    pass
                task.flush()
                code = proc.wait()
                # ---- 失败后是否自动降级重试 ---- #
                new_args = None
                if code != 0 and retry_fn and not task.retried:
                    task.retried = True
                    try:
                        new_args = retry_fn(task, code)
                    except Exception:              # noqa: BLE001
                        new_args = None
                if new_args:
                    task.push("[兼容模式] 检测到下载被拒绝（403），"
                              "已自动关闭 host/协议替换并重试…")
                    try:
                        proc = spawn(new_args)
                        task.proc = proc
                        task.progress = 0.0
                        continue
                    except Exception as exc:       # noqa: BLE001
                        task.push("[兼容模式] 重试启动失败：%s" % exc)
                task.exit_code = code
                break
        finally:
            task.running = False
            task.finished = time.time()
            if task.exit_code is None:
                task.exit_code = 0
            if task.exit_code != 0:
                task.push("[进程退出] 返回码 %s" % task.exit_code)

    threading.Thread(target=pump, daemon=True).start()
    return task, None


def _compat_retry(task, code):
    """下载因 403 失败时的降级策略：关闭 force-http / force-replace-host 重试。"""
    if task.kind != "download":
        return None
    body = task.request or {}
    if body.get("compat_mode") or CONFIG.get("compat_mode"):
        return None
    text = "\n".join(l["m"] for l in task.lines)
    if not re.search(r"\b403\b|Forbidden", text):
        return None
    targets = extract_targets(address_text(body))
    if not targets:
        return None
    return build_args(targets[0], "download", page=body.get("selectPage"),
                      overrides={"compat_mode": True})


# --------------------------------------------------------------------------- #
# 扫码登录会话（Web 端 / 电视端）
# --------------------------------------------------------------------------- #
LOGIN_LOCK = threading.RLock()
LOGIN_SESSIONS = {"web": None, "tv": None}


def _on_login_success(result):
    """登录成功后的落盘：cookie → BBDown.data，TV token → BBDownTV.data。"""
    cookie = (result.get("cookie") or "").strip()
    token = (result.get("token") or "").strip()
    patch = {}
    if cookie:
        patch["cookie"] = cookie
        try:
            write_text(COOKIE_FILE, cookie)
        except OSError:
            pass
    if token:
        patch["access_token"] = token
        try:
            write_text(TV_TOKEN_FILE, token)
        except OSError:
            pass
    if patch:
        cfg_set(patch)
    _ACCOUNT.update(cookie=None, at=0.0)          # 账号缓存立即失效


def start_login(mode):
    """启动（或复用）一次扫码登录流程。"""
    mode = "tv" if mode == "tv" else "web"
    with LOGIN_LOCK:
        for k, s in LOGIN_SESSIONS.items():
            if k != mode and s is not None:
                s.cancel()                        # 两种方式互斥
        cur = LOGIN_SESSIONS.get(mode)
        if cur is not None and cur.snapshot().get("active"):
            return cur, ""
        cur = bili_auth.LoginSession(mode=mode, on_success=_on_login_success)
        LOGIN_SESSIONS[mode] = cur
    ok, err = cur.start()
    if not ok:
        return None, err or "无法启动登录流程"
    return cur, ""


def login_session(mode):
    mode = "tv" if mode == "tv" else "web"
    with LOGIN_LOCK:
        return LOGIN_SESSIONS.get(mode)


def cancel_login(mode=None):
    modes = ["tv", "web"] if mode is None else ["tv" if mode == "tv" else "web"]
    with LOGIN_LOCK:
        for m in modes:
            s = LOGIN_SESSIONS.get(m)
            if s is not None:
                s.cancel()
    return True


def save_manual_cookie(raw):
    """校验并保存手动粘贴的 cookie。返回 (ok, message, extra)。"""
    cookie = bili_auth.normalize_cookie(raw)
    if not cookie:
        return False, "请粘贴 Cookie 字符串", {}
    if not bili_auth.cookie_to_dict(cookie).get("SESSDATA"):
        return False, "Cookie 里缺少 SESSDATA 字段，无法用于鉴权", {}
    ok, uname, mid, msg = bili_auth.fetch_nav(cookie)
    if not ok:
        return False, "Cookie 校验未通过：%s" % (msg or "无效或已过期"), {}
    cfg_set({"cookie": cookie})
    try:
        write_text(COOKIE_FILE, cookie)
    except OSError:
        pass
    _ACCOUNT.update(cookie=cookie, ok=True, uname=uname, mid=mid,
                    at=time.time(), message="")
    return True, ("已登录：" + uname if uname else "Cookie 保存成功"), \
        {"uname": uname, "mid": mid}


def save_manual_token(raw):
    """保存手动填写的 access_token（无法离线校验，仅做格式检查）。"""
    token = str(raw or "").strip()
    if not token:
        return False, "请填写 access_token", {}
    if len(token) < 16 or " " in token:
        return False, "access_token 格式看起来不正确（通常为很长的十六进制串）", {}
    cfg_set({"access_token": token})
    try:
        write_text(TV_TOKEN_FILE, token)
    except OSError:
        pass
    return True, "access_token 已保存", {}


def logout():
    """退出登录：清空配置与本地凭证文件。"""
    cfg_set({"cookie": "", "access_token": ""})
    remove_quiet(COOKIE_FILE)
    remove_quiet(TV_TOKEN_FILE)
    _ACCOUNT.update(cookie=None, ok=False, uname="", mid=0, at=0.0,
                    message="未登录")
    return True


# --------------------------------------------------------------------------- #
# 地址提取：从「分享文案」里取出可用的 B 站地址
# --------------------------------------------------------------------------- #
# 用户从 App / 网页点「分享 → 复制链接」拿到的往往是一整段文案，例如：
#   【Hsin let's Groove!】 https://www.bilibili.com/video/BV12eHa6RE59/?share_source=...
# 而 BBDown 只认干净地址，整段喂进去只会停在「获取aid... 输入有误」。
# 这里负责把任意文本拆成地址列表（去重、保持出现顺序）。
#
# 刻意保留链接原样而不重建成 https://www.bilibili.com/video/<BV>：
# 实测 BBDown 会读取链接里的 p 参数自动选集（?p=2 → 已选择：2），
# 重写会把这个信息丢掉。
#
# 前端不再自己写一套正则，全部走 /api/extract（本函数），
# 保证「界面提示的地址」与「实际交给 BBDown 的地址」永远一致。

# 链接里不该出现的字符：空白、通用标点（…—""）、中日韩文字与全角标点、引号括号
_NON_URL_CHARS = (r"\s\u2000-\u206f\u3000-\u303f\u4e00-\u9fff\uff00-\uffef"
                  r"<>\"'()\[\]{}|\\^`")

_TARGET_RE = re.compile(
    # 链接：允许省略 http(s)://，域名限定在 bilibili 系（b23.tv 短链交给 BBDown 跳转）
    r"(?P<url>(?:https?://)?(?:[A-Za-z0-9_-]{1,63}\.){0,8}"
    r"(?:bilibili\.com|b23\.tv|bili2233\.cn|bilibili\.tv)"
    r"(?::\d+)?/[^" + _NON_URL_CHARS + r"]*)"
    # 裸号
    r"|(?P<bv>BV[0-9A-Za-z]{10})"
    r"|(?P<ep>(?<![0-9A-Za-z])ep\d{1,20}(?![0-9]))"
    r"|(?P<ss>(?<![0-9A-Za-z])ss\d{1,20}(?![0-9]))"
    r"|(?P<av>(?<![0-9A-Za-z])av\d{1,20}(?![0-9]))",
    re.IGNORECASE)

# 去重键：同一视频的 BV / av / 完整链接 / 短链应视为同一个
_KEY_BV_RE = re.compile(r"BV[0-9A-Za-z]{10}", re.IGNORECASE)
_KEY_ID_RE = re.compile(r"(?<![0-9A-Za-z])(?:av|ep|ss)\d{1,20}(?![0-9])", re.IGNORECASE)

# 链接结尾常粘着句读或右括号，剥掉
_TRIM_CHARS = ".,;:!?'\"`*，。；：！？、·…“”‘’《》〈〉「」『』【】〔〕（）()[]{}<>"

NO_TARGET_MSG = ("没识别到 B 站地址。可以直接粘贴分享文案（带标题、多余文字都行），"
                 "也可以填 BV号 / av号 / ep / ss / 视频链接，多个用换行分隔。")


def _target_key(t):
    """同一视频的不同写法归一到同一个键，用于去重。"""
    m = _KEY_BV_RE.search(t)
    if m:
        return m.group(0).upper()
    m = _KEY_ID_RE.search(t)
    if m:
        return m.group(0).lower()
    return t.rstrip("/").lower()


def extract_targets(text):
    """从任意文本中提取 B 站地址，去重并保持出现顺序。没找到时返回 []。"""
    out, seen = [], set()
    for m in _TARGET_RE.finditer(text or ""):
        kind = m.lastgroup
        t = (m.group(kind) or "").strip().strip(_TRIM_CHARS).strip()
        if not t:
            continue
        # 只有链接才补协议；裸号（BV/av/ep/ss）必须原样交给 BBDown
        if kind == "url" and not re.match(r"(?i)^https?://", t):
            t = "https://" + t.lstrip("/")
        key = _target_key(t)
        if key in seen:
            continue
        seen.add(key)
        out.append(t)
    return out


def address_text(body):
    """把请求体里各种形态的地址字段拼成一段文本（兼容旧前端的 url / urls）。"""
    parts = []
    for k in ("text", "url", "urls"):
        v = body.get(k)
        if isinstance(v, str):
            parts.append(v)
        elif isinstance(v, (list, tuple)):
            parts.extend(str(x) for x in v if x is not None)
    return "\n".join(parts)


def read_clipboard():
    """读取系统剪贴板文本（best-effort，失败返回空串）。"""
    if os.name == "nt":
        # 显式把控制台输出编码切到 UTF-8，否则中文标题会被转成 GBK 而乱码
        ps = ("[Console]::OutputEncoding = [System.Text.Encoding]::UTF8;"
              "Get-Clipboard -Raw")
        cmds = [["powershell", "-NoProfile", "-ExecutionPolicy", "Bypass",
                 "-Command", ps]]
    elif sys.platform == "darwin":
        cmds = [["pbpaste"]]
    else:
        cmds = [["wl-paste", "-n"], ["xclip", "-selection", "clipboard", "-o"],
                ["xsel", "-b", "-o"]]
    for cmd in cmds:
        try:
            r = subprocess.run(cmd, capture_output=True, timeout=15,
                               creationflags=CREATE_NO_WINDOW)
        except Exception:                               # noqa: BLE001
            continue
        if r.returncode == 0:
            return decode(r.stdout or b"")
    return ""


# --------------------------------------------------------------------------- #
# 命令行参数拼装
# --------------------------------------------------------------------------- #
def build_args(url, mode="download", page=None, overrides=None):
    """根据配置拼装 BBDown 参数列表。mode: info | download"""
    c = dict(CONFIG)
    if overrides:
        c.update(overrides)
    a = [url]

    api = c.get("api", "web")
    if api == "tv":
        a.append("--use-tv-api")
    elif api == "app":
        a.append("--use-app-api")
    elif api == "intl":
        a.append("--use-intl-api")

    if c.get("use_aria2c"):
        a.append("--use-aria2c")
        if c.get("aria2c_path"):
            a += ["--aria2c-path", c["aria2c_path"]]

    if c.get("dfn_priority"):
        a += ["-q", c["dfn_priority"]]
    if c.get("encoding_priority"):
        a += ["-e", c["encoding_priority"]]
    if c.get("language"):
        a += ["--language", c["language"]]

    # 下载内容
    if c.get("video_only"):
        a.append("--video-only")
    if c.get("audio_only"):
        a.append("--audio-only")
    if c.get("danmaku_only"):
        a.append("--danmaku-only")
    if c.get("sub_only"):
        a.append("--sub-only")
    if c.get("cover_only"):
        a.append("--cover-only")

    if mode == "info":
        a.append("--only-show-info")
        a.append("--show-all")
    else:
        # 下载链路兼容性：部分网络下强制替换 host/协议会被 CDN 拒绝（403）
        if c.get("compat_mode"):
            a += ["--force-http", "false", "--force-replace-host", "false"]
        else:
            if not c.get("force_http", True):
                a += ["--force-http", "false"]
            if not c.get("force_replace_host", True):
                a += ["--force-replace-host", "false"]
        if c.get("allow_pcdn"):
            a.append("--allow-pcdn")
        if c.get("skip_mux") and not (c.get("video_only") or c.get("audio_only")):
            a.append("--skip-mux")
        if c.get("download_danmaku"):
            a.append("--download-danmaku")
        if c.get("skip_subtitle"):
            a.append("--skip-subtitle")
        if c.get("skip_cover"):
            a.append("--skip-cover")
        if c.get("interactive"):
            a.append("--interactive")
        if c.get("save_archives_to_file"):
            a.append("--save-archives-to-file")
        if c.get("file_pattern"):
            a += ["-F", c["file_pattern"]]
        if c.get("delay_per_page"):
            a += ["--delay-per-page", str(c["delay_per_page"])]

    sel = page if page is not None else (c.get("select_page") or "")
    if sel:
        a += ["-p", str(sel)]

    ff = find_ffmpeg()
    if ff:
        a += ["--ffmpeg-path", ff]

    if c.get("cookie"):
        a += ["-c", c["cookie"]]
    if c.get("access_token"):
        a += ["--token", c["access_token"]]

    if c.get("extra_args"):
        a += [s for s in str(c["extra_args"]).split() if s]

    if mode != "info" and c.get("download_dir"):
        a += ["--work-dir", c["download_dir"]]

    return a


# --------------------------------------------------------------------------- #
# ffmpeg 自动下载
# --------------------------------------------------------------------------- #
FFMPEG_STATE = {"running": False, "progress": 0, "total": 0,
                "received": 0, "message": "", "ok": False, "path": ""}


def _ffmpeg_worker(url):
    st = FFMPEG_STATE
    try:
        st.update(running=True, progress=0, received=0, total=0,
                  message="正在连接下载源…", ok=False, path="")
        req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
        data = io.BytesIO()
        with urllib.request.urlopen(req, timeout=60) as resp:
            total = int(resp.headers.get("Content-Length") or 0)
            st["total"] = total
            st["message"] = "正在下载 ffmpeg…"
            while True:
                chunk = resp.read(262144)
                if not chunk:
                    break
                data.write(chunk)
                st["received"] += len(chunk)
                if total:
                    st["progress"] = round(st["received"] * 100.0 / total, 2)
        st["message"] = "正在解压…"
        dest = os.path.join(TOOLS_DIR, "ffmpeg", "bin")
        os.makedirs(dest, exist_ok=True)
        target = os.path.join(dest, "ffmpeg.exe")
        with zipfile.ZipFile(data) as zf:
            member = next((n for n in zf.namelist()
                           if n.lower().endswith("/bin/ffmpeg.exe")
                           or n.lower() == "ffmpeg.exe"), None)
            if not member:
                raise RuntimeError("压缩包内未找到 ffmpeg.exe")
            with zf.open(member) as src, open(target, "wb") as out:
                shutil.copyfileobj(src, out)
            # 顺带取出 ffprobe.exe
            probe = next((n for n in zf.namelist()
                          if n.lower().endswith("/bin/ffprobe.exe")), None)
            if probe:
                with zf.open(probe) as src, open(os.path.join(dest, "ffprobe.exe"), "wb") as out:
                    shutil.copyfileobj(src, out)
        cfg_set({"ffmpeg_path": target})
        st.update(running=False, progress=100, ok=True, path=target,
                  message="ffmpeg 安装完成")
    except Exception as exc:                       # noqa: BLE001
        st.update(running=False, ok=False, message="下载失败：%s" % exc)


def start_ffmpeg_download(source_index=0):
    if FFMPEG_STATE["running"]:
        return False, "已有下载任务在进行中"
    idx = max(0, min(source_index, len(FFMPEG_SOURCES) - 1))
    url = FFMPEG_SOURCES[idx][1]
    threading.Thread(target=_ffmpeg_worker, args=(url,), daemon=True).start()
    return True, FFMPEG_SOURCES[idx][0]


# --------------------------------------------------------------------------- #
# 解析 BBDown -info 的文本输出
# --------------------------------------------------------------------------- #
LOG_PREFIX_RE = re.compile(r"^\[\d{4}-\d{2}-\d{2}[^\]]*\]\s*-?\s*")
PAGE_RE = re.compile(r"^P(\d+):\s*\[(\d+)\]\s*\[(.+?)\]\s*(?:\[([^\]]*)\])?\s*$")
STREAM_RE = re.compile(r"^\[\d+[PKk]?[^\]]*\]\s*\[(\d+x\d+)\]\s*\[([^\]]+)\]")


def _strip_log(s):
    return LOG_PREFIX_RE.sub("", s).strip()


def parse_info_text(text):
    info = {"title": "", "owner": "", "publishTime": "", "pages": [],
            "streams": [], "streamCount": 0, "raw": text}
    for raw in text.splitlines():
        s = _strip_log(raw.strip())
        if not s:
            continue
        if s.startswith("视频标题:"):
            info["title"] = s.split(":", 1)[1].strip()
        elif s.startswith("发布时间:"):
            info["publishTime"] = s.split(":", 1)[1].strip()
        elif s.startswith("UP主页:"):
            info["owner"] = s.split(":", 1)[1].strip()
        else:
            m = re.match(r"共计\s*(\d+)\s*条视频流", s)
            if m:
                info["streamCount"] = int(m.group(1))
            m = re.match(r"共计\s*(\d+)\s*个分P", s)
            if m:
                info["pageCount"] = int(m.group(1))
            m = PAGE_RE.match(s)
            if m:
                info["pages"].append({
                    "n": int(m.group(1)),
                    "cid": m.group(2),
                    "title": m.group(3).strip(),
                    "duration": (m.group(4) or "").strip(),
                })
                continue
            if STREAM_RE.match(s):
                info["streams"].append(s)
    info["streamCount"] = info["streamCount"] or len(info["streams"])
    return info


BILI_VIEW_API = "https://api.bilibili.com/x/web-interface/view"


def enrich_info(info, url):
    """用 B 站官方 view 接口补充 BBDown -info 未提供的字段（UP 主昵称 / 封面 / 时长）。
    任何异常都静默忽略，绝不影响主流程。"""
    m = re.search(r"(BV[0-9A-Za-z]{10})", url or "")
    if m:
        q = "bvid=" + m.group(1)
    else:
        m = re.search(r"av(\d+)", url or "", re.I)
        if not m:
            return info
        q = "aid=" + m.group(1)
    try:
        req = urllib.request.Request(
            BILI_VIEW_API + "?" + q,
            headers={
                "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                              "AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120 Safari/537.36",
                "Referer": "https://www.bilibili.com/",
            })
        with urllib.request.urlopen(req, timeout=12) as resp:
            data = (json.loads(resp.read().decode("utf-8")) or {}).get("data") or {}
    except Exception:                                   # noqa: BLE001
        return info

    owner = data.get("owner") or {}
    if owner.get("name"):
        info["ownerName"] = owner["name"]
    if data.get("pic"):
        info["cover"] = data["pic"]
    if data.get("duration"):
        info["duration"] = data["duration"]
    if not info.get("title") and data.get("title"):
        info["title"] = data["title"]
    if not info.get("publishTime") and data.get("pubdate"):
        info["publishTime"] = time.strftime("%Y-%m-%d %H:%M:%S",
                                            time.localtime(data["pubdate"]))
    # 用官方接口补齐全 P 标题
    pages = data.get("pages") or []
    if pages:
        by_n = {p.get("n"): p for p in (info.get("pages") or [])}
        for p in pages:
            item = by_n.get(p.get("page"))
            if item is not None and not item.get("title"):
                item["title"] = p.get("part") or ""
        info["pageCount"] = info.get("pageCount") or len(pages)
    return info


# --------------------------------------------------------------------------- #
# 桌面窗口（内嵌 WebView2，独立界面，不跳转浏览器）
# --------------------------------------------------------------------------- #
WINDOW_TITLE = "BBDown 图形界面"
CONSOLE_TITLE = "BBDown 图形界面 - 服务端"      # 与窗口标题区分，避免误聚焦控制台
HWND_FILE = os.path.join(DATA_DIR, "_window.hwnd")
WINDOW_STATE = {"mode": "headless", "desktop": False, "open": False}

_HICONS = []                    # 保持窗口图标句柄存活，防止被 GC 释放
_SERVER = {"httpd": None}


def window_mode_enabled():
    """当前进程是否以「独立窗口」方式运行。"""
    return bool(WINDOW_STATE.get("desktop"))


def backend_available():
    """检测内嵌窗口后端（pywebview + WebView2）是否可用。"""
    try:
        import webview  # noqa: F401
    except Exception:                               # noqa: BLE001
        return False, "未安装内嵌窗口组件（pywebview）"
    if os.name == "nt":
        try:
            import webview.platforms.winforms  # noqa: F401
        except Exception as exc:                    # noqa: BLE001
            return False, "Windows 窗口后端不可用：%s" % exc
    return True, ""


def _window_class(hwnd):
    buf = ctypes.create_unicode_buffer(256)
    ctypes.windll.user32.GetClassNameW(hwnd, buf, 256)
    return buf.value


def _window_text(hwnd):
    n = ctypes.windll.user32.GetWindowTextLengthW(hwnd)
    if n <= 0:
        return ""
    buf = ctypes.create_unicode_buffer(n + 2)
    ctypes.windll.user32.GetWindowTextW(hwnd, buf, n + 2)
    return buf.value


def _enum_windows_for_pid(pid=None):
    """枚举属于指定进程（默认本进程）的所有顶层窗口句柄。"""
    if os.name != "nt":
        return []
    u32 = ctypes.windll.user32
    if pid is None:
        pid = os.getpid()
    found = []
    proc = ctypes.WINFUNCTYPE(ctypes.c_bool, ctypes.c_void_p, ctypes.c_void_p)

    def _cb(hwnd, _lparam):
        wpid = ctypes.c_ulong(0)
        u32.GetWindowThreadProcessId(hwnd, ctypes.byref(wpid))
        if wpid.value == pid:
            found.append(hwnd)
        return True

    try:
        u32.EnumWindows(proc(_cb), 0)
    except Exception:                               # noqa: BLE001
        pass
    return found


def find_gui_window():
    """在本进程窗口里找出界面主窗口（可见、有标题、非控制台），找不到返回 0。"""
    if os.name != "nt":
        return 0
    u32 = ctypes.windll.user32
    fallback = 0
    for hwnd in _enum_windows_for_pid():
        if not u32.IsWindowVisible(hwnd):
            continue
        if _window_class(hwnd) == "ConsoleWindowClass":
            continue
        if not _window_text(hwnd).strip():
            continue
        if _window_text(hwnd).strip() == WINDOW_TITLE:
            return hwnd
        fallback = fallback or hwnd
    return fallback


def save_window_handle(hwnd):
    """把界面窗口句柄落盘，供下次双击时精准聚焦已有窗口。"""
    if os.name != "nt" or not hwnd:
        return
    try:
        with open(HWND_FILE, "w", encoding="utf-8") as f:
            json.dump({"hwnd": int(hwnd), "pid": os.getpid()}, f)
    except OSError:
        pass


def clear_window_handle():
    try:
        os.remove(HWND_FILE)
    except OSError:
        pass


def _load_window_handle():
    if os.name != "nt":
        return 0
    try:
        with open(HWND_FILE, "r", encoding="utf-8") as f:
            return int((json.load(f) or {}).get("hwnd") or 0)
    except Exception:                               # noqa: BLE001
        return 0


def focus_existing_window():
    """把已运行实例的界面窗口恢复并置顶（用于重复双击时不再开第二个窗口）。

    优先使用首次运行写下的窗口句柄：既快又准，且完全不依赖网络探测
    （本机全局代理会让「连接未监听端口」一直挂到超时，端口扫描极慢）。
    """
    if os.name != "nt":
        return False
    u32 = ctypes.windll.user32
    hwnd = _load_window_handle()
    if hwnd and (not u32.IsWindow(hwnd)
                 or _window_text(hwnd).strip() != WINDOW_TITLE):
        hwnd = 0
    if not hwnd:
        cand = u32.FindWindowW(None, WINDOW_TITLE)
        if cand and u32.IsWindowVisible(cand) and \
                _window_class(cand) != "ConsoleWindowClass":
            hwnd = cand
    if not hwnd:
        return False
    try:
        u32.ShowWindow(hwnd, 9)          # SW_RESTORE
        u32.SetForegroundWindow(hwnd)
        return True
    except Exception:                               # noqa: BLE001
        return False


def apply_window_icon(hwnd=None):
    """给窗口设置任务栏/标题栏图标（best-effort）。"""
    if os.name != "nt" or not os.path.isfile(ICON_PATH):
        return
    try:
        u32 = ctypes.windll.user32
        if not hwnd:
            hwnd = u32.FindWindowW(None, WINDOW_TITLE)
        if not hwnd:
            return
        IMAGE_ICON, LR_LOADFROMFILE, LR_DEFAULTSIZE = 1, 0x0010, 0x0040
        hicon = u32.LoadImageW(None, ICON_PATH, IMAGE_ICON,
                               u32.GetSystemMetrics(49), u32.GetSystemMetrics(50),
                               LR_LOADFROMFILE | LR_DEFAULTSIZE)
        if not hicon:
            return
        _HICONS.append(hicon)
        WM_SETICON = 0x0080
        u32.SendMessageW(hwnd, WM_SETICON, 0, hicon)   # ICON_SMALL
        u32.SendMessageW(hwnd, WM_SETICON, 1, hicon)   # ICON_BIG
    except Exception:                               # noqa: BLE001
        pass


def _watch_window_close(grace=3):
    """兜底看门狗：窗口一旦消失就强制退出。

    只依赖 pywebview 的 closed 事件不够稳——WebView2 / .NET 可能仍留着
    前台线程，导致 `webview.start()` 不返回、进程挂在后台（双击后"关不掉"）。
    这里独立探测窗口是否还活着，确认消失后直接结束进程。
    """
    if os.name != "nt":
        return

    def watch():
        # 先等窗口真正出现，避免误判
        for _ in range(120):
            if find_gui_window():
                break
            time.sleep(0.5)
        gone = 0
        while True:
            time.sleep(1.0)
            if find_gui_window():
                gone = 0
                continue
            gone += 1
            if gone >= grace:
                clear_window_handle()
                os._exit(0)

    threading.Thread(target=watch, daemon=True).start()


def _storage_dir():
    """WebView2 用户数据目录。

    优先放到 %LOCALAPPDATA%\\BBDownGUI（惯例位置），避免在程序目录里
    堆出 webview-data 缓存；拿不到环境变量时退回程序同级目录。
    """
    base = os.environ.get("LOCALAPPDATA")
    if base and os.path.isdir(base):
        return os.path.join(base, "BBDownGUI", "webview-data")
    return os.path.join(DATA_DIR, "webview-data")


def run_desktop_window(url):
    """在独立窗口中打开界面；返回后表示窗口已关闭。"""
    import webview

    WINDOW_STATE.update(desktop=True, mode="window")
    storage = _storage_dir()
    try:
        os.makedirs(storage, exist_ok=True)
    except OSError:
        storage = None

    # 窗口底色跟随主题，避免暗色主题下启动瞬间闪白
    bg = "#0f1524" if (CONFIG.get("theme") or "light") == "dark" else "#f4f6fb"
    win = webview.create_window(
        WINDOW_TITLE, url,
        width=1220, height=840, min_size=(980, 660),
        background_color=bg, text_select=True, zoomable=True,
    )

    def on_closed():
        WINDOW_STATE["open"] = False
        clear_window_handle()
        httpd = _SERVER.get("httpd")
        if httpd:
            try:
                httpd.shutdown()
            except Exception:                       # noqa: BLE001
                pass

    try:
        win.events.closed += on_closed
    except Exception:                               # noqa: BLE001
        pass

    def on_closing():
        """用户点关闭时不再等 WebView2 / .NET 收尾，短暂宽限后强制退出。"""
        def bye():
            time.sleep(1.5)
            clear_window_handle()
            os._exit(0)

        threading.Thread(target=bye, daemon=True).start()

    try:
        win.events.closing += on_closing
    except Exception:                               # noqa: BLE001
        pass

    def after_start(*_args, **_kwargs):
        """pywebview 会以 (window) 调用本回调，这里忽略参数。"""
        time.sleep(1.5)
        try:
            hwnd = find_gui_window()
            if hwnd:
                save_window_handle(hwnd)
                apply_window_icon(hwnd)
        except Exception:                           # noqa: BLE001
            pass

    WINDOW_STATE["open"] = True
    _watch_window_close()
    kwargs = {"private_mode": False}
    if storage:
        kwargs["storage_path"] = storage
    webview.start(after_start, win, **kwargs)


# --------------------------------------------------------------------------- #
# HTTP 处理
# --------------------------------------------------------------------------- #
MIME = {".html": "text/html; charset=utf-8",
        ".css": "text/css; charset=utf-8",
        ".js": "application/javascript; charset=utf-8",
        ".svg": "image/svg+xml", ".png": "image/png",
        ".ico": "image/x-icon", ".json": "application/json; charset=utf-8",
        ".woff2": "font/woff2"}


class Handler(BaseHTTPRequestHandler):
    server_version = "BBDownGUI/1.0"
    protocol_version = "HTTP/1.1"

    # ---- 基础工具 -------------------------------------------------------- #
    def log_message(self, fmt, *args):               # 静默访问日志
        pass

    def _send(self, code, body=b"", ctype="application/json; charset=utf-8",
              extra=None):
        if isinstance(body, str):
            body = body.encode("utf-8")
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        for k, v in (extra or {}).items():
            self.send_header(k, v)
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionAbortedError, ConnectionResetError):
            pass

    def _json(self, obj, code=200):
        self._send(code, json.dumps(obj, ensure_ascii=False))

    def _body(self):
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        try:
            return json.loads(self.rfile.read(n).decode("utf-8"))
        except Exception:                              # noqa: BLE001
            return {}

    def _file(self, path):
        if not os.path.isfile(path):
            self._send(404, "not found", "text/plain; charset=utf-8")
            return
        ext = os.path.splitext(path)[1].lower()
        with open(path, "rb") as f:
            self._send(200, f.read(), MIME.get(ext, "application/octet-stream"))

    # ---- GET ------------------------------------------------------------- #
    def do_GET(self):
        u = urlparse(self.path)
        path, qs = u.path, parse_qs(u.query)

        if path in ("/", "/index.html"):
            return self._file(os.path.join(WEB_DIR, "index.html"))
        if path.startswith("/static/"):
            rel = path[len("/static/"):].replace("/", os.sep)
            safe = os.path.normpath(os.path.join(WEB_DIR, rel))
            if safe.startswith(os.path.normpath(WEB_DIR)):
                return self._file(safe)
            return self._send(403, "forbidden", "text/plain")

        if path == "/api/status":
            return self._json(self.api_status())
        if path == "/api/task":
            since = int((qs.get("since") or ["0"])[0])
            with _lock:
                task = CURRENT["task"]
            if not task:
                return self._json({"exists": False, "queueLeft": QUEUE["left"]})
            snap = task.snapshot(since)
            snap["exists"] = True
            snap["queueLeft"] = QUEUE["left"]
            return self._json(snap)
        if path == "/api/ffmpeg/progress":
            return self._json(FFMPEG_STATE)
        if path == "/api/clipboard":
            text = read_clipboard()
            return self._json({"ok": bool(text), "text": text,
                               "message": "" if text else "剪贴板为空或读取失败"})

        # ---- 扫码登录 ---------------------------------------------------- #
        if path == "/api/login/status":
            mode = (qs.get("mode") or ["web"])[0]
            sess = login_session(mode)
            snap = sess.snapshot() if sess else {
                "state": "idle", "message": "尚未开始", "seq": 0,
                "hasImage": False, "mode": "tv" if mode == "tv" else "web",
                "active": False, "remain": 0, "result": {}, "url": "",
                "elapsed": 0,
            }
            snap["ok"] = True
            snap["login"] = login_status(
                with_account=snap.get("state") == "confirmed")
            return self._json(snap)
        if path == "/api/login/qrcode.png":
            mode = (qs.get("mode") or ["web"])[0]
            sess = login_session(mode)
            png = sess.image() if sess else b""
            if not png:
                return self._send(404, "no qrcode",
                                  "text/plain; charset=utf-8")
            return self._send(200, png, "image/png")

        # 兼容旧前端：BBDown 自己写出的 qrcode.png（字符画退化时基本不可用）
        if path == "/qrcode":
            p = os.path.join(WORKSPACE, "qrcode.png")
            if not os.path.isfile(p):
                p = os.path.join(APP_DIR, "qrcode.png")
            return self._file(p)
        return self._send(404, "not found", "text/plain; charset=utf-8")

    # ---- POST ------------------------------------------------------------ #
    def do_POST(self):
        path = urlparse(self.path).path
        body = self._body()

        if path == "/api/config":
            return self._json({"ok": True, "config": cfg_set(body)})
        if path == "/api/ffmpeg/detect":
            found = find_ffmpeg()
            if body.get("path"):
                p = os.path.abspath(os.path.expandvars(body["path"]))
                cfg_set({"ffmpeg_path": p if _usable(p) else ""})
                found = find_ffmpeg()
            return self._json({"ok": bool(found), "path": found,
                               "config": CONFIG})
        if path == "/api/ffmpeg/download":
            ok, msg = start_ffmpeg_download(int(body.get("source") or 0))
            return self._json({"ok": ok, "message": msg})
        if path == "/api/extract":
            text = address_text(body)
            targets = extract_targets(text)
            return self._json({"ok": True, "targets": targets,
                               "targetCount": len(targets),
                               "cleaned": bool(targets) and
                               text.strip() != "\n".join(targets)})
        if path == "/api/parse":
            return self._json(self.do_parse(body))
        if path == "/api/download":
            return self._json(self.do_download(body))
        if path == "/api/login":
            return self._json(self.do_login(body))
        # ---- 登录：新接口（自实现扫码，带真实状态） ---------------------- #
        if path == "/api/login/start":
            sess, err = start_login(body.get("mode") or "web")
            if err:
                return self._json({"ok": False, "message": err})
            return self._json({"ok": True, "mode": sess.mode})
        if path == "/api/login/cancel":
            cancel_login(body.get("mode"))
            return self._json({"ok": True})
        if path == "/api/login/cookie":
            ok, msg, extra = save_manual_cookie(body.get("cookie") or "")
            res = {"ok": ok, "message": msg, "login": login_status(True)}
            res.update(extra)
            return self._json(res)
        if path == "/api/login/token":
            ok, msg, extra = save_manual_token(body.get("token") or "")
            res = {"ok": ok, "message": msg}
            res.update(extra)
            return self._json(res)
        if path == "/api/login/logout":
            cancel_login()
            logout()
            return self._json({"ok": True, "login": login_status()})
        if path == "/api/account":
            return self._json({"ok": True,
                               "account": account_info(force=bool(body.get("force")))})
        if path == "/api/stop":
            return self._json(self.do_stop())
        if path == "/api/open":
            target = body.get("path") or CONFIG.get("download_dir") or WORKSPACE
            try:
                if not os.path.isdir(target):
                    os.makedirs(target, exist_ok=True)
                os.startfile(target)                    # noqa: S606
                return self._json({"ok": True})
            except Exception as exc:                    # noqa: BLE001
                return self._json({"ok": False, "message": str(exc)})
        if path == "/api/pick-folder":
            return self._json({"ok": True, "path": pick_folder()})
        if path == "/api/open-browser":
            webbrowser.open("http://%s:%d/" % self.server.server_address[:2])
            return self._json({"ok": True})
        if path == "/api/quit":
            self._json({"ok": True})
            threading.Thread(target=lambda: (time.sleep(0.3),
                                             os._exit(0)), daemon=True).start()
            return
        return self._send(404, "not found", "text/plain; charset=utf-8")

    # ---- 业务处理 -------------------------------------------------------- #
    def api_status(self):
        exe = CONFIG.get("bbdown_exe") or DEFAULT_BBDOWN
        ff = find_ffmpeg()
        return {
            "ok": True,
            "version": bbdown_version(exe),
            "bbdownExe": exe,
            "bbdownExeExists": os.path.isfile(exe),
            "ffmpeg": {"found": bool(ff), "path": ff},
            "login": login_status(with_account=True),
            "downloadDir": CONFIG.get("download_dir"),
            "theme": CONFIG.get("theme") or "light",
            "desktop": window_mode_enabled(),
            "config": CONFIG,
        }

    def do_parse(self, body):
        """同步解析：阻塞直到 BBDown -info 结束，返回结构化信息 + 原始日志。

        输入可以是整段分享文案；只解析识别到的第一个地址，其余的通过
        targetCount 告诉前端（解析多集要跑多次 BBDown，代价太高）。
        """
        targets = extract_targets(address_text(body))
        if not targets:
            return {"ok": False, "targets": [], "targetCount": 0, "message":
                    NO_TARGET_MSG}
        url = targets[0]
        if not find_ffmpeg():
            return {"ok": False, "message":
                    "BBDown 依赖 ffmpeg，请先在「环境设置」中指定或自动下载 ffmpeg。"}
        c = dict(CONFIG)
        c.update({k: v for k, v in body.items() if k in DEFAULT_CONFIG})
        with _lock:
            CONFIG.update(c)
            save_config(CONFIG)
        args = build_args(url, "info")
        task, err = start_task("解析 %s" % url, "info", args)
        if err:
            return {"ok": False, "message": err}
        deadline = time.time() + 180
        while task.running and time.time() < deadline:
            time.sleep(0.15)
        snap = task.snapshot(0)
        text = "\n".join(l["m"] for l in snap["logs"])
        info = parse_info_text(text)
        info = enrich_info(info, url)
        info["ok"] = task.exit_code == 0
        info["exitCode"] = task.exit_code
        info["logs"] = snap["logs"]
        info["url"] = url
        info["targets"] = targets
        info["targetCount"] = len(targets)
        return info

    def do_download(self, body):
        targets = extract_targets(address_text(body))
        if not targets:
            return {"ok": False, "targets": [], "targetCount": 0, "message":
                    NO_TARGET_MSG}
        if not find_ffmpeg():
            return {"ok": False, "message":
                    "BBDown 依赖 ffmpeg，请先在「环境设置」中指定或自动下载 ffmpeg。"}
        c = dict(CONFIG)
        c.update({k: v for k, v in body.items() if k in DEFAULT_CONFIG})
        with _lock:
            CONFIG.update(c)
            save_config(CONFIG)
        # 多个地址则逐个下载，串行执行
        target = targets[0]
        args = build_args(target, "download", page=body.get("selectPage"))
        task, err = start_task("下载 %s" % target, "download", args,
                               retry_fn=_compat_retry)
        if err:
            return {"ok": False, "message": err}
        task.request = body
        if len(targets) > 1:
            _queue_rest(task, targets[1:])
        return {"ok": True, "first": target, "targets": targets,
                "targetCount": len(targets)}

    def do_login(self, body):
        """兼容旧接口：`login` → Web 扫码，`logintv` → 电视端扫码。

        不再调用 BBDown 的 login / logintv 命令：它只在控制台用方块字符「画」
        二维码，本环境下会退化成整片实心方块（既看不出图形、也没有状态反馈）。
        现在改为直接对接官方接口，返回可扫描的图片与真实扫码状态。
        """
        mode = body.get("mode") or "login"
        sess, err = start_login("tv" if mode == "logintv" else "web")
        if err:
            return {"ok": False, "message": err}
        return {"ok": True, "mode": sess.mode}

    def do_stop(self):
        with _lock:
            task = CURRENT["task"]
        if not task or not task.running or not task.proc:
            return {"ok": False, "message": "没有正在运行的任务"}
        try:
            if os.name == "nt":
                subprocess.run(["taskkill", "/F", "/T", "/PID", str(task.proc.pid)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                               creationflags=CREATE_NO_WINDOW)
            else:
                task.proc.terminate()
            task.push("[已请求停止]")
            return {"ok": True}
        except Exception as exc:                        # noqa: BLE001
            return {"ok": False, "message": str(exc)}


def _queue_rest(task, rest):
    """多地址排队：当前任务结束后依次下载剩余地址。

    每个地址是一个新任务（各自带独立的进度跟踪），QUEUE["left"] 让前端知道
    「还没下完」，从而继续轮询而不是提前收工。
    """
    QUEUE["left"] = len(rest)

    def waiter():
        while task.running:
            time.sleep(0.5)
        for i, u in enumerate(rest):
            args = build_args(u, "download")
            t2, err = start_task("下载 %s" % u, "download", args)
            if err or not t2:
                QUEUE["left"] = 0
                return
            QUEUE["left"] = len(rest) - i - 1
            while t2.running:
                time.sleep(0.5)
        QUEUE["left"] = 0

    threading.Thread(target=waiter, daemon=True).start()


_VERSION_CACHE = {}


def bbdown_version(exe):
    """读取 BBDown 版本。

    /api/status 会被前端频繁轮询，若每次都去 spawn 一次 BBDown.exe 既慢又容易偶发失败，
    因此加缓存；只缓存成功结果，失败时下次会重试。
    """
    if not os.path.isfile(exe):
        return ""
    if exe in _VERSION_CACHE:
        return _VERSION_CACHE[exe]
    try:
        r = subprocess.run([exe, "--help"], capture_output=True, timeout=20,
                           creationflags=CREATE_NO_WINDOW)
        txt = decode(r.stdout or b"") + decode(r.stderr or b"")
        m = re.search(r"BBDown version ([\d.]+)", txt)
        if m:
            _VERSION_CACHE[exe] = m.group(1)
            return m.group(1)
        return ""
    except Exception:                                   # noqa: BLE001
        return ""


def pick_folder():
    """调用系统文件夹选择对话框（best-effort）。"""
    if os.name != "nt":
        return ""
    ps = ("Add-Type -AssemblyName System.Windows.Forms;"
          "$d = New-Object System.Windows.Forms.FolderBrowserDialog;"
          "$d.Description = '选择保存目录';"
          "$d.ShowNewFolderButton = $true;"
          "if ($d.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK)"
          "{ [Console]::Out.Write($d.SelectedPath) }")
    try:
        r = subprocess.run(["powershell", "-STA", "-NoProfile",
                            "-ExecutionPolicy", "Bypass", "-Command", ps],
                           capture_output=True, timeout=180)
        return decode(r.stdout or b"").strip()
    except Exception:                                   # noqa: BLE001
        return ""


def free_port(preferred):
    for p in [preferred] + list(range(preferred + 1, preferred + 30)):
        with socket.socket() as s:
            try:
                s.bind(("127.0.0.1", p))
                return p
            except OSError:
                continue
    return preferred


def _tcp_open(host, port, timeout=0.25):
    """快速判断端口是否有进程在监听。"""
    s = socket.socket()
    s.settimeout(timeout)
    try:
        return s.connect_ex((host, port)) == 0
    except OSError:
        return False
    finally:
        try:
            s.close()
        except OSError:
            pass


def _probe_existing(host, port):
    """探测该端口上是否已有本程序在运行（避免重复双击开出多个实例）。

    先做 TCP 预检：本机全局代理会让「连接未监听端口」挂到超时，
    若直接 HTTP 探测，一轮端口扫描要几十秒。预检用极短超时即可。
    """
    if not _tcp_open(host, port, 0.25):
        return False
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))  # 绕过系统代理
    try:
        with opener.open("http://%s:%d/api/status" % (host, port), timeout=0.8) as r:
            data = json.loads(r.read().decode("utf-8"))
        return isinstance(data, dict) and "version" in data
    except Exception:                                   # noqa: BLE001
        return False


def _out(msg=""):
    """无控制台窗口（打包 --windowed）时 stdout 可能为 None，统一兜底。"""
    try:
        print(msg)
    except Exception:                               # noqa: BLE001
        pass
    try:
        if sys.stdout is not None:
            sys.stdout.flush()
    except Exception:                               # noqa: BLE001
        pass


def _alert(msg, title=None):
    """无控制台窗口（打包 --windowed）时用系统弹窗提示，避免「双击没反应」。"""
    if os.name != "nt":
        return
    try:
        ctypes.windll.user32.MessageBoxW(None, str(msg),
                                         str(title or WINDOW_TITLE), 0x10)
    except Exception:                               # noqa: BLE001
        pass


def _probe_any(host, port, span=8):
    """在 [port, port+span) 范围内寻找已运行的本程序实例，返回端口或 None。"""
    for p in range(port, port + span):
        if _probe_existing(host, p):
            return p
    return None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8765)
    ap.add_argument("--window", action="store_true",
                    help="在独立窗口中打开界面（默认行为）")
    ap.add_argument("--browser", action="store_true",
                    help="改用系统浏览器打开界面")
    ap.add_argument("--no-browser", action="store_true",
                    help="只启动后台服务，不打开任何界面")
    ns = ap.parse_args()

    if os.name == "nt":
        try:
            os.system("title %s" % CONSOLE_TITLE)
        except Exception:                           # noqa: BLE001
            pass

    if not os.path.isdir(WEB_DIR):
        _out("缺少 web 目录：%s" % WEB_DIR)
        if getattr(sys, "frozen", False):
            _alert("程序文件不完整：缺少 web 目录。\n\n%s" % WEB_DIR)
        _pause_if_frozen()
        sys.exit(1)

    # ---- 已有实例：聚焦旧窗口 / 打开旧界面，绝不重复启动 ---- #
    # ① 最快路径：窗口句柄文件（瞬时，不碰网络）
    if not ns.browser and not ns.no_browser and focus_existing_window():
        _out("检测到界面已在运行，已切换到该窗口。")
        return

    # ② 回退：端口探测（兼容浏览器模式 / 句柄失效的情况）
    running_port = _probe_any(ns.host, ns.port)
    if running_port:
        url = "http://%s:%d/" % (ns.host, running_port)
        if ns.no_browser:
            _out("界面已在运行：%s" % url)
            return
        if not ns.browser and focus_existing_window():
            _out("检测到界面已在运行，已切换到该窗口。")
            return
        _out("检测到界面已在运行，正在打开：%s" % url)
        webbrowser.open(url)
        time.sleep(1.2)
        return

    # ---- 启动本地服务（界面与 API 均由此提供） ---- #
    port = free_port(ns.port)
    url = "http://%s:%d/" % (ns.host, port)
    httpd = ThreadingHTTPServer((ns.host, port), Handler)
    httpd.daemon_threads = True
    _SERVER["httpd"] = httpd
    threading.Thread(target=httpd.serve_forever, daemon=True).start()

    mode = "browser" if ns.browser else ("headless" if ns.no_browser else "window")
    if mode != "window":
        clear_window_handle()       # 非窗口模式不应残留旧句柄

    bb = CONFIG.get("bbdown_exe") or DEFAULT_BBDOWN
    _out("=" * 56)
    _out(" BBDown 图形界面   （%s 模式）" % {"window": "独立窗口",
                                             "browser": "浏览器",
                                             "headless": "后台服务"}[mode])
    _out(" 本地地址: %s" % url)
    _out(" BBDown  : %s%s" % (bb, "" if os.path.isfile(bb) else "   [未找到！]"))
    _out(" ffmpeg  : %s" % (find_ffmpeg() or "未找到（可在界面中自动下载）"))
    _out("=" * 56)

    # ---- 独立窗口（内嵌 WebView2，不跳转浏览器） ---- #
    if mode == "window":
        ok, why = backend_available()
        if not ok:
            _out("独立窗口不可用（%s），改用浏览器打开。" % why)
            mode = "browser"
        else:
            try:
                run_desktop_window(url)
                _out("窗口已关闭，程序退出。")
                # WebView2 / .NET 可能残留前台线程，强制干净退出，
                # 否则双击启动的 launcher 会一直挂在后台。
                try:
                    _SERVER.get("httpd") and _SERVER["httpd"].shutdown()
                except Exception:                   # noqa: BLE001
                    pass
                clear_window_handle()
                os._exit(0)
            except KeyboardInterrupt:
                return
            except Exception as exc:                # noqa: BLE001
                _out("独立窗口启动失败：%s" % exc)
                mode = "browser"

    if mode == "browser":
        _out(" 正在打开浏览器界面…")
        threading.Timer(0.6, lambda: webbrowser.open(url)).start()

    try:
        while True:
            time.sleep(3600)
    except KeyboardInterrupt:
        _out("\n已退出")


def _pause_if_frozen():
    """打包运行时出错不至于窗口一闪而过。"""
    if getattr(sys, "frozen", False):
        try:
            input("按回车键退出…")
        except Exception:                               # noqa: BLE001
            pass


if __name__ == "__main__":
    main()
