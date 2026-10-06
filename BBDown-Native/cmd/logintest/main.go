// Command logintest 联调扫码登录：申请真实二维码 → 存 PNG → 打印状态流转。
//
// 开发期工具，不参与最终发行版。
//
//	go run ./cmd/logintest -mode web -wait 20 -o .devdata/qr.png
package main

import (
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"time"

	"bbdown-native/internal/bilibili"
)

func main() {
	var (
		mode = flag.String("mode", "web", "web 或 tv")
		wait = flag.Int("wait", 20, "观察多少秒后退出")
		out  = flag.String("o", "", "把二维码存成 PNG（可选）")
	)
	flag.Parse()
	if err := run(*mode, *wait, *out); err != nil {
		fmt.Fprintln(os.Stderr, "错误:", err)
		os.Exit(1)
	}
}

func run(mode string, wait int, out string) error {
	start := time.Now()
	sess := bilibili.NewLoginSession()
	ok, msg := sess.Start(bilibili.ParseLoginMode(mode), func(r *bilibili.LoginResult) {
		fmt.Printf(">>> 登录成功! uname=%q mid=%d token=%d字节 cookie=%d字节\n",
			r.UName, r.MID, len(r.Token), len(r.Cookie))
	})
	if !ok {
		return fmt.Errorf("%s", msg)
	}

	lastSeq := -1
	saved := false
	deadline := time.Now().Add(time.Duration(wait) * time.Second)

	for time.Now().Before(deadline) {
		s := sess.Snapshot()
		if s.Seq != lastSeq {
			lastSeq = s.Seq
			fmt.Printf("[%5.1fs] %-9s %s", time.Since(start).Seconds(), s.State, s.Message)
			if s.Remain > 0 {
				fmt.Printf("   (剩 %ds)", s.Remain)
			}
			fmt.Println()
		}

		// 二维码一出来就存盘，交给 zxing 反解校验是否真的可扫
		if !saved {
			if img := sess.QRImage(); img != nil {
				b := img.Bounds()
				fmt.Printf("        二维码尺寸 %dx%d\n", b.Dx(), b.Dy())
				if out != "" {
					if err := savePNG(out, img); err != nil {
						return fmt.Errorf("存 PNG: %w", err)
					}
					fmt.Printf("        已存 %s\n", out)
				}
				saved = true
			}
		}
		if s.State == bilibili.StateConfirmed || s.State == bilibili.StateFailed {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}

	sess.Cancel()
	fmt.Println("--- 结束 ---")
	return nil
}

func savePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
