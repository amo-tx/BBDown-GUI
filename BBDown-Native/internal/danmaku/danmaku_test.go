package danmaku

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 解析

const sampleXML = `<?xml version="1.0" encoding="UTF-8"?><i>
<chatserver>chat.bilibili.com</chatserver>
<chatid>137649199</chatid>
<maxlimit>1000</maxlimit>
<d p="0.60100,4,25,15138834,1604742895,0,9e5adeaa,40685126698926083,10">底部红字</d>
<d p="1.20,1,25,16777215,1604742896,0,aaaa,1001,5">滚动白字</d>
<d p="2.00,5,36,16711680,1604742897,0,bbbb,1002,5">顶部大红字</d>
<d p="3.00,1,18,65280,1604742898,0,cccc,1003,5">小绿字</d>
<d p="4.00,7,25,16777215,1604742899,0,dddd,1004,5">[0,0,"1-1",4,"高级弹幕",0,0,0,0,500,0,0,0,0,500,0,1,"黑体",1]</d>
<d p="5.00,1,25,16777215,1604742900,0,eeee,1005,5">第一行/n第二行</d>
<d p="6.00,1,25,16777215,1604742901,0,ffff,1006,5">实体 &amp; 与 &lt;尖括号&gt;</d>
<d p="坏数据">这一条应该被跳过</d>
<d p="7.00,1">字段不够，跳过</d>
<d p="8.00,1,25,99999999,1,0,x,1,5">颜色越界当白色</d>
</i>`

func TestParseXMLFields(t *testing.T) {
	items, err := ParseXML([]byte(sampleXML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 合法条目 8 条：坏数据 + 字段不够的共 2 条被丢
	if len(items) != 8 {
		t.Fatalf("期望 8 条，实际 %d 条：%+v", len(items), items)
	}

	first := items[0]
	if first.Time != 0.601 {
		t.Errorf("时间解析错：%v", first.Time)
	}
	if first.Mode != ModeBottom {
		t.Errorf("模式解析错：%v", first.Mode)
	}
	// 15138834 = 0xE70012
	if first.Color != 0xE70012 {
		t.Errorf("颜色解析错：0x%06X", first.Color)
	}
	if first.Text != "底部红字" {
		t.Errorf("正文解析错：%q", first.Text)
	}

	if items[2].FontSize != 36 {
		t.Errorf("字号解析错：%d", items[2].FontSize)
	}
}

func TestParseXMLDropsBadRows(t *testing.T) {
	items, err := ParseXML([]byte(sampleXML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, it := range items {
		if strings.Contains(it.Text, "应该被跳过") || strings.Contains(it.Text, "字段不够") {
			t.Errorf("坏数据没有被跳过：%+v", it)
		}
	}
}

func TestParseXMLNewlineMarker(t *testing.T) {
	items, err := ParseXML([]byte(sampleXML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var got string
	for _, it := range items {
		if strings.HasPrefix(it.Text, "第一行") {
			got = it.Text
		}
	}
	if got != "第一行\n第二行" {
		t.Errorf("/n 没有还原成换行：%q", got)
	}
}

func TestParseXMLEntities(t *testing.T) {
	items, err := ParseXML([]byte(sampleXML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	var got string
	for _, it := range items {
		if strings.Contains(it.Text, "实体") {
			got = it.Text
		}
	}
	if got != "实体 & 与 <尖括号>" {
		t.Errorf("HTML 实体没有还原：%q", got)
	}
}

func TestParseXMLColorOutOfRangeFallsBackToWhite(t *testing.T) {
	items, err := ParseXML([]byte(sampleXML))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for _, it := range items {
		if strings.Contains(it.Text, "颜色越界") {
			if it.Color != 0xFFFFFF {
				t.Errorf("越界颜色应兜底为白色，实际 0x%06X", it.Color)
			}
			return
		}
	}
	t.Fatal("没找到颜色越界那条")
}

func TestParseXMLRejectsGarbage(t *testing.T) {
	if _, err := ParseXML([]byte("这不是 XML")); err == nil {
		t.Error("非 XML 内容应当报错")
	}
}

// ---------------------------------------------------------------------------
// 字号与坐标换算

// 1080p 下：字号 = 1080/27 = 40，行高 = 50，行数 = int(1080/50) = 21。
const (
	testW = 1920
	testH = 1080
)

func render(t *testing.T, items []Item) string {
	t.Helper()
	return string(RenderASS(items, Options{PlayResX: testW, PlayResY: testH}))
}

func TestASSHeaderHasPlayRes(t *testing.T) {
	out := render(t, nil)
	for _, want := range []string{
		"PlayResX: 1920",
		"PlayResY: 1080",
		"WrapStyle: 2",
		"[Events]",
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ASS 头里缺少 %q", want)
		}
	}
	// 标准字号 25 → 1080/27 ≈ 40
	if !strings.Contains(out, "Style: Danmaku,Microsoft YaHei,40,") {
		t.Errorf("样式行的字号不是 40：\n%s", firstStyleLine(out))
	}
}

func firstStyleLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "Style:") {
			return line
		}
	}
	return "(没有样式行)"
}

// 回归：滚动弹幕不能同时出现 \pos 和 \move —— 两者同时给出时 ASS 行为不确定。
func TestASSScrollUsesMoveNotPos(t *testing.T) {
	out := render(t, []Item{{Time: 1, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "滚动"}})
	line := dialogueOf(t, out, "滚动")
	if !strings.Contains(line, `\move(`) {
		t.Errorf("滚动弹幕没有 \\move：%s", line)
	}
	if strings.Contains(line, `\pos(`) {
		t.Errorf("滚动弹幕不该有 \\pos：%s", line)
	}
	if !strings.Contains(line, `\an7`) {
		t.Errorf("滚动弹幕应当用 \\an7（左上角锚点）：%s", line)
	}
}

func TestASSFixedAnchors(t *testing.T) {
	out := render(t, []Item{
		{Time: 1, Mode: ModeTop, FontSize: 25, Color: 0xFFFFFF, Text: "顶部"},
		{Time: 1, Mode: ModeBottom, FontSize: 25, Color: 0xFFFFFF, Text: "底部"},
	})
	top := dialogueOf(t, out, "顶部")
	if !strings.Contains(top, `\an8`) || !strings.Contains(top, `\pos(`) {
		t.Errorf("顶部固定弹幕应当用 \\an8\\pos：%s", top)
	}
	bottom := dialogueOf(t, out, "底部")
	if !strings.Contains(bottom, `\an2`) || !strings.Contains(bottom, `\pos(`) {
		t.Errorf("底部固定弹幕应当用 \\an2\\pos：%s", bottom)
	}
	for _, l := range []string{top, bottom} {
		if strings.Contains(l, `\move(`) {
			t.Errorf("固定弹幕不该有 \\move：%s", l)
		}
	}
}

// 回归：底部固定弹幕必须贴近画面底部。
// 早先的实现在「行号自上而下」的表里从第 0 行开始分配，于是底部弹幕被画到了屏幕顶上。
func TestASSBottomFixedSitsAtBottom(t *testing.T) {
	out := render(t, []Item{{Time: 0, Mode: ModeBottom, FontSize: 25, Color: 0xFFFFFF, Text: "底部"}})
	y := posY(t, dialogueOf(t, out, "底部"))
	// 行高 50、21 行 → 最底一行的下边缘 = 21*50 = 1050，几乎贴住 1080
	if y < 1000 {
		t.Errorf("底部固定弹幕应当贴近画面底部（y≈1050），实际 y=%.1f", y)
	}
}

// 底部固定弹幕多起来时要往上堆，而不是往下溢出。
func TestASSBottomFixedStacksUpward(t *testing.T) {
	var items []Item
	for i := 0; i < 3; i++ {
		items = append(items, Item{
			Time: 0, Mode: ModeBottom, FontSize: 25, Color: 0xFFFFFF,
			Text: strings.Repeat("底", i+1),
		})
	}
	out := render(t, items)
	y1 := posY(t, dialogueOf(t, out, "底"))
	y2 := posY(t, dialogueOf(t, out, "底底"))
	y3 := posY(t, dialogueOf(t, out, "底底底"))
	if !(y1 > y2 && y2 > y3) {
		t.Errorf("底部弹幕应当逐条往上堆，实际 y = %.1f / %.1f / %.1f", y1, y2, y3)
	}
}

func TestASSColorIsBGR(t *testing.T) {
	// 纯红 0xFF0000 → ASS 的 &HBBGGRR& 应当是 &H0000FF&
	out := render(t, []Item{{Time: 0, Mode: ModeTop, FontSize: 25, Color: 0xFF0000, Text: "红"}})
	if !strings.Contains(out, `\c&H0000FF&`) {
		t.Errorf("颜色没有转成 BGR 序：%s", dialogueOf(t, out, "红"))
	}
}

func TestASSEscapesBraces(t *testing.T) {
	out := render(t, []Item{{Time: 0, Mode: ModeTop, FontSize: 25, Color: 0xFFFFFF, Text: "{危险} \\反斜杠"}})
	if strings.Contains(out, "{危险}") {
		t.Error("裸花括号没有被转义，会被 ASS 当成特效块")
	}
	if !strings.Contains(out, "｛危险｝") {
		t.Errorf("花括号应换成全角：%s", dialogueOf(t, out, "危险"))
	}
	if strings.Contains(out, `\反斜杠`) {
		t.Error("反斜杠没有被转义")
	}
}

func TestASSMultilineUsesHardBreak(t *testing.T) {
	out := render(t, []Item{{Time: 0, Mode: ModeTop, FontSize: 25, Color: 0xFFFFFF, Text: "上\n下"}})
	line := dialogueOf(t, out, "上")
	if !strings.Contains(line, `上\N下`) {
		t.Errorf("换行应当转成 \\N：%s", line)
	}
}

func TestASSSkipsAdvancedByDefault(t *testing.T) {
	adv := []Item{
		{Time: 0, Mode: ModeAdvanced, FontSize: 25, Color: 0xFFFFFF, Text: `[0,0,"1-1",4,"x"]`},
		{Time: 1, Mode: ModeCode, FontSize: 25, Color: 0xFFFFFF, Text: "code"},
		{Time: 2, Mode: ModeBAS, FontSize: 25, Color: 0xFFFFFF, Text: "bas"},
	}
	out := render(t, adv)
	if n := strings.Count(out, "Dialogue:"); n != 0 {
		t.Errorf("默认应当跳过高级/代码弹幕，实际产出了 %d 行", n)
	}

	out2 := string(RenderASS(adv, Options{PlayResX: testW, PlayResY: testH, IncludeAdvanced: true}))
	if n := strings.Count(out2, "Dialogue:"); n != 3 {
		t.Errorf("开启 IncludeAdvanced 后应当产出 3 行，实际 %d 行", n)
	}
}

// ---------------------------------------------------------------------------
// 布局

// 同一时刻的两条短弹幕必须分到不同行，否则会叠在一起。
func TestASSSameTimeGoesToDifferentRows(t *testing.T) {
	items := []Item{
		{Time: 1, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "AAA"},
		{Time: 1, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "BBB"},
	}
	out := render(t, items)
	ya := moveY(t, dialogueOf(t, out, "AAA"))
	yb := moveY(t, dialogueOf(t, out, "BBB"))
	if ya == yb {
		t.Errorf("两条同秒弹幕落到了同一行（y=%.1f），会叠字", ya)
	}
	if ya != 25 || yb != 75 {
		t.Errorf("期望落在第 1、2 行（y=25 / 75），实际 %.1f / %.1f", ya, yb)
	}
}

// 时间错开足够远时，应当能复用第一行（否则行数会被白白浪费）。
func TestASSReusesRowAfterGap(t *testing.T) {
	items := []Item{
		{Time: 0, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "AA"},
		{Time: 30, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "BB"},
	}
	out := render(t, items)
	if ya, yb := moveY(t, dialogueOf(t, out, "AA")), moveY(t, dialogueOf(t, out, "BB")); ya != yb {
		t.Errorf("间隔 30 秒后应当复用第一行，实际 %.1f / %.1f", ya, yb)
	}
}

// 同一时刻塞满所有行之后，多出来的弹幕应当被丢弃，而不是叠上去。
func TestASSDropsWhenFull(t *testing.T) {
	var items []Item
	// 21 行，每行一条；再多给 5 条必然放不下
	for i := 0; i < 26; i++ {
		items = append(items, Item{
			Time: 1, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF,
			Text: strings.Repeat("宽", 20),
		})
	}
	out := render(t, items)
	n := strings.Count(out, "Dialogue:")
	if n > 21 {
		t.Errorf("行数上限是 21，却产出了 %d 行", n)
	}
	if n == 0 {
		t.Error("一条都没排上，布局算法有问题")
	}
}

func TestASSMaxLines(t *testing.T) {
	out := string(RenderASS(
		[]Item{{Time: 0, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "x"}},
		Options{PlayResX: testW, PlayResY: testH, MaxLines: 3}))
	if !strings.Contains(out, `\move(1920,25.0`) {
		t.Errorf("MaxLines 不该影响第一行的位置：%s", dialogueOf(t, out, "x"))
	}
}

func TestASSFontScale(t *testing.T) {
	out := string(RenderASS(
		[]Item{{Time: 0, Mode: ModeTop, FontSize: 25, Color: 0xFFFFFF, Text: "x"}},
		Options{PlayResX: testW, PlayResY: testH, FontScale: 2}))
	if !strings.Contains(out, `\fs80`) {
		t.Errorf("FontScale=2 时 25 号字应当变成 80：%s", dialogueOf(t, out, "x"))
	}
}

func TestASSBigFontSpansMultipleRows(t *testing.T) {
	// 36 号字 → 40 * 36/25 = 57.6，行高 50 → 占 2 行
	items := []Item{
		{Time: 1, Mode: ModeScrollRight, FontSize: 36, Color: 0xFFFFFF, Text: "大字"},
		{Time: 1, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "小字"},
	}
	out := render(t, items)
	yBig := moveY(t, dialogueOf(t, out, "大字"))
	ySmall := moveY(t, dialogueOf(t, out, "小字"))
	if ySmall <= yBig {
		t.Errorf("大字占两行，小字应当被挤到第 3 行（y=125），实际 big=%.1f small=%.1f", yBig, ySmall)
	}
}

func TestASSWrongModeFallsBackToScroll(t *testing.T) {
	// 模式 2/3/6 都是逆向滚动，目前按普通滚动处理，不能丢内容
	for _, m := range []Mode{ModeScrollLeft, ModeScrollTop, ModeReverse} {
		out := render(t, []Item{{Time: 0, Mode: m, FontSize: 25, Color: 0xFFFFFF, Text: "逆向"}})
		if !strings.Contains(out, `\move(`) {
			t.Errorf("模式 %d 应当降级成滚动弹幕", m)
		}
	}
}

// ---------------------------------------------------------------------------
// 时间格式

func TestASSTimeFormat(t *testing.T) {
	cases := []struct {
		sec  float64
		want string
	}{
		{0, "0:00:00.00"},
		{1.5, "0:00:01.50"},
		{61.23, "0:01:01.23"},
		{3661.5, "1:01:01.50"},
		{0.601, "0:00:00.60"},
		{-5, "0:00:00.00"}, // 负数兜底成 0
	}
	for _, c := range cases {
		if got := assTime(c.sec); got != c.want {
			t.Errorf("assTime(%v) = %q，期望 %q", c.sec, got, c.want)
		}
	}
}

func TestASSSortedByTime(t *testing.T) {
	out := render(t, []Item{
		{Time: 9, Mode: ModeTop, FontSize: 25, Color: 0xFFFFFF, Text: "晚"},
		{Time: 2, Mode: ModeTop, FontSize: 25, Color: 0xFFFFFF, Text: "早"},
	})
	if strings.Index(out, "早") > strings.Index(out, "晚") {
		t.Error("输出没有按时间排序")
	}
}

func TestRenderEmptyIsStillValidASS(t *testing.T) {
	out := render(t, nil)
	if !strings.Contains(out, "[Script Info]") || !strings.Contains(out, "[Events]") {
		t.Error("空弹幕也应当产出一份合法的 ASS 骨架")
	}
	if strings.Contains(out, "Dialogue:") {
		t.Error("空弹幕不该有 Dialogue 行")
	}
}

// ---------------------------------------------------------------------------
// ASS 结构自检
//
// ASS 是「字段用逗号分隔、最后一个字段可以含逗号」的老式格式，
// 多一个少一个字段都会让整份字幕在播放器里静默失效，所以结构必须锁死。

func TestASSStructureIsWellFormed(t *testing.T) {
	items := []Item{
		{Time: 0, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "带,逗号,的正文"},
		{Time: 1, Mode: ModeTop, FontSize: 25, Color: 0xFF0000, Text: "顶部"},
		{Time: 2, Mode: ModeBottom, FontSize: 36, Color: 0x00FF00, Text: "底部"},
	}
	out := render(t, items)

	styleFormat, eventFormat := parseFormats(t, out)

	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "Style: "):
			// 第 2 个 "Style: " 之后的字段数应当与 Format 一致
			if n := len(strings.Split(strings.TrimPrefix(line, "Style: "), ",")); n != len(styleFormat) {
				t.Errorf("Style 行有 %d 个字段，Format 声明了 %d 个：%s", n, len(styleFormat), line)
			}
		case strings.HasPrefix(line, "Dialogue: "):
			// 最后一个字段（Text）允许含逗号，所以用 SplitN
			parts := strings.SplitN(strings.TrimPrefix(line, "Dialogue: "), ",", len(eventFormat))
			if len(parts) != len(eventFormat) {
				t.Errorf("Dialogue 行有 %d 个字段，Format 声明了 %d 个：%s", len(parts), len(eventFormat), line)
			}
			if !strings.HasPrefix(parts[len(parts)-1], "{") {
				t.Errorf("Dialogue 的最后一个字段应当是特效块开头的正文：%s", line)
			}
		}
	}
}

// parseFormats 取出 [V4+ Styles] 与 [Events] 两段声明的字段列表。
func parseFormats(t *testing.T, out string) (style, event []string) {
	t.Helper()
	section := ""
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "[") {
			section = line
			continue
		}
		if !strings.HasPrefix(line, "Format: ") {
			continue
		}
		fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "Format: ")), ", ")
		switch section {
		case "[V4+ Styles]":
			style = fields
		case "[Events]":
			event = fields
		}
	}
	if len(style) == 0 || len(event) == 0 {
		t.Fatalf("没有解析到 Format 行（style=%d event=%d）", len(style), len(event))
	}
	return style, event
}

// 每条 Dialogue 的时间轴必须单调（结束晚于开始）。
func TestASSTimeAxisMonotonic(t *testing.T) {
	items := []Item{
		{Time: 0, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: "短"},
		{Time: 1, Mode: ModeScrollRight, FontSize: 25, Color: 0xFFFFFF, Text: strings.Repeat("长", 40)},
		{Time: 2, Mode: ModeTop, FontSize: 25, Color: 0xFFFFFF, Text: "顶部"},
	}
	out := render(t, items)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "Dialogue: ") {
			continue
		}
		f := strings.SplitN(strings.TrimPrefix(line, "Dialogue: "), ",", 4)
		start, err1 := parseASSTime(f[1])
		end, err2 := parseASSTime(f[2])
		if err1 != nil || err2 != nil {
			t.Fatalf("时间解析失败：%s", line)
		}
		if end <= start {
			t.Errorf("结束时间没有晚于开始时间：%s", line)
		}
	}
}

// parseASSTime 是 assTime 的逆运算，只用于测试。
func parseASSTime(s string) (float64, error) {
	var h, m int
	var sec float64
	if _, err := fmt.Sscanf(s, "%d:%d:%f", &h, &m, &sec); err != nil {
		return 0, err
	}
	return float64(h)*3600 + float64(m)*60 + sec, nil
}

// ---------------------------------------------------------------------------
// 辅助

// dialogueOf 取出包含 needle 的那一行 Dialogue。
func dialogueOf(t *testing.T, out, needle string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Dialogue:") && strings.Contains(line, needle) {
			return line
		}
	}
	t.Fatalf("输出里找不到含 %q 的 Dialogue 行：\n%s", needle, out)
	return ""
}

// posY 从 \pos(x,y) 里取出 y。
func posY(t *testing.T, line string) float64 {
	t.Helper()
	i := strings.Index(line, `\pos(`)
	if i < 0 {
		t.Fatalf("这一行没有 \\pos：%s", line)
	}
	rest := line[i+len(`\pos(`):]
	j := strings.IndexByte(rest, ')')
	if j < 0 {
		t.Fatalf("\\pos 括号不闭合：%s", line)
	}
	parts := strings.Split(rest[:j], ",")
	if len(parts) != 2 {
		t.Fatalf("\\pos 参数不是 2 个：%s", rest[:j])
	}
	y, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		t.Fatalf("解析 y 失败：%v", err)
	}
	return y
}

// moveY 从 \move(x1,y1,x2,y2) 里取出起始 y。
func moveY(t *testing.T, line string) float64 {
	t.Helper()
	i := strings.Index(line, `\move(`)
	if i < 0 {
		t.Fatalf("这一行没有 \\move：%s", line)
	}
	rest := line[i+len(`\move(`):]
	j := strings.IndexByte(rest, ')')
	if j < 0 {
		t.Fatalf("\\move 括号不闭合：%s", line)
	}
	parts := strings.Split(rest[:j], ",")
	if len(parts) != 4 {
		t.Fatalf("\\move 参数不是 4 个：%s", rest[:j])
	}
	y, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		t.Fatalf("解析 y 失败：%v", err)
	}
	return y
}
