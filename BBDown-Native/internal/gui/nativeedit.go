package gui

import (
	"fmt"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// 本文件解决一个具体问题：自绘界面里那几个「必须有真实文本输入与输入法」的
// 控件（视频地址、分P、目录、并行数），怎么摆到想摆的地方、又跟着主题变色。
//
// 为什么不用 walk 的 LineEdit / TextEdit：walk 会把它创建的每一个控件都交给
// 自己的布局系统接管，而这套界面是纯绝对定位的。walk 的布局系统容不下这种
// 用法 —— layout.go 里那句「没有布局就临时造一个 HBox」只作用于非 Widget
// 容器，Composite 会直接解引用自己的 nil 布局，实测是一次空指针崩溃
// （栈：CreateLayoutItemsForContainerWithContext → ContainerBase.CreateLayoutItem）。
//
// 所以这几个控件绕开 walk、直接用 Win32 建：它们是自绘层所在容器的子窗口，
// walk 不认识它们，也就永远不去动它们的尺寸。

// ---------------------------------------------------------------------------
// 原生 EDIT

type nativeEdit struct {
	hwnd  win.HWND
	multi bool
}

// EM_SETMARGINS 的取值。lxn/win 里没有导出，本地定义。
const (
	ecLeftMargin  = 0x0001
	ecRightMargin = 0x0002
)

func newNativeEdit(parent win.HWND, multi bool) (*nativeEdit, error) {
	style := uint32(win.WS_CHILD | win.WS_VISIBLE | win.WS_TABSTOP | win.ES_AUTOHSCROLL)
	if multi {
		// WS_VSCROLL 只能在创建时给，建完再想加就得改窗口样式并重排，
		// 所以宁可一开始就带上。
		style = uint32(win.WS_CHILD | win.WS_VISIBLE | win.WS_TABSTOP |
			win.ES_MULTILINE | win.ES_WANTRETURN | win.ES_AUTOVSCROLL | win.WS_VSCROLL)
	}
	cls, err := syscall.UTF16PtrFromString("EDIT")
	if err != nil {
		return nil, err
	}
	// 扩展样式传 0：不要 WS_EX_CLIENTEDGE 那个 Win95 立体边框，
	// 边框由自绘层负责画。
	h := win.CreateWindowEx(0, cls, nil, style, 0, 0, 10, 10, parent, 0, 0, nil)
	if h == 0 {
		return nil, fmt.Errorf("创建原生输入框失败")
	}
	// 左右各留一点内边距，文字不要贴着边框。
	win.SendMessage(h, win.EM_SETMARGINS, ecLeftMargin|ecRightMargin, uintptr(packShort(4, 4)))
	return &nativeEdit{hwnd: h, multi: multi}, nil
}

// packShort 把两个 16 位值拼成 EM_SETMARGINS 要的 lParam。
func packShort(lo, hi int) uint32 {
	return uint32(uint16(lo)) | uint32(uint16(hi))<<16
}

// Bounds 把控件移到指定矩形（相对父窗口客户区，原生像素）。
func (e *nativeEdit) Bounds(r walk.Rectangle) {
	if e == nil {
		return
	}
	win.MoveWindow(e.hwnd, int32(r.X), int32(r.Y), int32(r.Width), int32(r.Height), true)
}

// Text 读回内容。多行控件返回的是 \r\n 分行。
func (e *nativeEdit) Text() string {
	if e == nil {
		return ""
	}
	n := int32(win.SendMessage(e.hwnd, win.WM_GETTEXTLENGTH, 0, 0))
	if n <= 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	if win.SendMessage(e.hwnd, win.WM_GETTEXT, uintptr(n+1), uintptr(unsafe.Pointer(&buf[0]))) == 0 {
		return ""
	}
	// 只削掉结尾的换行：地址字段里的空格可能是有意义的，不能一起 trim。
	return strings.TrimRight(syscall.UTF16ToString(buf), "\r\n")
}

func (e *nativeEdit) SetText(s string) {
	if e == nil {
		return
	}
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	win.SendMessage(e.hwnd, win.WM_SETTEXT, 0, uintptr(unsafe.Pointer(p)))
}

func (e *nativeEdit) SetFont(hf win.HFONT) {
	if e == nil || hf == 0 {
		return
	}
	win.SendMessage(e.hwnd, win.WM_SETFONT, uintptr(hf), 1)
}

// SetCue 设置输入框为空时显示的灰色提示（EM_SETCUEBANNER）。
func (e *nativeEdit) SetCue(s string) {
	if e == nil || s == "" {
		return
	}
	p, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return
	}
	win.SendMessage(e.hwnd, win.EM_SETCUEBANNER, 1, uintptr(unsafe.Pointer(p)))
}

// Refresh 请求重画。切主题时必须调一次 —— 底色和字色是在 WM_CTLCOLOREDIT
// 里回答的，不重画的话控件还留着上一套颜色。
func (e *nativeEdit) Refresh() {
	if e == nil {
		return
	}
	win.InvalidateRect(e.hwnd, nil, true)
}

func (e *nativeEdit) Focus() {
	if e == nil {
		return
	}
	win.SetFocus(e.hwnd)
}

// Dispose 销毁控件。父窗口销毁时子窗口本来也会跟着走，这里是给提前收尾用的。
func (e *nativeEdit) Dispose() {
	if e == nil {
		return
	}
	win.DestroyWindow(e.hwnd)
	e.hwnd = 0
}

// fontHandle 取一个 walk 窗口当前用的 HFONT。
//
// walk 没有把 Font 的句柄暴露出来（handleForDPI 是私有的），但字体是设到
// 控件上的，而 WM_GETFONT 是标准消息，读回来即可 —— 省得我们再维护一套
// GDI 字体对象（那又是一批必须记着释放的句柄）。
func fontHandle(w walk.Window) win.HFONT {
	if w == nil {
		return 0
	}
	return win.HFONT(win.SendMessage(w.Handle(), win.WM_GETFONT, 0, 0))
}

// ---------------------------------------------------------------------------
// WM_CTLCOLOREDIT 接管
//
// walk 的容器确实处理 WM_CTLCOLOREDIT，但它只在「被着色的那个窗口自己也归
// walk 管」的时候才顺手设字色：源码里先 windowFromHandle(hwnd) 拿窗口对象，
// 拿不到就退回用容器自己，而容器不是 TextColorer，于是字色不设。
// 我们的 EDIT 是 walk 不认识的裸窗口，结果就是：底色跟得上主题，字色永远是
// 系统默认的黑 —— 浅色主题下没问题，深色主题下就是黑字压深底，完全看不清。
//
// 所以把父窗口的窗口过程再接一层，自己把底色与字色一起回答掉，其余消息原样
// 转发给 walk 的过程。

var (
	editHostHWND win.HWND
	editHostPrev uintptr

	editTheme struct {
		sync.Mutex
		brush win.HBRUSH
		fg    walk.Color
	}
)

// hostEdits 把原生 EDIT 挂到 hwnd 下，并接管该窗口的 WM_CTLCOLOREDIT。
func hostEdits(hwnd win.HWND) {
	if hwnd == 0 {
		return
	}
	editHostHWND = hwnd
	if prev := win.SetWindowLongPtr(hwnd, win.GWLP_WNDPROC, syscall.NewCallback(editHostProc)); prev != 0 {
		editHostPrev = prev
	}
}

func editHostProc(hwnd win.HWND, msg uint32, wParam, lParam uintptr) uintptr {
	if msg == win.WM_CTLCOLOREDIT || msg == win.WM_CTLCOLORSTATIC {
		editTheme.Lock()
		h, fg := editTheme.brush, editTheme.fg
		editTheme.Unlock()
		if h != 0 {
			hdc := win.HDC(wParam)
			win.SetTextColor(hdc, win.COLORREF(fg))
			// 透明背景模式：不然光标那一格会带出一块底色。
			win.SetBkMode(hdc, win.TRANSPARENT)
			return uintptr(h)
		}
	}
	return win.CallWindowProc(editHostPrev, hwnd, msg, wParam, lParam)
}

// setEditColors 换掉原生 EDIT 的底色与字色。
//
// 必须挂在自绘层之外的地方：这几个控件的底色不能跟着主题走「卡片白」，
// 而要和自绘的输入框外框完全一致，否则框里会显出一块颜色不同的矩形。
func setEditColors(bg, fg walk.Color) {
	editTheme.Lock()
	old := editTheme.brush
	editTheme.brush = win.CreateBrushIndirect(&win.LOGBRUSH{
		LbStyle: win.BS_SOLID,
		LbColor: win.COLORREF(bg),
	})
	editTheme.fg = fg
	editTheme.Unlock()

	// 旧画刷在这里释放：它已经不在任何一个 DC 里了（每次 WM_CTLCOLOREDIT
	// 都是一次性返回），现在换掉是安全的。
	if old != 0 {
		win.DeleteObject(win.HGDIOBJ(old))
	}
}
