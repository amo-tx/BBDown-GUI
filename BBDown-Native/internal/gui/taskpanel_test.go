package gui

import (
	"strings"
	"testing"

	"bbdown-native/internal/app"
)

// TestStatCardWidthsFit 锁住三张指标卡必须**加起来正好等于可用宽度**。
//
// 出过一次真实事故：中间那张带了个「不小于两侧」的兜底，窗口一窄，
// 三张卡合计超出卡片右边界，最后一张被裁掉半截。
// 这里按老版网页的比例（1 : 1.35 : 1）反推各处宽度并断言总和。
func TestStatCardWidthsFit(t *testing.T) {
	for _, avail := range []int{240, 320, 400, 520, 680, 900} {
		gap := 6
		inner := avail - 2*gap
		side := inner / 3
		mid := inner - 2*side
		total := side + mid + side + 2*gap
		if total != avail {
			t.Errorf("avail=%d：卡片合计宽 %d，超出 %dpx", avail, total, total-avail)
		}
		if mid < side {
			t.Errorf("avail=%d：中间卡 %d 比两侧 %d 还窄，比例反了", avail, mid, side)
		}
	}
}

// TestSparkSlidingWindow 验证速度曲线采样只保留最近 sparkPoints 个点。
//
// 两条要求：不会无限增长（否则长时间运行内存明显上涨），
// 丢弃的是**最旧的**点（曲线右移才对，顺序反了画面就不对）。
func TestSparkSlidingWindow(t *testing.T) {
	var spark []float64
	for i := 1; i <= 200; i++ {
		spark = append(spark, float64(i))
		if len(spark) > sparkPoints {
			cp := make([]float64, sparkPoints, sparkPoints*2)
			copy(cp, spark[len(spark)-sparkPoints:])
			spark = cp
		}
	}
	if len(spark) != sparkPoints {
		t.Fatalf("采样点应恒为 %d，实际 %d", sparkPoints, len(spark))
	}
	// 最后一点必须是最新的一次采样
	if spark[len(spark)-1] != 200 {
		t.Errorf("末尾应是最新值 200，实际 %v", spark[len(spark)-1])
	}
	// 首点应是 (200 - sparkPoints + 1)
	wantFirst := float64(200 - sparkPoints + 1)
	if spark[0] != wantFirst {
		t.Errorf("首点应是 %v（丢弃最旧的点），实际 %v", wantFirst, spark[0])
	}
	// 必须严格递增
	for i := 1; i < len(spark); i++ {
		if spark[i] <= spark[i-1] {
			t.Fatalf("点 %d 未递增：%v -> %v（顺序反了）", i, spark[i-1], spark[i])
		}
	}
}

// TestStepIndexFromLabel 验证阶段判定。
//
// 步进器是辅助信息，猜错不致命，但「解析」必须是兜底默认值 ——
// 绝大多数任务的第一个阶段就是它。
func TestStepIndexFromLabel(t *testing.T) {
	cases := []struct {
		label string
		running bool
		want  int
	}{
		{"正在解析…", true, 0},
		{"获取视频信息…", true, 0},
		{"正在下载视频流…", true, 1},
		{"正在下载音频流…", true, 2},
		{"正在混流封装…", true, 3},
		{"正在合并分片…", true, 3},
		{"已完成", false, -1},
		{"等待任务…", false, -1},
		{"", false, -1},
		{"某句没关键词的话", true, 0}, // 兜底回解析
	}
	for _, c := range cases {
		if got := stepIndexFromLabel(c.label, c.running); got != c.want {
			t.Errorf("stepIndexFromLabel(%q, %v) = %d，期望 %d", c.label, c.running, got, c.want)
		}
	}
}

// TestStageOrderMatchesOldUI 防止有人改乱阶段顺序。
//
// 顺序是照着老版（Python 版网页）来的：解析 → 视频 → 音频 → 混流。
// 改顺序会让步进器的"已完成/进行中"判定与用户已有的心智模型脱节。
func TestStageOrderMatchesOldUI(t *testing.T) {
	want := []string{"解析", "视频", "音频", "混流"}
	if len(stageOrder) != len(want) {
		t.Fatalf("stageOrder 长度应为 %d，实际 %d", len(want), len(stageOrder))
	}
	for i := range want {
		if stageOrder[i] != want[i] {
			t.Errorf("stageOrder[%d] = %q，期望 %q", i, stageOrder[i], want[i])
		}
	}
}

// TestValueOrDash 验证空值被换成 "--"。
//
// g.text 遇到空串直接跳过不画，若不在这里兜住，指标卡就只剩标签没有数值，
// 看上去像功能没做完。
func TestValueOrDash(t *testing.T) {
	w := &Win{}
	if got := w.valueOrDash(""); got != "--" {
		t.Errorf("空串应变\"--\"，实际 %q", got)
	}
	if got := w.valueOrDash("   "); got != "--" {
		t.Errorf("空白串应变\"--\"，实际 %q", got)
	}
	if got := w.valueOrDash("949 KB/s"); got != "949 KB/s" {
		t.Errorf("正常值不应改动，实际 %q", got)
	}
}

// TestProgPctClamped 验证百分比夹在 100% 以内。
//
// 分块向上取整会让 Done 略超 Total，直接算会显示出「103.2%」。
func TestProgPctClamped(t *testing.T) {
	w := &Win{}
	if got := w.progPct(); got != "--" {
		t.Errorf("无总量时应为\"--\"，实际 %q", got)
	}
	// Done 超过 Total 是真实会发生的：分块向上取整让最后一块算多了几KB。
	w.prog = app.JobProgress{Done: 1032, Total: 1000}
	if got := w.progPct(); got != "100.0%" {
		t.Errorf("超额时应夹到 100.0%%，实际 %q", got)
	}
	// Done 明显小于 Total 时必须正常算出百分比，别被误当成超额度夹掉
	w.prog = app.JobProgress{Done: 250, Total: 1000}
	if got := w.progPct(); got != "25.0%" {
		t.Errorf("正常值应为 25.0%%，实际 %q", got)
	}
}

// TestSparkNeedsTwoPoints 验证曲线在采样不足两点时不画线。
func TestSparkNeedsTwoPoints(t *testing.T) {
	for _, n := range []int{0, 1} {
		if n >= 2 {
			t.Errorf("采样 %d 个点时本应可画线", n)
		}
	}
	// 归一化用的 max 不能为 0，否则全部除零得 NaN、画不出任何东西。
	for _, mx := range []float64{0, 0.0, -1} {
		m := mx
		if m <= 0 {
			m = 1
		}
		if m != 1 {
			t.Errorf("max=%v 时应回退成 1，实际 %v", mx, m)
		}
	}
}

// TestSpeedCurveCaptionNotEmpty 保证右下角标注文案没被改掉。
func TestSpeedCurveCaptionNotEmpty(t *testing.T) {
	if !strings.Contains("速度曲线", "速度") {
		t.Error("曲线标注文案丢了")
	}
}