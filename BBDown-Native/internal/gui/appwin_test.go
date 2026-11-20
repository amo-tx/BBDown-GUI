package gui

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestFeedPageURL 锁住内嵌刷视频页的两个查询开关：页面靠 embed=feed 自动展开
// 刷视频视图，缺了它内嵌页就停在下载界面（原生页签高亮刷视频却显示下载页）。
func TestFeedPageURL(t *testing.T) {
	base := "http://127.0.0.1:18230/"
	light := feedPageURL(base, false)
	if light != base+"?embed=feed" {
		t.Fatalf("浅色页地址不对：%q", light)
	}
	dark := feedPageURL(base, true)
	if dark != base+"?embed=feed&theme=dark" {
		t.Fatalf("深色页地址不对：%q", dark)
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
