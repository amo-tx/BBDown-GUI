// Package gui 是自绘的原生窗口界面。
//
// 为什么要自绘：系统控件的圆角、配色、字号都被 Windows 主题锁死，做不出
// 老网页那套外观（B 站粉主色 + 白色圆角卡片 + 明暗双主题）。所以除了
// **真正需要文本输入与 IME 的地方**（那必须是原生 EDIT，自己实现输入法
// 是条不归路）之外，其余全部用 GDI 画。
//
// 线程约定：walk 的所有控件只能在消息循环线程上操作。后台任务通过
// w.mw.Synchronize(...) 把更新投递回来。
package gui

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	"github.com/lxn/walk/declarative"

	"bbdown-native/internal/app"
	"bbdown-native/internal/bilibili"
)

// 逻辑尺寸（96dpi）。实际绘制前都会经过 w.s() 按窗口 DPI 缩放。
const (
	lgHeaderH    = 62
	lgPad        = 18
	lgGap        = 14
	lgCardPad    = 16
	lgRadiusCard = 12
	lgRadiusSm   = 8
	lgBtnH       = 32
	lgPillH      = 30
	lgRowH       = 36
	lgHeaderBtnH = 28
)

// 画质分段。值直接就是 B 站的 qn 编号，0 表示「能拿多高拿多高」。
var qualityOpts = []struct {
	Label string
	QN    int
}{
	{"自动", 0},
	{"1080P", 80},
	{"720P", 64},
	{"480P", 32},
}

// 编码分段。
//
// 默认是「自动」而它指向 AVC 优先：HEVC/AV1 同画质下体积小，
// 但产出的文件在哔哩哔哩客户端等播放器上会全黑（文件本身是好的，
// 是那些播放器的解码路径吃不下）。AVC 到处都能播。
// 追求小体积可以显式选 HEVC / AV1，但要清楚兼容性代价。
var codecOpts = []struct {
	Label string
	Value string
}{
	{"自动", app.DefaultCodecOrder},
	{"AVC 通用", "avc"},
	{"HEVC 小体积", "hevc"},
	{"AV1 更小", "av1"},
}

// Win 是主窗口。
type Win struct {
	mw *walk.MainWindow
	cw *walk.CustomWidget

	// 原生输入控件，直接由 Win32 创建（原因见 nativeedit.go）。
	// 它们不是 walk 的子控件，所以位置完全由我们自己定。
	addr *nativeEdit // 视频地址，多行
	dir  *nativeEdit // 保存目录
	page *nativeEdit // 分P
	par  *nativeEdit // 并行数

	cfg    *app.Config
	runner *app.Runner
	login  *bilibili.LoginSession

	gfx   *gfx
	font  *fonts
	pal   Palette
	dark  bool
	scale float64

	// 布局缓存：尺寸没变就不重算，保证「绘制用的矩形」与「命中测试用的
	// 矩形」永远是同一批。
	laidW, laidH int
	els          []element

	// 由 rebuild 填好、鼠标与绘制共用
	headRect    walk.Rectangle
	detailRect  walk.Rectangle
	consoleRect walk.Rectangle

	// 交互态
	hoverID  string
	hoverOpt int
	pressID  string
	pressOpt int

	// 业务态
	mu       sync.Mutex
	items    []*app.Resolved
	qualSel  []int
	busy     bool
	cancel   context.CancelFunc
	listener bool

	// 附加内容的界面态。它们与配置一一对应，改动即落盘。
	keepTemp bool
	danmaku  bool
	subtitle bool
	cover    bool

	logs     []logLine
	scroll   int
	stageTxt string
	prog     app.JobProgress
	progText string
	// 三张指标卡：速度 / 已下载 / 剩余时间。各自独立成元素，才能像老版那样
	// 排成一行对齐的卡片，而不是挤在一行文字里被裁掉。
	statSpeed string
	statSize  string
	statETA   string
	// spark 是最近若干次的速度采样，给 elSpark 画曲线。老版取 60 个点，
	// 这里同样取 60（配合 download.go 每 300ms 一次，约 18 秒窗口）。
	spark []float64
	// sparkRunning 标记当前是否在跑：任务结束要把曲线清空，
	// 否则停下来之后图上还留着一条"正在高速下载"的线，误导。
	sparkRunning bool
	// stepCur 是当前阶段序号（对应 stageOrder），-1 表示还没开始。
	stepCur int
	// progShown 记录「进度详情区」（曲线/指标卡/步进器）当前是否展开，
	// 与 hasTaskUI() 对应：状态翻转的那一次要走 relayout 重建元素，
	// 其余时候原地刷新就够。
	progShown bool
	// autoscroll 对应老版的 #autoscroll：关掉后新日志不再把视口
	// 拽到底部，方便往上翻历史。
	autoscroll bool
	// stagelineW 是常驻阶段行的可用宽度（截断长日志行用），layout 时刷新。
	stagelineW int
	// logCountRight 是行数标签的右缘 X（布局时算好，刷新时按文本宽反推左缘）。
	logCountRight int
	acctText string
	acctTone statusTone
	infoTxt  string
	detail   string
	outputs  []string
}

// 阶段步进器的固定顺序，与老版网页一致：解析 → 视频 → 音频 → 混流。
// prog.Label 与这些名字对照可得出当前阶段；都不匹配时退到第 0 段。
var stageOrder = []string{"解析", "视频", "音频", "混流"}

// sparkPoints 是速度曲线保留的采样点数。
const sparkPoints = 60

// Run 创建并运行主窗口，直到用户关闭它。
func Run(cfg *app.Config) error {
	return RunWithWarning(cfg, "")
}

// RunWithWarning 与 Run 相同，但启动时先弹一条提示（配置损坏等情况）。
func RunWithWarning(cfg *app.Config, warning string) error {
	w := &Win{
		cfg:      cfg,
		dark:     cfg.Theme == app.ThemeDark,
		pal:      paletteFor(cfg.Theme == app.ThemeDark),
		gfx:      newGfx(),
		acctText: "未登录 —— 只能下到 480P",
		acctTone: toneMuted,
		stageTxt: "就绪",
		progText: "等待任务…",
		stepCur:  -1,
		autoscroll: true,
		infoTxt:  "尚未解析",
		keepTemp: cfg.KeepTemp,
		danmaku:  cfg.WantsDanmaku(),
		subtitle: cfg.WantsSubtitle(),
		cover:    cfg.SaveCover,
	}
	w.runner = app.NewRunner(cfg, app.Hooks{
		Stage:    w.onStage,
		Log:      w.onLog,
		Progress: w.onProgress,
		Output:   w.onOutput,
	})
	w.login = bilibili.NewLoginSession()

	if err := w.create(); err != nil {
		return err
	}
	w.refreshAccount()
	w.log("就绪。配置文件：%s", cfg.Path())
	if warning != "" {
		w.log("! %s", warning)
		go func() {
			time.Sleep(300 * time.Millisecond)
			w.mw.Synchronize(func() {
				walk.MsgBox(w.mw, "提示", warning, walk.MsgBoxIconWarning)
			})
		}()
	}
	w.mw.Run()
	return nil
}

// ---------------------------------------------------------------------------
// 建界面

// create 建出主窗口。
//
// 这里的结构是被 walk 逼出来的，改之前请先读 nativeedit.go 的说明：
//
//   - 主窗口必须声明一个 Layout。主窗口内部有个 clientComposite，它的窗口过程
//     在每次 WM_SIZE 都会跑一遍布局，而 walk 的布局在拿到 nil 布局时会直接
//     解引用 —— 所以「不给 Layout」不是选项，会崩。
//   - 自绘层（CustomWidget）是 clientComposite 的唯一子控件。CustomWidget 的
//     布局项是 Greedy 的，所以它会被自动拉伸到整个客户区 —— 正好就是我们
//     想要的，不必自己算尺寸。
//   - 那四个原生 EDIT 不是 walk 的子控件（那样会被布局系统接管、位置不保），
//     而是直接建在 clientComposite 底下、由我们绝对定位。
func (w *Win) create() error {
	if err := (declarative.MainWindow{
		AssignTo: &w.mw,
		Title:    "BBDown 原生版",
		Size:     declarative.Size{Width: 1140, Height: 790},
		MinSize:  declarative.Size{Width: 1000, Height: 680},
		Layout:   declarative.VBox{MarginsZero: true, Spacing: 0},
	}).Create(); err != nil {
		return err
	}

	var err error
	if w.font, err = newFonts(); err != nil {
		return err
	}
	w.scale = float64(w.mw.DPI()) / 96.0
	w.gfx.scale = w.scale

	// 自绘层：铺满客户区的那块画布。
	if w.cw, err = walk.NewCustomWidgetPixels(w.mw, 0, w.paint); err != nil {
		return err
	}
	w.cw.SetPaintMode(walk.PaintBuffered)
	w.cw.SetInvalidatesOnResize(true)
	w.cw.SetFont(w.font.Body)
	w.attachMouse()

	// 底色的作用有两层：一是自绘层画卡片时衬在下面，二是原生 EDIT 的底色
	// 由父窗口响应 WM_CTLCOLOREDIT 决定 —— 但字色 walk 不会管（见
	// nativeedit.go），所以这里还要把父窗口的窗口过程接一层。
	if err := w.applyStageBackground(); err != nil {
		return err
	}
	host := w.cw.Parent().Handle()
	hostEdits(host)

	hf := fontHandle(w.cw)
	mk := func(multi bool) (*nativeEdit, error) {
		e, err := newNativeEdit(host, multi)
		if err != nil {
			return nil, err
		}
		e.SetFont(hf)
		return e, nil
	}
	// 顺序就是 tab 顺序，也顺带决定了 z 序（后建的在上）。
	if w.addr, err = mk(true); err != nil {
		return err
	}
	if w.page, err = mk(false); err != nil {
		return err
	}
	w.page.SetCue("留空=全部，也可写 1 或 1,3-5")
	if w.dir, err = mk(false); err != nil {
		return err
	}
	w.dir.SetText(w.cfg.DownloadDir)
	if w.par, err = mk(false); err != nil {
		return err
	}
	w.par.SetText(fmt.Sprint(w.cfg.Parallel))

	w.applyPaletteToInputs()

	w.cw.SizeChanged().Attach(func() {
		w.ensureLayout()
		_ = w.cw.Invalidate()
	})
	w.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		w.onStop()
	})
	return nil
}

// applyStageBackground 按当前主题设置窗口底色。
//
// 底色用的是 **--bg**（页面底色）而不是 --surface（卡片底色）：卡片才是白底，
// 页面是一层浅灰蓝，这样才能看出一张张卡片浮在页面上。整块窗口如果都刷成
// 卡片色，卡片就没有边界了。
//
// 这个画刷同时也是原生 EDIT 的背景（父窗口响应 WM_CTLCOLOREDIT 时给出的
// 就是它）。所以自绘的输入框外框（elInputBox）刻意用同一个颜色填充，
// 两边才能浑然一体；只靠 1px 描边表达「这是输入框」，两个主题下都成立。
func (w *Win) applyStageBackground() error {
	// ⚠️ 这里绝不能用 g.brush 缓存，必须新建画刷把所有权交给 walk。
	//
	// walk 的 SetBackground 在替换背景时会对旧画刷调 detachWindow：
	// 一旦没有别的窗口挂着它，walk 会直接 Dispose()（DeleteObject）。
	// 而缓存里的画刷还要给自绘层反复用 —— 被销毁后句柄变 0，
	// 再从缓存取出来画就是无效 GDI 对象，整窗填充全部静默失效，
	// 表现就是「深浅色来回切一次后再切回来，界面全黑」。
	// 每次切换新建一个、由 walk 负责释放：一次切换只造一个句柄，无泄漏。
	b, err := walk.NewSolidColorBrush(w.pal.Bg)
	if err != nil {
		return errNoBrush
	}
	w.mw.SetBackground(b)
	return nil
}

// applyPaletteToInputs 把主题色刷到原生输入控件上。
func (w *Win) applyPaletteToInputs() {
	// 字色没法走 walk（它只管自己创建的控件），得我们自己回答
	// WM_CTLCOLOREDIT，见 nativeedit.go。
	setEditColors(w.pal.Bg, w.pal.Text)
	for _, e := range []*nativeEdit{w.addr, w.dir, w.page, w.par} {
		e.Refresh()
	}
}

// ---------------------------------------------------------------------------
// 布局

func (w *Win) s(v int) int { return int(math.Round(float64(v) * w.scale)) }

// ensureLayout 在尺寸变化时重算一遍元素矩形。
//
// 放在绘制与鼠标事件之前统一调用，是为了让「画的」和「点的」永远是同一批
// 矩形 —— 一旦分成两套算法，迟早会出现「按钮看着在这里、点下去却没反应」。
func (w *Win) ensureLayout() {
	if w.cw == nil {
		return
	}
	// 自绘层是客户区里唯一被布局拉伸的控件，它的尺寸就是客户区尺寸。
	// 原生 EDIT 与它是同一个父窗口、原点相同，所以可以直接共用这套坐标。
	b := w.cw.BoundsPixels()
	if b.Width <= 0 || b.Height <= 0 {
		return
	}
	if b.Width == w.laidW && b.Height == w.laidH {
		return
	}
	w.laidW, w.laidH = b.Width, b.Height
	w.rebuild(b.Width, b.Height)
}

func (w *Win) add(e element) *element {
	w.els = append(w.els, e)
	return &w.els[len(w.els)-1]
}

func (w *Win) rebuild(W, H int) {
	w.els = w.els[:0]
	pad := w.s(lgPad)
	gap := w.s(lgGap)
	cp := w.s(lgCardPad)
	headH := w.s(lgHeaderH)

	// ---------------- 顶栏 ----------------
	// 顶栏是一整条 --surface 色的横带，下面压一根分隔线。
	w.headRect = walk.Rectangle{X: 0, Y: 0, Width: W, Height: headH - 1}
	w.els = append(w.els, element{
		id: "logo", kind: elLogo,
		rect: walk.Rectangle{X: pad, Y: w.s(14), Width: w.s(34), Height: w.s(34)},
	})
	w.els = append(w.els, element{
		kind: elCardTitle, text: "BBDown 原生版",
		rect: walk.Rectangle{X: pad + w.s(46), Y: w.s(11), Width: w.s(300), Height: w.s(24)},
	})
	w.els = append(w.els, element{
		kind: elHint, text: "bilibili 视频下载 · 单文件零依赖版",
		rect: walk.Rectangle{X: pad + w.s(46), Y: w.s(34), Width: w.s(320), Height: w.s(18)},
	})

	// 右侧从右往左摆三颗按钮
	bh := w.s(lgHeaderBtnH)
	x := W - pad
	put := func(id, txt string, tone buttonTone, width int) {
		x -= w.s(width)
		w.els = append(w.els, element{
			id: id, kind: elButton, text: txt, tone: tone,
			rect: walk.Rectangle{X: x, Y: w.s(17), Width: w.s(width), Height: bh},
		})
		x -= w.s(8)
	}
	put("quit", "退出", btnQuiet, 52)
	put("recheck", "重新校验", btnGhost, 88)
	themeTxt := "深色"
	if w.dark {
		themeTxt = "浅色"
	}
	put("theme", themeTxt, btnGhost, 66)

	// 状态胶囊：从右往左紧贴着按钮排
	st := func(txt string, tone statusTone) {
		width := w.s(34) + w.measureHint(txt)
		x -= width
		w.els = append(w.els, element{
			kind: elStatusPill, text: txt, status: tone,
			rect: walk.Rectangle{X: x, Y: w.s(17), Width: width, Height: bh},
		})
		x -= w.s(7)
	}
	engineTone := toneOk
	if w.cfg.AccessToken == "" {
		engineTone = toneMuted
	}
	st(w.acctText, w.acctTone)
	st("内置封装", toneOk)
	st("零依赖引擎", engineTone)

	w.els = append(w.els, element{
		kind: elDivider,
		rect: walk.Rectangle{X: 0, Y: headH - 1, Width: W, Height: 1},
	})

	// ---------------- 两栏 ----------------
	top := headH + pad
	bottom := H - pad
	if bottom-top < w.s(240) {
		bottom = top + w.s(240)
	}
	avail := W - 3*pad
	leftW := avail * 58 / 100
	rightX := pad + leftW + gap*2
	rightW := W - pad - rightX
	if rightW < w.s(300) {
		rightW = w.s(300)
		rightX = W - pad - rightW
	}

	// ---- 左栏：视频地址 ----
	inX := pad + cp
	inW := leftW - 2*cp
	// 标题与详情分行完整显示（不用省略号）：解析出的「标题 — UP主」和
	// 「共 N 个分P · 总时长 · 可用画质…」都按可用宽贪心切行，卡片高度
	// 随行数伸展。标题用 11pt 粗体（elCardTitle）、详情用 9pt（elHint），
	// 切行宽度留 8px 余量，保证绘制期的按宽截断永远不会碰到这些行。
	titleLines := wrapText(w.infoTxt, 11, inW-w.s(8), w.scale)
	detailLines := wrapText(w.detail, 9, inW-w.s(8), w.scale)
	hAddr := w.s(154) + w.s(22)*len(titleLines) + w.s(20)*len(detailLines) + w.s(6)
	if len(titleLines) == 0 {
		hAddr = w.s(178) // 理论上不发生（infoTxt 恒非空），兜底防卡片塌掉
	}
	w.card("card-addr", pad, top, leftW, hAddr)
	w.cardHead(pad, top, leftW, "视频地址", "支持分享文案 / 短链 / BV 号 / 番剧 ep·ss")
	w.add(element{
		id: "addr", kind: elInputBox,
		rect: walk.Rectangle{X: inX, Y: top + w.s(44), Width: inW, Height: w.s(64)},
	})
	by := top + w.s(118)
	w.button("parse", "解析视频", btnPrimary, inX, by, 96)
	w.button("paste", "粘贴", btnGhost, inX+w.s(104), by, 72)
	w.button("clear-addr", "清空", btnGhost, inX+w.s(184), by, 72)
	iy := top + w.s(154)
	for _, ln := range titleLines {
		w.add(element{
			kind: elCardTitle, text: ln,
			rect: walk.Rectangle{X: inX, Y: iy, Width: inW, Height: w.s(20)},
		})
		iy += w.s(22)
	}
	for _, ln := range detailLines {
		w.add(element{
			kind: elHint, text: ln, sub: "detail",
			rect: walk.Rectangle{X: inX, Y: iy, Width: inW, Height: w.s(18)},
		})
		iy += w.s(20)
	}
	w.detailRect = walk.Rectangle{X: inX, Y: top + w.s(154), Width: inW, Height: iy - (top + w.s(154))}

	// ---- 左栏：下载设置 ----
	y := top + hAddr + gap
	hOpt := w.s(44) + 5*w.s(lgRowH) + w.s(18)
	w.card("card-opts", pad, y, leftW, hOpt)
	w.cardHead(pad, y, leftW, "下载设置", "改动会自动保存")

	row := y + w.s(44)
	row = w.pillRow(pad, row, leftW, "画质", "quality", labelsOfQuality(), w.qualityIndex())
	row = w.pillRow(pad, row, leftW, "编码", "codec", labelsOfCodec(), w.codecIndex())
	row = w.twoInputRow(pad, row, leftW, "分P", "page", "并行", "par")

	// 目录行的右侧不是输入框的延伸，而是两颗按钮
	dirRow := row
	row = w.rowLabelInput(pad, row, leftW, "目录", "dir")
	w.button("pickdir", "选择", btnGhost, inX+inW-w.s(148), dirRow+w.s(2), 70)
	w.button("opendir", "打开", btnGhost, inX+inW-w.s(72), dirRow+w.s(2), 70)

	// 附加内容：三个开关 + 最右边的「保留临时分片」
	w.els = append(w.els, element{
		kind: elHint, text: "附加内容",
		rect: walk.Rectangle{X: pad + cp, Y: row, Width: w.s(60), Height: w.s(lgRowH)},
	})
	w.els = append(w.els, element{
		id: "check-keeptemp", kind: elCheck, text: "保留临时分片", on: w.keepTemp,
		rect: walk.Rectangle{X: inX + inW - w.s(140), Y: row, Width: w.s(140), Height: w.s(lgRowH)},
	})
	cx := pad + cp + w.s(66)
	checks := []struct {
		id, txt string
		on      bool
	}{
		{"check-danmaku", "弹幕(ASS)", w.danmaku},
		{"check-subtitle", "字幕(SRT)", w.subtitle},
		{"check-cover", "封面", w.cover},
	}
	for _, c := range checks {
		width := w.s(26) + w.measureSmall(c.txt)
		w.els = append(w.els, element{
			id: c.id, kind: elCheck, text: c.txt, on: c.on,
			rect: walk.Rectangle{X: cx, Y: row, Width: width, Height: w.s(lgRowH)},
		})
		cx += width + w.s(10)
	}
	row += w.s(lgRowH)
	_ = row

	// ---- 左栏：账号 ----
	y += hOpt + gap
	hAcct := w.s(140)
	w.card("card-acct", pad, y, leftW, hAcct)
	w.cardHead(pad, y, leftW, "账号", "登录后可下载高清 / 会员清晰度")

	ay := y + w.s(46)
	w.add(element{
		id: "acct-state", kind: elStatusPill, text: w.acctText, status: w.acctTone,
		rect: walk.Rectangle{X: pad + cp, Y: ay, Width: w.s(40) + w.measureHint(w.acctText), Height: w.s(28)},
	})
	ay += w.s(38)
	loginBtns := []struct {
		id, txt string
		tab     int
		width   int
	}{
		{"login-qr", "扫码登录", loginTabQR, 86},
		{"login-tv", "电视端", loginTabTV, 72},
		{"login-browser", "浏览器导入", loginTabBrowser, 96},
		{"login-file", "凭证文件", loginTabFile, 86},
		{"login-pw", "账号密码", loginTabPassword, 86},
	}
	bx := pad + cp
	for i, b := range loginBtns {
		tone := btnGhost
		if i == 0 {
			tone = btnMini
		}
		w.button(b.id, b.txt, tone, bx, ay, b.width)
		bx += w.s(b.width + 8)
	}
	w.button("logout", "退出登录", btnGhost, pad+leftW-cp-w.s(88), ay, 88)

	// ---- 右栏：任务台 ----
	// 结构照搬老版（Python 版）网页的任务台，因为用户明确要求参考那个 UI：
	// 进度条 + 百分比 → 速度曲线 → 三张指标卡（速度/已下载/剩余时间）
	// → 阶段步进器 → 阶段文案 → 常驻阶段行 → 按钮区。
	//
	// 老版在空闲时会把曲线/指标卡/步进器/阶段文案整块隐藏（app.js 的
	// paintProgress：没有 prog.stage 就整排 add("hidden")），只留进度条
	// 和「等待任务…」。这里对齐同样的行为 —— 空闲时不摆一排 "--" 占位，
	// 卡片高度也随之收缩，把纵向空间让给日志。
	ry := top
	rInX := rightX + cp
	rInW := rightW - 2*cp

	showProg := w.hasTaskUI()
	w.progShown = showProg

	// 先把纵向坐标走一遍，算出卡高，再落卡片和元素（卡片必须先入列，
	// 画的时候按切片顺序才不会盖住内容）。
	ty := ry + w.s(46) + w.s(10) + w.s(14) // 进度条行之后
	if showProg {
		ty += w.s(34 + 8)  // 速度曲线
		ty += w.s(46 + 10) // 三张指标卡
		ty += w.s(24 + 8)  // 阶段步进器
		ty += w.s(20 + 8)  // 阶段文案
	}
	ty += w.s(20 + 10) // 常驻阶段行（老版 .stage）
	hTask := ty - ry + w.s(lgBtnH) + w.s(8) + w.s(lgBtnH) + w.s(6)
	w.card("card-task", rightX, ry, rightW, hTask)
	w.cardHead(rightX, ry, rightW, "任务台", w.stageTxt, "stagehint")

	// 进度条在左，百分比在右（老版.progress-wrap 是一个 flex 行）
	barW := rInW - w.s(52)
	w.els = append(w.els, element{
		id: "progbar", kind: elProgress, frac: w.progressFrac(),
		rect: walk.Rectangle{X: rInX, Y: ry + w.s(46), Width: barW, Height: w.s(10)},
	})
	w.els = append(w.els, element{
		id: "progpct", kind: elText, text: w.progPct(), disabled: w.prog.Total <= 0,
		rect: walk.Rectangle{X: rInX + barW + w.s(8), Y: ry + w.s(42), Width: w.s(44), Height: w.s(16)},
	})

	// y 在左栏布局里已声明，这里从进度条行之后接着走
	y = ry + w.s(46) + w.s(10) + w.s(14)
	if showProg {
		// 速度曲线（老版 .spark-wrap：高 24px，底部一条分隔线，右下角"速度曲线"标注）
		w.els = append(w.els, element{
			id: "spark", kind: elSpark, spark: w.spark,
			rect: walk.Rectangle{X: rInX, Y: y, Width: rInW, Height: w.s(34)},
		})
		y += w.s(34 + 8)

		// 三张指标卡。老版网格 1fr 1.35fr 1fr —— 中间"已下载"要放"6.4/13.5 MB"，
		// 得最宽。这里按同样的比例分配宽度。
		//
		// 宽度必须严格加起来等于可用宽：早先中间那张有个"保底不小于两侧"的兜底，
		// 窗口一窄三张卡的合计就超出卡片右边界，最后一张被裁掉半截。
		// 现在改成先算两侧各占1 份、余下的都给中间 —— 两侧等宽且总和恒定，
		// 任何宽度下都不会溢出。
		statY := y
		statH := w.s(46)
		statGap := w.s(6)
		statFree := rInW - 2*statGap
		sideW := statFree / 3      // 两侧各 1 份
		midW := statFree - 2*sideW // 中间吃掉余数（含除不尽的零头）
		if midW < sideW {
			// 极窄窗口（理论上不会发生）：让三张等分，宁可挤一点也不溢出
			sideW = statFree / 3
			midW = statFree - 2*sideW
		}
		for _, st := range []struct {
			id, k, v string
			x, width  int
		}{
			{"stat-speed", "速度", w.statSpeed, rInX, sideW},
			{"stat-size", "已下载", w.statSize, rInX + sideW + statGap, midW},
			{"stat-eta", "剩余时间", w.statETA, rInX + sideW + statGap + midW + statGap, sideW},
		} {
			w.els = append(w.els, element{
				id: st.id, kind: elStat, sub: st.k, text: w.valueOrDash(st.v),
				rect: walk.Rectangle{X: st.x, Y: statY, Width: st.width, Height: statH},
			})
		}
		y += w.s(46 + 10)

		// 阶段步进器：解析 → 视频 → 音频 → 混流
		w.els = append(w.els, element{
			id: "steps", kind: elStepStrip, opts: stageOrder, stepCur: w.stepCur,
			stepSkip: w.stepSkip(),
			rect:     walk.Rectangle{X: rInX, Y: y, Width: rInW, Height: w.s(24)},
		})
		y += w.s(24 + 8)

		// 阶段文案（老版 .pstage）
		w.els = append(w.els, element{
			id: "progtext", kind: elText, text: w.progText, disabled: w.progText == "",
			rect: walk.Rectangle{X: rInX, Y: y, Width: rInW, Height: w.s(20)},
		})
		y += w.s(20 + 8)
	}

	// 常驻阶段行（老版 .stage / #task-stage）：空闲是「等待任务…」，
	// 跑起来之后是最近一行日志的原文。
	w.stagelineW = rInW
	w.els = append(w.els, element{
		id: "stageline", kind: elText, text: w.stageLineText(),
		rect: walk.Rectangle{X: rInX, Y: y, Width: rInW, Height: w.s(20)},
	})
	y += w.s(20 + 10)

	// 按钮分两行：第一行主操作，第二行停止 / 清空 / 打开目录。
	// 这样比把四颗挤在一行更好点，也不会在窄窗口下被挤到卡片外。
	// 这几颗宽度都是从 rInW（物理像素）直接算的，必须走 buttonPx。
	btnH := w.s(lgBtnH)
	w.buttonPx("start", "开始下载", btnPrimary,
		walk.Rectangle{X: rInX, Y: y, Width: rInW, Height: btnH}).disabled = w.isBusy()
	y += btnH + w.s(8)

	bw2 := (rInW - 2*w.s(8)) / 3
	w.buttonPx("stop", "停止", btnDanger,
		walk.Rectangle{X: rInX, Y: y, Width: bw2, Height: btnH}).disabled = !w.isBusy()
	w.buttonPx("clean", "清空日志", btnGhost,
		walk.Rectangle{X: rInX + bw2 + w.s(8), Y: y, Width: bw2, Height: btnH})
	// 打开下载目录：与左侧输入框那颗「打开」各管一路 ——
	// 那颗开的是设置里的目标目录，这颗开的是实际落盘目录（多 P / 自定义文件名后可能不同）。
	w.buttonPx("opendir-out", "打开下载目录", btnGhost,
		walk.Rectangle{X: rInX + (bw2+w.s(8))*2, Y: y, Width: bw2, Height: btnH})

	// ---- 右栏：日志 ----
	ly := ry + hTask + gap
	lh := H - pad - ly
	if lh < w.s(120) {
		lh = w.s(120)
	}
	w.card("card-log", rightX, ly, rightW, lh)
	w.cardHead(rightX, ly, rightW, "日志控制台", "")
	// 底栏给「自动滚动 + 行数」留一条（老版 .console-foot）
	footH := w.s(26)
	w.consoleRect = walk.Rectangle{X: rightX + w.s(10), Y: ly + w.s(40), Width: rightW - w.s(20), Height: lh - w.s(50) - footH}
	w.els = append(w.els, element{kind: elConsole, rect: w.consoleRect})
	w.els = append(w.els, element{
		id: "autoscroll", kind: elCheck, text: "自动滚动", on: w.autoscroll,
		rect: walk.Rectangle{X: rightX + cp, Y: ly + lh - footH - w.s(2), Width: w.s(96), Height: w.s(20)},
	})
	w.logCountRight = rightX + rightW - cp
	cnt := w.logCountText()
	w.els = append(w.els, element{
		id: "logcount", kind: elHint, text: cnt,
		rect: walk.Rectangle{
			X: w.logCountRight - w.measureHint(cnt), Y: ly + lh - footH,
			Width: w.measureHint(cnt), Height: w.s(20),
		},
	})

	// ---- 原生输入框就位 ----
	w.placeInputs()
	w.refreshProgressView()
}

// placeInputs 把原生 EDIT 摆到自绘输入框内部。
//
// 两边共用同一套坐标：自绘层与原生 EDIT 是同一个父窗口的子窗口，原点相同，
// 所以把元素的矩形往内收一点就是控件该在的位置 —— 不需要任何换算。
func (w *Win) placeInputs() {
	inset := w.s(9)
	inner := func(id string, dy, sub int) (walk.Rectangle, bool) {
		r := w.find(id)
		if r == nil {
			return walk.Rectangle{}, false
		}
		return walk.Rectangle{
			X: r.X + inset, Y: r.Y + w.s(dy),
			Width: r.Width - 2*inset - w.s(sub), Height: w.s(22),
		}, true
	}
	if r := w.find("addr"); r != nil {
		w.addr.Bounds(walk.Rectangle{
			X: r.X + inset, Y: r.Y + w.s(7),
			Width: r.Width - 2*inset, Height: r.Height - w.s(14),
		})
	}
	if r, ok := inner("input-par", 5, 0); ok {
		w.par.Bounds(r)
	}
	if r, ok := inner("input-page", 5, 0); ok {
		w.page.Bounds(r)
	}
	// 目录输入框右边要留出「选择 / 打开」两颗按钮的位置
	if r, ok := inner("input-dir", 5, 158); ok {
		w.dir.Bounds(r)
	}
	// 摆完位一律把 EDIT 提到自绘层之上：自绘层是铺满客户区的一块画布，
	// 一旦它被 walk 抬到前面（建控件 / 显示 / 布局都会），点击就会全被它
	// 接走，表现为「文本框点不进去、没有光标、敲不进字」。见 nativeedit.go
	// 的 BringToTop 注释。placeInputs 每次布局重建都会跑，所以这里提一次
	// 就能一直保持正确。
	for _, e := range []*nativeEdit{w.addr, w.page, w.dir, w.par} {
		e.BringToTop()
	}
}

// find 返回指定 id 的矩形。
func (w *Win) find(id string) *walk.Rectangle {
	for i := range w.els {
		if w.els[i].id == id {
			return &w.els[i].rect
		}
	}
	return nil
}

// measureHint / measureSmall 用来给状态胶囊、复选框量宽度。
//
// 这里不查字体度量（布局阶段还没有 Canvas）：中文字符按 1 个字宽、
// 其他按 0.62 个估算，结果偏大一点没关系 —— 胶囊宽一点比文字被截断好。
func (w *Win) measureHint(s string) int  { return w.measureText(s, 9) }
func (w *Win) measureSmall(s string) int { return w.measureText(s, 9) }

func (w *Win) measureText(s string, sizePt int) int {
	return estimateTextWidth(s, sizePt, w.scale)
}

// wrapText 按估算宽度把文本贪心切成多行 —— 完整显示，不截断、不加省略号。
// 用户明确要求解析标题与详情不要用省略号，放不下就换行，卡片随之加高。
// 简化为逐 rune 贪心（不特意保英文单词完整）：标题场景下误差可忽略。
func wrapText(s string, sizePt int, maxW int, scale float64) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var lines []string
	start := 0
	for start < len(runes) {
		// 二分找本行最多能放下的字符数
		lo, hi := 1, len(runes)-start
		for lo < hi {
			mid := (lo + hi + 1) / 2
			if estimateTextWidth(string(runes[start:start+mid]), sizePt, scale) <= maxW {
				lo = mid
			} else {
				hi = mid - 1
			}
		}
		if lo < 1 {
			lo = 1 // 单字符超宽也强制放一行，避免死循环
		}
		lines = append(lines, string(runes[start:start+lo]))
		start += lo
	}
	return lines
}

// ---------------------------------------------------------------------------
// 布局小工具

func (w *Win) card(id string, x, y, width, height int) {
	w.els = append(w.els, element{
		id: id, kind: elCard,
		rect: walk.Rectangle{X: x, Y: y, Width: width, Height: height},
	})
}

// cardHead 画卡片标题行。hintID 非空时给副标题挂一个 id —— 任务台的阶段文案
// 会频繁变化，需要能原地改文字而不重算整套布局。
func (w *Win) cardHead(x, y, width int, title, hint string, hintID ...string) {
	cp := w.s(lgCardPad)
	w.els = append(w.els, element{
		kind: elCardTitle, text: title,
		rect: walk.Rectangle{X: x + cp, Y: y + w.s(13), Width: w.s(180), Height: w.s(22)},
	})
	if hint == "" {
		return
	}
	id := ""
	if len(hintID) > 0 {
		id = hintID[0]
	}
	tw := w.measureHint(hint)
	// 提示文案可能很长（多 P 下载时是 "(1/3) 视频标题…"），量出来的宽度
	// 会把元素顶出卡片右缘。超过可用宽就截断加省略号。
	avail := width - 2*cp - w.measureSmall(title) - w.s(10)
	if avail > 0 && tw > avail {
		tw = avail
		hint = w.gfx.truncateToWidth(hint, w.font.Small, avail)
	}
	w.els = append(w.els, element{
		id: id, kind: elHint, text: hint, sub: "cardhint",
		rect: walk.Rectangle{X: x + cp + w.measureSmall(title) + w.s(10), Y: y + w.s(15),
			Width: tw, Height: w.s(20)},
	})
}

func (w *Win) button(id, txt string, tone buttonTone, x, y, width int) *element {
	w.els = append(w.els, element{
		id: id, kind: elButton, text: txt, tone: tone,
		rect: walk.Rectangle{X: x, Y: y, Width: w.s(width), Height: w.s(lgBtnH)},
	})
	return &w.els[len(w.els)-1]
}

// buttonPx 用**物理像素**矩形放按钮。
//
// 任务台的通栏「开始下载」和第二行三等分按钮，宽度是从右栏的物理宽
// 直接算出来的 —— 再走 w.button() 会被 w.s() 二次放大 1.5 倍，
// 按钮冲出卡片右缘 28px（实测踩过：粉色按钮一直顶到窗口边）。
func (w *Win) buttonPx(id, txt string, tone buttonTone, r walk.Rectangle) *element {
	w.els = append(w.els, element{
		id: id, kind: elButton, text: txt, tone: tone, rect: r,
	})
	return &w.els[len(w.els)-1]
}

// pillRow 画一行「标签 + 分段选择器」。
func (w *Win) pillRow(x, y, cardW int, label, id string, opts []string, sel int) int {
	cp := w.s(lgCardPad)
	lw := w.s(56)
	w.els = append(w.els, element{
		kind: elHint, text: label,
		rect: walk.Rectangle{X: x + cp, Y: y, Width: lw, Height: w.s(lgRowH)},
	})
	e := element{
		id: id, kind: elPills, opts: opts, sel: sel,
		rect: walk.Rectangle{X: x + cp + lw, Y: y + w.s(3), Width: cardW - 2*cp - lw, Height: w.s(lgPillH)},
	}
	// 各段等分
	n := len(opts)
	if n > 0 {
		inner := walk.Rectangle{X: e.rect.X + 3, Y: e.rect.Y + 3, Width: e.rect.Width - 6, Height: e.rect.Height - 6}
		seg := inner.Width / n
		for i := 0; i < n; i++ {
			width := seg
			if i == n-1 {
				width = inner.Width - seg*(n-1)
			}
			e.optRects = append(e.optRects, walk.Rectangle{
				X: inner.X + seg*i, Y: inner.Y, Width: width, Height: inner.Height,
			})
		}
	}
	w.els = append(w.els, e)
	return y + w.s(lgRowH)
}

// twoInputRow 画一行「标签 + 输入框」× 2。
//
// 分P 与并行都是很短的数字，各占一行太浪费 —— 左栏的纵向空间要留给卡片标题。
func (w *Win) twoInputRow(x, y, cardW int, labelA, keyA, labelB, keyB string) int {
	cp := w.s(lgCardPad)
	lw := w.s(56)
	gap := w.s(26)
	total := cardW - 2*cp
	w1 := (total - 2*lw - gap) / 2
	if w1 < w.s(80) {
		w1 = w.s(80)
	}
	label := func(txt string, lx int) {
		w.els = append(w.els, element{
			kind: elHint, text: txt,
			rect: walk.Rectangle{X: lx, Y: y, Width: lw, Height: w.s(lgRowH)},
		})
	}
	box := func(key string, bx int) {
		w.els = append(w.els, element{
			id: "input-" + key, kind: elInputBox,
			rect: walk.Rectangle{X: bx, Y: y + w.s(2), Width: w1, Height: w.s(32)},
		})
	}
	label(labelA, x+cp)
	box(keyA, x+cp+lw)
	label(labelB, x+cp+lw+w1+gap)
	box(keyB, x+cp+lw+w1+gap+lw)
	return y + w.s(lgRowH)
}

// rowLabelInput 画一行「标签 + 输入框外框」，并把外框的 id 记成 input-<key>。
func (w *Win) rowLabelInput(x, y, cardW int, label, key string) int {
	cp := w.s(lgCardPad)
	lw := w.s(56)
	w.els = append(w.els, element{
		kind: elHint, text: label,
		rect: walk.Rectangle{X: x + cp, Y: y, Width: lw, Height: w.s(lgRowH)},
	})
	w.els = append(w.els, element{
		id: "input-" + key, kind: elInputBox,
		rect: walk.Rectangle{X: x + cp + lw, Y: y + w.s(2), Width: cardW - 2*cp - lw, Height: w.s(32)},
	})
	return y + w.s(lgRowH)
}

func labelsOfQuality() []string {
	out := make([]string, 0, len(qualityOpts))
	for _, o := range qualityOpts {
		out = append(out, o.Label)
	}
	return out
}

func labelsOfCodec() []string {
	out := make([]string, 0, len(codecOpts))
	for _, o := range codecOpts {
		out = append(out, o.Label)
	}
	return out
}

func (w *Win) qualityIndex() int {
	for i, o := range qualityOpts {
		if o.QN == w.cfg.Quality {
			return i
		}
	}
	return 0
}

func (w *Win) codecIndex() int {
	v := strings.ToLower(strings.TrimSpace(w.cfg.CodecOrder))
	for i, o := range codecOpts {
		if strings.EqualFold(o.Value, v) {
			return i
		}
	}
	return 0
}

func (w *Win) progressFrac() float64 {
	if w.prog.Total <= 0 {
		return 0
	}
	return float64(w.prog.Done) / float64(w.prog.Total)
}

// detailRect / consoleRect 由 rebuild 填充，鼠标与绘制共用。

// ---------------------------------------------------------------------------
// 绘制

// errNoBrush 在 GDI 句柄耗尽、连一个纯色画刷都造不出来时返回。
//
// 这种情况几乎不会发生，但一旦发生必须让绘制整帧放弃 —— 继续画会一路
// 返回 nil 画刷、最后表现为"窗口一片空白且毫无报错"，比直接报错难查得多。
var errNoBrush = fmt.Errorf("无法创建画刷（GDI 句柄可能已耗尽）")

// paint 是自绘控件的回调。
//
// 每帧都从 w.els 重新画一遍 —— 这就是「即时模式」：元素没有持久状态，
// 谁该在哪由 rebuild 算好，绘制只负责照着画。命中测试读的是同一份 w.els，
// 所以「看见的」与「点得到的」不可能对不上。
func (w *Win) paint(canvas *walk.Canvas, updateBounds walk.Rectangle) error {
	w.ensureLayout()

	// 底：整块页面底色。窗口本身也刷了这个颜色（原生 EDIT 的底色就靠它），
	// 这里再画一遍是为了局部重绘（拖动窗口时脏区可能只有一条）也是对的。
	if b := w.cw.BoundsPixels(); b.Width > 0 {
		if err := w.gfx.fill(canvas, walk.Rectangle{Width: b.Width, Height: b.Height}, 0, w.pal.Bg); err != nil {
			return err
		}
	}

	// 顶栏：一条 --surface 色的横带，压在分隔线上面
	if w.headRect.Width > 0 {
		if err := w.gfx.fill(canvas, w.headRect, 0, w.pal.Surface); err != nil {
			return err
		}
	}

	for i := range w.els {
		e := &w.els[i]
		// 只画与脏区相交的元素。拖动窗口时 updateBounds 会很小，
		// 全量重画在 4K 屏上会明显掉帧。
		if !intersects(e.rect, updateBounds) {
			continue
		}
		if e.kind == elConsole {
			if err := w.gfx.paintConsole(canvas, e, w.pal, w.font, w.logs, w.scroll); err != nil {
				return err
			}
			continue
		}
		if err := w.gfx.paintElement(canvas, e, w.pal, w.font); err != nil {
			return err
		}
	}
	return nil
}

func intersects(a, b walk.Rectangle) bool {
	if b.Width <= 0 || b.Height <= 0 {
		return true // 空脏区视为「全部」
	}
	return a.X < b.X+b.Width && b.X < a.X+a.Width &&
		a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

func ptIn(r walk.Rectangle, x, y int) bool {
	return x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height
}

// ---------------------------------------------------------------------------
// 鼠标

// hitResult 是一次命中测试的结果。
type hitResult struct {
	id    string
	opt   int
	hand  bool // 手型光标
	ibeam bool // 文本光标
}

// hit 找出坐标落在哪个可交互元素上。
//
// **必须倒着遍历**：w.els 是按绘制顺序追加的，越靠后越在上层。目录输入框
// 与「选择 / 打开」两颗按钮在几何上是重叠的（按钮摆在输入框内部），正着找
// 会先撞上输入框，于是按钮永远点不动。
func (w *Win) hit(x, y int) hitResult {
	for i := len(w.els) - 1; i >= 0; i-- {
		e := &w.els[i]
		if !ptIn(e.rect, x, y) {
			continue
		}
		switch e.kind {
		case elButton:
			if e.disabled {
				return hitResult{id: e.id, opt: -1}
			}
			return hitResult{id: e.id, opt: -1, hand: true}
		case elCheck:
			// 整行都可点，不只是那个 17px 的小方块
			return hitResult{id: e.id, opt: -1, hand: true}
		case elPills:
			opt := -1
			for j, r := range e.optRects {
				if ptIn(r, x, y) {
					opt = j
					break
				}
			}
			if opt < 0 {
				return hitResult{opt: -1}
			}
			return hitResult{id: e.id, opt: opt, hand: true}
		case elInputBox:
			return hitResult{opt: -1, ibeam: true}
		case elConsole:
			return hitResult{opt: -1}
		}
	}
	return hitResult{opt: -1}
}

func (w *Win) attachMouse() {
	if w.cw == nil {
		return
	}
	w.cw.MouseMove().Attach(func(x, y int, button walk.MouseButton) {
		w.setHover(w.hit(x, y))
	})
	w.cw.MouseDown().Attach(func(x, y int, button walk.MouseButton) {
		if button != walk.LeftButton {
			return
		}
		h := w.hit(x, y)
		if h.id == "" {
			w.pressID, w.pressOpt = "", -1
			return
		}
		w.pressID, w.pressOpt = h.id, h.opt
		w.setHover(h)
	})
	w.cw.MouseUp().Attach(func(x, y int, button walk.MouseButton) {
		if button != walk.LeftButton {
			return
		}
		id, opt := w.pressID, w.pressOpt
		w.pressID, w.pressOpt = "", -1

		h := w.hit(x, y)
		// 按下与松开必须在同一个控件（分段选择器还要同一段）上才算点击，
		// 否则中途滑出去也会触发 —— 那是很烦人的误操作。
		if id != "" && h.id == id && h.opt == opt {
			w.setHover(h)
			w.activate(id, opt)
			return
		}
		w.setHover(h)
	})
	w.cw.MouseWheel().Attach(func(x, y int, button walk.MouseButton) {
		if !ptIn(w.consoleRect, x, y) {
			return
		}
		w.scrollLogs(walk.MouseWheelEventDelta(button) / 120)
	})
}

// setHover 更新悬停态，变了才重画。
//
// 鼠标每动一像素都会走到这儿，无条件 Invalidate 会让整个窗口一直以
// 鼠标采样率重绘。
func (w *Win) setHover(h hitResult) {
	if w.hoverID == h.id && w.hoverOpt == h.opt {
		return
	}
	w.hoverID, w.hoverOpt = h.id, h.opt
	for i := range w.els {
		e := &w.els[i]
		e.hover = e.id != "" && e.id == h.id
		e.hoverOpt = -1
		if e.kind == elPills && e.hover {
			e.hoverOpt = h.opt
		}
	}
	if w.cw != nil {
		switch {
		case h.hand:
			w.cw.SetCursor(walk.CursorHand())
		case h.ibeam:
			w.cw.SetCursor(walk.CursorIBeam())
		default:
			w.cw.SetCursor(walk.CursorArrow())
		}
		_ = w.cw.Invalidate()
	}
}

// scrollLogs 滚日志。delta > 0 表示往下滚（看更新的），< 0 往上翻历史。
func (w *Win) scrollLogs(delta int) {
	if delta == 0 {
		delta = 1
	}
	maxScroll := len(w.logs) - 1
	if maxScroll < 0 {
		maxScroll = 0
	}
	// w.scroll 是「距离底部还有多少行」：0 = 贴着最新一条。
	next := w.scroll - delta*3
	if next < 0 {
		next = 0
	}
	if next > maxScroll {
		next = maxScroll
	}
	if next == w.scroll {
		return
	}
	w.scroll = next
	if w.cw != nil {
		_ = w.cw.Invalidate()
	}
}

// activate 执行一次点击。
func (w *Win) activate(id string, opt int) {
	switch id {
	case "quit":
		w.onQuit()
	case "recheck":
		w.onRecheck()
	case "theme":
		w.onToggleTheme()
	case "parse":
		w.onParse()
	case "paste":
		w.onPaste()
	case "clear-addr":
		w.onClearAddr()
	case "pickdir":
		w.onPickDir()
	case "opendir":
		w.onOpenDir()
	case "opendir-out":
		w.onOpenOutputDir()
	case "start":
		w.onStart()
	case "stop":
		w.onStop()
	case "clean":
		w.onCleanLog()
	case "logout":
		w.onLogout()
	case "login-qr":
		w.openLoginDialog(loginTabQR)
	case "login-tv":
		w.openLoginDialog(loginTabTV)
	case "login-browser":
		w.openLoginDialog(loginTabBrowser)
	case "login-file":
		w.openLoginDialog(loginTabFile)
	case "login-pw":
		w.openLoginDialog(loginTabPassword)
	case "quality", "codec":
		w.selectPill(id, opt)
	default:
		if strings.HasPrefix(id, "check-") {
			w.toggleCheck(id)
		} else if id == "autoscroll" {
			w.toggleCheck(id)
		}
	}
}

// ---------------------------------------------------------------------------
// 界面状态

// relayout 强制重算布局并重画。
//
// 文案或状态变了就得走一遍 —— 元素是即时模式里的临时对象，不重算的话
// 界面还停在上一帧的文案上。
func (w *Win) relayout() {
	w.laidW, w.laidH = -1, -1
	w.ensureLayout()
	if w.cw != nil {
		_ = w.cw.Invalidate()
	}
}

// refreshProgressView 原地刷新进度相关的元素。
//
// 进度回调一秒能来十几次，走 relayout 会把整套几何重算十几遍，纯属浪费。
// 这里只改已存在元素的内容 —— 但**布局尺寸变了就得走 relayout**，
// 所以凡是要改rect 的（进度百分比随窗口宽度变化）都在 ensureLayout 里做。
func (w *Win) refreshProgressView() {
	for i := range w.els {
		switch w.els[i].id {
		case "progtext":
			w.els[i].text = w.progText
			w.els[i].disabled = w.progText == ""
		case "progpct":
			w.els[i].text = w.progPct()
			w.els[i].disabled = w.prog.Total <= 0
		case "stat-speed":
			w.els[i].text = w.valueOrDash(w.statSpeed)
		case "stat-size":
			w.els[i].text = w.valueOrDash(w.statSize)
		case "stat-eta":
			w.els[i].text = w.valueOrDash(w.statETA)
		case "spark":
			// 切片要整体换掉而不是 append 到底层数组 —— 后者会让
			// 采样无限增长，几小时后内存明显上涨。
			w.els[i].spark = w.spark
		case "steps":
			w.els[i].stepCur = w.stepCur
			w.els[i].stepSkip = w.stepSkip()
		case "progbar":
			w.els[i].frac = w.progressFrac()
		case "stagehint":
			// 提示文案是动态的（「就绪」→「下载中」→ 多 P 的 "(1/3) 标题…"），
			// 而布局时 rect.Width 是按初始文案量的 —— 文案一变长就会被
			// 绘制期的按宽截断裁成「下…」。这里把宽度放开到任务台卡片的
			// 可用宽（标题右侧到卡右内边距），超长部分由绘制期统一截断。
			w.els[i].text = w.stageTxt
			for j := range w.els {
				if w.els[j].id == "card-task" {
					cp := w.s(lgCardPad)
					if avail := w.els[j].rect.Width - 2*cp - w.measureSmall("任务台") - w.s(10); avail > 0 {
						w.els[i].rect.Width = avail
					}
					break
				}
			}
		case "stageline":
			w.els[i].text = w.stageLineText()
		case "logcount":
			// 行数在变：宽度按新文案重算（留 2px 余量，避免估算刚好等于
			// 宽度时被绘制期截断切掉「行」字），再按右缘重新对齐。
			w.els[i].text = w.logCountText()
			w.els[i].rect.Width = w.measureHint(w.logCountText()) + 2
			w.els[i].rect.X = w.logCountRight - w.els[i].rect.Width
		case "autoscroll":
			w.els[i].on = w.autoscroll
		}
	}
}

// progPct 返回进度百分比文本。
//
// 没有总量时对齐老版 setBar 的空闲/进行中文案，而不是一个孤零零的 "--"：
// 空闲「就绪」、跑起来还没探到体积「进行中」。
func (w *Win) progPct() string {
	if w.prog.Total <= 0 {
		if w.isBusy() {
			return "进行中"
		}
		return "就绪"
	}
	pct := float64(w.prog.Done) * 100 / float64(w.prog.Total)
	if pct > 100 {
		pct = 100 // 收尾阶段 Done 可能略超 Total（分块向上取整），别显示 103%
	}
	return fmt.Sprintf("%.1f%%", pct)
}

// hasTaskUI 报告任务台的「进度详情区」（曲线/指标卡/步进器/阶段文案）
// 该不该展开。对齐老版 paintProgress 的显隐条件（!prog.stage 就整块隐藏）：
// 空闲时只留进度条和「等待任务…」，跑过一次之后（Total>0）保持展开，
// 让用户能看到最终数值，跟老版完成后的表现一致。
func (w *Win) hasTaskUI() bool {
	return w.isBusy() || w.prog.Total > 0
}

// stageLineText 返回常驻阶段行的文案（老版 #task-stage）：
// 还没跑过任何任务是「等待任务…」，之后是最近一行日志去掉时间戳。
func (w *Win) stageLineText() string {
	if len(w.logs) == 0 {
		return "等待任务…"
	}
	last := w.logs[len(w.logs)-1].text
	if strings.HasPrefix(last, "[") {
		if i := strings.Index(last, "] "); i >= 0 {
			last = last[i+2:]
		}
	}
	if w.stagelineW > 0 && w.gfx != nil {
		last = w.gfx.truncateToWidth(last, w.font.Small, w.stagelineW)
	}
	return last
}

// logCountText 日志底栏右侧的行数。
func (w *Win) logCountText() string {
	return fmt.Sprintf("%d 行", len(w.logs))
}

// valueOrDash 把空字符串换成 "--"。
//
// 老版在无数据时显示 "--"（`$("ps-speed").textContent = running ? ... : "--"`），
// 而 g.text 遇到空串会直接跳过不画 —— 结果卡片里就只剩标签、没有数值，
// 看起来像没做完。统一在这里兜住。
func (w *Win) valueOrDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "--"
	}
	return s
}

// stepSkip 返回各阶段本次是否隐藏。
//
// 老版会在已知本次选了哪些流时，把没选中的那一步藏起来（
// `if (s === "video") el.classList.toggle("hidden", known && !prog.hasVideo)`）。
// 我们这边拿不到"本次是否选了视频流"的可靠信息（进度结构里没有这两个字段），
// 所以一律不隐藏 —— 显示四步但其中某步很快跳过去，比少一步更好理解。
func (w *Win) stepSkip() []bool {
	return make([]bool, len(stageOrder))
}

// stepIndexFromLabel 从阶段文案里认出当前处在第几步。
//
// prog.Label 是自由文案（"正在下载视频流…"之类），没有阶段编号可用，
// 只能按关键词猜。猜不中就退回第 0 步 —— 步进器本身就是辅助信息，
// 猜错不致命，而"解析"作为默认起点在绝大多数时候都是对的。
//
// ⚠️ 「视频」这一支必须先排除解析类关键词。实测踩过：「获取视频信息…」
// 里既有"获取"（解析的信号）又有"视频"，若不看排除就会误判成第 1 步，
// 而它其实还在解析阶段。反过来「正在下载视频流…」不含任何解析词，
// 用排除法能正确落到第 1 步。
func stepIndexFromLabel(label string, running bool) int {
	if !running {
		return -1
	}
	switch {
	case strings.Contains(label, "混流") || strings.Contains(label, "封装") || strings.Contains(label, "合并"):
		return 3
	case strings.Contains(label, "音频"):
		return 2
	// "下载视频流"是第 1 步，但"获取视频信息"仍在解析阶段 ——
	// 所以只在排除了解析类关键词之后才认成视频流。
	case strings.Contains(label, "视频") && !looksLikeParse(label):
		return 1
	default:
		return 0
	}
}

// looksLikeParse 判断文案是否属于「解析/取信息」阶段。
var parseWords = []string{"解析", "获取", "准备", "查询", "信息", "探测", "校验"}

// 判定用的关键词表是包级只读的，循环里不建新切片。
func looksLikeParse(label string) bool {
	for _, k := range parseWords {
		if strings.Contains(label, k) {
			return true
		}
	}
	return false
}

// invalidate 请求重画，后台线程也要能用。
func (w *Win) invalidate() {
	if w.cw != nil {
		_ = w.cw.Invalidate()
	}
}

func (w *Win) selectPill(id string, opt int) {
	if opt < 0 {
		return
	}
	switch id {
	case "quality":
		if opt >= len(qualityOpts) {
			return
		}
		q := qualityOpts[opt].QN
		w.cfg.Quality = q
		w.mu.Lock()
		if len(w.qualSel) > 0 {
			w.qualSel[0] = q
		}
		w.mu.Unlock()
	case "codec":
		if opt >= len(codecOpts) {
			return
		}
		w.cfg.CodecOrder = codecOpts[opt].Value
	}
	w.saveConfig()
	w.relayout()
}

func (w *Win) toggleCheck(id string) {
	switch id {
	case "autoscroll":
		// 只影响本次会话的阅读习惯，不落盘 —— 老版也不记这个。
		w.autoscroll = !w.autoscroll
		w.relayout()
		return
	case "check-danmaku":
		w.danmaku = !w.danmaku
		w.cfg.Danmaku = extraValue(w.danmaku, app.DanmakuASS)
	case "check-subtitle":
		w.subtitle = !w.subtitle
		w.cfg.Subtitle = extraValue(w.subtitle, app.SubtitleSRT)
	case "check-cover":
		w.cover = !w.cover
		w.cfg.SaveCover = w.cover
	case "check-keeptemp":
		w.keepTemp = !w.keepTemp
		w.cfg.KeepTemp = w.keepTemp
	default:
		return
	}
	w.saveConfig()
	w.relayout()
}

// extraValue 把复选框状态翻译成配置里的枚举值。
//
// 关掉必须写成明确的 "off"，不能写成空串 —— 空串在下载层的意思是
// 「跟随配置」，会被理解成没提要求，于是弹幕照样下。
func extraValue(on bool, kind string) string {
	if on {
		return kind
	}
	return app.ExtraOff
}

func (w *Win) saveConfig() {
	if err := w.cfg.Save(); err != nil {
		w.log("配置没能保存：%v", err)
	}
}

// onToggleTheme 切换明暗主题。配置记的是用户的选择，下次启动沿用。
func (w *Win) onToggleTheme() {
	w.dark = !w.dark
	w.pal = paletteFor(w.dark)
	if w.dark {
		w.cfg.Theme = app.ThemeDark
	} else {
		w.cfg.Theme = app.ThemeLight
	}
	w.saveConfig()
	if err := w.applyStageBackground(); err != nil {
		w.log("主题切换失败：%v", err)
	}
	w.applyPaletteToInputs()
	w.relayout()
}

// ---------------------------------------------------------------------------
// Hook（全部由后台 goroutine 调用）

func (w *Win) log(format string, args ...any) {
	w.onLog(fmt.Sprintf(format, args...))
}

// onLog 追加一行日志。
//
// Synchronize 是投递而不是同步等待（内部走 PostMessage），所以在 UI 线程
// 上调它也不会死锁，只是这行会等当前消息处理完才真正落到 logs 里。
func (w *Win) onLog(line string) {
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		stamp := time.Now().Format("15:04:05")
		added := 0
		for _, part := range splitLines(line) {
			w.logs = append(w.logs, logLine{level: levelOf(part), text: "[" + stamp + "] " + part})
			added++
		}
		// 日志太长会把内存吃掉：留最近 5000 行已经很够用了
		if n := len(w.logs); n > 5000 {
			w.logs = append([]logLine(nil), w.logs[n-5000:]...)
		}
		// 自动滚动关掉时保持视口绝对位置不动（scroll 是「距底部还有几行」，
		// 新增了 added 行就要把距离拉长同样的量），方便往上翻历史。
		if !w.autoscroll {
			w.scroll += added
		}
		w.clampScroll()
		// 常驻阶段行跟着最新一行日志走，行数计数也是
		for i := range w.els {
			switch w.els[i].id {
			case "stageline":
				w.els[i].text = w.stageLineText()
			case "logcount":
				w.els[i].text = w.logCountText()
				w.els[i].rect.X = w.logCountRight - w.els[i].rect.Width
			}
		}
		w.invalidate()
	})
}

// levelOf 按行首标记挑颜色。
//
// 下载层用 "✓" / "✗" / "!" 开头表达结果，这里认这几个字符就够，
// 不需要它额外传一个等级字段。
func levelOf(s string) string {
	t := strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(t, "✓"):
		return "ok"
	case strings.HasPrefix(t, "✗"):
		return "bad"
	case strings.HasPrefix(t, "!"):
		return "warn"
	case strings.HasPrefix(t, "·"):
		return "dim"
	}
	return ""
}

func (w *Win) clampScroll() {
	maxScroll := len(w.logs) - 1
	if maxScroll < 0 {
		maxScroll = 0
	}
	if w.scroll > maxScroll {
		w.scroll = maxScroll
	}
}

func (w *Win) onStage(stage string) {
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		w.stageTxt = stage
		w.refreshProgressView()
		w.invalidate()
	})
}

func (w *Win) onProgress(p app.JobProgress) {
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		w.prog = p

		// 阶段文案（步进器下方那一行）。只放阶段本身，百分比与体积都另有位置。
		w.progText = p.Label

		// 三张指标卡。任务不在跑时统一显示 "--"，跟老版一致 ——
		// 停下来之后还留着 "949 KB/s" 会让人以为还在下。
		running := p.Speed > 0 || (p.Total > 0 && p.Done < p.Total)
		w.stepCur = stepIndexFromLabel(p.Label, running)
		if !running {
			w.statSpeed, w.statSize, w.statETA = "--", "--", "--"
			// 曲线在非下载态清空：老版 paintSpark 里 running 为假时
			// 直接 sparkData=[] 并隐藏整块，否则会留下一条静止的高位线误导人。
			w.spark = nil
			w.sparkRunning = false
		} else {
			w.statSpeed = humanSize(int64(p.Speed)) + "/s"
			if p.Total > 0 {
				w.statSize = humanSize(p.Done) + " / " + humanSize(p.Total)
			} else {
				w.statSize = humanSize(p.Done)
			}
			if p.ETA > 0 {
				w.statETA = humanETA(p.ETA)
			} else {
				w.statETA = "测算中"
			}
			// 采样速度给曲线。混流阶段不画（那时速度已无意义，
			// 老版也是 running && stage !== "mux" 才画）。
			if p.Speed > 0 && !strings.Contains(p.Label, "混流") {
				w.spark = append(w.spark, float64(p.Speed))
				if len(w.spark) > sparkPoints {
					// 超出就从头切掉一段。这里必须新建切片，
					// 直接改底层数组会把cap 一直带着增长。
					cp := make([]float64, sparkPoints, sparkPoints*2)
					copy(cp, w.spark[len(w.spark)-sparkPoints:])
					w.spark = cp
				}
				w.sparkRunning = true
			}
		}

		// 进度详情区的展开/收起是布局级变化：状态翻转的那一次必须
		// 走 relayout 重建元素（卡片高度也要变），其余时候原地刷新。
		if need := w.hasTaskUI(); need != w.progShown {
			w.relayout()
		} else {
			w.refreshProgressView()
		}
		w.invalidate()
	})
}

func (w *Win) onOutput(path string) {
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		w.outputs = append(w.outputs, path)
	})
}

// setBusy 切换「运行中」的界面态。
func (w *Win) setBusy(busy bool) {
	w.mu.Lock()
	w.busy = busy
	w.mu.Unlock()
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		// 忙碌态翻转往往伴随进度详情区的展开/收起（hasTaskUI 含 isBusy），
		// 这种时候要走 relayout 整体重建。
		if need := w.hasTaskUI(); need != w.progShown {
			w.relayout()
			return
		}
		for i := range w.els {
			switch w.els[i].id {
			case "start", "parse":
				w.els[i].disabled = busy
			case "stop":
				w.els[i].disabled = !busy
			}
		}
		w.invalidate()
	})
}

func (w *Win) isBusy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy
}

// ---------------------------------------------------------------------------
// 动作

func (w *Win) onQuit() {
	if w.mw != nil {
		w.mw.Close()
	}
}

func (w *Win) onPaste() {
	if w.addr == nil {
		return
	}
	text, err := walk.Clipboard().Text()
	if err != nil || strings.TrimSpace(text) == "" {
		w.log("剪贴板里没有文本")
		return
	}
	w.addr.SetText(strings.TrimSpace(text))
	w.log("已粘贴 %d 个字符", len([]rune(text)))
}

func (w *Win) onClearAddr() {
	if w.addr != nil {
		w.addr.SetText("")
	}
	w.mu.Lock()
	w.items = nil
	w.qualSel = nil
	w.mu.Unlock()
	w.infoTxt = "尚未解析"
	w.detail = ""
	w.relayout()
}

func (w *Win) onParse() {
	if w.addr == nil || w.isBusy() {
		return
	}
	text := w.addr.Text()
	if strings.TrimSpace(text) == "" {
		w.infoTxt = "尚未解析"
		w.detail = "请先粘贴或输入视频地址"
		w.relayout()
		return
	}

	w.setBusy(true)
	w.infoTxt = "正在解析…"
	w.detail = ""
	w.relayout()

	go func() {
		defer w.setBusy(false)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		items, err := w.runner.Resolve(ctx, text)
		if err != nil {
			w.log("✗ %v", err)
			w.mw.Synchronize(func() {
				w.infoTxt = "解析失败"
				w.detail = err.Error()
				w.relayout()
			})
			return
		}
		w.mu.Lock()
		w.items = items
		w.qualSel = make([]int, len(items))
		w.mu.Unlock()

		first := items[0]
		w.mw.Synchronize(func() {
			w.infoTxt = first.DisplayName()
			w.detail = describe(first)
			w.relayout()
		})
		for _, it := range items {
			w.log("解析到：%s（%d 个分P）", it.Info.Title, len(it.Pages))
		}
	}()
}

func (w *Win) onPickDir() {
	if w.dir == nil {
		return
	}
	dlg := new(walk.FileDialog)
	dlg.Title = "选择保存目录"
	// ⚠️ 故意不设 InitialDirPath：walk 把它当作浏览树的**根**（PidlRoot）。
	// 设了之后树里只剩当前目录这一层、去不了别的盘 —— 复现下来对话框里
	// 只有一个「downloads」节点，这就是「无法选择保存目录」的原因。
	// 留空时 SHParseDisplayName 失败、根落在桌面，全盘可浏览，与老版
	// FolderBrowserDialog 行为一致。代价是无法预选当前目录（walk 的
	// 回调只处理 BFFM_SELCHANGED，没有发 BFFM_SETSELECTION 的逻辑）。
	ok, err := dlg.ShowBrowseFolder(w.mw)
	if err != nil {
		w.log("选择目录失败：%v", err)
		return
	}
	if !ok {
		return
	}
	w.dir.SetText(dlg.FilePath)
	w.cfg.DownloadDir = dlg.FilePath
	w.saveConfig()
}

func (w *Win) currentDir() string {
	dir := ""
	if w.dir != nil {
		dir = strings.TrimSpace(w.dir.Text())
	}
	if dir == "" {
		dir = w.cfg.DownloadDir
	}
	return dir
}

func (w *Win) onOpenDir() {
	dir := w.currentDir()
	if dir == "" {
		w.log("还没有设置保存目录")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.log("打不开目录：%v", err)
		return
	}
	cmd := exec.Command("explorer.exe", filepath.Clean(dir))
	if err := cmd.Start(); err != nil {
		w.log("打开目录失败：%v", err)
		return
	}
	// explorer 打开成功时返回的是非 0 退出码，不能拿 Wait 的结果当判据，
	// 这里只是把进程收掉，避免留下僵尸句柄。
	go func() { _ = cmd.Wait() }()
}

// onOpenOutputDir 打开下载产物所在目录。
//
// 与 onOpenDir 的区别：那颗开的是「设置里填的目标目录」，这颗优先开
// 「实际落盘的目录」—— 多 P 任务、弹幕/封面另存、或用户中途改过路径时，
// 两者可能不一致，看目录得看后者。
// 若已经知道产出文件，直接让资源管理器选中它们，省得自己翻。
func (w *Win) onOpenOutputDir() {
	dir := w.outputDir()
	if dir == "" {
		w.log("还没有下载过文件，先完成一次下载再打开目录")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		w.log("打不开目录：%v", err)
		return
	}

	// explorer /select 只接受单个路径，所以有多个产出时退回开目录本身。
	var target string
	if files := w.existingOutputs(); len(files) == 1 {
		target = filepath.Join(dir, files[0])
	}
	if target == "" {
		target = dir
	}
	cmd := exec.Command("explorer.exe", target)
	if err := cmd.Start(); err != nil {
		w.log("打开目录失败：%v", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}

// outputDir 推断产出目录：优先用最后一次下载的实测路径，其次用设置里的目录。
// outputs 里存的是成品完整路径（app.Hooks.Output 回报的 dest）。
func (w *Win) outputDir() string {
	w.mu.Lock()
	outs := append([]string(nil), w.outputs...)
	w.mu.Unlock()
	for i := len(outs) - 1; i >= 0; i-- {
		if outs[i] == "" {
			continue
		}
		if d := filepath.Dir(outs[i]); d != "" && d != "." {
			return d
		}
	}
	return w.currentDir()
}

// existingOutputs 返回当前真实存在的产出文件名（不含目录）。
func (w *Win) existingOutputs() []string {
	w.mu.Lock()
	outs := append([]string(nil), w.outputs...)
	w.mu.Unlock()
	dir := w.outputDir()
	if dir == "" {
		return nil
	}
	var names []string
	for _, p := range outs {
		name := filepath.Base(p)
		if name == "" || name == "." || name == string(filepath.Separator) {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			names = append(names, name)
		}
	}
	return names
}

func (w *Win) onStart() {
	if w.isBusy() {
		return
	}
	w.mu.Lock()
	items := append([]*app.Resolved(nil), w.items...)
	quals := append([]int(nil), w.qualSel...)
	w.mu.Unlock()

	if len(items) == 0 {
		w.log("请先点「解析视频」确认地址可用")
		w.infoTxt = "尚未解析"
		w.detail = "先解析，再开始下载"
		w.relayout()
		return
	}
	dir := w.currentDir()
	if dir == "" {
		w.log("请先选择保存目录")
		return
	}

	pageSpec := ""
	if w.page != nil {
		pageSpec = strings.TrimSpace(w.page.Text())
	}
	parallel := w.cfg.Parallel
	if w.par != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(w.par.Text())); err == nil && n >= 1 {
			parallel = n
		}
	}

	// 记住这次的选择，下次启动直接沿用
	w.cfg.DownloadDir = dir
	w.cfg.Parallel = parallel
	if len(quals) > 0 {
		w.cfg.Quality = quals[0]
	}
	w.cfg.KeepTemp = w.keepTemp
	w.saveConfig()

	extra := func(on bool, kind string) string { return extraValue(on, kind) }
	cover := app.ExtraOff
	if w.cover {
		cover = "on"
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()

	w.setBusy(true)
	w.prog = app.JobProgress{}
	w.progText = "准备开始…"
	w.outputs = nil
	w.refreshProgressView()
	w.invalidate()
	w.log("=== 开始任务，共 %d 个视频 ===", len(items))

	go func() {
		defer func() {
			cancel()
			w.mu.Lock()
			w.cancel = nil
			w.mu.Unlock()
			w.setBusy(false)
			w.mw.Synchronize(func() {
				w.stageTxt = "就绪"
				w.refreshProgressView()
				w.invalidate()
			})
		}()

		var failed int
		for i, it := range items {
			if ctx.Err() != nil {
				w.log("已取消")
				return
			}
			q := 0
			if i < len(quals) {
				q = quals[i]
			}
			w.log("--- %d/%d %s ---", i+1, len(items), it.Info.Title)
			err := w.runner.Download(ctx, it, app.Options{
				PageSpec:  pageSpec,
				OutDir:    dir,
				Quality:   q,
				Codec:     w.cfg.CodecOrder,
				Parallel:  parallel,
				Danmaku:   extra(w.danmaku, app.DanmakuASS),
				Subtitle:  extra(w.subtitle, app.SubtitleSRT),
				SaveCover: cover,
			})
			if err != nil {
				failed++
				w.log("✗ %s：%v", it.Info.Title, err)
			}
		}

		if failed == 0 {
			w.log("=== 全部完成 ===")
		} else {
			w.log("=== 结束，%d 个失败 ===", failed)
		}
	}()
}

func (w *Win) onStop() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		w.log("正在停止…")
	}
}

func (w *Win) onCleanLog() {
	w.logs = nil
	w.scroll = 0
	w.invalidate()
}

func (w *Win) onRecheck() {
	w.refreshAccount()
}

// ---------------------------------------------------------------------------
// 账号

func (w *Win) refreshAccount() {
	if !bilibili.HasAuthCookie(w.cfg.Cookie) {
		w.setAccount("未登录 —— 只能下到 480P", toneMuted)
		return
	}
	label := "已登录"
	if w.cfg.UName != "" {
		label = "已登录：" + w.cfg.UName
	}
	w.setAccount(label+"（校验中…）", toneInfo)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		acc, err := w.runner.Client().Verify(ctx, w.cfg.Cookie)
		w.mw.Synchronize(func() {
			switch {
			case err != nil:
				w.setAccount("凭证已保存（校验失败）", toneBad)
				w.log("! 账号校验没成功：%v", err)
			case acc != nil && acc.LoggedIn:
				w.cfg.UName = acc.UName
				w.cfg.MID = acc.MID
				w.saveConfig()
				w.setAccount("已登录："+acc.UName, toneOk)
				w.log("✓ 账号校验通过：%s（mid=%d）", acc.UName, acc.MID)
			default:
				msg := "凭证已失效"
				if acc != nil && acc.Message != "" {
					msg = acc.Message
				}
				w.setAccount("未登录 —— "+msg, toneBad)
			}
		})
	}()
}

// setAccount 更新账号状态。账号胶囊出现在顶栏与账号卡两处，都要改。
func (w *Win) setAccount(text string, tone statusTone) {
	w.acctText, w.acctTone = text, tone
	w.relayout()
}

func (w *Win) onLogout() {
	w.cfg.Cookie = ""
	w.cfg.AccessToken = ""
	w.cfg.UName = ""
	w.cfg.MID = 0
	w.saveConfig()
	w.runner.SetCredentials("", "")
	w.setAccount("未登录 —— 只能下到 480P", toneMuted)
	w.log("已清除本地凭证")
}

// ---------------------------------------------------------------------------
// 小工具

// describe 拼出标题下面那行详情。
func describe(it *app.Resolved) string {
	if it == nil || it.Info == nil {
		return ""
	}
	parts := []string{
		fmt.Sprintf("共 %d 个分P", len(it.Pages)),
		"总时长 " + humanDur(it.Info.Duration),
	}
	if len(it.Avail) > 0 {
		var names []string
		for _, q := range it.Avail {
			names = append(names, bilibili.QualityName(q))
		}
		parts = append(parts, "可用画质："+strings.Join(names, "、"))
	} else {
		parts = append(parts, "画质未知（可能需要登录）")
	}
	if it.Info.IsBangumi {
		parts = append(parts, "番剧/剧集")
	}
	return strings.Join(parts, "　·　")
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func humanDur(sec int) string {
	if sec <= 0 {
		return "0:00"
	}
	h, m, s := sec/3600, (sec%3600)/60, sec%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// humanETA 把剩余时长压成尽量短的写法。
//
// 不用 time.Duration.String()（它会输出 "1h2m3.456789s"，一长串小数），
// 而是把不足 1 分钟说成「<1 分」、不到 1 小时说成「3 分 20 秒」。
func humanETA(d time.Duration) string {
	sec := int(d.Round(time.Second).Seconds())
	switch {
	case sec < 60:
		return fmt.Sprintf("%d 秒", max(sec, 1))
	case sec < 3600:
		return fmt.Sprintf("%d 分 %d 秒", sec/60, sec%60)
	default:
		return humanDur(sec)
	}
}
