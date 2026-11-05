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
// AI 总结的模型层
//
// 两级：
//   1. 主路 —— 国产 OpenAI 兼容网关。默认智谱 open.bigmodel.cn 的
//      GLM-4-Flash（永久免费、只限并发不限量、国内直连稳定），config.json
//      填 ai_key 即启用；换硅基流动 / 百炼等服务改 ai_base + ai_model 即可，
//      都是同一个 OpenAI 兼容协议。
//   2. 兜底 —— Pollinations（text.pollinations.ai，免 key 公益网关）。
//      ai_key 没填、或主路调用失败时自动走它；它匿名可用但限流严重，
//      稳定性远不如带 key 的国产网关，所以只当备胎。
//
// 领免费 Key：https://open.bigmodel.cn 注册 → API 密钥 → 创建，填进
// config.json 的 "ai_key" 字段即可。

const (
	// defaultAIBase 默认国产网关：智谱（GLM-4-Flash 永久免费）。
	defaultAIBase = "https://open.bigmodel.cn/api/paas/v4"
	// defaultAIModel 默认模型：GLM-4-Flash，128K 上下文，中文总结够用。
	defaultAIModel  = "glm-4-flash"
	defaultFreeAI   = "openai"
	summaryMaxRunes = 16000 // 喂给模型的单次文稿上限（约 8k token）
	aiTimeoutSec    = 180
)

// pollinationsEndpoint 兜底网关地址。用 var 是为了单测能把它指到
// httptest 服务器上验证兜底逻辑。
var pollinationsEndpoint = "https://text.pollinations.ai/"

// aiHTTPClient 独立 http 客户端：不沾 bilibili.Client 的 UA/Referer，
// 避免网关把请求当成异常流量。
var aiHTTPClient = &http.Client{Timeout: aiTimeoutSec * time.Second}

// chatRequest / chatResponse 是 OpenAI 兼容协议的最小子集，
// 国产网关（智谱 / 硅基流动 / 百炼 / 千帆兼容模式）全都认这个形状。
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// openAIChat 调一次 OpenAI 兼容的 /chat/completions，返回纯文本输出。
// base 允许填到 /v4、/v1 这一级，也允许直接填完整的 /chat/completions 地址。
func openAIChat(ctx context.Context, base, key, model, system, user string) (string, error) {
	if strings.TrimSpace(base) == "" {
		base = defaultAIBase
	}
	if strings.TrimSpace(model) == "" {
		model = defaultAIModel
	}
	if !strings.HasSuffix(strings.TrimRight(base, "/"), "/chat/completions") {
		base = strings.TrimRight(base, "/") + "/chat/completions"
	}
	payload, err := json.Marshal(chatRequest{
		Model: model,
		Messages: []chatMessage{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := aiHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接失败: %w", err)
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
		return "", fmt.Errorf("HTTP %d：%s", resp.StatusCode, strings.TrimSpace(head))
	}
	var cr chatResponse
	if err := json.Unmarshal(body, &cr); err != nil {
		return "", fmt.Errorf("响应不是合法 JSON：%s", summarizeBody(body))
	}
	if cr.Error != nil && cr.Error.Message != "" {
		return "", fmt.Errorf("网关报错：%s", cr.Error.Message)
	}
	if len(cr.Choices) == 0 || strings.TrimSpace(cr.Choices[0].Message.Content) == "" {
		return "", fmt.Errorf("返回内容为空：%s", summarizeBody(body))
	}
	return strings.TrimSpace(cr.Choices[0].Message.Content), nil
}

// summarizeBody 把网关返回的异常报文压成一行短摘要，方便日志里看原因。
func summarizeBody(body []byte) string {
	s := strings.TrimSpace(string(body))
	s = strings.ReplaceAll(s, "\n", " ")
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120]) + "…"
	}
	return s
}

// AIChat 总结对话主入口：有 ai_key 走国产网关，失败或没填则兜底
// Pollinations。log 可为 nil。
func AIChat(ctx context.Context, cfg *Config, system, user string, log func(string, ...any)) (string, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	if key := cfg.AIKeyName(); key != "" {
		out, err := openAIChat(ctx, cfg.AIBaseURL(), key, cfg.AIModelName(), system, user)
		if err == nil {
			return out, nil
		}
		log("国产模型 %s 调用失败（%v），改走 Pollinations 兜底…", cfg.AIModelName(), err)
	} else {
		log("提示：config.json 未填 ai_key，走 Pollinations 免 key 网关（限流多、偶发失败）。")
		log("稳定方案：open.bigmodel.cn 免费注册领 Key（GLM-4-Flash 永久免费），填入 ai_key 即可。")
	}
	return FreeAIChat(ctx, cfg.FreeAIModelName(), system, user)
}

// FreeAIChat Pollinations 兜底：免 key 匿名可用，但限流严重、偶发超时，
// 失败后隔几秒重试一次再放弃。
func FreeAIChat(ctx context.Context, model, system, user string) (string, error) {
	if strings.TrimSpace(model) == "" {
		model = defaultFreeAI
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		out, err := pollinationsOnce(ctx, model, system, user)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return "", lastErr
}

// pollinationsOnce 发一次 Pollinations 请求（成功直接回纯文本）。
func pollinationsOnce(ctx context.Context, model, system, user string) (string, error) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, pollinationsEndpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := aiHTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("连接免费 AI 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("免费 AI 返回 HTTP %d：%s", resp.StatusCode, summarizeBody(body))
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
