package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bbdown-native/internal/bilibili"
	"bbdown-native/internal/download"
	"bbdown-native/internal/mux"
)

// Hooks 是任务向界面回报的回调。**全部在后台 goroutine 里被调用**，
// 界面实现时必须自己切回 UI 线程（win32 的控件只能在创建它的线程上操作）。
type Hooks struct {
	Stage    func(stage string)
	Log      func(line string)
	Progress func(JobProgress)
	// Output 在每次封装成功后回报成品路径。界面据此列出「本次产物」，
	// 不必自己去猜文件名规则。
	Output func(path string)
}

func (h Hooks) output(path string) {
	if h.Output != nil {
		h.Output(path)
	}
}

func (h Hooks) stage(s string) {
	if h.Stage != nil {
		h.Stage(s)
	}
}

func (h Hooks) log(format string, args ...any) {
	if h.Log != nil {
		h.Log(fmt.Sprintf(format, args...))
	}
}

func (h Hooks) progress(p JobProgress) {
	if h.Progress != nil {
		h.Progress(p)
	}
}

// JobProgress 是整个任务的合并进度（视频 + 音频算在一起）。
type JobProgress struct {
	Label string  // "视频" / "音频" / "封装"
	Done  int64   // 已下载字节
	Total int64   // 总字节
	Speed float64 // 字节/秒
	ETA   time.Duration
}

// Resolved 是一个地址的解析结果，界面据此展示与让用户选择。
type Resolved struct {
	Info  *bilibili.VideoInfo
	Pages []bilibili.Page // 全部分P
	Avail []int           // 该稿件可用的画质编号（降序）
}

// DisplayName 返回界面上的标题行。
func (r *Resolved) DisplayName() string {
	if r.Info == nil {
		return ""
	}
	name := r.Info.Title
	if r.Info.Owner != "" {
		name += "  —  " + r.Info.Owner
	}
	return name
}

// Options 控制一次下载。
type Options struct {
	PageSpec string // "1,3-5"，空表示全部
	OutDir   string // 空则用配置里的
	Quality  int    // 0 表示能拿多高拿多高
	Codec    string // "hevc" / "avc" / "av1"，空则用配置
	Parallel int    // 0 则用配置
}

// Runner 把「解析 → 选流 → 下载 → 封装」串起来。
type Runner struct {
	cfg   *Config
	hooks Hooks

	mu     sync.Mutex
	client *bilibili.Client
}

// NewRunner 创建 Runner，并把配置里的凭证装进客户端。
func NewRunner(cfg *Config, hooks Hooks) *Runner {
	c := bilibili.NewClient()
	if cfg != nil {
		c.Cookie = cfg.Cookie
	}
	return &Runner{cfg: cfg, hooks: hooks, client: c}
}

// Client 暴露底层客户端，界面需要在别处校验 cookie。
func (r *Runner) Client() *bilibili.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.client
}

// SetCredentials 在登录成功后立即换上新凭证，不必等下次启动。
func (r *Runner) SetCredentials(cookie, token string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cookie != "" {
		r.client.Cookie = cookie
	}
	_ = token // access_token 目前只做落盘留存，取流仍走 cookie
}

// ---------------------------------------------------------------------------
// 解析

// Resolve 解析分享文案里的全部地址。
//
// 返回的每个条目都已带上可用画质，界面可以直接切换画质而不用重新请求。
func (r *Runner) Resolve(ctx context.Context, text string) ([]*Resolved, error) {
	targets := bilibili.ExtractTargets(text)
	if len(targets) == 0 {
		return nil, errors.New("没有识别到 B 站地址，请粘贴视频链接或分享文案")
	}
	r.hooks.log("识别到 %d 个地址，正在解析…", len(targets))

	targets = r.client.ResolveTargets(ctx, targets)

	var out []*Resolved
	var firstErr error
	for _, t := range targets {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		info, err := r.client.VideoInfo(ctx, t)
		if err != nil {
			r.hooks.log("解析失败 %s：%v", t.Display(), err)
			if firstErr == nil {
				firstErr = fmt.Errorf("解析 %s 失败：%w", t.Display(), err)
			}
			continue
		}
		item := &Resolved{Info: info, Pages: info.Pages}
		// 顺手取一次流，只是为了拿到可选画质列表（成本很低，且能提前暴露权限问题）。
		page := info.Pages[info.DefaultPage-1]
		if pu, err := r.client.PlayURL(ctx, info, page); err == nil {
			item.Avail = sortedQualities(pu)
		} else {
			r.hooks.log("取流失败（可能是未登录或需要会员）：%v", err)
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, errors.New("没有解析出任何可下载的内容")
	}
	return out, nil
}

// sortedQualities 汇总可用画质，降序去重。
func sortedQualities(pu *bilibili.PlayURL) []int {
	seen := map[int]bool{}
	for _, v := range pu.Video {
		seen[v.ID] = true
	}
	order := []int{127, 126, 125, 120, 116, 112, 80, 64, 32, 16, 6}
	var out []int
	for _, q := range order {
		if seen[q] {
			out = append(out, q)
		}
	}
	// 接口出现了未知编号也带上，免得漏掉
	for q := range seen {
		found := false
		for _, o := range out {
			if o == q {
				found = true
				break
			}
		}
		if !found {
			out = append(out, q)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 下载

// Download 下载并封装一个已解析的条目。
func (r *Runner) Download(ctx context.Context, item *Resolved, opt Options) error {
	if item == nil || item.Info == nil {
		return errors.New("没有可下载的内容")
	}

	outDir := opt.OutDir
	if strings.TrimSpace(outDir) == "" {
		outDir = r.cfg.DownloadDir
	}
	if outDir == "" {
		return errors.New("没有设置下载目录")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("创建下载目录失败：%w", err)
	}

	pages, err := item.Info.PickPages(opt.PageSpec)
	if err != nil {
		return err
	}
	codec := opt.Codec
	if codec == "" {
		codec = r.cfg.CodecOrder
	}
	parallel := opt.Parallel
	if parallel == 0 {
		parallel = r.cfg.Parallel
	}
	quality := opt.Quality
	if quality == 0 {
		quality = r.cfg.Quality
	}

	r.hooks.log("目标目录：%s", outDir)
	r.hooks.log("共 %d 个分P，编码偏好 %s", len(pages), codec)

	var firstErr error
	for i, page := range pages {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.hooks.stage(fmt.Sprintf("(%d/%d) %s", i+1, len(pages), truncateRunes(page.Title, 40)))
		if err := r.downloadOne(ctx, item, page, outDir, len(pages) > 1,
			quality, codec, parallel); err != nil {
			r.hooks.log("✗ %s：%v", page.Title, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
	}
	return firstErr
}

func (r *Runner) downloadOne(ctx context.Context, item *Resolved, page bilibili.Page,
	outDir string, multiPage bool, quality int, codec string, parallel int) error {

	info := item.Info

	r.hooks.stage("取流")
	pu, err := r.client.PlayURL(ctx, info, page)
	if err != nil {
		return fmt.Errorf("取流失败：%w", err)
	}

	vStream, err := pu.SelectVideo(priorityFor(quality, item.Avail), codecList(codec))
	if err != nil {
		return err
	}
	aStream, aErr := pu.SelectAudio()

	r.hooks.log("画质 %s（qn=%d）· 编码 %s · %dx%d · %s",
		bilibili.QualityName(vStream.ID), vStream.ID,
		bilibili.CodecName(vStream.Codecs),
		vStream.Width, vStream.Height, vStream.FrameRate)
	if aErr == nil {
		r.hooks.log("音频 %s · %d kbps", aStream.Codecs, aStream.Bandwidth/1000)
	} else {
		r.hooks.log("没有音轨，只封装视频")
	}

	// 临时文件放在目标目录的同级，跨盘符复制会拖慢封装。
	work, err := os.MkdirTemp(outDir, ".bbtmp-")
	if err != nil {
		return fmt.Errorf("创建临时目录失败：%w", err)
	}
	if !r.cfg.KeepTemp {
		defer os.RemoveAll(work)
	}

	// 先探一次真实体积，好把两条流合并成一个进度条。
	vURLs := candidateURLs(vStream)
	r.hooks.stage("探测体积")
	vSize := r.probeSize(ctx, vURLs, vStream.EstimateSize(pu.Duration))
	aSize := int64(0)
	var aURLs []string
	if aErr == nil {
		aURLs = candidateURLs(aStream)
		aSize = r.probeSize(ctx, aURLs, aStream.EstimateSize(pu.Duration))
	}
	total := vSize + aSize

	dl := download.New(r.client)
	dl.Parallel = parallel
	dl.OnLog = func(s string) { r.hooks.log("%s", s) }

	videoPath := filepath.Join(work, "video.m4s")
	audioPath := filepath.Join(work, "audio.m4s")

	var vDone int64
	r.hooks.progress(JobProgress{Label: "视频", Done: 0, Total: total})
	dl.OnProgress = func(p download.Progress) {
		vDone = p.Done
		r.hooks.progress(JobProgress{Label: "视频", Done: p.Done, Total: total, Speed: p.Speed, ETA: etaOf(total-p.Done, p.Speed)})
	}
	r.hooks.stage("下载视频流")
	if _, err := dl.Fetch(ctx, vURLs, videoPath, vSize); err != nil {
		return fmt.Errorf("下载视频流失败：%w", err)
	}

	if aErr == nil {
		dl.OnProgress = func(p download.Progress) {
			r.hooks.progress(JobProgress{Label: "音频", Done: vDone + p.Done, Total: total, Speed: p.Speed, ETA: etaOf(total-vDone-p.Done, p.Speed)})
		}
		r.hooks.stage("下载音频流")
		if _, err := dl.Fetch(ctx, aURLs, audioPath, aSize); err != nil {
			return fmt.Errorf("下载音频流失败：%w", err)
		}
	} else {
		audioPath = ""
	}

	dest := outputPath(outDir, info, page, multiPage)
	// 多 P 稿件会输出到子目录，封装器不做建目录的事。
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("创建输出目录失败：%w", err)
	}
	r.hooks.stage("封装")
	r.hooks.progress(JobProgress{Label: "封装", Done: total, Total: total})
	r.hooks.log("正在封装 %s", filepath.Base(dest))

	res, err := mux.Mux(videoPath, audioPath, dest)
	if err != nil {
		return fmt.Errorf("封装失败：%w", err)
	}
	r.hooks.log("✓ 完成 %s · %s · %d 帧 / %d 帧音频",
		filepath.Base(dest), humanDuration(res.DurationMs), res.VideoSamples, res.AudioSamples)
	r.hooks.output(dest)
	return nil
}

// priorityFor 组出画质优先级：选中的排最前，其余从高到低兜底。
func priorityFor(quality int, avail []int) []int {
	if quality == 0 {
		return nil
	}
	out := []int{quality}
	for _, q := range avail {
		if q != quality {
			out = append(out, q)
		}
	}
	for _, q := range []int{127, 126, 125, 120, 116, 112, 80, 64, 32, 16, 6} {
		dup := false
		for _, o := range out {
			if o == q {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, q)
		}
	}
	return out
}

func codecList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// candidateURLs = 主地址 + 备用地址 + 镜像主机。
// 部分网络下原 CDN 会被 403，换源是刚需。
func candidateURLs(s bilibili.Stream) []string {
	out := s.URLs()
	if m := bilibili.MirrorURLs(s.BaseURL); len(m) > 1 {
		out = append(out, m[1:]...)
	}
	return out
}

// probeSize 探真实字节数；失败则退回按码率估算。
func (r *Runner) probeSize(ctx context.Context, urls []string, estimate int64) int64 {
	for _, u := range urls {
		if ctx.Err() != nil {
			break
		}
		if n, err := r.client.StreamSize(ctx, u); err == nil && n > 0 {
			return n
		}
	}
	if estimate > 0 {
		return estimate
	}
	return 0
}

func etaOf(remain int64, speed float64) time.Duration {
	if speed <= 0 || remain <= 0 {
		return 0
	}
	return time.Duration(float64(remain)/speed) * time.Second
}

// ---------------------------------------------------------------------------
// 输出路径

// outputPath 决定成品文件的位置。
//
// 单 P 稿件：<目录>/<标题>.mp4
// 多 P 稿件：<目录>/<标题>/<序号>. <分P标题>.mp4
func outputPath(outDir string, info *bilibili.VideoInfo, page bilibili.Page, multiPage bool) string {
	title := info.DisplayTitle()
	if !multiPage {
		return uniquePath(filepath.Join(outDir, title+".mp4"))
	}
	sub := filepath.Join(outDir, title)
	part := bilibili.SanitizeFileName(page.Title)
	name := fmt.Sprintf("%02d. %s.mp4", page.Index, part)
	return uniquePath(filepath.Join(sub, name))
}

// uniquePath 如果目标已存在就加 " (2)"、" (3)" 后缀，避免覆盖已有文件。
func uniquePath(p string) string {
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return p
}

// ---------------------------------------------------------------------------
// 小工具

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func humanDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}
