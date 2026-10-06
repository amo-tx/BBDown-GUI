// Package mux 用纯 Go 把 B 站的 DASH 流（fMP4）合成一个普通的渐进式 MP4。
//
// 为什么自己写：这个项目的目标产物不能依赖 ffmpeg。而 B 站的 DASH 流本身
// 已经是合法的 fragmented MP4（init 段 + 一串 moof/mdat），所以只要
//
//	解析 init 段的 moov → 取出编码配置（stsd）等模板
//	扫描所有 moof 的 traf，还原出每条轨道的采样表
//	按渐进式 MP4 的要求重建 stbl（stts/ctts/stss/stsc/stsz/stco）+ mdat
//
// 就能得到一个不依赖任何外部程序的 MP4。
package mux

import (
	"encoding/binary"
	"fmt"
	"io"
)

// containerTypes 是「子盒容器」类型：盒体由一串子盒直接构成。
var containerTypes = map[string]bool{
	"moov": true, "trak": true, "mdia": true, "minf": true, "stbl": true,
	"mvex": true, "moof": true, "traf": true, "edts": true, "dinf": true,
	"udta": true, "mfra": true, "skip": true, "wave": true,
}

// fullBoxTypes 是带 (version, flags) 头的盒类型。
var fullBoxTypes = map[string]bool{
	"mvhd": true, "tkhd": true, "mdhd": true, "hdlr": true, "vmhd": true,
	"smhd": true, "dref": true, "elst": true, "stts": true, "stss": true,
	"ctts": true, "stsc": true, "stsz": true, "stco": true, "co64": true,
	"stsd": true, "tfhd": true, "tfdt": true, "trun": true, "trex": true,
	"mehd": true, "sidx": true, "mfhd": true, "nmhd": true, "sthd": true,
}

// Box 描述一个盒在文件中的位置与头信息。
type Box struct {
	Type    string // 4 字节类型
	Start   int64  // 含头起始偏移
	Size    int64  // 含头总长度
	Hdr     int64  // 头长度：8 或 16（64 位长度）
	Full    bool   // 是否是完整盒（带 version/flags）
	Version byte
	Flags   uint32
}

// BodyStart 返回盒体起始偏移（已跳过头部与 version/flags）。
func (b Box) BodyStart() int64 {
	if b.Full {
		return b.Start + b.Hdr + 4
	}
	return b.Start + b.Hdr
}

// End 返回盒结束偏移。
func (b Box) End() int64 { return b.Start + b.Size }

// BodyLen 返回盒体有效字节数（不含头与 version/flags）。
func (b Box) BodyLen() int64 { return b.End() - b.BodyStart() }

// readBox 读取 offset 处的一个盒头。end 是父级范围，用于边界校验。
func readBox(r io.ReaderAt, off, end int64) (Box, error) {
	if end-off < 8 {
		return Box{}, io.ErrUnexpectedEOF
	}
	var hdr [16]byte
	if _, err := r.ReadAt(hdr[:8], off); err != nil {
		return Box{}, err
	}
	size := int64(binary.BigEndian.Uint32(hdr[0:4]))
	b := Box{Type: string(hdr[4:8]), Start: off, Hdr: 8}

	switch size {
	case 1: // 64 位长度
		if end-off < 16 {
			return Box{}, io.ErrUnexpectedEOF
		}
		if _, err := r.ReadAt(hdr[:16], off); err != nil {
			return Box{}, err
		}
		size = int64(binary.BigEndian.Uint64(hdr[8:16]))
		b.Hdr = 16
	case 0: // 延伸到父级末尾
		size = end - off
	}
	if size < b.Hdr || off+size > end {
		return Box{}, fmt.Errorf("盒 %q 长度异常：size=%d off=%d end=%d", b.Type, size, off, end)
	}
	b.Size = size

	if fullBoxTypes[b.Type] {
		var vf [4]byte
		if _, err := r.ReadAt(vf[:], off+b.Hdr); err != nil {
			return Box{}, err
		}
		b.Full = true
		b.Version = vf[0]
		b.Flags = uint32(vf[1])<<16 | uint32(vf[2])<<8 | uint32(vf[3])
	}
	return b, nil
}

// eachBox 遍历 [start,end) 范围内的同级盒。
func eachBox(r io.ReaderAt, start, end int64, fn func(Box) error) error {
	off := start
	for {
		if end-off < 8 {
			return nil
		}
		b, err := readBox(r, off, end)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		off = b.End()
	}
}

// rawBox 原样读出整个盒（含头）的字节。
func rawBox(r io.ReaderAt, b Box) ([]byte, error) {
	out := make([]byte, b.Size)
	if _, err := r.ReadAt(out, b.Start); err != nil {
		return nil, err
	}
	return out, nil
}

// readAt 读一段字节。
func readAt(r io.ReaderAt, off, n int64) ([]byte, error) {
	out := make([]byte, n)
	if _, err := r.ReadAt(out, off); err != nil {
		return nil, err
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// 写侧原语

// cat 拼接若干字节切片。
func cat(parts ...[]byte) []byte {
	n := 0
	for _, p := range parts {
		n += len(p)
	}
	out := make([]byte, 0, n)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// u32 返回大端 32 位字节。
func u32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// u64 返回大端 64 位字节。
func u64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

// u16 返回大端 16 位字节。
func u16(v uint16) []byte {
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], v)
	return b[:]
}

// box 组装一个普通盒。
func box(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(8+len(payload)))
	copy(out[4:8], typ)
	copy(out[8:], payload)
	return out
}

// fullBox 组装一个完整盒（带 version + 24 位 flags）。
func fullBox(typ string, version byte, flags uint32, payload []byte) []byte {
	out := make([]byte, 12+len(payload))
	binary.BigEndian.PutUint32(out[0:4], uint32(12+len(payload)))
	copy(out[4:8], typ)
	out[8] = version
	out[9] = byte(flags >> 16)
	out[10] = byte(flags >> 8)
	out[11] = byte(flags)
	copy(out[12:], payload)
	return out
}

// patchU32 就地改写一个大端 32 位值。
func patchU32(b []byte, off int, v uint32) {
	binary.BigEndian.PutUint32(b[off:off+4], v)
}

// patchU64 就地改写一个大端 64 位值。
func patchU64(b []byte, off int, v uint64) {
	binary.BigEndian.PutUint64(b[off:off+8], v)
}

// findChild 在盒体范围内按类型找第一个子盒，找不到返回 ok=false。
func findChild(r io.ReaderAt, parent Box, typ string) (Box, bool, error) {
	var found Box
	ok := false
	err := eachBox(r, parent.BodyStart(), parent.End(), func(b Box) error {
		if !ok && b.Type == typ {
			found, ok = b, true
		}
		return nil
	})
	return found, ok, err
}
