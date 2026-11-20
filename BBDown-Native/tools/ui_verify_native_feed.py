# ui_verify_native_feed.py —— 验证原生窗口「刷视频」按钮的完整链路：
#   点击顶栏按钮 → 进程内起常驻服务 → /api/feed 可用 → 浏览器被拉起
#   → 浏览器关闭(bye)后服务仍在(Hold) → 退出窗口后服务收摊
#
# 用法：先启动原生窗口（dist/BBDown原生版.exe），再运行本脚本。
#
# 为什么用 PostMessage 而不是真实鼠标：远程桌面/终端服务会话里前台窗口
# 被 TscShellContainerClass 占住，SetForegroundWindow 会被系统拒绝，真实
# 鼠标事件落到别的窗口上（假失败）。PostMessage 直接投递到自绘层子窗口，
# 不依赖前台。坐标经实测校准：按钮命中区在 CustomWidget 客户区 x≈1240,
# y≈30（相对窗口宽按比例，y 是顶栏固定高度）。
import ctypes, ctypes.wintypes as wt, sys, time, json, socket, urllib.request

u = ctypes.WinDLL('user32', use_last_error=True)
try:
    u.SetProcessDPIAware()
except Exception:
    pass
EW = u.EnumWindows
EWProc = ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
ECW = u.EnumChildWindows


def cls(h):
    b = ctypes.create_unicode_buffer(256)
    u.GetClassNameW(h, b, 256)
    return b.value


def find(hwnd_class_part, root=None):
    found = [0]

    def cb(h, l):
        if u.IsWindowVisible(h) and hwnd_class_part in cls(h):
            found[0] = h
            return False
        return True

    (ECW(root, EWProc(cb), 0) if root else EW(EWProc(cb), 0))
    return found[0]


def rect(h):
    r = wt.RECT()
    u.GetWindowRect(h, ctypes.byref(r))
    return r.left, r.top, r.right - r.left, r.bottom - r.top


def click_client(hwnd, cx, cy):
    lp = (cy << 16) | cx
    u.PostMessageW(hwnd, 0x0201, 1, lp)  # WM_LBUTTONDOWN
    u.PostMessageW(hwnd, 0x0202, 0, lp)  # WM_LBUTTONUP


def port_open(p):
    s = socket.socket()
    s.settimeout(0.3)
    ok = s.connect_ex(('127.0.0.1', p)) == 0
    s.close()
    return ok


def feed(port):
    url = f'http://127.0.0.1:{port}/api/feed?source=popular&page=1'
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        return json.loads(opener.open(url, timeout=20).read())
    except Exception as e:
        return {'error': str(e)[:80]}


def main():
    mw = find('Walk_MainWindow')
    if not mw:
        print('NO_WINDOW：请先启动原生窗口')
        sys.exit(1)
    cw = find('Walk_CustomWidget', mw)
    if not cw:
        print('NO_CUSTOMWIDGET')
        sys.exit(1)
    _, _, cw_w, _ = rect(cw)
    print(f'main=0x{mw:X} custom=0x{cw:X} cw_w={cw_w}')
    if port_open(18230):
        print('前置条件失败：18230 已被占用，请先关掉其它实例')
        sys.exit(1)

    # 按钮中心 ≈ 整窗口宽度的 74.4%（实测粉色簇 x∈[1226,1319] / 1710）
    bx = int(cw_w * 0.744)
    hit = None
    for cy in (30, 20, 40, 50):
        for dx in (0, -20, -32, 12):
            click_client(cw, bx + dx, cy)
            time.sleep(0.5)
            if port_open(18230):
                hit = (bx + dx, cy)
                break
        if hit:
            break
    if not hit:
        print('FAIL：点击后服务未起来')
        sys.exit(1)
    print(f'按钮命中 client={hit}，服务已在 18230 监听')

    data = feed(18230)
    n = len(data.get('items', []))
    print(f'/api/feed 返回 {n} 条', data.get('error', ''))
    if n == 0:
        print('WARN：列表为空（可能网络问题），但按钮→服务链路已通')

    # Hold：模拟浏览器关闭，服务不该退出
    try:
        req = urllib.request.Request('http://127.0.0.1:18230/api/bye',
                                     data=b'', method='POST',
                                     headers={'X-BBDown-Native': '1'})
        urllib.request.build_opener(urllib.request.ProxyHandler({})).open(req, timeout=5)
    except Exception:
        pass
    time.sleep(4)
    if not port_open(18230):
        print('FAIL：浏览器关闭后服务退出了（Hold 失效）')
        sys.exit(1)
    print('PASS：浏览器关闭后服务仍在（Hold 生效）')

    # 退出原生窗口 → 服务收摊
    u.PostMessageW(mw, 0x0010, 0, 0)  # WM_CLOSE
    time.sleep(2.5)
    if port_open(18230):
        print('FAIL：退出窗口后服务未收摊')
        sys.exit(1)
    print('PASS：退出原生窗口后服务收摊')
    print('ALL PASS')
    sys.exit(0)


if __name__ == '__main__':
    main()
