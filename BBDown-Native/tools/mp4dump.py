# -*- coding: utf-8 -*-
"""转储 MP4 的 moov 关键结构：trak/hdlr/elst/mdhd/hvcC/stsd。

只依赖标准库，用于诊断「某个播放器黑屏、其他播放器正常」这类兼容性问题。
最常见的两个真凶都在这里：
  1. elst 里有 media_time>0 的延迟编辑项 —— 严格按edit list 走的播放器
     会从「空洞」开始渲染，于是全黑，而容错强的播放器会继续往下解。
  2. hvcC/avcC 被改写或参数集缺失 —— 硬解播放器拿不到参数就全黑。
"""
import struct
import sys

CONT = {b'moov', b'trak', b'mdia', b'minf', b'stbl', b'edts', b'dinf', b'udta'}


def walk(d, off, end, depth, path, out):
    while off + 8 <= end:
        size = struct.unpack('>I', d[off:off + 4])[0]
        typ = d[off + 4:off + 8]
        hdr = 8
        if size == 1:
            if off + 16 > end:
                break
            size = struct.unpack('>Q', d[off + 8:off + 16])[0]
            hdr = 16
        elif size == 0:
            size = end - off
        if size < hdr or off + size > end:
            break
        out.append('%s%s size=%d @%d' % ('  ' * depth, typ.decode('latin1'), size, off))
        body = off + hdr
        if typ == b'hdlr':
            out.append('%s  handler=%s' % ('  ' * depth, d[body + 8:body + 12].decode('latin1')))
        elif typ == b'mdhd':
            ver = d[body]
            if ver == 1:
                ts, dur = struct.unpack('>IQ', d[body + 20:body + 32])
            else:
                ts, dur = struct.unpack('>II', d[body + 12:body + 20])
            out.append('%s  timescale=%d duration=%d (%.3fs)' % ('  ' * depth, ts, dur, dur / ts if ts else 0))
        elif typ == b'elst':
            ver = d[body]
            n = struct.unpack('>I', d[body + 4:body + 8])[0]
            p = body + 8
            out.append('%s  ELST 条目数=%d' % ('  ' * depth, n))
            for i in range(n):
                if ver == 1:
                    seg, mt = struct.unpack('>Qq', d[p:p + 16]); p += 16
                else:
                    seg, mt = struct.unpack('>Ii', d[p:p + 8]); p += 8
                rate = struct.unpack('>i', d[p:p + 4])[0]; p += 4
                out.append('%s    #%d segment_duration=%d media_time=%d rate=%d'
                           % ('  ' * depth, i, seg, mt, rate))
        elif typ == b'stts':
            n = struct.unpack('>I', d[body + 4:body + 8])[0]
            tot = 0
            cnt = 0
            for i in range(n):
                c, dl = struct.unpack('>II', d[body + 8 + i * 8:body + 16 + i * 8])
                tot += c
                cnt += 1
            out.append('%s  条目=%d 总样本数=%d' % ('  ' * depth, cnt, tot))
        elif typ in (b'hvcC', b'avcC', b'av1C', b'vpcC'):
            out.append('%s  %s 原始 %d 字节: %s' % ('  ' * depth, typ.decode('latin1'),
                                                      len(d[body:off + size]), d[body:off + size].hex()))
        elif typ in (b'hvc1', b'hev1', b'avc1', b'av01', b'mp4a', b'enca', b'enca'):
            out.append('%s  sample entry=%s  剩余 %d 字节' % ('  ' * depth, typ.decode('latin1'), size - hdr))
            j = d.find(b'hvcC', body, off + size)
            if j >= 0:
                k = d.find(b'hvcC', j - 4, j + 4)
                if k >= 0:
                    ksize = struct.unpack('>I', d[k - 4:k])[0] if k >= 4 else 0
                    out.append('%s  └─ hvcC 在样本描述内 offset=%d 盒长=%d'
                               % ('  ' * depth, k, ksize))
        if typ in CONT:
            walk(d, body, off + size, depth + 1, path + '/' + typ.decode('latin1'), out)
        off += size


def main():
    fn = sys.argv[1]
    d = open(fn, 'rb').read()
    print('文件: %s  大小=%d 字节' % (fn, len(d)))
    out = []
    walk(d, 0, len(d), 0, '', out)
    print('\n'.join(out))

    # 顶层 moov 里所有 trak 的快速摘要
    print('\n=== 顶层盒清单 ===')
    off = 0
    while off + 8 <= len(d):
        size = struct.unpack('>I', d[off:off + 4])[0]
        typ = d[off + 4:off + 8].decode('latin1')
        hdr = 8
        if size == 1:
            size = struct.unpack('>Q', d[off + 8:off + 16])[0]; hdr = 16
        elif size == 0:
            size = len(d) - off
        print('  %-6s size=%d' % (typ, size))
        off += size


if __name__ == '__main__':
    main()