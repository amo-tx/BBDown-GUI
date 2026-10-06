#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""生成 BBDown 图形界面 的应用图标（纯标准库，无需 Pillow）。

用法:  python make_icon.py            # 输出 icon.ico 与 icon_preview.png
设计:  粉紫渐变圆角方块 + 白色下载箭头与托盘，4 倍超采样抗锯齿。
"""

import os
import struct
import zlib

HERE = os.path.dirname(os.path.abspath(__file__))
SS = 4                                   # 超采样倍数
TOP = (0xFB, 0x72, 0x99)                 # B 站粉
BOTTOM = (0x6C, 0x4C, 0xE0)              # 紫


def _blend(c1, c2, t):
    return tuple(round(a + (b - a) * t) for a, b in zip(c1, c2))


def _in_round_rect(x, y, w, h, r):
    """圆角矩形内部判定：仅四个角做圆形裁剪，其余区域一律为内部。"""
    if x < 0 or y < 0 or x > w - 1 or y > h - 1:
        return False
    dx, dy = w - 1 - r, h - 1 - r
    if x < r and y < r:                  # 左上角
        return (x - r) ** 2 + (y - r) ** 2 <= r * r
    if x > dx and y < r:                 # 右上角
        return (x - dx) ** 2 + (y - r) ** 2 <= r * r
    if x < r and y > dy:                 # 左下角
        return (x - r) ** 2 + (y - dy) ** 2 <= r * r
    if x > dx and y > dy:                # 右下角
        return (x - dx) ** 2 + (y - dy) ** 2 <= r * r
    return True


def _in_arrow(x, y, n):
    """白色下载箭头（设计坐标 256×256，等比映射到 n×n）。"""
    u, v = x / n * 256.0, y / n * 256.0
    if 112 <= u <= 144 and 44 <= v <= 136:                       # 竖杆
        return True
    if 136 <= v <= 180:                                          # 箭头三角
        half = (180 - v) / (180 - 136) * 36.0
        if abs(u - 128) <= half:
            return True
    if 72 <= u <= 184 and 196 <= v <= 214:                       # 底部托盘
        return True
    return False


def render(size):
    """渲染为 size×size 的 RGBA 字节串（超采样后盒式滤波，按 alpha 加权）。"""
    n = size * SS
    buf = [(0, 0, 0, 0)] * (n * n)
    r = n * 0.22

    for j in range(n):
        bg = _blend(TOP, BOTTOM, j / (n - 1))
        for i in range(n):
            if _in_arrow(i, j, n):
                buf[j * n + i] = (255, 255, 255, 255)
            elif _in_round_rect(i, j, n, n, r):
                buf[j * n + i] = (bg[0], bg[1], bg[2], 255)

    out = bytearray()
    for j in range(size):
        for i in range(size):
            r_ = g_ = b_ = a_ = 0
            for dj in range(SS):
                for di in range(SS):
                    p = buf[(j * SS + dj) * n + (i * SS + di)]
                    r_ += p[0] * p[3]
                    g_ += p[1] * p[3]
                    b_ += p[2] * p[3]
                    a_ += p[3]
            if a_ == 0:
                out += bytes((0, 0, 0, 0))
            else:
                out += bytes((round(r_ / a_), round(g_ / a_), round(b_ / a_),
                              round(a_ / (SS * SS))))
    return bytes(out)


def write_ico(path, sizes=(256, 64, 48, 32, 16)):
    imgs = [(s, render(s)) for s in sizes]
    header = struct.pack("<HHH", 0, 1, len(imgs))
    offset = 6 + 16 * len(imgs)
    entries, blob = b"", b""
    for s, rgba in imgs:
        w = 0 if s >= 256 else s          # 256 用 0 表示
        bih = struct.pack("<IiiHHIIiiII", 40, s, s * 2, 1, 32, 0,
                          len(rgba), 0, 0, 0, 0)   # 高度写两倍（XOR + AND 掩码）
        body = bih + rgba
        entries += struct.pack("<BBBBHHII", w, w, 0, 0, 1, 32, len(body), offset)
        blob += body
        offset += len(body)
    with open(path, "wb") as f:
        f.write(header + entries + blob)
    return os.path.getsize(path)


def write_png(path, size=256):
    rgba = render(size)
    raw = b"".join(b"\x00" + rgba[y * size * 4:(y + 1) * size * 4]
                   for y in range(size))

    def chunk(tag, data):
        body = tag + data
        return (struct.pack(">I", len(data)) + body
                + struct.pack(">I", zlib.crc32(body) & 0xFFFFFFFF))

    png = (b"\x89PNG\r\n\x1a\n"
           + chunk(b"IHDR", struct.pack(">IIBBBBB", size, size, 8, 6, 0, 0, 0))
           + chunk(b"IDAT", zlib.compress(raw, 9))
           + chunk(b"IEND", b""))
    with open(path, "wb") as f:
        f.write(png)
    return os.path.getsize(path)


if __name__ == "__main__":
    ico = os.path.join(HERE, "icon.ico")
    png = os.path.join(HERE, "icon_preview.png")
    print("icon.ico        %6.1f KB" % (write_ico(ico) / 1024))
    print("icon_preview.png%6.1f KB" % (write_png(png) / 1024))
