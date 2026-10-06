#!/usr/bin/env python3
"""一次性调试脚本：打印 MP4 / fMP4 的盒（box）结构。

只用于开发期分析真实 m4s 的结构，不是交付物。
"""
import struct
import sys

CONTAINERS = {
    b"moov", b"trak", b"mdia", b"minf", b"stbl", b"mvex", b"moof",
    b"traf", b"edts", b"dinf", b"udta", b"mfra", b"skip", b"wave",
}
FULLBOXES = {
    b"mvhd", b"tkhd", b"mdhd", b"hdlr", b"vmhd", b"smhd", b"dref", b"elst",
    b"stts", b"stss", b"ctts", b"stsc", b"stsz", b"stco", b"co64", b"stsd",
    b"tfhd", b"tfdt", b"trun", b"trex", b"mehd", b"sidx", b"mfhd", b"smhd",
    b"nmhd", b"sthd", b"subs", b"saiz", b"saio", b"senc", b"prft", b"emsg",
}


def boxes(buf, start, end, depth, out):
    off = start
    while off + 8 <= end:
        size = struct.unpack(">I", buf[off:off + 4])[0]
        btype = buf[off + 4:off + 8]
        hdr = 8
        if size == 1:
            if off + 16 > end:
                break
            size = struct.unpack(">Q", buf[off + 8:off + 16])[0]
            hdr = 16
        elif size == 0:
            size = end - off
        if size < hdr or off + size > end:
            out.append(f"{'  ' * depth}[!] {btype!r} 尺寸异常 size={size} off={off}")
            return
        if btype == b"uuid":
            hdr += 16

        extra = ""
        if btype in FULLBOXES:
            ver = buf[off + hdr]
            flags = int.from_bytes(buf[off + hdr + 1:off + hdr + 4], "big")
            extra = f" v={ver} flags=0x{flags:x}"

        if btype == b"stsd":
            n = struct.unpack(">I", buf[off + hdr + 4:off + hdr + 8])[0]
            extra += f" entries={n}"
            fmt = buf[off + hdr + 8 + 4:off + hdr + 8 + 8]
            extra += f" fmt={fmt.decode('latin1')}"
        elif btype == b"stsz":
            ss, cnt = struct.unpack(">II", buf[off + hdr + 4:off + hdr + 12])
            extra += f" sampleSize={ss} count={cnt}"
        elif btype == b"stco":
            cnt = struct.unpack(">I", buf[off + hdr + 4:off + hdr + 8])[0]
            extra += f" count={cnt}"
        elif btype == b"co64":
            cnt = struct.unpack(">I", buf[off + hdr + 4:off + hdr + 8])[0]
            extra += f" count={cnt}"
        elif btype == b"stts":
            cnt = struct.unpack(">I", buf[off + hdr + 4:off + hdr + 8])[0]
            extra += f" count={cnt}"
        elif btype == b"mdhd":
            if buf[off + hdr] == 1:
                ts, dur = struct.unpack(">IQ", buf[off + hdr + 20:off + hdr + 32])
            else:
                ts, dur = struct.unpack(">II", buf[off + hdr + 12:off + hdr + 20])
            extra += f" timescale={ts} duration={dur}"
        elif btype == b"hdlr":
            handler = buf[off + hdr + 8:off + hdr + 12]
            extra += f" handler={handler.decode('latin1')!r}"
        elif btype == b"tkhd":
            tid = struct.unpack(">I", buf[off + hdr + (20 if buf[off + hdr] == 1 else 12):][:4])[0]
            extra += f" trackID={tid}"
        elif btype == b"trex":
            tid, dsdi, dsdur, dssz, dsf = struct.unpack(">IIIII", buf[off + hdr + 4:off + hdr + 24])
            extra += f" trackID={tid} defaultDur={dsdur} defaultSize={dssz} defaultFlags=0x{dsf:x}"
        elif btype == b"tfhd":
            tid = struct.unpack(">I", buf[off + hdr + 4:off + hdr + 8])[0]
            extra += f" trackID={tid}"
        elif btype == b"tfdt":
            if buf[off + hdr] == 1:
                extra += f" baseTime={struct.unpack('>Q', buf[off + hdr + 4:off + hdr + 12])[0]}"
            else:
                extra += f" baseTime={struct.unpack('>I', buf[off + hdr + 4:off + hdr + 8])[0]}"
        elif btype == b"trun":
            cnt = struct.unpack(">I", buf[off + hdr + 4:off + hdr + 8])[0]
            extra += f" sampleCount={cnt}"
        elif btype == b"mvhd":
            if buf[off + hdr] == 1:
                ts, dur = struct.unpack(">IQ", buf[off + hdr + 20:off + hdr + 32])
            else:
                ts, dur = struct.unpack(">II", buf[off + hdr + 12:off + hdr + 20])
            extra += f" timescale={ts} duration={dur}"

        out.append(f"{'  ' * depth}{btype.decode('latin1')} size={size} off={off}{extra}")

        low = btype.lower()
        if low in CONTAINERS:
            boxes(buf, off + hdr, off + size, depth + 1, out)
        elif btype == b"stsd":
            # 采样描述是「entry count + 一系列 entry box」
            boxes(buf, off + hdr + 8, off + size, depth + 1, out)
        off += size


def main(path):
    with open(path, "rb") as f:
        buf = f.read()
    out = []
    boxes(buf, 0, len(buf), 0, out)
    print(f"### {path}  共 {len(buf)} 字节")
    print("\n".join(out))


if __name__ == "__main__":
    for p in sys.argv[1:]:
        main(p)
        print()
