//go:build !windows

package cookie

import "errors"

// 本包在 Windows 上才有真实现（见 browser.go / dpapi.go）。
//
// 这里给一份空壳而不是让整个包消失，是为了让 `GOOS=linux go build ./...`
// 仍然能过 —— 一个只在 Windows 下能编译的仓库，任何非 Windows 的 CI 或
// 编辑器工具（gopls、静态检查）都会一直报错，久了就没人看告警了。

// Profile 在非 Windows 平台上只是一个占位。
type Profile struct {
	Browser    string
	Name       string
	Dir        string
	CookiesDB  string
	LocalState string
}

// Display 返回给人看的一行描述。
func (p Profile) Display() string { return p.Browser + " · " + p.Name }

var (
	// ErrNoBilibiliLogin 表示库里没有 B 站的登录态。
	ErrNoBilibiliLogin = errors.New("这个浏览器里没有 B 站的登录 Cookie")
	// ErrAppBound 表示 Cookie 用了应用绑定加密。
	ErrAppBound = errors.New("该浏览器的 Cookie 用了应用绑定加密（v20），无法直接解密；请改用扫码登录")
)

// Discover 在非 Windows 平台上永远返回空列表。
func Discover() []Profile { return nil }

// HasBilibili 在非 Windows 平台上永远返回「没有」。
func (p Profile) HasBilibili() (bool, error) { return false, nil }

// BilibiliCookie 在非 Windows 平台上不可用。
func (p Profile) BilibiliCookie() (string, error) {
	return "", errors.New("从本机浏览器导入 Cookie 目前只在 Windows 上可用")
}
