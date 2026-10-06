package mux

import (
	"encoding/binary"
	"testing"
)

// 这一组测试锁定一个曾经真实发生过的 bug：
//
// DASH 初始化段里的 elst 有两种形状 ——
//   单段式： [(0, media_time)]            ← 早先的测试样本都是这种
//   两段式： [(80, -1), (0, media_time)]  ← B 站很多稿件是这种，第一项是空编辑
//
// 旧的 patchElst 无脑改「首项」，碰上两段式就把整片时长写进了那个空编辑，
// 而空编辑的 segment_duration 含义是「轨道整体延后多久开始」，于是整条轨道
// 被推迟了一整片时长：实测 3:32 的视频被 ffmpeg 报成 7:04，音视频时间戳
// 整体平移 212 秒。
//
// 单段式样本跑不出这个问题，所以它躲过了之前所有验证。

// buildElst 造一个 elst 盒。entries 是 (segment_duration, media_time)。
func buildElst(version byte, entries ...[2]int64) []byte {
	var payload []byte
	payload = append(payload, u32(uint32(len(entries)))...)
	for _, e := range entries {
		if version == 1 {
			payload = append(payload, u64(uint64(e[0]))...)
			payload = append(payload, u64(uint64(e[1]))...)
		} else {
			payload = append(payload, u32(clamp32(uint64(e[0])))...)
			payload = append(payload, u32(uint32(int32(e[1])))...)
		}
		payload = append(payload, 0, 1, 0, 0) // media_rate = 1.0
	}
	return fullBox("elst", version, 0, payload)
}

// readElstEntries 从 edts 盒里取出 elst 的条目，顺便断言层级没有套错。
func readElstEntries(t *testing.T, edts []byte) [][2]int64 {
	t.Helper()
	if len(edts) < 16 {
		t.Fatalf("edts 太短：%d 字节", len(edts))
	}
	if got := string(edts[4:8]); got != "edts" {
		t.Fatalf("顶层盒应为 edts，实际 %q", got)
	}
	if got := string(edts[12:16]); got != "elst" {
		t.Fatalf("edts 的第一个子盒应为 elst，实际 %q（是不是把 edts 又套了一层？）", got)
	}

	body := edts[8:]
	version := body[8]
	count := int(binary.BigEndian.Uint32(body[12:16]))
	entrySize := 12
	if version == 1 {
		entrySize = 20
	}
	if 16+count*entrySize > len(body) {
		t.Fatalf("entry_count=%d 与实际长度 %d 不符", count, len(body))
	}

	out := make([][2]int64, 0, count)
	for i := 0; i < count; i++ {
		p := 16 + i*entrySize
		var seg, mt int64
		if version == 1 {
			seg = int64(binary.BigEndian.Uint64(body[p : p+8]))
			mt = int64(binary.BigEndian.Uint64(body[p+8 : p+16]))
		} else {
			seg = int64(binary.BigEndian.Uint32(body[p : p+4]))
			mt = int64(int32(binary.BigEndian.Uint32(body[p+4 : p+8])))
		}
		out = append(out, [2]int64{seg, mt})
	}
	return out
}

// 两段式源：空编辑必须被丢掉，时长写给真编辑项，media_time 原样保留。
func TestPatchElstDropsEmptyEdit(t *testing.T) {
	const durMovie = 212240
	edts := box("edts", buildElst(0, [2]int64{80, -1}, [2]int64{0, 2560}))

	out := patchElst(edts, durMovie)
	if out == nil {
		t.Fatal("有真编辑项时不该返回 nil")
	}
	entries := readElstEntries(t, out)
	if len(entries) != 1 {
		t.Fatalf("空编辑应被丢掉，期望只剩 1 条，实际 %d 条：%v", len(entries), entries)
	}
	if entries[0][0] != durMovie {
		t.Errorf("segment_duration 应为 %d，实际 %d", durMovie, entries[0][0])
	}
	if entries[0][1] != 2560 {
		t.Errorf("media_time 必须原样保留 2560，实际 %d", entries[0][1])
	}
}

// 单段式源（早先样本的形状）：行为不能变。
func TestPatchElstSingleEntry(t *testing.T) {
	const durMovie = 15509
	edts := box("edts", buildElst(0, [2]int64{0, 2133}))

	entries := readElstEntries(t, patchElst(edts, durMovie))
	if len(entries) != 1 {
		t.Fatalf("期望 1 条，实际 %d 条", len(entries))
	}
	if entries[0][0] != durMovie || entries[0][1] != 2133 {
		t.Errorf("期望 (%d, 2133)，实际 %v", durMovie, entries[0])
	}
}

// 音频常见的形状：media_time = 0 的单条编辑。
func TestPatchElstAudioShape(t *testing.T) {
	edts := box("edts", buildElst(0, [2]int64{0, 0}))
	entries := readElstEntries(t, patchElst(edts, 212309))
	if len(entries) != 1 || entries[0][0] != 212309 || entries[0][1] != 0 {
		t.Fatalf("期望 [(212309, 0)]，实际 %v", entries)
	}
}

// 全是空编辑：没有可用的真编辑项，调用方应整个跳过 edts。
func TestPatchElstAllEmpty(t *testing.T) {
	edts := box("edts", buildElst(0, [2]int64{80, -1}))
	if out := patchElst(edts, 1000); out != nil {
		t.Fatalf("全是空编辑时应返回 nil，实际 %d 字节", len(out))
	}
}

// v1 版 elst（64 位字段）同样要能处理。
func TestPatchElstVersion1(t *testing.T) {
	edts := box("edts", buildElst(1, [2]int64{80, -1}, [2]int64{0, 2560}))
	entries := readElstEntries(t, patchElst(edts, 212240))
	if len(entries) != 1 || entries[0][1] != 2560 {
		t.Fatalf("v1 处理不对，实际 %v", entries)
	}
}

// 结果盒的总长度必须自洽 —— 防的是「把 edts 又套一层」这类低级错
// （源模板里传进来的 Edts 已含盒头，很容易多包一次）。
func TestPatchElstBoxSizeIsConsistent(t *testing.T) {
	edts := box("edts", buildElst(0, [2]int64{80, -1}, [2]int64{0, 2560}))
	out := patchElst(edts, 1000)

	declared := int(binary.BigEndian.Uint32(out[0:4]))
	if declared != len(out) {
		t.Errorf("外层长度字段 %d 与实际 %d 不符", declared, len(out))
	}
	child := int(binary.BigEndian.Uint32(out[8:12]))
	if 8+child != len(out) {
		t.Errorf("子盒长度 %d 与外层 %d 不符", child, len(out))
	}
}

// 传入的不是 edts 时应当原样返回，而不是把它弄坏。
func TestPatchElstPassesThroughUnexpectedInput(t *testing.T) {
	raw := box("free", []byte{1, 2, 3, 4})
	out := patchElst(raw, 1000)
	if len(out) != len(raw) || string(out[4:8]) != "free" {
		t.Fatalf("非 edts 输入应原样返回，实际 %q", out[4:8])
	}
}

// clamp32 是 32 位盒字段的溢出保护，顺手验证边界。
func TestClamp32(t *testing.T) {
	if got := clamp32(123); got != 123 {
		t.Errorf("clamp32(123) = %d", got)
	}
	if got := clamp32(1 << 33); got != 0xFFFFFFFF {
		t.Errorf("clamp32(1<<33) 应饱和到 0xFFFFFFFF，实际 %#x", got)
	}
}
