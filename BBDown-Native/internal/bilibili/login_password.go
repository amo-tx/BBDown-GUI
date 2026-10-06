package bilibili

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// 账号密码登录走的是**电视端**接口。
//
// 为什么不用网页端的 /x/passport-login/web/login：它强制要求先过极验
// （geetest）滑块 —— /x/passport-login/captcha 返回 type=geetest，
// 而带着空的 challenge/validate/seccode 去登录一律得到 -105「验证码错误」。
// APP 端的 /x/passport-login/oauth2/login 同样要极验。
//
// 只有电视端这一组接口用 appkey+appsec 签名，不要滑块 —— 电视遥控器上
// 没法拖滑块，所以它压根没有这条防线。这一点是实测出来的：
// 用不存在的账号打这个接口，稳定返回 -629「用户名或密码错误」，
// 而不是极验的 -105，说明请求已经走到凭据校验那一步了。
const (
	// tvLoginKeyURL 是电视端**专用**的公钥端点。
	//
	// 千万别图省事改用网页端的 /x/passport-login/web/key：两者返回的是
	// 不同的 RSA 公钥（实测公钥指纹不同）。用错公钥加密出来的密码，
	// 服务端解不开，结果是无论密码多正确都稳定报「用户名或密码错误」——
	// 一个非常难排查的静默失败。
	tvLoginKeyURL = "https://passport.bilibili.com/x/passport-tv-login/key"
	tvLoginURL    = "https://passport.bilibili.com/x/passport-tv-login/login"

	// tvLoginCID 是「中国大陆」的国家/地区代码。接口要它。
	tvLoginCID = "86"
)

// 登录相关的错误。调用方可以直接 errors.Is 判断，界面据此给提示。
var (
	ErrNeedUsername    = errors.New("请填写账号（手机号或邮箱）")
	ErrNeedPassword    = errors.New("请填写密码")
	ErrWrongCredential = errors.New("账号或密码不正确")
	// ErrTVLoginRisk 表示账号触发了服务端的安全策略（如 -652）。
	ErrTVLoginRisk = errors.New("这个账号被要求额外验证，请改用扫码登录，或从浏览器导入 Cookie")
)

// rsaKeyData 是 key 接口的响应体。
//
// hash 是每次请求都变的随机盐，**必须和同一次响应里的 key 配套使用**；
// 拿另一次请求的 hash 去配这次的 key，加密出来的东西服务端一样解不开。
type rsaKeyData struct {
	Hash string `json:"hash"`
	Key  string `json:"key"`
}

// LoginWithPassword 用账号密码登录，成功后返回与扫码登录同构的凭证。
//
// password 只在本函数内用于加密，不会被写进任何结构体、配置或日志。
func (c *Client) LoginWithPassword(ctx context.Context, username, password string) (*LoginResult, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, ErrNeedUsername
	}
	if password == "" {
		return nil, ErrNeedPassword
	}

	// 1) 取电视端专用的盐与公钥
	var kd rsaKeyData
	if err := c.getURL(ctx, tvLoginKeyURL, &kd); err != nil {
		return nil, fmt.Errorf("获取加密公钥失败：%w", err)
	}
	pub, err := parseRSAPublicKey(kd.Key)
	if err != nil {
		return nil, fmt.Errorf("公钥格式异常：%w", err)
	}
	if kd.Hash == "" {
		return nil, errors.New("服务端未返回加密盐值")
	}

	enc, err := rsaEncryptPassword(pub, kd.Hash+password)
	if err != nil {
		return nil, fmt.Errorf("密码加密失败：%w", err)
	}

	// 2) 签名并提交。tvSign 会补上 appkey / local_id / ts / sign。
	form := tvSign(url.Values{
		"username": {username},
		"password": {enc},
		"cid":      {tvLoginCID},
	})

	var res tvPollResp
	if err := postGet(ctx, c.hc, tvLoginURL, form, &res); err != nil {
		return nil, fmt.Errorf("登录请求失败：%w", err)
	}
	if res.Code != CodeOK {
		return nil, tvLoginError(res.Code, res.Message)
	}

	// 3) 组装结果。结构与电视端扫码轮询完全一致，所以复用同一个类型。
	d := res.Data
	out := &LoginResult{
		Cookie: NormalizeCookie(joinTVCookies(d.CookieInfo.Cookies)),
		Token:  strings.TrimSpace(d.AccessToken),
		MID:    d.MID,
	}
	if !HasAuthCookie(out.Cookie) && out.Token == "" {
		// 业务码是 0 却没给凭证，多半是触发了二次验证（安全中心）。
		return nil, ErrTVLoginRisk
	}

	// 顺手补一下昵称；拿不到不影响登录本身。
	if HasAuthCookie(out.Cookie) {
		vctx, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		if acc, err := NewClient().Verify(vctx, out.Cookie); err == nil && acc != nil && acc.LoggedIn {
			out.UName = acc.UName
			if acc.MID != 0 {
				out.MID = acc.MID
			}
		}
	}
	return out, nil
}

// tvLoginError 把电视端登录的业务错误码翻成人话。
func tvLoginError(code int, msg string) error {
	switch code {
	case -629:
		return ErrWrongCredential
	case -652:
		// 实测用真实存在、但未在电视端登录过的手机号打这个接口就会得到它。
		// 服务端文案是「重复的用户」，语义不明确，所以给一条可执行的建议。
		return ErrTVLoginRisk
	case -3:
		return errors.New("签名被拒绝，可能是本机时间不准（请校准系统时间后重试）")
	case -400:
		return errors.New("账号或密码格式不对")
	default:
		return fmt.Errorf("登录失败：%s（错误码 %d）", firstNonEmpty(msg, "未知原因"), code)
	}
}

// ---------------------------------------------------------------------------
// RSA

// parseRSAPublicKey 解析 PEM 公钥，PKIX 与 PKCS#1 两种编码都认。
func parseRSAPublicKey(pemText string) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode([]byte(pemText))
	if blk == nil {
		return nil, errors.New("不是 PEM 格式")
	}
	if pk, err := x509.ParsePKIXPublicKey(blk.Bytes); err == nil {
		if rp, ok := pk.(*rsa.PublicKey); ok {
			return rp, nil
		}
		return nil, errors.New("公钥不是 RSA")
	}
	if rp, err := x509.ParsePKCS1PublicKey(blk.Bytes); err == nil {
		return rp, nil
	}
	return nil, errors.New("公钥无法解析")
}

// rsaEncryptPassword 按 B 站约定加密密码：PKCS#1 v1.5 填充，结果 base64。
//
// 明文是 `盐 + 原密码`（盐就是 key 接口返回的 hash）—— 少了这一步，
// 即使公钥用对了也一样登录失败。
func rsaEncryptPassword(pub *rsa.PublicKey, plain string) (string, error) {
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(plain))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}
