package server

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/cookie"
)

// 本文件是「更多登录方式」的四条补充通道：
//
//	GET  /api/login/browsers      列出本机浏览器配置（供用户挑一个导入）
//	POST /api/login/from-browser  从指定浏览器配置里读 Cookie 并登录
//	POST /api/login/import        从一段文本 / 一个文件导入凭证（BBDown.data 等）
//	POST /api/login/password      账号密码登录（走电视端接口，不要极验）
//
// 它们最终都汇到 applyLoginResult 同一条落盘路径上，与扫码登录行为一致。
//
// 关于短信验证码登录：实测做不了。B 站的短信接口
// （/x/passport-login/web/sms/send）强制要求先完成极验滑块，不带滑块参数
// 一律返回 -105「验证码错误」；而电视端虽然有一个 /x/passport-tv-login/sms/send
// 能发出短信，对应的登录端点却不接受任何短信形态的参数（枚举 28 种组合
// 全部返回 -400）。极验必须由真实浏览器执行 JS 才能过，本项目零依赖，
// 所以界面上没有这条入口 —— 用「从浏览器导入 Cookie」可以覆盖同样的需求。

// ---------------------------------------------------------------------------
// 浏览器

type browserProfileDTO struct {
	Index   int    `json:"index"`
	Browser string `json:"browser"`
	Name    string `json:"name"`
	HasAuth bool   `json:"has_auth"`
	Note    string `json:"note,omitempty"`
}

func (s *Server) apiLoginBrowsers(w http.ResponseWriter, r *http.Request) {
	profiles := cookie.Discover()
	out := make([]browserProfileDTO, 0, len(profiles))
	for i, p := range profiles {
		dto := browserProfileDTO{Index: i, Browser: p.Browser, Name: p.Name}
		// 探测只读 SQLite、不解密，所以很快也不会泄露任何值。
		if has, err := p.HasBilibili(); err != nil {
			dto.Note = "读取失败（浏览器可能正开着）"
		} else {
			dto.HasAuth = has
			if !has {
				dto.Note = "这个配置里没有 B 站登录"
			}
		}
		out = append(out, dto)
	}
	writeJSON(w, map[string]any{"ok": true, "profiles": out})
}

func (s *Server) apiLoginFromBrowser(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Index int `json:"index"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}

	profiles := cookie.Discover()
	if in.Index < 0 || in.Index >= len(profiles) {
		writeErr(w, http.StatusBadRequest, "请选择一个有效的浏览器配置")
		return
	}
	p := profiles[in.Index]

	ck, err := p.BilibiliCookie()
	if err != nil {
		switch {
		case errors.Is(err, cookie.ErrNoBilibiliLogin):
			s.logf("✗ %s：没有 B 站登录 Cookie —— 请先在那个浏览器里登录一次 bilibili.com", p.Display())
			writeErr(w, http.StatusBadRequest,
				"%s 里没有 B 站的登录 Cookie。请先用那个浏览器打开并登录 bilibili.com，再回来导入。", p.Display())
		case errors.Is(err, cookie.ErrAppBound):
			s.logf("✗ %s：Cookie 用了应用绑定加密，无法解密", p.Display())
			writeErr(w, http.StatusBadRequest,
				"%s 的 Cookie 用了应用绑定加密（Chrome 127+ 的新格式），无法解密。请改用扫码登录。", p.Display())
		default:
			s.logf("✗ %s：读取失败：%v", p.Display(), err)
			writeErr(w, http.StatusBadRequest,
				"读取 %s 失败：%v\n（浏览器正开着占用着数据库是常见原因，关掉浏览器再试）", p.Display(), err)
		}
		return
	}

	s.logf("从 %s 读到 Cookie，正在校验…", p.Display())
	s.finishCookieLogin(w, r, ck, "浏览器 "+p.Display())
}

// ---------------------------------------------------------------------------
// 凭证文件 / 文本

func (s *Server) apiLoginImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
		Path string `json:"path"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}

	text := in.Text
	source := "粘贴的文本"
	if strings.TrimSpace(text) == "" {
		path := strings.TrimSpace(in.Path)
		if path == "" {
			writeErr(w, http.StatusBadRequest, "请粘贴凭证内容，或选择一个凭证文件")
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			s.logf("✗ 读取凭证文件失败：%v", err)
			writeErr(w, http.StatusBadRequest, "读取文件失败：%v", err)
			return
		}
		text = string(raw)
		source = "文件 " + fileBase(path)
	}

	cred := bilibili.ParseCredentialText(text)
	if cred.Empty() {
		s.logf("✗ %s：没找到 SESSDATA 或 access_token", source)
		writeErr(w, http.StatusBadRequest,
			"%s：没找到可用的凭证。\n"+
				"需要 BBDown.data（一行 SESSDATA=…; bili_jct=…）或 BBDownTV.data（access_token）。", source)
		return
	}

	// 只有 access_token（BBDownTV.data）时无法用 nav 校验，直接存下来。
	if !bilibili.HasAuthCookie(cred.Cookie) && cred.Token != "" {
		s.cfg.AccessToken = cred.Token
		if err := s.cfg.Save(); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存配置失败：%v", err)
			return
		}
		s.runner.SetCredentials(s.cfg.Cookie, cred.Token)
		s.logf("✓ 已导入电视端 access_token（%s）", source)
		writeJSON(w, map[string]any{
			"ok": true, "kind": "token",
			"message": "已导入电视端凭证。它会用于会员清晰度，但界面上的昵称需要网页端 Cookie 才能显示。",
		})
		return
	}

	s.logf("从 %s 读到 Cookie，正在校验…", source)
	s.finishCookieLogin(w, r, cred.Cookie, source)
}

// ---------------------------------------------------------------------------
// 账号密码

func (s *Server) apiLoginPassword(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}

	// 密码只在这一帧里存在：不进配置、不进日志、不回显。
	res, err := s.runner.Client().LoginWithPassword(r.Context(), in.Username, in.Password)
	if err != nil {
		s.logf("✗ 账号密码登录失败：%v", err)
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	s.applyLoginResult(res)
	if res.Token != "" && bilibili.HasAuthCookie(res.Cookie) {
		s.logf("✓ 账号密码登录成功（网页 Cookie + 电视端 token 都已保存）")
	} else if res.Token != "" {
		s.logf("✓ 账号密码登录成功（电视端凭证）")
	}
	writeJSON(w, map[string]any{"ok": true, "uname": res.UName, "mid": res.MID})
}

// ---------------------------------------------------------------------------
// 共用

// finishCookieLogin 校验一段 cookie，通过就落盘。四条通道都走这里。
//
// source 是给人看的来源描述（如「粘贴的文本」「浏览器 Chrome · Default」），
// 只出现在提示里，不参与鉴权。
func (s *Server) finishCookieLogin(w http.ResponseWriter, r *http.Request, ck, source string) {
	acct, err := s.verifyAndStore(r.Context(), ck, true)
	if err != nil {
		s.logf("✗ %s：Cookie 校验出错：%v", source, err)
		writeErr(w, http.StatusBadRequest, "校验失败：%v", err)
		return
	}
	if !acct.LoggedIn {
		s.logf("✗ %s：Cookie 已失效：%s", source, acct.Message)
		// 用「：」而不是「里的」连接 —— source 既有"粘贴的文本"这类短语，
		// 也有"浏览器 Chrome · Default"这类带空格的组合，只有冒号两边都通顺。
		writeErr(w, http.StatusBadRequest,
			"%s：Cookie 已经失效了（%s）。请重新登录一次再导入。", source, acct.Message)
		return
	}
	s.logf("✓ 从 %s 登录成功：%s", source, acct.UName)
	writeJSON(w, map[string]any{"ok": true, "uname": acct.UName, "mid": acct.MID, "source": source})
}

// fileBase 只取文件名，避免把用户的完整路径写进日志。
func fileBase(path string) string {
	path = strings.TrimRight(path, `/\`)
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}
