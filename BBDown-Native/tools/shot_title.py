# shot_title.py —— 按窗口标题子串截图（任意类名，用于截老版 pywebview 窗口）
# 用法: python shot_title.py <标题子串> <out.png>
import ctypes, ctypes.wintypes as wt, sys
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


def main():
    title, out = sys.argv[1], sys.argv[2]
    h = [0]

    @ctypes.WINFUNCTYPE(ctypes.c_bool, wt.HWND, wt.LPARAM)
    def cb(hw, l):
        if not u32.IsWindowVisible(hw):
            return True
        b = ctypes.create_unicode_buffer(512)
        u32.GetWindowTextW(hw, b, 512)
        if title.lower() in b.value.lower():
            r = wt.RECT()
            u32.GetWindowRect(hw, ctypes.byref(r))
            if (r.right - r.left) > 300:
                h[0] = hw
                return False
        return True
    u32.EnumWindows(cb, 0)
    if not h[0]:
        print('NO WINDOW')
        sys.exit(1)
    hw = h[0]
    r = wt.RECT()
    u32.GetWindowRect(hw, ctypes.byref(r))
    w, ht = r.right - r.left, r.bottom - r.top
    hdc = u32.GetWindowDC(hw)
    mdc = g32.CreateCompatibleDC(hdc)
    bmp = g32.CreateCompatibleBitmap(hdc, w, ht)
    g32.SelectObject(mdc, wt.HGDIOBJ(bmp))
    u32.PrintWindow(hw, mdc, 2)

    class BMI(ctypes.Structure):
        _fields_ = [('s', wt.DWORD), ('w', wt.LONG), ('h', wt.LONG), ('p', wt.WORD),
                    ('b', wt.WORD), ('c', wt.DWORD), ('sz', wt.DWORD),
                    ('x', wt.LONG), ('y', wt.LONG), ('u', wt.DWORD), ('ci', wt.DWORD)]
    bi = BMI(); bi.s = ctypes.sizeof(BMI); bi.w = w; bi.h = -ht
    bi.p = 1; bi.b = 32
    buf = ctypes.create_string_buffer(w * ht * 4)
    g32.GetDIBits(mdc, wt.HBITMAP(bmp), 0, ht, buf, ctypes.byref(bi), 0)
    from PIL import Image
    Image.frombytes('RGB', (w, ht), bytes(buf), 'raw', 'BGRX').save(out)
    print(f'saved {out} {w}x{ht}')


if __name__ == '__main__':
    main()
