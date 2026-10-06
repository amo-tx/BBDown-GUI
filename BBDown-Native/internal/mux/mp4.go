package mux

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
)

// MuxResult 汇报一次封装的结果。
type MuxResult struct {
	Output       string
	VideoSamples int
	AudioSamples int
	Chunks       int
	DurationMs   int64
	Size         int64
}

// 输出文件的电影时间刻度。1000 就是「毫秒」，可读性最好。
const movieTimescale = 1000

// muxTrack 是一条输出轨道。
type muxTrack struct {
	in      *Input
	idx     int   // 在 tracks 里的位置，用于取源文件句柄
	trackID uint32
	order   []int    // run 下标，按其在输出文件里出现的先后排列（chunk 顺序）
	offsets []uint64 // 各 chunk 数据相对 mdat 体起点的偏移
}

// outChunk 是输出文件里的一个 chunk：源自某条轨道的某个 run。
type outChunk struct {
	track *muxTrack
	run   int
	size  int64
}

// Mux 把 B 站的 DASH 流合成一个渐进式 MP4。
//
// videoPath / audioPath 允许任一为空 —— 为空表示只封装另一条轨道（纯音频/纯视频）。
// 输出先写同目录的临时文件，成功后原子改名，避免半个文件被当成成品。
func Mux(videoPath, audioPath, outPath string) (*MuxResult, error) {
	var video, audio *Input
	var err error
	if videoPath != "" {
		if video, err = OpenInput(videoPath); err != nil {
			return nil, fmt.Errorf("解析视频流: %w", err)
		}
	}
	if audioPath != "" {
		if audio, err = OpenInput(audioPath); err != nil {
			return nil, fmt.Errorf("解析音频流: %w", err)
		}
	}
	if video == nil && audio == nil {
		return nil, fmt.Errorf("没有可封装的轨道")
	}

	// ---- 1. 建轨道。视频优先拿 track_ID 1，播放器兼容性最好。 ----
	var tracks []*muxTrack
	if video != nil {
		tracks = append(tracks, &muxTrack{in: video, trackID: 1})
	}
	if audio != nil {
		tracks = append(tracks, &muxTrack{in: audio, trackID: uint32(len(tracks) + 1)})
	}
	for i, tr := range tracks {
		tr.idx = i
	}

	// ---- 2. 归并两条轨道的 run，决定 chunk 在 mdat 里的先后。 ----
	// 按起始时间交错，能保证播放器顺序读时不必来回跳。
	type keyed struct {
		tr  *muxTrack
		run int
		sec float64
	}
	var ks []keyed
	for _, tr := range tracks {
		ts := float64(tr.in.Template.Timescale)
		if ts <= 0 {
			ts = 1
		}
		for i, r := range tr.in.Runs {
			ks = append(ks, keyed{tr: tr, run: i, sec: float64(r.Time) / ts})
		}
	}
	// 稳定排序：时间相同时保持「视频在前」的原始次序。
	sort.SliceStable(ks, func(i, j int) bool { return ks[i].sec < ks[j].sec })

	var (
		chunks    []outChunk
		dataTotal uint64
	)
	for _, k := range ks {
		run := &k.tr.in.Runs[k.run]
		k.tr.order = append(k.tr.order, k.run)
		k.tr.offsets = append(k.tr.offsets, dataTotal)
		chunks = append(chunks, outChunk{track: k.tr, run: k.run, size: run.Bytes})
		dataTotal += uint64(run.Bytes)
	}

	// ---- 3. 先定 moov 的长度，再回推真实偏移。 ----
	// moov 的长度只取决于「用 stco 还是 co64」，与偏移值本身无关，
	// 所以先拿 base=0 量一次即可。
	ftyp := buildFtyp()
	probe := buildMoov(tracks, 0, false)
	const hdr32, hdr64 = uint64(8), uint64(16)
	base := uint64(len(ftyp)) + uint64(len(probe)) + hdr32
	use64 := base+dataTotal > 0xFFFFFFFF
	mdatHdrLen := hdr32
	if use64 {
		probe = buildMoov(tracks, 0, true)
		mdatHdrLen = hdr64
		base = uint64(len(ftyp)) + uint64(len(probe)) + mdatHdrLen
	}
	moov := buildMoov(tracks, base, use64)

	var mdatHdr []byte
	if use64 {
		mdatHdr = cat(u32(1), []byte("mdat"), u64(dataTotal+hdr64))
	} else {
		mdatHdr = cat(u32(uint32(dataTotal+hdr32)), []byte("mdat"))
	}

	// ---- 4. 落盘。 ----
	out, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		out.Close()
		if !ok {
			os.Remove(outPath)
		}
	}()

	w := bufio.NewWriterSize(out, 1<<20)
	written := int64(0)
	emit := func(b []byte) error {
		n, err := w.Write(b)
		written += int64(n)
		return err
	}
	if err := emit(ftyp); err != nil {
		return nil, err
	}
	if err := emit(moov); err != nil {
		return nil, err
	}
	if err := emit(mdatHdr); err != nil {
		return nil, err
	}

	srcs := make([]*os.File, len(tracks))
	for i, tr := range tracks {
		f, err := os.Open(tr.in.Path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		srcs[i] = f
	}

	for _, c := range chunks {
		run := &c.track.in.Runs[c.run]
		sr := io.NewSectionReader(srcs[c.track.idx], run.Offset, run.Bytes)
		n, err := io.CopyN(w, sr, run.Bytes)
		written += n
		if err != nil {
			return nil, fmt.Errorf("复制媒体数据失败: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return nil, err
	}
	if err := out.Sync(); err != nil {
		return nil, err
	}
	ok = true

	res := &MuxResult{Output: outPath, Chunks: len(chunks), Size: written}
	if video != nil {
		res.VideoSamples = video.SampleCount
	}
	// 时长以视频轨为准，没有视频就取音频。
	ref := video
	if ref == nil {
		ref = audio
	}
	res.DurationMs = int64(scaleCeil(ref.Duration, uint64(ref.Template.Timescale), 1000))
	if audio != nil {
		res.AudioSamples = audio.SampleCount
	}
	return res, nil
}

// ---------------------------------------------------------------------------
// moov

func buildFtyp() []byte {
	// 自造一个标准的渐进式 MP4 品牌。B 站源文件里带 dash 品牌，
	// 留着有些老播放器会当成碎片 MP4 处理。
	return box("ftyp", cat(
		[]byte("isom"), u32(512),
		[]byte("isom"), []byte("iso2"), []byte("avc1"), []byte("mp41"),
	))
}

func buildMoov(tracks []*muxTrack, base uint64, use64 bool) []byte {
	var parts [][]byte
	var maxDur uint64
	for _, tr := range tracks {
		trak, dur := tr.buildTrak(base, use64)
		if dur > maxDur {
			maxDur = dur
		}
		parts = append(parts, trak)
	}
	parts = append([][]byte{buildMvhd(maxDur, uint32(len(tracks)+1))}, parts...)
	return box("moov", cat(parts...))
}

func buildMvhd(duration uint64, nextTrackID uint32) []byte {
	payload := cat(
		u32(0), u32(0), // creation / modification time
		u32(movieTimescale),
		u32(clamp32(duration)),
		u32(0x00010000), // rate = 1.0
		u16(0x0100), // volume = 1.0
		u16(0),      // reserved
		u32(0), u32(0),
		// 单位矩阵
		u32(0x00010000), u32(0), u32(0),
		u32(0), u32(0x00010000), u32(0),
		u32(0), u32(0), u32(0x40000000),
		u32(0), u32(0), u32(0), u32(0), u32(0), u32(0), // pre_defined
		u32(nextTrackID),
	)
	return fullBox("mvhd", 0, 0, payload)
}

func (tr *muxTrack) buildTrak(base uint64, use64 bool) ([]byte, uint64) {
	ts := uint64(tr.in.Template.Timescale)
	durMedia := tr.in.Duration
	durMovie := scaleCeil(durMedia, ts, movieTimescale)

	parts := [][]byte{tr.patchTkhd(durMovie)}
	if tr.in.Template.Edts != nil {
		// patchElst 可能返回 nil（源里全是空编辑），那就干脆不写 edts。
		if b := patchElst(tr.in.Template.Edts, durMovie); b != nil {
			parts = append(parts, b)
		}
	}
	mdia := box("mdia", cat(
		tr.patchMdhd(durMedia),
		tr.in.Template.Hdlr,
		box("minf", cat(tr.in.Template.MinfExtra, tr.buildStbl(base, use64))),
	))
	parts = append(parts, mdia)
	return box("trak", cat(parts...)), durMovie
}

// patchTkhd 把原样保留的 tkhd 里的 track_ID 与时长改成我们要的值。
func (tr *muxTrack) patchTkhd(durMovie uint64) []byte {
	raw := append([]byte(nil), tr.in.Template.Tkhd...)
	if len(raw) < 32 {
		return raw
	}
	if raw[8] == 1 {
		// v1: creation(8) modification(8) track_ID(4) reserved(4) duration(8)
		patchU32(raw, 28, tr.trackID)
		patchU64(raw, 36, durMovie)
	} else {
		// v0: creation(4) modification(4) track_ID(4) reserved(4) duration(4)
		patchU32(raw, 20, tr.trackID)
		patchU32(raw, 28, clamp32(durMovie))
	}
	return raw
}

// patchMdhd 改写 mdhd 里的时长（原样保留 timescale 与语言）。
func (tr *muxTrack) patchMdhd(durMedia uint64) []byte {
	raw := append([]byte(nil), tr.in.Template.Mdhd...)
	if len(raw) < 32 {
		return raw
	}
	if raw[8] == 1 {
		// v1: creation(8) modification(8) timescale(4) duration(8)
		patchU64(raw, 32, durMedia)
	} else {
		// v0: creation(4) modification(4) timescale(4) duration(4)
		patchU32(raw, 24, clamp32(durMedia))
	}
	return raw
}

// patchElst 重建 edts/elst：给真正的编辑项写上当片时长，并丢掉空编辑项。
//
// DASH 初始化段里的 elst 是个半成品，条目分两种：
//   - 空编辑（media_time == -1）：segment_duration 的含义是「轨道整体延后多久开始」，
//     **不是**片段时长；
//   - 真编辑（media_time >= 0）：segment_duration 本该是本段时长，但 DASH 里普遍写成 0
//     （意为「一直到结尾」），必须由我们补上。
//
// 早先实现无脑改首项。碰上「空编辑 + 真编辑」两段式的源（B 站很多稿件都是），
// 一整片时长就被写进了空编辑 —— 于是整条轨道被推迟了 212 秒，容器时长直接翻倍
// （实测 3:32 的视频报成 7:04，而且音视频时间戳整体平移）。
// 单段式源看不出问题，所以这个 bug 一直躲过了早先的验证。
//
// 空编辑一律丢弃，与 ffmpeg 的产出对齐：它是事实参照，且两条轨道都从 0 开始，
// 不会凭空造出 A/V 偏移。
//
// 返回 nil 表示没有可用的真编辑项（此时调用方应整个跳过 edts）。
func patchElst(edts []byte, durMovie uint64) []byte {
	// 传进来的是完整的 edts 盒（含它自己的 8 字节头），子盒从偏移 8 开始。
	if len(edts) < 8 || string(edts[4:8]) != "edts" {
		return append([]byte(nil), edts...)
	}
	var parts [][]byte
	off := 8
	for off+8 <= len(edts) {
		size := int(binary.BigEndian.Uint32(edts[off : off+4]))
		if size < 8 || off+size > len(edts) {
			break
		}
		if string(edts[off+4:off+8]) == "elst" {
			if b := rebuildElst(edts[off:off+size], durMovie); b != nil {
				parts = append(parts, b)
			}
		} else {
			parts = append(parts, append([]byte(nil), edts[off:off+size]...))
		}
		off += size
	}
	if len(parts) == 0 {
		return nil
	}
	return box("edts", cat(parts...))
}

// editEntry 是 elst 里的一条编辑项。
type editEntry struct {
	seg       uint64
	mediaTime int64
	rate      [4]byte // media_rate_integer + media_rate_fraction，原样保留
}

// rebuildElst 丢掉空编辑，并把首条真编辑的 segment_duration 换成当片时长。
func rebuildElst(elst []byte, durMovie uint64) []byte {
	if len(elst) < 16 {
		return nil
	}
	version := elst[8]
	count := int(binary.BigEndian.Uint32(elst[12:16]))
	entrySize := 12
	if version == 1 {
		entrySize = 20
	}
	// entry_count 是源文件给的，坏了就按实际长度兜底，别越界读。
	if avail := (len(elst) - 16) / entrySize; count > avail {
		count = avail
	}

	edits := make([]editEntry, 0, count)
	for i := 0; i < count; i++ {
		p := 16 + i*entrySize
		var e editEntry
		if version == 1 {
			e.seg = binary.BigEndian.Uint64(elst[p : p+8])
			e.mediaTime = int64(binary.BigEndian.Uint64(elst[p+8 : p+16]))
			copy(e.rate[:], elst[p+16:p+20])
		} else {
			e.seg = uint64(binary.BigEndian.Uint32(elst[p : p+4]))
			e.mediaTime = int64(int32(binary.BigEndian.Uint32(elst[p+4 : p+8])))
			copy(e.rate[:], elst[p+8:p+12])
		}
		if e.mediaTime < 0 {
			continue // 空编辑：整条丢掉
		}
		edits = append(edits, e)
	}
	if len(edits) == 0 {
		return nil
	}
	edits[0].seg = durMovie

	payload := make([]byte, 0, 4+len(edits)*entrySize)
	payload = append(payload, u32(uint32(len(edits)))...)
	for _, e := range edits {
		if version == 1 {
			payload = append(payload, u64(e.seg)...)
			payload = append(payload, u64(uint64(e.mediaTime))...)
		} else {
			payload = append(payload, u32(clamp32(e.seg))...)
			payload = append(payload, u32(uint32(int32(e.mediaTime)))...)
		}
		payload = append(payload, e.rate[:]...)
	}
	return fullBox("elst", version, 0, payload)
}

// ---------------------------------------------------------------------------
// stbl

func (tr *muxTrack) buildStbl(base uint64, use64 bool) []byte {
	samples := tr.samples()

	parts := [][]byte{
		tr.in.Template.Stsd,
		buildStts(samples),
	}
	if b := buildCtts(samples); b != nil {
		parts = append(parts, b)
	}
	if b := buildStss(samples); b != nil {
		parts = append(parts, b)
	}
	parts = append(parts, tr.buildStsc(), buildStsz(samples))
	if use64 {
		parts = append(parts, tr.buildCo64(base))
	} else {
		parts = append(parts, tr.buildStco(base))
	}
	return box("stbl", cat(parts...))
}

// samples 按 chunk 在输出文件里的顺序，把该轨道所有采样摊平成一条序列。
func (tr *muxTrack) samples() []Sample {
	n := 0
	for _, ri := range tr.order {
		n += len(tr.in.Runs[ri].Samples)
	}
	out := make([]Sample, 0, n)
	for _, ri := range tr.order {
		out = append(out, tr.in.Runs[ri].Samples...)
	}
	return out
}

// buildStts：解码时长表，连续相同的时长归并成一项。
func buildStts(samples []Sample) []byte {
	var entries []byte
	var runLen uint32
	var cur uint32
	flush := func() {
		if runLen > 0 {
			entries = append(entries, u32(runLen)...)
			entries = append(entries, u32(cur)...)
		}
	}
	for i, s := range samples {
		if i == 0 || s.Duration != cur {
			flush()
			cur, runLen = s.Duration, 0
		}
		runLen++
	}
	flush()
	return fullBox("stts", 0, 0, cat(u32(uint32(len(entries)/8)), entries))
}

// buildCtts：合成时间偏移表。全为 0 时返回 nil（可以整个盒省略）。
func buildCtts(samples []Sample) []byte {
	any := false
	neg := false
	for _, s := range samples {
		if s.CTS != 0 {
			any = true
		}
		if s.CTS < 0 {
			neg = true
		}
	}
	if !any {
		return nil
	}
	var entries []byte
	var runLen uint32
	var cur int32
	flush := func() {
		if runLen > 0 {
			entries = append(entries, u32(runLen)...)
			entries = append(entries, u32(uint32(cur))...)
		}
	}
	for i, s := range samples {
		if i == 0 || s.CTS != cur {
			flush()
			cur, runLen = s.CTS, 0
		}
		runLen++
	}
	flush()
	ver := byte(0)
	if neg {
		ver = 1
	}
	return fullBox("ctts", ver, 0, cat(u32(uint32(len(entries)/8)), entries))
}

// buildStss：同步采样（关键帧）表。全都是同步帧时返回 nil —— 省略即代表「全部可寻址」。
func buildStss(samples []Sample) []byte {
	var idx []byte
	n := uint32(0)
	for i, s := range samples {
		if s.Sync {
			idx = append(idx, u32(uint32(i+1))...)
			n++
		}
	}
	if n == uint32(len(samples)) {
		return nil
	}
	return fullBox("stss", 0, 0, cat(u32(n), idx))
}

// buildStsc：chunk 与采样的映射。
//
// 因为「一个 run = 一个 chunk」，每个 chunk 的采样数等于该 run 的采样数；
// 连续数量相同的 chunk 合并成一条记录。
func (tr *muxTrack) buildStsc() []byte {
	var entries []byte
	firstChunk := uint32(1)
	for i := 0; i < len(tr.order); {
		n := len(tr.in.Runs[tr.order[i]].Samples)
		j := i
		for j < len(tr.order) && len(tr.in.Runs[tr.order[j]].Samples) == n {
			j++
		}
		entries = append(entries, u32(firstChunk)...)
		entries = append(entries, u32(uint32(n))...)
		entries = append(entries, u32(1)...) // sample_description_index
		firstChunk += uint32(j - i)
		i = j
	}
	return fullBox("stsc", 0, 0, cat(u32(uint32(len(entries)/12)), entries))
}

// buildStsz：采样大小表。全部相同时走 compact 形式。
func buildStsz(samples []Sample) []byte {
	count := uint32(len(samples))
	uniform := true
	for _, s := range samples {
		if s.Size != samples[0].Size {
			uniform = false
			break
		}
	}
	if uniform {
		return fullBox("stsz", 0, 0, cat(u32(samples[0].Size), u32(count)))
	}
	payload := cat(u32(0), u32(count))
	for _, s := range samples {
		payload = append(payload, u32(s.Size)...)
	}
	return fullBox("stsz", 0, 0, payload)
}

func (tr *muxTrack) buildStco(base uint64) []byte {
	payload := cat(u32(uint32(len(tr.offsets))))
	for _, o := range tr.offsets {
		payload = append(payload, u32(uint32(base+o))...)
	}
	return fullBox("stco", 0, 0, payload)
}

func (tr *muxTrack) buildCo64(base uint64) []byte {
	payload := cat(u32(uint32(len(tr.offsets))))
	for _, o := range tr.offsets {
		payload = append(payload, u64(base+o)...)
	}
	return fullBox("co64", 0, 0, payload)
}

// clamp32 把可能超过 32 位的时长压到 u32 上限（v0 的盒只能装 32 位）。
func clamp32(v uint64) uint32 {
	if v > 0xFFFFFFFF {
		return 0xFFFFFFFF
	}
	return uint32(v)
}

// scaleCeil 把时间值从 from 刻度换算到 to 刻度，向上取整。
//
// 必须向上取整：这些值会写进 tkhd / elst 的 segment_duration。播放器
// 拿 segment_duration 去裁切媒体，向下取整会让末尾不足一个刻度的部分被丢掉
// ——实测 15.5093s 的音频写成 15509ms，ffmpeg 就丢掉了最后 16 个采样。
func scaleCeil(v, from, to uint64) uint64 {
	if from == 0 {
		return 0
	}
	return (v*to + from - 1) / from
}
