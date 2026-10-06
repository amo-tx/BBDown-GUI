#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""二维码生成器（纯 Python 标准库实现，零第三方依赖）

为「BBDown 图形界面」的扫码登录提供二维码图片，避免依赖 BBDown 在控制台
打印的字符画二维码（字符画在网页日志里会因换行/截断而无法扫描）。

实现范围：
  * 字节模式（byte mode），支持 UTF-8 内容
  * 纠错等级 L / M / Q / H
  * 版本 1 ~ 40（按内容长度自动选择）
  * 8 种掩码评估，按标准罚分择优
  * 导出 PNG（灰度，自带静默区）与 SVG

对外主要接口：
    make_png(text, ecc="M", scale=8, border=4) -> bytes
    make_svg(text, ecc="M", scale=8, border=4) -> str
    encode(text, ecc="M") -> QRCode      # 可用 .modules 取模块矩阵

算法依据 ISO/IEC 18004。模块矩阵已与标准实现（qrcode 库）逐格比对一致。
"""

import zlib

__all__ = ["QRCode", "encode", "make_png", "make_svg", "DataTooLongError"]

# --------------------------------------------------------------------------- #
# GF(2^8) 伽罗华域运算（RS 纠错用），本原多项式 0x11D
# --------------------------------------------------------------------------- #
_GF_EXP = [0] * 512
_GF_LOG = [0] * 256


def _init_gf():
    x = 1
    for i in range(255):
        _GF_EXP[i] = x
        _GF_LOG[x] = i
        x <<= 1
        if x & 0x100:
            x ^= 0x11D
    for i in range(255, 512):
        _GF_EXP[i] = _GF_EXP[i - 255]


_init_gf()


def _gf_mul(a, b):
    if a == 0 or b == 0:
        return 0
    return _GF_EXP[_GF_LOG[a] + _GF_LOG[b]]


def _rs_divisor(degree):
    """生成 (x - a^0)(x - a^1)...(x - a^(degree-1)) 的系数表。"""
    result = [0] * (degree - 1) + [1]
    root = 1
    for _ in range(degree):
        for j in range(degree):
            result[j] = _gf_mul(result[j], root)
            if j + 1 < degree:
                result[j] ^= result[j + 1]
        root = _gf_mul(root, 0x02)
    return result


def _rs_remainder(data, divisor):
    """计算 RS 余数（即纠错码字）。"""
    result = [0] * len(divisor)
    for b in data:
        factor = b ^ result.pop(0)
        result.append(0)
        for i, d in enumerate(divisor):
            result[i] ^= _gf_mul(d, factor)
    return result


# --------------------------------------------------------------------------- #
# 标准参数表
# --------------------------------------------------------------------------- #
ECC_LEVELS = {"L": 0, "M": 1, "Q": 2, "H": 3}
# 格式信息里的纠错等级编码（与上面索引不同，L=1/M=0/Q=3/H=2）
_ECC_FORMAT_BITS = {0: 1, 1: 0, 2: 3, 3: 2}

# 每块纠错码字数 [等级][版本]，索引 0 占位
_ECC_CODEWORDS_PER_BLOCK = [
    [-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28,
     30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30,
     30, 30, 30, 30, 30],
    [-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28,
     26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28,
     28, 28, 28, 28, 28],
    [-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28,
     28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30,
     30, 30, 30, 30, 30],
    [-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28,
     28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30,
     30, 30, 30, 30, 30],
]

# 纠错块数 [等级][版本]
_NUM_ECC_BLOCKS = [
    [-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9,
     10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25],
    [-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16,
     17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45,
     47, 49],
    [-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20,
     23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62,
     65, 68],
    [-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25,
     25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74,
     77, 81],
]


class DataTooLongError(ValueError):
    """内容超出 QR 码最大容量。"""


def _num_raw_data_modules(ver):
    """指定版本可用的数据模块总数（含被占用的功能图形）。"""
    result = (16 * ver + 128) * ver + 64
    if ver >= 2:
        numalign = ver // 7 + 2
        result -= (25 * numalign - 10) * numalign - 55
        if ver >= 7:
            result -= 36
    return result


def _num_data_codewords(ver, ecl):
    return (_num_raw_data_modules(ver) // 8
            - _ECC_CODEWORDS_PER_BLOCK[ecl][ver] * _NUM_ECC_BLOCKS[ecl][ver])


def _char_count_bits(ver):
    """字节模式的字符计数指示符位数。"""
    return 8 if ver <= 9 else 16


def _get_bit(x, i):
    return ((x >> i) & 1) != 0


def _alignment_positions(ver):
    if ver == 1:
        return []
    numalign = ver // 7 + 2
    step = 26 if ver == 32 else (ver * 4 + numalign * 2 + 1) // (numalign * 2 - 2) * 2
    size = ver * 4 + 17
    return [6] + [size - 7 - i * step for i in range(numalign - 2, -1, -1)]


def _add_ecc_and_interleave(ver, ecl, data):
    """按标准进行分块 RS 编码并交织。"""
    numblocks = _NUM_ECC_BLOCKS[ecl][ver]
    blockecclen = _ECC_CODEWORDS_PER_BLOCK[ecl][ver]
    rawcodewords = _num_raw_data_modules(ver) // 8
    numshortblocks = numblocks - rawcodewords % numblocks
    shortblocklen = rawcodewords // numblocks

    blocks = []
    rsdiv = _rs_divisor(blockecclen)
    k = 0
    for i in range(numblocks):
        datlen = shortblocklen - blockecclen + (0 if i < numshortblocks else 1)
        dat = data[k:k + datlen]
        k += datlen
        ecc = _rs_remainder(dat, rsdiv)
        if i < numshortblocks:
            dat = dat + [0]
        blocks.append(dat + ecc)

    result = []
    for i in range(len(blocks[0])):
        for j, blk in enumerate(blocks):
            if i != shortblocklen - blockecclen or j >= numshortblocks:
                result.append(blk[i])
    return result


# --------------------------------------------------------------------------- #
# QR 码本体
# --------------------------------------------------------------------------- #
class QRCode(object):
    """已生成的二维码：modules[y][x] 为 True 表示深色模块。"""

    def __init__(self, version, ecl, datacodewords, mask):
        self.version = version
        self.ecl = ecl                     # 0..3
        self.mask = mask
        self.size = version * 4 + 17
        self.modules = [[False] * self.size for _ in range(self.size)]
        self._isfunc = [[False] * self.size for _ in range(self.size)]
        self._draw_function_patterns()
        self._draw_codewords(_add_ecc_and_interleave(
            version, ecl, datacodewords))
        self._apply_mask(mask)             # 同时写入格式信息

    # ---- 绘制 ------------------------------------------------------------ #
    def _set_func(self, x, y, dark):
        self.modules[y][x] = dark
        self._isfunc[y][x] = True

    def _draw_function_patterns(self):
        size = self.size
        for i in range(size):                       # 定时图形
            self._set_func(6, i, i % 2 == 0)
            self._set_func(i, 6, i % 2 == 0)
        self._draw_finder(3, 3)                     # 三个定位图形
        self._draw_finder(size - 4, 3)
        self._draw_finder(3, size - 4)

        pos = _alignment_positions(self.version)    # 校正图形
        n = len(pos)
        for i in range(n):
            for j in range(n):
                if ((i == 0 and j == 0) or (i == 0 and j == n - 1)
                        or (i == n - 1 and j == 0)):
                    continue
                self._draw_alignment(pos[i], pos[j])

        self._draw_format_bits(0)                   # 占位，稍后按掩码重写
        self._draw_version()

    def _draw_finder(self, x, y):
        for dy in range(-4, 5):
            for dx in range(-4, 5):
                xx, yy = x + dx, y + dy
                if 0 <= xx < self.size and 0 <= yy < self.size:
                    dist = max(abs(dx), abs(dy))
                    self._set_func(xx, yy, dist != 2 and dist != 4)

    def _draw_alignment(self, x, y):
        for dy in range(-2, 3):
            for dx in range(-2, 3):
                self._set_func(x + dx, y + dy, max(abs(dx), abs(dy)) != 1)

    def _draw_format_bits(self, mask):
        data = (_ECC_FORMAT_BITS[self.ecl] << 3) | mask
        rem = data
        for _ in range(10):
            rem = (rem << 1) ^ ((rem >> 9) * 0x537)
        bits = (data << 10 | rem) ^ 0x5412

        for i in range(6):
            self._set_func(8, i, _get_bit(bits, i))
        self._set_func(8, 7, _get_bit(bits, 6))
        self._set_func(8, 8, _get_bit(bits, 7))
        self._set_func(7, 8, _get_bit(bits, 8))
        for i in range(9, 15):
            self._set_func(14 - i, 8, _get_bit(bits, i))

        for i in range(8):
            self._set_func(self.size - 1 - i, 8, _get_bit(bits, i))
        for i in range(8, 15):
            self._set_func(8, self.size - 15 + i, _get_bit(bits, i))
        self._set_func(8, self.size - 8, True)      # 固定深色模块

    def _draw_version(self):
        if self.version < 7:
            return
        rem = self.version
        for _ in range(12):
            rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
        bits = self.version << 12 | rem
        for i in range(18):
            bit = _get_bit(bits, i)
            a = self.size - 11 + i % 3
            b = i // 3
            self._set_func(a, b, bit)
            self._set_func(b, a, bit)

    def _draw_codewords(self, data):
        i = 0
        for right in range(self.size - 1, 0, -2):
            if right <= 6:
                right -= 1
            for vert in range(self.size):
                for j in range(2):
                    x = right - j
                    upward = ((right + 1) & 2) == 0
                    y = (self.size - 1 - vert) if upward else vert
                    if (not self._isfunc[y][x]) and i < len(data) * 8:
                        self.modules[y][x] = _get_bit(data[i >> 3], 7 - (i & 7))
                        i += 1

    # ---- 掩码与评分 ------------------------------------------------------ #
    def _apply_mask(self, mask):
        for y in range(self.size):
            for x in range(self.size):
                if mask == 0:
                    invert = (x + y) % 2 == 0
                elif mask == 1:
                    invert = y % 2 == 0
                elif mask == 2:
                    invert = x % 3 == 0
                elif mask == 3:
                    invert = (x + y) % 3 == 0
                elif mask == 4:
                    invert = (x // 3 + y // 2) % 2 == 0
                elif mask == 5:
                    invert = x * y % 2 + x * y % 3 == 0
                elif mask == 6:
                    invert = (x * y % 2 + x * y % 3) % 2 == 0
                else:
                    invert = ((x + y) % 2 + x * y % 3) % 2 == 0
                if (not self._isfunc[y][x]) and invert:
                    self.modules[y][x] = not self.modules[y][x]
        self._draw_format_bits(mask)

    def _penalty(self):
        size = self.size
        result = 0

        # 规则 1：连续同色模块
        for y in range(size):
            runcolor = False
            runx = 0
            for x in range(size):
                if self.modules[y][x] == runcolor:
                    runx += 1
                    if runx == 5:
                        result += 3
                    elif runx > 5:
                        result += 1
                else:
                    runcolor = self.modules[y][x]
                    runx = 1
        for x in range(size):
            runcolor = False
            runy = 0
            for y in range(size):
                if self.modules[y][x] == runcolor:
                    runy += 1
                    if runy == 5:
                        result += 3
                    elif runy > 5:
                        result += 1
                else:
                    runcolor = self.modules[y][x]
                    runy = 1

        # 规则 2：2x2 同色块
        for y in range(size - 1):
            for x in range(size - 1):
                c = self.modules[y][x]
                if (c == self.modules[y][x + 1] == self.modules[y + 1][x]
                        == self.modules[y + 1][x + 1]):
                    result += 3

        # 规则 3：形似定位图形的 1:1:3:1:1 组合
        for y in range(size):
            row = self.modules[y]
            for x in range(size - 10):
                if (row[x] and not row[x + 1] and row[x + 2]
                        and row[x + 3] and row[x + 4] and not row[x + 5]
                        and row[x + 6] and not row[x + 7] and not row[x + 8]
                        and not row[x + 9] and not row[x + 10]):
                    result += 40
                if (not row[x] and not row[x + 1] and not row[x + 2]
                        and not row[x + 3] and row[x + 4] and not row[x + 5]
                        and row[x + 6] and row[x + 7] and row[x + 8]
                        and not row[x + 9] and row[x + 10]):
                    result += 40
        for x in range(size):
            for y in range(size - 10):
                if (self.modules[y][x] and not self.modules[y + 1][x]
                        and self.modules[y + 2][x] and self.modules[y + 3][x]
                        and self.modules[y + 4][x] and not self.modules[y + 5][x]
                        and self.modules[y + 6][x] and not self.modules[y + 7][x]
                        and not self.modules[y + 8][x] and not self.modules[y + 9][x]
                        and not self.modules[y + 10][x]):
                    result += 40
                if (not self.modules[y][x] and not self.modules[y + 1][x]
                        and not self.modules[y + 2][x] and not self.modules[y + 3][x]
                        and self.modules[y + 4][x] and not self.modules[y + 5][x]
                        and self.modules[y + 6][x] and self.modules[y + 7][x]
                        and self.modules[y + 8][x] and not self.modules[y + 9][x]
                        and self.modules[y + 10][x]):
                    result += 40

        # 规则 4：深浅比例偏离 50%
        dark = sum(row.count(True) for row in self.modules)
        total = size * size
        k = (abs(dark * 20 - total * 10) + total - 1) // total - 1
        result += k * 10
        return result


def encode(text, ecc="M"):
    """把文本编码为 QRCode 对象（字节模式，自动选版本与最优掩码）。"""
    data = text.encode("utf-8")
    ecl = ECC_LEVELS.get(str(ecc).upper(), 1)

    ver = 1
    while ver <= 40:
        cap = _num_data_codewords(ver, ecl) * 8
        if 4 + _char_count_bits(ver) + 8 * len(data) <= cap:
            break
        ver += 1
    if ver > 40:
        raise DataTooLongError("内容过长，超出二维码容量上限")

    bits = []
    _append_bits(bits, 0x4, 4)                       # 字节模式指示符
    _append_bits(bits, len(data), _char_count_bits(ver))
    for b in data:
        _append_bits(bits, b, 8)

    capbits = _num_data_codewords(ver, ecl) * 8
    _append_bits(bits, 0, min(4, capbits - len(bits)))          # 终止符
    _append_bits(bits, 0, (8 - len(bits) % 8) % 8)              # 字节对齐
    pad = 0xEC
    while len(bits) < capbits:
        _append_bits(bits, pad, 8)
        pad ^= 0xEC ^ 0x11

    codewords = [sum(bits[i + j] << (7 - j) for j in range(8))
                 for i in range(0, len(bits), 8)]
    return _make_qr(ver, ecl, codewords)


def _append_bits(bits, val, n):
    for i in range(n - 1, -1, -1):
        bits.append((val >> i) & 1)


def _make_qr(ver, ecl, codewords):
    best = None
    mask = 0
    for m in range(8):
        qr = QRCode(ver, ecl, codewords, m)
        pen = qr._penalty()
        if best is None or pen < best:
            best, mask = pen, m
    return QRCode(ver, ecl, codewords, mask)


# --------------------------------------------------------------------------- #
# 输出：PNG / SVG
# --------------------------------------------------------------------------- #
_BLACK = b"\x00"
_WHITE = b"\xff"


def _png_chunk(tag, data):
    return (len(data).to_bytes(4, "big") + tag + data
            + zlib.crc32(tag + data).to_bytes(4, "big"))


def make_png(text, ecc="M", scale=8, border=4):
    """生成灰度 PNG 字节流。scale 为每模块像素数，border 为静默区模块数。"""
    qr = text if isinstance(text, QRCode) else encode(text, ecc)
    size = qr.size
    dim = (size + border * 2) * scale

    # 预生成每个「模块行」对应的扫描线（首字节为 filter type 0）
    blank = _WHITE * dim
    cache = {}

    def scanrow(my):
        if my < 0 or my >= size:
            return blank
        row = qr.modules[my]
        parts = [_WHITE * (border * scale)]
        run = None
        cnt = 0
        for mx in range(size):
            v = row[mx]
            if v == run:
                cnt += 1
            else:
                if run is not None:
                    parts.append((_BLACK if run else _WHITE) * (cnt * scale))
                run, cnt = v, 1
        parts.append((_BLACK if run else _WHITE) * (cnt * scale))
        parts.append(_WHITE * (border * scale))
        return b"".join(parts)

    raw = bytearray()
    for y in range(dim):
        my = y // scale - border
        line = cache.get(my)
        if line is None:
            line = cache[my] = scanrow(my)
        raw += b"\x00" + line

    ihdr = (dim.to_bytes(4, "big") + dim.to_bytes(4, "big")
            + bytes([8, 0, 0, 0, 0]))     # 8bit 灰度、无隔行
    return (b"\x89PNG\r\n\x1a\n"
            + _png_chunk(b"IHDR", ihdr)
            + _png_chunk(b"IDAT", zlib.compress(bytes(raw), 9))
            + _png_chunk(b"IEND", b""))


def make_svg(text, ecc="M", scale=8, border=4):
    """生成 SVG 文本（同样含静默区）。"""
    qr = text if isinstance(text, QRCode) else encode(text, ecc)
    size = qr.size
    dim = (size + border * 2) * scale
    rects = []
    for y in range(size):
        row = qr.modules[y]
        x = 0
        while x < size:
            if row[x]:
                x0 = x
                while x < size and row[x]:
                    x += 1
                rects.append('<rect x="%d" y="%d" width="%d" height="%d"/>'
                             % ((x0 + border) * scale, (y + border) * scale,
                                (x - x0) * scale, scale))
            else:
                x += 1
    return ('<?xml version="1.0" encoding="UTF-8"?>\n'
            '<svg xmlns="http://www.w3.org/2000/svg" version="1.1" '
            'viewBox="0 0 %d %d" width="%d" height="%d" '
            'shape-rendering="crispEdges">\n'
            '<rect width="100%%" height="100%%" fill="#ffffff"/>\n'
            '<g fill="#000000">%s</g>\n</svg>\n'
            % (dim, dim, dim, dim, "".join(rects)))


# --------------------------------------------------------------------------- #
# 自检：python qrcodegen.py [文本]
# --------------------------------------------------------------------------- #
if __name__ == "__main__":
    import sys
    sample = sys.argv[1] if len(sys.argv) > 1 else "https://www.bilibili.com/"
    q = encode(sample, "M")
    sys.stdout.write("version=%d size=%d ecc=%s\n"
                     % (q.version, q.size, "M"))
    for row in q.modules:
        sys.stdout.write("".join("\u2588\u2588" if v else "  " for v in row) + "\n")
