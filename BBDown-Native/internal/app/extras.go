package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/danmaku"
	"bbdown-native/internal/subtitle"
)

// extrasPlan 是一次下载里要产出的附加内容（已把配置与本次覆盖合并好）。
type extrasPlan struct {
	danmaku  string // ass / xml / both / off
	subtitle string // srt / json / both / off
	cover    bool
}

// planExtras 合并配置与本次下载的覆盖项。
//
// 空字符串表示「跟随配置」，显式写 "off" 才是关掉 —— 这样界面上的复选框
// 不勾选时能明确表达「这次不要」，而不会退回到配置里的开。
func (r *Runner) planExtras(opt Options) extrasPlan {
	p := extrasPlan{
		danmaku:  r.cfg.Danmaku,
		subtitle: r.cfg.Subtitle,
		cover:    r.cfg.SaveCover,
	}
	if opt.Danmaku != "" {
		p.danmaku = opt.Danmaku
	}
	if opt.Subtitle != "" {
		p.subtitle = opt.Subtitle
	}
	switch strings.ToLower(strings.TrimSpace(opt.SaveCover)) {
	case "on", "true":
		p.cover = true
	case "off", "false":
		p.cover = false
	}
	return p
}

// wants 判断一个枚举值是否包含某种产出。
func wants(mode, one string) bool {
	return mode == one || mode == ExtraBoth
}

// ---------------------------------------------------------------------------
// 封面

// writeCover 把封面图存到 outDir。
//
// 封面是「一个稿件一张」，所以放在 Download 里按稿件写一次，
// 而不是跟着每个分P 各写一份。
func (r *Runner) writeCover(ctx context.Context, info *bilibili.VideoInfo, outDir string, multiPage bool) {
	if info.Cover == "" {
		return
	}
	name := info.DisplayTitle() + ".jpg"
	if multiPage {
		// 多P 会输出到子目录，封面放进去当封面墙。
		name = "cover.jpg"
		outDir = filepath.Join(outDir, info.DisplayTitle())
	}
	dest := uniquePath(filepath.Join(outDir, name))

	data, err := r.client.FetchBytes(ctx, httpsCover(info.Cover))
	if err != nil {
		r.hooks.log("! 封面下载失败：%v", err)
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		r.hooks.log("! 封面目录创建失败：%v", err)
		return
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		r.hooks.log("! 封面写入失败：%v", err)
		return
	}
	r.hooks.log("✓ 封面 %s（%s）", filepath.Base(dest), humanSize(int64(len(data))))
	r.hooks.output(dest)
}

// httpsCover 把封面的 http 地址升到 https。
//
// B 站的图片床（i0/i1.hdslb.com）返回的是明文 http，部分网络环境会拦，
// 而它本身是支持 https 的。
func httpsCover(u string) string {
	switch {
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	case strings.HasPrefix(u, "http://"):
		return "https://" + strings.TrimPrefix(u, "http://")
	default:
		return u
	}
}

// ---------------------------------------------------------------------------
// 弹幕与字幕

// writeExtras 落盘弹幕与字幕。任何一个失败都只记日志，不影响视频本身。
//
// dest 是已封装好的 mp4 路径，附加文件用它的同名主干。
func (r *Runner) writeExtras(ctx context.Context, info *bilibili.VideoInfo, page bilibili.Page,
	dest string, videoW, videoH int, plan extrasPlan) {

	base := strings.TrimSuffix(dest, filepath.Ext(dest))

	if plan.danmaku != ExtraOff {
		r.writeDanmaku(ctx, page.Cid, base, videoW, videoH, plan.danmaku)
	}
	if plan.subtitle != ExtraOff {
		r.writeSubtitle(ctx, info, page.Cid, base, plan.subtitle)
	}
}

func (r *Runner) writeDanmaku(ctx context.Context, cid int64, base string, videoW, videoH int, mode string) {
	raw, err := r.client.FetchDanmakuXML(ctx, cid)
	if err != nil {
		r.hooks.log("! 弹幕下载失败：%v", err)
		return
	}

	if wants(mode, "xml") {
		p := base + ".xml"
		if err := os.WriteFile(p, raw, 0o644); err != nil {
			r.hooks.log("! 弹幕 XML 写入失败：%v", err)
		} else {
			r.hooks.log("✓ 弹幕 %s（%s）", filepath.Base(p), humanSize(int64(len(raw))))
			r.hooks.output(p)
		}
	}

	if !wants(mode, "ass") {
		return
	}
	items, err := danmaku.ParseXML(raw)
	if err != nil {
		r.hooks.log("! 弹幕解析失败：%v", err)
		return
	}
	if len(items) == 0 {
		r.hooks.log("· 这个分P 没有弹幕")
		return
	}
	ass, placed := danmaku.RenderASSStats(items, danmaku.Options{
		PlayResX: videoW,
		PlayResY: videoH,
	})
	p := base + ".ass"
	if err := os.WriteFile(p, ass, 0o644); err != nil {
		r.hooks.log("! 弹幕 ASS 写入失败：%v", err)
		return
	}
	if placed < len(items) {
		r.hooks.log("✓ 弹幕 %s（%d 条 → 同屏画下 %d 条 / %s）",
			filepath.Base(p), len(items), placed, humanSize(int64(len(ass))))
	} else {
		r.hooks.log("✓ 弹幕 %s（%d 条 / %s）",
			filepath.Base(p), len(items), humanSize(int64(len(ass))))
	}
	r.hooks.output(p)
}

func (r *Runner) writeSubtitle(ctx context.Context, info *bilibili.VideoInfo, cid int64, base, mode string) {
	list, err := r.client.SubtitleList(ctx, info, cid)
	if err != nil {
		// 「要登录才有字幕」是常见情况，不该显示成错误吓唬用户。
		if errors.Is(err, bilibili.ErrSubtitleNeedsLogin) {
			r.hooks.log("· 该稿件有字幕，但 B 站要求登录后才能取（登录后重下即可）")
			return
		}
		r.hooks.log("! 字幕列表获取失败：%v", err)
		return
	}
	if len(list) == 0 {
		return
	}

	// 多语言时给文件名带上语言标签，只有一条时就不加，保持文件名干净。
	multi := len(list) > 1
	var got int
	for _, meta := range list {
		if ctx.Err() != nil {
			return
		}
		raw, err := r.client.FetchSubtitleJSON(ctx, meta.URL)
		if err != nil {
			r.hooks.log("! 字幕下载失败（%s）：%v", meta.LanDoc, err)
			continue
		}
		track, err := subtitle.ParseJSON(raw)
		if err != nil {
			r.hooks.log("! 字幕解析失败（%s）：%v", meta.LanDoc, err)
			continue
		}
		// 元信息以列表接口为准：字幕文件本身不带语言字段。
		track.Lang = meta.Lan
		track.LangDoc = meta.LanDoc
		track.AI = meta.AI

		suffix := ""
		if multi {
			suffix = "." + track.Label()
		}
		label := meta.LanDoc
		if label == "" {
			label = meta.Lan
		}

		if wants(mode, "srt") {
			p := base + suffix + ".srt"
			if err := os.WriteFile(p, track.SRT(), 0o644); err != nil {
				r.hooks.log("! 字幕 SRT 写入失败：%v", err)
			} else {
				r.hooks.log("✓ 字幕 %s（%s · %d 条）", filepath.Base(p), label, len(track.Cues))
				r.hooks.output(p)
			}
		}
		if wants(mode, "json") {
			p := base + suffix + ".json"
			if err := os.WriteFile(p, track.Raw(), 0o644); err != nil {
				r.hooks.log("! 字幕 JSON 写入失败：%v", err)
			} else {
				r.hooks.log("✓ 字幕 %s（%s）", filepath.Base(p), label)
				r.hooks.output(p)
			}
		}
		got++
	}
	if got == 0 {
		r.hooks.log("! 字幕都没能下载下来")
	}
}

// humanSize 把字节数变成人看的单位。
func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
