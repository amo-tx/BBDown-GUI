# ui_verify_feed_mode.py —— 验证刷视频页的两种模式与「关闭键」修复：
#   点刷视频页签 → 精选模式（本地页，且不带「✕ 关闭」）
#   → 点页签条右端「首页」→ 自绘首页瀑布流 → 点「精选」回到紧凑列表
#
# 用法：先启动 dist/BBDown原生版.exe，再运行本脚本。
# 坐标是 CustomWidget 客户区里的物理像素（150% DPI、默认窗口 1140x790）。
import ctypes, ctypes.wintypes as wt, sys, time, os

u = ctypes.WinDLL('user32', use_last_error=True)


def _decl(name, restype, *argtypes):
    fn = getattr(u, name)
    fn.restype = restype
    fn.argtypes = argtypes
    return fn


# 必须声明 argtypes：64 位下 HWND 超过 32 位，不声明会被截断，消息投到
# 无效窗口、GetWindowRect 返回空矩形（截图直接报 empty image）。
_Declare = lambda: None
try:
    u.SetProcessDPIAware()
except Exception:
    pass
_EnumWindows = _decl('EnumWindows', wt.BOOL, ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM), wt.LPARAM)
_EnumChild = _decl('EnumChildWindows', wt.BOOL, wt.HWND, ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM), wt.LPARAM)
_GetClassName = _decl('GetClassNameW', ctypes.c_int, wt.HWND, wt.LPWSTR, ctypes.c_int)
_IsVisible = _decl('IsWindowVisible', wt.BOOL, wt.HWND)
_IsWindow = _decl('IsWindow', wt.BOOL, wt.HWND)
_PostMsg = _decl('PostMessageW', wt.BOOL, wt.HWND, wt.UINT, wt.WPARAM, wt.LPARAM)
_GetClientRect = _decl('GetClientRect', wt.BOOL, wt.HWND, ctypes.POINTER(wt.RECT))
_GetWindowRect = _decl('GetWindowRect', wt.BOOL, wt.HWND, ctypes.POINTER(wt.RECT))
_SetWindowPos = _decl('SetWindowPos', wt.BOOL, wt.HWND, wt.HWND,
                      ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_int, wt.UINT)

CB = ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)


def cls(h):
    b = ctypes.create_unicode_buffer(256)
    _GetClassName(h, b, 256)
    return b.value


def find(part, root=None):
    found = [0]

    def cb(h, l):
        if _IsVisible(h) and part in cls(h):
            found[0] = h
            return False
        return True

    if root:
        _EnumChild(root, CB(cb), 0)
    else:
        _EnumWindows(CB(cb), 0)
    return found[0]


def click(hwnd, cx, cy):
    lp = (cy << 16) | cx
    _PostMsg(hwnd, 0x0201, 1, lp)  # WM_LBUTTONDOWN
    _PostMsg(hwnd, 0x0202, 0, lp)  # WM_LBUTTONUP


def shot(name, mw):
    # WebView2 是独立子窗口，PrintWindow 抓父窗口常只有底色：
    # 先抬到最前再用 ImageGrab 抓整窗矩形。
    _SetWindowPos(mw, wt.HWND(-1), 0, 0, 0, 0, 0x0013)  # TOPMOST|NOMOVE|NOSIZE|NOACTIVATE
    time.sleep(0.8)
    r = wt.RECT()
    if not _GetWindowRect(mw, ctypes.byref(r)):
        print('FAIL：GetWindowRect 失败')
        return
    print(f'  窗口矩形 {r.left},{r.top} - {r.right},{r.bottom}')
    try:
        from PIL import ImageGrab
    except ImportError:
        print('SKIP 截图：缺 pillow')
        return
    img = ImageGrab.grab(bbox=(r.left, r.top, r.right, r.bottom), all_screens=True)
    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '_shots', name)
    os.makedirs(os.path.dirname(out), exist_ok=True)
    img.save(out)
    print(f'  截图 {name} → {out}')
    _SetWindowPos(mw, wt.HWND(-2), 0, 0, 0, 0, 0x0013)  # NOTOPMOST


def main():
    mw = find('Walk_MainWindow')
    if not mw:
        print('NO_WINDOW：请先启动原生窗口')
        sys.exit(1)
    cw = find('Walk_CustomWidget', mw)
    if not cw:
        print('NO_CUSTOMWIDGET')
        sys.exit(1)
    r = wt.RECT()
    _GetClientRect(cw, ctypes.byref(r))
    W, H = r.right - r.left, r.bottom - r.top
    print(f'客户区 {W}x{H}')

    k = W / 1694.0
    def px(v):
        return int(round(v * k))

    click(cw, px(170), px(110))
    time.sleep(3.0)
    print('已点「刷视频」 → 精选模式')
    shot('feed_pick.png', mw)

    mode_right = W - px(27)
    seg = px(96)
    gap = px(9)
    y = px(124)
    pick_x = mode_right - 2 * seg - gap + seg // 2
    web_x = mode_right - seg // 2
    print(f'模式分段：精选≈({pick_x},{y}) 首页≈({web_x},{y})')

    click(cw, web_x, y)
    print('已点「首页」，等页面加载…')
    time.sleep(9.0)
    shot('feed_web.png', mw)

    click(cw, pick_x, y)
    print('已点「精选」，等页面回来…')
    time.sleep(4.0)
    shot('feed_pick_back.png', mw)

    print('DONE（截图待核对）')


if __name__ == '__main__':
    main()
