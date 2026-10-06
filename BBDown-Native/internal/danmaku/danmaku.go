// Package danmaku 解析 B 站弹幕 XML，并渲染成可直接播放的 ASS 字幕。
//
// 为什么自己生成 ASS：本项目不依赖 ffmpeg，而弹幕 XML 光有文件是不能跟着
// 视频播放的。ASS 是纯文本格式，播放器（PotPlayer / mpv / VLC / MPC）会自动
// 加载与视频同名的 .ass，所以「<标题>.mp4」+「<标题>.ass」放一起就有效果了。
//
// 本包**不碰网络**，只做「XML → 弹幕列表 → ASS 文本」的纯变换，方便单测。
package danmaku

import (
	"encoding/xml"
	"fmt"
	"html"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Mode 是弹幕的显示方式，取值直接对应 B 站 p 属性里的第 2 个字段。
type Mode int

const (
	ModeScrollRight Mode = 1 // 普通滚动（右 → 左），最常见
	ModeScrollLeft  Mode = 2 // 逆向滚动
	ModeScrollTop   Mode = 3 // 逆向滚动（同上，B 站历史遗留）
	ModeBottom      Mode = 4 // 底部固定
	ModeTop         Mode = 5 // 顶部固定
	ModeReverse     Mode = 6 // 逆向滚动
	ModeAdvanced    Mode = 7 // 高级弹幕（JSON 定位），需要专门的渲染器
	ModeCode        Mode = 8 // 代码弹幕（BAS）
	ModeBAS         Mode = 9 // BAS 脚本弹幕
)

// Item 是一条弹幕。
type Item struct {
	Time     float64 // 出现时间（秒）
	Mode     Mode    // 显示方式
	FontSize int     // 18 = 小，25 = 标准，36 = 大
	Color    uint32  // 0xRRGGBB
	Text     string  // 正文（已把 /n 还原成换行）
}

// rawDoc 对应弹幕 XML 的顶层结构。
//
// 完整形态长这样（只关心 <d>）：
//
//	<i><chatserver>…</chatserver><chatid>…</chatid>
//	   <d p="0.60100,4,25,15138834,1604742895,0,9e5adeaa,4068…,10">正文</d>
//	   …
//	</i>
type rawDoc struct {
	D []rawItem `xml:"d"`
}

type rawItem struct {
	P    string `xml:"p,attr"`
	Text string `xml:",chardata"`
}

// ParseXML 把弹幕 XML 解析成弹幕列表。
//
// 对残缺数据刻意宽容：单条弹幕字段不够或数值非法就跳过它，
// 不让一条坏数据毁掉整份几十万条的弹幕。
func ParseXML(data []byte) ([]Item, error) {
	var doc rawDoc
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("弹幕 XML 解析失败: %w", err)
	}

	out := make([]Item, 0, len(doc.D))
	for _, d := range doc.D {
		if it, ok := parseItem(d); ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// parseItem 解析单条弹幕的 p 属性。
//
// p 的字段依次是：
//
//	0 时间(秒，浮点)
//	1 模式
//	2 字号
//	3 颜色(十进制 RGB)
//	4 发送时间戳
//	5 弹幕池（0 普通 / 1 字幕 / 2 特殊）
//	6 发送者 uid 的散列
//	7 弹幕 id
//	8 权重
func parseItem(d rawItem) (Item, bool) {
	parts := strings.Split(d.P, ",")
	if len(parts) < 4 {
		return Item{}, false
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil || sec < 0 {
		return Item{}, false
	}
	mode, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return Item{}, false
	}
	size, err := strconv.Atoi(strings.TrimSpace(parts[2]))
	if err != nil || size <= 0 {
		size = 25
	}
	// 颜色是十进制 0xRRGGBB；超范围就当白色。
	var color uint32 = 0xFFFFFF
	if v, err := strconv.ParseUint(strings.TrimSpace(parts[3]), 10, 32); err == nil && v <= 0xFFFFFF {
		color = uint32(v)
	}

	text := cleanText(d.Text)
	if text == "" {
		return Item{}, false
	}
	return Item{
		Time:     sec,
		Mode:     Mode(mode),
		FontSize: size,
		Color:    color,
		Text:     text,
	}, true
}

// cleanText 规整弹幕正文。
//
// B 站用 "/n" 表示换行（和 JSON 里的 "\n" 写法无关，就是字面两个字符），
// 这里统一转成真换行，渲染时再变成 ASS 的 \N。
func cleanText(s string) string {
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "/n", "\n")
	// 去掉零宽字符：它们在 ASS 里会渲染成方块。
	s = strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\ufeff', '\u200e', '\u200f', '\u2060':
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// 尺寸与节奏常量，取值以「接近 B 站播放器观感」为准。
const (
	// StandardSize 是 B 站「标准」字号的数值，ASS 字号以它为基准换算。
	StandardSize = 25
	// CanvasPerStandard 是一屏能放下多少个「标准字号」的行。
	// 1080p 下约 27 行，与 B 站实际行数接近。
	CanvasPerStandard = 27.0
	// ScrollSeconds 是一条弹幕走完**一个屏宽**所需的时间。
	// 注意 B 站是「同速度」而非「同时长」：宽弹幕在屏上的时间更长。
	ScrollSeconds = 8.0
	// FixedSeconds 是顶部/底部固定弹幕的停留时间。
	FixedSeconds = 4.0
	// outlineRatio 是描边宽度占画面高度的比例（1080p 下 2px）。
	outlineRatio = 2.0 / 1080.0
)

// Options 控制 ASS 的生成。
type Options struct {
	PlayResX int // 视频宽，用来算滚动距离
	PlayResY int // 视频高，用来算字号与行高
	FontName string
	// FontScale 是字号的整体倍率，1 表示按 B 站观感还原。
	FontScale float64
	// MaxLines 限制可用行数，0 表示按画面高度自动算。
	// 弹幕太密时限制行数能避免整屏被糊住。
	MaxLines int
	// IncludeAdvanced 为真时把高级/代码弹幕降级成普通滚动弹幕。
	// 默认为假：那些弹幕的正文是定位 JSON，硬塞进去只会看到乱码。
	IncludeAdvanced bool
}

func (o Options) withDefaults() Options {
	if o.PlayResX <= 0 {
		o.PlayResX = 1920
	}
	if o.PlayResY <= 0 {
		o.PlayResY = 1080
	}
	if o.FontName == "" {
		o.FontName = "Microsoft YaHei"
	}
	if o.FontScale <= 0 {
		o.FontScale = 1
	}
	return o
}

// RenderASS 把弹幕渲染成 ASS 字幕文本。
func RenderASS(items []Item, opt Options) []byte {
	out, _ := RenderASSStats(items, opt)
	return out
}

// RenderASSStats 与 RenderASS 相同，额外返回**实际画出来**的条数。
//
// 之所以要这个数：同一个瞬间挤进来的弹幕远多于屏幕能放下的行数，
// 多出来的会被丢掉（B 站自己也会，只是它选择叠着盖上去）。
// 把「解析到多少 / 实际画出多少」都报给用户，才不会让人以为弹幕丢了。
func RenderASSStats(items []Item, opt Options) ([]byte, int) {
	opt = opt.withDefaults()

	baseSize := float64(opt.PlayResY) / CanvasPerStandard * opt.FontScale
	outline := math.Max(1, float64(opt.PlayResY)*outlineRatio)

	lay := newLayout(opt, baseSize)

	// 按时间排序：布局算法依赖「时间递增」这个前提。
	sorted := make([]Item, len(items))
	copy(sorted, items)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time < sorted[j].Time })

	var b strings.Builder
	writeHeader(&b, opt, baseSize, outline)

	placed := 0
	for _, it := range sorted {
		if isExotic(it.Mode) && !opt.IncludeAdvanced {
			continue
		}
		if line := lay.place(it); line != "" {
			b.WriteString(line)
			b.WriteByte('\n')
			placed++
		}
	}
	return []byte(b.String()), placed
}

// isExotic 判断是不是需要专门渲染器的弹幕类型。
func isExotic(m Mode) bool {
	return m == ModeAdvanced || m == ModeCode || m == ModeBAS
}

func writeHeader(b *strings.Builder, opt Options, baseSize, outline float64) {
	b.WriteString("[Script Info]\n")
	b.WriteString("; 由 BBDown 原生版生成\n")
	b.WriteString("ScriptType: v4.00+\n")
	b.WriteString("Collisions: Normal\n")
	// WrapStyle 2 = 不自动换行。弹幕必须整行显示，换行会把算好的宽度全打乱。
	b.WriteString("WrapStyle: 2\n")
	b.WriteString("ScaledBorderAndShadow: yes\n")
	fmt.Fprintf(b, "PlayResX: %d\n", opt.PlayResX)
	fmt.Fprintf(b, "PlayResY: %d\n", opt.PlayResY)
	b.WriteString("Timer: 100.0000\n\n")

	b.WriteString("[V4+ Styles]\n")
	b.WriteString("Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour," +
		" Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow," +
		" Alignment, MarginL, MarginR, MarginV, Encoding\n")
	// Alignment 7 = 左上角对齐。滚动弹幕靠这个把 \move 的坐标当左上角用；
	// 固定弹幕在事件里用 \an2 / \an8 覆盖掉。
	fmt.Fprintf(b, "Style: Danmaku,%s,%.0f,&H00FFFFFF,&H00FFFFFF,&H00000000,&H00000000,"+
		"0,0,0,0,100,100,0,0,1,%.1f,1,7,0,0,0,1\n\n", opt.FontName, baseSize, outline)

	b.WriteString("[Events]\n")
	b.WriteString("Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n")
}

// ---------------------------------------------------------------------------
// 分行布局

// 三种定位方式。
const (
	scrollMode = iota
	topMode
	bottomMode
)

// layout 负责给弹幕分行，避免同屏叠字。
//
// 用一张「行 → 该行下次可用的时间」表做贪心分配，这是弹幕渲染器的标准做法：
//   - 滚动弹幕：同一行里，上一条必须**完全进入**屏幕之后才能放新的。又因为
//     同速度不会追尾，只需判断「进入完成」这一个条件。
//   - 固定弹幕：整段停留时间都占着行。
//
// 顶部与底部固定各自从两端往中间排，互不干扰。
type layout struct {
	opt      Options
	baseSize float64
	outline  float64

	rowHeight float64 // 一行的基准高度
	rows      int     // 可用行数

	scrollFree []float64 // 下标 = 行号（自上而下）
	topFree    []float64
	bottomFree []float64
}

func newLayout(opt Options, baseSize float64) *layout {
	rowHeight := baseSize * 1.25
	rows := int(math.Floor(float64(opt.PlayResY) / rowHeight))
	if rows < 1 {
		rows = 1
	}
	if opt.MaxLines > 0 && rows > opt.MaxLines {
		rows = opt.MaxLines
	}
	return &layout{
		opt:        opt,
		baseSize:   baseSize,
		outline:    math.Max(1, float64(opt.PlayResY)*outlineRatio),
		rowHeight:  rowHeight,
		rows:       rows,
		scrollFree: make([]float64, rows),
		topFree:    make([]float64, rows),
		bottomFree: make([]float64, rows),
	}
}

// place 给一条弹幕分配位置，返回一行 Dialogue；放不下就返回空串。
func (l *layout) place(it Item) string {
	fs := l.fontSizeFor(it.FontSize)
	width := l.textWidth(it.Text, fs)
	height := fs * 1.25
	need := l.span(height)

	switch it.Mode {
	case ModeBottom:
		return l.placeFixed(it, fs, need, false)
	case ModeTop:
		return l.placeFixed(it, fs, need, true)
	case ModeScrollLeft, ModeScrollTop, ModeReverse:
		// 逆向滚动在 ASS 里没有原生支持（\move 只能是单向直线），
		// 按普通滚动处理：位置会不同，但至少不会丢内容。
		return l.placeScroll(it, fs, width, need)
	default:
		return l.placeScroll(it, fs, width, need)
	}
}

// fontSizeFor 把 B 站字号换算成 ASS 字号。
func (l *layout) fontSizeFor(size int) float64 {
	if size <= 0 {
		size = StandardSize
	}
	return l.baseSize * float64(size) / StandardSize
}

// span 返回一条弹幕要占几行（大字号占多行）。
func (l *layout) span(height float64) int {
	n := int(math.Ceil(height / l.rowHeight))
	if n < 1 {
		n = 1
	}
	if n > l.rows {
		n = l.rows
	}
	return n
}

// placeScroll 放一条滚动弹幕。返回空串表示这一帧实在挤不下，丢弃。
func (l *layout) placeScroll(it Item, fs, width float64, need int) string {
	// 恒定速度模型：走完一个屏宽用 ScrollSeconds 秒。
	speed := float64(l.opt.PlayResX) / ScrollSeconds
	enter := width / speed                             // 完全进入屏幕所需时间
	total := (float64(l.opt.PlayResX) + width) / speed // 从右侧屏外走到左侧屏外

	row := l.alloc(l.scrollFree, need, it.Time, false)
	if row < 0 {
		return ""
	}
	for j := 0; j < need; j++ {
		l.scrollFree[row+j] = it.Time + enter
	}

	y := float64(row)*l.rowHeight + l.rowHeight/2
	// 只用 \move，不要再给 \pos —— 两者同时出现时 ASS 的行为不确定。
	// \an7 是左上角对齐（Style 里也是 7），所以 x 就是文字左边缘。
	over := fmt.Sprintf("\\an7\\move(%d,%.1f,%.1f,%.1f)\\fs%.0f%s",
		l.opt.PlayResX, y, -width, y, fs, assColor(it.Color))
	return dialogueLine(it.Time, it.Time+total, over, it.Text)
}

// placeFixed 放一条固定弹幕（顶部或底部）。
func (l *layout) placeFixed(it Item, fs float64, need int, top bool) string {
	free := l.topFree
	if !top {
		free = l.bottomFree
	}
	// 底部固定弹幕要从下往上占行，否则它们会被画到屏幕顶上。
	row := l.alloc(free, need, it.Time, !top)
	if row < 0 {
		return ""
	}
	for j := 0; j < need; j++ {
		free[row+j] = it.Time + FixedSeconds
	}

	cx := float64(l.opt.PlayResX) / 2
	var over string
	if top {
		// \an8 = 顶部居中，坐标是文字上边缘。
		y := float64(row) * l.rowHeight
		over = fmt.Sprintf("\\an8\\pos(%.1f,%.1f)\\fs%.0f%s", cx, y, fs, assColor(it.Color))
	} else {
		// \an2 = 底部居中，坐标是文字下边缘。行号自上而下，所以下边缘
		// 是这一段的底 = (row+need) * 行高。
		y := float64(row+need) * l.rowHeight
		over = fmt.Sprintf("\\an2\\pos(%.1f,%.1f)\\fs%.0f%s", cx, y, fs, assColor(it.Color))
	}
	return dialogueLine(it.Time, it.Time+FixedSeconds, over, it.Text)
}

// alloc 在 free 表里找连续 need 行都空着的位置，返回起始行号；找不到返回 -1。
//
// fromBottom 为真时从最后一行往上找 —— 底部固定弹幕必须这样分配，
// 否则画面底部空着、顶部被底部弹幕占满，看起来完全不对。
//
// 只认「上一条已经腾出这一行」的行，不会为了让新弹幕挤进来而推迟它出现的时间 ——
// 弹幕的时间是内容的一部分，改了时间就等于换了弹幕。
// 因此高峰期会有弹幕被丢弃（返回 -1），这与 B 站的行为一致。
func (l *layout) alloc(free []float64, need int, t float64, fromBottom bool) int {
	if need > l.rows {
		return -1
	}
	last := l.rows - need
	for n := 0; n <= last; n++ {
		i := n
		if fromBottom {
			i = last - n
		}
		ok := true
		for j := 0; j < need; j++ {
			if free[i+j] > t {
				ok = false
				break
			}
		}
		if ok {
			return i
		}
	}
	return -1
}

// textWidth 估算一段文本渲染后的像素宽度。
//
// ASS 不像浏览器那样能自己量文本，只能估。按东亚字符 1 em、其余 0.5~0.6 em 算。
// 估算宁大勿小：偏大只是滚动距离算长一点，偏小会让同一行里的弹幕叠起来。
func (l *layout) textWidth(text string, fs float64) float64 {
	var em float64
	for _, line := range strings.Split(text, "\n") {
		var w float64
		for _, r := range line {
			w += runeEm(r)
		}
		if w > em {
			em = w
		}
	}
	if em <= 0 {
		em = 1
	}
	return em*fs + l.outline*2
}

// runeEm 返回单个字符占多少个 em。
func runeEm(r rune) float64 {
	switch {
	case r >= 0x2E80: // CJK、假名、韩文、全角标点
		return 1.0
	case r == ' ' || r == '\t':
		return 0.35
	case r >= '0' && r <= '9', r >= 'A' && r <= 'Z':
		return 0.60
	case r >= 'a' && r <= 'z':
		return 0.52
	default:
		return 0.55
	}
}

// ---------------------------------------------------------------------------
// 文本拼装

// dialogueLine 拼出一行 Dialogue。
func dialogueLine(start, end float64, overrides, text string) string {
	return fmt.Sprintf("Dialogue: 0,%s,%s,Danmaku,,0,0,0,,{%s}%s",
		assTime(start), assTime(end), overrides, escape(text))
}

// assColor 把 0xRRGGBB 转成 ASS 的 &HBBGGRR（ASS 是 BGR 序）。
func assColor(rgb uint32) string {
	r := (rgb >> 16) & 0xFF
	g := (rgb >> 8) & 0xFF
	b := rgb & 0xFF
	return fmt.Sprintf("\\c&H%02X%02X%02X&", b, g, r)
}

// escape 处理 ASS 里有特殊含义的字符。
//
// 裸的 { } 会被当成特效块的开头，必须换掉；换行用 ASS 的硬换行 \N；
// 反斜杠虽然只在 {} 里特殊，但用户可能手改文件，也一并换成全角。
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '{':
			b.WriteRune('｛')
		case '}':
			b.WriteRune('｝')
		case '\\':
			b.WriteRune('＼')
		case '\n':
			b.WriteString("\\N")
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// assTime 把秒格式化成 ASS 的 H:MM:SS.CC。
func assTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	cs := int(math.Round(sec * 100))
	h := cs / 360000
	cs -= h * 360000
	m := cs / 6000
	cs -= m * 6000
	s := cs / 100
	cs -= s * 100
	return fmt.Sprintf("%d:%02d:%02d.%02d", h, m, s, cs)
}
