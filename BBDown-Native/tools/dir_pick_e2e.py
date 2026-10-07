# dir_pick_e2e.py —— 「选择保存目录」端到端回归验证：
# 启动 exe → 点「选择」→ 断言对话框是全盘树（回归防护：曾因 walk 把
# InitialDirPath 当 PidlRoot，树被钉死在当前目录、哪都去不了）
# → 按比例坐标点选 C: 盘节点 → 点「确定」→ 断言 config.download_dir
# 被更新 → 恢复原值。
#
# ⚠️ 不要用跨进程 SendMessage 发 BFFM_SETSELECTION 来"模拟导航"：
# 跨进程消息缓冲编组会让 SHBrowseForFolder 的处理直接 fail-fast
# （退出码 0xC0000409，整个进程没了）—— 那是自动化伪影，真实点击没问题。
import ctypes, ctypes.wintypes as wt, sys, os, time, subprocess, json
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
CFG = os.path.join(ROOT, 'dist', 'config.json')
OUT = os.path.join(ROOT, 'dist', '_udir')
os.makedirs(OUT, exist_ok=True)
BTN = (0.4528, 0.6059)          # 「选择」按钮在整窗图里的比例位置
# 对话框（605x632 基准）内：(C:) 系统盘节点与「确定」按钮的比例位置
TREE_ITEM = (150 / 605, 205 / 632)
OK_BTN = (346 / 605, 574 / 632)


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
    return img, (r.left, r.top)


def lclick(x, y):
    u32.SetCursorPos(int(x), int(y))
    time.sleep(0.12)
    u32.mouse_event(2, 0, 0, 0, 0)
    time.sleep(0.04)
    u32.mouse_event(4, 0, 0, 0, 0)


def find_top_windows(pid):
    out = []

    @ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
    def cb(hw, l):
        if not u32.IsWindowVisible(hw):
            return True
        cls = ctypes.create_unicode_buffer(128)
        u32.GetClassNameW(hw, cls, 128)
        p = wt.DWORD()
        u32.GetWindowThreadProcessId(hw, ctypes.byref(p))
        if p.value == pid:
            out.append((hw, cls.value))
        return True
    u32.EnumWindows(cb, 0)
    return out


def main():
    old = json.load(open(CFG, encoding='utf-8')).get('download_dir')
    print('original download_dir =', old)

    subprocess.run(['taskkill', '/IM', 'UI验证.exe', '/F'], capture_output=True)
    time.sleep(1)
    proc = subprocess.Popen([EXE], cwd=os.path.dirname(EXE))
    h = 0
    for _ in range(40):
        time.sleep(0.5)
        m = [x for x in find_top_windows(proc.pid) if 'Walk_MainWindow' in x[1]]
        if m:
            h = m[0][0]
            break
    if not h:
        print('FAIL: no main window'); sys.exit(1)

    r = wt.RECT(); u32.GetWindowRect(h, ctypes.byref(r))
    W, H = r.right - r.left, r.bottom - r.top
    u32.SetForegroundWindow(h)
    time.sleep(0.3)
    lclick(r.left + W * BTN[0], r.top + H * BTN[1])
    time.sleep(2.5)

    dlgs = [x for x in find_top_windows(proc.pid) if x[1] == '#32770']
    if not dlgs:
        print('FAIL: dialog not open'); sys.exit(1)
    d = dlgs[0][0]
    img, _ = grab(d)
    img.save(os.path.join(OUT, 'dialog.png'))

    dr = wt.RECT(); u32.GetWindowRect(d, ctypes.byref(dr))
    DW, DH = dr.right - dr.left, dr.bottom - dr.top
    lclick(dr.left + DW * TREE_ITEM[0], dr.top + DH * TREE_ITEM[1])   # 选中 C: 盘
    time.sleep(0.8)
    lclick(dr.left + DW * OK_BTN[0], dr.top + DH * OK_BTN[1])         # 确定
    time.sleep(1.5)

    new = json.load(open(CFG, encoding='utf-8')).get('download_dir')
    alive = proc.poll() is None
    closed = len([x for x in find_top_windows(proc.pid) if x[1] == '#32770']) == 0
    print('download_dir after =', new, '| proc alive:', alive, '| dialog closed:', closed)
    subprocess.run(['taskkill', '/IM', 'UI验证.exe', '/F'], capture_output=True)

    cfg = json.load(open(CFG, encoding='utf-8'))
    cfg['download_dir'] = old
    json.dump(cfg, open(CFG, 'w', encoding='utf-8'), ensure_ascii=False, indent=2)
    print('config restored')

    if alive and closed and new and new != old and new.startswith('C:'):
        print('E2E_OK')
    else:
        print('FAIL')
        sys.exit(1)


if __name__ == '__main__':
    main()
