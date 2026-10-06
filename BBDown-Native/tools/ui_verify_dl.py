# ui_verify_dl.py —— 一体化真实下载验证：
# 启动 exe → 填地址 → 像素扫描定位两颗粉色主按钮（解析视频=窄、开始下载=通栏宽）
# → 点解析 → 点开始下载 → 分时截图，验证曲线/指标卡/进度条动态填充。
# 用法: python ui_verify_dl.py <BV地址> [输出目录]
import ctypes, ctypes.wintypes as wt, sys, os, time, struct, subprocess
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

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))  # BBDown-Native/
EXE = os.path.join(ROOT, 'dist', 'UI验证.exe')
URL = sys.argv[1] if len(sys.argv) > 1 else 'https://www.bilibili.com/video/BV1dwpf6nE6r/'
OUT = sys.argv[2] if len(sys.argv) > 2 else os.path.join(ROOT, 'dist', '_uidl')
os.makedirs(OUT, exist_ok=True)


def is_pink(p):
    r, g, b = p[0], p[1], p[2]
    return r > 200 and 60 < g < 120 and 100 < b < 160


def grab(h):
    """PrintWindow 抓整个窗口，返回 PIL.Image（含非客户区）。"""
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
    # 客户区在整窗图里的偏移
    pt = wt.POINT(0, 0); u32.ClientToScreen(h, ctypes.byref(pt))
    off = (pt.x - r.left, pt.y - r.top)
    return img, off, (r.left, r.top)


def clusters(img, step=3, minpts=8):
    """扫描粉色像素，8px 网格合并成连通簇，返回 [(bbox, npts), ...]。"""
    w, h = img.size
    px = img.load()
    pts = set()
    for y in range(0, h, step):
        for x in range(0, w, step):
            if is_pink(px[x, y]):
                pts.add((x // 8, y // 8))
    seen, out = set(), []
    for p in list(pts):
        if p in seen:
            continue
        stack, comp = [p], []
        seen.add(p)
        while stack:
            c = stack.pop(); comp.append(c)
            for dx in (-1, 0, 1):
                for dy in (-1, 0, 1):
                    n = (c[0] + dx, c[1] + dy)
                    if n in pts and n not in seen:
                        seen.add(n); stack.append(n)
        if len(comp) >= minpts:
            xs = [c[0] * 8 for c in comp]; ys = [c[1] * 8 for c in comp]
            out.append(((min(xs), min(ys), max(xs) + 8, max(ys) + 8), len(comp)))
    return out


def click(h, imgx, imgy, off, winxy):
    """把整窗图坐标换算成屏幕坐标并点击。"""
    sx = winxy[0] + imgx
    sy = winxy[1] + imgy
    u32.SetForegroundWindow(h)
    time.sleep(0.15)
    u32.SetCursorPos(int(sx), int(sy))
    time.sleep(0.1)
    u32.mouse_event(2, 0, 0, 0, 0)   # LEFTDOWN
    time.sleep(0.05)
    u32.mouse_event(4, 0, 0, 0, 0)   # LEFTUP


def set_text(h, text):
    """把地址填进「最宽的」EDIT —— 地址框横跨左栏，分P/并行等窄框不是目标。
    （z 序第一个 EDIT 是分P框，直接取第一个会填错地方。）"""
    res = []

    @ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
    def cb(c, l):
        b = ctypes.create_unicode_buffer(64)
        u32.GetClassNameW(c, b, 64)
        if b.value.lower() == 'edit':
            r = wt.RECT()
            u32.GetWindowRect(c, ctypes.byref(r))
            res.append((r.right - r.left, c))
        return True
    u32.EnumChildWindows(h, cb, 0)
    if not res:
        return False
    res.sort(reverse=True)
    u32.SendMessageW(res[0][1], 0x000C, 0, text)  # WM_SETTEXT
    return True


def main():
    # 清场
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
        if f[0]:
            h = f[0]
            break
    if not h:
        print('FAIL: window not found'); sys.exit(1)
    time.sleep(1.5)

    img, off, win = grab(h)
    img.save(os.path.join(OUT, 't0_initial.png'))
    W, H = img.size
    print(f'window {W}x{H} clientoff={off}')

    if not set_text(h, URL):
        print('FAIL: no EDIT found'); sys.exit(1)
    print('url set ok')

    # 找粉色主按钮
    cls = clusters(img)
    print('pink clusters:', [(b, n) for b, n in cls])
    if len(cls) < 2:
        print('FAIL: expected >=2 pink buttons'); sys.exit(1)
    # 解析视频：宽 80~300 的粉色簇（复选框 ~24px 太窄，通栏开始下载 >500 太宽）
    srt = sorted(cls, key=lambda t: t[0][2] - t[0][0])
    narrow = [c for c in srt if 80 <= (c[0][2] - c[0][0]) < 300]
    wide = [c for c in srt if (c[0][2] - c[0][0]) >= 500]
    if not narrow or not wide:
        print('FAIL: cannot classify buttons', srt); sys.exit(1)
    pb = narrow[0][0]           # 解析视频
    sb = wide[-1][0]            # 开始下载（通栏）
    pc = ((pb[0] + pb[2]) // 2, (pb[1] + pb[3]) // 2)
    sc = ((sb[0] + sb[2]) // 2, (sb[1] + sb[3]) // 2)
    print(f'parse btn bbox={pb} center={pc}')
    print(f'start btn bbox={sb} center={sc}')

    click(h, pc[0], pc[1], off, win)
    print('clicked parse, waiting...')
    time.sleep(10)
    img2, _, _ = grab(h)
    img2.save(os.path.join(OUT, 't1_parsed.png'))

    # 解析后标题占行会让地址卡加高、按钮整体下移 —— 开始下载的坐标
    # 必须重新扫描，用解析前的旧坐标会点空（2026-10-07 踩过）。
    cls2 = clusters(img2)
    srt2 = sorted(cls2, key=lambda t: t[0][2] - t[0][0])
    wide2 = [c for c in srt2 if (c[0][2] - c[0][0]) >= 500]
    if not wide2:
        print('FAIL: start button not found after parse', cls2); sys.exit(1)
    sb2 = wide2[-1][0]
    sc = ((sb2[0] + sb2[2]) // 2, (sb2[1] + sb2[3]) // 2)
    print(f'start btn after parse bbox={sb2} center={sc}')

    click(h, sc[0], sc[1], off, win)
    print('clicked start')
    # 下载窗口可能只有几秒，先密后疏采样
    delays = [(1, 's1'), (1, 's2'), (1, 's3'), (1, 's4'), (2, 's6'),
              (4, 't3_10s'), (15, 't4_25s'), (35, 't5_60s')]
    for d, name in delays:
        time.sleep(d)
        im, _, _ = grab(h)
        im.save(os.path.join(OUT, f'{name}.png'))
        print(f'saved {name}')

    # 简单量化：t1 vs t5 在任务台区域（进度条/曲线区）的差异像素数
    a = img2.load(); b = img2.load()
    im5 = Image.open(os.path.join(OUT, 't5_60s.png'))
    a = img2.load(); b = im5.load()
    diff = 0
    for y in range(H // 2, H, 2):
        for x in range(0, W, 2):
            if a[x, y] != b[x, y]:
                diff += 1
    print(f'diff pixels (lower half, t1 vs t5): {diff}')
    subprocess.run(['taskkill', '/IM', 'UI验证.exe', '/F'], capture_output=True)
    print('DONE')


if __name__ == '__main__':
    main()
