package app

import "testing"

// TestLegacyDefaultCodecOrderMigration 锁住编码偏好迁移的行为。
//
// 背景：默认编码偏好曾是 HEVC 优先，产出物在部分播放器（如哔哩哔哩客户端）
// 里全黑。改成 AVC 优先后，存量配置里写着旧默认值的那份也得跟着变，
// 但**用户自己调过的顺序不能被动**。
func TestLegacyDefaultCodecOrderMigration(t *testing.T) {
	// 恰好是旧默认值（含大小写/空白差异）→ 迁移
	migrated := []string{
		"hevc,avc,av1",
		"HEVC,AVC,AV1",
		" hevc , avc , av1 ",
		"hevc,avc,av1,",
	}
	for _, s := range migrated {
		c := &Config{CodecOrder: s}
		c.normalize()
		if c.CodecOrder != DefaultCodecOrder {
			t.Errorf("normalize(%q).CodecOrder = %q，期望迁移成 %q",
				s, c.CodecOrder, DefaultCodecOrder)
		}
	}

	// 用户明确表达过偏好 / 顺序不同 → 保持不动
	keep := []string{
		"hevc",         // 只要 HEVC
		"avc",          // 只要 AVC
		"av1",          // 只要 AV1
		"hevc,av1",     // 自定义顺序
		"avc,av1,hevc", // 自定义顺序
	}
	for _, s := range keep {
		c := &Config{CodecOrder: s}
		c.normalize()
		if c.CodecOrder != s {
			t.Errorf("normalize(%q) 把用户的偏好改成了 %q，不该动它",
				s, c.CodecOrder)
		}
	}

	// 空值→ 默认值
	c := &Config{}
	c.normalize()
	if c.CodecOrder != DefaultCodecOrder {
		t.Errorf("空 CodecOrder = %q，期望 %q", c.CodecOrder, DefaultCodecOrder)
	}
}

// TestDefaultCodecOrderIsAVCFirst 保证默认是 AVC 打头。
// 这条断言写死在测试里，是为了以后有人「为了省体积」把 HEVC 挪回首位时
// 能立刻看到失败 —— 那次改动就是黑屏问题的来源，不该无声地发生。
func TestDefaultCodecOrderIsAVCFirst(t *testing.T) {
	got := (&Config{}).CodecList()
	if len(got) == 0 {
		t.Fatal("CodecList 为空")
	}
	if got[0] != "avc" {
		t.Fatalf("默认编码第一位是 %q，期望avc（HEVC 优先会导致部分播放器全黑）", got[0])
	}
}

// TestQualityPriority4K 锁住选 4K 时优先级把 120 排第一，
// 这样 SelectVideo 才会优先拿 4K 流而不是退回 1080P。
func TestQualityPriority4K(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Quality = 120
	p := cfg.QualityPriority()
	if len(p) == 0 || p[0] != 120 {
		t.Fatalf("选 4K 时优先级首位应是 120，实际 %v", p)
	}
	// 高画质要按从高到低兜底：4K 拿不到时退 1080P60 / 1080P，而不是先退到 480P
	seen := map[int]bool{}
	for _, q := range p {
		seen[q] = true
	}
	for _, want := range []int{116, 112, 80, 64} {
		if !seen[want] {
			t.Fatalf("4K 降级链缺 qn=%d：%v", want, p)
		}
	}
	// 自动模式仍返回空列表（交给 SelectVideo 拿最高可用）
	cfg.Quality = 0
	if got := cfg.QualityPriority(); len(got) != 0 {
		t.Fatalf("自动模式不该有画质优先级，实际 %v", got)
	}
}
