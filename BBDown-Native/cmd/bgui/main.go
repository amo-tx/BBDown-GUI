// Command bgui 是 BBDown 原生版的入口。
//
// 单文件 exe，不依赖 Python / WebView2 / ffmpeg。
//
// 默认界面=本地服务 + 系统浏览器：解析、下载、封装全部在本进程内完成，
// 页面只是控制面板。系统浏览器人人都有，所以「拿到整合包双击就能用」
// 这个要求不需要任何运行时前置。
//
// 用 -ui native 可以改用内置的原生 Win32 窗口。
package main

import (
	"crypto/sha1"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/lxn/walk"

	"bbdown-native/internal/app"
	"bbdown-native/internal/gui"
	"bbdown-native/internal/server"
)

// exeDir 返回可执行文件所在目录。
func exeDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func main() {
	// walk 要求 GUI 跑在固定的 OS 线程上，弹窗、原生窗口都靠它。
	runtime.LockOSThread()

	ui := flag.String("ui", "browser", "界面方式：browser=本地服务+系统浏览器，native=原生窗口")
	noOpen := flag.Bool("no-open", false, "只起服务、不自动打开浏览器（调试用）")
	flag.Parse()

	cfg, warn := app.LoadConfig()

	if *ui == "native" {
		runNative(cfg, warn)
		return
	}
	runBrowser(cfg, warn, *noOpen)
}

// runNative 走内置的 Win32 窗口。
func runNative(cfg *app.Config, warn string) {
	var runErr error
	defer func() {
		if r := recover(); r != nil {
			path := writeCrashLog(r)
			msg := fmt.Sprintf("发生了未预期的错误：\n\n%v", r)
			if path != "" {
				msg += "\n\n详细信息已经写到：\n" + path
			}
			messageBox("程序异常", msg)
			os.Exit(1)
		}
		if runErr != nil {
			messageBox("启动失败", runErr.Error())
			os.Exit(1)
		}
	}()

	if warn != "" {
		runErr = gui.RunWithWarning(cfg, warn)
		return
	}
	runErr = gui.Run(cfg)
}

// writeCrashLog 把 panic 的文案与调用栈落盘，返回文件路径。
//
// 界面是 windowsgui 子系统，没有 stdout/stderr，panic 一旦被 recover 掉就
// 只剩一个消息框里的一行字 —— 「invalid memory address」这种提示对定位毫无
// 帮助。所以顺手把完整的栈写下来，用户截图或把文件发过来就能查。
//
// 优先写 exe 同级目录（绿色包的惯例，用户找得到）；写不进去就退到临时目录。
func writeCrashLog(r any) string {
	body := fmt.Sprintf("时间：%s\n错误：%v\n\n%s\n",
		time.Now().Format("2006-01-02 15:04:05"), r, debug.Stack())

	var candidates []string
	if dir, err := exeDir(); err == nil {
		candidates = append(candidates, filepath.Join(dir, "崩溃日志.txt"))
	}
	candidates = append(candidates, filepath.Join(os.TempDir(), "bbdown-native-crash.log"))

	for _, p := range candidates {
		if err := os.WriteFile(p, []byte(body), 0o600); err == nil {
			return p
		}
	}
	return ""
}

// runBrowser 起本地服务并拉起系统浏览器。
func runBrowser(cfg *app.Config, warn string, noOpen bool) {
	// 已经有一个实例在跑的话，把浏览器指过去就完事 ——
	// 不然双击两次会起两个互不相干的服务，用户根本分不清哪个在下载。
	if url := findRunning(); url != "" {
		if !noOpen {
			_ = server.OpenBrowser(url)
		}
		return
	}

	srv := server.New(cfg, warn)
	url, err := srv.Listen()
	if err != nil {
		messageBox("启动失败", err.Error())
		os.Exit(1)
	}
	writePortFile(url)
	defer clearPortFile()

	// 控制台构建（go run / 开发版）下这行能直接看到地址；
	// 发布版是 windowsgui 子系统，没有 stdout，写了也无害。
	fmt.Println("界面地址：", url)

	if !noOpen {
		if err := server.OpenBrowser(url); err != nil {
			messageBox("请手动打开界面",
				"浏览器没能自动打开，请手动访问：\n\n"+url)
		}
	}

	// 页面关掉（或超时无请求）后，服务会自己收摊 ——
	// windowsgui 的 exe 没有窗口可关，只能这样避免留下看不见的常驻进程。
	srv.Wait()
}

// ---------------------------------------------------------------------------
// 单实例

// portFile 放在临时目录里，文件名带 exe 路径的哈希。
//
// 刻意不写在 exe 旁边：那会往绿色包里塞一个隐藏文件，用户拷贝目录时
// 还得猜它是干什么的。
func portFile() string {
	exe, err := os.Executable()
	if err != nil {
		exe = "bbdown"
	}
	sum := sha1.Sum([]byte(strings.ToLower(exe)))
	return filepath.Join(os.TempDir(), fmt.Sprintf("bbdown-native-%x.url", sum[:8]))
}

// findRunning 看上一次启动留下的地址是不是还活着，是就返回它。
func findRunning() string {
	raw, err := os.ReadFile(portFile())
	if err != nil {
		return ""
	}
	url := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		return ""
	}
	if probe(url) {
		return url
	}
	return ""
}

// probe 请求一次 /api/status，确认对面确实是我们的服务。
func probe(url string) bool {
	cl := &http.Client{
		Timeout: 1500 * time.Millisecond,
		// 显式关掉代理：本机回环不该绕出去。
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := cl.Get(strings.TrimSuffix(url, "/") + "/api/status")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var s struct {
		App string `json:"app"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&s); err != nil {
		return false
	}
	return s.App == server.AppName
}

func writePortFile(url string) {
	_ = os.WriteFile(portFile(), []byte(url), 0o600)
}

func clearPortFile() { _ = os.Remove(portFile()) }

func messageBox(title, msg string) {
	_ = walk.MsgBox(nil, title, msg, walk.MsgBoxIconError)
}
