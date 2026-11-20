// Package gui 的内嵌浏览器宿主。
//
// 刷视频页需要 MSE 才能把 B 站 DASH 的音视频流拼起来播最高画质，GDI 自绘
// 做不了这件事。这里用系统自带的 WebView2 Runtime（Windows 11 预装，Windows 10
// 可从微软装）在本地窗口里嵌一块 Chromium，直接跑 internal/server/web 那套页面。
//
// 为什么不用 walk.CustomWidget 当宿主：CustomWidget 没有实现 walk.Container，
// 而建在 MainWindow 上的 walk 控件会被 VBox 布局接管、没法绝对定位。所以宿主
// HWND 直接走 Win32 建在 clientComposite 上，不进 walk 的 WidgetList，位置完全
// 由我们自己控制。WebView2 的 COM 绑定（github.com/jchv/go-webview2）是纯 Go
// 的，Loader.dll 以 go:embed 内嵌，不需要 cgo / 额外 DLL，保持单文件交付。
package gui

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jchv/go-webview2/pkg/edge"
	"github.com/jchv/go-webview2/webviewloader"
	"github.com/lxn/walk"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

// webPanel 是一块隐藏/显示由页签控制的 WebView2 宿主。
// 只在 UI 线程上读写，不需要加锁。
type webPanel struct {
	hwnd   win.HWND
	chrome *edge.Chromium
	ready  bool
	url    string
	err    error
}

// newWebPanel 建宿主窗口。此时还不会加载 WebView2 —— 首次切到刷视频页才初始化，
// 免得用户只用下载功能时也白白拉起一个 Chromium 进程。
func newWebPanel(parent win.HWND) *webPanel {
	p := &webPanel{}
	p.hwnd = win.CreateWindowEx(
		0,
		windows.StringToUTF16Ptr("Static"),
		nil,
		win.WS_CHILD|win.WS_CLIPSIBLINGS|win.WS_CLIPCHILDREN,
		0, 0, 0, 0,
		parent, 0, win.GetModuleHandle(nil), nil,
	)
	if p.hwnd == 0 {
		p.err = fmt.Errorf("无法创建内嵌浏览器宿主窗口")
	}
	return p
}

// move 按**物理像素**摆宿主窗口（与自绘层、原生 EDIT 同一坐标系）。
func (p *webPanel) move(r walk.Rectangle) {
	if p == nil || p.hwnd == 0 {
		return
	}
	if r.Width < 0 {
		r.Width = 0
	}
	if r.Height < 0 {
		r.Height = 0
	}
	win.SetWindowPos(p.hwnd, 0,
		int32(r.X), int32(r.Y), int32(r.Width), int32(r.Height),
		win.SWP_NOZORDER|win.SWP_NOACTIVATE)
}

func (p *webPanel) show(v bool) {
	if p == nil || p.hwnd == 0 {
		return
	}
	if v {
		win.ShowWindow(p.hwnd, win.SW_SHOW)
		// ShowWindow 不保证子窗口抬到最前：自绘层是它的兄弟窗口，z 序更高时
		// 会严严实实盖住浏览器，表现就是刷视频页一片空白（只有页签条可见）。
		win.SetWindowPos(p.hwnd, win.HWND_TOP, 0, 0, 0, 0,
			win.SWP_NOMOVE|win.SWP_NOSIZE|win.SWP_NOACTIVATE)
	} else {
		win.ShowWindow(p.hwnd, win.SW_HIDE)
	}
}

// navigate 首次调用时初始化 WebView2，随后导航到指定地址。
//
// 这里先查运行时版本再 Embed：go-webview2 内部遇到环境创建失败会 log.Fatalf
// 直接干掉进程，而用户机器上没装 Runtime 属于可预期的降级场景，不该让整个
// 下载器陪葬。查到没装就记一条错误，paint 会在页面上画出人话提示。
func (p *webPanel) navigate(url string) {
	if p == nil || p.err != nil {
		return
	}
	if !p.ready {
		ver, err := webviewloader.GetInstalledVersion()
		if err != nil {
			p.err = fmt.Errorf("检测 WebView2 运行时失败：%v", err)
			return
		}
		if ver == "" {
			p.err = fmt.Errorf("本机未安装 WebView2 Runtime，无法在窗口内播放视频")
			return
		}
		c := edge.NewChromium()
		c.DataPath = filepath.Join(os.Getenv("LOCALAPPDATA"), "BBDown-Native", "webview2")
		// Embed 内部要跑一个嵌套消息循环等 COM 回调，必须在 UI 线程调用。
		if !c.Embed(uintptr(p.hwnd)) {
			p.err = fmt.Errorf("WebView2 初始化失败，请重新安装 WebView2 Runtime")
			return
		}
		p.chrome = c
		p.ready = true
	}
	if p.url == url {
		return
	}
	p.chrome.Navigate(url)
	p.url = url
	p.chrome.Resize()
}

// resized 在宿主尺寸变化后同步 WebView2 的渲染区域。
func (p *webPanel) resized() {
	if p == nil || !p.ready {
		return
	}
	p.chrome.Resize()
	_ = p.chrome.NotifyParentWindowPositionChanged()
}

// close 释放宿主窗口。WebView2 的子窗口会随父窗口一起销毁。
func (p *webPanel) close() {
	if p == nil {
		return
	}
	if p.hwnd != 0 {
		win.DestroyWindow(p.hwnd)
		p.hwnd = 0
	}
	p.chrome = nil
	p.ready = false
}
