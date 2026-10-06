// Command e2e 在**不启动界面**的前提下把整条链路跑一遍：
// 解析地址 → 取流 → 下载 → 封装 → 校验产物。
//
// 这是交付前的主自检：界面只是壳，真正容易出问题的是这条链路。
//
//	go run ./cmd/e2e -url "<分享文案>" -out .devdata/e2e
//
// 退出码非 0 表示链路有问题。
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bbdown-native/internal/app"
	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/mux"
)

func main() {
	var (
		addr    = flag.String("url", "", "视频地址或分享文案（必填）")
		out     = flag.String("out", "", "输出目录；留空用临时目录")
		quality = flag.Int("q", 0, "画质编号，0 = 能拿多高拿多高")
		codec   = flag.String("codec", "hevc,avc,av1", "编码偏好")
		pages   = flag.String("pages", "1", "分P选择，如 1 或 1,3-5")
		verbose = flag.Bool("v", false, "打开下载层日志")
	)
	flag.Parse()

	if *addr == "" {
		// 没给地址就退回 .devdata 里已有的 m4s 做纯封装校验
		fmt.Fprintln(os.Stderr, "缺少 -url；将只对 .devdata 做封装校验")
		if err := muxCheck(); err != nil {
			fmt.Fprintln(os.Stderr, "封装校验失败:", err)
			os.Exit(1)
		}
		return
	}

	outDir := *out
	if outDir == "" {
		d, err := os.MkdirTemp("", "bge2e-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer os.RemoveAll(d)
		outDir = d
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	cfg := app.DefaultConfig()
	cfg.DownloadDir = outDir
	cfg.Quality = *quality
	cfg.CodecOrder = *codec
	if c := os.Getenv("BB_COOKIE"); c != "" {
		cfg.Cookie = c
		fmt.Println("已从环境变量读入 cookie")
	}

	runner := app.NewRunner(cfg, app.Hooks{
		Stage: func(s string) { fmt.Printf("[阶段] %s\n", s) },
		Log:   func(s string) { fmt.Printf("       %s\n", s) },
		Progress: func(p app.JobProgress) {
			if p.Total <= 0 {
				return
			}
			pct := float64(p.Done) * 100 / float64(p.Total)
			fmt.Printf("\r[进度] %-4s %5.1f%%  %s  %s",
				p.Label, pct, humanSize(p.Done)+" / "+humanSize(p.Total),
				speed(p.Speed, p.ETA))
		},
	})
	_ = *verbose

	// ---- 1. 解析 ----
	fmt.Println("=== 1/4 解析 ===")
	items, err := runner.Resolve(ctx, *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "解析失败:", err)
		os.Exit(1)
	}
	// 进度行会把这一行盖掉，补一个换行
	defer fmt.Println()
	for i, it := range items {
		fmt.Printf("  [%d] %s\n", i+1, it.DisplayName())
		fmt.Printf("      共 %d 个分P，总时长 %s\n", len(it.Pages), humanDur(it.Info.Duration))
		fmt.Printf("      cid=%d  %s\n", it.Pages[it.Info.DefaultPage-1].Cid, it.Info.Target.Raw)
		if len(it.Avail) > 0 {
			var names []string
			for _, q := range it.Avail {
				names = append(names, fmt.Sprintf("%s(%d)", bilibili.QualityName(q), q))
			}
			fmt.Printf("      可用画质: %s\n", strings.Join(names, ", "))
		} else {
			fmt.Printf("      可用画质: 取流未成功（未登录时上限 480P）\n")
		}
	}

	// ---- 2~4. 下载 + 封装 ----
	fmt.Println("=== 2/4 下载 ===  === 3/4 封装 ===")
	if err := runner.Download(ctx, items[0], app.Options{
		PageSpec: *pages,
		OutDir:   outDir,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "\n下载/封装失败:", err)
		os.Exit(1)
	}

	fmt.Println("=== 4/4 校验产物 ===")
	ok, err := verifyOutput(outDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "校验失败:", err)
		os.Exit(1)
	}
	if !ok {
		os.Exit(1)
	}
	fmt.Println("\n端到端通过。")
}

// verifyOutput 对目录里每个 .mp4 做一次解封装，确认能读出音视频轨。
func verifyOutput(dir string) (bool, error) {
	var files []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.EqualFold(filepath.Ext(p), ".mp4") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if len(files) == 0 {
		return false, fmt.Errorf("目录里没有产出 mp4：%s", dir)
	}

	allOK := true
	for _, f := range files {
		info, err := mux.Inspect(f)
		if err != nil {
			fmt.Printf("  ✗ %s\n      校验失败: %v\n", filepath.Base(f), err)
			allOK = false
			continue
		}
		fmt.Printf("  ✓ %s  %s  时长 %s  共 %d 轨\n",
			filepath.Base(f), humanSize(info.Size), humanDur(int(info.Duration/1000)), len(info.Tracks))
		for _, t := range info.Tracks {
			kind, detail := "音频", ""
			if t.IsVideo() {
				kind = "视频"
				detail = fmt.Sprintf("%s  %dx%d  %d 采样 / %d chunk  关键帧 %d",
					t.Codec, t.Width, t.Height, t.Samples, t.Chunks, syncCount(t))
			} else {
				detail = fmt.Sprintf("%s  %d Hz / %d 声道  %d 采样 / %d chunk",
					t.Codec, t.SampleRate, t.Channels, t.Samples, t.Chunks)
			}
			fmt.Printf("      %-4s  %-9s  %.2fs  %s\n", kind, t.Handler, t.Seconds(), detail)
		}
		for _, w := range info.Warnings {
			fmt.Printf("      ! %s\n", w)
		}
		// 至少要有实际内容才算通过
		if info.Duration == 0 {
			fmt.Println("      ✗ 时长为 0")
			allOK = false
		}
	}
	return allOK, nil
}

func syncCount(t mux.TrackInfo) int {
	if t.SyncSamples == 0 {
		return t.Samples // 没写 stss 就等于全部同步
	}
	return t.SyncSamples
}

// muxCheck 是不带网地址时的纯封装回归：用 .devdata 里已有的 m4s 跑一遍。
func muxCheck() error {
	v := filepath.Join(".devdata", "video.m4s")
	a := filepath.Join(".devdata", "audio.m4s")
	if _, err := os.Stat(v); err != nil {
		return fmt.Errorf("缺少 %s", v)
	}
	out := filepath.Join(".devdata", "e2e_check.mp4")
	res, err := mux.Mux(v, a, out)
	if err != nil {
		return err
	}
	fmt.Printf("封装完成 %s  视频 %d 采样 / 音频 %d 采样  %s\n",
		out, res.VideoSamples, res.AudioSamples, humanDur(int(res.DurationMs/1000)))
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func speed(bps float64, eta time.Duration) string {
	if bps <= 0 {
		return ""
	}
	s := fmt.Sprintf("%s/s", humanSize(int64(bps)))
	if eta > 0 {
		s += "  ETA " + eta.Round(time.Second).String()
	}
	return s
}

func humanDur(sec int) string {
	if sec <= 0 {
		return "0:00"
	}
	h, m, s := sec/3600, (sec%3600)/60, sec%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
