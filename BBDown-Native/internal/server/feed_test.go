package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamTokenSignAndVerify(t *testing.T) {
	key := []byte("test-key-do-not-use-in-prod")
	urls := []string{"https://cn-sdjn-fx-01-02.bilivideo.com/upgcxcode/a.m4s?e=abc"}
	tok, err := signStreamToken(key, urls, "TESTCOOKIE=fake")
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifyStreamToken(key, tok)
	if err != nil {
		t.Fatalf("校验自己签的 token 失败: %v", err)
	}
	if len(got.URLs) != 1 || got.URLs[0] != urls[0] {
		t.Fatalf("URL 不一致: %v", got.URLs)
	}
	if got.Cookie != "TESTCOOKIE=fake" {
		t.Fatalf("Cookie 不一致: %q", got.Cookie)
	}

	// 篡改签名应被拒
	bad := tok[:len(tok)-1] + "x"
	if _, err := verifyStreamToken(key, bad); err == nil {
		t.Fatal("篡改签名未被拒")
	}
	// 换密钥应被拒（跨进程 token 失效）
	if _, err := verifyStreamToken([]byte("other-key"), tok); err == nil {
		t.Fatal("换密钥后 token 仍有效")
	}
}

func TestStreamTokenRejectsSSRF(t *testing.T) {
	// token 是服务端签的，防 SSRF 只需挡住内网/回环字面量。
	// B 站 CDN 域名五花八门（mcdn.bilivideo.cn、edge.mountaintoys.cn 等），
	// 硬编码白名单会误杀，所以这里测的是 isPrivateHost。
	for host, want := range map[string]bool{
		"cn-sdjn-fx-01-02.bilivideo.com":       false,
		"xy223x94x121x139xy.mcdn.bilivideo.cn": false,
		"t6ur09t.edge.mountaintoys.cn":         false,
		"127.0.0.1":                            true,
		"localhost":                            true,
		"192.168.1.1":                          true,
		"10.0.0.1":                             true,
		"[::1]":                                true,
		"evil.com":                             false, // 域名不是内网字面量，签名有效就放行
	} {
		if got := isPrivateHost(host); got != want {
			t.Errorf("isPrivateHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestApiFeedBadSource(t *testing.T) {
	s := &Server{cfg: nil}
	req := httptest.NewRequest(http.MethodGet, "/api/feed?source=evil", nil)
	rec := httptest.NewRecorder()
	s.apiFeed(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法来源应返回 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "未知") {
		t.Fatalf("错误信息不对: %s", rec.Body.String())
	}
}

func TestApiStreamBadToken(t *testing.T) {
	s := &Server{}
	req := httptest.NewRequest(http.MethodGet, "/api/stream?token=garbage", nil)
	rec := httptest.NewRecorder()
	s.apiStream(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 token 应返回 400，实际 %d", rec.Code)
	}
}
