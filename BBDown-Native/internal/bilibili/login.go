package bilibili

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// 登录相关接口。Web 端与电视端在不同的域名下（passport，而非 api）。
const (
	webQRGenerate = "https://passport.bilibili.com/x/passport-login/web/qrcode/generate"
	webQRPoll     = "https://passport.bilibili.com/x/passport-login/web/qrcode/poll"

	tvQRGenerate = "https://passport.bilibili.com/x/passport-tv-login/qrcode/auth_code"
	tvQRPoll     = "https://passport.bilibili.com/x/passport-tv-login/qrcode/poll"

	tvAppKey = "4409e2ce8ffd12b8"
	tvAppSec = "59b43e04ad6965f34319062b478f83dd"
)

// 扫码状态码。Web 与 TV 的「未扫码」码不同，其余一致。
const (
	qrStatusOK      = 0
	qrStatusExpired = 86038
	qrStatusScanned = 86090
	qrWebWaiting    = 86101
	qrTVWaiting     = 86039
	qrLifetime      = 180 * time.Second
	qrPollInterval  = 2 * time.Second
	maxQRRefresh    = 3
	qrImageSizePx   = 320
	passportTimeout = 15 * time.Second
)

// LoginMode 选择登录方式。
type LoginMode int

const (
	// LoginWeb 网页端扫码：cookie 有效期约 30 天，画质上限与浏览器一致。
	LoginWeb LoginMode = iota
	// LoginTV 电视端扫码：额外拿到 180 天有效的 access_token，适合长期挂机下载。
	LoginTV
)

func (m LoginMode) String() string {
	if m == LoginTV {
		return "tv"
	}
	return "web"
}

// ParseLoginMode 解析界面传进来的字符串。
func ParseLoginMode(s string) LoginMode {
	if strings.EqualFold(strings.TrimSpace(s), "tv") {
		return LoginTV
	}
	return LoginWeb
}

// LoginState 是扫码流程的状态。
type LoginState int

const (
	StateIdle LoginState = iota
	StateLoading
	StatePending
	StateScanned
	StateConfirmed
	StateExpired
	StateFailed
)

func (s LoginState) String() string {
	switch s {
	case StateLoading:
		return "loading"
	case StatePending:
		return "pending"
	case StateScanned:
		return "scanned"
	case StateConfirmed:
		return "confirmed"
	case StateExpired:
		return "expired"
	case StateFailed:
		return "error"
	default:
		return "idle"
	}
}

// LoginResult 是登录成功后的凭证。
type LoginResult struct {
	Cookie string `json:"cookie"`
	Token  string `json:"token"`
	MID    int64  `json:"mid"`
	UName  string `json:"uname"`
}

// LoginSnapshot 是给界面用的一次性快照。
type LoginSnapshot struct {
	State   LoginState   `json:"state"`
	Message string       `json:"message"`
	Mode    LoginMode    `json:"mode"`
	Seq     int          `json:"seq"`
	Remain  int          `json:"remain"`
	URL     string       `json:"url"`
	Result  *LoginResult `json:"result,omitempty"`
	Running bool         `json:"running"`
}

// LoginSession 驱动一次扫码登录。
//
// 单张二维码有效期 180 秒，失效后自动重新申请，最多 maxQRRefresh 轮。
// 全程只读写内存状态；是否落盘由调用方通过 OnSuccess 决定。
type LoginSession struct {
	mu      sync.Mutex
	mode    LoginMode
	state   LoginState
	msg     string
	seq     int
	qrURL   string
	qr      image.Image
	expire  time.Time
	result  *LoginResult
	running bool

	cancel context.CancelFunc
	onOK   func(*LoginResult)
}

// NewLoginSession 创建一个空会话。
func NewLoginSession() *LoginSession {
	return &LoginSession{state: StateIdle, msg: "尚未开始"}
}

// Snapshot 返回当前状态的副本，供界面轮询绘制。
func (s *LoginSession) Snapshot() LoginSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := LoginSnapshot{
		State:   s.state,
		Message: s.msg,
		Mode:    s.mode,
		Seq:     s.seq,
		URL:     s.qrURL,
		Running: s.running,
	}
	if !s.expire.IsZero() && s.running {
		if r := int(time.Until(s.expire).Seconds()); r > 0 {
			snap.Remain = r
		}
	}
	if s.result != nil {
		cp := *s.result
		snap.Result = &cp
	}
	return snap
}

// QRImage 返回当前二维码图片，没有则返回 nil。
//
// 返回 image.Image 而不是 PNG 字节：界面（原生 GDI）需要像素，
// 而另存为 PNG 只是其中一种用途。
func (s *LoginSession) QRImage() image.Image {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.qr
}

// Start 开始一轮登录。mode 为 web 或 tv；onSuccess 在登录成功时回调，可为 nil。
//
// 返回 false 表示已有流程在跑。
func (s *LoginSession) Start(mode LoginMode, onSuccess func(*LoginResult)) (bool, string) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return false, "登录流程正在进行中"
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.onOK = onSuccess
	s.mode = mode
	s.result = nil
	s.qr = nil
	s.qrURL = ""
	s.running = true
	s.setState(StateLoading, "正在申请登录二维码…")
	s.mu.Unlock()

	go s.run(ctx)
	return true, ""
}

// Cancel 中止当前流程。
func (s *LoginSession) Cancel() {
	s.mu.Lock()
	cancel := s.cancel
	running := s.running
	if running {
		s.setState(StateIdle, "已取消登录")
		s.running = false
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// ---------------------------------------------------------------------------
// 状态维护（调用方必须已持锁或明确是单线程）

func (s *LoginSession) setState(st LoginState, msg string) {
	s.state = st
	s.msg = msg
	s.seq++
	if st == StateConfirmed || st == StateFailed {
		s.expire = time.Time{}
	}
}

func (s *LoginSession) setQR(img image.Image, rawURL, tip string) {
	s.qr = img
	s.qrURL = rawURL
	s.expire = time.Now().Add(qrLifetime)
	if tip == "" {
		tip = "请使用「哔哩哔哩」App 扫描二维码"
	}
	s.setState(StatePending, tip)
}

func (s *LoginSession) finish(res *LoginResult) {
	s.mu.Lock()
	s.result = res
	s.running = false
	s.setState(StateConfirmed, "登录成功")
	cb := s.onOK
	s.mu.Unlock()

	if cb != nil {
		cb(res)
	}
}

func (s *LoginSession) fail(msg string) {
	s.mu.Lock()
	s.running = false
	s.setState(StateFailed, msg)
	s.mu.Unlock()
}

// active 判断流程是否仍在进行（用于 goroutine 里随时退出）。
func (s *LoginSession) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// ---------------------------------------------------------------------------
// 主流程

func (s *LoginSession) run(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			s.fail(fmt.Sprintf("登录流程内部错误: %v", r))
		}
	}()

	hc := &http.Client{
		Timeout:   passportTimeout,
		Transport: &http.Transport{ForceAttemptHTTP2: true},
		Jar:       newRecJar(),
	}
	jar := hc.Jar.(*recJar)

	for attempt := 0; attempt < maxQRRefresh; attempt++ {
		if ctx.Err() != nil || !s.active() {
			return
		}

		var (
			rawURL string
			key    string
			tip    string
			err    error
		)
		if s.mode == LoginTV {
			rawURL, key, err = tvGenerate(ctx, hc)
			tip = "用「哔哩哔哩」App 扫码，可获得 180 天有效的电视端凭证"
		} else {
			rawURL, key, err = webGenerate(ctx, hc)
			tip = "请使用「哔哩哔哩」App 扫描二维码"
		}
		if err != nil {
			if ctx.Err() != nil || !s.active() {
				return
			}
			s.fail("获取二维码失败：" + err.Error())
			return
		}

		s.mu.Lock()
		s.setQR(renderQR(rawURL), rawURL, tip)
		s.mu.Unlock()

		// poll 返回 (结果, 是否收工)。收工但结果为空 = 已经走过失败分支。
		var (
			res  *LoginResult
			stop bool
		)
		if s.mode == LoginTV {
			res, stop = s.pollTV(ctx, hc, key)
		} else {
			res, stop = s.pollWeb(ctx, hc, jar, key)
		}
		if res != nil {
			s.finish(res)
			return
		}
		if stop {
			return
		}
		if ctx.Err() != nil || !s.active() {
			return
		}
	}
	s.mu.Lock()
	if s.state == StateLoading || s.state == StatePending || s.state == StateScanned {
		s.running = false
		s.setState(StateExpired, "二维码多次失效，请重新获取")
	}
	s.mu.Unlock()
}

// pollWeb 轮询 Web 端状态。
// 返回 (结果, 是否收工)：结果非 nil 表示成功；收工但结果为空表示已走失败分支。
func (s *LoginSession) pollWeb(ctx context.Context, hc *http.Client, jar *recJar, key string) (*LoginResult, bool) {
	deadline := time.Now().Add(qrLifetime)
	for {
		if !sleepCtx(ctx, qrPollInterval) {
			return nil, true
		}
		if !s.active() || time.Now().After(deadline) {
			return nil, false
		}

		var res webPollResp
		if err := postGet(ctx, hc, webQRPoll+"?qrcode_key="+url.QueryEscape(key), nil, &res); err != nil {
			continue // 网络抖动，继续轮询
		}
		switch res.Data.Code {
		case qrStatusOK:
			cookie := jar.String()
			if !HasAuthCookie(cookie) {
				s.fail("登录成功但未取到 SESSDATA，请重试")
				return nil, true
			}
			return s.newResult(ctx, cookie, "", 0), true
		case qrStatusScanned:
			s.setStateLocked(StateScanned, "已扫码，请在手机上点击「确认登录」")
		case qrStatusExpired:
			s.setStateLocked(StatePending, "二维码已失效，正在重新获取…")
			return nil, false
		case qrWebWaiting:
			s.waiting()
		default:
			s.setStateLocked(StatePending, firstNonEmpty(res.Data.Message, res.Message, "等待扫码…"))
		}
	}
}

// pollTV 轮询电视端状态。
func (s *LoginSession) pollTV(ctx context.Context, hc *http.Client, auth string) (*LoginResult, bool) {
	deadline := time.Now().Add(qrLifetime)
	for {
		if !sleepCtx(ctx, qrPollInterval) {
			return nil, true
		}
		if !s.active() || time.Now().After(deadline) {
			return nil, false
		}

		var res tvPollResp
		if err := postGet(ctx, hc, tvQRPoll, tvSign(url.Values{"auth_code": {auth}}), &res); err != nil {
			continue
		}
		switch res.Code {
		case qrStatusOK:
			d := res.Data
			return s.newResult(ctx, joinTVCookies(d.CookieInfo.Cookies), d.AccessToken, d.MID), true
		case qrStatusScanned:
			s.setStateLocked(StateScanned, "已扫码，请在手机上点击「确认登录」")
		case qrStatusExpired:
			s.setStateLocked(StatePending, "二维码已失效，正在重新获取…")
			return nil, false
		case qrTVWaiting:
			s.waiting()
		default:
			s.setStateLocked(StatePending, firstNonEmpty(res.Message, "等待扫码…"))
		}
	}
}

// setStateLocked 加锁改状态，供轮询循环使用。
func (s *LoginSession) setStateLocked(st LoginState, msg string) {
	s.mu.Lock()
	s.setState(st, msg)
	s.mu.Unlock()
}

func (s *LoginSession) waiting() {
	s.mu.Lock()
	if s.state != StatePending {
		s.setState(StatePending, "等待扫码…")
	}
	s.mu.Unlock()
}

// newResult 组装结果，并顺手拉一次昵称（失败不影响登录）。
func (s *LoginSession) newResult(ctx context.Context, cookie, token string, mid int64) *LoginResult {
	r := &LoginResult{Cookie: NormalizeCookie(cookie), Token: token, MID: mid}

	if r.Cookie != "" {
		vctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		if acc, err := NewClient().Verify(vctx, r.Cookie); err == nil && acc != nil && acc.LoggedIn {
			r.UName = acc.UName
			if acc.MID != 0 {
				r.MID = acc.MID
			}
		}
		cancel()
	}
	return r
}

// ---------------------------------------------------------------------------
// 二维码渲染

// renderQR 把登录 URL 编码成二维码图片。
//
// 自己生成而不调用 BBDown：`BBDown login` 只在控制台画字符画，
// 实测会退化成整片实心方块，而且不返回登录状态。
func renderQR(content string) image.Image {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return nil
	}
	return q.Image(qrImageSizePx)
}

// ---------------------------------------------------------------------------
// HTTP 小工具

type passportEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type webGenerateResp struct {
	URL       string `json:"url"`
	QRCodeKey string `json:"qrcode_key"`
}

type webPollData struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	URL     string `json:"url"`
}

type webPollResp struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    webPollData `json:"data"`
}

type tvGenerateResp struct {
	URL      string `json:"url"`
	AuthCode string `json:"auth_code"`
}

// tvCookie 是电视端接口返回的一条 cookie。
//
// 提成具名类型而不是写成匿名结构体，是为了让扫码轮询（pollTV）与
// 账号密码登录（LoginWithPassword）能共用同一个响应结构与拼接函数。
type tvCookie struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type tvPollResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		AccessToken string `json:"access_token"`
		MID         int64  `json:"mid"`
		CookieInfo  struct {
			Cookies []tvCookie `json:"cookies"`
		} `json:"cookie_info"`
	} `json:"data"`
}

// joinTVCookies 把电视端返回的 cookie 列表拼成字符串。
func joinTVCookies(cs []tvCookie) string {
	if len(cs) == 0 {
		return ""
	}
	vals := map[string]string{}
	for _, c := range cs {
		if c.Name != "" {
			vals[c.Name] = c.Value
		}
	}
	return joinCookies(vals)
}

func webGenerate(ctx context.Context, hc *http.Client) (string, string, error) {
	var env passportEnvelope
	if err := postGet(ctx, hc, webQRGenerate+"?source=main-fe-header", nil, &env); err != nil {
		return "", "", err
	}
	if env.Code != 0 {
		return "", "", fmt.Errorf("%s", firstNonEmpty(env.Message, "接口拒绝"))
	}
	var d webGenerateResp
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return "", "", fmt.Errorf("二维码响应结构异常: %w", err)
	}
	if d.URL == "" || d.QRCodeKey == "" {
		return "", "", fmt.Errorf("二维码接口未返回有效内容")
	}
	return d.URL, d.QRCodeKey, nil
}

func tvGenerate(ctx context.Context, hc *http.Client) (string, string, error) {
	var env passportEnvelope
	if err := postGet(ctx, hc, tvQRGenerate, tvSign(nil), &env); err != nil {
		return "", "", err
	}
	if env.Code != 0 {
		return "", "", fmt.Errorf("%s", firstNonEmpty(env.Message, "接口拒绝"))
	}
	var d tvGenerateResp
	if err := json.Unmarshal(env.Data, &d); err != nil {
		return "", "", fmt.Errorf("二维码响应结构异常: %w", err)
	}
	if d.AuthCode == "" {
		return "", "", fmt.Errorf("电视端接口未返回 auth_code")
	}
	return d.URL, d.AuthCode, nil
}

// tvSign 按电视端规则签名：appkey + local_id + ts，字典序拼接后加盐取 md5。
func tvSign(params url.Values) url.Values {
	p := url.Values{}
	for k, v := range params {
		p[k] = append([]string(nil), v...)
	}
	p.Set("appkey", tvAppKey)
	p.Set("local_id", "0")
	p.Set("ts", strconv.FormatInt(time.Now().Unix(), 10))
	// url.Values.Encode() 按 key 排序，正好是官方要求的字典序。
	sum := md5.Sum([]byte(p.Encode() + tvAppSec))
	p.Set("sign", hex.EncodeToString(sum[:]))
	return p
}

// postGet 发请求并解析响应。form 为 nil 时用 GET，否则用 POST 表单。
func postGet(ctx context.Context, hc *http.Client, rawURL string, form url.Values, out any) error {
	var body io.Reader
	method := http.MethodGet
	if form != nil {
		method = http.MethodPost
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Referer", "https://www.bilibili.com/")
	req.Header.Set("Origin", "https://www.bilibili.com")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("响应不是合法 JSON: %w", err)
	}
	return nil
}

// sleepCtx 睡 d，期间 ctx 被取消则返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
