//go:build windows

package cookie

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Profile 是一个浏览器配置目录。
type Profile struct {
	Browser    string // "Edge" / "Chrome"
	Name       string // "Default" / "Profile 1"
	Dir        string // 配置目录
	CookiesDB  string // Cookies 文件（SQLite）
	LocalState string // Local State（含加密主密钥）
}

// Display 返回给人看的一行描述。
func (p Profile) Display() string { return p.Browser + " · " + p.Name }

// ErrNoBilibiliLogin 表示库里没有 B 站的登录态。
var ErrNoBilibiliLogin = errors.New("这个浏览器里没有 B 站的登录 Cookie")

// ErrAppBound 表示遇到了新版的「应用绑定加密」，光有 Local State 解不开。
var ErrAppBound = errors.New("该浏览器的 Cookie 用了应用绑定加密（v20），无法直接解密；请改用扫码登录")

// browserRoots 是已知的浏览器数据根目录。
//
// 只列 Chromium 系（它们共用同一套 cookie 加密方案）。Firefox 存的是明文
// SQLite，本来也能读，但它的表结构不同，等有人需要再加。
var browserRoots = []struct {
	Name string
	Rel  string // 相对 %LOCALAPPDATA%
}{
	{"Edge", `Microsoft\Edge\User Data`},
	{"Chrome", `Google\Chrome\User Data`},
}

// Discover 找出本机所有可用的浏览器配置。
//
// 没能识别的目录会被静默跳过 —— 这是个「帮你省事」的功能，
// 没找到不该让界面报错。
func Discover() []Profile {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return nil
	}

	var out []Profile
	for _, root := range browserRoots {
		dataDir := filepath.Join(base, root.Rel)
		if st, err := os.Stat(dataDir); err != nil || !st.IsDir() {
			continue
		}
		entries, err := os.ReadDir(dataDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			if name != "Default" && !strings.HasPrefix(name, "Profile ") {
				continue
			}
			dir := filepath.Join(dataDir, name)
			db, ok := findCookiesFile(dir)
			if !ok {
				continue
			}
			localState := filepath.Join(dataDir, "Local State")
			if _, err := os.Stat(localState); err != nil {
				continue
			}
			out = append(out, Profile{
				Browser:    root.Name,
				Name:       name,
				Dir:        dir,
				CookiesDB:  db,
				LocalState: localState,
			})
		}
	}
	// 让结果稳定：同一次调用两次返回的顺序要一致，界面才好展示。
	sort.Slice(out, func(i, j int) bool {
		if out[i].Browser != out[j].Browser {
			return out[i].Browser < out[j].Browser
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// findCookiesFile 兼容两种目录布局：
// 新版在 <profile>/Network/Cookies，老版直接在 <profile>/Cookies。
func findCookiesFile(dir string) (string, bool) {
	for _, rel := range []string{filepath.Join("Network", "Cookies"), "Cookies"} {
		p := filepath.Join(dir, rel)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Size() > 0 {
			return p, true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// 主密钥

// localState 是 Local State 里我们关心的部分。
type localState struct {
	OSCrypt struct {
		EncryptedKey string `json:"encrypted_key"`
	} `json:"os_crypt"`
}

// chromeMasterKey 取出用于解 cookie 的 32 字节 AES 密钥。
//
// Local State 里的 encrypted_key 是 base64，解开后前面 5 个字节是字面量 "DPAPI"，
// 其余才是真正的 DPAPI 密文。这个前缀是个历史包袱，但必须剥掉。
func chromeMasterKey(localStatePath string) ([]byte, error) {
	raw, err := os.ReadFile(localStatePath)
	if err != nil {
		return nil, fmt.Errorf("读取 Local State 失败：%w", err)
	}
	var ls localState
	if err := json.Unmarshal(raw, &ls); err != nil {
		return nil, fmt.Errorf("解析 Local State 失败：%w", err)
	}
	if ls.OSCrypt.EncryptedKey == "" {
		return nil, errors.New("Local State 里没有 os_crypt.encrypted_key")
	}
	blob, err := base64.StdEncoding.DecodeString(ls.OSCrypt.EncryptedKey)
	if err != nil {
		return nil, fmt.Errorf("encrypted_key 不是合法 base64：%w", err)
	}
	const prefix = "DPAPI"
	if len(blob) < len(prefix) || string(blob[:len(prefix)]) != prefix {
		return nil, errors.New("encrypted_key 缺少 DPAPI 前缀，格式不是预期的那种")
	}
	key, err := dpapiUnprotect(blob[len(prefix):])
	if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("主密钥长度是 %d 字节，期望 32", len(key))
	}
	return key, nil
}

// ---------------------------------------------------------------------------
// 取 cookie

// HasBilibili 快速判断这个配置里有没有可用的 B 站登录态。
//
// 与 BilibiliCookie 的区别是它**只读 SQLite、完全不碰解密**：不解出任何
// cookie 值，也不需要主密钥，因此即使在浏览器开着、文件被锁的时候也可能
// 读到（只读共享）。界面用它给每个浏览器配置标一个「有 / 没有 B 站登录」，
// 免得用户一个个去试。
func (p Profile) HasBilibili() (bool, error) {
	tbl, err := ReadTable(p.CookiesDB, "cookies")
	if err != nil {
		return false, fmt.Errorf("读取 Cookies 库失败（浏览器可能正锁定该文件）：%w", err)
	}
	hostIdx := tbl.Column("host_key")
	nameIdx := tbl.Column("name")
	valIdx := tbl.Column("value")
	encIdx := tbl.Column("encrypted_value")
	expIdx := tbl.Column("expires_utc")
	if hostIdx < 0 || nameIdx < 0 {
		return false, errors.New("Cookies 库里没有预期的列，可能是未知的表结构")
	}

	for _, row := range tbl.Rows {
		if hostIdx >= len(row) || nameIdx >= len(row) {
			continue
		}
		if !isBilibiliHost(strings.ToLower(strings.TrimSpace(row[hostIdx].Text()))) {
			continue
		}
		// 只看 SESSDATA —— 没有它，其余字段再多也登不了录。
		if row[nameIdx].Text() != "SESSDATA" {
			continue
		}
		var expires int64
		if expIdx >= 0 && expIdx < len(row) {
			expires = row[expIdx].Int
		}
		if isExpired(expires) {
			continue
		}
		// 值可能放在明文列，也可能放在密文列，两边都算「有」。
		if valIdx >= 0 && valIdx < len(row) && row[valIdx].Text() != "" {
			return true, nil
		}
		if encIdx >= 0 && encIdx < len(row) && len(row[encIdx].Bytes()) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// cookieRow 是一条待处理的 cookie.
type cookieRow struct {
	name    string
	value   string
	host    string
	expires int64 // 微秒（1601 起算）；0 表示会话 cookie
}

// BilibiliCookie 从这个配置里导出 B 站 cookie，返回 "k=v; k=v" 形式。
//
// 返回的顺序是稳定的，方便比对；具体顺序不影响 B 站接口。
func (p Profile) BilibiliCookie() (string, error) {
	key, err := chromeMasterKey(p.LocalState)
	if err != nil {
		return "", err
	}

	tbl, err := ReadTable(p.CookiesDB, "cookies")
	if err != nil {
		return "", fmt.Errorf("读取 Cookies 库失败（浏览器可能正锁定该文件）：%w", err)
	}
	hostIdx := tbl.Column("host_key")
	nameIdx := tbl.Column("name")
	valIdx := tbl.Column("value")
	encIdx := tbl.Column("encrypted_value")
	expIdx := tbl.Column("expires_utc")
	if hostIdx < 0 || nameIdx < 0 || valIdx < 0 || encIdx < 0 {
		return "", errors.New("Cookies 库里没有预期的列，可能是未知的表结构")
	}

	// 同名的 cookie 可能同时存在于 www.bilibili.com 与 .bilibili.com，
	// 需要挑出真正生效的那一条。
	best := map[string]cookieRow{}

	for _, row := range tbl.Rows {
		if hostIdx >= len(row) || nameIdx >= len(row) {
			continue
		}
		host := strings.ToLower(strings.TrimSpace(row[hostIdx].Text()))
		if !isBilibiliHost(host) {
			continue
		}
		name := row[nameIdx].Text()
		if name == "" {
			continue
		}

		var expires int64
		if expIdx >= 0 && expIdx < len(row) {
			expires = row[expIdx].Int
		}
		if isExpired(expires) {
			continue
		}

		value := ""
		if valIdx < len(row) {
			value = row[valIdx].Text()
		}
		if value == "" && encIdx >= 0 && encIdx < len(row) {
			dec, err := decryptValue(key, row[encIdx].Bytes(), host)
			if err != nil {
				// 单条解不开就跳过它：可能是个别 cookie 用了别的加密方式，
				// 不该因此让整个导入失败。
				if errors.Is(err, ErrAppBound) {
					return "", err
				}
				continue
			}
			value = dec
		}
		if value == "" {
			continue
		}

		cur, ok := best[name]
		cand := cookieRow{name: name, value: value, host: host, expires: expires}
		if !ok || preferCookie(cand, cur) {
			best[name] = cand
		}
	}

	if len(best) == 0 {
		return "", ErrNoBilibiliLogin
	}
	if _, ok := best["SESSDATA"]; !ok {
		return "", ErrNoBilibiliLogin
	}

	names := make([]string, 0, len(best))
	for n := range best {
		names = append(names, n)
	}
	sort.Strings(names)

	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, n+"="+best[n].value)
	}
	return strings.Join(parts, "; "), nil
}

// preferCookie 决定同名的两条 cookie 留哪条。
//
// 优先父域（host_key 以 "." 开头），因为它对 api.bilibili.com 也生效；
// 同域时取过期时间更晚的。
func preferCookie(a, b cookieRow) bool {
	aParent := strings.HasPrefix(a.host, ".")
	bParent := strings.HasPrefix(b.host, ".")
	if aParent != bParent {
		return aParent
	}
	return a.expires > b.expires
}

// isBilibiliHost 判断域名是不是 B 站的。
//
// 用后缀匹配而不是 Contains："notbilibili.com" 这种钓鱼域名不该被收进来。
func isBilibiliHost(host string) bool {
	h := strings.TrimPrefix(host, ".")
	return h == "bilibili.com" || strings.HasSuffix(h, ".bilibili.com")
}

// chromeEpochOffset 是 1601-01-01 到 1970-01-01 之间的秒数。
const chromeEpochOffset = 11644473600

// isExpired 判断 expires_utc（微秒，1601 起算）是否已过期。
// 0 表示会话 cookie，不算过期。
func isExpired(expiresUTC int64) bool {
	if expiresUTC <= 0 {
		return false
	}
	sec := expiresUTC/1_000_000 - chromeEpochOffset
	return time.Now().Unix() > sec
}

// ---------------------------------------------------------------------------
// 解密

// decryptValue 解密一条 Chromium 的 encrypted_value。
//
// 现在的格式是：
//
//	v10 | 12 字节 nonce | 密文 | 16 字节 GCM tag
//
// 加解密用的是 Local State 里那把 32 字节密钥。老版本浏览器（Chrome 80 之前）
// 直接存整段 DPAPI 密文，所以没有 v10 前缀时走 DPAPI。
func decryptValue(key, blob []byte, host string) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	prefix := ""
	if len(blob) >= 3 {
		prefix = string(blob[:3])
	}

	switch prefix {
	case "v10", "v11":
		return aesGCMOpen(key, blob[3:], host)
	case "v20":
		return "", ErrAppBound
	default:
		// 老格式：整段都是 DPAPI。
		plain, err := dpapiUnprotect(blob)
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
}

// aesGCMOpen 用 AES-256-GCM 解一段 Chromium 密文。
//
// 关于附加数据（AAD）：Chrome 较早的版本用空 AAD，较新的版本把 host_key
// 当作 AAD 绑进认证标签。两个都试一遍最稳 —— 试错一次的代价只是多算一次
// GCM，而不试就意味着一部分用户永远导不出来。
func aesGCMOpen(key, data []byte, host string) (string, error) {
	const nonceLen = 12
	const tagLen = 16
	if len(data) < nonceLen+tagLen {
		return "", errors.New("密文长度不足")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce, ct := data[:nonceLen], data[nonceLen:]

	if pt, err := gcm.Open(nil, nonce, ct, []byte(host)); err == nil {
		return string(pt), nil
	}
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("GCM 解密失败：%w", err)
	}
	return string(pt), nil
}
