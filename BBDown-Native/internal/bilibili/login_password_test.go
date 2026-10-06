package bilibili

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// 公钥解析

func selfSignedPubPEM(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func TestParseRSAPublicKeyPKIX(t *testing.T) {
	_, pemText := selfSignedPubPEM(t)
	pub, err := parseRSAPublicKey(pemText)
	if err != nil {
		t.Fatalf("PKIX 公钥应当能解析：%v", err)
	}
	if pub.N.BitLen() != 1024 {
		t.Errorf("位长 %d，期望 1024", pub.N.BitLen())
	}
}

// 有些端点给的是 PKCS#1 编码，两种都要认。
func TestParseRSAPublicKeyPKCS1(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: der}))

	pub, err := parseRSAPublicKey(pemText)
	if err != nil {
		t.Fatalf("PKCS#1 公钥应当能解析：%v", err)
	}
	if pub.N.Cmp(key.PublicKey.N) != 0 {
		t.Error("解出来的模数与原始公钥不一致")
	}
}

func TestParseRSAPublicKeyGarbage(t *testing.T) {
	for _, in := range []string{"", "not pem at all", "-----BEGIN PUBLIC KEY-----\nzzz\n-----END PUBLIC KEY-----\n"} {
		if _, err := parseRSAPublicKey(in); err == nil {
			t.Errorf("输入 %q 应当报错", in)
		}
	}
}

// ---------------------------------------------------------------------------
// 密码加密

// 加密之后能用私钥解回来，且明文确实是「盐 + 密码」。
//
// 这条是密码登录最容易静默出错的地方：少拼盐、或盐与公钥不是同一次
// 响应的，服务端都只会回一个含糊的「账号或密码不正确」。
func TestRSAEncryptPasswordRoundTrip(t *testing.T) {
	key, pemText := selfSignedPubPEM(t)
	pub, err := parseRSAPublicKey(pemText)
	if err != nil {
		t.Fatal(err)
	}

	const salt = "0647190aadc6964f"
	const password = "pa55w0rd-中文-!@#"
	enc, err := rsaEncryptPassword(pub, salt+password)
	if err != nil {
		t.Fatalf("加密失败：%v", err)
	}

	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatalf("结果不是合法 base64：%v", err)
	}
	plain, err := rsa.DecryptPKCS1v15(nil, key, raw)
	if err != nil {
		t.Fatalf("解不回来：%v", err)
	}
	if string(plain) != salt+password {
		t.Errorf("明文是 %q，期望 %q", plain, salt+password)
	}
}

// 密文应当比公钥长度等于一次分组 —— 1024 位 = 128 字节。
func TestRSAEncryptPasswordBlockSize(t *testing.T) {
	_, pemText := selfSignedPubPEM(t)
	pub, _ := parseRSAPublicKey(pemText)
	enc, err := rsaEncryptPassword(pub, "x")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base64.StdEncoding.DecodeString(enc)
	if len(raw) != 128 {
		t.Errorf("密文长度 %d，期望 128", len(raw))
	}
}

// ---------------------------------------------------------------------------
// 错误码映射

func TestTVLoginErrorMapping(t *testing.T) {
	cases := []struct {
		code int
		want error
	}{
		{-629, ErrWrongCredential},
		{-652, ErrTVLoginRisk},
	}
	for _, c := range cases {
		got := tvLoginError(c.code, "服务端文案")
		if !errors.Is(got, c.want) {
			t.Errorf("code=%d 得到 %v，期望 %v", c.code, got, c.want)
		}
	}
}

// 未知错误码要把服务端文案带出来，不能吞掉。
func TestTVLoginErrorUnknownKeepsMessage(t *testing.T) {
	err := tvLoginError(-12345, "某个没见过的原因")
	if err == nil {
		t.Fatal("未知错误码也应当报错")
	}
	if !strings.Contains(err.Error(), "某个没见过的原因") || !strings.Contains(err.Error(), "-12345") {
		t.Errorf("应当同时带上文案与错误码，实际 %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// 入参校验（不碰网络）

func TestLoginWithPasswordValidatesInput(t *testing.T) {
	c := NewClient()
	ctx := t.Context()

	if _, err := c.LoginWithPassword(ctx, "", "pwd"); !errors.Is(err, ErrNeedUsername) {
		t.Errorf("空账号应当返回 ErrNeedUsername，实际 %v", err)
	}
	if _, err := c.LoginWithPassword(ctx, "   ", "pwd"); !errors.Is(err, ErrNeedUsername) {
		t.Errorf("纯空白账号应当返回 ErrNeedUsername，实际 %v", err)
	}
	if _, err := c.LoginWithPassword(ctx, "user", ""); !errors.Is(err, ErrNeedPassword) {
		t.Errorf("空密码应当返回 ErrNeedPassword，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// tvCookie 拼接

func TestJoinTVCookies(t *testing.T) {
	got := joinTVCookies([]tvCookie{
		{Name: "SESSDATA", Value: "aaa"},
		{Name: "bili_jct", Value: "bbb"},
		{Name: "", Value: "should-be-skipped"},
	})
	m := CookieToMap(got)
	if m["SESSDATA"] != "aaa" || m["bili_jct"] != "bbb" {
		t.Errorf("拼接结果不对：%q", got)
	}
	if len(m) != 2 {
		t.Errorf("空名字的项应当被跳过，实际 %v", m)
	}
	if joinTVCookies(nil) != "" {
		t.Error("空列表应当返回空串")
	}
}
