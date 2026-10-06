# shot.py —— 抓 BBDown 原生窗口截图（PrintWindow，含 PW_RENDERFULLCONTENT），
# 用来肉眼确认自绘按钮的位置。python shot.py <out.png>
import ctypes, ctypes.wintypes as wt, sys
u32=ctypes.WinDLL('user32',use_last_error=True); g32=ctypes.WinDLL('gdi32',use_last_error=True)
try:
    ctypes.WinDLL('user32').SetProcessDPIAware()
except Exception: pass
EW=u32.EnumWindows; EWProc=ctypes.WINFUNCTYPE(ctypes.c_bool,wt.HWND,wt.LPARAM)
GetClassNameW=u32.GetClassNameW; GetWindowRect=u32.GetWindowRect; IsWindowVisible=u32.IsWindowVisible
PrintWindow=u32.PrintWindow
PrintWindow.argtypes=[wt.HWND, wt.HDC, wt.UINT]
PrintWindow.restype=wt.BOOL
g32.CreateCompatibleDC.restype=wt.HDC; g32.CreateCompatibleBitmap.restype=wt.HBITMAP
g32.SelectObject.argtypes=[wt.HDC,wt.HGDIOBJ]; g32.BitBlt.restype=wt.BOOL
g32.GetDIBits.argtypes=[wt.HDC, wt.HBITMAP, wt.UINT, wt.UINT, ctypes.c_void_p, ctypes.c_void_p, wt.UINT]
g32.GetDIBits.restype=wt.INT

def cls(h):
    b=ctypes.create_unicode_buffer(256); GetClassNameW(h,b,256); return b.value
def find_main(dlg=False):
    """默认找主窗口；dlg=True 时找最上层的 walk 对话框（扫码/登录等模态窗）。"""
    f=[0]
    def ok(h):
        if not IsWindowVisible(h): return False
        c=cls(h)
        want='Walk_Dialog_Class' if dlg else 'Walk_MainWindow'
        if want not in c: return False
        r=wt.RECT(); GetWindowRect(h,ctypes.byref(r))
        return (r.right-r.left)>200 and (r.bottom-r.top)>100
    def cb(h,l):
        if ok(h): f[0]=h; return False
        return True
    EW(EWProc(cb),0); return f[0]

def main():
    out=sys.argv[1] if len(sys.argv)>1 else 'win.png'
    dlg = len(sys.argv)>2 and sys.argv[2]=='dlg'
    h=find_main(dlg)
    if not h: print('NO WINDOW'); sys.exit(1)
    r=wt.RECT(); GetWindowRect(h,ctypes.byref(r))
    w,ht=r.right-r.left, r.bottom-r.top
    hdc=u32.GetWindowDC(h)
    mdc=g32.CreateCompatibleDC(hdc); bmp=g32.CreateCompatibleBitmap(hdc,w,ht)
    g32.SelectObject(mdc,wt.HGDIOBJ(bmp))
    PrintWindow(h,mdc,2)  # PW_RENDERFULLCONTENT
    # BITMAPFILEHEADER + BITMAPINFOHEADER (top-down)
    class BMIHDR(ctypes.Structure):
        _fields_=[('biSize',wt.DWORD),('biWidth',wt.LONG),('biHeight',wt.LONG),('biPlanes',wt.WORD),
                  ('biBitCount',wt.WORD),('biCompression',wt.DWORD),('biSizeImage',wt.DWORD),
                  ('biXPelsPerMeter',wt.LONG),('biYPelsPerMeter',wt.LONG),('biClrUsed',wt.DWORD),('biClrImportant',wt.DWORD)]
    bi=BMIHDR(); bi.biSize=ctypes.sizeof(BMIHDR); bi.biWidth=w; bi.biHeight=-ht  # 负=自上而下
    bi.biPlanes=1; bi.biBitCount=32; bi.biCompression=0
    buf=ctypes.create_string_buffer(w*ht*4)
    got=g32.GetDIBits(mdc,wt.HBITMAP(bmp),0,ht,buf,ctypes.byref(bi),0)
    # 组装 BMP 文件头
    import struct
    off=14+ctypes.sizeof(BMIHDR)
    fh=b'BM'+struct.pack('<IHHI',off+len(buf),0,0,off)
    ih=struct.pack('<IiiHHIIiiII',40,w,-ht,1,32,0,len(buf),2835,2835,0,0)
    open(out,'wb').write(fh+ih+bytes(buf))
    print(f'saved {out} {w}x{ht} getdibits={got}')

if __name__=='__main__': main()