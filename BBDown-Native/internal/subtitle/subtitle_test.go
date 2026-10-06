package subtitle

import "testing"

const sampleJSON = `{
  "font_size": 0.4,
  "font_color": "#FFFFFF",
  "background_alpha": 0.5,
  "Stroke": "none",
  "body": [
    {"from": 0.0,    "to": 2.5,  "location": 2, "content": "第一句"},
    {"from": 2.5,    "to": 5.0,  "location": 2, "content": "第二句\n换行了"},
    {"from": 5.0,    "to": 5.0,  "location": 2, "content": "零时长，应被丢掉"},
    {"from": 6.0,    "to": 3.0,  "location": 2, "content": "时间倒挂，应被丢掉"},
    {"from": 8.0,    "to": 10.0, "location": 2, "content": "   "},
    {"from": -1.0,   "to": 12.0, "location": 2, "content": "负起点应夹到 0"},
    {"from": 3725.25,"to": 3726.5,"location": 2, "content": "一小时零两分"}
  ]
}`

func TestParseJSON(t *testing.T) {
	tr, err := ParseJSON([]byte(sampleJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 7 条原始数据里，「零时长」「时间倒挂」「全空白」三条应当被丢掉
	if len(tr.Cues) != 4 {
		t.Fatalf("期望 4 条，实际 %d 条：%+v", len(tr.Cues), tr.Cues)
	}
	if tr.Cues[0].Text != "第一句" || tr.Cues[0].From != 0 || tr.Cues[0].To != 2.5 {
		t.Errorf("第一条解析错：%+v", tr.Cues[0])
	}
	if tr.Cues[1].Text != "第二句\n换行了" {
		t.Errorf("正文换行应当保留：%q", tr.Cues[1].Text)
	}
	if tr.Cues[2].From != 0 {
		t.Errorf("负起点应当夹到 0，实际 %v", tr.Cues[2].From)
	}
}

func TestParseJSONKeepsRaw(t *testing.T) {
	tr, err := ParseJSON([]byte(sampleJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 原始 JSON 必须逐字节保留，这样写出去的 .json 就是服务端那份
	if string(tr.Raw()) != sampleJSON {
		t.Error("原始 JSON 没有被原样保留")
	}
}

func TestParseJSONWithBOM(t *testing.T) {
	withBOM := append([]byte{0xEF, 0xBB, 0xBF}, []byte(sampleJSON)...)
	if _, err := ParseJSON(withBOM); err != nil {
		t.Fatalf("带 BOM 的 JSON 应当能解析：%v", err)
	}
}

func TestParseJSONRejectsGarbage(t *testing.T) {
	if _, err := ParseJSON([]byte("<html>风控页</html>")); err == nil {
		t.Error("非 JSON 内容应当报错")
	}
}

func TestParseJSONEmptyBody(t *testing.T) {
	tr, err := ParseJSON([]byte(`{"body":[]}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(tr.Cues) != 0 {
		t.Errorf("空 body 不该产生条目，实际 %d 条", len(tr.Cues))
	}
	if got := string(tr.SRT()); got != "" {
		t.Errorf("空字幕的 SRT 应当为空，实际 %q", got)
	}
}

func TestSRTFormat(t *testing.T) {
	tr, err := ParseJSON([]byte(sampleJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := string(tr.SRT())
	want := "1\n00:00:00,000 --> 00:00:02,500\n第一句\n\n" +
		"2\n00:00:02,500 --> 00:00:05,000\n第二句\n换行了\n\n" +
		"3\n00:00:00,000 --> 00:00:12,000\n负起点应夹到 0\n\n" +
		"4\n01:02:05,250 --> 01:02:06,500\n一小时零两分\n\n"
	if got != want {
		t.Errorf("SRT 输出不符。\n实际:\n%q\n期望:\n%q", got, want)
	}
}

func TestSRTTimeFormat(t *testing.T) {
	cases := []struct {
		sec  float64
		want string
	}{
		{0, "00:00:00,000"},
		{1.5, "00:00:01,500"},
		{61.234, "00:01:01,234"},
		{3725.25, "01:02:05,250"},
		{2.5004, "00:00:02,500"}, // 毫秒四舍五入
		{-3, "00:00:00,000"},
	}
	for _, c := range cases {
		if got := srtTime(c.sec); got != c.want {
			t.Errorf("srtTime(%v) = %q，期望 %q", c.sec, got, c.want)
		}
	}
}

func TestLabel(t *testing.T) {
	cases := []struct {
		lang string
		ai   bool
		want string
	}{
		{"zh-CN", false, "zh-CN"},
		{"ai-zh", true, "ai-zh"},
		{"zh TW", false, "zh_TW"}, // 空格要换掉
		{"", false, "sub"},
		{"", true, "ai"},
	}
	for _, c := range cases {
		tr := &Track{Lang: c.lang, AI: c.ai}
		if got := tr.Label(); got != c.want {
			t.Errorf("Label(lang=%q ai=%v) = %q，期望 %q", c.lang, c.ai, got, c.want)
		}
	}
}
