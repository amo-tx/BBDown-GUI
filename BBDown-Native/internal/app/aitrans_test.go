package app

import (
	"strings"
	"testing"

	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/subtitle"
)

func TestRenderTranscript(t *testing.T) {
	info := &bilibili.VideoInfo{
		Title: "示例视频", Owner: "阿莫", Bvid: "BV1GJ411x7h7",
		Pages: []bilibili.Page{{Index: 1, Title: "示例视频", Cid: 1, Duration: 214}},
	}
	page := info.Pages[0]
	track := &subtitle.Track{
		Lang: "ai-zh", LangDoc: "中文（自动生成）", AI: true,
		Cues: []subtitle.Cue{
			{From: 0, To: 2, Text: "第一句话。"},
			{From: 2, To: 4, Text: "第二句话"},
		},
	}
	got := string(renderTranscript(info, page, track, "sub", 2))
	for _, want := range []string{"《示例视频》", "BV1GJ411x7h7", "AI 字幕", "3:34", "第一句话。", "第二句话"} {
		if !strings.Contains(got, want) {
			t.Fatalf("文稿缺少 %q：\n%s", want, got)
		}
	}
	// 单P 稿件不应出现分P 行
	if strings.Contains(got, "分P") {
		t.Fatalf("单P 稿件不该出现分P 行：\n%s", got)
	}
}

func TestRenderTranscriptASR(t *testing.T) {
	info := &bilibili.VideoInfo{
		Title: "无字幕视频", Owner: "阿莫", Bvid: "BV1xx",
		Pages: []bilibili.Page{{Index: 1, Title: "无字幕视频", Duration: 61}},
	}
	cues := []subtitle.Cue{{From: 1.2, To: 3.4, Text: "本地转写的一句"}}
	got := string(RenderTranscript(info, info.Pages[0], cues, "asr"))
	for _, want := range []string{"本地 whisper 语音转写", "B 站无可用字幕", "本地转写的一句"} {
		if !strings.Contains(got, want) {
			t.Fatalf("ASR 文稿缺少 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "B 站 AI 字幕") {
		t.Fatalf("ASR 来源不该标成 B 站字幕：\n%s", got)
	}
}

func TestRenderTranscriptMultiPage(t *testing.T) {
	info := &bilibili.VideoInfo{
		Title: "多P", Owner: "up", Bvid: "BV1xx",
		Pages: []bilibili.Page{
			{Index: 1, Title: "上集", Duration: 60},
			{Index: 2, Title: "下集", Duration: 60},
		},
	}
	got := string(renderTranscript(info, info.Pages[1], &subtitle.Track{}, "sub", 0))
	if !strings.Contains(got, "分P 2：下集") {
		t.Fatalf("多P 应标注分P 行：\n%s", got)
	}
}

func TestRenderSummaryMD(t *testing.T) {
	info := &bilibili.VideoInfo{
		Title: "示例视频", Owner: "阿莫", Bvid: "BV1GJ411x7h7",
		Pages: []bilibili.Page{{Index: 1, Title: "示例视频", Duration: 3600}},
	}
	aiOut := "## 摘要\n\n这是模型写的一段话。\n\n## 大纲\n\n- **[0:30] 开场**：要点"
	got := string(renderSummaryMD(info, info.Pages[0], aiOut))
	for _, want := range []string{"# 示例视频 — AI 视频总结", "免费 AI 分析文稿", "## 摘要", "## 大纲", "**[0:30] 开场**"} {
		if !strings.Contains(got, want) {
			t.Fatalf("总结缺 %q：\n%s", want, got)
		}
	}
	// 模型输出应是主体，不应被二次加工掉
	if !strings.Contains(got, "这是模型写的一段话。") {
		t.Fatalf("模型输出丢失：\n%s", got)
	}
}

func TestTranscriptForSummary(t *testing.T) {
	cues := []subtitle.Cue{{From: 65, To: 67, Text: "第一句"}, {From: 130, To: 133, Text: "第二句"}}
	got := transcriptForSummary(cues)
	for _, want := range []string{"[1:05] 第一句", "[2:10] 第二句"} {
		if !strings.Contains(got, want) {
			t.Fatalf("总结文稿缺 %q：\n%s", want, got)
		}
	}
	// 超长截断要留标记且不超上限太多
	var long []subtitle.Cue
	for i := 0; i < 3000; i++ {
		long = append(long, subtitle.Cue{From: float64(i), Text: strings.Repeat("字", 20)})
	}
	got = transcriptForSummary(long)
	if !strings.Contains(got, "已截断") {
		t.Fatalf("超长文稿应标注截断")
	}
}

func TestBuildSummaryUserPrompt(t *testing.T) {
	info := &bilibili.VideoInfo{Title: "示例", Owner: "阿莫", Pages: []bilibili.Page{{Index: 1, Title: "示例"}}}
	got := buildSummaryUserPrompt(info, info.Pages[0], "[0:01] 你好")
	for _, want := range []string{"《示例》", "UP 主：阿莫", "[0:01] 你好"} {
		if !strings.Contains(got, want) {
			t.Fatalf("用户提示缺 %q：\n%s", want, got)
		}
	}
}

func TestPickSubtitleTrack(t *testing.T) {
	mk := func(lan string, ai bool) bilibili.SubtitleMeta {
		return bilibili.SubtitleMeta{Lan: lan, AI: ai}
	}
	// 优先真人中文
	list := []bilibili.SubtitleMeta{mk("en-US", false), mk("zh-CN", false), mk("ai-zh", true)}
	if got := pickSubtitleTrack(list); got.Lan != "zh-CN" {
		t.Fatalf("应优先真人中文，拿到 %s", got.Lan)
	}
	// 只有 AI 中文
	list = []bilibili.SubtitleMeta{mk("en-US", false), mk("ai-zh", true)}
	if got := pickSubtitleTrack(list); got.Lan != "ai-zh" {
		t.Fatalf("应取 AI 中文，拿到 %s", got.Lan)
	}
	// 都没有就取第一条
	list = []bilibili.SubtitleMeta{mk("en-US", false), mk("ko", false)}
	if got := pickSubtitleTrack(list); got.Lan != "en-US" {
		t.Fatalf("兜底应取第一条，拿到 %s", got.Lan)
	}
}

func TestAppendUTF8BOM(t *testing.T) {
	if got := appendUTF8BOM([]byte("abc")); len(got) != 6 || got[0] != 0xEF {
		t.Fatalf("应补 BOM：% x", got)
	}
	if got := appendUTF8BOM([]byte{0xEF, 0xBB, 0xBF, 'x'}); len(got) != 4 {
		t.Fatalf("已有 BOM 不该重复补：% x", got)
	}
}

func TestMMSS(t *testing.T) {
	cases := map[int]string{0: "0:00", 65: "1:05", 3600: "1:00:00", 3725: "1:02:05", -3: "0:00"}
	for sec, want := range cases {
		if got := mmss(sec); got != want {
			t.Fatalf("mmss(%d)=%s，要 %s", sec, got, want)
		}
	}
}
