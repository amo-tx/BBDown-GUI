// Package gui 用 lxn/walk（纯 syscall 的 Win32 封装）搭原生窗口。
//
// 线程约定：所有控件只能在创建它们的线程（即 walk 的消息循环线程）上操作。
// 后台任务通过 mw.Synchronize(...) 把更新投递回该线程，
// 这是本包唯一的跨线程手段。
package gui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"bbdown-native/internal/app"
	"bbdown-native/internal/bilibili"
)

// 编码下拉的选项（值 → 传给下载层的偏好串）。
//
// 文案刻意压短：walk 的 ComboBox 是非可编辑的，此时它的 LayoutFlags 只有
// GrowableHorz、**没有** ShrinkableHorz，而且 MinSize()/IdealSize() 都直接返回
// 「最长条目文本宽 + 下拉箭头」算出来的值 —— 声明式 MinSize 改不动它。
// 也就是说条目文案多长，这一行就至少多宽；长了就把右列整个顶出窗口。
var codecChoices = []struct {
	Label string
	Value string
}{
	{"自动（优先 HEVC）", "hevc,avc,av1"},
	{"AVC / H.264", "avc"},
	{"HEVC / H.265", "hevc"},
	{"AV1", "av1"},
}

// Win 是主窗口。
type Win struct {
	mw *walk.MainWindow

	addrTE    *walk.TextEdit
	pasteBtn  *walk.PushButton
	parseBtn  *walk.PushButton
	infoLB    *walk.Label
	detailLB  *walk.Label
	qualityCB *walk.ComboBox
	codecCB   *walk.ComboBox
	pageLE    *walk.LineEdit
	dirLE     *walk.LineEdit
	acctLB    *walk.Label
	loginBtn  *walk.PushButton
	logoutBtn *walk.PushButton
	startBtn  *walk.PushButton
	cancelBtn *walk.PushButton
	progPB    *walk.ProgressBar
	progLB    *walk.Label
	logTE     *walk.TextEdit
	statusLB  *walk.StatusBarItem

	cfg    *app.Config
	runner *app.Runner
	login  *bilibili.LoginSession

	mu       sync.Mutex
	items    []*app.Resolved
	qualSel  []int // 与 items 一一对应，记录用户为每个目标选的画质（0=自动）
	busy     bool
	cancel   context.CancelFunc
	listener bool // 登录状态轮询是否在跑
}

// Run 创建并运行主窗口，直到用户关闭它。
func Run(cfg *app.Config) error {
	return RunWithWarning(cfg, "")
}

// RunWithWarning 与 Run 相同，但在启动时先弹一条提示（用于配置文件损坏等情况）。
func RunWithWarning(cfg *app.Config, warning string) error {
	w := &Win{cfg: cfg}
	w.runner = app.NewRunner(cfg, app.Hooks{
		Stage:    w.onStage,
		Log:      w.onLog,
		Progress: w.onProgress,
	})
	w.login = bilibili.NewLoginSession()

	if err := w.create(); err != nil {
		return err
	}
	w.refreshAccount()
	w.log("就绪。配置文件：%s", cfg.Path())
	if warning != "" {
		w.log("! %s", warning)
		// 界面已经建好，此时弹窗有正确的父窗口
		go func() {
			time.Sleep(300 * time.Millisecond)
			w.mw.Synchronize(func() {
				walk.MsgBox(w.mw, "提示", warning, walk.MsgBoxIconWarning)
			})
		}()
	}
	// mw.Run() 返回的是消息循环的退出码，不是错误
	w.mw.Run()
	return nil
}

// ---------------------------------------------------------------------------
// 建界面

func (w *Win) create() error {
	// 同样要短：画质这一项是 ComboBox 的条目，条目文本有多长，控件就有多宽。
	qualityModel := []string{"自动（最高可用）"}
	codecModel := make([]string, 0, len(codecChoices))
	for _, c := range codecChoices {
		codecModel = append(codecModel, c.Label)
	}

	mw := MainWindow{
		AssignTo: &w.mw,
		Title:    "BBDown 原生版",
		MinSize:  Size{Width: 760, Height: 620},
		Size:     Size{Width: 920, Height: 730},
		Layout:   VBox{MarginsZero: false, Spacing: 6},

		Children: []Widget{
			// ---------- 地址 ----------
			// TextEdit 必须 CompactHeight：非紧凑模式下它的 LayoutFlags 含
			// GreedyVert|GrowableVert，会跟下面的日志框抢垂直空间，而且理想高度
			// 恒为 100px（IdealSize() 直接返回 SizeFrom96DPI(100,100)），
			// 声明的 MinSize{Height:62} 对它无效（MinSize() 也被重写成固定值）。
			// 紧凑模式下去掉 GreedyVert，高度只按内容走，垂直余量就全归日志框。
			GroupBox{
				Title:  "视频地址",
				Layout: VBox{Spacing: 5},
				Children: []Widget{
					TextEdit{
						AssignTo:      &w.addrTE,
						VScroll:       true,
						CompactHeight: true,
						MinSize:       Size{Height: 52},
						// MaxSize 是这里唯一还能生效的尺寸约束：textEditLayoutItem
						// 只重写了 MinSize/IdealSize/HeightForWidth，MaxSize() 仍走
						// LayoutItemBase。没有它，紧凑高度会跟着粘贴内容的行数一路
						// 长上去（贴一大段分享文案就等于把下面的区块全挤扁）。
						MaxSize: Size{Height: 96},
					},
					Composite{
						Layout: HBox{Spacing: 6},
						Children: []Widget{
							PushButton{
								AssignTo:  &w.pasteBtn,
								Text:      "粘贴",
								MinSize:   Size{Width: 76},
								OnClicked: w.onPaste,
							},
							PushButton{
								AssignTo:  &w.parseBtn,
								Text:      "解析",
								MinSize:   Size{Width: 76},
								OnClicked: w.onParse,
							},
							PushButton{
								Text:      "清空",
								MinSize:   Size{Width: 76},
								OnClicked: func() { w.addrTE.SetText("") },
							},
							HSpacer{},
							Label{
								Text: "支持分享文案 / 短链 / BV 号 / 番剧 ep·ss",
							},
						},
					},
					// 标题与详情合并成一行，省掉一整行的高度
					Composite{
						Layout: HBox{Spacing: 6},
						Children: []Widget{
							Label{
								AssignTo: &w.infoLB,
								Text:     "尚未解析",
							},
							HSpacer{},
							Label{
								AssignTo:      &w.detailLB,
								Text:          "",
								EllipsisMode:  EllipsisEnd,
								TextAlignment: AlignFar,
							},
						},
					},
				},
			},

			// ---------- 选项 ----------
			// 用显式的 Label + Composite{HBox} 分行，而不是 Grid{Columns:4}：
			// Grid 会按「最宽的那个控件」定列宽，下拉框一旦选了长文案
			// 就会把右边的列整体顶出窗口。
			GroupBox{
				Title:  "下载选项",
				Layout: VBox{Spacing: 5},
				Children: []Widget{
					Composite{
						Layout: HBox{Spacing: 6},
						Children: []Widget{
							Label{Text: "画质", MinSize: Size{Width: 40}},
							ComboBox{
								AssignTo:              &w.qualityCB,
								Model:                 qualityModel,
								CurrentIndex:          0,
								OnCurrentIndexChanged: w.onQualityChanged,
							},
							Label{Text: "编码", MinSize: Size{Width: 40}},
							ComboBox{
								AssignTo:     &w.codecCB,
								Model:        codecModel,
								CurrentIndex: selectIndexByValue(w.cfg.CodecOrder),
							},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{Spacing: 6},
						Children: []Widget{
							Label{Text: "分P", MinSize: Size{Width: 40}},
							LineEdit{
								AssignTo:  &w.pageLE,
								CueBanner: "留空=全部，也可写 1 或 1,3-5",
							},
							Label{Text: "保存到", MinSize: Size{Width: 52}},
							LineEdit{AssignTo: &w.dirLE, Text: w.cfg.DownloadDir},
							PushButton{
								Text:      "浏览",
								MinSize:   Size{Width: 64},
								OnClicked: w.onBrowse,
							},
						},
					},
				},
			},

			// ---------- 账号 ----------
			GroupBox{
				Title:  "账号",
				Layout: HBox{Spacing: 6},
				Children: []Widget{
					Label{AssignTo: &w.acctLB, Text: "未登录 —— 未登录只能下到 480P"},
					HSpacer{},
					PushButton{
						AssignTo:  &w.loginBtn,
						Text:      "扫码登录",
						MinSize:   Size{Width: 92},
						OnClicked: w.onLogin,
					},
					PushButton{
						AssignTo:  &w.logoutBtn,
						Text:      "退出登录",
						MinSize:   Size{Width: 92},
						OnClicked: w.onLogout,
					},
				},
			},

			// ---------- 操作与进度 ----------
			// StretchFactor=1：外层 VBox 的垂直余量全给这一块，再往下由日志框
			// （GrowableVert）吸收。按钮行与进度条各占一行 —— 挤在一行里进度条会被按钮顶掉。
			GroupBox{
				Title:         "任务",
				Layout:        VBox{Spacing: 5},
				StretchFactor: 1,
				Children: []Widget{
					Composite{
						Layout: HBox{Spacing: 6},
						Children: []Widget{
							PushButton{
								AssignTo:  &w.startBtn,
								Text:      "开始下载",
								MinSize:   Size{Width: 110},
								OnClicked: w.onStart,
							},
							PushButton{
								AssignTo:  &w.cancelBtn,
								Text:      "取消",
								MinSize:   Size{Width: 80},
								Enabled:   false,
								OnClicked: w.onCancel,
							},
							PushButton{
								Text:      "打开目录",
								MinSize:   Size{Width: 96},
								OnClicked: w.onOpenDir,
							},
							HSpacer{},
							Label{
								AssignTo: &w.progLB,
								Text:     "—",
							},
						},
					},
					ProgressBar{
						AssignTo: &w.progPB,
						MaxValue: 1000,
					},
					TextEdit{
						AssignTo: &w.logTE,
						ReadOnly: true,
						VScroll:  true,
						MinSize:  Size{Height: 120},
					},
				},
			},
		},

		// 状态栏是 MainWindow 自己的字段，不能放进 Children
		StatusBarItems: []StatusBarItem{
			{
				AssignTo: &w.statusLB,
				Text:     "就绪",
				Width:    200,
			},
			{
				Text:  workDirHint(),
				Width: 700,
			},
		},
	}

	if err := mw.Create(); err != nil {
		return err
	}
	w.mw.Closing().Attach(func(canceled *bool, reason walk.CloseReason) {
		w.onCancel()
	})
	return nil
}

// selectIndexByValue 把配置里的编码偏好映射成下拉下标。
func selectIndexByValue(v string) int {
	v = strings.TrimSpace(strings.ToLower(v))
	for i, c := range codecChoices {
		if c.Value == v {
			return i
		}
	}
	return 0
}

func workDirHint() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return "程序目录：" + filepath.Dir(exe)
}

// ---------------------------------------------------------------------------
// 日志 / 进度（后台 goroutine 调用 → 必须切回 UI 线程）

func (w *Win) log(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	w.onLog(line)
}

func (w *Win) onLog(line string) {
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		stamp := time.Now().Format("15:04:05")
		w.logTE.AppendText("[" + stamp + "] " + line + "\r\n")
	})
}

func (w *Win) onStage(stage string) {
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		w.statusLB.SetText(stage)
	})
}

func (w *Win) onProgress(p app.JobProgress) {
	if w.mw == nil {
		return
	}
	w.mw.Synchronize(func() {
		if p.Total > 0 {
			v := int(p.Done * 1000 / p.Total)
			if v < 0 {
				v = 0
			}
			if v > 1000 {
				v = 1000
			}
			w.progPB.SetValue(v)
		} else {
			w.progPB.SetValue(0)
		}
		parts := []string{p.Label}
		if p.Total > 0 {
			parts = append(parts, fmt.Sprintf("%.1f%%", float64(p.Done)*100/float64(p.Total)))
			parts = append(parts, humanSize(p.Done)+" / "+humanSize(p.Total))
		}
		if p.Speed > 0 {
			parts = append(parts, humanSize(int64(p.Speed))+"/s")
		}
		if p.ETA > 0 {
			parts = append(parts, "剩余 "+p.ETA.Round(time.Second).String())
		}
		w.progLB.SetText(strings.Join(parts, "  "))
	})
}

// setBusy 切换「运行中」的控件可用状态。
func (w *Win) setBusy(busy bool) {
	w.mu.Lock()
	w.busy = busy
	w.mu.Unlock()
	w.mw.Synchronize(func() {
		w.startBtn.SetEnabled(!busy)
		w.parseBtn.SetEnabled(!busy)
		w.pasteBtn.SetEnabled(!busy)
		w.cancelBtn.SetEnabled(busy)
	})
}

// ---------------------------------------------------------------------------
// 动作

func (w *Win) onPaste() {
	text, err := walk.Clipboard().Text()
	if err != nil || strings.TrimSpace(text) == "" {
		w.log("剪贴板里没有文本")
		return
	}
	w.addrTE.SetText(strings.TrimSpace(text))
	w.log("已粘贴 %d 个字符", len([]rune(text)))
}

func (w *Win) onParse() {
	text := w.addrTE.Text()
	if strings.TrimSpace(text) == "" {
		walk.MsgBox(w.mw, "提示", "请先粘贴或输入视频地址", walk.MsgBoxIconInformation)
		return
	}
	if w.isBusy() {
		return
	}
	w.setBusy(true)
	w.infoLB.SetText("正在解析…")
	w.detailLB.SetText("")

	go func() {
		defer w.setBusy(false)
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		items, err := w.runner.Resolve(ctx, text)
		if err != nil {
			w.log("✗ %v", err)
			w.mw.Synchronize(func() {
				w.infoLB.SetText("解析失败")
				w.detailLB.SetText(err.Error())
			})
			return
		}
		w.mu.Lock()
		w.items = items
		w.qualSel = make([]int, len(items))
		w.mu.Unlock()

		// 画质下拉按第一个目标的可用画质重建。
		// 不带 "（qn=116）"：ComboBox 是非可编辑的，条目文本宽 = 控件宽的下限，
		// 加一截数字后缀就会把「编码」那一列往右推。
		model := []string{"自动（最高可用）"}
		for _, q := range items[0].Avail {
			model = append(model, bilibili.QualityName(q))
		}
		w.mw.Synchronize(func() {
			w.qualityCB.SetModel(model)
			w.qualityCB.SetCurrentIndex(0)
			w.infoLB.SetText(items[0].DisplayName())
			w.detailLB.SetText(describe(items[0]))
		})
		for _, it := range items {
			w.log("解析到：%s（%d 个分P）", it.Info.Title, len(it.Pages))
		}
	}()
}

func (w *Win) onQualityChanged() {
	idx := w.qualityCB.CurrentIndex()
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.items) == 0 {
		return
	}
	if idx <= 0 || idx-1 >= len(w.items[0].Avail) {
		w.qualSel[0] = 0
		return
	}
	w.qualSel[0] = w.items[0].Avail[idx-1]
}

func (w *Win) onBrowse() {
	dlg := new(walk.FileDialog)
	dlg.Title = "选择保存目录"
	dlg.InitialDirPath = w.dirLE.Text()
	if ok, err := dlg.ShowBrowseFolder(w.mw); err != nil {
		w.log("选择目录失败：%v", err)
	} else if ok {
		w.dirLE.SetText(dlg.FilePath)
	}
}

func (w *Win) onOpenDir() {
	dir := strings.TrimSpace(w.dirLE.Text())
	if dir == "" {
		dir = w.cfg.DownloadDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		walk.MsgBox(w.mw, "打不开", err.Error(), walk.MsgBoxIconError)
		return
	}
	// walk 没有 ShellExecute 封装，直接用 explorer 打开最省事。
	cmd := exec.Command("explorer.exe", filepath.Clean(dir))
	if err := cmd.Start(); err != nil {
		w.log("打开目录失败：%v", err)
		return
	}
	// explorer 打开成功时返回的是非 0 退出码，不能拿 Wait 的结果当判据
	go func() { _ = cmd.Wait() }()
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
		walk.MsgBox(w.mw, "提示", "请先点「解析」确认地址可用", walk.MsgBoxIconInformation)
		return
	}

	dir := strings.TrimSpace(w.dirLE.Text())
	if dir == "" {
		walk.MsgBox(w.mw, "提示", "请选择保存目录", walk.MsgBoxIconInformation)
		return
	}

	codec := codecChoices[w.codecCB.CurrentIndex()].Value
	pageSpec := strings.TrimSpace(w.pageLE.Text())

	// 记住这次的选择，下次启动直接沿用
	w.cfg.DownloadDir = dir
	w.cfg.CodecOrder = codec
	if len(items) > 0 && len(quals) > 0 {
		w.cfg.Quality = quals[0]
	}
	if err := w.cfg.Save(); err != nil {
		w.log("配置没能保存：%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.cancel = cancel
	w.mu.Unlock()

	w.setBusy(true)
	w.progPB.SetValue(0)
	w.log("=== 开始任务，共 %d 个视频 ===", len(items))

	go func() {
		defer func() {
			cancel()
			w.mu.Lock()
			w.cancel = nil
			w.mu.Unlock()
			w.setBusy(false)
			w.mw.Synchronize(func() {
				w.statusLB.SetText("就绪")
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
				PageSpec: pageSpec,
				OutDir:   dir,
				Quality:  q,
				Codec:    codec,
			})
			if err != nil {
				failed++
				w.log("✗ %s：%v", it.Info.Title, err)
			}
		}

		if failed == 0 {
			w.log("=== 全部完成 ===")
			w.mw.Synchronize(func() {
				walk.MsgBox(w.mw, "完成", "全部任务已完成。", walk.MsgBoxIconInformation)
			})
		} else {
			w.log("=== 结束，%d 个失败 ===", failed)
		}
	}()
}

func (w *Win) onCancel() {
	w.mu.Lock()
	cancel := w.cancel
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		w.log("正在取消…")
	}
}

func (w *Win) isBusy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy
}

// ---------------------------------------------------------------------------
// 账号

func (w *Win) refreshAccount() {
	if !bilibili.HasAuthCookie(w.cfg.Cookie) {
		w.acctLB.SetText("未登录 —— 未登录只能下到 480P")
		return
	}
	label := "已登录"
	if w.cfg.UName != "" {
		label = "已登录：" + w.cfg.UName
	}
	w.acctLB.SetText(label + "（正在校验…）")
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		acc, err := w.runner.Client().Verify(ctx, w.cfg.Cookie)
		w.mw.Synchronize(func() {
			switch {
			case err != nil:
				w.acctLB.SetText("已保存凭证（校验失败：" + err.Error() + "）")
			case acc != nil && acc.LoggedIn:
				w.cfg.UName = acc.UName
				w.cfg.MID = acc.MID
				if err := w.cfg.Save(); err != nil {
					w.log("凭证没能保存：%v", err)
				}
				w.acctLB.SetText("已登录：" + acc.UName)
				w.log("账号校验通过：%s（mid=%d）", acc.UName, acc.MID)
			default:
				msg := "凭证已失效"
				if acc != nil && acc.Message != "" {
					msg = acc.Message
				}
				w.acctLB.SetText("未登录 —— " + msg)
			}
		})
	}()
}

func (w *Win) onLogout() {
	w.cfg.Cookie = ""
	w.cfg.AccessToken = ""
	w.cfg.UName = ""
	w.cfg.MID = 0
	if err := w.cfg.Save(); err != nil {
		w.log("配置没能保存：%v", err)
	}
	w.runner.SetCredentials("", "")
	w.acctLB.SetText("未登录 —— 未登录只能下到 480P")
	w.log("已清除本地凭证")
}

// ---------------------------------------------------------------------------
// 小工具

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
