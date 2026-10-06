// Command muxtest 把 .devdata 下的 video.m4s / audio.m4s 封装成 MP4。
//
// 这是开发期的联调入口，不参与最终发行版。
//
//	go run ./cmd/muxtest -o out.mp4
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"bbdown-native/internal/mux"
)

func main() {
	var (
		dir = flag.String("dir", ".devdata", "存放 video.m4s / audio.m4s 的目录")
		out = flag.String("o", "out.mp4", "输出的 MP4 路径")
		noA = flag.Bool("no-audio", false, "忽略音频轨")
		noV = flag.Bool("no-video", false, "忽略视频轨")
	)
	flag.Parse()

	vp := filepath.Join(*dir, "video.m4s")
	ap := filepath.Join(*dir, "audio.m4s")
	if *noV {
		vp = ""
	}
	if *noA {
		ap = ""
	}

	for _, p := range []string{vp, ap} {
		if p == "" {
			continue
		}
		st, err := os.Stat(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "找不到输入文件: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("输入 %-28s %10d 字节\n", p, st.Size())
	}

	res, err := mux.Mux(vp, ap, *out)
	if err != nil {
		fmt.Fprintf(os.Stderr, "封装失败: %v\n", err)
		os.Exit(1)
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))

	st, _ := os.Stat(*out)
	fmt.Printf("产物 %s  %.2f MiB\n", *out, float64(st.Size())/(1<<20))
}
