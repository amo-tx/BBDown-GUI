# win_dump.py —— 用 ctypes 枚举目标进程的窗口树：类名/样式/可见/可用/z序/焦点。
# 纯 user32，无第三方依赖。用法：python win_dump.py <PID>
import ctypes, ctypes.wintypes as wt, sys

u32 = ctypes.WinDLL('user32', use_last_error=True)
EnumWindows = u32.EnumWindows
EnumWindowsProc = ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
EnumChildWindows = u32.EnumChildWindows
GetWindowThreadProcessId = u32.GetWindowThreadProcessId
GetClassNameW = u32.GetClassNameW
GetWindowTextW = u32.GetWindowTextW
GetWindowLongW = u32.GetWindowLongW
GetWindowRect = u32.GetWindowRect
IsWindowVisible = u32.IsWindowVisible
IsWindowEnabled = u32.IsWindowEnabled
GetWindow = u32.GetWindow
GetParent = u32.GetParent

class GUITHREADINFO(ctypes.Structure):
    _fields_ = [('cbSize', wt.DWORD), ('flags', wt.DWORD),
                ('hwndActive', wt.HWND), ('hwndFocus', wt.HWND),
                ('hwndCapture', wt.HWND), ('hwndMenuOwner', wt.HWND),
                ('hwndMoveSize', wt.HWND), ('hwndCaret', wt.HWND),
                ('rcCaret', wt.RECT)]
GetGUIThreadInfo = u32.GetGUIThreadInfo

WS_CHILD=0x40000000; WS_VISIBLE=0x10000000; WS_DISABLED=0x08000000; WS_TABSTOP=0x00010000
ES_MULTILINE=0x0004; ES_AUTOVSCROLL=0x0040; ES_AUTOHSCROLL=0x0080
WS_EX_NOACTIVATE=0x08000000; WS_EX_CLIENTEDGE=0x00000200
GW_HWNDPREV=3

def cls(h):
    b=ctypes.create_unicode_buffer(256); GetClassNameW(h,b,256); return b.value
def txt(h):
    b=ctypes.create_unicode_buffer(512); GetWindowTextW(h,b,512); return b.value
def style_str(s):
    b=[]
    b.append('WS_CHILD' if s&WS_CHILD else '-')
    b.append('VIS' if s&WS_VISIBLE else '!VIS')
    b.append('DISABLED' if s&WS_DISABLED else 'en')
    if s&WS_TABSTOP: b.append('TABSTOP')
    if s&ES_MULTILINE: b.append('ML')
    if s&ES_AUTOHSCROLL: b.append('AHS')
    return '|'.join(b)
def zorder(t):
    h=GetWindow(t, GW_HWNDPREV); i=0
    while h and i<500:
        if h==t: return i
        h=GetWindow(h, GW_HWNDPREV); i+=1
    return -1

def find_main(pid):
    found=[0]
    def cb(h,l):
        p=wt.DWORD(); GetWindowThreadProcessId(h, ctypes.byref(p))
        if (pid==0 or p.value==pid) and IsWindowVisible(h):
            r=wt.RECT(); GetWindowRect(h, ctypes.byref(r))
            if (r.right-r.left)>200 and (r.bottom-r.top)>200 and 'Walk' in cls(h):
                found[0]=h; return False
        return True
    EnumWindows(EnumWindowsProc(cb), 0)
    return found[0]

def walk(parent, depth, out):
    def cb(h,l):
        r=wt.RECT(); GetWindowRect(h, ctypes.byref(r))
        st=GetWindowLongW(h,-16)&0xFFFFFFFF; ex=GetWindowLongW(h,-20)&0xFFFFFFFF
        ind='  '*depth
        line=f"{ind}0x{h:X} cls={cls(h)} z={zorder(h)} vis={bool(IsWindowVisible(h))} en={bool(IsWindowEnabled(h))} rect={r.left},{r.top},{r.right-r.left}x{r.bottom-r.top} [{style_str(st)}]"
        if ex & WS_EX_NOACTIVATE: line+=' ex[NOACTIVATE]'
        t=txt(h)
        if t: line+=f' txt="{t}"'
        out.append(line)
        if depth<6: walk(h, depth+1, out)
        return True
    EnumChildWindows(parent, EnumWindowsProc(cb), 0)

def main():
    pid=int(sys.argv[1])
    h=find_main(pid)
    if not h:
        print('NO MAIN WINDOW for pid', pid); return
    out=[]
    out.append(f'main=0x{h:X} cls={cls(h)} txt="{txt(h)}"')
    tid=GetWindowThreadProcessId(h, None)
    gti=GUITHREADINFO(); gti.cbSize=ctypes.sizeof(gti)
    if GetGUIThreadInfo(tid, ctypes.byref(gti)):
        fa=gti.hwndActive or 0; ff=gti.hwndFocus or 0; fc=gti.hwndCaret or 0
        out.append(f'[focus] active=0x{fa:X} focus=0x{ff:X} caret=0x{fc:X} focusCls={cls(gti.hwndFocus) if gti.hwndFocus else "NONE"}')
    else:
        out.append('[focus] GetGUIThreadInfo failed')
    walk(h,1,out)
    print('\n'.join(out))

if __name__=='__main__':
    main()