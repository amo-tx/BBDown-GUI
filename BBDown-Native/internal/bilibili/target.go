package bilibili

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// Kind 表示目标类型。
type Kind int

const (
	KindVideo   Kind = iota // 普通投稿（BV / av）
	KindBangumi             // 番剧/剧集（ep / ss）
)

func (k Kind) String() string {
	if k == KindBangumi {
		return "番剧"
	}
	return "视频"
}

// Target 是一个已识别的下载目标。
type Target struct {
	// Raw 是用户原始输入里出现的那一段（便于在界面上回显）。
	Raw  string
	Kind Kind
	// ID 是规范化后的编号，如 BV1xx411c7mD / av170001 / ep123456。
	ID string
	// Page 来自链接里的 ?p=，0 表示未指定。
	Page int
	// Short 表示这是一个 b23.tv 短链，ID 需要解析后才能确定。
	Short bool
}

// Display 返回给界面展示的一行文本。
func (t Target) Display() string {
	if t.Short {
		return t.Raw + "（短链，解析后确定）"
	}
	if t.ID == "" {
		return t.Raw
	}
	if t.Page > 0 {
		return fmt.Sprintf("%s（第 %d P）", t.ID, t.Page)
	}
	return t.ID
}

// ---------------------------------------------------------------------------
// 文本 → 目标列表

// 链接里不允许出现的字符。
//
// 必须把中文/全角标点、emoji、引号括号显式列出来——它们不在 \u4e00-\u9fff（汉字）
// 区间内，只排除汉字的话「，。」「🤔」会被吃进链接尾部，后面的 BV 号就解析不出来了。
const nonURLChars = `\s\x{00a0}\x{2000}-\x{206f}\x{2190}-\x{21ff}\x{2600}-\x{27bf}` +
	`\x{2e80}-\x{2eff}\x{3000}-\x{303f}\x{fe00}-\x{fe0f}\x{fe30}-\x{fe4f}\x{ff00}-\x{ffef}` +
	`\x{1f000}-\x{1faff}<>"'` + "`" + `\[\]{}|\\^`

var (
	// 主匹配：URL | BV 号 | av/ep/ss 号
	// 分组编号：1=url  2=BV  3=av/ep/ss
	targetRe = regexp.MustCompile(`(?i)(https?://[^` + nonURLChars + `]+)|(BV[0-9A-Za-z]{10})|((?:av|ep|ss)[0-9]{1,20})`)

	// 无协议写法：www.bilibili.com/video/BVxxx
	bareHostRe = regexp.MustCompile(`(?i)(?:^|[^0-9A-Za-z:/.])((?:www\.|m\.)?(?:bilibili\.com|b23\.tv|bili2233\.cn|bilibili\.tv)/)`)
)

// 允许的域名后缀。只认 bilibili 系，避免把文案里夹着的其它网站链接误当地址。
var allowedHostSuffix = []string{"bilibili.com", "b23.tv", "bili2233.cn", "bilibili.tv"}

// 链接尾部需要剥掉的标点（正则可能把句读一起带进来）。
const trimTail = ".,;:!?、。，；：！？…·\"'"

// 需要剥掉的右括号（成对包裹链接时）。
const trimTailBrackets = ")]}）｝〕》」』＞"

// ExtractTargets 从任意文本里提取 B 站地址。
//
// 支持：带标题的分享文案、Markdown 链接、一段话里多个地址、裸 BV/av/ep/ss 号。
// 结果按出现顺序去重。
func ExtractTargets(text string) []Target {
	text = addSchemeToBareHosts(text)

	var out []Target
	seen := map[string]bool{}

	for _, loc := range targetRe.FindAllStringSubmatchIndex(text, -1) {
		start, end := loc[0], loc[1]
		var t Target

		switch {
		case loc[2] >= 0: // 完整链接
			raw := strings.TrimRight(text[loc[2]:loc[3]], trimTail)
			raw = strings.TrimRight(raw, trimTailBrackets)
			u, err := url.Parse(raw)
			if err != nil || u.Hostname() == "" {
				continue
			}
			if !hostAllowed(u.Hostname()) {
				continue
			}
			t = targetFromURL(raw)

		case loc[4] >= 0: // BV 号
			if !boundaryOK(text, start, end) {
				continue
			}
			t = targetFromID(text[loc[4]:loc[5]])
			t.Raw = text[loc[4]:loc[5]]

		case loc[6] >= 0: // av / ep / ss 号
			if !boundaryOK(text, start, end) {
				continue
			}
			t = targetFromID(text[loc[6]:loc[7]])
			t.Raw = text[loc[6]:loc[7]]
		}

		if t.ID == "" && !t.Short {
			continue
		}
		key := dedupKey(t)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, t)
	}
	return out
}

// addSchemeToBareHosts 给没写协议的 B 站域名补上 https://。
//
// 补了方案之后，链接里的 ?p= 才能被完整保留（只靠 BV 号匹配会丢掉分P信息）。
func addSchemeToBareHosts(text string) string {
	return bareHostRe.ReplaceAllStringFunc(text, func(m string) string {
		// m 形如 "<前导字符>www.bilibili.com/"。前导字符一定是非字母数字
		// （正则里已经排除了 : / . 和字母数字），所以第一个字母就是域名起点。
		i := strings.IndexFunc(m, func(r rune) bool {
			return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'
		})
		if i < 0 {
			return m
		}
		return m[:i] + "https://" + m[i:]
	})
}

// boundaryOK 检查匹配两侧不是字母数字，避免把长串截断误判。
func boundaryOK(text string, start, end int) bool {
	if start > 0 && isAlnum(text[start-1]) {
		return false
	}
	if end < len(text) && isAlnum(text[end]) {
		return false
	}
	return true
}

func isAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func hostAllowed(host string) bool {
	host = strings.ToLower(host)
	for _, suf := range allowedHostSuffix {
		if host == suf || strings.HasSuffix(host, "."+suf) {
			return true
		}
	}
	return false
}

// targetFromURL 从完整链接里解析出目标。链接原样保留在 Raw 里。
func targetFromURL(raw string) Target {
	u, err := url.Parse(raw)
	if err != nil {
		return Target{Raw: raw}
	}
	host := strings.ToLower(u.Hostname())
	if host == "b23.tv" || strings.HasSuffix(host, ".b23.tv") || host == "bili2233.cn" {
		return Target{Raw: raw, Short: true}
	}

	page := 0
	if p := u.Query().Get("p"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}

	seg := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, s := range seg {
		low := strings.ToLower(s)
		if low == "video" || low == "bangumi" || low == "play" {
			if i+1 < len(seg) {
				if t := targetFromID(seg[i+1]); t.ID != "" {
					t.Raw = raw
					t.Page = page
					return t
				}
			}
			continue
		}
		// 兜底：路径里任意一段本身就是编号（如 /BV1xx411c7mD）
		if t := targetFromID(s); t.ID != "" {
			t.Raw = raw
			t.Page = page
			return t
		}
	}
	return Target{Raw: raw}
}

var (
	bvRe = regexp.MustCompile(`(?i)^BV([0-9A-Za-z]{10})$`)
	avRe = regexp.MustCompile(`(?i)^av([0-9]{1,20})$`)
	epRe = regexp.MustCompile(`(?i)^ep([0-9]{1,20})$`)
	ssRe = regexp.MustCompile(`(?i)^ss([0-9]{1,20})$`)
)

// targetFromID 把裸编号规范化成 Target。
//
// BV 号只有前缀大小写可归一（正文大小写敏感，不能动），数字编号去掉前导零。
func targetFromID(id string) Target {
	id = strings.TrimSpace(id)
	switch {
	case bvRe.MatchString(id):
		m := bvRe.FindStringSubmatch(id)
		return Target{Kind: KindVideo, ID: "BV" + m[1]}
	case avRe.MatchString(id):
		m := avRe.FindStringSubmatch(id)
		return Target{Kind: KindVideo, ID: "av" + stripLeadingZeros(m[1])}
	case epRe.MatchString(id):
		m := epRe.FindStringSubmatch(id)
		return Target{Kind: KindBangumi, ID: "ep" + stripLeadingZeros(m[1])}
	case ssRe.MatchString(id):
		m := ssRe.FindStringSubmatch(id)
		return Target{Kind: KindBangumi, ID: "ss" + stripLeadingZeros(m[1])}
	}
	return Target{}
}

func stripLeadingZeros(s string) string {
	s = strings.TrimLeft(s, "0")
	if s == "" {
		return "0"
	}
	return s
}

// dedupKey 生成去重键。
func dedupKey(t Target) string {
	if t.Short {
		return "short:" + t.Raw
	}
	prefix := t.ID
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	return strings.ToUpper(prefix) + ":" + t.ID
}

// ResolveShortLink 跟随 b23.tv 短链跳转，返回最终链接。
func (c *Client) ResolveShortLink(ctx context.Context, short string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, short, nil)
	if err != nil {
		return "", err
	}
	c.SetHeaders(req)
	// 不自动跟随，手动读 Location，避免被跳到无关页面。
	hc := &http.Client{
		Timeout: c.hc.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "" {
		return loc, nil
	}
	return short, nil
}

// ResolveTargets 把列表里的短链就地展开成真实目标。
// 展开失败的原样保留（后续会以 Short 状态报错，而不是静默丢弃）。
func (c *Client) ResolveTargets(ctx context.Context, targets []Target) []Target {
	out := make([]Target, 0, len(targets))
	for _, t := range targets {
		if !t.Short {
			out = append(out, t)
			continue
		}
		final, err := c.ResolveShortLink(ctx, t.Raw)
		if err != nil {
			out = append(out, t)
			continue
		}
		nt := targetFromURL(final)
		if nt.ID == "" {
			out = append(out, t)
			continue
		}
		nt.Raw = t.Raw
		out = append(out, nt)
	}
	return out
}
