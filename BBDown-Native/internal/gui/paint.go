package gui

import (
	"math"
	"strings"

	"github.com/lxn/walk"
)

// 本文件负责「画」。界面用即时模式：每次重绘都重新算一遍元素矩形，
// 鼠标点击再拿同一批矩形去做命中测试。
//
// 为什么不用 walk 的原生控件：这次的目标是把老网页那套外观搬进原生窗口，
// 而系统控件的圆角、配色、字号都被主题锁死，改不动。所以除了「真正需要
// 文本输入 / IME 的地方」（那是原生 EDIT）之外，其余全部自绘。

// ---------------------------------------------------------------------------
// 元素模型

type elementKind int

const (
	elCard elementKind = iota
	elCardTitle
	elHint
	elText
	elDivider
	elButton
	elPills
	elCheck
	elInputBox // 只是一圈外框，里面的原生 EDIT 由窗口单独摆放
	elProgress
	elConsole
	elStatusPill
	elLogo
)

// statusTone 给状态胶囊选配色。
type statusTone int

const (
	toneMuted statusTone = iota
	toneOk
	toneBad
	toneInfo
)

// buttonTone 给按钮选配色。
type buttonTone int

const (
	btnPrimary buttonTone = iota // 粉色实心，主操作
	btnGhost                     // 卡片底 + 描边，次操作
	btnMini                      // 小号粉色
	btnQuiet                     // 无边框，只有文字
)

type element struct {
	id   string
	kind elementKind
	rect walk.Rectangle

	text string
	sub  string

	tone     buttonTone
	status   statusTone
	disabled bool

	// elPills
	opts     []string
	sel      int
	optRects []walk.Rectangle

	// elCheck
	on bool

	// elProgress
	frac float64

	// 交互态，由鼠标处理填写
	hover    bool
	hoverOpt int
	pressed  bool
}

// ---------------------------------------------------------------------------
// GDI 资源缓存
//
// 这是本文件最要紧的一件事：画刷与字体都是 GDI 句柄，而 Windows 每个进程
// 只有 10000 个上限。paint 会被调用成千上万次，一旦在里面 New 画刷，
// 界面用不了几分钟就会因为句柄耗尽而画不出任何东西（且报错极难定位）。
// 所以所有画刷按颜色缓存、全程复用。

type gfx struct {
	brushes map[walk.Color]*walk.SolidColorBrush

	// 窗口的 DPI 缩放比（96dpi = 1.0）。文字宽度的估算要用它，
	// 由 appwin 在量出窗口 DPI 后回填。
	scale float64
}

func newGfx() *gfx { return &gfx{brushes: map[walk.Color]*walk.SolidColorBrush{}} }

func (g *gfx) brush(c walk.Color) walk.Brush {
	if b, ok := g.brushes[c]; ok {
		return b
	}
	b, err := walk.NewSolidColorBrush(c)
	if err != nil {
		// 造不出画刷基本只可能是句柄耗尽；返回 nil 让调用方跳过这一笔，
		// 总比 panic 掉整个窗口好。
		return nil
	}
	g.brushes[c] = b
	return b
}

// roundedBorder 用「先填外圈、再填内圈」的方式画圆角描边。
//
// 刻意不用 GDI 的 RoundRect + Pen：RoundRect 的描边是压在边界线上的，
// 1px 笔宽会被四舍五入到半个像素，圆角处经常出现缺口或毛边。两次纯色
// 填充的结果是确定的，两个主题下都不会有意外。
func (g *gfx) roundedBorder(c *walk.Canvas, r walk.Rectangle, radius, width int, border, fill walk.Color) error {
	if r.Width <= 0 || r.Height <= 0 {
		return nil
	}
	if width > 0 {
		b := g.brush(border)
		if b == nil {
			return nil
		}
		if err := c.FillRoundedRectanglePixels(b, r, walk.Size{Width: radius * 2, Height: radius * 2}); err != nil {
			return err
		}
	}
	f := g.brush(fill)
	if f == nil {
		return nil
	}
	ir := radius - width
	if ir < 0 {
		ir = 0
	}
	inner := walk.Rectangle{
		X:      r.X + width,
		Y:      r.Y + width,
		Width:  r.Width - 2*width,
		Height: r.Height - 2*width,
	}
	if inner.Width <= 0 || inner.Height <= 0 {
		return nil
	}
	return c.FillRoundedRectanglePixels(f, inner, walk.Size{Width: ir * 2, Height: ir * 2})
}

// fill 画一个实心圆角矩形。
func (g *gfx) fill(c *walk.Canvas, r walk.Rectangle, radius int, col walk.Color) error {
	if r.Width <= 0 || r.Height <= 0 {
		return nil
	}
	b := g.brush(col)
	if b == nil {
		return nil
	}
	if radius <= 0 {
		return c.FillRectanglePixels(b, r)
	}
	return c.FillRoundedRectanglePixels(b, r, walk.Size{Width: radius * 2, Height: radius * 2})
}

// text 画一行（或一段）文字。
func (g *gfx) text(c *walk.Canvas, s string, f *walk.Font, col walk.Color, r walk.Rectangle, format walk.DrawTextFormat) error {
	if s == "" || r.Width <= 0 || r.Height <= 0 {
		return nil
	}
	return c.DrawTextPixels(s, f, col, r, format|walk.TextNoPrefix)
}

// estimateTextWidth 估算一段文字在 sizePt 号字体下的像素宽度：
// 中文按 1 个字宽、其他按 0.6。布局量宽与日志截断都用这一套。
//
// 刻意不做真实测量：walk 的 MeasureTextPixels 内部是 metafile + DrawTextEx，
// 但**没带 DT_CALCRECT**，DrawTextEx 不会回写矩形，返回的宽度就是调用方
// 传进去的 bounds.Width —— 传多大它「量」出多大，纯属摆设（实测踩过：
// 日志整行被截成只剩一个省略号）。真要量就得自己建 DC 选字体，为一个
// 截断不值得，估算的误差对这两处用途都无关紧要。
func estimateTextWidth(s string, sizePt int, scale float64) int {
	var units float64
	for _, r := range s {
		if r > 0x2e80 {
			units++
		} else {
			units += 0.6
		}
	}
	return int(math.Round(units * float64(sizePt) * scale * 1.34))
}

// truncateToWidth 按估算宽度把一行截断，超出部分用省略号。
//
// 不用 DrawText 的 TextEndEllipsis：它只支持单行且同样依赖字体度量，
// 自己按字符切更可控，也能保证省略号一定画得出来。
func (g *gfx) truncateToWidth(s string, f *walk.Font, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(s)
	// 控制台固定用 9pt 等宽字体，见 theme.go 的 newFonts。
	if estimateTextWidth(s, 9, g.scale) <= max {
		return s
	}
	// 二分找最长能放下的前缀
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if estimateTextWidth(string(runes[:mid])+"…", 9, g.scale) <= max {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	if lo == 0 {
		return "…"
	}
	return string(runes[:lo]) + "…"
}

// ---------------------------------------------------------------------------
// 各类元素的画法

// paintElement 画一个元素。c 的坐标原点已经是控件左上角。
func (g *gfx) paintElement(c *walk.Canvas, e *element, p Palette, f *fonts) error {
	switch e.kind {
	case elCard:
		return g.paintCard(c, e, p)
	case elCardTitle:
		return g.text(c, e.text, f.Head, p.Text, e.rect, walk.TextSingleLine|walk.TextVCenter)
	case elHint:
		return g.text(c, e.text, f.Small, p.Text3, e.rect, walk.TextSingleLine|walk.TextVCenter)
	case elText:
		col := p.Text2
		if e.disabled {
			col = p.Text3
		}
		return g.text(c, e.text, f.Body, col, e.rect, walk.TextSingleLine|walk.TextVCenter)
	case elDivider:
		return g.fill(c, e.rect, 0, p.Line)
	case elLogo:
		return g.paintLogo(c, e, p, f)
	case elStatusPill:
		return g.paintStatusPill(c, e, p, f)
	case elButton:
		return g.paintButton(c, e, p, f)
	case elPills:
		return g.paintPills(c, e, p, f)
	case elCheck:
		return g.paintCheck(c, e, p, f)
	case elInputBox:
		// 底色必须与窗口底一致 —— 里面真的嵌着一个原生 EDIT，而 EDIT 的
		// 背景色由 stage 的背景画刷决定（见 applyStageBackground）。两边
		// 不一致的话，输入框里会出现一块颜色不同的矩形。
		return g.roundedBorder(c, e.rect, lgRadiusSm, 1, p.LineStr, p.Bg)
	case elProgress:
		return g.paintProgress(c, e, p)
	}
	return nil
}

func (g *gfx) paintCard(c *walk.Canvas, e *element, p Palette) error {
	// 卡片自带 1px 描边。没有做投影：GDI 没有模糊，用几层递减的圆角矩形
	// 去"假装"投影，在浅色底上会显出一圈生硬的色阶，反而不如干净的描边。
	return g.roundedBorder(c, e.rect, lgRadiusCard, 1, p.Line, p.Surface)
}

func (g *gfx) paintLogo(c *walk.Canvas, e *element, p Palette, f *fonts) error {
	r := e.rect
	// 上下两段渐变，模仿网页里 logo 的立体感
	half := r.Height / 2
	if err := g.fill(c, walk.Rectangle{X: r.X, Y: r.Y, Width: r.Width, Height: half + 2}, lgRadiusSm, p.Accent); err != nil {
		return err
	}
	if err := g.fill(c, walk.Rectangle{X: r.X, Y: r.Y + half - 2, Width: r.Width, Height: r.Height - half + 2}, lgRadiusSm, p.Accent2); err != nil {
		return err
	}
	return g.text(c, "B", f.Head, walk.RGB(0xff, 0xff, 0xff), r, walk.TextSingleLine|walk.TextVCenter|walk.TextCenter)
}

func (g *gfx) paintStatusPill(c *walk.Canvas, e *element, p Palette, f *fonts) error {
	bg, fg := p.Surface3, p.Text2
	switch e.status {
	case toneOk:
		bg, fg = p.OkBg, p.OkFg
	case toneBad:
		bg, fg = p.BadBg, p.BadFg
	case toneInfo:
		bg, fg = p.AccentSoft, p.Accent2
	}
	// 左侧一个小圆点，跟网页上的状态标签一致
	if err := g.fill(c, e.rect, e.rect.Height/2, bg); err != nil {
		return err
	}
	dot := e.rect.Height / 4
	if dot < 3 {
		dot = 3
	}
	dr := walk.Rectangle{
		X:      e.rect.X + 9,
		Y:      e.rect.Y + (e.rect.Height-dot)/2,
		Width:  dot,
		Height: dot,
	}
	if err := g.fill(c, dr, dot/2, fg); err != nil {
		return err
	}
	tr := walk.Rectangle{
		X:      dr.X + dot + 6,
		Y:      e.rect.Y,
		Width:  e.rect.Width - (dr.X - e.rect.X) - dot - 6 - 8,
		Height: e.rect.Height,
	}
	return g.text(c, e.text, f.Small, fg, tr, walk.TextSingleLine|walk.TextVCenter)
}

func (g *gfx) paintButton(c *walk.Canvas, e *element, p Palette, f *fonts) error {
	bg, fg, border := p.Surface, p.Text, p.LineStr
	switch e.tone {
	case btnPrimary:
		bg, fg = p.Accent, walk.RGB(0xff, 0xff, 0xff)
		if e.pressed {
			bg = p.Accent2
		} else if e.hover {
			bg = p.Accent2
		}
	case btnMini:
		bg, fg = p.AccentSoft, p.Accent2
		if e.hover || e.pressed {
			bg = p.Accent
			fg = walk.RGB(0xff, 0xff, 0xff)
		}
	case btnQuiet:
		if e.hover {
			bg = p.Surface3
		}
		return g.text(c, e.text, f.Small, p.Text2, e.rect, walk.TextSingleLine|walk.TextVCenter|walk.TextCenter)
	}

	if e.disabled {
		// 禁用态一律压成灰底灰字，不保留主色 —— 否则用户会以为还能点。
		bg = p.Surface3
		fg = p.Text3
		border = p.Line
	} else if e.hover && e.tone == btnGhost {
		bg = p.Surface3
	}

	w := 1
	if e.tone == btnPrimary {
		w = 0
	}
	if err := g.roundedBorder(c, e.rect, lgRadiusSm, w, border, bg); err != nil {
		return err
	}
	font := f.Small
	if e.tone == btnPrimary {
		font = f.SmallB
	}
	return g.text(c, e.text, font, fg, e.rect, walk.TextSingleLine|walk.TextVCenter|walk.TextCenter)
}

func (g *gfx) paintPills(c *walk.Canvas, e *element, p Palette, f *fonts) error {
	// 外圈是浅底，选中项浮出一块白（暗色主题下浮出一块更亮的卡片色）。
	if err := g.roundedBorder(c, e.rect, lgRadiusSm, 1, p.Line, p.Surface3); err != nil {
		return err
	}
	for i, opt := range e.opts {
		if i >= len(e.optRects) {
			break
		}
		r := e.optRects[i]
		selected := i == e.sel
		bg, fg, border := p.Surface3, p.Text2, p.Surface3
		w := 0
		if selected {
			bg, fg, border, w = p.AccentSoft, p.Accent2, p.Accent, 1
		} else if e.hover && e.hoverOpt == i {
			bg, fg = p.Surface, p.Text
		}
		if err := g.roundedBorder(c, r, lgRadiusSm-2, w, border, bg); err != nil {
			return err
		}
		font := f.Small
		if selected {
			font = f.SmallB
		}
		if err := g.text(c, opt, font, fg, r, walk.TextSingleLine|walk.TextVCenter|walk.TextCenter); err != nil {
			return err
		}
	}
	return nil
}

func (g *gfx) paintCheck(c *walk.Canvas, e *element, p Palette, f *fonts) error {
	box := e.rect.Height
	if box > 17 {
		box = 17
	}
	br := walk.Rectangle{X: e.rect.X, Y: e.rect.Y + (e.rect.Height-box)/2, Width: box, Height: box}
	border, fill := p.LineStr, p.Surface
	if e.on {
		border, fill = p.Accent, p.Accent
	}
	if e.disabled {
		border, fill = p.Line, p.Surface3
	}
	if err := g.roundedBorder(c, br, 4, 1, border, fill); err != nil {
		return err
	}
	if e.on {
		// 用两条线段画对勾：GDI 画线比找字体符号可靠（不用担心字体里
		// 有没有 ✓ 这个字形）。
		col := walk.RGB(0xff, 0xff, 0xff)
		if e.disabled {
			col = p.Text3
		}
		pen, err := walk.NewCosmeticPen(walk.PenSolid, col)
		if err != nil {
			return nil
		}
		defer pen.Dispose()
		x, y := br.X, br.Y
		w := br.Width
		a := walk.Point{X: x + w*25/100, Y: y + w*52/100}
		b := walk.Point{X: x + w*43/100, Y: y + w*70/100}
		cc := walk.Point{X: x + w*76/100, Y: y + w*31/100}
		if err := c.DrawLinePixels(pen, a, b); err != nil {
			return err
		}
		if err := c.DrawLinePixels(pen, b, cc); err != nil {
			return err
		}
	}

	tr := walk.Rectangle{
		X:      br.X + box + 7,
		Y:      e.rect.Y,
		Width:  e.rect.Width - box - 7,
		Height: e.rect.Height,
	}
	col := p.Text2
	if e.disabled {
		col = p.Text3
	}
	return g.text(c, e.text, f.Small, col, tr, walk.TextSingleLine|walk.TextVCenter)
}

func (g *gfx) paintProgress(c *walk.Canvas, e *element, p Palette) error {
	if err := g.roundedBorder(c, e.rect, e.rect.Height/2, 0, 0, p.Surface3); err != nil {
		return err
	}
	if e.frac <= 0 {
		return nil
	}
	frac := e.frac
	if frac > 1 {
		frac = 1
	}
	w := int(float64(e.rect.Width) * frac)
	if w < e.rect.Height {
		// 太窄的时候画不出圆头，直接给一个最小可见宽度，看起来像"刚开始"。
		w = e.rect.Height
	}
	bar := walk.Rectangle{X: e.rect.X, Y: e.rect.Y, Width: w, Height: e.rect.Height}
	return g.fill(c, bar, e.rect.Height/2, p.Accent)
}

// ---------------------------------------------------------------------------
// 日志控制台
//
// 控制台也是自绘的。用原生 TextEdit 的话，它的背景色由父窗口的
// WM_CTLCOLOREDIT 统一决定，没法只让日志这一块变深色；而日志区在
// 浅色主题下也必须是深色底，所以这里自己画。

// logLine 是一条日志。level 决定前缀色。
type logLine struct {
	level string // "" | "ok" | "bad" | "warn" | "dim"
	text  string
}

// consoleMetrics 返回控制台的可用行数与行高。
//
// 行高跟着 DPI 走：9pt 等宽字体在 96dpi 下约 16px 一行，随 scale 线性放大。
// 写死的话，高 DPI 屏上字比行高大，上下两行会叠在一起。
func consoleMetrics(rect walk.Rectangle, scale float64) (lineH, visible int, inner walk.Rectangle) {
	lineH = int(math.Round(16.2 * scale))
	if lineH < 14 {
		lineH = 14
	}
	inner = walk.Rectangle{
		X:      rect.X + 12,
		Y:      rect.Y + 9,
		Width:  rect.Width - 24 - 10, // 右侧留出滚动条的位置
		Height: rect.Height - 18,
	}
	if inner.Height < lineH {
		return lineH, 0, inner
	}
	return lineH, inner.Height / lineH, inner
}

// paintConsole 画日志控制台。
func (g *gfx) paintConsole(c *walk.Canvas, e *element, p Palette, f *fonts, lines []logLine, scroll int) error {
	if err := g.fill(c, e.rect, lgRadiusSm, p.ConsoleBg); err != nil {
		return err
	}
	lineH, visible, inner := consoleMetrics(e.rect, g.scale)
	if visible == 0 {
		return nil
	}

	total := len(lines)
	// scroll 是从末尾往上翻的行数
	end := total - scroll
	if end > total {
		end = total
	}
	start := end - visible
	if start < 0 {
		start = 0
	}

	// 内容不满一屏时从底部对齐 —— 日志区被拉高时，文字不该从顶上飘着。
	y := inner.Y
	if n := end - start; n < visible {
		y += (visible - n) * lineH
	}

	for i := start; i < end; i++ {
		ln := lines[i]
		col := p.ConsoleFg
		switch ln.level {
		case "ok":
			col = p.Green
		case "bad":
			col = p.Red
		case "warn":
			col = p.Amber
		case "dim":
			col = p.ConsoleDim
		}
		s := g.truncateToWidth(ln.text, f.Mono, inner.Width)
		r := walk.Rectangle{X: inner.X, Y: y, Width: inner.Width, Height: lineH}
		if err := g.text(c, s, f.Mono, col, r, walk.TextSingleLine|walk.TextVCenter); err != nil {
			return err
		}
		y += lineH
	}

	// 滚动条：内容没超出就完全不画，免得出现一根永远满格的滑块。
	if total > visible {
		trackX := e.rect.X + e.rect.Width - 10
		track := walk.Rectangle{X: trackX, Y: e.rect.Y + 8, Width: 4, Height: e.rect.Height - 16}
		if err := g.fill(c, track, 2, p.Surface3); err != nil {
			return err
		}
		thumbH := visible * track.Height / total
		if thumbH < 20 {
			thumbH = 20
		}
		// scroll=0 表示贴在底部
		maxTop := track.Height - thumbH
		top := 0
		if maxScroll := total - visible; maxScroll > 0 {
			top = maxTop * (maxScroll - scroll) / maxScroll
		}
		thumb := walk.Rectangle{X: track.X, Y: track.Y + top, Width: 4, Height: thumbH}
		if err := g.fill(c, thumb, 2, p.LineStr); err != nil {
			return err
		}
	}
	return nil
}

// splitLines 把一段可能含换行的文本拆成日志行。
func splitLines(s string) []string {
	if !strings.ContainsAny(s, "\r\n") {
		return []string{s}
	}
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '\r' || r == '\n' })
	if len(parts) == 0 {
		return []string{""}
	}
	return parts
}
