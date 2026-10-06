//go:build windows

package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
)

// ---------------------------------------------------------------------------
// 启动外部程序

// hideWindow 让子进程不闪出控制台窗口。
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}

func openInExplorer(dir string) error {
	cmd := exec.Command("explorer.exe", filepath.Clean(dir))
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// explorer.exe 打开了窗口也会返回非 0 退出码，所以只回收不判定。
	go func() { _ = cmd.Wait() }()
	return nil
}

// OpenBrowser 用系统默认浏览器打开一个地址。
func OpenBrowser(url string) error {
	cmd := exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

// ---------------------------------------------------------------------------
// PowerShell 桥
//
// 剪贴板与目录选择框都走这里。剪贴板本来可以用 user32 的
// OpenClipboard/GlobalLock 直接读，但那条路必须把 Win32 返回的 uintptr
// 转成 unsafe.Pointer ——go vet 的 unsafeptr 检查会拦（三种写法都试过，
// 一律报 misuse）。与其在项目里塞一段需要打包豁免的 unsafe，不如统一
// 交给 PowerShell：慢半拍，但零 unsafe、零额外依赖，也和选择框共用一套机制。

// psResultPath 生成一个临时结果文件路径。结果不用 stdout 回传，
// 是因为 PowerShell 的 stdout 编码会受代码页影响，中文很容易被打乱。
func psResultPath(kind string) string {
	return filepath.Join(os.TempDir(),
		fmt.Sprintf("bbdown-%s-%d.txt", kind, time.Now().UnixNano()))
}

// runPS 跑一段 PowerShell 脚本（脚本自己把结果写成 UTF-8 文件）。
func runPS(script string) error {
	cmd := exec.Command("powershell.exe",
		"-NoProfile", "-NonInteractive", "-STA", "-EncodedCommand", encodePS(script))
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// readPSResult 读回结果文件；不存在表示脚本没写（用户取消 / 没有内容）。
func readPSResult(path string) (string, bool, error) {
	raw, err := os.ReadFile(path)
	_ = os.Remove(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimRight(string(raw), "\r\n"), true, nil
}

// psQuote 生成 PowerShell 单引号字面量（单引号本身用两个单引号转义）。
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// encodePS 转成 -EncodedCommand 要的 base64(UTF-16LE)。
//
// 用编码而不是直接传参：脚本里的中文和引号完全不经过命令行解析，
// 就不会被代码页转换弄乱（这是个老坑，PowerShell 按 ANSI 读参数时会毁掉非 ASCII）。
func encodePS(script string) string {
	units := utf16.Encode([]rune(script))
	buf := make([]byte, 0, len(units)*2)
	for _, u := range units {
		buf = append(buf, byte(u), byte(u>>8))
	}
	return base64.StdEncoding.EncodeToString(buf)
}

// ---------------------------------------------------------------------------
// 剪贴板

func clipboardRead() (string, error) {
	tmp := psResultPath("clip")
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
try {
  $t = Get-Clipboard -Raw -ErrorAction Stop
  if ($null -eq $t) { $t = '' }
  [System.IO.File]::WriteAllText(%s, [string]$t, [System.Text.Encoding]::UTF8)
} catch { }
`, psQuote(tmp))

	if err := runPS(script); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("读取剪贴板失败：%v", err)
	}
	text, ok, err := readPSResult(tmp)
	if err != nil {
		return "", err
	}
	if !ok || strings.TrimSpace(text) == "" {
		return "", errors.New("剪贴板里没有文本内容")
	}
	return text, nil
}

// ---------------------------------------------------------------------------
// 目录选择框

// pickFolder 弹一个系统目录选择框，返回空串表示用户取消。
//
// 走 WinForms 的 FolderBrowserDialog：Win32 那个 SHBrowseForFolder 的观感
// 很旧，而现代替代要拼 COM vtable，为一个文件夹对话框不值当。
func pickFolder(initial string) (string, error) {
	tmp := psResultPath("pick")
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName System.Windows.Forms | Out-Null
$dlg = New-Object System.Windows.Forms.FolderBrowserDialog
$dlg.Description = '选择下载目录'
$dlg.ShowNewFolderButton = $true
$init = %s
if (Test-Path -LiteralPath $init -PathType Container) { $dlg.SelectedPath = $init }
$owner = New-Object System.Windows.Forms.Form
$owner.TopMost = $true
$r = $dlg.ShowDialog($owner)
$owner.Dispose()
if ($r -eq [System.Windows.Forms.DialogResult]::OK) {
  [System.IO.File]::WriteAllText(%s, $dlg.SelectedPath, [System.Text.Encoding]::UTF8)
}
`, psQuote(initial), psQuote(tmp))

	if err := runPS(script); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("打开目录选择框失败：%v", err)
	}
	dir, ok, err := readPSResult(tmp)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", nil
	}
	return strings.TrimSpace(dir), nil
}
