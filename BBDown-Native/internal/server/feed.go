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

// streamClient 是拉流专用客户端。整条 DASH 流可能持续几分钟，不能带
// http.Client.Timeout（45 秒后连接会被掐断）；也不该每个请求 new 一个
// Client，那等于每次都重新做 TLS 握手，首帧会白等 100~300ms。
var (
	streamClient = bilibili.NewClient()
	streamHTTP   = streamClient.StreamHTTP()
)

// feedCacheTTL：热门榜是公共数据，一分钟内不会变。缓存一下，反复打开
// 刷视频面板/切回来时能立刻出列表；个性化推荐不缓存（要新鲜）。
const feedCacheTTL = 60 * time.Second

type feedCacheEntry struct {
	items []bilibili.FeedItem
	at    time.Time
}

func (s *Server) cachedFeed(source string, page int) ([]bilibili.FeedItem, bool) {
	if source != "popular" {
		return nil, false
	}
	s.feedMu.Lock()
	defer s.feedMu.Unlock()
	e, ok := s.feedCache[source+":"+strconv.Itoa(page)]
	if !ok || time.Since(e.at) > feedCacheTTL {
		return nil, false
	}
	return e.items, true
}

func (s *Server) cacheFeed(source string, page int, items []bilibili.FeedItem) {
	if source != "popular" || len(items) == 0 {
		return
	}
	s.feedMu.Lock()
	defer s.feedMu.Unlock()
	if s.feedCache == nil {
		s.feedCache = make(map[string]feedCacheEntry)
	}
	s.feedCache[source+":"+strconv.Itoa(page)] = feedCacheEntry{items: items, at: time.Now()}
}

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

	// 热门榜有缓存：反复切回刷视频面板不必再等一次 B 站接口。
	if items, ok := s.cachedFeed(source, page); ok {
		writeJSON(w, map[string]any{"source": source, "items": items, "cached": true})
		return
	}

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
	s.cacheFeed(source, page, items)
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

// previewCall 是一次预览解析（含并发合并与短缓存）。
type previewCall struct {
	done     chan struct{} // 解析完成时关闭，等待方靠它同步
	dto      previewDTO
	err      error
	code     int // 失败时的 HTTP 状态码
	at       time.Time
	finished bool
}

// previewCacheTTL：解析结果留 5 分钟。流 token 本身 2 小时有效，这里只为了
// 「切走又切回来」和「连点两下」不必重打三个 B 站接口。
const previewCacheTTL = 5 * time.Minute

// beginPreview 返回本次请求要用的调用。owner=true 表示由自己真正解析，
// 解析完必须 finishPreview 广播结果；false 表示命中缓存或已在解析中。
func (s *Server) beginPreview(key string) (*previewCall, bool) {
	s.previewMu.Lock()
	defer s.previewMu.Unlock()
	if s.previewCache == nil {
		s.previewCache = make(map[string]*previewCall)
	}
	if c, ok := s.previewCache[key]; ok {
		if !c.finished {
			return c, false // 有人正在解析，等它
		}
		if c.err == nil && time.Since(c.at) < previewCacheTTL {
			return c, false // 缓存命中
		}
	}
	// 顺手做个上限：刷得久了别把 map 撑大
	if len(s.previewCache) > 128 {
		for k, c := range s.previewCache {
			if c.finished && time.Since(c.at) > previewCacheTTL {
				delete(s.previewCache, k)
			}
		}
	}
	c := &previewCall{done: make(chan struct{})}
	s.previewCache[key] = c
	return c, true
}

func (s *Server) finishPreview(key string, c *previewCall, dto previewDTO, code int, err error) {
	s.previewMu.Lock()
	c.dto, c.err, c.code, c.at, c.finished = dto, err, code, time.Now(), true
	if err != nil {
		// 失败不缓存，下次重试
		delete(s.previewCache, key)
	}
	s.previewMu.Unlock()
	close(c.done)
}

// apiPreview 解析一支视频并挑当前账号能拿到的最高画质，返回视频/音频两条流的
// 签名 token。前端拿 token 走 /api/stream 播，CDN 地址不外泄。
//
// 解析要串 view + playurl 两个往返（WBI 签名另有全局缓存的密钥），慢的时候
// 好几秒。列表自动选中和用户点卡片经常同时发同一个 bvid，所以这里并发合并 +
// 短缓存：同一个 key 同时只有一个请求真的去打 B 站。
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
	key := strings.ToLower(bvid) + "#" + strconv.Itoa(pg)

	call, owner := s.beginPreview(key)
	if !owner {
		select {
		case <-call.done:
		case <-r.Context().Done():
			return
		}
		if call.err != nil {
			writeErr(w, call.code, "%s", call.err.Error())
			return
		}
		writeJSON(w, call.dto)
		return
	}

	ctx, cancel := withFeedTimeout(r)
	defer cancel()
	dto, code, err := s.resolvePreview(ctx, bvid, pg)
	s.finishPreview(key, call, dto, code, err)
	if err != nil {
		writeErr(w, code, "%s", err.Error())
		return
	}
	writeJSON(w, dto)
}

// resolvePreview 真正去打 B 站解析一支视频。
func (s *Server) resolvePreview(ctx context.Context, bvid string, pg int) (previewDTO, int, error) {
	c := bilibili.NewClient()
	c.Cookie = s.cfg.Cookie
	targets := bilibili.ExtractTargets(bvid)
	resolved := c.ResolveTargets(ctx, targets)
	if len(resolved) == 0 {
		return previewDTO{}, http.StatusBadRequest, fmt.Errorf("无法识别视频地址")
	}
	info, err := c.VideoInfo(ctx, resolved[0])
	if err != nil {
		return previewDTO{}, http.StatusBadGateway, fmt.Errorf("取视频信息失败：%w", err)
	}
	idx := pg - 1
	if idx >= len(info.Pages) {
		idx = 0
	}
	page := info.Pages[idx]

	pu, err := c.PlayURL(ctx, info, page)
	if err != nil {
		return previewDTO{}, http.StatusBadGateway, fmt.Errorf("取流失败：%w", err)
	}
	// quality/codecs 都留空 → SelectVideo 自动挑「可用流里画质最高、AVC 优先」。
	v, err := pu.SelectVideo(nil, nil)
	if err != nil {
		return previewDTO{}, http.StatusBadGateway, fmt.Errorf("挑流失败：%w", err)
	}
	var a bilibili.Stream
	if len(pu.Audio) > 0 {
		a, _ = pu.SelectAudio()
	}

	key := s.streamSecret()
	vt, err := signStreamToken(key, v.URLs(), c.Cookie)
	if err != nil {
		return previewDTO{}, http.StatusInternalServerError, fmt.Errorf("生成视频流令牌失败：%w", err)
	}
	at := ""
	if a.BaseURL != "" {
		at, err = signStreamToken(key, a.URLs(), c.Cookie)
		if err != nil {
			return previewDTO{}, http.StatusInternalServerError, fmt.Errorf("生成音频流令牌失败：%w", err)
		}
	}

	return previewDTO{
		Bvid: info.Bvid, Title: info.Title, Cover: info.Cover,
		Owner: info.Owner, Duration: page.Duration,
		Quality: v.ID, QualityNm: bilibili.QualityName(v.ID),
		Video: streamDTO{Token: vt, Mime: v.MimeType, Codecs: v.Codecs, Width: v.Width, Height: v.Height, Frame: v.FrameRate},
		Audio: streamDTO{Token: at, Mime: a.MimeType, Codecs: a.Codecs},
	}, 0, nil
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

	client := streamClient
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
		resp, err := streamHTTP.Do(req)
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
