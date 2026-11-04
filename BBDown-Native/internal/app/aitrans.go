package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bbdown-native/internal/asr"
	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/download"
	"bbdown-native/internal/subtitle"
)

// ---------------------------------------------------------------------------
// 音频转文字 & AI 总结
//
// 转文字两级管线：
//   1. B 站字幕（AI 字幕或 UP 主上传）——快、准；
//   2. 本地 whisper.cpp 托底——B 站没字幕时，下载最低码率音频、
//      纯 Go 解码成 16k 单声道 WAV，交给本地 whisper 转写
//     （首次使用自动下载 whisper.exe + ggml 模型，之后离线可用）。
//
// AI 总结不用 B 站的 conclusion 接口，而是把文稿喂给免费 AI
//（Pollinations 免 key 网关，见 freeai.go）。

// ErrNoSubtitle 表示 B 站侧没有任何可用字幕（可走本地托底）。
var ErrNoSubtitle = errors.New("该视频没有可用字幕")

// pickSubtitleTrack 从字幕列表里挑最合适的一份：优先真人上传的中文，
// 其次 AI 生成的中文，再次任何第一条。
func pickSubtitleTrack(list []bilibili.SubtitleMeta) *bilibili.SubtitleMeta {
	var first, aiZh, zh *bilibili.SubtitleMeta
	for i := range list {
		m := &list[i]
		if zh == nil && (m.Lan == "zh-CN" || strings.HasPrefix(m.Lan, "zh")) && !m.AI {
			zh = m
		}
		if aiZh == nil && (m.Lan == "ai-zh" || strings.HasPrefix(m.Lan, "ai-zh")) {
			aiZh = m
		}
		if first == nil {
			first = m
		}
	}
	switch {
	case zh != nil:
		return zh
	case aiZh != nil:
		return aiZh
	default:
		return first
	}
}

// subtitleCues 走 B 站字幕取字幕条。没有任何字幕时报 ErrNoSubtitle，
// 其他错误（网络、登录）也返回——调用方决定要不要托底。
func subtitleCues(ctx context.Context, c *bilibili.Client, info *bilibili.VideoInfo, page bilibili.Page) ([]subtitle.Cue, *bilibili.SubtitleMeta, error) {
	list, err := c.SubtitleList(ctx, info, page.Cid)
	if err != nil {
		if errors.Is(err, bilibili.ErrSubtitleNeedsLogin) {
			return nil, nil, fmt.Errorf("该稿件有 AI 字幕，但 B 站要求登录后才能取（请先登录）")
		}
		return nil, nil, err
	}
	if len(list) == 0 {
		return nil, nil, ErrNoSubtitle
	}
	meta := pickSubtitleTrack(list)
	raw, err := c.FetchSubtitleJSON(ctx, meta.URL)
	if err != nil {
		return nil, nil, err
	}
	track, err := subtitle.ParseJSON(raw)
	if err != nil {
		return nil, nil, err
	}
	return track.Cues, meta, nil
}

// FetchTranscript 走 B 站字幕取指定分P 的文稿（老接口，托底前先走这条）。
// 返回 TXT 正文与字幕条数；没有字幕时报 ErrNoSubtitle。
func FetchTranscript(ctx context.Context, c *bilibili.Client, info *bilibili.VideoInfo, page bilibili.Page) ([]byte, int, error) {
	cues, meta, err := subtitleCues(ctx, c, info, page)
	if err != nil {
		return nil, 0, err
	}
	track := &subtitle.Track{Lang: meta.Lan, LangDoc: meta.LanDoc, AI: meta.AI, Cues: cues}
	return renderTranscript(info, page, track, "sub", len(cues)), len(cues), nil
}

// Transcript 取分P 文稿：先 B 站字幕，没有就本地 whisper 托底。
// 返回字幕条与来源标签（"sub"=B 站字幕，"asr"=本地转写）。
// log 用于向 GUI 汇报各阶段进度（可为 nil）。
func Transcript(ctx context.Context, c *bilibili.Client, info *bilibili.VideoInfo, page bilibili.Page, cfg *Config, log func(string, ...any)) ([]subtitle.Cue, string, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	cues, meta, err := subtitleCues(ctx, c, info, page)
	if err == nil {
		tag := "B 站字幕"
		if meta.AI {
			tag = "B 站 AI 字幕"
		}
		log("来源：%s（%s · %d 条）", tag, meta.LanDoc, len(cues))
		return cues, "sub", nil
	}
	log("B 站字幕不可用（%v），改用本地语音转写…", err)
	cues, err = transcribeLocally(ctx, c, info, page, cfg, log)
	if err != nil {
		return nil, "", err
	}
	return cues, "asr", nil
}

// transcribeLocally 本地托底：下载最低码率音频 → 解码 WAV → whisper。
func transcribeLocally(ctx context.Context, c *bilibili.Client, info *bilibili.VideoInfo, page bilibili.Page, cfg *Config, log func(string, ...any)) ([]subtitle.Cue, error) {
	pu, err := c.PlayURL(ctx, info, page)
	if err != nil {
		return nil, fmt.Errorf("取音频流失败: %w", err)
	}
	if len(pu.Audio) == 0 {
		return nil, errors.New("该视频拿不到音频流（可能是付费/番剧内容），无法本地转写")
	}
	// 选 AAC-LC 音轨：B 站 64k 档是 HE-AAC（SBR），纯 Go 解码器不支持；
	// Codecs "mp4a.40.2" 就是 LC。音频流按码率升序，第一个 LC 即最省的。
	var st bilibili.Stream
	found := false
	for _, a := range pu.Audio {
		if strings.Contains(a.Codecs, "mp4a.40.2") {
			st, found = a, true
			break
		}
	}
	if !found {
		// 没有 LC 档（极罕见）：取最高码率，解码时会给出明确报错。
		st = pu.Audio[len(pu.Audio)-1]
	}

	dir := asr.Home()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	// 缓存名带流 ID：同一视频换选流策略后不会错用旧文件。
	m4s := filepath.Join(dir, fmt.Sprintf("audio_%d_%d_%d.m4s", info.Aid, page.Cid, st.ID))
	dl := download.New(c)
	dl.OnLog = func(s string) { log("%s", s) }
	// GitHub 镜像单连接可能只有几十 KB/s：用更细的分块 + 更多并发
	// 把总吞吐抬上去（对 B 站 CDN 也无害）。
	dl.ChunkSize = 1 << 20
	dl.Parallel = 8
	if _, statErr := os.Stat(m4s); statErr != nil {
		log("下载音频（%d kbps · %s · 约 %d MB）…", st.Bandwidth/1000, st.Codecs, st.EstimateSize(page.Duration)/(1<<20))
		if _, err := dl.Fetch(ctx, st.URLs(), m4s, st.EstimateSize(page.Duration)); err != nil {
			return nil, fmt.Errorf("下载音频失败: %w", err)
		}
	} else {
		log("复用上次缓存的音频，直接转写")
	}

	wav := m4s + ".wav"
	defer os.Remove(wav)

	log("纯 Go 解码音频 → 16kHz 单声道 WAV…")
	var lastStep = -1
	secs, err := asr.M4SToWAV(m4s, wav, func(f float64) {
		step := int(f * 10)
		if step > lastStep {
			lastStep = step
			log("解码进度 %d%%", step*10)
		}
	})
	if err != nil {
		return nil, err
	}
	log("音频 %s，开始转写", mmss(secs))

	exePath, modelPath, err := asr.Ensure(ctx, cfg.ASRModelName(), log)
	if err != nil {
		return nil, err
	}
	log("本地转写中（whisper · CPU · 时长越长越久，请耐心等）…")
	cues, err := asr.Transcribe(ctx, exePath, modelPath, wav, cfg.ASRLangName())
	if err != nil {
		return nil, err
	}
	log("✓ 本地转写完成：%d 条", len(cues))
	return cues, nil
}

// RenderTranscript 把字幕条渲染成文稿 TXT 正文。
// source 指示来源："sub"=B 站字幕 / "asr"=本地 whisper 转写。
func RenderTranscript(info *bilibili.VideoInfo, page bilibili.Page, cues []subtitle.Cue, source string) []byte {
	track := &subtitle.Track{Cues: cues}
	return renderTranscript(info, page, track, source, len(cues))
}

// renderTranscript 拼文稿正文（纯函数，单测锁定格式）。
// source："sub"=B 站字幕（AI 与否看 track.AI），"asr"=本地 whisper 转写。
func renderTranscript(info *bilibili.VideoInfo, page bilibili.Page, track *subtitle.Track, source string, cueCount int) []byte {
	var b strings.Builder
	src := info.Bvid
	if src == "" {
		src = fmt.Sprintf("av%d", info.Aid)
	}
	fmt.Fprintf(&b, "《%s》\n", info.Title)
	if page.Index > 1 || (len(info.Pages) > 1 && page.Title != info.Title) {
		fmt.Fprintf(&b, "分P %d：%s\n", page.Index, page.Title)
	}
	fmt.Fprintf(&b, "UP 主：%s · 时长 %s · 来源 %s\n", info.Owner, mmss(page.Duration), src)
	var tag, label string
	switch source {
	case "asr":
		tag, label = "文稿来自本地 whisper 语音转写（B 站无可用字幕）", "自动识别语言"
	default:
		label = track.LangDoc
		if label == "" {
			label = track.Lang
		}
		tag = "字幕文稿"
		if track.AI {
			tag = "文稿来自 B 站 AI 字幕（音频转写）"
		}
	}
	fmt.Fprintf(&b, "%s · %s · %d 条\n\n", tag, label, cueCount)
	b.WriteString(track.Text())
	b.WriteByte('\n')
	return []byte(b.String())
}

// FetchSummary 取指定分P 的 AI 总结（免费 AI 分析文稿）：返回 Markdown 正文。
// 文稿先走 B 站字幕、再本地 whisper 托底，然后交给免费模型生成摘要+大纲。
func FetchSummary(ctx context.Context, c *bilibili.Client, info *bilibili.VideoInfo, page bilibili.Page, cfg *Config, log func(string, ...any)) ([]byte, error) {
	cues, _, err := Transcript(ctx, c, info, page, cfg, log)
	if err != nil {
		return nil, err
	}
	out, err := FreeAIChat(ctx, cfg.FreeAIModelName(), summarySystemPrompt, buildSummaryUserPrompt(info, page, transcriptForSummary(cues)))
	if err != nil {
		return nil, err
	}
	return renderSummaryMD(info, page, out), nil
}

// buildSummaryUserPrompt 拼用户消息（纯函数，单测锁定）。
func buildSummaryUserPrompt(info *bilibili.VideoInfo, page bilibili.Page, transcript string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "视频标题：《%s》", info.Title)
	if page.Index > 1 || (len(info.Pages) > 1 && page.Title != info.Title) {
		fmt.Fprintf(&b, "（分P %d：%s）", page.Index, page.Title)
	}
	if info.Owner != "" {
		fmt.Fprintf(&b, "\nUP 主：%s", info.Owner)
	}
	fmt.Fprintf(&b, "\n\n以下是带时间戳的视频文稿，请总结：\n\n%s", transcript)
	return b.String()
}

// renderSummaryMD 拼 AI 总结的 Markdown（纯函数，单测锁定格式）。
func renderSummaryMD(info *bilibili.VideoInfo, page bilibili.Page, aiOut string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — AI 视频总结\n\n", info.Title)
	if page.Index > 1 || (len(info.Pages) > 1 && page.Title != info.Title) {
		fmt.Fprintf(&b, "分P %d：%s\n\n", page.Index, page.Title)
	}
	fmt.Fprintf(&b, "> 来源：免费 AI 分析文稿 · 时长 %s · 生成于 %s\n\n",
		mmss(page.Duration), time.Now().Format("2006-01-02 15:04"))
	b.WriteString(strings.TrimSpace(aiOut))
	b.WriteString("\n")
	return []byte(b.String())
}

// SaveText 把结果写到 dir（不存在则现建），返回实际落盘路径。
func SaveText(dir, base string, data []byte) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := uniquePath(filepath.Join(dir, base))
	if err := os.WriteFile(p, appendUTF8BOM(data), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// appendUTF8BOM 给纯文本加 UTF-8 BOM：Windows 记事本靠它把文件识别成
// UTF-8，缺了会把中文当 ANSI 显示成乱码（老版本记事本尤其如此）。
func appendUTF8BOM(data []byte) []byte {
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		return data
	}
	return append([]byte{0xEF, 0xBB, 0xBF}, data...)
}

// mmss 把秒格式化成 m:ss / h:mm:ss。
func mmss(sec int) string {
	if sec < 0 {
		sec = 0
	}
	h := sec / 3600
	m := sec % 3600 / 60
	s := sec % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}
