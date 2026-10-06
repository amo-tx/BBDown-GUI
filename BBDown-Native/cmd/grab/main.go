// 命令 grab 把指定视频的视频流与音频流拉到本地目录，供封装器开发与验证使用。
//
//	go run ./cmd/grab <地址> <输出目录>
//
// 它同时也是下载层的端到端自检：进度、并发分块、换源重试都会真实走到。
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/download"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "用法: grab <地址/分享文案> <输出目录>")
		os.Exit(2)
	}
	addr, outDir := os.Args[1], os.Args[2]

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	c := bilibili.NewClient()
	if s := os.Getenv("BB_COOKIE"); s != "" {
		c.Cookie = s
	}

	targets := bilibili.ExtractTargets(addr)
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "没有识别到地址")
		os.Exit(1)
	}
	targets = c.ResolveTargets(ctx, targets)
	info, err := c.VideoInfo(ctx, targets[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析失败:", err)
		os.Exit(1)
	}
	page := info.Pages[info.DefaultPage-1]
	pu, err := c.PlayURL(ctx, info, page)
	if err != nil {
		fmt.Fprintln(os.Stderr, "取流失败:", err)
		os.Exit(1)
	}
	v, err := pu.SelectVideo(nil, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "选视频流失败:", err)
		os.Exit(1)
	}
	a, err := pu.SelectAudio()
	if err != nil {
		fmt.Fprintln(os.Stderr, "选音频流失败:", err)
		os.Exit(1)
	}

	fmt.Printf("标题: %s\n时长: %ds\n视频: qn=%d %s %dp\n音频: %s\n",
		info.Title, pu.Duration, v.ID, bilibili.CodecName(v.Codecs), v.Height, a.Codecs)

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	dl := download.New(c)
	dl.OnLog = func(s string) { fmt.Println("  " + s) }

	// 候选地址 = 主地址 + 备用地址 + 镜像主机（部分网络下原主机容易被 403）。
	cand := func(s bilibili.Stream) []string {
		out := s.URLs()
		m := bilibili.MirrorURLs(s.BaseURL)
		if len(m) > 1 {
			out = append(out, m[1:]...)
		}
		return out
	}

	jobs := []struct {
		name string
		st   bilibili.Stream
	}{{"video.m4s", v}, {"audio.m4s", a}}

	for _, j := range jobs {
		dest := filepath.Join(outDir, j.name)
		fmt.Printf("\n== 下载 %s ==\n", j.name)
		lastPct := -1
		dl.OnProgress = func(p download.Progress) {
			if p.Total <= 0 {
				return
			}
			pct := int(p.Done * 100 / p.Total)
			// 只在百分比变化或结束时打印，避免刷屏
			if pct == lastPct && p.Done != p.Total {
				return
			}
			lastPct = pct
			fmt.Printf("  %3d%%  %7.2f / %7.2f MB  %6.2f MB/s  ETA %s\n",
				pct,
				float64(p.Done)/1048576, float64(p.Total)/1048576,
				p.Speed/1048576, p.ETA.Round(time.Second))
		}
		n, err := dl.Fetch(ctx, cand(j.st), dest, j.st.EstimateSize(pu.Duration))
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s 下载失败: %v\n", j.name, err)
			os.Exit(1)
		}
		fmt.Printf("  完成 %s  %d 字节\n", j.name, n)
	}

	fmt.Println("\n全部完成，文件列表：")
	entries, _ := os.ReadDir(outDir)
	for _, e := range entries {
		fi, _ := e.Info()
		fmt.Printf("  %-12s %10d 字节\n", e.Name(), fi.Size())
	}
}
