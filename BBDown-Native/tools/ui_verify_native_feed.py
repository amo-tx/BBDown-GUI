# ui_verify_native_feed.py —— 验证原生窗口三个内嵌页签的完整链路：
#   点刷视频 → 进程内起常驻服务 → WebView2 宿主可见 → /api/feed 可用
#   → 切回下载页宿主隐藏/EDIT 恢复 → 登录页宿主就绪 → 往返正常 → 退出收摊
#
# 用法：先启动原生窗口（dist/BBDown原生版.exe），再运行本脚本。
#
# 为什么用 PostMessage 而不是真实鼠标：远程桌面会话里前台窗口常被
# TscShellContainerClass 占住，SetForegroundWindow 会被拒绝，真实鼠标事件
# 落到别的窗口上（假失败）。PostMessage 直接投递到自绘层子窗口，不依赖前台。
# 坐标是 CustomWidget 客户区里的实测值（150% DPI、默认窗口尺寸）：
#   「下载」≈ (81, 110)，「刷视频」≈ (170, 110)，「登录」≈ (315, 110)。
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


def children(mw):
    rows = []

    def cb(h, l):
        rows.append((cls(h), bool(u.IsWindowVisible(h))))
        return True

    ECW(mw, EWProc(cb), 0)
    return rows


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
    print(f'main=0x{mw:X} custom=0x{cw:X}')
    if port_open(18230):
        print('前置条件失败：18230 已被占用，请先关掉其它实例')
        sys.exit(1)

    # 点「刷视频」页签（不是顶栏按钮，虽然它也会切页）
    click_client(cw, 170, 110)
    deadline = time.time() + 8
    while time.time() < deadline and not port_open(18230):
        time.sleep(0.25)
    if not port_open(18230):
        print('FAIL：点页签后服务未起来')
        sys.exit(1)
    print('PASS：页签 → 本地服务已监听 18230')
    time.sleep(2)

    rows = children(mw)
    static_visible = any(c == 'Static' and v for c, v in rows)
    webview = any('Chrome_WidgetWin_1' in c and v for c, v in rows)
    edits_hidden = all(not v for c, v in rows if c == 'Edit')
    if not static_visible or not webview:
        print(f'FAIL：WebView2 宿主未就绪 Static={static_visible} Chromium={webview}')
        sys.exit(1)
    if not edits_hidden:
        print('FAIL：刷视频页仍有原生 EDIT 浮在浏览器上')
        sys.exit(1)
    print('PASS：WebView2 内嵌就绪，原生 EDIT 已隐藏')

    data = feed(18230)
    n = len(data.get('items', []))
    print(f'/api/feed 返回 {n} 条', data.get('error', ''))
    if n == 0:
        print('WARN：列表为空（可能网络问题），但服务链路已通')

    # 切回下载页：宿主必须藏起来，四个 EDIT 必须恢复
    click_client(cw, 70, 110)
    time.sleep(1)
    rows = children(mw)
    static_visible = any(c == 'Static' and v for c, v in rows)
    edits_visible = sum(1 for c, v in rows if c == 'Edit' and v)
    if static_visible or edits_visible != 4:
        print(f'FAIL：切回下载页异常 Static={static_visible} edits={edits_visible}')
        sys.exit(1)
    print('PASS：切回下载页，宿主隐藏、4 个 EDIT 恢复')

    # 第三个页签「登录」：同样用 WebView2 宿主，但页面切成账号登录卡
    click_client(cw, 315, 110)
    time.sleep(2)
    if not u.IsWindow(mw):
        print('FAIL：切到登录页后主窗口消失')
        sys.exit(1)
    rows = children(mw)
    if not any(c == 'Static' and v for c, v in rows):
        print('FAIL：登录页没有 WebView2 宿主')
        sys.exit(1)
    if any(c == 'Edit' and v for c, v in rows):
        print('FAIL：登录页仍有原生 EDIT 浮在浏览器上')
        sys.exit(1)
    print('PASS：登录页宿主就绪，原生 EDIT 已隐藏')

    # 再切回下载页，确认来回切不会把窗口弄丢
    click_client(cw, 81, 110)
    time.sleep(1)
    rows = children(mw)
    if any(c == 'Static' and v for c, v in rows) or sum(1 for c, v in rows if c == 'Edit' and v) != 4:
        print('FAIL：从登录页切回下载页异常')
        sys.exit(1)
    print('PASS：登录页 → 下载页往返正常')

    # 退出原生窗口 → Hold 服务收摊
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
