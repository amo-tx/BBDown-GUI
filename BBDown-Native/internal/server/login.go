package server

import (
	"bytes"
	"image/png"
	"net/http"
	"strings"
	"time"

	"bbdown-native/internal/bilibili"
)

type loginDTO struct {
	Mode    string `json:"mode"`
	State   string `json:"state"`
	Message string `json:"message"`
	Seq     int    `json:"seq"`
	Remain  int    `json:"remain"`
	Running bool   `json:"running"`
	HasQR   bool   `json:"has_qr"`
	URL     string `json:"url"`
	UName   string `json:"uname"`
}

func (s *Server) apiLoginStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.login.Snapshot()
	writeJSON(w, loginDTO{
		Mode:    snap.Mode.String(),
		State:   snap.State.String(),
		Message: snap.Message,
		Seq:     snap.Seq,
		Remain:  snap.Remain,
		Running: snap.Running,
		HasQR:   s.login.QRImage() != nil,
		URL:     snap.URL,
		UName:   s.cfg.UName,
	})
}

func (s *Server) apiLoginStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}
	mode := bilibili.ParseLoginMode(in.Mode)

	ok, msg := s.login.Start(mode, func(res *bilibili.LoginResult) {
		s.applyLoginResult(res)
	})
	if !ok {
		writeJSON(w, map[string]any{"ok": false, "message": msg})
		return
	}
	s.logf("已请求 %s 端登录二维码", modeLabel(mode))
	writeJSON(w, map[string]any{"ok": true, "mode": mode.String()})
}

func (s *Server) apiLoginCancel(w http.ResponseWriter, r *http.Request) {
	s.login.Cancel()
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiLoginQR(w http.ResponseWriter, r *http.Request) {
	img := s.login.QRImage()
	if img == nil {
		http.NotFound(w, r)
		return
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		writeErr(w, http.StatusInternalServerError, "二维码编码失败：%v", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) apiLoginCookie(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Cookie string `json:"cookie"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}
	cookie := bilibili.NormalizeCookie(in.Cookie)
	if !bilibili.HasAuthCookie(cookie) {
		writeErr(w, http.StatusBadRequest, "这段文本里没有 SESSDATA，不是有效的 cookie")
		return
	}
	acct, err := s.verifyAndStore(r.Context(), cookie, true)
	if err != nil {
		s.logf("✗ Cookie 校验失败：%v", err)
		writeErr(w, http.StatusBadRequest, "校验失败：%v", err)
		return
	}
	if !acct.LoggedIn {
		s.logf("✗ Cookie 未通过校验")
		writeErr(w, http.StatusBadRequest, "Cookie 已失效：%s", acct.Message)
		return
	}
	s.logf("✓ Cookie 校验通过：%s", acct.UName)
	writeJSON(w, map[string]any{"ok": true, "uname": acct.UName, "mid": acct.MID})
}

func (s *Server) apiLoginLogout(w http.ResponseWriter, r *http.Request) {
	s.login.Cancel()
	s.cfg.Cookie = ""
	s.cfg.AccessToken = ""
	s.cfg.UName = ""
	s.cfg.MID = 0
	if err := s.cfg.Save(); err != nil {
		writeErr(w, http.StatusInternalServerError, "清除配置失败：%v", err)
		return
	}
	s.runner.SetCredentials("", "")
	s.mu.Lock()
	s.account = bilibili.Account{}
	s.accountAt = time.Now()
	s.mu.Unlock()
	s.logf("已退出登录")
	writeJSON(w, map[string]any{"ok": true})
}

// applyLoginResult 是登录成功后的落盘动作。
func (s *Server) applyLoginResult(res *bilibili.LoginResult) {
	if res == nil {
		return
	}
	if res.Cookie != "" {
		s.cfg.Cookie = bilibili.NormalizeCookie(res.Cookie)
	}
	if res.Token != "" {
		s.cfg.AccessToken = strings.TrimSpace(res.Token)
	}
	if res.UName != "" {
		s.cfg.UName = res.UName
	}
	if res.MID != 0 {
		s.cfg.MID = res.MID
	}
	if err := s.cfg.Save(); err != nil {
		s.logf("✗ 登录成功，但保存配置失败：%v", err)
	} else {
		s.logf("✓ 登录成功：%s", firstNonEmptyStr(res.UName, "已登录"))
	}
	s.runner.SetCredentials(s.cfg.Cookie, s.cfg.AccessToken)
	s.setAccount(bilibili.Account{
		LoggedIn: true,
		UName:    res.UName,
		MID:      res.MID,
		Message:  "已登录",
	})
}

func modeLabel(m bilibili.LoginMode) string {
	if m == bilibili.LoginTV {
		return "电视"
	}
	return "网页"
}
