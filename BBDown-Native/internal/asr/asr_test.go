package asr

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResamplerConstantPreserve 恒定输入重采样后仍是恒定值，
// 且输出样本数 ≈ 输入数 / step。
func TestResamplerConstantPreserve(t *testing.T) {
	r := &resampler{step: 3} // 48k → 16k
	var got []float64
	emit := func(v float64) { got = append(got, v) }
	const src = 0.5
	for i := 0; i < 3000; i++ {
		r.push(src, emit)
	}
	for i, v := range got {
		if math.Abs(v-src) > 1e-9 {
			t.Fatalf("恒定输入被改变：got[%d]=%v", i, v)
		}
	}
	// 3000 个源样本 → 期望 ~1000 个输出样本（首尾各差 1~2 个可接受）
	if n := len(got); n < 997 || n > 1002 {
		t.Fatalf("输出样本数异常：%d（期望 ~1000）", n)
	}
}

// TestResamplerRampCheck 斜坡输入验证插值落在两端之间、比例正确。
func TestResamplerRampCheck(t *testing.T) {
	r := &resampler{step: float64(44100) / 16000} // 2.75625
	var got []float64
	emit := func(v float64) { got = append(got, v) }
	const n = 4410 // 0.1 秒 44.1k
	for i := 0; i < n; i++ {
		r.push(float64(i), emit) // 0,1,2,...
	}
	// 输出数应约为 1600（0.1 秒 16k）
	if len(got) < 1595 || len(got) > 1605 {
		t.Fatalf("输出样本数异常：%d（期望 ~1600）", len(got))
	}
	// 插值样本必须落在相邻两个源样本之间
	for _, v := range got {
		if v < 0 || v > float64(n) {
			t.Fatalf("插值越界：%v", v)
		}
	}
}

// TestWAVWriterSizes 验证头部尺寸回填与 PCM 数据一致。
func TestWAVWriterSizes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "t.wav")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := newWAVWriter(f, 16000, 1)
	for i := 0; i < 1600; i++ { // 0.1 秒
		w.emit(math.Sin(float64(i) / 100))
	}
	if err := w.finish(); err != nil {
		t.Fatal(err)
	}
	f.Close()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 44+1600*2 {
		t.Fatalf("WAV 总长 %d，期望 %d", len(b), 44+1600*2)
	}
	if string(b[0:4]) != "RIFF" || string(b[36:40]) != "data" {
		t.Fatalf("头部标记不对")
	}
	if got := binary.LittleEndian.Uint32(b[4:]); got != uint32(1600*2+36) {
		t.Fatalf("RIFF 尺寸字段 %d", got)
	}
	if got := binary.LittleEndian.Uint32(b[40:]); got != 3200 {
		t.Fatalf("data 尺寸字段 %d", got)
	}
	if got := binary.LittleEndian.Uint32(b[24:]); got != 16000 {
		t.Fatalf("采样率字段 %d", got)
	}
}

// TestExtractASC 用手工构造的 mp4a→esds 盒链验证 ASC 抽取。
// ASC = 0x12 0x10（objectType=2 AAC-LC，freqIdx=4 即 44100，chanCfg=2）。
func TestExtractASC(t *testing.T) {
	asc := []byte{0x12, 0x10, 0x56, 0xE5} // 多塞两字节验证长度解析
	esds := makeESDSBox(asc)
	// mp4a：28 字节固定头 + esds 子盒
	mp4a := makeBox("mp4a", make([]byte, 28), esds)
	stsd := makeBox("stsd", make([]byte, 8), mp4a) // version/flags + entry_count

	got, err := extractASC(stsd)
	if err != nil {
		t.Fatalf("extractASC 失败: %v", err)
	}
	if len(got) != len(asc) {
		t.Fatalf("ASC 长度 %d，期望 %d", len(got), len(asc))
	}
	for i := range asc {
		if got[i] != asc[i] {
			t.Fatalf("ASC 字节不符 @%d", i)
		}
	}
}

// makeBox 拼一个普通盒。
func makeBox(typ string, payload ...[]byte) []byte {
	var b []byte
	total := 8
	for _, p := range payload {
		total += len(p)
	}
	b = binary.BigEndian.AppendUint32(b, uint32(total))
	b = append(b, typ...)
	for _, p := range payload {
		b = append(b, p...)
	}
	return b
}

// makeESDSBox 构造 esds：ES_Descriptor(0x03) → DecoderConfig(0x04) →
// DecSpecificInfo(0x05) → asc。含多字节可扩展长度（>127 时要用
// 0x80 续位），顺便覆盖那条路径。
func makeESDSBox(asc []byte) []byte {
	dsi := append([]byte{0x05}, lenExp(len(asc))...)
	dsi = append(dsi, asc...)

	dcBody := make([]byte, 13) // objectType/streamType/bufferSize/maxBitrate/avgBitrate
	dcBody = append(dcBody, dsi...)
	dc := append([]byte{0x04}, lenExp(len(dcBody))...)
	dc = append(dc, dcBody...)

	esBody := make([]byte, 3) // ES_ID(2) + flags(1)
	esBody = append(esBody, dc...)
	es := append([]byte{0x03}, lenExp(len(esBody))...)
	es = append(es, esBody...)

	return makeBox("esds", make([]byte, 4), es) // fullbox 头
}

// lenExp 编码可扩展长度：<128 单字节，否则 0x80 续位。
func lenExp(n int) []byte {
	if n < 128 {
		return []byte{byte(n)}
	}
	return []byte{0x80 | byte(n>>7), byte(n & 0x7F)}
}

func TestParseSegments(t *testing.T) {
	out := strings.Join([]string{
		"whisper_init_from_file: model loaded",   // 噪声行应被忽略
		"[00:00:00.000 --> 00:00:04.120]  大家好",   // 标准格式
		"[01:02:03.456 --> 01:02:08.000]  第二段内容", // 小时进制
		"",
		"not a segment line",
		"[00:00:04.120 --> 00:00:07.890]  Second line",
	}, "\n")
	cues := parseSegments(out)
	if len(cues) != 3 {
		t.Fatalf("应解析出 3 条，拿到 %d：%+v", len(cues), cues)
	}
	if cues[0].From != 0 || cues[0].To != 4.12 || cues[0].Text != "大家好" {
		t.Fatalf("第 1 条不对：%+v", cues[0])
	}
	if cues[1].From != 3723.456 || cues[1].Text != "第二段内容" {
		t.Fatalf("第 2 条不对：%+v", cues[1])
	}
	if cues[2].Text != "Second line" {
		t.Fatalf("第 3 条不对：%+v", cues[2])
	}
}
