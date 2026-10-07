# theme_toggle_check.py —— 主题来回切换回归验证：
# 启动 exe → 连点右上角「深色/浅色」切换按钮 N 次 → 每次截图统计
# 纯黑像素占比。全黑 bug 的特征是整窗像素 sum(r,g,b)<20 占比 >90%
# （正常深色主题底色 0x0b0f18 三通道和为 50，不会被算成纯黑）。
import ctypes, ctypes.wintypes as wt, sys, os, time, subprocess
from PIL import Image

u32 = ctypes.WinDLL('user32', use_last_error=True)
g32 = ctypes.WinDLL('gdi32', use_last_error=True)
try:
    ctypes.WinDLL('user32').SetProcessDPIAware()
except Exception:
    pass
u32.PrintWindow.argtypes = [wt.HWND, wt.HDC, wt.UINT]
u32.PrintWindow.restype = wt.BOOL
g32.CreateCompatibleDC.restype = wt.HDC
g32.CreateCompatibleBitmap.restype = wt.HBITMAP
g32.SelectObject.argtypes = [wt.HDC, wt.HGDIOBJ]
g32.GetDIBits.argtypes = [wt.HDC, wt.HBITMAP, wt.UINT, wt.UINT, ctypes.c_void_p, ctypes.c_void_p, wt.UINT]
g32.GetDIBits.restype = wt.INT

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
EXE = os.path.join(ROOT, 'dist', 'UI验证.exe')
OUT = os.path.join(ROOT, 'dist', '_uitheme')
os.makedirs(OUT, exist_ok=True)
N = int(sys.argv[1]) if len(sys.argv) > 1 else 8
# 主题按钮在整窗图里的比例位置（顶部右侧按钮行，实测截得）
BTN = (0.800, 0.081)


def grab(h):
    r = wt.RECT(); u32.GetWindowRect(h, ctypes.byref(r))
    w, ht = r.right - r.left, r.bottom - r.top
    hdc = u32.GetWindowDC(h)
    mdc = g32.CreateCompatibleDC(hdc)
    bmp = g32.CreateCompatibleBitmap(hdc, w, ht)
    g32.SelectObject(mdc, wt.HGDIOBJ(bmp))
    u32.PrintWindow(h, mdc, 2)

    class BMI(ctypes.Structure):
        _fields_ = [('s', wt.DWORD), ('w', wt.LONG), ('h', wt.LONG), ('p', wt.WORD),
                    ('b', wt.WORD), ('c', wt.DWORD), ('sz', wt.DWORD),
                    ('x', wt.LONG), ('y', wt.LONG), ('u', wt.DWORD), ('ci', wt.DWORD)]
    bi = BMI(); bi.s = ctypes.sizeof(BMI); bi.w = w; bi.h = -ht
    bi.p = 1; bi.b = 32
    buf = ctypes.create_string_buffer(w * ht * 4)
    g32.GetDIBits(mdc, wt.HBITMAP(bmp), 0, ht, buf, ctypes.byref(bi), 0)
    img = Image.frombytes('RGB', (w, ht), bytes(buf), 'raw', 'BGRX')
    pt = wt.POINT(0, 0); u32.ClientToScreen(h, ctypes.byref(pt))
    return img, (pt.x - r.left, pt.y - r.top), (r.left, r.top)


def black_frac(img):
    """纯黑像素（三通道和 < 20）占比 —— 全黑 bug 的判据。"""
    small = img.resize((200, 120))
    n = bad = 0
    for p in small.getdata():
        n += 1
        if p[0] + p[1] + p[2] < 20:
            bad += 1
    return bad / n


def mean_rgb(img):
    small = img.resize((100, 60))
    rs = gs = bs = 0
    d = list(small.getdata())
    for p in d:
        rs += p[0]; gs += p[1]; bs += p[2]
    n = len(d)
    return rs // n, gs // n, bs // n


def click(h, imgx, imgy, off, winxy):
    u32.SetForegroundWindow(h)
    time.sleep(0.15)
    u32.SetCursorPos(int(winxy[0] + imgx), int(winxy[1] + imgy))
    time.sleep(0.1)
    u32.mouse_event(2, 0, 0, 0, 0)
    time.sleep(0.05)
    u32.mouse_event(4, 0, 0, 0, 0)


def main():
    subprocess.run(['taskkill', '/IM', 'UI验证.exe', '/F'], capture_output=True)
    time.sleep(1)
    proc = subprocess.Popen([EXE], cwd=os.path.dirname(EXE))
    h = 0
    for _ in range(60):
        time.sleep(0.5)
        f = [0]

        @ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
        def cb(hw, l):
            if not u32.IsWindowVisible(hw):
                return True
            b = ctypes.create_unicode_buffer(64)
            u32.GetClassNameW(hw, b, 64)
            pid = wt.DWORD()
            u32.GetWindowThreadProcessId(hw, ctypes.byref(pid))
            if 'Walk_MainWindow' in b.value and pid.value == proc.pid:
                f[0] = hw
                return False
            return True
        u32.EnumWindows(cb, 0)
        h = f[0]
        if h:
            break
    if not h:
        print('FAIL: window not found')
        sys.exit(1)

    prev = None
    bad_shots = []
    for i in range(N + 1):
        time.sleep(0.6)
        img, off, win = grab(h)
        img.save(os.path.join(OUT, f's{i}.png'))
        frac = black_frac(img)
        m = mean_rgb(img)
        tag = 'INIT' if i == 0 else f'T{i}'
        # 全黑判定：纯黑占比 > 50%（正常任何主题都远低于此）
        flag = 'BLACK!' if frac > 0.5 else 'ok'
        if frac > 0.5:
            bad_shots.append(i)
        changed = '' if prev is None else ('CHANGED' if abs(m[0]-prev[0])+abs(m[1]-prev[1])+abs(m[2]-prev[2]) > 15 else 'nochange')
        print(f'{tag}: black={frac:.2%} mean={m} {flag} {changed}')
        prev = m
        if i < N:
            click(h, img.size[0]*BTN[0], img.size[1]*BTN[1], off, win)

    subprocess.run(['taskkill', '/IM', 'UI验证.exe', '/F'], capture_output=True)
    if bad_shots:
        print(f'FAIL: all-black at shots {bad_shots}')
        sys.exit(1)
    print('ALL_OK')


if __name__ == '__main__':
    main()
