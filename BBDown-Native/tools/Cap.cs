// Screenshot helper for GUI verification. Compiled at runtime by shot.ps1
// via `Add-Type -Path`. Kept in its own file so the C# never has to travel
// through a PowerShell here-string (quoting there is fragile, and compile
// errors surface only as non-terminating errors that silently kill the type).
//
// Targets the C# 5 compiler that ships with Windows PowerShell 5.1 - avoid
// string interpolation, expression-bodied members, `out var`, etc.
using System;
using System.Drawing;
using System.Drawing.Imaging;
using System.Runtime.InteropServices;
using System.Text;

public class Cap
{
    [StructLayout(LayoutKind.Sequential)]
    public struct RECT { public int Left; public int Top; public int Right; public int Bottom; }

    [DllImport("user32.dll")] private static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")] private static extern bool GetClientRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")] private static extern bool PrintWindow(IntPtr h, IntPtr hdc, uint flags);
    [DllImport("user32.dll")] private static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll")] private static extern int GetSystemMetrics(int idx);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] private static extern int GetWindowTextW(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] private static extern int GetClassNameW(IntPtr h, StringBuilder s, int n);
    [DllImport("user32.dll")] private static extern IntPtr GetParent(IntPtr h);
    [DllImport("user32.dll")] private static extern bool SetProcessDPIAware();
    [DllImport("user32.dll")] private static extern uint GetDpiForWindow(IntPtr h);

    private delegate bool EnumProc(IntPtr h, IntPtr l);
    [DllImport("user32.dll")] private static extern bool EnumChildWindows(IntPtr parent, EnumProc cb, IntPtr l);

    // MUST be called before any measurement. Without it this PowerShell process
    // is only system-DPI-aware, so on a scaled display GetWindowRect returns
    // *virtualised* logical coordinates for our per-monitor-v2 target window.
    // The bitmap then gets sized to the logical rect while PrintWindow renders
    // at the true physical size, silently cropping the right and bottom edges -
    // which looks exactly like a layout overflow that isn't there.
    public static string MakeDpiAware()
    {
        bool ok = SetProcessDPIAware();
        return "SetProcessDPIAware=" + ok.ToString();
    }

    public static string Dpi(IntPtr h)
    {
        uint d = GetDpiForWindow(h);
        double scale = d / 96.0;
        return string.Format("dpi={0} scale={1:F2}", d, scale);
    }

    // Shrink the window to a given client-ish size so the layout can be checked
    // at its minimum. This is the failure mode that a screenshot at the default
    // size never shows: a flex child whose min-height collapses to 0 once the
    // window gets short, which makes a whole block of text silently vanish.
    [DllImport("user32.dll")]
    private static extern bool SetWindowPos(IntPtr h, IntPtr after, int x, int y, int cx, int cy, uint flags);

    public static string ResizeTo(IntPtr h, int w, int ht)
    {
        const uint SWP_NOMOVE = 0x0002;
        const uint SWP_NOZORDER = 0x0004;
        const uint SWP_NOACTIVATE = 0x0010;
        bool ok = SetWindowPos(h, IntPtr.Zero, 0, 0, w, ht, SWP_NOMOVE | SWP_NOZORDER | SWP_NOACTIVATE);
        return string.Format("resize to {0}x{1} = {2}", w, ht, ok);
    }

    public static string Metrics()
    {
        return GetSystemMetrics(0).ToString() + "x" + GetSystemMetrics(1).ToString();
    }

    public static string Info(IntPtr h)
    {
        RECT w;
        RECT c;
        bool okW = GetWindowRect(h, out w);
        bool okC = GetClientRect(h, out c);
        StringBuilder sb = new StringBuilder(512);
        int n = GetWindowTextW(h, sb, 512);
        return string.Format("visible={0} okW={1} rect={2},{3} {4}x{5} | okC={6} client={7}x{8} | len={9} title={10}",
            IsWindowVisible(h), okW, w.Left, w.Top, w.Right - w.Left, w.Bottom - w.Top,
            okC, c.Right - c.Left, c.Bottom - c.Top, n, sb.ToString());
    }

    // Dump the whole child-window tree with class, text and rect.
    // Rects are relative to the root window's client origin, so a control whose
    // right edge exceeds the client width is immediately visible as a number
    // instead of having to be eyeballed in a screenshot.
    public static string Dump(IntPtr root)
    {
        RECT rr;
        GetWindowRect(root, out rr);
        StringBuilder sb = new StringBuilder();
        DumpOne(sb, root, 0, rr.Left, rr.Top);
        EnumChildWindows(root, delegate(IntPtr h, IntPtr l)
        {
            DumpOne(sb, h, Depth(h, root), rr.Left, rr.Top);
            return true;
        }, IntPtr.Zero);
        return sb.ToString();
    }

    private static int Depth(IntPtr h, IntPtr root)
    {
        int d = 0;
        IntPtr p = GetParent(h);
        while (p != IntPtr.Zero && p != root && d < 16)
        {
            d++;
            p = GetParent(p);
        }
        return d + 1;
    }

    private static void DumpOne(StringBuilder sb, IntPtr h, int depth, int ox, int oy)
    {
        RECT r;
        GetWindowRect(h, out r);
        StringBuilder cls = new StringBuilder(128);
        GetClassNameW(h, cls, 128);
        StringBuilder txt = new StringBuilder(256);
        GetWindowTextW(h, txt, 256);
        string t = txt.ToString().Replace("\r", " ").Replace("\n", " ");
        if (t.Length > 46) { t = t.Substring(0, 46) + ".."; }
        string pad = new string(' ', depth * 2);
        sb.AppendLine(string.Format("{0}{1} [{2},{3} {4}x{5}] vis={6} \"{7}\"",
            pad, cls.ToString(),
            r.Left - ox, r.Top - oy, r.Right - r.Left, r.Bottom - r.Top,
            IsWindowVisible(h) ? 1 : 0, t));
    }

    // Capture the whole window (frame included) and save as PNG. Returns a
    // status line; also reports the share of sampled pixels that are not
    // black, so a blank/cloaked capture is distinguishable from a real one.
    public static string Save(IntPtr h, string path)
    {
        RECT w;
        if (!GetWindowRect(h, out w)) { return "GetWindowRect failed"; }
        int ww = w.Right - w.Left;
        int hh = w.Bottom - w.Top;
        if (ww <= 0 || hh <= 0) { return "empty rect " + ww + "x" + hh; }

        using (Bitmap bmp = new Bitmap(ww, hh))
        {
            using (Graphics g = Graphics.FromImage(bmp))
            {
                IntPtr hdc = g.GetHdc();
                bool ok = PrintWindow(h, hdc, 2); // 2 = PW_RENDERFULLCONTENT
                g.ReleaseHdc(hdc);
                if (!ok) { return "PrintWindow returned false, rect " + ww + "x" + hh; }
            }

            long lit = 0;
            long total = 0;
            int step = 5;
            for (int y = 0; y < hh; y += step)
            {
                for (int x = 0; x < ww; x += step)
                {
                    Color col = bmp.GetPixel(x, y);
                    total++;
                    if (col.R + col.G + col.B > 60) { lit++; }
                }
            }
            double pct = total == 0 ? 0.0 : (100.0 * lit / total);
            bmp.Save(path, ImageFormat.Png);
            return string.Format("saved {0}  {1}x{2}  lit={3:F1}%", path, ww, hh, pct);
        }
    }
}
