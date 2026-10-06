#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""哔哩哔哩扫码登录（Web 端 / 电视端），纯标准库实现。

为什么不用 `BBDown login`：
  BBDown 只会在控制台用方块字符「画」出二维码并往工作目录写一个 qrcode.png，
  既不返回登录状态，也无法在网页里可靠显示（字符画换行即失效）。
  这里直接对接官方登录接口，自己生成真正的二维码图片，并轮询扫码状态，
  登录成功后自动落盘 cookie / access_token，全程可观测。

接口：
  Web 端  GET  /x/passport-login/web/qrcode/generate  → url + qrcode_key
          GET  /x/passport-login/web/qrcode/poll      → 轮询（data.code 表示状态）
  电视端  POST /x/passport-tv-login/qrcode/auth_code   → url + auth_code（需 appkey 签名）
          POST /x/passport-tv-login/qrcode/poll       → 轮询（顶层 code 表示状态）
状态码：
  Web：86101 未扫码 / 86090 已扫码待确认 / 86038 已失效 / 0 成功
  TV ：86039 未扫码 / 86090 已扫码待确认 / 86038 已失效 / 0 成功
"""

import hashlib
import http.cookiejar
import json
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

import qrcodegen

WEB_GENERATE = ("https://passport.bilibili.com/x/passport-login/"
                "web/qrcode/generate")
WEB_POLL = "https://passport.bilibili.com/x/passport-login/web/qrcode/poll"

TV_GENERATE = ("https://passport.bilibili.com/x/passport-tv-login/"
               "qrcode/auth_code")
TV_POLL = "https://passport.bilibili.com/x/passport-tv-login/qrcode/poll"
TV_APPKEY = "4409e2ce8ffd12b8"
TV_APPSEC = "59b43e04ad6965f34319062b478f83dd"

NAV_API = "https://api.bilibili.com/x/web-interface/nav"

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")

# cookie 里真正决定鉴权的字段，按重要性排序（用于展示与校验）
KEY_COOKIES = ("SESSDATA", "DedeUserID", "bili_jct", "DedeUserID__ckMd5", "sid")

# 单张二维码有效期（官方为 180 秒）
QR_LIFETIME = 180.0
POLL_INTERVAL = 2.0
MAX_REFRESH = 3          # 二维码失效后最多自动重申请次数


def _headers(extra=None):
    h = {"User-Agent": UA,
         "Referer": "https://www.bilibili.com/",
         "Origin": "https://www.bilibili.com",
         "Accept": "application/json, text/plain, */*"}
    if extra:
        h.update(extra)
    return h


def make_opener(jar=None):
    """构造带 cookie 容器的 opener（登录链路需要自动收 set-cookie）。"""
    jar = jar if jar is not None else http.cookiejar.CookieJar()
    return urllib.request.build_opener(
        urllib.request.HTTPCookieProcessor(jar)), jar


def _get_json(opener, url, timeout=15, headers=None):
    req = urllib.request.Request(url, headers=headers or _headers())
    with opener.open(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8", "replace")
    try:
        return json.loads(raw)
    except ValueError:
        raise RuntimeError("接口返回了非 JSON 内容：%s" % raw[:120])


def _post_form(opener, url, params, timeout=15):
    body = urllib.parse.urlencode(params).encode("utf-8")
    req = urllib.request.Request(
        url, data=body,
        headers=_headers({"Content-Type":
                          "application/x-www-form-urlencoded; charset=utf-8"}))
    with opener.open(req, timeout=timeout) as r:
        raw = r.read().decode("utf-8", "replace")
    try:
        return json.loads(raw)
    except ValueError:
        raise RuntimeError("接口返回了非 JSON 内容：%s" % raw[:120])


# --------------------------------------------------------------------------- #
# cookie 工具
# --------------------------------------------------------------------------- #
def jar_to_cookie(jar):
    """把 CookieJar 转成 cookie 字符串（同名去重，保留最后写入的值）。"""
    seen = {}
    for c in jar:
        if not c.value:
            continue
        dom = (c.domain or "").lstrip(".")
        if dom and "bilibili" not in dom and "hdslb" not in dom:
            continue
        seen[c.name] = c.value
    order = [k for k in KEY_COOKIES if k in seen]
    order += [k for k in seen if k not in order]
    return "; ".join("%s=%s" % (k, seen[k]) for k in order)


def cookie_to_dict(cookie):
    d = {}
    for part in str(cookie or "").split(";"):
        part = part.strip()
        if not part or "=" not in part:
            continue
        k, v = part.split("=", 1)
        k = k.strip()
        if k:
            d[k] = v.strip()
    return d


def normalize_cookie(text):
    """宽容地清洗用户粘贴的内容：允许带 Cookie: 前缀、换行、制表符。"""
    s = str(text or "").strip()
    if not s:
        return ""
    s = s.replace("\r", ";").replace("\n", ";").replace("\t", " ")
    for prefix in ("cookie:", "Cookie:", "COOKIE:"):
        if s.startswith(prefix):
            s = s[len(prefix):]
    parts = []
    for part in s.split(";"):
        part = part.strip()
        if part and "=" in part:
            parts.append(part)
    # 同名保留最后一个
    order, seen = [], {}
    for p in parts:
        k = p.split("=", 1)[0].strip()
        if k not in seen:
            order.append(k)
        seen[k] = p
    return "; ".join(seen[k] for k in order)


def fetch_nav(cookie, timeout=12):
    """用 cookie 查询账号信息。返回 (isLogin, uname, mid, message)。"""
    jar = http.cookiejar.CookieJar()
    for k, v in cookie_to_dict(cookie).items():
        jar.set_cookie(http.cookiejar.Cookie(
            version=0, name=k, value=v, port=None, port_specified=False,
            domain=".bilibili.com", domain_specified=True, domain_initial_dot=True,
            path="/", path_specified=True, secure=True, expires=None,
            discard=False, comment=None, comment_url=None, rest={}))
    opener = urllib.request.build_opener(
        urllib.request.HTTPCookieProcessor(jar))
    try:
        data = _get_json(opener, NAV_API, timeout=timeout,
                         headers=_headers({"Cookie": cookie}))
    except Exception as exc:                            # noqa: BLE001
        return False, "", 0, str(exc)
    if data.get("code") != 0:
        return False, "", 0, str(data.get("message") or "接口拒绝")
    d = data.get("data") or {}
    if not d.get("isLogin"):
        return False, "", 0, "cookie 无效或已过期"
    return True, str(d.get("uname") or ""), int(d.get("mid") or 0), ""


# --------------------------------------------------------------------------- #
# 登录会话
# --------------------------------------------------------------------------- #
class LoginSession(object):
    """一次扫码登录流程。状态机：

    idle → loading → pending → scanned → confirmed
                        ↓           ↓
                     expired     expired / error
    """

    def __init__(self, mode="web", on_success=None):
        self.mode = "tv" if mode == "tv" else "web"
        self._on_success = on_success
        self._lock = threading.RLock()
        self._thread = None
        self._stop = False
        self._state = "idle"
        self._message = "尚未开始"
        self._png = b""
        self._url = ""
        self._key = ""
        self._deadline = 0.0
        self._started = 0.0
        self._finished = 0.0
        self._result = {}
        self._seq = 0                       # 每次状态变化自增，前端据此判断是否变化

    # ---- 对外接口 -------------------------------------------------------- #
    def start(self):
        with self._lock:
            if self._thread and self._thread.is_alive():
                return False, "登录流程正在进行中"
            self._stop = False
            self._started = time.time()
            self._finished = 0.0
            self._result = {}
            self._png = b""
            self._set_locked("loading", "正在申请登录二维码…")
            self._thread = threading.Thread(target=self._run, daemon=True)
            self._thread.start()
            return True, ""

    def cancel(self):
        with self._lock:
            self._stop = True
            if self._state in ("loading", "pending", "scanned", "expired"):
                self._set_locked("idle", "已取消登录")
        return True

    def snapshot(self):
        with self._lock:
            return {
                "state": self._state,
                "message": self._message,
                "mode": self.mode,
                "seq": self._seq,
                "hasImage": bool(self._png),
                "remain": (max(0, int(self._deadline - time.time()))
                           if self._deadline else 0),
                "url": self._url,
                "elapsed": (round((self._finished or time.time()) - self._started, 1)
                            if self._started else 0),
                "result": dict(self._result),
                "active": bool(self._thread and self._thread.is_alive()),
            }

    def image(self):
        with self._lock:
            return self._png

    # ---- 内部 ------------------------------------------------------------ #
    def _set_locked(self, state, message, png=None, url=None, key=None,
                    result=None):
        self._state = state
        self._message = message
        self._seq += 1
        if png is not None:
            self._png = png
        if url is not None:
            self._url = url
        if key is not None:
            self._key = key
        if result is not None:
            self._result = result
        if state in ("confirmed", "error"):
            self._finished = time.time()
            self._deadline = 0.0

    def _set(self, state, message, **kw):
        with self._lock:
            self._set_locked(state, message, **kw)

    def _stopped(self):
        with self._lock:
            return self._stop

    def _run(self):
        try:
            if self.mode == "tv":
                self._run_tv()
            else:
                self._run_web()
        except Exception as exc:                        # noqa: BLE001
            if not self._stopped():
                self._set("error", "登录失败：%s" % exc)

    # ---- Web 端 ---------------------------------------------------------- #
    def _run_web(self):
        for attempt in range(MAX_REFRESH):
            if self._stopped():
                return
            self._set("loading", "正在申请登录二维码…")
            opener, jar = make_opener()
            data = _get_json(opener, WEB_GENERATE + "?source=main-fe-header")
            if data.get("code") != 0:
                raise RuntimeError(data.get("message") or "获取二维码失败")
            info = data.get("data") or {}
            url, key = info.get("url") or "", info.get("qrcode_key") or ""
            if not url or not key:
                raise RuntimeError("二维码接口未返回有效内容")
            self._show_qr(url, key, "请使用「哔哩哔哩」App 扫描二维码")

            if self._poll_web(opener, jar, key):
                return
            if self._stopped():
                return
            time.sleep(0.6)
        self._set("expired", "二维码多次失效，请点击「重新获取」再试")

    def _poll_web(self, opener, jar, key):
        """轮询 Web 端扫码状态。返回 True 表示已登录。"""
        with self._lock:
            self._deadline = time.time() + QR_LIFETIME
        while True:
            if self._stopped():
                return False
            with self._lock:
                if time.time() > self._deadline:
                    return False
            time.sleep(POLL_INTERVAL)
            if self._stopped():
                return False
            try:
                res = _get_json(opener, WEB_POLL + "?qrcode_key=" + key)
            except Exception:                           # noqa: BLE001
                continue                                # 网络抖动，继续轮询
            d = res.get("data") or {}
            code = d.get("code")
            if code == 0:
                cookie = jar_to_cookie(jar)
                if not cookie_to_dict(cookie).get("SESSDATA"):
                    self._set("error", "登录成功但未取到 SESSDATA，请重试")
                    return True
                self._finish_success(cookie, "", 0)
                return True
            if code == 86090:
                self._set("scanned", "已扫码，请在手机上点击「确认登录」")
            elif code == 86038:
                self._set("expired", "二维码已失效，正在重新获取…")
                return False
            elif code == 86101:
                if self._state != "pending":
                    self._set("pending", "等待扫码…")
            else:
                self._set("pending", str(d.get("message") or "等待扫码…"))

    # ---- 电视端 ---------------------------------------------------------- #
    def _tv_sign(self, params):
        p = dict(params)
        p["appkey"] = TV_APPKEY
        p["local_id"] = "0"
        p["ts"] = str(int(time.time()))
        q = urllib.parse.urlencode(sorted(p.items()))
        p["sign"] = hashlib.md5((q + TV_APPSEC).encode("utf-8")).hexdigest()
        return p

    def _run_tv(self):
        for _ in range(MAX_REFRESH):
            if self._stopped():
                return
            self._set("loading", "正在申请电视端登录二维码…")
            opener, _jar = make_opener()
            res = _post_form(opener, TV_GENERATE, self._tv_sign({}))
            if res.get("code") != 0:
                raise RuntimeError(res.get("message") or "获取二维码失败")
            info = res.get("data") or {}
            url, auth = info.get("url") or "", info.get("auth_code") or ""
            if not auth:
                raise RuntimeError("电视端二维码接口未返回 auth_code")
            self._show_qr(url, auth,
                          "用「哔哩哔哩」App 扫码，即可获得 180 天有效的 TV 凭证")

            if self._poll_tv(opener, auth):
                return
            if self._stopped():
                return
            time.sleep(0.6)
        self._set("expired", "二维码多次失效，请点击「重新获取」再试")

    def _poll_tv(self, opener, auth):
        with self._lock:
            self._deadline = time.time() + QR_LIFETIME
        while True:
            if self._stopped():
                return False
            with self._lock:
                if time.time() > self._deadline:
                    return False
            time.sleep(POLL_INTERVAL)
            if self._stopped():
                return False
            try:
                res = _post_form(opener, TV_POLL,
                                 self._tv_sign({"auth_code": auth}))
            except Exception:                           # noqa: BLE001
                continue
            code = res.get("code")
            if code == 0:
                d = res.get("data") or {}
                token = str(d.get("access_token") or "")
                cookies = (d.get("cookie_info") or {}).get("cookies") or []
                cookie = "; ".join(
                    "%s=%s" % (c.get("name"), c.get("value"))
                    for c in cookies if c.get("name"))
                self._finish_success(cookie, token, int(d.get("mid") or 0))
                return True
            if code == 86090:
                self._set("scanned", "已扫码，请在手机上点击「确认登录」")
            elif code == 86038:
                self._set("expired", "二维码已失效，正在重新获取…")
                return False
            elif code == 86039:
                if self._state != "pending":
                    self._set("pending", "等待扫码…")
            else:
                self._set("pending", str(res.get("message") or "等待扫码…"))

    # ---- 公共 ------------------------------------------------------------ #
    def _show_qr(self, url, key, tip):
        try:
            png = qrcodegen.make_png(url, ecc="M", scale=8, border=4)
        except Exception:                               # noqa: BLE001
            png = b""
        with self._lock:
            self._deadline = time.time() + QR_LIFETIME
        self._set("pending", tip, png=png, url=url, key=key)

    def _finish_success(self, cookie, token, mid):
        result = {"cookie": cookie, "token": token, "mid": mid, "uname": ""}
        self._set("confirmed", "登录成功", result=result)
        if self._on_success:
            try:
                self._on_success(dict(result))
            except Exception:                           # noqa: BLE001
                pass
        # 拉一次昵称用于界面展示（失败不影响登录结果）
        if cookie:
            ok, uname, nmid, _msg = fetch_nav(cookie)
            if ok:
                with self._lock:
                    if self._state == "confirmed":
                        self._result["uname"] = uname
                        self._result["mid"] = nmid or mid
                        self._seq += 1
