// 命令 probe 是开发期的联调工具：拿一个地址走一遍
// 提取 → 解析信息 → 取流，并把结果打出来。
//
// 它不参与打包，只用来在改接口层时快速定位问题：
//
//	go run ./cmd/probe "https://www.bilibili.com/video/BV12eHa6RE59"
//
// 需要登录态时设环境变量 BB_COOKIE。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"bbdown-native/internal/bilibili"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "用法: probe <地址/BV号/分享文案>")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	c := bilibili.NewClient()
	if s := os.Getenv("BB_COOKIE"); s != "" {
		c.Cookie = s
		fmt.Println("[i] 已注入 Cookie")
	}

	// 1. 提取
	targets := bilibili.ExtractTargets(os.Args[1])
	fmt.Printf("[1] 提取到 %d 个目标\n", len(targets))
	if len(targets) == 0 {
		os.Exit(1)
	}
	targets = c.ResolveTargets(ctx, targets)
	for i, t := range targets {
		fmt.Printf("    #%d kind=%v id=%q page=%d short=%v\n", i+1, t.Kind, t.ID, t.Page, t.Short)
	}

	// 2. 解析信息
	info, err := c.VideoInfo(ctx, targets[0])
	if err != nil {
		fmt.Println("[2] 解析失败:", err)
		os.Exit(1)
	}
	fmt.Printf("[2] 标题=%q\n    UP主=%q 时长=%ds 默认第%dP 共%dP 番剧=%v aid=%d\n",
		info.Title, info.Owner, info.Duration, info.DefaultPage, len(info.Pages), info.IsBangumi, info.Aid)
	for _, p := range info.Pages {
		if p.Index <= 3 || p.Index == len(info.Pages) {
			fmt.Printf("    P%d cid=%d %ds %q\n", p.Index, p.Cid, p.Duration, p.Title)
		}
	}

	// 3. 取流
	page := info.Pages[info.DefaultPage-1]
	pu, err := c.PlayURL(ctx, info, page)
	if err != nil {
		fmt.Println("[3] 取流失败:", err)
		os.Exit(1)
	}
	fmt.Printf("[3] 取流成功：时长=%ds 可选画质=%v\n", pu.Duration, pu.AcceptQuality)
	for _, v := range pu.Video {
		fmt.Printf("    视频 qn=%-4d %-4d×%-5d %-5s %-12s %8.2f Mbps  预估 %6.1f MB\n",
			v.ID, v.Width, v.Height, v.FrameRate, bilibili.CodecName(v.Codecs),
			float64(v.Bandwidth)/1e6, float64(v.EstimateSize(pu.Duration))/1048576)
	}
	for _, a := range pu.Audio {
		fmt.Printf("    音频 id=%-4d %-12s %8.2f kbps  预估 %6.1f MB\n",
			a.ID, a.Codecs, float64(a.Bandwidth)/1e3, float64(a.EstimateSize(pu.Duration))/1048576)
	}

	// 4. 选流结果
	v, err := pu.SelectVideo(nil, nil)
	if err != nil {
		fmt.Println("[4] 选视频流失败:", err)
		os.Exit(1)
	}
	a, err := pu.SelectAudio()
	if err != nil {
		fmt.Println("[4] 选音频流失败:", err)
		os.Exit(1)
	}
	fmt.Printf("[4] 自动选择：视频 qn=%d(%s) %s  %dp\n", v.ID, bilibili.QualityName(v.ID), v.Codecs, v.Height)
	fmt.Printf("    音频 %s\n", a.Codecs)
	fmt.Printf("    视频首地址 %.110s...\n", v.BaseURL)
	fmt.Printf("    音频首地址 %.110s...\n", a.BaseURL)

	// 5. 试拉一小段确认 CDN 不拦（只看前 64KB）
	if n, err := probeRange(ctx, c, v.BaseURL, 65536); err != nil {
		fmt.Println("[5] 视频分片探测失败:", err)
	} else {
		fmt.Printf("[5] 视频分片探测：拉到 %d 字节，CDN 正常\n", n)
	}
	if n, err := probeRange(ctx, c, a.BaseURL, 65536); err != nil {
		fmt.Println("[5] 音频分片探测失败:", err)
	} else {
		fmt.Printf("[5] 音频分片探测：拉到 %d 字节，CDN 正常\n", n)
	}

	_ = json.Marshal
}

func probeRange(ctx context.Context, c *bilibili.Client, rawURL string, n int64) (int64, error) {
	return bilibili.ProbeRange(ctx, c, rawURL, n)
}
