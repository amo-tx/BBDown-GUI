package gui

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestEmbedPageURL 锁住内嵌页的查询开关：页面靠 embed=login/feed 自动展开
// 对应视图，缺了它内嵌页就停在下载界面（原生页签高亮却显示下载页）。
func TestEmbedPageURL(t *testing.T) {
	base := "http://127.0.0.1:18230/"
	cases := []struct {
		kind, method string
		dark         bool
		want         string
	}{
		{"feed", "", false, base + "?embed=feed"},
		{"feed", "", true, base + "?embed=feed&theme=dark"},
		{"login", "tv", true, base + "?embed=login&theme=dark&method=tv"},
		{"login", "", false, base + "?embed=login"},
	}
	for _, c := range cases {
		if got := embedPageURL(base, c.kind, c.method, c.dark); got != c.want {
			t.Errorf("embedPageURL(%q,%q,%v) = %q, want %q", c.kind, c.method, c.dark, got, c.want)
		}
	}
}

// TestFeedPageURL 锁住刷视频两种模式各自的落点：精选走本地页面并且必须带
// embed=feed 开关（少了它页面停在下载界面），官网直接是 bilibili.com 首页。
func TestFeedPageURL(t *testing.T) {
	base := "http://127.0.0.1:18230/"
	if got := feedPageURL(base, feedModePick, false); got != base+"?embed=feed" {
		t.Errorf("精选模式 = %q，应带 embed=feed", got)
	}
	if got := feedPageURL(base, feedModePick, true); got != base+"?embed=feed&theme=dark" {
		t.Errorf("精选模式深色 = %q", got)
	}
	// 官网模式完全不碰本地服务：地址就是 bilibili.com，深色主题也照旧
	// （B 站页面不跟我们的主题走）。
	if got := feedPageURL(base, feedModeWeb, true); got != feedWebURL {
		t.Errorf("官网模式 = %q，应为 %q", got, feedWebURL)
	}
}

// TestExplorerTargetSelectsSingleFile 锁住「打开下载目录」在有单个产出时
// 必须用 explorer /select,<路径> 选中文件，而不是把文件路径直接喂给
// explorer（那等于"用默认程序打开"，会弹出播放器而不是资源管理器）。
func TestExplorerTargetSelectsSingleFile(t *testing.T) {
	dir := filepath.Join("C:", "Downloads", "BBDown")
	got := explorerTarget(dir, []string{"视频.mp4"})
	want := "/select," + filepath.Join(dir, "视频.mp4")
	if got != want {
		t.Fatalf("单文件应生成 /select 参数：got %q, want %q", got, want)
	}
	// /select, 与路径必须在同一个参数里（无空格），否则 explorer 不认
	if strings.Contains(got, " /select") || strings.Contains(got, ", ") {
		t.Fatalf("/select, 后不能插入空格：%q", got)
	}
}

// TestExplorerTargetFallsBackToDir 多个产出或没有产出时没法选中，直接开目录。
func TestExplorerTargetFallsBackToDir(t *testing.T) {
	dir := filepath.Join("C:", "Downloads", "BBDown")
	for _, files := range [][]string{nil, {}, {"a.mp4", "b.mp4"}} {
		if got := explorerTarget(dir, files); got != dir {
			t.Fatalf("files=%v 应开目录 %q，实际 %q", files, dir, got)
		}
	}
}
