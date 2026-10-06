package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bbdown-native/internal/bilibili"
)

// ---------------------------------------------------------------------------
// 状态

type settingsDTO struct {
	Quality     int    `json:"quality"`
	CodecOrder  string `json:"codec_order"`
	Parallel    int    `json:"parallel"`
	KeepTemp    bool   `json:"keep_temp"`
	DownloadDir string `json:"download_dir"`
}

type accountDTO struct {
	Checked  bool   `json:"checked"`
	LoggedIn bool   `json:"logged_in"`
	UName    string `json:"uname"`
	MID      int64  `json:"mid"`
	Message  string `json:"message"`
}

type statusResp struct {
	App        string     `json:"app"`
	Version    string     `json:"version"`
	Engine     string     `json:"engine"`
	Muxer      string     `json:"muxer"`
	ConfigPath string     `json:"config_path"`
	Warning    string     `json:"warning"`
	CookieSet  bool       `json:"cookie_set"`
	TokenSet   bool       `json:"token_set"`
	Account    accountDTO `json:"account"`
	Settings   settingsDTO `json:"settings"`
	Running    bool       `json:"running"`
}

func (s *Server) settingsSnapshot() settingsDTO {
	return settingsDTO{
		Quality:     s.cfg.Quality,
		CodecOrder:  s.cfg.CodecOrder,
		Parallel:    s.cfg.Parallel,
		KeepTemp:    s.cfg.KeepTemp,
		DownloadDir: s.cfg.DownloadDir,
	}
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	acct := s.account
	checked := !s.accountAt.IsZero()
	running := s.task.running
	s.mu.Unlock()

	writeJSON(w, statusResp{
		App:        AppName,
		Version:    AppVersion,
		Engine:     "Go 原生 · 单文件",
		Muxer:      "内置 MP4 封装（无需 ffmpeg）",
		ConfigPath: s.cfg.Path(),
		Warning:    s.warn,
		CookieSet:  bilibili.HasAuthCookie(s.cfg.Cookie),
		TokenSet:   strings.TrimSpace(s.cfg.AccessToken) != "",
		Account: accountDTO{
			Checked:  checked,
			LoggedIn: acct.LoggedIn,
			UName:    acct.UName,
			MID:      acct.MID,
			Message:  acct.Message,
		},
		Settings: s.settingsSnapshot(),
		Running:  running,
	})
}

// setAccount 记住一次账号校验结果。
func (s *Server) setAccount(a bilibili.Account) {
	s.mu.Lock()
	s.account = a
	s.accountAt = time.Now()
	s.mu.Unlock()
}

// checkAccount 在启动时校验一次已保存的 cookie，结果只用来显示。
func (s *Server) checkAccount() {
	if !bilibili.HasAuthCookie(s.cfg.Cookie) {
		return
	}
	s.verifyAndStore(context.Background(), s.cfg.Cookie, false)
}

// verifyAndStore 校验 cookie 并把结果写进内存（save=true 时也写盘）。
func (s *Server) verifyAndStore(ctx context.Context, cookie string, save bool) (*bilibili.Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	acct, err := s.runner.Client().Verify(ctx, cookie)
	if err != nil {
		s.setAccount(bilibili.Account{Message: err.Error()})
		return nil, err
	}
	s.setAccount(*acct)

	if save && acct.LoggedIn {
		s.cfg.Cookie = bilibili.NormalizeCookie(cookie)
		s.cfg.UName = acct.UName
		s.cfg.MID = acct.MID
		if err := s.cfg.Save(); err != nil {
			return acct, fmt.Errorf("登录成功，但保存配置失败：%w", err)
		}
		s.runner.SetCredentials(s.cfg.Cookie, s.cfg.AccessToken)
	}
	return acct, nil
}

// ---------------------------------------------------------------------------
// 配置

func (s *Server) apiConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, http.StatusMethodNotAllowed, "只支持 POST")
		return
	}
	var in settingsDTO
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}

	if in.CodecOrder != "" {
		s.cfg.CodecOrder = in.CodecOrder
	}
	if in.DownloadDir != "" {
		s.cfg.DownloadDir = strings.TrimSpace(in.DownloadDir)
	}
	s.cfg.Quality = in.Quality
	s.cfg.KeepTemp = in.KeepTemp
	if in.Parallel > 0 {
		s.cfg.Parallel = in.Parallel
	}

	warn := ""
	if err := s.cfg.Save(); err != nil {
		warn = "配置保存失败：" + err.Error()
	}
	writeJSON(w, map[string]any{
		"ok":       true,
		"settings": s.settingsSnapshot(),
		"warning":  warn,
	})
}

// ---------------------------------------------------------------------------
// 地址提取

type targetDTO struct {
	Raw     string `json:"raw"`
	Display string `json:"display"`
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Page    int    `json:"page"`
	Short   bool   `json:"short"`
}

func (s *Server) apiExtract(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}
	targets := bilibili.ExtractTargets(in.Text)
	out := make([]targetDTO, 0, len(targets))
	for _, t := range targets {
		out = append(out, targetDTO{
			Raw:     t.Raw,
			Display: t.Display(),
			ID:      t.ID,
			Kind:    t.Kind.String(),
			Page:    t.Page,
			Short:   t.Short,
		})
	}
	writeJSON(w, map[string]any{"targets": out, "count": len(out)})
}

// ---------------------------------------------------------------------------
// 解析

type pageDTO struct {
	Index    int    `json:"index"`
	Title    string `json:"title"`
	Duration int    `json:"duration"`
}

type qualityDTO struct {
	QN   int    `json:"qn"`
	Name string `json:"name"`
}

type itemDTO struct {
	Index     int          `json:"index"`
	Title     string       `json:"title"`
	Owner     string       `json:"owner"`
	Cover     string       `json:"cover"`
	Duration  int          `json:"duration"`
	IsBangumi bool         `json:"is_bangumi"`
	ID        string       `json:"id"`
	Default   int          `json:"default_page"`
	Pages     []pageDTO    `json:"pages"`
	Qualities []qualityDTO `json:"qualities"`
}

func (s *Server) apiParse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}

	// 用一个独立的超时：解析要打好几个接口，但也不该无限等下去。
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	if strings.TrimSpace(in.Text) == "" {
		writeErr(w, http.StatusBadRequest, "请先粘贴视频地址")
		return
	}
	s.logf("开始解析…")

	items, err := s.runner.Resolve(ctx, in.Text)
	if err != nil {
		s.logf("✗ 解析失败：%v", err)
		s.handleErr(w, err)
		return
	}

	s.mu.Lock()
	s.items = items
	s.mu.Unlock()

	out := make([]itemDTO, 0, len(items))
	for i, it := range items {
		info := it.Info
		dto := itemDTO{
			Index:     i,
			Title:     info.Title,
			Owner:     info.Owner,
			Cover:     info.Cover,
			Duration:  info.Duration,
			IsBangumi: info.IsBangumi,
			ID:        firstNonEmptyStr(info.Bvid, info.Target.ID),
			Default:   info.DefaultPage,
		}
		if dto.ID == "" {
			dto.ID = info.Target.ID
		}
		for _, p := range info.Pages {
			dto.Pages = append(dto.Pages, pageDTO{Index: p.Index, Title: p.Title, Duration: p.Duration})
		}
		for _, q := range it.Avail {
			dto.Qualities = append(dto.Qualities, qualityDTO{QN: q, Name: bilibili.QualityName(q)})
		}
		out = append(out, dto)
	}

	s.logf("✓ 解析完成，共 %d 个目标", len(out))
	writeJSON(w, map[string]any{"items": out, "count": len(out)})
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 剪贴板 / 打开目录 / 选目录 / 退出

func (s *Server) apiClipboard(w http.ResponseWriter, r *http.Request) {
	text, err := clipboardRead()
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%s", err.Error())
		return
	}
	writeJSON(w, map[string]any{"text": text})
}

func (s *Server) apiOpen(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}
	dir := strings.TrimSpace(in.Path)
	if dir == "" {
		dir = s.cfg.DownloadDir
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		// 目录还不存在就现建一个，比报错更符合直觉。
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeErr(w, http.StatusBadRequest, "目录不存在也建不出来：%s", dir)
			return
		}
	}
	if err := openInExplorer(dir); err != nil {
		writeErr(w, http.StatusInternalServerError, "打开目录失败：%v", err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiPickFolder(w http.ResponseWriter, r *http.Request) {
	start := s.cfg.DownloadDir
	if fi, err := os.Stat(start); err != nil || !fi.IsDir() {
		start = filepath.Dir(start)
	}
	dir, err := pickFolder(start)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "%s", err.Error())
		return
	}
	if dir == "" {
		writeJSON(w, map[string]any{"ok": false, "canceled": true})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "path": dir})
}

// apiVerify 手动重新校验一次已保存的凭据（界面上那个「重新校验」）。
func (s *Server) apiVerify(w http.ResponseWriter, r *http.Request) {
	if !bilibili.HasAuthCookie(s.cfg.Cookie) {
		writeJSON(w, accountDTO{
			Checked: true,
			Message: "本机还没有保存过 cookie",
		})
		return
	}
	acct, err := s.verifyAndStore(r.Context(), s.cfg.Cookie, false)
	if err != nil {
		writeJSON(w, accountDTO{Checked: true, Message: err.Error()})
		return
	}
	writeJSON(w, accountDTO{
		Checked:  true,
		LoggedIn: acct.LoggedIn,
		UName:    acct.UName,
		MID:      acct.MID,
		Message:  acct.Message,
	})
}

func (s *Server) apiQuit(w http.ResponseWriter, r *http.Request) {	writeJSON(w, map[string]any{"ok": true})
	go func() {
		time.Sleep(150 * time.Millisecond) // 先把响应发出去
		s.Shutdown()
	}()
}

// apiBye 是浏览器的「页面卸载」通知。它不是 authoritative 的：
// 真正决定退出的是 sweep 里的宽限期判断，这里只负责划一条时间线。
func (s *Server) apiBye(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.byeAt = time.Now()
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
