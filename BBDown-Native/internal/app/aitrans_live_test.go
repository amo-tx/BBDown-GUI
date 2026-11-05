package app

import (
	"context"
	"os"
	"testing"
	"time"

	"bbdown-native/internal/bilibili"
)

// TestLiveASRFallback 真实链路验证（环境变量守卫，默认跳过）：
//
//	AITRANS_LIVE=1 AITRANS_CONFIG=<config 路径> go test ./internal/app/ -run TestLiveASRFallback -v
//
// 直接走 transcribeLocally（绕过字幕），验证「下载音频 → 纯 Go 解码 →
// whisper 转写 → 免费 AI 总结」整条托底链。
func TestLiveASRFallback(t *testing.T) {
	if os.Getenv("AITRANS_LIVE") == "" {
		t.Skip("需要 AITRANS_LIVE=1 才跑真实链路")
	}
	cfgPath := os.Getenv("AITRANS_CONFIG")
	if cfgPath == "" {
		t.Skip("需要 AITRANS_CONFIG 指向配置文件")
	}
	cfg, _ := LoadConfigFrom(cfgPath)
	if cfg.Cookie == "" {
		t.Skip("config 没有 cookie")
	}
	// 用 tiny 模型验证链路（75MB，下载快、转写快）；生产默认 small。
	cfg.ASRModel = "tiny"

	client := bilibili.NewClient()
	client.Cookie = cfg.Cookie
	targets := bilibili.ExtractTargets("BV1GJ411x7h7")
	if len(targets) == 0 {
		t.Fatal("目标提取失败")
	}
	info, err := client.VideoInfo(ctxOf(60*time.Second), targets[0])
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	page := info.Pages[0]
	logf := func(f string, a ...any) { t.Logf(f, a...) }

	start := time.Now()
	cues, err := transcribeLocally(ctxOf(45*time.Minute), client, info, page, cfg, logf)
	if err != nil {
		t.Fatalf("本地转写失败: %v", err)
	}
	t.Logf("转写 %d 条，耗时 %s", len(cues), time.Since(start).Round(time.Second))
	if len(cues) == 0 {
		t.Fatal("转写结果为空")
	}
	// 打印前 3 条看看内容是否像人话
	for i, c := range cues {
		if i >= 3 {
			break
		}
		t.Logf("  [%6.2f-%6.2f] %s", c.From, c.To, c.Text)
	}

	// AI 总结链路（ai_key 填了走国产网关，没填走 Pollinations 兜底）
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	out, err := AIChat(ctx, cfg, summarySystemPrompt, buildSummaryUserPrompt(info, page, transcriptForSummary(cues)), logf)
	if err != nil {
		t.Fatalf("AI 总结失败: %v", err)
	}
	if len(out) < 100 {
		t.Fatalf("总结过短（%d 字节）：%s", len(out), out)
	}
	head := out
	if len(head) > 500 {
		head = head[:500]
	}
	t.Logf("AI 总结（%d 字节）开头：\n%s", len(out), head)
}

func ctxOf(d time.Duration) context.Context {
	c, cancel := context.WithTimeout(context.Background(), d)
	_ = cancel // 测试进程退出即释放
	return c
}
