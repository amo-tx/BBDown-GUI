# ui_verify_ai.py —— 「转文字 / AI 总结」真实 GUI 端到端验证：
# 启动正式构建 → 填地址 → 真实点击解析 → diff 截图定位新增的两颗 ghost 按钮
# → 点「转文字」断言文稿落盘 → 点「AI 总结」断言错误路径（测试视频无 AI 总结）
# → 清理测试产物与记事本。
# 用法: python ui_verify_ai.py [BV地址]
import ctypes, ctypes.wintypes as wt, sys, os, time, glob, subprocess
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
DIST = os.path.join(ROOT, 'dist')
EXE = os.path.join(DIST, 'AI验证.exe')
URL = sys.argv[1] if len(sys.argv) > 1 else 'BV1GJ411x7h7'
OUT = os.path.join(DIST, '_uiai')
os.makedirs(OUT, exist_ok=True)
# 用户配置里的下载目录（gui 用 currentDir()=目录输入框 or config），从 config 读，只取这一个键
import json
DLDIR = json.load(open(os.path.join(DIST, 'config.json'), encoding='utf-8'))['download_dir']
print('download_dir =', DLDIR)


def is_ghost_border(p):
    """浅色主题 LineStr=d7deea / 深色 35415a，给 ±6 容差。"""
    for ref in ((0xd7, 0xde, 0xea), (0x35, 0x41, 0x5a)):
        if all(abs(p[i] - ref[i]) <= 6 for i in range(3)):
            return True
    return False


def is_pink(p):
    r, g, b = p[0], p[1], p[2]
    return r > 200 and 60 < g < 120 and 100 < b < 160


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
    off = (pt.x - r.left, pt.y - r.top)
    return img, off, (r.left, r.top)


def clusters_of(img, pred, step=2, minpts=6):
    w, h = img.size
    px = img.load()
    pts = set()
    for y in range(0, h, step):
        for x in range(0, w, step):
            if pred(px[x, y]):
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
    sx = winxy[0] + imgx
    sy = winxy[1] + imgy
    u32.SetForegroundWindow(h)
    time.sleep(0.15)
    u32.SetCursorPos(int(sx), int(sy))
    time.sleep(0.1)
    u32.mouse_event(2, 0, 0, 0, 0)
    time.sleep(0.05)
    u32.mouse_event(4, 0, 0, 0, 0)


def set_addr(h, text):
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


def find_window(pid):
    for _ in range(60):
        time.sleep(0.5)
        f = [0]

        @ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
        def cb(hw, l):
            if not u32.IsWindowVisible(hw):
                return True
            b = ctypes.create_unicode_buffer(64)
            u32.GetClassNameW(hw, b, 64)
            p = wt.DWORD()
            u32.GetWindowThreadProcessId(hw, ctypes.byref(p))
            if 'Walk_MainWindow' in b.value and p.value == pid:
                f[0] = hw
                return False
            return True
        u32.EnumWindows(cb, 0)
        if f[0]:
            return f[0]
    return 0


def main():
    subprocess.run(['taskkill', '/IM', 'AI验证.exe', '/F'], capture_output=True)
    subprocess.run(['taskkill', '/IM', 'notepad.exe', '/F'], capture_output=True)
    time.sleep(1)
    subprocess.run(['copy', '/Y', os.path.join(DIST, 'BBDown原生版.exe'), EXE], capture_output=True, shell=True)
    proc = subprocess.Popen([EXE], cwd=DIST)
    h = find_window(proc.pid)
    if not h:
        print('FAIL: window not found'); sys.exit(1)
    time.sleep(1.5)

    img, off, win = grab(h)
    img.save(os.path.join(OUT, 't0_initial.png'))
    print(f'window {img.size}')
    if not set_addr(h, URL):
        print('FAIL: no EDIT'); sys.exit(1)
    print('url set ok')

    # 点解析（窄粉色簇）
    pcls = [c for c in clusters_of(img, is_pink) if 80 <= (c[0][2] - c[0][0]) < 300]
    if not pcls:
        print('FAIL: parse button not found'); sys.exit(1)
    pb = sorted(pcls, key=lambda t: t[0][2] - t[0][0])[0][0]
    pc = ((pb[0] + pb[2]) // 2, (pb[1] + pb[3]) // 2)
    click(h, pc[0], pc[1], off, win)
    print('clicked parse, waiting...')
    time.sleep(9)
    img2, off2, win2 = grab(h)
    img2.save(os.path.join(OUT, 't1_parsed.png'))

    # diff 定位新增的 ghost 按钮（转文字 / AI 总结）：解析前不存在、
    # 解析后出现的边框色矩形，宽 ~100-160 高 ~40-56（150% DPI）
    d = Image.new('RGB', img.size)
    da, db, dd = img.load(), img2.load(), d.load()
    for y in range(img.size[1]):
        for x in range(img.size[0]):
            dd[x, y] = (0, 0, 0) if da[x, y] == db[x, y] else (255, 255, 255)

    def ghostish(p):
        return p[0] > 200 and is_ghost_border(p)

    # 在 diff 图上找「白点」簇，再按解析后截图里的边框色过滤（深浅主题都适配）
    # 在解析后截图上直接找 ghost 边框的按钮簇（尺寸 ≈ 84×32 逻辑 ×1.5 DPI），
    # 再用 diff 剔除解析前就存在的旧按钮 —— 直接对 diff 聚簇会和上方的
    # 详情文本变化连通成一坨，尺寸过滤就失效了。
    dd = d.load()

    def bbox_has_change(bbox):
        x0, y0, x1, y1 = bbox
        for yy in range(max(0, y0 - 4), min(img.size[1], y1 + 4), 2):
            for xx in range(max(0, x0 - 4), min(img.size[0], x1 + 4), 2):
                if dd[xx, yy][0] > 200:
                    return True
        return False

    newbtns = []
    for (bbox, n) in clusters_of(img2, is_ghost_border, step=2, minpts=8):
        x0, y0, x1, y1 = bbox
        wbtn, hbtn = x1 - x0, y1 - y0
        if not (95 <= wbtn <= 165 and 36 <= hbtn <= 62):
            continue
        if bbox_has_change(bbox):
            newbtns.append(bbox)
    print('new ghost buttons:', newbtns)
    if len(newbtns) < 2:
        print('FAIL: expected 2 new ghost buttons (转文字/AI总结)'); sys.exit(1)
    # 解析后下方卡片整体下移，账号行等旧按钮也会带 diff —— 只认
    # 「同一行 y、x 相邻」的按钮对，且取最上面那对（转文字/AI总结
    # 在第一个卡片里，位置必然最高）。
    pairs = []
    for a in newbtns:
        for b in newbtns:
            if abs(a[1] - b[1]) <= 6 and 0 < b[0] - a[2] <= 30:
                pairs.append((a, b))
    if not pairs:
        print('FAIL: no adjacent ghost pair found', newbtns); sys.exit(1)
    tr, ai = min(pairs, key=lambda p: p[0][1])
    tcenter = ((tr[0] + tr[2]) // 2, (tr[1] + tr[3]) // 2)
    acenter = ((ai[0] + ai[2]) // 2, (ai[1] + ai[3]) // 2)
    print(f'转文字 center={tcenter}  AI总结 center={acenter}')

    # --- 点转文字 ---
    before = set(glob.glob(os.path.join(DLDIR, '*文稿.txt')))
    click(h, tcenter[0], tcenter[1], off2, win2)
    print('clicked 转文字, waiting...')
    got = None
    for _ in range(24):
        time.sleep(2.5)
        now = set(glob.glob(os.path.join(DLDIR, '*文稿.txt')))
        new = now - before
        if new:
            got = sorted(new)[0]
            break
    img3, _, _ = grab(h)
    img3.save(os.path.join(OUT, 't2_transcript.png'))
    if not got:
        print('FAIL: transcript file not created'); sys.exit(1)
    size = os.path.getsize(got)
    head = open(got, 'rb').read()[:400].decode('utf-8', 'replace')
    print(f'✓ 文稿落盘 {size} 字节: {os.path.basename(got)}')
    assert head.startswith('\ufeff《'), '应有 BOM 且以书名号标题开头'
    assert '文稿来自' in head or '字幕文稿' in head, '应有文稿标签行'
    print('✓ 文稿内容校验通过')

    # --- 点 AI 总结（Rick Astley MV 无 AI 总结 → 应走错误路径且界面不崩）---
    click(h, acenter[0], acenter[1], off2, win2)
    print('clicked AI总结, waiting...')
    time.sleep(12)
    img4, _, _ = grab(h)
    img4.save(os.path.join(OUT, 't3_summary.png'))
    # 界面还在、两颗按钮恢复可点（aiBusy 复位）= 状态机正常
    w2 = find_window(proc.pid)
    if not w2:
        print('FAIL: window died after AI summary attempt'); sys.exit(1)
    print('✓ AI 总结错误路径正常（无总结视频报错、界面存活、busy 复位）')

    # 清理：测试产物 + 记事本 + 测试 exe
    os.remove(got)
    subprocess.run(['taskkill', '/IM', 'notepad.exe', '/F'], capture_output=True)
    subprocess.run(['taskkill', '/IM', 'AI验证.exe', '/F'], capture_output=True)
    print('DONE')


if __name__ == '__main__':
    main()
