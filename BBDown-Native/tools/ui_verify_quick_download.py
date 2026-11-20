# ui_verify_quick_download.py —— 验证「刷视频页点一键下载 → 直接跳到下载页」：
#   1. 点「刷视频」页签（这样内嵌服务才会起来）
#   2. 用页面同一条接口 POST /api/quick-download（带 X-BBDown-Native 头）
#   3. 截图看原生窗口是不是自己切到了「下载」页签、任务台有没有在跑
#
# 用法：先启动 dist/BBDown原生版.exe，再运行本脚本。
import ctypes, ctypes.wintypes as wt, sys, time, os, json, urllib.request

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
_EnumChild = _decl('EnumChildWindows', wt.BOOL, wt.HWND, ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM), wt.LPARAM)
_GetClassName = _decl('GetClassNameW', ctypes.c_int, wt.HWND, wt.LPWSTR, ctypes.c_int)
_IsVisible = _decl('IsWindowVisible', wt.BOOL, wt.HWND)
_PostMsg = _decl('PostMessageW', wt.BOOL, wt.HWND, wt.UINT, wt.WPARAM, wt.LPARAM)
_GetClientRect = _decl('GetClientRect', wt.BOOL, wt.HWND, ctypes.POINTER(wt.RECT))
_GetWindowRect = _decl('GetWindowRect', wt.BOOL, wt.HWND, ctypes.POINTER(wt.RECT))
_SetWindowPos = _decl('SetWindowPos', wt.BOOL, wt.HWND, wt.HWND,
                      ctypes.c_int, ctypes.c_int, ctypes.c_int, ctypes.c_int, wt.UINT)

CB = ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
BVID = 'BV1GJ411x7h7'


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
    _PostMsg(hwnd, 0x0201, 1, lp)
    _PostMsg(hwnd, 0x0202, 0, lp)


def shot(name, mw):
    _SetWindowPos(mw, wt.HWND(-1), 0, 0, 0, 0, 0x0013)
    time.sleep(0.8)
    r = wt.RECT()
    if not _GetWindowRect(mw, ctypes.byref(r)):
        print('FAIL：GetWindowRect 失败')
        return
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
    _SetWindowPos(mw, wt.HWND(-2), 0, 0, 0, 0, 0x0013)


def feed_port():
    for port in range(18230, 18245):
        try:
            with urllib.request.urlopen(f'http://127.0.0.1:{port}/api/home?tab=popular', timeout=2) as resp:
                body = json.loads(resp.read().decode('utf-8'))
            if 'items' in body:
                return port
        except Exception:
            continue
    return 0


def quick_download(port):
    data = json.dumps({'bvid': BVID, 'page': 1}).encode()
    req = urllib.request.Request(
        f'http://127.0.0.1:{port}/api/quick-download', data=data,
        headers={'Content-Type': 'application/json', 'X-BBDown-Native': '1'},
        method='POST')
    with urllib.request.urlopen(req, timeout=30) as resp:
        return resp.status, resp.read().decode('utf-8')


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
    k = W / 1694.0
    px = lambda v: int(round(v * k))
    print(f'客户区 {W}x{H}')

    click(cw, px(170), px(110))
    print('已点「刷视频」页签，等内嵌服务起来…')
    time.sleep(5.0)

    port = feed_port()
    if not port:
        print('FAIL：没找到内嵌服务端口')
        sys.exit(1)
    print(f'内嵌服务端口 {port}')
    shot('quick_before.png', mw)

    code, body = quick_download(port)
    print(f'POST /api/quick-download → {code} {body[:120]}')
    time.sleep(2.5)
    shot('quick_jump.png', mw)
    print('已截图（应已自动切到「下载」页签并在下载）')

    time.sleep(18)
    shot('quick_done.png', mw)
    print('DONE（截图待核对）')


if __name__ == '__main__':
    main()
