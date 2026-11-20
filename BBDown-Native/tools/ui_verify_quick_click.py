# ui_verify_quick_click.py —— 真正在页面上点「一键下载」：
#   截图里找粉色按钮簇 → 鼠标事件点它 → 截图看窗口是否自己切到「下载」页签。
# 前提：原生窗口已经停在「刷视频 / 精选」页签（先跑 ui_verify_feed_mode.py）。
import ctypes, ctypes.wintypes as wt, sys, time, os

u = ctypes.WinDLL('user32', use_last_error=True)


def _decl(name, restype, *argtypes):
    fn = getattr(u, name)
    fn.restype = restype
    fn.argtypes = argtypes
    return fn


try:
    u.SetProcessDPIAware()
except Exception:
    pass
_EnumWindows = _decl('EnumWindows', wt.BOOL, ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM), wt.LPARAM)
_GetClassName = _decl('GetClassNameW', ctypes.c_int, wt.HWND, wt.LPWSTR, ctypes.c_int)
_IsVisible = _decl('IsWindowVisible', wt.BOOL, wt.HWND)
_GetWindowRect = _decl('GetWindowRect', wt.BOOL, wt.HWND, ctypes.POINTER(wt.RECT))
_SetWindowPos = _decl('SetWindowPos', wt.BOOL, wt.HWND, wt.HWND,
                      ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_int, wt.UINT)
_SetCursorPos = _decl('SetCursorPos', wt.BOOL, ctypes.c_int, ctypes.c_int)
_mouse_event = _decl('mouse_event', None, wt.DWORD, wt.DWORD, wt.DWORD, wt.DWORD, ctypes.c_void_p)

CB = ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)


def cls(h):
    b = ctypes.create_unicode_buffer(256)
    _GetClassName(h, b, 256)
    return b.value


def find(part):
    found = [0]

    def cb(h, l):
        if _IsVisible(h) and part in cls(h):
            found[0] = h
            return False
        return True

    _EnumWindows(CB(cb), 0)
    return found[0]


def grab(mw):
    _SetWindowPos(mw, wt.HWND(-1), 0, 0, 0, 0, 0x0013)  # TOPMOST
    time.sleep(0.7)
    r = wt.RECT()
    _GetWindowRect(mw, ctypes.byref(r))
    from PIL import ImageGrab
    img = ImageGrab.grab(bbox=(r.left, r.top, r.right, r.bottom), all_screens=True)
    _SetWindowPos(mw, wt.HWND(-2), 0, 0, 0, 0, 0x0013)  # NOTOPMOST
    return img, r


def pink_clusters(img):
    """找 B 站粉的实心按钮簇（主色 #FB7299 附近）。"""
    w, h = img.size
    px = img.convert('RGB').load()
    cols = {}
    step = 2
    for y in range(0, h, step):
        for x in range(0, w, step):
            r, g, b = px[x, y]
            if r > 225 and 95 < g < 140 and 140 < b < 180:
                cols.setdefault(y // 12, []).append((x, y))
    out = []
    for key in sorted(cols):
        pts = cols[key]
        if len(pts) < 12:
            continue
        xs = [p[0] for p in pts]
        ys = [p[1] for p in pts]
        cw, ch = max(xs) - min(xs), max(ys) - min(ys)
        if 60 <= cw <= 260 and ch <= 70:
            out.append((min(xs), min(ys), cw, ch))
    return out


def click_at(x, y):
    _SetCursorPos(x, y)
    time.sleep(0.2)
    _mouse_event(0x0002, 0, 0, 0, None)  # LEFTDOWN
    time.sleep(0.06)
    _mouse_event(0x0004, 0, 0, 0, None)  # LEFTUP


def main():
    mw = find('Walk_MainWindow')
    if not mw:
        print('NO_WINDOW')
        sys.exit(1)
    img, r = grab(mw)
    print(f'窗口 {r.left},{r.top} 截图 {img.size[0]}x{img.size[1]}')
    cs = pink_clusters(img)
    for c in cs:
        print(f'  粉色簇 x={c[0]} y={c[1]} w={c[2]} h={c[3]}')
    # 「一键下载」是位于播放器下方那一行、靠左的粉色按钮：
    # 取下半屏（y > 60%）里最靠下的宽按钮。
    cand = [c for c in cs if c[1] > img.size[1] * 0.6 and c[0] > img.size[0] * 0.25]
    if not cand:
        print('FAIL：没找到「一键下载」按钮')
        sys.exit(1)
    bx = max(cand, key=lambda c: c[1])
    sx = r.left + bx[0] + bx[2] // 2
    sy = r.top + bx[1] + bx[3] // 2
    print(f'点击「一键下载」≈ 屏幕({sx},{sy})')
    click_at(sx, sy)

    time.sleep(3.0)
    img2, _ = grab(mw)
    out = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', '_shots', 'quick_click.png')
    img2.save(out)
    print(f'  截图 → {out}')
    print('DONE（核对：是否已切到「下载」页签且任务在跑）')


if __name__ == '__main__':
    main()
