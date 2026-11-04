package bilibili

// 临时 live 测试：AITRANS_LIVE=1 且 AITRANS_CONFIG=<config.json 路径> 时运行。
// 用热门榜扫视频，找到有 AI 总结的稿件验证成功路径。验证完删除本文件。
// 绝不打印 cookie。

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func loadLiveCookie(t *testing.T) string {
	path := os.Getenv("AITRANS_CONFIG")
	if path == "" {
		t.Skip("AITRANS_CONFIG 未设置")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 config 失败: %v", err)
	}
	var cfg struct {
		Cookie string `json:"cookie"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config 不是合法 JSON: %v", err)
	}
	if cfg.Cookie == "" {
		t.Skip("config 没有 cookie")
	}
	return cfg.Cookie
}

func TestLiveAISummary(t *testing.T) {
	cookie := loadLiveCookie(t)
	c := NewClient()
	c.Cookie = cookie
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 热门榜拿一批候选
	var pop struct {
		List []struct {
			Bvid  string `json:"bvid"`
			Title string `json:"title"`
			Owner struct {
				Mid int64 `json:"mid"`
			} `json:"owner"`
			Cid  int64 `json:"cid"`
			Aid  int64 `json:"aid"`
		} `json:"list"`
	}
	if err := c.get(ctx, "/x/web-interface/popular", nil, &pop); err != nil {
		t.Fatalf("热门榜失败: %v", err)
	}
	t.Logf("热门榜 %d 条，逐个试 conclusion…", len(pop.List))

	found := 0
	for i, v := range pop.List {
		if i >= 15 || found >= 2 {
			break
		}
		info := &VideoInfo{Aid: v.Aid, Bvid: v.Bvid, Mid: v.Owner.Mid, Title: v.Title}
		res, err := c.AISummary(ctx, info, v.Cid)
		if err != nil {
			t.Logf("× %-28s %v", truncForLog(v.Title, 28), err)
			continue
		}
		found++
		t.Logf("✓ %-28s 摘要 %d 字，大纲 %d 章", truncForLog(v.Title, 28), len(res.Summary), len(res.Outline))
		if res.Summary != "" {
			t.Logf("   摘要片段：%s", truncForLog(res.Summary, 80))
		}
	}
	if found == 0 {
		t.Fatalf("热门前 15 里没有找到任何带 AI 总结的稿件？链路可能有系统性问题")
	}
}

func truncForLog(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
