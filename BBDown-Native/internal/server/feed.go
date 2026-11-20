package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bbdown-native/internal/bilibili"
)

// withFeedTimeout 给刷视频接口一个独立超时（取列表/取流要打几个 B 站接口）。
func withFeedTimeout(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 90*time.Second)
}

func hasLogin(cookie string) bool { return bilibili.HasAuthCookie(cookie) }

// ---------------- 刷视频：列表 ----------------

// apiFeed 返回一页「刷视频」卡片。source=popular 免登录，rcmd 需登录。
func (s *Server) apiFeed(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source == "" {
		source = "popular"
	}
	if source != "popular" && source != "rcmd" {
		writeErr(w, http.StatusBadRequest, "未知的视频来源：%s", source)
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))

	ctx, cancel := withFeedTimeout(r)
	defer cancel()

	c := bilibili.NewClient()
	c.Cookie = s.cfg.Cookie

	var items []bilibili.FeedItem
	var err error
	if source == "rcmd" {
		if !hasLogin(s.cfg.Cookie) {
			writeErr(w, http.StatusUnauthorized, "个性化推荐需要先登录（切到「热门」可免登录刷）")
			return
		}
		items, err = c.Recommended(ctx, page)
	} else {
		items, err = c.Popular(ctx, page)
	}
	if err != nil {
		s.logf("刷视频列表失败（%s）：%v", source, err)
		writeErr(w, http.StatusBadGateway, "取视频列表失败：%v", err)
		return
	}
	writeJSON(w, map[string]any{"source": source, "items": items})
}

// ---------------- 刷视频：预览取流 ----------------

// previewDTO 是一支视频可预览的两条 DASH 流（已被 token 化，前端看不到 CDN 地址）。
type previewDTO struct {
	Bvid      string    `json:"bvid"`
	Title     string    `json:"title"`
	Cover     string    `json:"cover"`
	Owner     string    `json:"owner"`
	Duration  int       `json:"duration"`
	Quality   int       `json:"quality"`
	QualityNm string    `json:"quality_name"`
	Video     streamDTO `json:"video"`
	Audio     streamDTO `json:"audio"`
}

type streamDTO struct {
	Token  string `json:"token"`
	Mime   string `json:"mime"`
	Codecs string `json:"codecs"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Frame  string `json:"frame"`
}

// apiPreview 解析一支视频并挑当前账号能拿到的最高画质，返回视频/音频两条流的
// 签名 token。前端拿 token 走 /api/stream 播，CDN 地址不外泄。
func (s *Server) apiPreview(w http.ResponseWriter, r *http.Request) {
	bvid := strings.TrimSpace(r.URL.Query().Get("bvid"))
	if bvid == "" {
		writeErr(w, http.StatusBadRequest, "缺少 bvid")
		return
	}
	pg, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if pg < 1 {
		pg = 1
	}

	ctx, cancel := withFeedTimeout(r)
	defer cancel()

	c := bilibili.NewClient()
	c.Cookie = s.cfg.Cookie
	targets := bilibili.ExtractTargets(bvid)
	resolved := c.ResolveTargets(ctx, targets)
	if len(resolved) == 0 {
		writeErr(w, http.StatusBadRequest, "无法识别视频地址")
		return
	}
	info, err := c.VideoInfo(ctx, resolved[0])
	if err != nil {
		writeErr(w, http.StatusBadGateway, "取视频信息失败：%v", err)
		return
	}
	idx := pg - 1
	if idx >= len(info.Pages) {
		idx = 0
	}
	page := info.Pages[idx]

	pu, err := c.PlayURL(ctx, info, page)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "取流失败：%v", err)
		return
	}
	// quality/codecs 都留空 → SelectVideo 自动挑「可用流里画质最高、AVC 优先」。
	v, err := pu.SelectVideo(nil, nil)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "挑流失败：%v", err)
		return
	}
	var a bilibili.Stream
	if len(pu.Audio) > 0 {
		a, _ = pu.SelectAudio()
	}

	key := s.streamSecret()
	vt, err := signStreamToken(key, v.URLs(), c.Cookie)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "生成视频流令牌失败：%v", err)
		return
	}
	at := ""
	if a.BaseURL != "" {
		at, err = signStreamToken(key, a.URLs(), c.Cookie)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "生成音频流令牌失败：%v", err)
			return
		}
	}

	writeJSON(w, previewDTO{
		Bvid: info.Bvid, Title: info.Title, Cover: info.Cover,
		Owner: info.Owner, Duration: page.Duration,
		Quality: v.ID, QualityNm: bilibili.QualityName(v.ID),
		Video: streamDTO{Token: vt, Mime: v.MimeType, Codecs: v.Codecs, Width: v.Width, Height: v.Height, Frame: v.FrameRate},
		Audio: streamDTO{Token: at, Mime: a.MimeType, Codecs: a.Codecs},
	})
}

// ---------------- 刷视频：流中转 ----------------

type streamToken struct {
	URLs      []string `json:"u"`
	Cookie    string   `json:"c"`
	ExpiresAt int64    `json:"e"`
}

// streamSecret 派生一个只在本进程有效的 HMAC 密钥。token 跨进程无效，
// 避免有人把 token 抄出去长期当 B 站CDN 的免鉴权代理用。
func (s *Server) streamSecret() []byte {
	mac := hmac.New(sha256.New, []byte("bbdown-native-stream"))
	mac.Write([]byte(s.url)) // 端口每次启动可能变，天然进程隔离
	return mac.Sum(nil)
}

func signStreamToken(key []byte, urls []string, cookie string) (string, error) {
	if len(urls) == 0 {
		return "", fmt.Errorf("没有可用流地址")
	}
	t := streamToken{URLs: urls, Cookie: cookie, ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}
	body, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(b64))
	sig := hex.EncodeToString(mac.Sum(nil))
	return b64 + "." + sig, nil
}

func verifyStreamToken(key []byte, token string) (*streamToken, error) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return nil, fmt.Errorf("令牌格式不对")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(parts[0]))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(parts[1]), []byte(want)) {
		return nil, fmt.Errorf("令牌校验失败")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var t streamToken
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	if time.Now().Unix() > t.ExpiresAt {
		return nil, fmt.Errorf("令牌已过期，请刷新页面")
	}
	// 防 SSRF：token 是服务端签的（前端改不了），所以只需挡住「内网地址」
	// 这种明显越界的。B 站 CDN 会分发到 various 合作方域名（mcdn.bilivideo.cn、
	// edge.mountaintoys.cn 等），硬编码域名白名单会误杀，不能用。
	for _, raw := range t.URLs {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" {
			return nil, fmt.Errorf("流地址不合法")
		}
		if isPrivateHost(u.Hostname()) {
			return nil, fmt.Errorf("流地址指向内网")
		}
	}
	if len(t.URLs) == 0 {
		return nil, fmt.Errorf("令牌里没有流地址")
	}
	return &t, nil
}

// isPrivateHost 挡住内网/回环/链路本地地址，防 SSRF。
// 注意别用 net.ParseIP 判断「域名」——CDN 域名解析出的 IP 可能是内网的
// （运营商 CDN 常见），但我们只做字面量判断：域名交给 CDN 自己解析。
func isPrivateHost(host string) bool {
	h := strings.ToLower(strings.Trim(host, "[]"))
	if h == "localhost" || h == "::1" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false // 是域名，不是 IP 字面量
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

// apiStream 把 CDN 流反代给浏览器 <video>。带 B 站防盗链头，透传 Range，
// 遇到主地址失败自动换备用地址。GET/HEAD 都支持。
func (s *Server) apiStream(w http.ResponseWriter, r *http.Request) {
	tok, err := verifyStreamToken(s.streamSecret(), r.URL.Query().Get("token"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "流令牌无效：%v", err)
		return
	}

	client := bilibili.NewClient()
	var lastErr error
	for _, raw := range tok.URLs {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, raw, nil)
		if err != nil {
			lastErr = err
			continue
		}
		client.SetHeaders(req)
		if tok.Cookie != "" {
			req.Header.Set("Cookie", tok.Cookie)
		}
		// 透传 Range —— 浏览器拖动进度条全靠它。
		if rg := r.Header.Get("Range"); rg != "" {
			req.Header.Set("Range", rg)
		}
		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			req.Header.Set("If-Modified-Since", ims)
		}
		resp, err := client.HTTP().Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("CDN 返回 HTTP %d", resp.StatusCode)
			resp.Body.Close()
			continue
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.WriteHeader(resp.StatusCode)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, resp.Body)
		}
		return
	}
	writeErr(w, http.StatusBadGateway, "取流失败：%v", lastErr)
}

// ---------------- 一键下载 ----------------

type quickDownloadReq struct {
	Bvid string `json:"bvid"`
	Page int    `json:"page"`
}

// apiQuickDownload 从刷视频页直接发起下载：复用 /api/download 全套状态机，
// 只是先内部把 bvid 解析成 item 塞进 s.items，再走同一个 runDownload。
func (s *Server) apiQuickDownload(w http.ResponseWriter, r *http.Request) {
	var in quickDownloadReq
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}
	in.Bvid = strings.TrimSpace(in.Bvid)
	if in.Bvid == "" {
		writeErr(w, http.StatusBadRequest, "缺少 bvid")
		return
	}

	s.mu.Lock()
	if s.task.running {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "已经有任务在跑，先等它结束")
		return
	}
	s.mu.Unlock()

	ctx, cancel := withFeedTimeout(r)
	defer cancel()
	items, err := s.runner.Resolve(ctx, in.Bvid)
	if err != nil {
		s.logf("一键下载解析失败：%v", err)
		s.handleErr(w, err)
		return
	}
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, "无法识别视频地址")
		return
	}
	spec := ""
	if in.Page > 1 {
		spec = strconv.Itoa(in.Page)
	}
	s.mu.Lock()
	s.items = items
	s.task.resetForRun()
	s.mu.Unlock()
	s.onStage("解析")
	go s.runDownload(items, downloadReq{Specs: []string{spec}})
	writeJSON(w, map[string]any{"ok": true, "title": items[0].DisplayName()})
}
