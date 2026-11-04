package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"bbdown-native/internal/subtitle"
)

// ---------------------------------------------------------------------------
// 免费 AI 总结
//
// 不用 B 站的 AI 视频总结，改走免费 AI：把文稿喂给 Pollinations
//（text.pollinations.ai，免 key 的公益网关）让它生成摘要 + 大纲。
// Pollinations 背后是多家人家开源/免费档模型，匿名可用有速率限制，
// 个人工具的调用频率完全够。
//
// 端点与模型都可以用配置覆盖（freeai_model / 免费网关换源时改
// freeai.go 顶部的常量即可），默认 openai 即 GPT 免费档。

const (
	freeAIEndpoint   = "https://text.pollinations.ai/"
	defaultFreeAI    = "openai"
	summaryMaxRunes  = 16000 // 喂给模型的单次文稿上限（约 8k token）
	freeAITimeoutSec = 180
)

// freeAIClient 独立 http 客户端：不沾 bilibili.Client 的 UA/Referer，
// 避免网关把请求当成异常流量。
var freeAIClient = &http.Client{Timeout: freeAITimeoutSec * time.Second}

// FreeAIChat 发一轮对话，返回模型输出的纯文本。
func FreeAIChat(ctx context.Context, model, system, user string) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = defaultFreeAI
	}
	payload, err := json.Marshal(map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, freeAIEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := freeAIClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接免费 AI 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		head := string(body)
		if len(head) > 200 {
			head = head[:200]
		}
		return "", fmt.Errorf("免费 AI 返回 HTTP %d：%s", resp.StatusCode, strings.TrimSpace(head))
	}
	out := strings.TrimSpace(string(body))
	if out == "" {
		return "", fmt.Errorf("免费 AI 返回空内容")
	}
	return out, nil
}

// summarySystemPrompt 是总结任务的系统提示词。
const summarySystemPrompt = `你是一名专业的中文视频内容整理助手。用户会给你一份视频文稿（每行以 [分:秒] 时间戳开头）。请输出 Markdown 总结，结构如下：

## 摘要
用 3~6 句话概括这个视频讲了什么。

## 大纲
按时间顺序把内容分成 3~8 个章节。每章一行，格式为：
- **[分:秒] 章节标题**：一句话说明该章节的要点
时间戳必须取自文稿中实际出现的数值。

## 要点回顾
用 3~6 条列表提炼最有价值的信息。

要求：只输出 Markdown 正文，不要开场白和结尾语；文稿是语音转写，可能有识别错误，理解意思后纠正再总结；如果内容太少就如实少写。`

// transcriptForSummary 把字幕条拼成带时间戳的文稿，超长截断。
func transcriptForSummary(cues []subtitle.Cue) string {
	var b strings.Builder
	for _, c := range cues {
		fmt.Fprintf(&b, "[%s] %s\n", mmss(int(c.From)), c.Text)
	}
	s := strings.TrimSpace(b.String())
	r := []rune(s)
	if len(r) > summaryMaxRunes {
		s = string(r[:summaryMaxRunes]) + "\n\n（文稿过长，已截断）"
	}
	return s
}
