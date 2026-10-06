package mux

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// Sample 是一个采样（视频帧或音频帧）的元数据。
type Sample struct {
	Size     uint32
	Duration uint32
	CTS      int32 // 合成时间偏移（相对解码时间），负责 B 帧的显示顺序
	Sync     bool  // 是否是同步采样（关键帧）
}

// Run 是一段连续存放的采样，对应源文件里的一个 moof/traf/trun。
//
// 同一 run 内的采样在源文件中是首尾相连的，复制到输出后依然相邻，
// 因此正好可以当作 MP4 的一个 chunk。
type Run struct {
	Time    uint64 // 该 run 的起始解码时间（以 Template.Timescale 为单位）
	Offset  int64  // 源文件中首个采样数据的绝对偏移
	Bytes   int64  // 该 run 全部采样的字节数
	Samples []Sample
}

// TrackTemplate 是从 init 段 moov 里取出、需要原样复用的盒字节。
type TrackTemplate struct {
	TrackID   uint32
	Timescale uint32
	Handler   string // "vide" / "soun"
	Tkhd      []byte // 原样（稍后改 track_ID 与时长）
	Edts      []byte // 原样（含 elst，可能为 nil）
	Mdhd      []byte // 原样（稍后改时长）
	Hdlr      []byte // 原样
	MinfExtra []byte // minf 里除 stbl 之外的子盒（vmhd/smhd + dinf）
	Stsd      []byte // 原样（编码配置 avcC/hvcC/mp4a 都在里面）
}

// Input 是一个已解析的 DASH 流。
type Input struct {
	Path           string
	Ftyp           []byte
	Template       TrackTemplate
	Runs           []Run
	Duration       uint64 // 总时长，以 Template.Timescale 为单位
	MovieTimescale uint32 // 源文件 mvhd 的时间刻度
	SampleCount    int
}

// trexDefaults 是 mvex/trex 里的默认采样参数。
type trexDefaults struct {
	Duration uint32
	Size     uint32
	Flags    uint32
	Set      bool
}

// OpenInput 解析一个 fMP4 文件，提取模板与采样表。
//
// 全程只通过 io.ReaderAt 读头部，不把整个文件载入内存——2 小时的 4K 视频
// 可能有几个 GB。
func OpenInput(path string) (*Input, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	total := st.Size()

	in := &Input{Path: path, MovieTimescale: 1000}

	var moov Box
	var moovOK bool
	if err := eachBox(f, 0, total, func(b Box) error {
		switch b.Type {
		case "ftyp":
			raw, err := rawBox(f, b)
			if err != nil {
				return err
			}
			in.Ftyp = raw
		case "moov":
			moov, moovOK = b, true
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if !moovOK {
		return nil, fmt.Errorf("文件里没有 moov，可能不是有效的 DASH 流")
	}

	trex, err := in.parseMoov(f, moov)
	if err != nil {
		return nil, err
	}
	if in.Template.Stsd == nil {
		return nil, fmt.Errorf("moov 里没有找到 stsd（编码配置缺失）")
	}
	if in.Template.Timescale == 0 {
		return nil, fmt.Errorf("mdhd 的 timescale 为 0")
	}

	if err := in.parseFragments(f, total, trex); err != nil {
		return nil, err
	}
	if len(in.Runs) == 0 {
		return nil, fmt.Errorf("没有解析到任何媒体分片")
	}
	for _, r := range in.Runs {
		in.SampleCount += len(r.Samples)
		for _, s := range r.Samples {
			in.Duration += uint64(s.Duration)
		}
	}
	return in, nil
}

// ---------------------------------------------------------------------------
// moov

func (in *Input) parseMoov(f io.ReaderAt, moov Box) (trexDefaults, error) {
	var td trexDefaults
	return td, eachBox(f, moov.BodyStart(), moov.End(), func(b Box) error {
		switch b.Type {
		case "mvhd":
			var p [20]byte
			if _, err := f.ReadAt(p[:], b.BodyStart()); err != nil {
				return err
			}
			if b.Version == 1 {
				in.MovieTimescale = binary.BigEndian.Uint32(p[16:20])
			} else {
				in.MovieTimescale = binary.BigEndian.Uint32(p[8:12])
			}
		case "trak":
			return in.parseTrak(f, b)
		case "mvex":
			return eachBox(f, b.BodyStart(), b.End(), func(t Box) error {
				if t.Type != "trex" {
					return nil
				}
				var p [20]byte
				if _, err := f.ReadAt(p[:], t.BodyStart()); err != nil {
					return err
				}
				// trex 体：track_ID, sample_desc_index, default_duration, default_size, default_flags
				td.Duration = binary.BigEndian.Uint32(p[8:12])
				td.Size = binary.BigEndian.Uint32(p[12:16])
				td.Flags = binary.BigEndian.Uint32(p[16:20])
				td.Set = true
				return nil
			})
		}
		return nil
	})
}

func (in *Input) parseTrak(f io.ReaderAt, trak Box) error {
	return eachBox(f, trak.BodyStart(), trak.End(), func(b Box) error {
		switch b.Type {
		case "tkhd":
			raw, err := rawBox(f, b)
			if err != nil {
				return err
			}
			in.Template.Tkhd = raw
			// tkhd 体：creation, modification, track_ID —— v0 时 track_ID 在第 8 字节，v1 在第 16 字节
			off := int64(8)
			if b.Version == 1 {
				off = 16
			}
			var p [4]byte
			if _, err := f.ReadAt(p[:], b.BodyStart()+off); err != nil {
				return err
			}
			in.Template.TrackID = binary.BigEndian.Uint32(p[:])
		case "edts":
			raw, err := rawBox(f, b)
			if err != nil {
				return err
			}
			in.Template.Edts = raw
		case "mdia":
			return in.parseMdia(f, b)
		}
		return nil
	})
}

func (in *Input) parseMdia(f io.ReaderAt, mdia Box) error {
	return eachBox(f, mdia.BodyStart(), mdia.End(), func(b Box) error {
		switch b.Type {
		case "mdhd":
			raw, err := rawBox(f, b)
			if err != nil {
				return err
			}
			in.Template.Mdhd = raw
			var p [20]byte
			if _, err := f.ReadAt(p[:], b.BodyStart()); err != nil {
				return err
			}
			// mdhd 体：creation, modification, timescale
			if b.Version == 1 {
				in.Template.Timescale = binary.BigEndian.Uint32(p[16:20])
			} else {
				in.Template.Timescale = binary.BigEndian.Uint32(p[8:12])
			}
		case "hdlr":
			raw, err := rawBox(f, b)
			if err != nil {
				return err
			}
			in.Template.Hdlr = raw
			var p [12]byte
			if _, err := f.ReadAt(p[:], b.BodyStart()); err != nil {
				return err
			}
			// hdlr 体：pre_defined, handler_type
			in.Template.Handler = string(p[4:8])
		case "minf":
			return in.parseMinf(f, b)
		}
		return nil
	})
}

func (in *Input) parseMinf(f io.ReaderAt, minf Box) error {
	var extra [][]byte
	err := eachBox(f, minf.BodyStart(), minf.End(), func(b Box) error {
		if b.Type == "stbl" {
			return eachBox(f, b.BodyStart(), b.End(), func(c Box) error {
				if c.Type != "stsd" {
					return nil
				}
				raw, err := rawBox(f, c)
				if err != nil {
					return err
				}
				in.Template.Stsd = raw
				return nil
			})
		}
		raw, err := rawBox(f, b)
		if err != nil {
			return err
		}
		extra = append(extra, raw)
		return nil
	})
	if err != nil {
		return err
	}
	in.Template.MinfExtra = cat(extra...)
	return nil
}

// ---------------------------------------------------------------------------
// moof / traf / trun

// tfhd/trun 的标志位。
const (
	tfhdBaseDataOffset    = 0x000001
	tfhdSampleDescIndex   = 0x000002
	tfhdDefaultDuration   = 0x000008
	tfhdDefaultSize       = 0x000010
	tfhdDefaultFlags      = 0x000020
	tfhdDefaultBaseIsMoof = 0x020000

	trunDataOffset     = 0x000001
	trunFirstFlags     = 0x000004
	trunSampleDuration = 0x000100
	trunSampleSize     = 0x000200
	trunSampleFlags    = 0x000400
	trunSampleCTS      = 0x000800

	// sample_is_non_sync_sample：这一位为 0 表示是关键帧。
	sampleNonSync = 0x00010000
)

func (in *Input) parseFragments(f io.ReaderAt, total int64, td trexDefaults) error {
	return eachBox(f, 0, total, func(b Box) error {
		if b.Type != "moof" {
			return nil
		}
		// default-base-is-moof 时，基准就是 moof 的起始位置。
		return eachBox(f, b.BodyStart(), b.End(), func(t Box) error {
			if t.Type != "traf" {
				return nil
			}
			return in.parseTraf(f, t, b.Start, td)
		})
	})
}

func (in *Input) parseTraf(f io.ReaderAt, traf Box, moofStart int64, td trexDefaults) error {
	var (
		defDur, defSize, defFlags    uint32
		haveDur, haveSize, haveFlags bool
		baseTime                     uint64
		baseOffset                   = moofStart
		truns                        []Box
	)

	err := eachBox(f, traf.BodyStart(), traf.End(), func(b Box) error {
		switch b.Type {
		case "tfhd":
			fl := b.Flags
			off := b.BodyStart() + 4 // 跳过 track_ID
			if fl&tfhdBaseDataOffset != 0 {
				// base_data_offset 一旦出现就直接用；default-base-is-moof 只在它缺席时兜底。
				var p [8]byte
				if _, err := f.ReadAt(p[:], off); err != nil {
					return err
				}
				baseOffset = int64(binary.BigEndian.Uint64(p[:]))
				off += 8
			}
			if fl&tfhdSampleDescIndex != 0 {
				off += 4
			}
			if fl&tfhdDefaultDuration != 0 {
				var p [4]byte
				if _, err := f.ReadAt(p[:], off); err != nil {
					return err
				}
				defDur, haveDur = binary.BigEndian.Uint32(p[:]), true
				off += 4
			}
			if fl&tfhdDefaultSize != 0 {
				var p [4]byte
				if _, err := f.ReadAt(p[:], off); err != nil {
					return err
				}
				defSize, haveSize = binary.BigEndian.Uint32(p[:]), true
				off += 4
			}
			if fl&tfhdDefaultFlags != 0 {
				var p [4]byte
				if _, err := f.ReadAt(p[:], off); err != nil {
					return err
				}
				defFlags, haveFlags = binary.BigEndian.Uint32(p[:]), true
				off += 4
			}
		case "tfdt":
			var p [8]byte
			if _, err := f.ReadAt(p[:], b.BodyStart()); err != nil {
				return err
			}
			if b.Version == 1 {
				baseTime = binary.BigEndian.Uint64(p[:])
			} else {
				baseTime = uint64(binary.BigEndian.Uint32(p[:4]))
			}
		case "trun":
			truns = append(truns, b)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// tfhd 没给的默认值，回退到 trex。
	if !haveDur && td.Set {
		defDur, haveDur = td.Duration, true
	}
	if !haveSize && td.Set {
		defSize, haveSize = td.Size, true
	}
	if !haveFlags && td.Set {
		defFlags, haveFlags = td.Flags, true
	}

	dataPos := int64(-1)
	for _, tb := range truns {
		fl := tb.Flags
		off := tb.BodyStart()

		// trun 体的字段顺序（ISO/IEC 14496-12 §8.8.8）：
		//   sample_count（恒在）→ data_offset ? → first_sample_flags ? → 逐采样字段
		// sample_count 必须最先读，否则后面每个字段都会错位 4 字节。
		var p [4]byte
		if _, err := f.ReadAt(p[:], off); err != nil {
			return err
		}
		count := binary.BigEndian.Uint32(p[:])
		off += 4
		if count == 0 {
			continue
		}

		if fl&trunDataOffset != 0 {
			if _, err := f.ReadAt(p[:], off); err != nil {
				return err
			}
			dataPos = baseOffset + int64(int32(binary.BigEndian.Uint32(p[:])))
			off += 4
		} else if dataPos < 0 {
			return fmt.Errorf("trun 既没有 data_offset 也没有可继承的位置")
		}

		var firstFlags uint32
		if fl&trunFirstFlags != 0 {
			if _, err := f.ReadAt(p[:], off); err != nil {
				return err
			}
			firstFlags = binary.BigEndian.Uint32(p[:])
			off += 4
		}

		// 一次性把这一整个 trun 的采样字段读进来，避免逐字段多次 ReadAt。
		perSample := 0
		for _, bit := range []uint32{trunSampleDuration, trunSampleSize, trunSampleFlags, trunSampleCTS} {
			if fl&bit != 0 {
				perSample += 4
			}
		}
		var buf []byte
		if perSample > 0 {
			buf, err = readAt(f, off, int64(perSample)*int64(count))
			if err != nil {
				return err
			}
		}

		samples := make([]Sample, 0, count)
		cur := 0
		get32 := func() uint32 {
			v := binary.BigEndian.Uint32(buf[cur : cur+4])
			cur += 4
			return v
		}

		for i := uint32(0); i < count; i++ {
			s := Sample{Duration: defDur, Size: defSize}
			flags := defFlags

			if fl&trunSampleDuration != 0 {
				s.Duration = get32()
			}
			if fl&trunSampleSize != 0 {
				s.Size = get32()
			}
			if fl&trunSampleFlags != 0 {
				flags = get32()
			}
			if fl&trunSampleCTS != 0 {
				// v0 是 unsigned、v1 是 signed，但 CTS 偏移量都很小，
				// 直接按 int32 解释对两种版本都成立。
				s.CTS = int32(get32())
			}
			// first_sample_flags 只作用于本 trun 的第一个采样
			if i == 0 && fl&trunFirstFlags != 0 {
				flags = firstFlags
			}
			s.Sync = flags&sampleNonSync == 0
			samples = append(samples, s)
		}

		var bytes int64
		for _, s := range samples {
			bytes += int64(s.Size)
		}
		in.Runs = append(in.Runs, Run{Time: baseTime, Offset: dataPos, Bytes: bytes, Samples: samples})
		dataPos += bytes
	}
	return nil
}
