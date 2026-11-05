package asr

import (
	"runtime"
	"testing"
)

// TestQuantizedName 模型名到量化版的映射。
func TestQuantizedName(t *testing.T) {
	cases := map[string]string{
		"tiny":           "tiny-q5_1",
		"base":           "base-q5_1",
		"small":          "small-q5_1",
		"medium":         "medium-q5_0",
		"large-v3":       "large-v3-q5_0",
		"large-v3-turbo": "large-v3-turbo-q5_0",
		"small-fp16":     "small",       // 显式 fp16 逃生口
		"small-q5_1":     "small-q5_1",  // 已是量化名原样
		"some-custom":    "some-custom", // 未知名不动
	}
	for in, want := range cases {
		if got := quantizedName(in); got != want {
			t.Fatalf("quantizedName(%q)=%q，要 %q", in, got, want)
		}
	}
}

// TestThreads 线程数收敛在 [1,8]。
func TestThreads(t *testing.T) {
	n := Threads()
	if n < 1 || n > 8 {
		t.Fatalf("线程数应在 1~8，实际 %d", n)
	}
	if n > runtime.NumCPU() {
		t.Fatalf("线程数不该超过逻辑核数： %d > %d", n, runtime.NumCPU())
	}
}
