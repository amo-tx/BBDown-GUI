# click_test.py —— 决定性测试：把窗口置前，真实点击一个 EDIT，看焦点是否落到它，
# 再敲一个字符并读回文本，判定「文本框能否获得焦点 / 能否输入」。
# 用法：python click_test.py [要点击的EDIT序号，默认1=addr]
import ctypes, ctypes.wintypes as wt, sys, time

u32=ctypes.WinDLL('user32',use_last_error=True)
EW=u32.EnumWindows; EWProc=ctypes.WINFUNCTYPE(ctypes.c_bool,wt.HWND,wt.LPARAM)
ECW=u32.EnumChildWindows
GetWindowThreadProcessId=u32.GetWindowThreadProcessId
GetClassNameW=u32.GetClassNameW; GetWindowTextW=u32.GetWindowTextW
GetWindowRect=u32.GetWindowRect; IsWindowVisible=u32.IsWindowVisible
SetForegroundWindow=u32.SetForegroundWindow; BringWindowToTop=u32.BringWindowToTop
GetForegroundWindow=u32.GetForegroundWindow
ShowWindow=u32.ShowWindow
SetCursorPos=u32.SetCursorPos; GetCursorPos=u32.GetCursorPos
mouse_event=u32.mouse_event; keybd_event=u32.keybd_event
SendMessage=u32.SendMessageW
SendMessage.argtypes=[wt.HWND, ctypes.c_uint, ctypes.c_size_t, ctypes.c_void_p]
SendMessage.restype=ctypes.c_ssize_t
PostMessage=u32.PostMessageW
PostMessage.argtypes=[wt.HWND, ctypes.c_uint, ctypes.c_size_t, ctypes.c_ssize_t]
PostMessage.restype=wt.BOOL
WM_GETTEXT=0x000D; WM_GETTEXTLENGTH=0x000E
MOUSEEVENTF_LEFTDOWN=0x0002; MOUSEEVENTF_LEFTUP=0x0004; MOUSEEVENTF_ABSOLUTE=0x8000; MOUSEEVENTF_MOVE=0x0001
KEYEVENTF_KEYUP=0x0002

# ---- SendInput 键盘注入（比 keybd_event 可靠）----
ULONG_PTR=ctypes.c_ulonglong if ctypes.sizeof(ctypes.c_void_p)==8 else ctypes.c_ulong
class KEYBDINPUT(ctypes.Structure):
    _fields_=[('wVk',wt.WORD),('wScan',wt.WORD),('dwFlags',wt.DWORD),('time',wt.DWORD),('dwExtraInfo',ULONG_PTR)]
class _U(ctypes.Union):
    _fields_=[('ki',KEYBDINPUT),('pad',ctypes.c_byte*32)]
class INPUT(ctypes.Structure):
    _anonymous_=('u',)
    _fields_=[('type',wt.DWORD),('u',_U)]
SendInput=u32.SendInput
SendInput.argtypes=[wt.UINT, ctypes.POINTER(INPUT), ctypes.c_int]
SendInput.restype=wt.UINT
INPUT_KEYBOARD=1
def send_key(vk):
    for flags in (0, KEYEVENTF_KEYUP):
        inp=INPUT(); inp.type=INPUT_KEYBOARD; inp.ki=KEYBDINPUT(vk,0,flags,0,0)
        SendInput(1, ctypes.byref(inp), ctypes.sizeof(INPUT)); time.sleep(0.06)

class GUITHREADINFO(ctypes.Structure):
    _fields_=[('cbSize',wt.DWORD),('flags',wt.DWORD),('hwndActive',wt.HWND),('hwndFocus',wt.HWND),
              ('hwndCapture',wt.HWND),('hwndMenuOwner',wt.HWND),('hwndMoveSize',wt.HWND),
              ('hwndCaret',wt.HWND),('rcCaret',wt.RECT)]
GetGUIThreadInfo=u32.GetGUIThreadInfo
SM_XVIRTUALSCREEN=76; SM_YVIRTUALSCREEN=77; SM_CXVIRTUALSCREEN=78; SM_CYVIRTUALSCREEN=79
GetSystemMetrics=u32.GetSystemMetrics

def cls(h):
    b=ctypes.create_unicode_buffer(256); GetClassNameW(h,b,256); return b.value
def gettext(h):
    n=SendMessage(h,WM_GETTEXTLENGTH,0,0)
    b=ctypes.create_unicode_buffer(n+2)
    SendMessage(h,WM_GETTEXT,n+2,ctypes.byref(b))
    return b.value
def focus_of(tid):
    g=GUITHREADINFO(); g.cbSize=ctypes.sizeof(g)
    if GetGUIThreadInfo(tid,ctypes.byref(g)):
        return (g.hwndFocus or 0, g.hwndCaret or 0)
    return (0,0)

def find_main():
    f=[0]
    def cb(h,l):
        if IsWindowVisible(h) and 'Walk_MainWindow' in cls(h):
            r=wt.RECT(); GetWindowRect(h,ctypes.byref(r))
            if (r.right-r.left)>200: f[0]=h; return False
        return True
    EW(EWProc(cb),0); return f[0]

def find_edits(root):
    eds=[]
    def cb(h,l):
        if cls(h)=='Edit': eds.append(h)
        return True
    ECW(root,EWProc(cb),0)
    return eds

def all_walk_windows():
    """列出当前桌面上所有可见的 walk 顶层窗口（主窗口 + 各种模态对话框）。
    用途：测试前断言「一个都没开」，否则前面测试留下的模态对话框会抢占前台，
    让本脚本对主窗口的点击全部落空 —— 那时的 ✗ 是环境造成的假失败，不是产品缺陷。"""
    res=[]
    def cb(h,l):
        if IsWindowVisible(h) and 'Walk' in cls(h):
            r=wt.RECT(); GetWindowRect(h,ctypes.byref(r))
            res.append((h, cls(h), r.left, r.top, r.right-r.left, r.bottom-r.top))
        return True
    EW(EWProc(cb),0)
    return res

def click_screen(x,y):
    SetCursorPos(x,y); time.sleep(0.05)
    mouse_event(MOUSEEVENTF_LEFTDOWN,0,0,0,0); time.sleep(0.05)
    mouse_event(MOUSEEVENTF_LEFTUP,0,0,0,0)

def main():
    # 前置断言：只允许存在一个主窗口，且它就是前台。多一个 = 环境脏，判定不可信。
    pre=all_walk_windows()
    if len(pre)!=1 or 'Walk_MainWindow' not in pre[0][1]:
        print('环境不干净：桌面上存在多个 walk 窗口，本轮结果不可信，请先清理残留进程')
        for h,c,l,t,w,ht in pre:
            print(f'  0x{h:X} cls={c} rect={l},{t},{w}x{ht}')
        sys.exit(2)

    main_h=find_main()
    if not main_h: print('NO WINDOW'); sys.exit(1)
    tid=GetWindowThreadProcessId(main_h,None)
    edits=find_edits(main_h)
    print(f'main=0x{main_h:X} edits={[hex(e) for e in edits]}')

    ShowWindow(main_h,9); BringWindowToTop(main_h); SetForegroundWindow(main_h)
    time.sleep(0.3)
    fg=GetForegroundWindow()
    print(f'前台窗口=0x{fg:X} 是不是本窗口: {"YES" if fg==main_h else "NO"}')
    if fg!=main_h:
        print('主窗口没能置前（多半有模态对话框抢占），本轮结果不可信')
        sys.exit(2)

    # 全部 EDIT 都测一遍：能否点到焦点
    allok=True
    for i,e in enumerate(edits):
        r=wt.RECT(); GetWindowRect(e,ctypes.byref(r))
        cx,cy=(r.left+r.right)//2,(r.top+r.bottom)//2
        f0,_=focus_of(tid)
        click_screen(cx,cy); time.sleep(0.25)
        f,c=focus_of(tid)
        ok = (f==e)
        allok = allok and ok
        print(f'  EDIT[{i}] 0x{e:X} rect={r.left},{r.top},{r.right-r.left}x{r.bottom-r.top} 点击后focus={"该EDIT ✓" if ok else f"0x{f:X}({cls(f) if f else "NONE"}) ✗"}')

    # 打字测试（仅当前台确实是本窗口时有效）
    if fg==main_h and edits:
        e=edits[0]
        r=wt.RECT(); GetWindowRect(e,ctypes.byref(r))
        click_screen((r.left+r.right)//2,(r.top+r.bottom)//2); time.sleep(0.25)
        f,_=focus_of(tid); print(f'打字前 focus={"该EDIT" if f==e else hex(f)}')
        before=gettext(e)
        keybd_event(0x58,0,0,0); time.sleep(0.06); keybd_event(0x58,0,KEYEVENTF_KEYUP,0); time.sleep(0.35)
        after=gettext(e)
        print(f'  敲X(keybd_event) 前={before!r} 后={after!r} 输入={"YES" if after!=before else "NO"}')
        # 直接发 WM_CHAR 绕过键盘/消息循环：判断 EDIT 本身是否接受字符
        b2=gettext(e)
        SendMessage(e, 0x0102, ord('Z'), 0)   # WM_CHAR 'Z'
        time.sleep(0.25)
        a2=gettext(e)
        print(f'  发WM_CHAR(Z) 前={b2!r} 后={a2!r} EDIT收字符={"YES" if a2!=b2 else "NO"}')
        # SendInput 真实按键路径（WM_KEYDOWN→TranslateMessage→WM_CHAR）
        b3=gettext(e); send_key(0x58); time.sleep(0.3); a3=gettext(e)
        print(f'  SendInput敲X 前={b3!r} 后={a3!r} 真实按键输入={"YES" if a3!=b3 else "NO"}')
        # PostMessage WM_KEYDOWN 进目标队列，走 walk 循环的 TranslateMessage→WM_CHAR
        b4=gettext(e); PostMessage(e, 0x0100, 0x58, 0x001E0001); time.sleep(0.4); a4=gettext(e)
        print(f'  PostMessage KEYDOWN 前={b4!r} 后={a4!r} 循环处理={"YES" if a4!=b4 else "NO"}')
    else:
        print('  (窗口不在前台，跳过打字测试——键盘输入只送前台窗口，此处结果不可信)')

    print('焦点判定：', '全部 ✓' if allok else '有 ✗')
    sys.exit(0 if allok else 1)

if __name__=='__main__': main()