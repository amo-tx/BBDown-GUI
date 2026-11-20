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

// TestFeedPageURL 锁住刷视频两种模式各自的落点：都是本地页面，靠 embed 开关
// 区分（feed = 紧凑列表，home = 首页瀑布流）；少了开关页面就停在下载界面。
func TestFeedPageURL(t *testing.T) {
	base := "http://127.0.0.1:18230/"
	cases := []struct {
		mode feedMode
		dark bool
		want string
	}{
		{feedModePick, false, base + "?embed=feed"},
		{feedModePick, true, base + "?embed=feed&theme=dark"},
		{feedModeHome, false, base + "?embed=home"},
		{feedModeHome, true, base + "?embed=home&theme=dark"},
	}
	for _, c := range cases {
		if got := feedPageURL(base, c.mode, c.dark); got != c.want {
			t.Errorf("feedPageURL(mode=%d, dark=%v) = %q, want %q", c.mode, c.dark, got, c.want)
		}
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
