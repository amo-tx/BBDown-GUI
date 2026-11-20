package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bbdown-native/internal/app"
	"bbdown-native/internal/bilibili"
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

// TestHomeHasMore 锁住首页「加载更多」的语义：排行榜一次给全 100 条且不能翻页，
// 永远不该说还有下一页；热门按每页 20 条判断；推荐只要拿到内容就继续。
func TestHomeHasMore(t *testing.T) {
	cases := []struct {
		tab  string
		n    int
		page int
		want bool
	}{
		{"rank", 100, 1, false},
		{"rank", 0, 2, false},
		{"popular", 20, 1, true},
		{"popular", 7, 1, false},
		{"rcmd", 12, 1, true},
		{"rcmd", 0, 1, false},
	}
	for _, c := range cases {
		if got := homeHasMore(c.tab, c.n, c.page); got != c.want {
			t.Errorf("homeHasMore(%q,%d,%d) = %v, want %v", c.tab, c.n, c.page, got, c.want)
		}
	}
}

// TestHomeCacheSeparateFromFeed 首页缓存和「刷视频」的热门缓存共用一张表，
// 但键加了 home: 前缀，不能互相顶掉（顶掉会让首页拿到 20 条的列表页数据）。
func TestHomeCacheSeparateFromFeed(t *testing.T) {
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")
	feedItems := []bilibili.FeedItem{{Bvid: "BVfeed", Title: "列表页"}}
	homeItems := []bilibili.FeedItem{{Bvid: "BVhome", Title: "首页"}}

	s.cacheFeed("popular", 1, feedItems)
	s.cacheList("popular:0:1", homeItems)

	gotFeed, ok := s.cachedFeed("popular", 1)
	if !ok || gotFeed[0].Bvid != "BVfeed" {
		t.Fatalf("刷视频缓存被污染：ok=%v %v", ok, gotFeed)
	}
	gotHome, ok := s.cachedList("popular:0:1")
	if !ok || gotHome[0].Bvid != "BVhome" {
		t.Fatalf("首页缓存没命中：ok=%v %v", ok, gotHome)
	}
	// 过期立刻失效
	s.feedMu.Lock()
	e := s.feedCache["home:popular:0:1"]
	e.at = time.Now().Add(-2 * feedCacheTTL)
	s.feedCache["home:popular:0:1"] = e
	s.feedMu.Unlock()
	if _, ok := s.cachedList("popular:0:1"); ok {
		t.Fatal("过期缓存不应命中")
	}
}

// TestApiHomeUnknownTab 未知分类要明确报 400，别让前端拿到一坨空列表还以为是没内容。
func TestApiHomeUnknownTab(t *testing.T) {
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")
	req := httptest.NewRequest(http.MethodGet, "/api/home?tab=evil", nil)
	rec := httptest.NewRecorder()
	s.apiHome(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未知分类应返回 400，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "未知") {
		t.Fatalf("错误信息不对: %s", rec.Body.String())
	}
}

// TestApiHomeRankPage2Empty 排行榜没有第二页：页号 >1 时直接给空列表 +
// has_more=false，不去打 B 站接口（也就不会因为缓存没命中而白等）。
func TestApiHomeRankPage2Empty(t *testing.T) {
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")
	req := httptest.NewRequest(http.MethodGet, "/api/home?tab=rank&rid=13&page=2", nil)
	rec := httptest.NewRecorder()
	s.apiHome(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("第二页应返回 200 空列表，实际 %d", rec.Code)
	}
	var body struct {
		Items   []bilibili.FeedItem `json:"items"`
		HasMore bool                `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(body.Items) != 0 || body.HasMore {
		t.Fatalf("第二页应为空且 has_more=false：%+v", body)
	}
}

// TestQuickDownloadHandsOffToNative 锁住内嵌页签里的「一键下载」：
// 挂了 NativeHooks 时任务要交回主窗口（先把窗口切到下载页签），
// 服务端自己的 Runner 一次都不许碰 —— 碰了就等于另起一个任务，
// 原生任务台看不到进度，用户点了下载像没反应。
func TestQuickDownloadHandsOffToNative(t *testing.T) {
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")

	var actions []string
	var gotBvid string
	var gotPage int
	s.UseNative(NativeHooks{
		Action: func(a string) { actions = append(actions, a) },
		QuickDownload: func(bvid string, page int) error {
			gotBvid, gotPage = bvid, page
			return nil
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/quick-download",
		strings.NewReader(`{"bvid":"BV1GJ411x7h7","page":2}`))
	rec := httptest.NewRecorder()
	s.apiQuickDownload(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if len(actions) != 1 || actions[0] != "download-tab" {
		t.Fatalf("切换页签的动作没发出去：%v", actions)
	}
	if gotBvid != "BV1GJ411x7h7" || gotPage != 2 {
		t.Fatalf("交回原生层的参数不对：bvid=%q page=%d", gotBvid, gotPage)
	}
	if !strings.Contains(rec.Body.String(), `"native":true`) {
		t.Fatalf("响应应标明交给了原生层：%s", rec.Body.String())
	}
	// 没起后台任务：服务端 task 状态必须还是空闲
	s.mu.Lock()
	running := s.task.running
	s.mu.Unlock()
	if running {
		t.Fatal("交给原生层后，服务端不该再自己跑一条任务")
	}
}

// TestQuickDownloadNativeBusy 原生层说「正忙」时要把错误原样回给页面，
// 让用户在刷视频页上看到提示，而不是默默什么都没发生。
func TestQuickDownloadNativeBusy(t *testing.T) {
	cfg, warn := app.LoadConfigFrom(filepath.Join(t.TempDir(), "config.json"))
	if warn != "" {
		t.Fatalf("测试配置加载异常：%s", warn)
	}
	s := New(cfg, "")
	s.UseNative(NativeHooks{
		QuickDownload: func(string, int) error { return errors.New("已经有一个任务在跑，等它结束再点") },
	})

	req := httptest.NewRequest(http.MethodPost, "/api/quick-download",
		strings.NewReader(`{"bvid":"BV1GJ411x7h7"}`))
	rec := httptest.NewRecorder()
	s.apiQuickDownload(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("忙时应返回 409，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "已经有一个任务在跑") {
		t.Fatalf("错误信息没带回页面：%s", rec.Body.String())
	}
}
