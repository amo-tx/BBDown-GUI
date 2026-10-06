# qr_verify.py —— 端到端验证「扫码登录」窗口：点按钮 → 等对话框 → 看二维码/状态/链接有没有自动填上。
# 用法：python qr_verify.py [按钮在截图里的x y]（默认点第一个登录按钮）
# 判定口径：对话框出现 1.5 s 内就该拿到二维码 URL；拿不到就是又退回了「打开是空码」。
import ctypes, ctypes.wintypes as wt, sys, time

u32 = ctypes.WinDLL('user32', use_last_error=True)
EW = u32.EnumWindows; EWProc = ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
ECW = u32.EnumChildWindows
GetClassNameW = u32.GetClassNameW; GetWindowTextW = u32.GetWindowTextW
GetWindowRect = u32.GetWindowRect; IsWindowVisible = u32.IsWindowVisible
SetForegroundWindow = u32.SetForegroundWindow; BringWindowToTop = u32.BringWindowToTop
SetCursorPos = u32.SetCursorPos; mouse_event = u32.mouse_event
u32.SetProcessDPIAware()

def cls(h):
    b = ctypes.create_unicode_buffer(256); GetClassNameW(h, b, 256); return b.value
def txt(h):
    b = ctypes.create_unicode_buffer(1024); GetWindowTextW(h, b, 1024); return b.value
def rect(h):
    r = wt.RECT(); GetWindowRect(h, ctypes.byref(r)); return r

def tops():
    out = []
    def cb(h, l):
        if IsWindowVisible(h):
            r = rect(h)
            out.append((h, cls(h), r.left, r.top, r.right - r.left, r.bottom - r.top, txt(h)))
        return True
    EW(EWProc(cb), 0)
    return out

def find_main():
    for h, c, *_ in tops():
        if 'Walk_MainWindow' in c and _[2] > 200:
            return h
    return 0

def click(x, y):
    SetCursorPos(x, y); time.sleep(0.08)
    mouse_event(0x0002, 0, 0, 0, 0); time.sleep(0.08)
    mouse_event(0x0004, 0, 0, 0, 0)

def walk_dump(root):
    out = []
    def cb(h, l):
        r = rect(h)
        out.append((cls(h), r.left, r.top, r.right - r.left, r.bottom - r.top, txt(h)))
        return True
    ECW(root, EWProc(cb), 0)
    return out

def main():
    m = find_main()
    if not m:
        print('NO MAIN WINDOW'); sys.exit(1)
    wr = rect(m)
    print('main=0x%X win=(%d,%d,%dx%d)' % (m, wr.left, wr.top, wr.right - wr.left, wr.bottom - wr.top))

    # 先确认桌面干净：只允许一个 walk 窗口
    walk_tops = [t for t in tops() if 'Walk' in t[1]]
    if len(walk_tops) != 1:
        print('环境不干净，walk 顶层窗口数 =', len(walk_tops))
        for h, c, l, t, w, ht, tx in walk_tops:
            print('  0x%X %s %dx%d %r' % (h, c, w, ht, tx[:30]))
        sys.exit(2)

    ShowWin = u32.ShowWindow
    ShowWin(m, 9); BringWindowToTop(m); SetForegroundWindow(m); time.sleep(0.4)

    if len(sys.argv) >= 3:
        bx, by = int(sys.argv[1]), int(sys.argv[2])
    else:
        bx, by = 66, 956          # 截图里「扫码登录」按钮中心（相对窗口左上角）
    sx, sy = wr.left + bx, wr.top + by
    print('点击 扫码登录 屏幕坐标=(%d,%d)' % (sx, sy))
    click(sx, sy)

    # 对话框是模态的，等它出来
    dlg = 0
    for _ in range(40):
        time.sleep(0.1)
        for h, c, l, t, w, ht, tx in tops():
            if 'Walk_Dialog' in c:
                dlg = h; break
        if dlg: break
    if not dlg:
        print('✗ 1.5s 内没有出现登录对话框'); sys.exit(1)
    dr = rect(dlg)
    print('对话框 0x%X %r rect=(%d,%d,%dx%d)' % (dlg, txt(dlg), dr.left, dr.top,
                                                 dr.right - dr.left, dr.bottom - dr.top))

    # 等二维码：URL 与状态文本应该在 1.5~2.5s 内自己填上
    url = ''; status = ''
    deadline = time.time() + 6
    while time.time() < deadline:
        for c, l, t, w, ht, tx in walk_dump(dlg):
            if c == 'Edit' and ('passport' in tx or 'http' in tx):
                url = tx
            if 'Static' in c and tx and ('二维码' in tx or '扫码' in tx or '失效' in tx or '秒' in tx or '登录' in tx):
                status = tx
        if url:
            break
        time.sleep(0.25)

    print('二维码链接 =', repr(url[:90]) if url else '(空 —— 没拿到)')
    print('状态文本   =', repr(status[:80]) if status else '(空)')
    ok = bool(url)
    print('判定：', '二维码已自动获取 ✓' if ok else '二维码为空 ✗')
    sys.exit(0 if ok else 1)

if __name__ == '__main__':
    main()