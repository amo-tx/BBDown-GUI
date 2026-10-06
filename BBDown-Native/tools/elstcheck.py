#!/usr/bin/env python3
"""一次性调试脚本：打印 MP4 / fMP4 的时间字段（mvhd / tkhd / mdhd / elst / stts）。

排查「ffmpeg 把产物当成需要尾部裁剪」时用的，不是交付物。

    python tools/elstcheck.py a.mp4 b.mp4
"""
import struct
import sys

FULL = {
    b"mvhd", b"tkhd", b"mdhd", b"hdlr", b"elst", b"stts", b"stsz",
    b"stco", b"co64", b"stsd", b"dref", b"vmhd", b"smhd",
}
CONT = {b"moov", b"trak", b"mdia", b"minf", b"stbl", b"edts", b"dinf", b"mvex"}


def walk(buf, start, end, depth, out):
    off = start
    guard = 0
    while off + 8 <= end:
        guard += 1
        if guard > 2000:
            out.append("  " * depth + "!! 同级盒数量异常，中止")
            return
        size = struct.unpack(">I", buf[off:off + 4])[0]
        bt = buf[off + 4:off + 8]
        hdr = 8
        if size == 1:
            if off + 16 > end:
                out.append("  " * depth + "!! 64 位长度越界")
                return
            size = struct.unpack(">Q", buf[off + 8:off + 16])[0]
            hdr = 16
        elif size == 0:
            size = end - off
        if size < hdr or off + size > end:
            out.append("  " * depth + f"!! {bt!r} 尺寸异常 size={size} off={off}")
            return

        body = off + hdr + (4 if bt in FULL else 0)
        ver = buf[off + hdr] if bt in FULL else None
        tag = "  " * depth + bt.decode("latin1")

        if bt == b"mvhd":
            if ver == 1:
                ts, dur = struct.unpack(">IQ", buf[body + 16:body + 28])
            else:
                ts, dur = struct.unpack(">II", buf[body + 8:body + 16])
            out.append(f"{tag} ts={ts} dur={dur} ({dur / ts:.4f}s)")
        elif bt == b"tkhd":
            if ver == 1:
                tid = struct.unpack(">I", buf[body + 16:body + 20])[0]
                dur = struct.unpack(">Q", buf[body + 24:body + 32])[0]
            else:
                tid = struct.unpack(">I", buf[body + 8:body + 12])[0]
                dur = struct.unpack(">I", buf[body + 16:body + 20])[0]
            out.append(f"{tag} trackID={tid} dur={dur}")
        elif bt == b"mdhd":
            if ver == 1:
                ts, dur = struct.unpack(">IQ", buf[body + 16:body + 28])
            else:
                ts, dur = struct.unpack(">II", buf[body + 8:body + 16])
            out.append(f"{tag} ts={ts} dur={dur} ({dur / ts:.4f}s)")
        elif bt == b"hdlr":
            out.append(f"{tag} handler={buf[body + 4:body + 8].decode('latin1')!r}")
        elif bt == b"elst":
            n = struct.unpack(">I", buf[body:body + 4])[0]
            out.append(f"{tag} v={ver} entries={n}")
            p = body + 4
            for i in range(n):
                if ver == 1:
                    seg, mt = struct.unpack(">Qq", buf[p:p + 16])
                    p += 20
                else:
                    seg, mt = struct.unpack(">Ii", buf[p:p + 8])
                    p += 12
                out.append(f"{'  ' * (depth + 1)}[{i}] segment_duration={seg} "
                           f"media_time={mt}")
        elif bt == b"stts":
            n = struct.unpack(">I", buf[body:body + 4])[0]
            p = body + 4
            tot = 0
            cnt = 0
            for _ in range(n):
                c, d = struct.unpack(">II", buf[p:p + 8])
                tot += c * d
                cnt += c
                p += 8
            out.append(f"{tag} runs={n} samples={cnt} total={tot}")
        else:
            out.append(f"{tag} size={size}")

        if bt in CONT:
            walk(buf, body, off + size, depth + 1, out)
        off += size


def main(path):
    with open(path, "rb") as f:
        buf = f.read()
    out = []
    walk(buf, 0, len(buf), 0, out)
    print(f"##### {path}  ({len(buf)} 字节)")
    print("\n".join(out))
    print()


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(0)
    for p in sys.argv[1:]:
        main(p)
