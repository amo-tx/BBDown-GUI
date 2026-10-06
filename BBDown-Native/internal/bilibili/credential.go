package bilibili

import (
	"encoding/json"
	"regexp"
	"strings"
)

// Credential 是从外部文本里抽出来的凭证。
//
// 两个字段都可能为空：只有网页端 cookie（BBDown.data）、只有电视端
// access_token（BBDownTV.data）、或者两者都有（从别的工具导出的 JSON）。
type Credential struct {
	Cookie string `json:"cookie"`
	Token  string `json:"access_token"`
}

// Empty 判断这份凭证是不是什么都没抽到。
func (c Credential) Empty() bool { return c.Cookie == "" && c.Token == "" }

// Usable 判断凭证能不能拿去登录 —— 有 SESSDATA 或者有 access_token 之一即可。
func (c Credential) Usable() bool { return HasAuthCookie(c.Cookie) || c.Token != "" }

// ---------------------------------------------------------------------------
// 解析

// cookieFieldRe 匹配 `字段=值` 形式的 cookie 片段。
//
// 值的字符类刻意**不排除逗号**：SESSDATA 的实际形态是
// `xxxx%2C<时间戳>%2C<hash>*<校验>`，里面本来就有逗号与星号，
// 按逗号截断会得到一个看似正常、实则永远过不了鉴权的值。
// 只排除真正的分隔符（分号、引号、空白、&）。
var cookieFieldRe = regexp.MustCompile(
	`(?i)\b(SESSDATA|DedeUserID__ckMd5|DedeUserID|bili_jct|sid|buvid3|buvid4|b_nut)\s*=\s*([^;"'\s&]+)`)

// tokenFieldRe 匹配形如 `access_token=xxx` / `"access_token":"xxx"` 的片段。
var tokenFieldRe = regexp.MustCompile(
	`(?i)(?:["']?access[_-]?token["']?|["']?token["']?)\s*[:=]\s*["']?([A-Za-z0-9._~+/=-]{20,})`)

// bareTokenRe 判断整段文本是不是一个裸 token。
//
// 有些版本的 BBDown 往 BBDownTV.data 里直接写一行 access_token，没有任何键名。
var bareTokenRe = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]{40,}$`)

// ParseCredentialText 从一段任意文本里尽力抽出 cookie 与 access_token。
//
// 为什么这么宽容：BBDown 自己的落盘格式随版本变过（单行 cookie、token 与
// cookie 各占一行、带 `access_token=` 前缀……），用户手上还可能留着别的工具
// 导出的 JSON。与其猜格式，不如把认识的字段直接捞出来 —— 捞错了也无所谓，
// 调用方一定会拿它去 /x/web-interface/nav 验一次，验不过就会明确报错。
func ParseCredentialText(text string) Credential {
	raw := strings.TrimSpace(stripBOM(text))
	if raw == "" {
		return Credential{}
	}

	// JSON 优先：{"cookie":"SESSDATA=...","access_token":"..."} 这类形态
	// 直接交给 NormalizeCookie 会被整段当成一个巨大的字段名，必须先识别。
	if strings.HasPrefix(raw, "{") {
		if c, ok := parseCredentialJSON(raw); ok {
			return c
		}
	}

	var c Credential
	c.Token = extractToken(raw)
	c.Cookie = extractCookie(raw)
	return c
}

// extractCookie 从一段文本里抽出可用的 cookie。
//
// 两步走，缺一不可：
//
//  1. scavengeCookie —— 按认得的字段名精确捞。它对「从开发者工具里粘出来的
//     查询串」（`SESSDATA=x&bili_jct=y`）也是对的。
//  2. NormalizeCookie —— 整段按分号归一化，能把第 1 步不认识的字段
//     （buvid3、i-wanna-go-back 之类）一并收进来。
//
// 真正拿到 SESSDATA 的那一边说了算：整段归一化只认分号，碰到 `&` 会把
// 后面所有字段都塞进 SESSDATA 的值里，得到一个看似正常、实则永远过不了
// 鉴权的串。所以第 1 步优先。
//
// 两边都没捞到 SESSDATA 就直接返回空 —— 没有 SESSDATA 的 cookie 本来就
// 登不了录，返回一堆凑出来的垃圾字段只会让上层的报错更难懂。
func extractCookie(raw string) string {
	scav := scavengeCookie(raw)
	whole := NormalizeCookie(raw)

	primary, secondary := scav, whole
	if !HasAuthCookie(scav) {
		primary, secondary = whole, scav
	}
	if !HasAuthCookie(primary) {
		return ""
	}

	merged := map[string]string{}
	for k, v := range CookieToMap(primary) {
		merged[k] = v
	}
	for k, v := range CookieToMap(secondary) {
		if _, ok := merged[k]; !ok {
			merged[k] = v
		}
	}
	return joinCookies(merged)
}

// parseCredentialJSON 解析 JSON 形态的凭证。ok=false 表示这段不是能认的 JSON。
func parseCredentialJSON(raw string) (Credential, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return Credential{}, false
	}

	var c Credential
	// 先把对象里所有「像 cookie 的键值对」收集起来，再顺手接上一个叫
	// cookie 的整串字段 —— 两种排布都很常见。
	vals := map[string]string{}
	for k, v := range obj {
		s, ok := v.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		switch {
		case strings.EqualFold(k, "SESSDATA"),
			strings.EqualFold(k, "bili_jct"),
			strings.EqualFold(k, "DedeUserID"),
			strings.EqualFold(k, "DedeUserID__ckMd5"),
			strings.EqualFold(k, "sid"):
			vals[k] = s
		case strings.EqualFold(k, "cookie"), strings.EqualFold(k, "Cookie"):
			c.Cookie = NormalizeCookie(s)
		case strings.EqualFold(k, "access_token"), strings.EqualFold(k, "accessToken"),
			strings.EqualFold(k, "token"):
			c.Token = s
		}
	}
	if c.Cookie == "" && len(vals) > 0 {
		c.Cookie = joinCookies(vals)
	}
	if c.Cookie == "" && c.Token == "" {
		return Credential{}, false
	}
	return c, true
}

// extractToken 找一个 access_token。
func extractToken(raw string) string {
	if m := tokenFieldRe.FindStringSubmatch(raw); m != nil {
		return m[1]
	}
	// 裸 token：整段就是一个长串，没有任何分隔符。
	trimmed := strings.TrimSpace(raw)
	if !strings.ContainsAny(trimmed, ";\"' \t\r\n=") && bareTokenRe.MatchString(trimmed) {
		return trimmed
	}
	return ""
}

// scavengeCookie 从混杂文本里把认识的 cookie 字段捡出来。
func scavengeCookie(raw string) string {
	vals := map[string]string{}
	for _, m := range cookieFieldRe.FindAllStringSubmatch(raw, -1) {
		name, val := m[1], m[2]
		// 同名取最后一次出现：拼接的文本里后面的通常是生效的那份。
		vals[canonicalCookieName(name)] = val
	}
	if len(vals) == 0 {
		return ""
	}
	return joinCookies(vals)
}

// canonicalCookieName 把字段名收敛成官方大小写。
//
// 正则用了 (?i)，捞出来的键名大小写是文本里的原样；而 B 站的鉴权
// 只认精确名字，大小写错了就白搭。
var cookieNameCanon = map[string]string{
	"sessdata":          "SESSDATA",
	"bili_jct":          "bili_jct",
	"dedeuserid":        "DedeUserID",
	"dedeuserid__ckmd5": "DedeUserID__ckMd5",
	"sid":               "sid",
	"buvid3":            "buvid3",
	"buvid4":            "buvid4",
	"b_nut":             "b_nut",
}

func canonicalCookieName(name string) string {
	if c, ok := cookieNameCanon[strings.ToLower(name)]; ok {
		return c
	}
	return name
}

// stripBOM 去掉 UTF-8 BOM。
//
// Windows 上记事本「另存为」默认就带 BOM，用户手工存过的凭证文件里
// 非常容易出现它，留着会让第一个字段名变成 "\ufeffSESSDATA"。
func stripBOM(s string) string {
	return strings.TrimPrefix(s, "\ufeff")
}
