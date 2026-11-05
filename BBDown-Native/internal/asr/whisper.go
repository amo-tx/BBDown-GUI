package asr

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"bbdown-native/internal/subtitle"
)

// ---------------------------------------------------------------------------
// whisper.cpp 本地安装
//
// 二进制来自 whisper.cpp 官方 release（whisper-bin-x64.zip，约 9MB），
// 模型来自 HuggingFace 官方仓库（默认走 hf-mirror.com 国内 CDN）。
// GitHub 直连/单线程镜像在国内很慢，所以下载统一走调用方注入的
// Fetcher（app 层传多线程分块下载器），不在这里自己发请求。

const (
	whisperTag = "b5454"
	whisperZip = "https://github.com/ggml-org/whisper.cpp/releases/download/" + whisperTag + "/whisper-bin-x64.zip"
	// whisperCLI 是解包后我们保留的可执行文件名。b5454 的 zip 里 CLI 叫
	// whisper-cli.exe（老版本叫 whisper.exe），且依赖同目录的 whisper.dll
	// 与 ggml*.dll，所以要一起解出来。
	whisperCLI = "whisper-cli.exe"
)

// Fetcher 抓文件到 dest（多候选地址由实现方负责）。
type Fetcher interface {
	Fetch(ctx context.Context, urls []string, dest string, estimate int64) (int64, error)
}

// LogFn 向 GUI 日志汇报进度。
type LogFn func(string, ...any)

// httpFetcher 是外部资源（GitHub 镜像 / HuggingFace）专用的分块并行
// 下载器。不能复用 internal/download：那个包的所有请求都带 B 站
// Referer/Origin 头（B 站 CDN 需要），而 hf-mirror 对陌生 Referer 会
// 返回伪装成完整文件的 6500 字节反盗链页（实测踩坑）。
type httpFetcher struct {
	hc       *http.Client
	Parallel int
	Chunk    int64
}

func newHTTPFetcher() *httpFetcher {
	return &httpFetcher{hc: &http.Client{Timeout: 120 * time.Second}, Parallel: 8, Chunk: 1 << 20}
}

func (f *httpFetcher) probeSize(ctx context.Context, urls []string, estimate int64) int64 {
	for _, u := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Range", "bytes=0-0")
		resp, err := f.hc.Do(req)
		if err != nil {
			continue
		}
		cr := resp.Header.Get("Content-Range") // bytes 0-0/<total>
		io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		if resp.StatusCode == http.StatusPartialContent && strings.Contains(cr, "/") {
			if i := strings.LastIndexByte(cr, '/'); i >= 0 {
				var total int64
				if _, err := fmt.Sscanf(cr[i+1:], "%d", &total); err == nil && total > 0 {
					return total
				}
			}
		}
	}
	return estimate
}

// Fetch 实现 Fetcher：分块并行 + 每块跨候选地址重试。
func (f *httpFetcher) Fetch(ctx context.Context, urls []string, dest string, estimate int64) (int64, error) {
	if len(urls) == 0 {
		return 0, errors.New("没有可用的下载地址")
	}
	total := f.probeSize(ctx, urls, estimate)
	if total <= 0 {
		return 0, errors.New("无法确定文件大小")
	}
	fh, err := os.Create(dest)
	if err != nil {
		return 0, err
	}
	if err := fh.Truncate(total); err != nil {
		fh.Close()
		return 0, err
	}

	chunk := f.Chunk
	count := int((total + chunk - 1) / chunk)
	par := f.Parallel
	if par > count {
		par = count
	}
	done := int64(0)
	var doneMu sync.Mutex
	errCh := make(chan error, count)
	sem := make(chan struct{}, par)
	for i := 0; i < count; i++ {
		idx := i
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; doneMu.Lock(); done++; doneMu.Unlock() }()
			start := int64(idx) * chunk
			end := start + chunk - 1
			if end >= total {
				end = total - 1
			}
			var lastErr error
			for round := 0; round < 3; round++ {
				for _, u := range urls {
					if ctx.Err() != nil {
						errCh <- ctx.Err()
						return
					}
					if err := f.fetchRange(ctx, u, fh, start, end); err == nil {
						errCh <- nil
						return
					} else {
						lastErr = err
					}
				}
				select {
				case <-ctx.Done():
					errCh <- ctx.Err()
					return
				case <-time.After(time.Duration(round+1) * 300 * time.Millisecond):
				}
			}
			errCh <- fmt.Errorf("块 %d/%d 下载失败: %w", idx+1, count, lastErr)
		}()
	}
	for i := 0; i < count; i++ {
		if err := <-errCh; err != nil {
			fh.Close()
			return 0, err
		}
	}
	// 进度日志：轻量报一次完成状态
	_ = done
	if err := fh.Close(); err != nil {
		return 0, err
	}
	st, err := os.Stat(dest)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// fetchRange 拉取 [start,end] 区间写入文件对应偏移。
func (f *httpFetcher) fetchRange(ctx context.Context, url string, fh *os.File, start, end int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	req.Header.Set("User-Agent", "Mozilla/5.0 BBDownNative/2.0")
	resp, err := f.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	got, err := io.Copy(io.NewOffsetWriter(fh, start), resp.Body)
	if err != nil {
		return err
	}
	if got != end-start+1 {
		return fmt.Errorf("长度不符：期望 %d 实际 %d", end-start+1, got)
	}
	return nil
}

// Home 是本地 ASR 资源目录（whisper.exe + ggml 模型 + 音频缓存）。
func Home() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "BBDown-Native", "asr")
}

// 模型候选地址：hf-mirror（国内 CDN）优先，官方 HuggingFace 兜底。
func modelURLs(model string) []string {
	const repo = "ggerganov/whisper.cpp/resolve/main/"
	p := "ggml-" + model + ".bin"
	return []string{"https://hf-mirror.com/" + repo + p, "https://huggingface.co/" + repo + p}
}

// zipURLs 返回 whisper 二进制 zip 的候选地址。国内镜像可用性按小时波动，
// 全部列上，下载器每个分块会逐个尝试。
func zipURLs() []string {
	return []string{
		"https://gh-proxy.com/" + whisperZip,
		"https://ghproxy.net/" + whisperZip,
		whisperZip,
	}
}

// modelEstimate 是常见 ggml 模型的体积（仅用于下载器预估，探到真实
// Content-Length 后就不依赖它）。
var modelEstimate = map[string]int64{
	"tiny": 77 << 20, "base": 148 << 20, "small": 486 << 20,
	"medium": 1530 << 20, "large-v3-turbo": 809 << 20, "large-v3": 3100 << 20,

	"tiny-q5_1": 31 << 20, "base-q5_1": 58 << 20, "small-q5_1": 182 << 20,
	"medium-q5_0": 514 << 20, "large-v3-turbo-q5_0": 574 << 20, "large-v3-q5_0": 1620 << 20,
}

// quantizedName 把用户填的模型名映射到默认实际下载的量化版。
//
// 实测（Ryzen 7 6800H，60 秒中文样本）：量化版与全精度速度几乎一样
// （瓶颈是算力不是内存带宽），但体积/内存减半以上、下载快得多，没有理由
// 不用。config 里显式填 "xxx-fp16" 可以强制全精度。
func quantizedName(model string) string {
	if strings.HasSuffix(model, "-fp16") {
		return strings.TrimSuffix(model, "-fp16")
	}
	if strings.Contains(model, "-q5") {
		return model // 已经是量化名，原样
	}
	switch model {
	case "tiny":
		return "tiny-q5_1"
	case "base":
		return "base-q5_1"
	case "small":
		return "small-q5_1"
	case "medium":
		return "medium-q5_0"
	case "large-v3-turbo":
		return "large-v3-turbo-q5_0"
	case "large-v3":
		return "large-v3-q5_0"
	default:
		return model
	}
}

// Ensure 确保 whisper.exe 与 ggml 模型就位，返回两者路径。model 传
// config 里的原始名（如 small），内部自动映射到量化版（small-q5_1）。
// log 用于向 GUI 日志汇报进度（可为 nil）。
func Ensure(ctx context.Context, model string, log LogFn) (exePath, modelPath string, err error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	model = quantizedName(model)
	dl := newHTTPFetcher()
	dir := Home()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	exePath = filepath.Join(dir, whisperCLI)
	modelPath = filepath.Join(dir, "ggml-"+model+".bin")

	if _, statErr := os.Stat(exePath); statErr != nil {
		log("首次使用本地转写：下载 whisper 主程序（约 9MB）…")
		if err := installWhisperExe(ctx, dl, dir); err != nil {
			return "", "", fmt.Errorf("下载 whisper 主程序失败: %w", err)
		}
		log("✓ whisper 主程序安装完成")
	} else {
		log("whisper 主程序已就绪")
	}
	if _, statErr := os.Stat(modelPath); statErr != nil {
		log("首次使用本地转写：下载语音模型 ggml-%s.bin（数十至数百 MB）…", model)
		if _, err := dl.Fetch(ctx, modelURLs(model), modelPath, modelEstimate[model]); err != nil {
			return "", "", fmt.Errorf("下载语音模型失败: %w", err)
		}
		log("✓ 语音模型下载完成")
	} else {
		log("语音模型已就绪：%s", filepath.Base(modelPath))
	}
	return exePath, modelPath, nil
}

// installWhisperExe 下载 zip 并解出 whisper-cli.exe 及其运行所需 DLL。
func installWhisperExe(ctx context.Context, dl Fetcher, dir string) error {
	zipPath := filepath.Join(dir, "whisper-bin-x64.zip")
	if _, err := dl.Fetch(ctx, zipURLs(), zipPath, 9_000_000); err != nil {
		return err
	}
	defer os.Remove(zipPath)

	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()

	// 只解运行必需的文件：CLI（老/新版本名不同）+ 全部 ggml/whisper DLL。
	need := func(base string) bool {
		switch {
		case base == "whisper-cli.exe", base == "whisper.exe":
			return true
		case base == "whisper.dll", base == "ggml.dll", base == "ggml-base.dll":
			return true
		case strings.HasPrefix(base, "ggml-cpu-"):
			return true
		}
		return false
	}
	found := false
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		base := filepath.Base(zf.Name)
		if !need(base) {
			continue
		}
		target := base
		if target == "whisper.exe" { // 老版 zip：统一叫 whisper-cli.exe
			target = whisperCLI
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		dst, err := os.CreateTemp(dir, "asr-*.part")
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(dst, rc)
		rc.Close()
		if err == nil {
			err = dst.Close()
		} else {
			dst.Close()
		}
		if err != nil {
			os.Remove(dst.Name())
			return err
		}
		dstPath := filepath.Join(dir, target)
		os.Remove(dstPath)
		if err := os.Rename(dst.Name(), dstPath); err != nil {
			os.Remove(dst.Name())
			return err
		}
		if target == whisperCLI {
			found = true
		}
	}
	if !found {
		return fmt.Errorf("压缩包里没有 whisper-cli.exe")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 转写执行

var segRe = regexp.MustCompile(
	`^\[(\d+):(\d{2}):(\d{2})\.(\d{3})\s*-->\s*(\d+):(\d{2}):(\d{2})\.(\d{3})\]\s?(.*)$`)

// parseSegments 解析 whisper.cpp 的 stdout：每行
// 「[hh:mm:ss.mmm --> hh:mm:ss.mmm] 文本」转成字幕条。
func parseSegments(out string) []subtitle.Cue {
	var cues []subtitle.Cue
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		m := segRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		cues = append(cues, subtitle.Cue{
			From: hmsToSec(m[1], m[2], m[3], m[4]),
			To:   hmsToSec(m[5], m[6], m[7], m[8]),
			Text: strings.TrimSpace(m[9]),
		})
	}
	return cues
}

func hmsToSec(h, m, s, ms string) float64 {
	hi, _ := strconv.Atoi(h)
	mi, _ := strconv.Atoi(m)
	si, _ := strconv.Atoi(s)
	msi, _ := strconv.Atoi(ms)
	return float64(hi*3600+mi*60+si) + float64(msi)/1000
}

// Threads 是转写用的线程数。whisper-cli 默认只用 4 线程，保守得很；
// 实测（Ryzen 7 6800H，60 秒样本）拉到 8 线程能快 33%，16 线程（超线程）
// 几乎不再提升，超过物理核就是白烧电。所以取 min(逻辑核, 8)。
func Threads() int {
	n := runtime.NumCPU()
	if n > 8 {
		n = 8
	}
	if n < 1 {
		n = 1
	}
	return n
}

// Transcribe 跑 whisper.exe 转写一个 16kHz WAV，返回带时间戳的字幕条。
// lang 为空表示让 whisper 自动识别语言。
func Transcribe(ctx context.Context, exePath, modelPath, wavPath, lang string) ([]subtitle.Cue, error) {
	args := []string{"-m", modelPath, "-f", wavPath, "-pp",
		"-t", strconv.Itoa(Threads())}
	if lang != "" {
		args = append(args, "-l", lang)
	}
	cmd := exec.CommandContext(ctx, exePath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("启动 whisper 失败: %w", err)
	}
	var out strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	waitErr := cmd.Wait()
	cues := parseSegments(out.String())
	if waitErr != nil && len(cues) == 0 {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return nil, fmt.Errorf("whisper 转写失败: %v%s", waitErr, tailMsg(msg))
	}
	if len(cues) == 0 {
		return nil, errors.New("whisper 没有输出任何语音内容（可能整段无语音或全是背景音乐）")
	}
	return cues, nil
}

func tailMsg(msg string) string {
	if msg == "" {
		return ""
	}
	return "：" + msg
}
