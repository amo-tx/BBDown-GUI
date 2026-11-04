package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/subtitle"
)

// ---------------------------------------------------------------------------
// 音频转文字 & AI 总结
//
// 两个「原生智能」功能，都借 B 站自己的大模型能力：
//   - 转文字 = AI 字幕（音频转写）拼成纯文稿；
//   - AI 总结 = view/conclusion 接口的摘要 + 章节大纲。
// 本程序是单文件零依赖定位，不内置 ASR、不接外部大模型，这两条路
// 是唯一不需要 API key 也不需要下模型的实现方式。

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

// FetchTranscript 取指定分P 的文稿：返回 TXT 正文与字幕条数。
func FetchTranscript(ctx context.Context, c *bilibili.Client, info *bilibili.VideoInfo, page bilibili.Page) ([]byte, int, error) {
	list, err := c.SubtitleList(ctx, info, page.Cid)
	if err != nil {
		if errors.Is(err, bilibili.ErrSubtitleNeedsLogin) {
			return nil, 0, fmt.Errorf("该稿件有 AI 字幕，但 B 站要求登录后才能取（请先登录）")
		}
		return nil, 0, err
	}
	if len(list) == 0 {
		return nil, 0, fmt.Errorf("该视频没有可用字幕，无法转文字")
	}
	meta := pickSubtitleTrack(list)
	raw, err := c.FetchSubtitleJSON(ctx, meta.URL)
	if err != nil {
		return nil, 0, err
	}
	track, err := subtitle.ParseJSON(raw)
	if err != nil {
		return nil, 0, err
	}
	track.Lang = meta.Lan
	track.LangDoc = meta.LanDoc
	track.AI = meta.AI
	return renderTranscript(info, page, track, len(track.Cues)), len(track.Cues), nil
}

// renderTranscript 拼文稿正文（纯函数，单测锁定格式）。
func renderTranscript(info *bilibili.VideoInfo, page bilibili.Page, track *subtitle.Track, cueCount int) []byte {
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
	label := track.LangDoc
	if label == "" {
		label = track.Lang
	}
	tag := "字幕文稿"
	if track.AI {
		tag = "文稿来自 B 站 AI 字幕（音频转写）"
	}
	fmt.Fprintf(&b, "%s · %s · %d 条\n\n", tag, label, cueCount)
	b.WriteString(track.Text())
	b.WriteByte('\n')
	return []byte(b.String())
}

// FetchSummaryText 取指定分P 的 AI 总结：返回 Markdown 正文。
func FetchSummaryText(ctx context.Context, c *bilibili.Client, info *bilibili.VideoInfo, page bilibili.Page) ([]byte, error) {
	res, err := c.AISummary(ctx, info, page.Cid)
	if err != nil {
		return nil, err
	}
	return renderSummaryMD(info, page, res), nil
}

// renderSummaryMD 拼 AI 总结的 Markdown（纯函数，单测锁定格式）。
func renderSummaryMD(info *bilibili.VideoInfo, page bilibili.Page, res *bilibili.AISummaryResult) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — AI 视频总结\n\n", info.Title)
	if page.Index > 1 || (len(info.Pages) > 1 && page.Title != info.Title) {
		fmt.Fprintf(&b, "分P %d：%s\n\n", page.Index, page.Title)
	}
	fmt.Fprintf(&b, "> 来源：B 站 AI 视频总结 · 时长 %s · 生成于 %s\n\n",
		mmss(page.Duration), time.Now().Format("2006-01-02 15:04"))
	if strings.TrimSpace(res.Summary) != "" {
		b.WriteString("## 摘要\n\n")
		b.WriteString(strings.TrimSpace(res.Summary))
		b.WriteString("\n\n")
	}
	if len(res.Outline) > 0 {
		b.WriteString("## 大纲\n\n")
		for _, part := range res.Outline {
			head := part.Part
			if head == "" {
				head = part.Title
			}
			if head != "" {
				fmt.Fprintf(&b, "**%s**\n\n", head)
			}
			for _, pt := range part.Content {
				if strings.TrimSpace(pt.Content) == "" {
					continue
				}
				fmt.Fprintf(&b, "- %s %s\n", mmss(int(pt.Timestamp)), strings.TrimSpace(pt.Content))
			}
			b.WriteString("\n")
		}
	}
	return []byte(strings.TrimSpace(b.String()) + "\n")
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
