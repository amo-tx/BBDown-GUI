// Package server 把引擎包装成一个只监听回环地址的本地 HTTP 服务，
// 界面由系统浏览器承载。
//
// 为什么不是内嵌 WebView2：这是个「拿到即开用」的整合包，不该要求用户机器上
// 存在某个运行时。系统浏览器人人都有，而真正干活的解析 / 下载 / 封装全部在
// 本进程内完成，照样零外部依赖。
//
// 安全边界：只 bind 127.0.0.1；所有写操作（POST）必须带自定义头
// X-BBDown-Native —— 跨站请求带上自定义头会触发 CORS 预检，而本服务从不
// 返回 CORS 响应头，于是浏览器会直接把请求拦下来。这样别的网页即使猜到了
// 端口号也调不动接口。
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"bbdown-native/internal/app"
	"bbdown-native/internal/bilibili"
)

const (
	AppName    = "BBDown 原生版"
	AppVersion = "2.0.0"

	// defaultPort 是首选端口；被占用时往后顺延。
	defaultPort = 18230
	portTries   = 24

	// reqHeader 是写操作必须携带的自定义头，见包注释里的 CSRF 说明。
	reqHeader = "X-BBDown-Native"

	// byeGrace 是收到浏览器「页面已卸载」通知后的宽限期。
	// 留几秒是为了让刷新页面（先卸载再加载）不被误判成退出。
	byeGrace = 6 * time.Second

	// idleTimeout 是兜底：一直没有任何前端请求就收摊。刻意放得很宽，
	// 免得后台标签页被浏览器降频后把服务误关掉。
	idleTimeout = 3 * time.Minute
)

//go:embed web
var webFS embed.FS

var staticFS = func() fs.FS {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic(err) // 编译期就嵌进来了，取不到说明构建坏了
	}
	return sub
}()

// Server 持有整个应用的运行时状态。
type Server struct {
	cfg    *app.Config
	runner *app.Runner
	login  *bilibili.LoginSession
	warn   string

	mu        sync.Mutex
	task      taskState
	items     []*app.Resolved
	account   bilibili.Account
	accountAt time.Time
	seen      bool      // 是否已经有前端连上来过
	lastSeen  time.Time // 最后一次收到请求的时间
	byeAt     time.Time // 收到「页面卸载」的时间

	ln      net.Listener
	httpSrv *http.Server
	url     string

	quitOnce sync.Once
	quit     chan struct{}
}

// New 构造服务。warn 是启动时读配置留下的告警，会透传到界面上。
func New(cfg *app.Config, warn string) *Server {
	s := &Server{
		cfg:   cfg,
		warn:  warn,
		login: bilibili.NewLoginSession(),
		quit:  make(chan struct{}),
	}
	s.task.reset()
	// 钩子闭包捕获 s，所以必须先建 Server 再建 Runner。
	s.runner = app.NewRunner(cfg, app.Hooks{
		Stage:    s.onStage,
		Log:      s.onLog,
		Progress: s.onProgress,
		Output:   s.onOutput,
	})
	return s
}

// URL 返回服务地址，Listen 之前为空。
func (s *Server) URL() string { return s.url }

// Listen 绑定端口并开始服务。
func (s *Server) Listen() (string, error) {
	ln, err := listenLoopback()
	if err != nil {
		return "", err
	}
	s.ln = ln
	s.url = fmt.Sprintf("http://127.0.0.1:%d/", ln.Addr().(*net.TCPAddr).Port)

	s.httpSrv = &http.Server{
		Handler:           s.middleware(s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// 本地回环，不需要写超时：下载接口可能会跑很久。
	go func() { _ = s.httpSrv.Serve(ln) }()

	s.touch()
	go s.sweep()
	go s.checkAccount()
	return s.url, nil
}

// Wait 阻塞到服务被关掉。
func (s *Server) Wait() { <-s.quit }

// Shutdown 关停服务，可重复调用。
func (s *Server) Shutdown() {
	s.quitOnce.Do(func() {
		close(s.quit)
		if s.httpSrv != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = s.httpSrv.Shutdown(ctx)
		}
	})
}

// listenLoopback 从首选端口往后找一个能用的。
func listenLoopback() (net.Listener, error) {
	var lastErr error
	for i := 0; i < portTries; i++ {
		addr := fmt.Sprintf("127.0.0.1:%d", defaultPort+i)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			return ln, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("找不到可用端口（%d 起试了 %d 个）：%w", defaultPort, portTries, lastErr)
}

// ---------------------------------------------------------------------------
// 中间件

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.touch()

		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
			if r.Method == http.MethodPost && r.Header.Get(reqHeader) == "" {
				writeErr(w, http.StatusForbidden, "请求缺少来源标记，已被拒绝")
				return
			}
		} else {
			// 静态资源不缓存：升级后同端口可能复用，缓存住旧页面会很难排查。
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) touch() {
	s.mu.Lock()
	s.seen = true
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// sweep 负责「浏览器关了就把进程收掉」——窗口化的 exe 没有窗口可关，
// 不这么做就会留下一个看不见的常驻进程。
func (s *Server) sweep() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case <-t.C:
		}
		if s.shouldQuit() {
			s.Shutdown()
			return
		}
	}
}

func (s *Server) shouldQuit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 有任务在跑就绝不退出：用户关掉页面不代表不要那些文件了。
	if s.task.running {
		return false
	}
	if !s.seen {
		return false
	}
	if !s.byeAt.IsZero() && time.Since(s.byeAt) >= byeGrace {
		return true
	}
	return time.Since(s.lastSeen) >= idleTimeout
}

// ---------------------------------------------------------------------------
// 路由

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	mux.HandleFunc("/api/status", s.apiStatus)
	mux.HandleFunc("/api/verify", s.apiVerify)
	mux.HandleFunc("/api/config", s.apiConfig)
	mux.HandleFunc("/api/extract", s.apiExtract)
	mux.HandleFunc("/api/clipboard", s.apiClipboard)
	mux.HandleFunc("/api/parse", s.apiParse)
	mux.HandleFunc("/api/task", s.apiTask)
	mux.HandleFunc("/api/download", s.apiDownload)
	mux.HandleFunc("/api/stop", s.apiStop)
	mux.HandleFunc("/api/open", s.apiOpen)
	mux.HandleFunc("/api/pick-folder", s.apiPickFolder)
	mux.HandleFunc("/api/quit", s.apiQuit)
	mux.HandleFunc("/api/bye", s.apiBye)

	mux.HandleFunc("/api/login/start", s.apiLoginStart)
	mux.HandleFunc("/api/login/cancel", s.apiLoginCancel)
	mux.HandleFunc("/api/login/status", s.apiLoginStatus)
	mux.HandleFunc("/api/login/qrcode.png", s.apiLoginQR)
	mux.HandleFunc("/api/login/cookie", s.apiLoginCookie)
	mux.HandleFunc("/api/login/logout", s.apiLoginLogout)

	return mux
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	serveEmbedded(w, r, "index.html", "text/html; charset=utf-8")
}

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	serveEmbedded(w, r, "favicon.ico", "image/x-icon")
}

func serveEmbedded(w http.ResponseWriter, r *http.Request, name, ctype string) {
	raw, err := fs.ReadFile(staticFS, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	_, _ = w.Write(raw)
}

// ---------------------------------------------------------------------------
// 小工具

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

type errBody struct {
	Error string `json:"error"`
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(errBody{Error: fmt.Sprintf(format, args...)})
}

// decode 读请求体。空体不算错（前端有些调用就是不带参数）。
func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Server) handleErr(w http.ResponseWriter, err error) {
	msg := err.Error()
	switch {
	case errors.Is(err, context.Canceled):
		writeErr(w, http.StatusRequestTimeout, "已取消")
	case errors.Is(err, context.DeadlineExceeded):
		writeErr(w, http.StatusGatewayTimeout, "操作超时")
	default:
		writeErr(w, http.StatusBadRequest, "%s", msg)
	}
}
