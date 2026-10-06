package bilibili

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// keyCookies 是决定鉴权的字段，拼接 cookie 字符串时排在最前面。
var keyCookies = []string{"SESSDATA", "DedeUserID", "bili_jct", "DedeUserID__ckMd5", "sid"}

// NormalizeCookie 宽容地清洗用户粘贴的内容：
// 允许带 `Cookie:` 前缀、换行、制表符，同名项保留最后一个。
func NormalizeCookie(text string) string {
	s := strings.TrimSpace(text)
	if s == "" {
		return ""
	}
	// 换行与制表符都当分隔符。cookie 的值不允许含这些字符（RFC 6265 的
	// cookie-octet 不含它们），所以从表格/多行文本里粘出来的内容也能正确切开。
	s = strings.NewReplacer("\r", ";", "\n", ";", "\t", ";").Replace(s)
	for _, prefix := range []string{"cookie:", "Cookie:", "COOKIE:"} {
		if strings.HasPrefix(s, prefix) {
			s = s[len(prefix):]
			break
		}
	}

	var order []string
	seen := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		i := strings.IndexByte(part, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(part[:i])
		if k == "" {
			continue
		}
		if _, ok := seen[k]; !ok {
			order = append(order, k)
		}
		seen[k] = k + "=" + strings.TrimSpace(part[i+1:])
	}

	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, seen[k])
	}
	return strings.Join(parts, "; ")
}

// CookieToMap 把 cookie 字符串拆成键值对。
func CookieToMap(cookie string) map[string]string {
	m := map[string]string{}
	for _, part := range strings.Split(cookie, ";") {
		i := strings.IndexByte(part, '=')
		if i <= 0 {
			continue
		}
		k := strings.TrimSpace(part[:i])
		if k != "" {
			m[k] = strings.TrimSpace(part[i+1:])
		}
	}
	return m
}

// HasAuthCookie 判断 cookie 里是否含 SESSDATA —— 没有它一律视为未登录。
func HasAuthCookie(cookie string) bool {
	return CookieToMap(cookie)["SESSDATA"] != ""
}

// joinCookies 把键值对拼成 cookie 字符串：关键字段在前，其余按字典序，
// 这样落盘后的字符串是稳定的，便于比对"这次登录是否换了账号"。
func joinCookies(vals map[string]string) string {
	key := make([]string, 0, len(keyCookies))
	inKey := map[string]bool{}
	for _, k := range keyCookies {
		if _, ok := vals[k]; ok {
			key = append(key, k)
			inKey[k] = true
		}
	}
	rest := make([]string, 0, len(vals))
	for k := range vals {
		if !inKey[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)

	order := append(key, rest...)
	parts := make([]string, 0, len(order))
	for _, k := range order {
		if v := vals[k]; v != "" {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, "; ")
}

// ---------------------------------------------------------------------------
// 带记录的 cookie 容器

// recJar 在标准 cookiejar 之外，额外记录「最后一次见到的值」。
//
// 为什么要自己记：net/http/cookiejar 没有「列出全部 cookie」的公开接口，
// 而登录成功后必须把 SESSDATA / bili_jct 取出来落盘。只靠 jar.Cookies(url)
// 会因为 path / domain 不完全匹配而漏字段。
type recJar struct {
	real http.CookieJar

	mu   sync.Mutex
	vals map[string]string
}

func newRecJar() *recJar {
	jar, _ := cookiejar.New(nil)
	return &recJar{real: jar, vals: map[string]string{}}
}

func (j *recJar) SetCookies(u *url.URL, cs []*http.Cookie) {
	if isBilibiliHost(u.Hostname()) {
		j.mu.Lock()
		for _, c := range cs {
			if c.Value != "" {
				j.vals[c.Name] = c.Value
			}
		}
		j.mu.Unlock()
	}
	j.real.SetCookies(u, cs)
}

func (j *recJar) Cookies(u *url.URL) []*http.Cookie { return j.real.Cookies(u) }

// String 返回当前收集到的 cookie 字符串。
func (j *recJar) String() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := make(map[string]string, len(j.vals))
	for k, v := range j.vals {
		cp[k] = v
	}
	return joinCookies(cp)
}

func isBilibiliHost(host string) bool {
	return strings.HasSuffix(host, "bilibili.com") || strings.HasSuffix(host, "hdslb.com")
}

// ---------------------------------------------------------------------------
// 账号校验

// Account 是校验通过的账号信息。
type Account struct {
	LoggedIn bool
	UName    string
	MID      int64
	Message  string
}

type navResp struct {
	IsLogin bool   `json:"isLogin"`
	UName   string `json:"uname"`
	MID     int64  `json:"mid"`
}

// Verify 用给定的 cookie 查询账号信息，判断它到底通不通。
//
// 注意与 wbiKeys 的区别：wbiKeys 要的是未登录也能拿到的密钥，
// 所以必须容忍 code=-101；而这里恰恰要把 -101 当成"cookie 失效"这个答案。
func (c *Client) Verify(ctx context.Context, cookie string) (*Account, error) {
	saved := c.Cookie
	c.Cookie = NormalizeCookie(cookie)
	defer func() { c.Cookie = saved }()

	var nav navResp
	err := c.get(ctx, "/x/web-interface/nav", nil, &nav)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && (ae.Code == CodeNotLoggedIn || ae.Code == CodeRiskControl) {
			return &Account{Message: "cookie 无效或已过期"}, nil
		}
		return nil, err
	}
	if !nav.IsLogin {
		return &Account{Message: "cookie 未登录"}, nil
	}
	return &Account{LoggedIn: true, UName: nav.UName, MID: nav.MID}, nil
}

// MIDString 便于直接填进界面。
func (a *Account) MIDString() string {
	if a == nil || a.MID == 0 {
		return ""
	}
	return strconv.FormatInt(a.MID, 10)
}
