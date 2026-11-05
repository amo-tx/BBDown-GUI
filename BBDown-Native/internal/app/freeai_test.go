package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestOpenAIChat 验证国产网关（OpenAI 兼容协议）调用：URL 拼接、Bearer 头、
// 响应解析。
func TestOpenAIChat(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		var req chatRequest
		json.NewDecoder(r.Body).Decode(&req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"  ## 摘要\n\n测试总结内容。  "}}]}`))
	}))
	defer srv.Close()

	out, err := openAIChat(context.Background(), srv.URL, "testkey.xxx", "", "sys", "user msg")
	if err != nil {
		t.Fatalf("openAIChat 失败: %v", err)
	}
	if out != "## 摘要\n\n测试总结内容。" {
		t.Fatalf("输出没去首尾空白：%q", out)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("base 应自动拼 /chat/completions，实际 %s", gotPath)
	}
	if gotAuth != "Bearer testkey.xxx" {
		t.Fatalf("Authorization 头不对：%q", gotAuth)
	}
	// model 留空应退默认 glm-4-flash
	if gotModel != defaultAIModel {
		t.Fatalf("model 应退默认 %s，实际 %s", defaultAIModel, gotModel)
	}
}

// TestOpenAIChatFullURL base 直接给完整 /chat/completions 地址时不应重复拼接。
func TestOpenAIChatFullURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4/chat/completions" {
			t.Errorf("路径应是 /v4/chat/completions，实际 %s", r.URL.Path)
		}
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()
	out, err := openAIChat(context.Background(), srv.URL+"/v4/chat/completions", "k", "m", "s", "u")
	if err != nil || out != "ok" {
		t.Fatalf("完整地址调用失败: %v, %q", err, out)
	}
}

// TestOpenAIChatError 网关报错要透出原因，不能吞成空。
func TestOpenAIChatError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"令牌无效"}}`))
	}))
	defer srv.Close()
	_, err := openAIChat(context.Background(), srv.URL, "bad", "m", "s", "u")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("应报 HTTP 401，实际: %v", err)
	}
	// 200 但 choices 为空也要报错
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv2.Close()
	if _, err := openAIChat(context.Background(), srv2.URL, "k", "m", "s", "u"); err == nil {
		t.Fatal("空 choices 应报错")
	}
}

// TestAIChatFallback 主路失败要自动兜底到 Pollinations。
func TestAIChatFallback(t *testing.T) {
	oldEndpoint := pollinationsEndpoint
	defer func() { pollinationsEndpoint = oldEndpoint }()

	// 主路：智谱形状的服务器，一直 500
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"message":"炸了"}}`))
	}))
	defer badSrv.Close()
	// 兜底：Pollinations 形状的服务器，直接回纯文本
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("## 摘要\n\n兜底成功"))
	}))
	defer okSrv.Close()
	pollinationsEndpoint = okSrv.URL

	cfg := DefaultConfig()
	cfg.AIKey = "k"
	cfg.AIBase = badSrv.URL
	logs := []string{}
	out, err := AIChat(context.Background(), cfg, "sys", "user", func(f string, a ...any) {
		logs = append(logs, f)
	})
	if err != nil || !strings.Contains(out, "兜底成功") {
		t.Fatalf("主路失败后应兜底成功: %v, %q", err, out)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "兜底") {
		t.Fatalf("应日志提示兜底：%s", joined)
	}
}

// TestAIChatNoKey 没填 key 直接走兜底，且日志里给领 key 指引。
func TestAIChatNoKey(t *testing.T) {
	oldEndpoint := pollinationsEndpoint
	defer func() { pollinationsEndpoint = oldEndpoint }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("兜底输出"))
	}))
	defer srv.Close()
	pollinationsEndpoint = srv.URL

	cfg := DefaultConfig() // ai_key 为空
	logs := []string{}
	out, err := AIChat(context.Background(), cfg, "s", "u", func(f string, a ...any) { logs = append(logs, f) })
	if err != nil || out != "兜底输出" {
		t.Fatalf("无 key 应直接兜底: %v, %q", err, out)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "ai_key") {
		t.Fatalf("日志应提示配置 ai_key：%s", strings.Join(logs, "\n"))
	}
}

// TestSaveTextOnce 已存在的文件不重复写。
func TestSaveTextOnce(t *testing.T) {
	dir := t.TempDir()
	p1, created1, err := SaveTextOnce(dir, "a_文稿.txt", []byte("第一版"))
	if err != nil || !created1 {
		t.Fatalf("首次应新写: %v", err)
	}
	// 改掉磁盘上的内容，再调一次不应覆盖
	if err := os.WriteFile(p1, []byte("用户改过的"), 0o644); err != nil {
		t.Fatal(err)
	}
	p2, created2, err := SaveTextOnce(dir, "a_文稿.txt", []byte("第二版"))
	if err != nil {
		t.Fatal(err)
	}
	if p2 != p1 || created2 {
		t.Fatalf("已存在应复用路径不重写: %q created=%v", p2, created2)
	}
	got, _ := os.ReadFile(p2)
	if string(got) != "用户改过的" {
		t.Fatalf("已存在文件被覆盖了：%q", got)
	}
}
