package mux

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"strings"
)

// TrackInfo 是一条轨道在成品文件里的概况。
type TrackInfo struct {
	TrackID     uint32
	Handler     string // "vide" / "soun"
	Codec       string // stsd 里的 fourcc：hvc1 / avc1 / av01 / mp4a
	Timescale   uint32
	Duration    uint64 // 以 Timescale 为单位
	Samples     int    // stts 里的采样总数
	SyncSamples int    // stss 有几项；0 表示没写 stss（即全部同步）
	Chunks      int    // stco / co64 有几项

	// 视频
	Width  int
	Height int
	// 音频
	SampleRate int
	Channels   int
}

// Seconds 返回该轨道的时长（秒）。
func (t TrackInfo) Seconds() float64 {
	if t.Timescale == 0 {
		return 0
	}
	return float64(t.Duration) / float64(t.Timescale)
}

// IsVideo 判断是不是视频轨。
func (t TrackInfo) IsVideo() bool { return t.Handler == "vide" }

// FileInfo 是一次结构校验的结果。
type FileInfo struct {
	Path           string
	Size           int64
	MovieTimescale uint32
	Duration       uint64
	Tracks         []TrackInfo
	// Warnings 是「能读出来但不规范」的问题，不一定是错误。
	Warnings []string
}

// Inspect 解析一个**渐进式** MP4 的 moov，并校验各张表是否自洽。
//
// 与 OpenInput 的分工：OpenInput 面向下载下来的 fMP4（有 moof/mdat 结构），
// 这里面向我们自己封出来的成品（有 stbl 采样表、没有 moof）。
// 校验项包括：
//   - moov / trak / stbl 骨架齐全，stsd 有编码配置
//   - stts 的采样总数与 stsz 一致
//   - stsc 声明的 chunk 数与 stco/co64 一致
//   - 所有 chunk 偏移都落在 mdat 内（这条能直接抓出偏移算错）
func Inspect(path string) (*FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}

	fi := &FileInfo{Path: path, Size: st.Size()}

	var mdatRanges [][2]int64
	var moov Box
	var hasMoov bool
	if err := eachBox(f, 0, st.Size(), func(b Box) error {
		switch b.Type {
		case "moov":
			moov, hasMoov = b, true
		case "mdat":
			mdatRanges = append(mdatRanges, [2]int64{b.BodyStart(), b.End()})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if !hasMoov {
		return nil, fmt.Errorf("没有 moov：这不是一个可播放的 MP4")
	}
	if len(mdatRanges) == 0 {
		return nil, fmt.Errorf("没有 mdat：文件里没有媒体数据")
	}

	if mv, ok, err := findChild(f, moov, "mvhd"); err != nil {
		return nil, err
	} else if ok {
		var p [20]byte
		if _, err := f.ReadAt(p[:], mv.BodyStart()); err == nil {
			if mv.Version == 1 {
				fi.MovieTimescale = binary.BigEndian.Uint32(p[16:20])
			} else {
				fi.MovieTimescale = binary.BigEndian.Uint32(p[8:12])
			}
		}
	}

	if err := eachBox(f, moov.BodyStart(), moov.End(), func(b Box) error {
		if b.Type != "trak" {
			return nil
		}
		ti, err := inspectTrak(f, b)
		if err != nil {
			return err
		}
		fi.Tracks = append(fi.Tracks, *ti)
		return nil
	}); err != nil {
		return nil, err
	}
	if len(fi.Tracks) == 0 {
		return nil, fmt.Errorf("moov 里没有任何 trak")
	}

	// 交叉校验各张表
	for _, t := range fi.Tracks {
		label := fmt.Sprintf("%s 轨（track %d）", handlerName(t.Handler), t.TrackID)
		if t.Samples == 0 {
			return nil, fmt.Errorf("%s 的 stts 里没有采样", label)
		}
		if t.Chunks == 0 {
			return nil, fmt.Errorf("%s 的 stco/co64 里没有 chunk", label)
		}
		if t.Timescale == 0 {
			return nil, fmt.Errorf("%s 的 mdhd timescale 为 0", label)
		}
	}

	// chunk 偏移必须落在 mdat 里
	for _, t := range fi.Tracks {
		if err := checkOffsets(f, moov, &t, mdatRanges); err != nil {
			return nil, err
		}
	}
	maxDur := uint64(0)
	for _, t := range fi.Tracks {
		if d := scaleCeil(t.Duration, uint64(t.Timescale), 1000); d > maxDur {
			maxDur = d
		}
	}
	fi.Duration = maxDur
	return fi, nil
}

func inspectTrak(f io.ReaderAt, trak Box) (*TrackInfo, error) {
	ti := &TrackInfo{}
	err := eachBox(f, trak.BodyStart(), trak.End(), func(b Box) error {
		switch b.Type {
		case "tkhd":
			var p [4]byte
			off := int64(8)
			if b.Version == 1 {
				off = 16
			}
			if _, err := f.ReadAt(p[:], b.BodyStart()+off); err != nil {
				return err
			}
			ti.TrackID = binary.BigEndian.Uint32(p[:])
		case "mdia":
			return eachBox(f, b.BodyStart(), b.End(), func(c Box) error {
				switch c.Type {
				case "mdhd":
					var p [20]byte
					if _, err := f.ReadAt(p[:], c.BodyStart()); err != nil {
						return err
					}
					if c.Version == 1 {
						ti.Timescale = binary.BigEndian.Uint32(p[16:20])
						var d [8]byte
						if _, err := f.ReadAt(d[:], c.BodyStart()+20); err != nil {
							return err
						}
						ti.Duration = binary.BigEndian.Uint64(d[:])
					} else {
						ti.Timescale = binary.BigEndian.Uint32(p[8:12])
						ti.Duration = uint64(binary.BigEndian.Uint32(p[12:16]))
					}
				case "hdlr":
					var p [12]byte
					if _, err := f.ReadAt(p[:], c.BodyStart()); err != nil {
						return err
					}
					ti.Handler = string(p[4:8])
				case "minf":
					return eachBox(f, c.BodyStart(), c.End(), func(d Box) error {
						if d.Type != "stbl" {
							return nil
						}
						return inspectStbl(f, d, ti)
					})
				}
				return nil
			})
		}
		return nil
	})
	return ti, err
}

func inspectStbl(f io.ReaderAt, stbl Box, ti *TrackInfo) error {
	return eachBox(f, stbl.BodyStart(), stbl.End(), func(b Box) error {
		switch b.Type {
		case "stsd":
			// 结构：version/flags(4) entry_count(4) 然后是一串采样描述盒。
			// 注意 BodyStart() 已经跳过 version/flags，所以它正指向 entry_count。
			var hdr [4]byte
			if _, err := f.ReadAt(hdr[:], b.BodyStart()); err != nil {
				return err
			}
			count := binary.BigEndian.Uint32(hdr[:])
			if count == 0 {
				return fmt.Errorf("stsd 里没有采样描述")
			}
			entryStart := b.BodyStart() + 4
			var eh [36]byte
			if _, err := f.ReadAt(eh[:], entryStart); err != nil {
				return err
			}
			ti.Codec = string(eh[4:8])
			// SampleEntry 固定头 8 字节（6 reserved + 2 data_reference_index）之后：
			//   视频：pre_defined(2) reserved(2) pre_defined[3](12) → 宽高在 32 / 34
			//   音频：reserved[2](8) channelcount(2) samplesize(2)
			//         pre_defined(2) reserved(2) → 声道在 24、采样率在 32
			if isAudioCodec(ti.Codec) {
				ti.Channels = int(binary.BigEndian.Uint16(eh[24:26]))
				ti.SampleRate = int(binary.BigEndian.Uint32(eh[32:36]) >> 16)
			} else {
				ti.Width = int(binary.BigEndian.Uint16(eh[32:34]))
				ti.Height = int(binary.BigEndian.Uint16(eh[34:36]))
			}
		case "stts":
			var hdr [4]byte
			if _, err := f.ReadAt(hdr[:], b.BodyStart()); err != nil {
				return err
			}
			runs := binary.BigEndian.Uint32(hdr[:])
			if runs == 0 {
				return nil
			}
			// 逐条累加 count，得到采样总数
			buf, err := readAt(f, b.BodyStart()+4, int64(runs)*8)
			if err != nil {
				return err
			}
			total := 0
			for i := uint32(0); i < runs; i++ {
				total += int(binary.BigEndian.Uint32(buf[i*8 : i*8+4]))
			}
			ti.Samples = total
		case "stss":
			var hdr [4]byte
			if _, err := f.ReadAt(hdr[:], b.BodyStart()); err != nil {
				return err
			}
			ti.SyncSamples = int(binary.BigEndian.Uint32(hdr[:]))
		case "stco", "co64":
			var hdr [4]byte
			if _, err := f.ReadAt(hdr[:], b.BodyStart()); err != nil {
				return err
			}
			ti.Chunks = int(binary.BigEndian.Uint32(hdr[:]))
		}
		return nil
	})
}

// checkOffsets 确认每个 chunk 的数据都落在某个 mdat 区间内。
//
// 这条检查很有价值：偏移一旦算错（比如 le 存储的盒没用大端、或 base 回推偏了），
// 播放器会静默黑屏，但解码器本身不报错。这里能直接抓到。
func checkOffsets(f io.ReaderAt, moov Box, ti *TrackInfo, mdats [][2]int64) error {
	var stbl Box
	if err := eachBox(f, moov.BodyStart(), moov.End(), func(b Box) error {
		if b.Type != "trak" {
			return nil
		}
		var tid uint32
		if tb, ok, _ := findChild(f, b, "tkhd"); ok {
			var p [4]byte
			off := int64(8)
			if tb.Version == 1 {
				off = 16
			}
			if _, err := f.ReadAt(p[:], tb.BodyStart()+off); err == nil {
				tid = binary.BigEndian.Uint32(p[:])
			}
		}
		if tid != ti.TrackID {
			return nil
		}
		return eachBox(f, b.BodyStart(), b.End(), func(c Box) error {
			if c.Type != "mdia" {
				return nil
			}
			return eachBox(f, c.BodyStart(), c.End(), func(d Box) error {
				if d.Type != "minf" {
					return nil
				}
				return eachBox(f, d.BodyStart(), d.End(), func(e Box) error {
					if e.Type == "stbl" {
						stbl = e
					}
					return nil
				})
			})
		})
	}); err != nil {
		return err
	}
	if stbl.Type == "" {
		return nil
	}

	var offsets []int64
	if err := eachBox(f, stbl.BodyStart(), stbl.End(), func(b Box) error {
		switch b.Type {
		case "stco":
			// BodyStart() 已跳过 version/flags，正指向 entry_count
			var hdr [4]byte
			if _, err := f.ReadAt(hdr[:], b.BodyStart()); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(hdr[:])
			buf, err := readAt(f, b.BodyStart()+4, int64(n)*4)
			if err != nil {
				return err
			}
			for i := uint32(0); i < n; i++ {
				offsets = append(offsets, int64(binary.BigEndian.Uint32(buf[i*4:i*4+4])))
			}
		case "co64":
			var hdr [4]byte
			if _, err := f.ReadAt(hdr[:], b.BodyStart()); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(hdr[:])
			buf, err := readAt(f, b.BodyStart()+4, int64(n)*8)
			if err != nil {
				return err
			}
			for i := uint32(0); i < n; i++ {
				offsets = append(offsets, int64(binary.BigEndian.Uint64(buf[i*8:i*8+8])))
			}
		}
		return nil
	}); err != nil {
		return err
	}

	// chunk 数必须与 stco/co64 一致，否则说明表被写坏了
	if len(offsets) != ti.Chunks {
		return fmt.Errorf("%s 轨的 stco/co64 有 %d 项，但统计到 %d 个偏移",
			handlerName(ti.Handler), ti.Chunks, len(offsets))
	}

	for i, off := range offsets {
		ok := false
		for _, r := range mdats {
			if off >= r[0] && off < r[1] {
				ok = true
				break
			}
		}
		if !ok {
			return fmt.Errorf("%s 轨的第 %d 个 chunk 偏移 %d 不在任何 mdat 区间内（mdat=%v）",
				handlerName(ti.Handler), i+1, off, mdats)
		}
	}
	return nil
}

func handlerName(h string) string {
	switch h {
	case "vide":
		return "视频"
	case "soun":
		return "音频"
	case "":
		return "未知"
	}
	return h
}

// isAudioCodec 判断 stsd 的 fourcc 是不是音频编码。
func isAudioCodec(codec string) bool {
	switch {
	case strings.HasPrefix(codec, "mp4a"),
		strings.HasPrefix(codec, "ac-3"),
		strings.HasPrefix(codec, "ec-3"),
		strings.HasPrefix(codec, "opus"),
		strings.HasPrefix(codec, "alac"),
		strings.HasPrefix(codec, "fLaC"):
		return true
	}
	return false
}
