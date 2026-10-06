#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""从磁盘分片增长推算 BBDown 的下载进度（纯标准库）。

为什么需要它
------------
BBDown 1.6.3 只在 stdout 是终端时才画进度条；一旦输出被重定向（GUI 必须用管道
读取），它**完全不输出百分比、速度、剩余时间**，日志里只剩阶段标记：

    [ts] - 开始下载P1视频...
    [ts] - 合并视频分片...
    [ts] - 清理分片...
    [ts] - 开始下载P1音频...
    [ts] - 下载P1完毕
    [ts] - 开始合并音视频...
    [ts] - 任务完成

好消息是它在下载期间会把分片落到磁盘上。实测 1.6.3 的命名规则（<work-dir>
即界面里的「保存目录」）：

    00000_<aid>.P<n>.<cid>.vclip     视频分片（边下边长，前缀 00000_ 为分片序号）
    00000_<aid>.P<n>.<cid>.aclip     音频分片
    <aid>.P<n>.<cid>.mp4             合并后的视频（分片清理后）
    <aid>.P<n>.<cid>.m4a             合并后的音频
    <成品标题>.mp4 / [P<n>]<标题>.mp4  最终产物（**没有** .P<n>. 中缀，需排除）

而解析阶段就会打印每个流的预估体积：

    [ts] - 已选择的流:
                            [视频] [480P 清晰] [852x480] [AVC] [25] [786 kbps] [~20.44 MB]
                            [音频] [M4A] [203 kbps] [~5.28 MB]

实测该预估的单位是 MiB（预估 25.72 MiB vs 实际产物 25.75 MiB，误差 0.1%），
可以直接当分母。

因此进度模型是：
    进度 = 「分片字节增量」÷「预估体积」
    速度 = 字节增量的指数滑动平均
    剩余 = 本阶段剩余字节 ÷ 速度

设计约束
--------
* **只认分片**：靠文件名里的 `<aid>.P<n>.<cid>.<ext>` 模式识别，成品与封面、
  弹幕、字幕一律不计入，否则混流产物会让进度虚高一大截。
* **不回退**：合并分片会让 .vclip 变成 .mp4（甚至短暂并存），所以每个分页的
  视频/音频字节都取「分片与合并产物之和」的历史峰值。
* **不误算旧残留**：启动时对分片做基线快照，只统计增量；上一次中断留下的分片
  不会被算进本次进度。
* **线程安全**：feed() 由进程输出泵调用，tick() 由监控线程调用，
  snapshot() 由 HTTP 线程调用，全部走同一把锁。
"""

import collections
import os
import re
import threading
import time

MiB = 1048576.0

# 合并产物（分片清理后出现，代表同一份数据的另一种形态）
MERGED_EXT = {"vclip": "segv", "aclip": "sega", "mp4": "ovv", "m4a": "ova"}
# 成品 / 附属文件扩展名（只用于「产物列表」，不参与进度）
OUTPUT_EXT = frozenset(("mp4", "mkv", "flv", "m4a", "mp3", "aac",
                        "ass", "xml", "srt", "jpg", "png"))

# <aid>.P<n>.<cid>.<ext>，允许 00000_ 之类的前缀；成品 [P1]标题.mp4 不会命中
_FRAG_RE = re.compile(r"(\d+)\.P(\d+)\.[^.\\/]+\.([A-Za-z0-9]+)$")
# [2026-10-06 12:42:43.076] - 日志行前缀（用于区分缩进的流清单）
_LOG_RE = re.compile(r"^\[\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}")
# 开始解析P1: 80433022... (1 of 1)
_START_RE = re.compile(r"开始解析P(\d+)\s*[:：].*?\((\d+)\s*of\s*(\d+)\)")
# 获取aid结束: 80433022
_AID_RE = re.compile(r"获取aid结束\s*[:：]\s*(\d+)")
# [视频] [480P 清晰] ... [~20.44 MB]
_EST_RE = re.compile(r"\[(视频|音频)\].*?\[~([\d.]+)\s*MB\]")

# (正则, 阶段, 界面文案)
_STAGE_RULES = (
    (re.compile(r"开始下载P\d+视频"), "video", "正在下载视频流…"),
    (re.compile(r"开始下载P\d+音频"), "audio", "正在下载音频流…"),
    (re.compile(r"开始下载P\d+弹幕"), "extra", "正在下载弹幕…"),
    (re.compile(r"开始下载P\d+字幕"), "extra", "正在下载字幕…"),
    (re.compile(r"开始下载P\d+封面"), "extra", "正在下载封面…"),
    (re.compile(r"正在下载弹幕"), "extra", "正在下载弹幕…"),
    (re.compile(r"正在保存弹幕"), "extra", "正在保存弹幕…"),
    (re.compile(r"合并视频分片"), "video", "正在合并视频分片…"),
    (re.compile(r"合并音频分片"), "audio", "正在合并音频分片…"),
    (re.compile(r"下载P\d+完毕"), "page_done", "本分P下载完成…"),
    (re.compile(r"开始合并音视频"), "mux", "正在混流（合并音视频）…"),
    (re.compile(r"清理临时文件"), "done", "正在清理临时文件…"),
    (re.compile(r"任务完成"), "done", "任务完成"),
)

# 无预估体积时，各阶段在「本分页」里占的固定比例
_STAGE_FRAC = {"parse": 0.03, "video": 0.45, "audio": 0.78,
               "extra": 0.86, "page_done": 0.94, "mux": 0.94, "done": 1.0}

_NOISE_DIRS = frozenset(("node_modules", ".git", ".workbuddy", "__pycache__",
                         "BBDown-GUI", "tools", "$RECYCLE.BIN",
                         "System Volume Information"))


class DownloadProgress(object):
    """跟踪一次 download 任务的进度。"""

    POLL = 0.4          # 磁盘采样间隔（秒）
    WINDOW = 6.0        # 速度统计窗口（秒）
    ALPHA = 0.45        # 速度平滑系数
    MIN_DT = 0.5        # 窗口内至少积累这么久才刷新速度
    MAX_ENTRIES = 4000  # 目录遍历上限（防御性保护）
    FULL_SCAN = 2.5     # 全目录重扫间隔（秒），其间只扫已知分片目录
    RETRY_SCAN = 0.8    # 已知目录为空时，最短多久做一次全扫描

    def __init__(self, root, start=None):
        self.root = root or ""
        self.start = time.time() if start is None else start
        self._lock = threading.RLock()

        self.aid = ""
        self.page = 1
        self.pageCount = 1
        self.pages = {}          # 分页 -> {"v": 预估字节, "a": 预估字节}
        self.measured = {}       # 分页 -> {"v": 历史峰值, "a": 历史峰值}
        self.stage = "parse"
        self.stageText = "正在解析视频信息…"
        self.percent = 0.0
        self.speed = 0.0
        self.eta = 0.0
        self.done = False
        self.ok = None
        self.outputs = []

        self._selecting = False
        self._win = collections.deque()   # (时间, 累计字节) 滑动窗口
        self._peak = 0.0         # 进度单调保护
        self._dirs = set()       # 曾出现分片文件的目录
        self._full_at = 0.0      # 上次全量扫描时间
        self._out_at = 0.0       # 上次产物扫描时间
        self.baseline = self._snapshot_existing()

    # ------------------------------------------------------------------ #
    # 磁盘侧
    # ------------------------------------------------------------------ #
    def _walk(self, full):
        """遍历保存目录，返回命中的分片文件路径列表（深度 ≤ 3）。"""
        root = self.root
        if not root or not os.path.isdir(root):
            return []
        hits = []
        if full:
            count = 0
            self._dirs = set()
            for dirpath, dirnames, filenames in os.walk(root):
                rel = os.path.relpath(dirpath, root)
                depth = 0 if rel == "." else rel.count(os.sep) + 1
                if depth >= 3:
                    dirnames[:] = []
                else:
                    dirnames[:] = [d for d in dirnames
                                   if d not in _NOISE_DIRS and not d.startswith("$")]
                for fn in filenames:
                    count += 1
                    if count > self.MAX_ENTRIES:
                        return hits
                    if _FRAG_RE.search(fn):
                        hits.append(os.path.join(dirpath, fn))
                        self._dirs.add(dirpath)
            self._full_at = time.time()
        else:
            for d in list(self._dirs):
                try:
                    entries = os.scandir(d)
                except OSError:
                    self._dirs.discard(d)
                    continue
                with entries:
                    for e in entries:
                        if _FRAG_RE.search(e.name):
                            hits.append(e.path)
        return hits

    def _iter_frags(self):
        """取当前的分片文件；命中为空时尽早重新全扫。

        只靠 FULL_SCAN 定时重扫是不够的：任务刚开始时缓存目录还是空的，
        若此时时钟刚走过一轮全扫，快下载（几秒内结束）会全程看不到任何进度。
        """
        now = time.time()
        if (now - self._full_at) > self.FULL_SCAN:
            return self._walk(True)
        hits = self._walk(False)
        if not hits and (now - self._full_at) > self.RETRY_SCAN:
            return self._walk(True)
        return hits

    def _snapshot_existing(self):
        """记录任务开始前已存在的分片体积，用于算增量。"""
        base = {}
        for p in self._walk(True):
            try:
                base[p] = os.path.getsize(p)
            except OSError:
                continue
        # 这次全扫只是取基线，不能当作「本轮已扫过」：否则接下来
        # FULL_SCAN 秒内只会查那份还没出现分片的空缓存。
        self._full_at = 0.0
        self._dirs = set()
        return base

    def _read_frags(self):
        """返回 {分页: {"segv":..,"sega":..,"ovv":..,"ova":..}} 的实时体积。"""
        live = {}
        for p in self._iter_frags():
            m = _FRAG_RE.search(os.path.basename(p))
            if not m:
                continue
            if self.aid and m.group(1) != self.aid:
                continue                      # 别的视频的残留
            bucket = MERGED_EXT.get(m.group(3).lower())
            if not bucket:
                continue
            try:
                size = os.path.getsize(p)
            except OSError:
                continue
            if size <= 0:
                continue
            base = self.baseline.get(p, 0)
            if base and size < base:
                base = 0                      # 被截断重建 → 当作本次全新下载
            if base == 0:
                try:
                    if os.path.getmtime(p) < self.start - 2:
                        continue              # 上一次中断留下的旧分片
                except OSError:
                    pass
            delta = size - base
            if delta <= 0:
                continue
            slot = live.setdefault(int(m.group(2)), {"segv": 0, "sega": 0,
                                                     "ovv": 0, "ova": 0})
            slot[bucket] += delta
        return live

    def _scan_outputs(self):
        """下载目录中的成品文件（本次任务产生），可能位于子目录里。

        分片目录（<work-dir>/<aid>/）会被跳过：封面 {aid}.jpg、{aid}.tmp 之类都
        是下载过程中的临时文件，不是交付物。
        """
        root = self.root
        if not root or not os.path.isdir(root):
            return
        out = []
        count = 0
        for dirpath, dirnames, filenames in os.walk(root):
            rel = os.path.relpath(dirpath, root)
            depth = 0 if rel == "." else rel.count(os.sep) + 1
            if depth >= 3:
                dirnames[:] = []
            else:
                dirnames[:] = [d for d in dirnames
                               if d not in _NOISE_DIRS and not d.startswith("$")]
            if dirpath in self._dirs:
                continue                      # 分片临时目录，不算产物
            for fn in filenames:
                count += 1
                if count > self.MAX_ENTRIES:
                    break
                if _FRAG_RE.search(fn):
                    continue
                if self.aid and (fn == self.aid or fn.startswith(self.aid + ".")):
                    continue                  # {aid}.jpg / {aid}.tmp 等临时文件
                if fn.rsplit(".", 1)[-1].lower() not in OUTPUT_EXT:
                    continue
                fp = os.path.join(dirpath, fn)
                try:
                    st = os.stat(fp)
                except OSError:
                    continue
                if st.st_size <= 0 or st.st_mtime < self.start - 2:
                    continue
                out.append({"name": fn, "size": st.st_size})
        if out:
            self.outputs = sorted(out, key=lambda x: -x["size"])

    # ------------------------------------------------------------------ #
    # 日志侧
    # ------------------------------------------------------------------ #
    def feed(self, text):
        """喂入一行 BBDown 输出（已去 ANSI）。"""
        s = (text or "").strip()
        if not s:
            return
        with self._lock:
            m = _AID_RE.search(s)
            if m:
                self.aid = m.group(1)

            m = _START_RE.search(s)
            if m:
                self.page = max(1, int(m.group(1)))
                self.pageCount = max(1, int(m.group(3)))
                self.stage = "parse"
                self.stageText = ("正在解析第 %d/%d 个分P…" % (self.page, self.pageCount)
                                  if self.pageCount > 1 else "正在解析视频信息…")
                self.pages.setdefault(self.page, {"v": 0, "a": 0})
                self._selecting = False
                return

            if "已选择的流" in s:
                self._selecting = True
                return
            if self._selecting:
                if _LOG_RE.match(s):
                    self._selecting = False        # 流清单结束，本行继续按阶段处理
                else:
                    em = _EST_RE.search(s)
                    if em:
                        slot = self.pages.setdefault(self.page, {"v": 0, "a": 0})
                        val = float(em.group(2)) * MiB
                        key = "v" if em.group(1) == "视频" else "a"
                        if val > (slot.get(key) or 0):
                            slot[key] = val
                    return

            for rx, stage, label in _STAGE_RULES:
                if rx.search(s):
                    self.stage = stage
                    self.stageText = label
                    break

    # ------------------------------------------------------------------ #
    # 计算
    # ------------------------------------------------------------------ #
    def _est_of(self, page):
        d = self.pages.get(page) or {}
        return (d.get("v") or 0), (d.get("a") or 0)

    def _got_of(self, page):
        d = self.measured.get(page) or {}
        return (d.get("v") or 0), (d.get("a") or 0)

    def _downloaded(self):
        return sum(self._got_of(p)[0] + self._got_of(p)[1] for p in self.measured)

    def _total(self):
        tot = 0.0
        for p in set(self.pages) | set(self.measured):
            ev, ea = self._est_of(p)
            gv, ga = self._got_of(p)
            tot += max(ev, gv) + max(ea, ga)
        return tot

    def _page_frac(self):
        ev, ea = self._est_of(self.page)
        gv, ga = self._got_of(self.page)
        if ev + ea > 0:
            share = (min(gv, ev) + min(ga, ea)) / (ev + ea)
            frac = 0.05 + 0.88 * share
        else:
            frac = _STAGE_FRAC.get(self.stage, 0.02)
        if self.stage in ("page_done", "mux", "done"):
            frac = max(frac, 0.94)
        return min(frac, 1.0)

    def _percent(self):
        pc = max(1, self.pageCount)
        base = min(max(self.page - 1, 0), pc - 1)
        pct = (base + self._page_frac()) / pc * 100.0
        if self.done and self.ok:
            return 100.0
        if pct > self._peak:
            self._peak = pct
        else:
            pct = self._peak                 # 进度只增不减（分P数可能在解析后才明确）
        return min(99.5 if not self.done else 100.0, pct)

    def _update_speed(self, measured):
        """速度 = 最近 WINDOW 秒的字节增量均值。

        不用瞬时差分：BBDown 是多线程分段写入，磁盘体积会「猛涨几秒、然后几秒
        不动」，瞬时差分会让速度在 0 和几十 MB/s 之间反复横跳、剩余时间随之乱跳。
        窗口均值天然平滑；窗口内增量为 0 时保留上一次的速度，避免缓冲写入期间
        显示 0 B/s。
        """
        now = time.time()
        self._win.append((now, measured))
        while len(self._win) > 2 and now - self._win[0][0] > self.WINDOW:
            self._win.popleft()
        t0, b0 = self._win[0]
        dt = now - t0
        if dt < self.MIN_DT:
            return
        raw = max(0.0, (measured - b0) / dt)
        if raw <= 0:
            return
        self.speed = raw if self.speed <= 0 else \
            (self.ALPHA * raw + (1 - self.ALPHA) * self.speed)

    def _update_eta(self):
        # 混流阶段不再有下载流量，速度与剩余时间都归零；
        # 「清理临时文件 / 任务完成」时保留最后一次速度，好让界面能显示平均速度。
        if self.stage == "mux":
            self.speed = 0.0
            self.eta = 0.0
            return
        if self.speed <= 0:
            self.eta = 0.0
            return
        ev, ea = self._est_of(self.page)
        page_total = ev + ea
        if page_total <= 0:
            self.eta = 0.0
            return
        gv, ga = self._got_of(self.page)
        # 口径是「整个任务」：本分页剩余 + 后续分页（按本页体积估）。
        # 若只算当前阶段，视频刚下完时会显示「即将完成」，而进度条才走到一半。
        remain = max(0.0, page_total - (gv + ga))
        remain += max(0, self.pageCount - self.page) * page_total
        self.eta = remain / self.speed

    def _fold(self):
        """把实时体积并入各分页的历史峰值（分片与合并产物取较大者）。"""
        for page, b in self._read_frags().items():
            cur = self.measured.setdefault(page, {"v": 0, "a": 0})
            cur["v"] = max(cur["v"], b["segv"], b["ovv"])
            cur["a"] = max(cur["a"], b["sega"], b["ova"])

    def tick(self):
        with self._lock:
            self._fold()
            measured = self._downloaded()
            self._update_speed(measured)
            self.percent = self._percent()
            self._update_eta()
            now = time.time()
            if self.stage != "parse" and now - self._out_at > 2.0:
                self._out_at = now
                self._scan_outputs()

    def finish(self, ok):
        with self._lock:
            self.ok = bool(ok)
            self.eta = 0.0
            try:
                self._fold()
                self._scan_outputs()
            except Exception:                               # noqa: BLE001
                pass
            self.done = True
            self.speed = 0.0
            self.percent = self._percent()

    def snapshot(self):
        with self._lock:
            ev, ea = self._est_of(self.page)
            return {
                "active": not self.done,
                "stage": self.stage,
                "stageText": self.stageText,
                "percent": round(self.percent, 2),
                "speed": round(self.speed, 1),
                "eta": round(self.eta, 1),
                "page": self.page,
                "pages": self.pageCount,
                "hasVideo": ev > 0,
                "hasAudio": ea > 0,
                "estimated": (ev + ea) > 0,
                "downloaded": int(self._downloaded()),
                "total": int(self._total()),
                "outputs": list(self.outputs),
                "done": self.done,
            }
