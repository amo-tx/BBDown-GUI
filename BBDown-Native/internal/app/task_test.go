package app

import "testing"

// TestSetCredentialsClearsCookie 锁定退出登录也能生效：SetCredentials 原来
// 只在 cookie 非空时赋值，导致「登录页退出 → 切回下载页」后界面显示未登录、
// 下载引擎却还带着旧 Cookie 继续请求。
func TestSetCredentialsClearsCookie(t *testing.T) {
	r := NewRunner(&Config{}, Hooks{})
	r.SetCredentials("TESTCOOKIE=fake", "")
	if r.Client().Cookie == "" {
		t.Fatal("设置凭证后 client.Cookie 应更新")
	}
	r.SetCredentials("", "")
	if got := r.Client().Cookie; got != "" {
		t.Fatalf("清空凭证后 client.Cookie 应为空，实际 %q", got)
	}
}
