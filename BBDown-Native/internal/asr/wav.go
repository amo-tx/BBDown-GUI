package asr

import (
	"encoding/binary"
	"math"
	"os"
)

// ---------------------------------------------------------------------------
// 线性重采样

// resampler 是流式线性插值重采样器：srcRate → dstRate（本程序只
// 用于 44.1k/48k → 16k，step > 1 即降采样）。
//
// 数学：第 j 个输出样本位于源流位置 x_j = j·step，落在源样本
// i = floor(x) 与 i+1 之间线性插值。push 的循环保证只有拿到
// v[i+1] 才输出，所以任意块边界都安全。
type resampler struct {
	step float64 // srcRate / dstRate
	x    float64 // 下一个输出样本的源位置（以源样本序号计）
	i    int     // 已收到的源样本总数（不含首个待配对样本）
	v0   float64 // 源样本 i-1
	v1   float64 // 源样本 i
	once bool
}

func (r *resampler) push(s float64, emit func(float64)) {
	if !r.once {
		r.v1, r.once = s, true
		return
	}
	r.v0, r.v1 = r.v1, s
	r.i++
	for {
		k := math.Floor(r.x)
		if k+1 > float64(r.i) {
			break
		}
		frac := r.x - k
		emit(r.v0 + (r.v1-r.v0)*frac)
		r.x += r.step
	}
}

// ---------------------------------------------------------------------------
// WAV 写出

// wavWriter 写 16bit PCM WAV。头部尺寸未知先占位，finish 时回填，
// 因此支持边解码边落盘。
type wavWriter struct {
	f       *os.File
	samples int // 单声道样本数
	buf     []byte
	written int64
}

func newWAVWriter(f *os.File, rate, ch int) *wavWriter {
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], 0) // 占位，finish 回填
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], uint16(ch))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*ch*2)) // 字节率
	binary.LittleEndian.PutUint16(h[32:], uint16(ch*2))      // 块对齐
	binary.LittleEndian.PutUint16(h[34:], 16)                // 位深
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], 0) // 占位，finish 回填
	f.Write(h)
	return &wavWriter{f: f}
}

// emit 追加一个 [-1,1] 浮点样本，攒够 64KB 落盘。
func (w *wavWriter) emit(v float64) {
	if v > 1 {
		v = 1
	} else if v < -1 {
		v = -1
	}
	s := int16(math.Round(v * 32767))
	w.buf = binary.LittleEndian.AppendUint16(w.buf, uint16(s))
	w.samples++
	if len(w.buf) >= 64<<10 {
		w.flush()
	}
}

func (w *wavWriter) flush() {
	if len(w.buf) == 0 {
		return
	}
	w.f.Write(w.buf)
	w.written += int64(len(w.buf))
	w.buf = w.buf[:0]
}

// finish 回填 RIFF/data 尺寸。重复调用安全（EOF 后无新数据）。
func (w *wavWriter) finish() error {
	w.flush()
	patch := func(off int64, v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		w.f.WriteAt(b[:], off)
	}
	dataLen := uint32(w.written)
	patch(4, dataLen+36)
	patch(40, dataLen)
	return nil
}
