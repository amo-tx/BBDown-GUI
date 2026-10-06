#!/usr/bin/env python3
"""一次性调试脚本：分析 fMP4 里封装器最需要搞清楚的几个字段。

关心的东西：
  - elst（edit list）：决定起始时间要不要补偿 CTS 偏移
  - tfhd 的默认值与 flags 拆解
  - trun 的 first_sample_flags / data_offset / 前几个采样
  - 同步帧标志（决定 stss 怎么写）
"""
import struct
import sys


def read_boxes(buf, start, end):
    off = start
    while off + 8 <= end:
        size = struct.unpack(">I", buf[off:off + 4])[0]
        btype = buf[off + 4:off + 8]
        hdr = 8
        if size == 1:
            size = struct.unpack(">Q", buf[off + 8:off + 16])[0]
            hdr = 16
        elif size == 0:
            size = end - off
        if size < hdr or off + size > end:
            return
        yield btype, off, size, hdr
        off += size


def find(buf, path, start=0, end=None):
    """按路径找盒子，例如 [b'moov', b'trak', b'mdia']。"""
    if end is None:
        end = len(buf)
    cur = [(start, end)]
    for want in path:
        nxt = []
        for s, e in cur:
            for btype, off, size, hdr in read_boxes(buf, s, e):
                if btype == want or (want == b"*" and True):
                    nxt.append((off + hdr, off + size))
        cur = nxt
    return cur


TFHD_FLAGS = {
    0x000001: "base-data-offset-present",
    0x000002: "sample-description-index-present",
    0x000008: "default-sample-duration-present",
    0x000010: "default-sample-size-present",
    0x000020: "default-sample-flags-present",
    0x010000: "duration-is-empty",
    0x020000: "default-base-is-moof",
}
TRUN_FLAGS = {
    0x000001: "data-offset-present",
    0x000004: "first-sample-flags-present",
    0x000100: "sample-duration-present",
    0x000200: "sample-size-present",
    0x000400: "sample-flags-present",
    0x000800: "sample-composition-time-offsets-present",
}
SAMPLE_FLAG_BITS = {
    0x00010000: "sample_is_non_sync_sample",
    0x00020000: "sample_depends_on",
    0x00040000: "sample_is_depended_on",
    0x00080000: "sample_has_redundancy",
    0x00C00000: "sample_padding_value",
    0x01000000: "sample_is_non_sync(2)",
}


def flags_str(val, table):
    names = [n for bit, n in table.items() if val & bit]
    return f"0x{val:x} [{', '.join(names) if names else '—'}]"


def main(path):
    with open(path, "rb") as f:
        buf = f.read()
    print(f"### {path}  ({len(buf)} 字节)")

    # --- elst ---
    for s, e in find(buf, [b"moov", b"trak", b"edts", b"elst"]):
        ver = buf[s]
        cnt = struct.unpack(">I", buf[s + 4:s + 8])[0]
        print(f"  elst v{ver} 条目数={cnt}")
        p = s + 8
        for i in range(cnt):
            if ver == 1:
                dur, mt = struct.unpack(">QQ", buf[p:p + 16])
                rate = struct.unpack(">hh", buf[p + 16:p + 20])
                p += 20
            else:
                dur, mt = struct.unpack(">II", buf[p:p + 8])
                rate = struct.unpack(">hh", buf[p + 8:p + 12])
                p += 12
            print(f"     [{i}] segment_duration={dur} media_time={mt} rate={rate}")

    # --- 第一个 moof ---
    moofs = list(read_boxes(buf, 0, len(buf)))
    first_moof = next((o for t, o, s, h in moofs if t == b"moof"), None)
    if first_moof is None:
        print("  没有 moof")
        return

    for s, e in find(buf, [b"moof", b"traf"], first_moof, first_moof + 4096):
        for btype, off, size, hdr in read_boxes(buf, s, e):
            if btype == b"tfhd":
                ver = buf[off + hdr]
                fl = int.from_bytes(buf[off + hdr + 1:off + hdr + 4], "big")
                p = off + hdr + 4
                tid = struct.unpack(">I", buf[p:p + 4])[0]
                p += 4
                extra = []
                if fl & 0x000001:
                    bdo = struct.unpack(">Q", buf[p:p + 8])[0]
                    p += 8
                    extra.append(f"baseDataOffset={bdo}")
                if fl & 0x000002:
                    sdi = struct.unpack(">I", buf[p:p + 4])[0]
                    p += 4
                    extra.append(f"sampleDescIndex={sdi}")
                if fl & 0x000008:
                    v = struct.unpack(">I", buf[p:p + 4])[0]
                    p += 4
                    extra.append(f"defaultSampleDuration={v}")
                if fl & 0x000010:
                    v = struct.unpack(">I", buf[p:p + 4])[0]
                    p += 4
                    extra.append(f"defaultSampleSize={v}")
                if fl & 0x000020:
                    v = struct.unpack(">I", buf[p:p + 4])[0]
                    p += 4
                    extra.append(f"defaultSampleFlags={flags_str(v, SAMPLE_FLAG_BITS)}")
                print(f"  tfhd v{ver} {flags_str(fl, TFHD_FLAGS)} trackID={tid} {' '.join(extra)}")

            elif btype == b"trun":
                ver = buf[off + hdr]
                fl = int.from_bytes(buf[off + hdr + 1:off + hdr + 4], "big")
                cnt = struct.unpack(">I", buf[off + hdr + 4:off + hdr + 8])[0]
                p = off + hdr + 8
                extra = []
                data_off = None
                if fl & 0x000001:
                    data_off = struct.unpack(">i", buf[p:p + 4])[0]
                    p += 4
                first_flags = None
                if fl & 0x000004:
                    first_flags = struct.unpack(">I", buf[p:p + 4])[0]
                    p += 4
                    extra.append(f"firstSampleFlags={flags_str(first_flags, SAMPLE_FLAG_BITS)}")
                print(f"  trun v{ver} {flags_str(fl, TRUN_FLAGS)} sampleCount={cnt} dataOffset={data_off} {' '.join(extra)}")
                for i in range(min(4, cnt)):
                    d = s_ = cts = None
                    if fl & 0x000100:
                        d = struct.unpack(">I", buf[p:p + 4])[0]; p += 4
                    if fl & 0x000200:
                        s_ = struct.unpack(">I", buf[p:p + 4])[0]; p += 4
                    if fl & 0x000400:
                        f2 = struct.unpack(">I", buf[p:p + 4])[0]; p += 4
                    if fl & 0x000800:
                        if ver == 1:
                            cts = struct.unpack(">i", buf[p:p + 4])[0]
                        else:
                            cts = struct.unpack(">I", buf[p:p + 4])[0]
                        p += 4
                    print(f"      sample[{i}] dur={d} size={s_} cts={cts}")
                # 统计同步帧
                if fl & 0x000800 or fl & 0x000200:
                    pp = p
                    sizes = []
                    syncs = 0
                    for i in range(cnt):
                        if fl & 0x000100:
                            pp += 4
                        if fl & 0x000200:
                            sizes.append(struct.unpack(">I", buf[pp:pp + 4])[0]); pp += 4
                        if fl & 0x000400:
                            pp += 4
                        if fl & 0x000800:
                            pp += 4
                    print(f"      该 moof 采样数={cnt} 总字节={sum(sizes)}")


if __name__ == "__main__":
    for p in sys.argv[1:]:
        main(p)
        print()
