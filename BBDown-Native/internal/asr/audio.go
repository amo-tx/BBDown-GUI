// Package asr 提供本地语音转文字（ASR）托底：B 站没有字幕时，
// 把视频音频流抽出来、纯 Go 解码成 whisper 需要的 16kHz 单声道
// WAV，再交给本地 whisper.cpp（自动下载）转写。
//
// 本文件负责「音频格式」这一半。设计约束与项目一致：
//   - 不引 cgo、不调外部程序完成解码（whisper.exe 除外）；
//   - 解码用 github.com/tphakala/go-aac（LGPL，FFmpeg 定点解码器移植），
//     它接受「裸 AAC access unit + AudioSpecificConfig」，正好对上
//     fMP4 里样本表的形态，不用自己拼 ADTS 头。
package asr

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	"github.com/tphakala/go-aac/pcm"

	"bbdown-native/internal/mux"
)

// M4SToWAV 把 B 站音频流（fMP4/m4s，AAC-LC）解码成 16kHz 单声道
// PCM16 WAV。progress（可为 nil）每解码完一帧回调一次，参数是
// 0~1 的完成度。
// 返回音频总秒数。
func M4SToWAV(m4sPath, wavPath string, progress func(frac float64)) (int, error) {
	in, err := mux.OpenInput(m4sPath)
	if err != nil {
		return 0, fmt.Errorf("读取音频流失败: %w", err)
	}
	asc, err := extractASC(in.Template.Stsd)
	if err != nil {
		return 0, fmt.Errorf("解析 AAC 编码参数失败: %w", err)
	}

	m4sFile, err := os.Open(m4sPath)
	if err != nil {
		return 0, err
	}
	defer m4sFile.Close()

	dec, err := pcm.NewDecoder(newAUReader(m4sFile, in), pcm.WithRawStream(asc))
	if err != nil {
		if errors.Is(err, pcm.ErrUnsupportedSBR) {
			return 0, errors.New("该音轨是 HE-AAC（SBR）格式，本地解码器不支持")
		}
		return 0, fmt.Errorf("初始化 AAC 解码器失败: %w", err)
	}
	info := dec.Info()
	if info.Channels < 1 || info.Channels > 2 {
		return 0, fmt.Errorf("不支持的声道数 %d（只支持单/立体声）", info.Channels)
	}

	out, err := os.Create(wavPath)
	if err != nil {
		return 0, err
	}
	defer out.Close()

	w := newWAVWriter(out, 16000, 1)
	defer func() { _ = w.finish() }()

	const targetRate = 16000
	conv := &monoConv{ch: info.Channels, rs: &resampler{step: float64(info.SampleRate) / targetRate}}
	totalSrc := float64(in.SampleCount) * 1024 // AAC 每帧 1024 样本
	var doneSrc float64
	emit := func(v float64) { w.emit(v) }

	// 解码输出是交错 S16LE，Read 切分边界任意，要自己拼齐 2 字节对齐。
	var leftover byte
	hasLeft := false
	buf := make([]byte, 64<<10)
	for {
		n, rerr := dec.Read(buf)
		for i := 0; i < n; i++ {
			if hasLeft {
				conv.feed(int16(binary.LittleEndian.Uint16([]byte{leftover, buf[i]})), emit)
				hasLeft = false
				continue
			}
			if i+1 >= n {
				leftover, hasLeft = buf[i], true
				break
			}
			conv.feed(int16(binary.LittleEndian.Uint16(buf[i:i+2])), emit)
			i++
		}
		if n > 0 {
			doneSrc += float64(n/2) / float64(info.Channels)
			if progress != nil && totalSrc > 0 {
				progress(math.Min(1, doneSrc/totalSrc))
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return 0, fmt.Errorf("AAC 解码中断: %w", rerr)
		}
	}
	if err := w.finish(); err != nil {
		return 0, err
	}
	secs := int(math.Round(float64(w.samples) / targetRate))
	return secs, nil
}

// monoConv 把交错 S16 流降混成单声道浮点流，声道相位跨块保持。
type monoConv struct {
	ch    int
	phase int     // 交错位置 % 声道数
	acc   float64 // 立体声时先攒左声道
	rs    *resampler
}

func (c *monoConv) feed(v int16, emit func(float64)) {
	if c.ch == 1 {
		c.rs.push(float64(v)/32768, emit)
		return
	}
	if c.phase == 0 {
		c.acc = float64(v) / 32768
		c.phase = 1
		return
	}
	c.phase = 0
	c.rs.push((c.acc+float64(v)/32768)/2, emit)
}

// ---------------------------------------------------------------------------
// AudioSpecificConfig

// extractASC 从 stsd 盒字节里解出 AAC 的 AudioSpecificConfig
// （mp4a → [wave →] esds → DecoderSpecificInfo）。
// stsd 可能带盒头（8 字节 size+type）也可能只存了盒体，两种都兼容。
func extractASC(stsd []byte) ([]byte, error) {
	if len(stsd) < 8 {
		return nil, fmt.Errorf("stsd 过短（%d 字节）", len(stsd))
	}
	body := stsd
	if binary.BigEndian.Uint32(stsd[0:4]) >= 8 && string(stsd[4:8]) == "stsd" {
		body = stsd[8:] // 带头：跳过 stsd 头
	}
	if len(body) < 8 {
		return nil, fmt.Errorf("stsd 盒体过短")
	}
	body = body[8:] // version/flags(4) + entry_count(4)

	mp4a, ok := findBox(body, "mp4a")
	if !ok {
		return nil, fmt.Errorf("stsd 里没有 mp4a")
	}
	// mp4a（AudioSampleEntry）固定头 28 字节：保留区(6)+dataRef(2)+
	// version/revision(4)+vendor(4)+channels/sampleSize/rate(12)，
	// 之后才是子盒（esds，或 wave → esds）。
	entry := mp4a
	if len(entry) > 28 {
		entry = entry[28:]
	}
	esds, ok := findBox(entry, "esds")
	if !ok {
		if wave, ok2 := findBox(entry, "wave"); ok2 {
			esds, ok = findBox(wave, "esds")
		}
	}
	if !ok {
		return nil, fmt.Errorf("mp4a 里没有 esds（不是 AAC 流？）")
	}
	// esds 是 fullbox：跳过 version/flags。
	if len(esds) < 4 {
		return nil, fmt.Errorf("esds 过短")
	}
	return parseESDS(esds[4:])
}

// findBox 在一段盒序列里找第一个指定类型的子盒，返回盒体。
func findBox(data []byte, typ string) ([]byte, bool) {
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off : off+4]))
		if size < 8 || off+size > len(data) {
			return nil, false
		}
		if string(data[off+4:off+8]) == typ {
			return data[off+8 : off+size], true
		}
		off += size
	}
	return nil, false
}

// parseESDS 走 ES_Descriptor → DecoderConfigDescriptor →
// DecSpecificInfo 三层，取出 ASC 原始字节。
func parseESDS(b []byte) ([]byte, error) {
	// ES_Descriptor：tag 0x03。
	body, err := descriptor(b, 0x03)
	if err != nil {
		return nil, err
	}
	// 头部：ES_ID(2) + flags(1)，flags 决定后面还挂了什么。
	if len(body) < 3 {
		return nil, fmt.Errorf("ES_Descriptor 过短")
	}
	flags := body[2]
	p := 3
	if flags&0x80 != 0 { // streamDependenceFlag
		p += 2
	}
	if flags&0x40 != 0 { // URL_Flag
		if p >= len(body) {
			return nil, fmt.Errorf("ES_Descriptor URL 字段损坏")
		}
		p += 1 + int(body[p])
	}
	if flags&0x20 != 0 { // OCRstreamFlag
		p += 2
	}
	if p > len(body) {
		return nil, fmt.Errorf("ES_Descriptor 头部越界")
	}
	// DecoderConfigDescriptor：tag 0x04。
	dc, err := descriptor(body[p:], 0x04)
	if err != nil {
		return nil, err
	}
	// 固定头部 13 字节（objectType+streamType+bufferSize+max/avgBitrate）。
	if len(dc) < 13 {
		return nil, fmt.Errorf("DecoderConfigDescriptor 过短")
	}
	// DecSpecificInfo：tag 0x05，载荷就是 ASC。
	asc, err := descriptor(dc[13:], 0x05)
	if err != nil {
		return nil, err
	}
	if len(asc) < 2 {
		return nil, fmt.Errorf("AudioSpecificConfig 过短")
	}
	return asc, nil
}

// descriptor 读一个 MPEG-4 描述符：tag 字节 + 可扩展长度 + 载荷。
func descriptor(b []byte, wantTag byte) ([]byte, error) {
	if len(b) < 2 {
		return nil, fmt.Errorf("描述符过短")
	}
	if b[0] != wantTag {
		return nil, fmt.Errorf("描述符 tag 不符：要 %02X 得 %02X", wantTag, b[0])
	}
	length := 0
	i := 1
	for ; i < len(b) && i <= 5; i++ {
		length = length<<7 | int(b[i]&0x7F)
		if b[i]&0x80 == 0 {
			i++
			break
		}
	}
	if i+length > len(b) {
		return nil, fmt.Errorf("描述符长度越界（%d > %d）", i+length, len(b))
	}
	return b[i : i+length], nil
}

// ---------------------------------------------------------------------------
// 裸 AU 流

// auReader 把 fMP4 的样本表包装成 go-aac 要求的「裸 AU 流」：
// 每个 access unit 前面加 2 字节大端长度。实现 io.Reader。
type auReader struct {
	f   *os.File
	in  *mux.Input
	run int
	smp int
	off int64 // 下一个样本数据的绝对偏移

	pend []byte
	pos  int
}

func newAUReader(f *os.File, in *mux.Input) *auReader {
	return &auReader{f: f, in: in, off: firstOffset(in)}
}

func firstOffset(in *mux.Input) int64 {
	for _, r := range in.Runs {
		return r.Offset
	}
	return 0
}

func (r *auReader) Read(p []byte) (int, error) {
	for r.pos >= len(r.pend) {
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.pend[r.pos:])
	r.pos += n
	return n, nil
}

// fill 装载下一个样本（长度前缀 + 载荷）。
func (r *auReader) fill() error {
	for {
		if r.run >= len(r.in.Runs) {
			return io.EOF
		}
		run := &r.in.Runs[r.run]
		if r.smp < len(run.Samples) {
			break
		}
		r.run++
		r.smp = 0
		if r.run < len(r.in.Runs) {
			r.off = r.in.Runs[r.run].Offset
		}
	}
	s := r.in.Runs[r.run].Samples[r.smp]
	data := make([]byte, s.Size)
	if _, err := r.f.ReadAt(data, r.off); err != nil {
		return fmt.Errorf("读样本数据失败: %w", err)
	}
	r.off += int64(s.Size)
	r.smp++

	buf := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(buf[0:2], uint16(len(data)))
	copy(buf[2:], data)
	r.pend, r.pos = buf, 0
	return nil
}
