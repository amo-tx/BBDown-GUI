// Package download 负责把 DASH 流拉到本地。
//
// B 站的 DASH 流是「一个地址返回完整文件」（不是 HLS 那种一堆小分片），
// 所以这里的策略是：探测总长度后按 Range 切成若干块并发拉取，
// 顺便用旁路状态文件记录已完成块，实现跨次运行的断点续传。
package download

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"bbdown-native/internal/bilibili"
)

// 单块大小与默认并发数。块太大则并发收益下降，太小则请求开销变高。
const (
	defaultChunkSize = 2 << 20 // 2 MiB
	defaultParallel  = 6
	maxRetryRound    = 3
)

// Progress 是一次进度快照。
type Progress struct {
	Done  int64         // 已下载字节
	Total int64         // 总字节（-1 表示未知）
	Speed float64       // 字节/秒
	ETA   time.Duration // 预计剩余
}

// Downloader 下载 DASH 流。
type Downloader struct {
	Client     *bilibili.Client
	Parallel   int
	ChunkSize  int64
	OnProgress func(Progress)
	OnLog      func(string)
}

// New 创建下载器。
func New(c *bilibili.Client) *Downloader {
	return &Downloader{
		Client:    c,
		Parallel:  defaultParallel,
		ChunkSize: defaultChunkSize,
	}
}

func (d *Downloader) log(format string, args ...any) {
	if d.OnLog != nil {
		d.OnLog(fmt.Sprintf(format, args...))
	}
}

// sidecar 记录分块完成情况，用来断点续传。
type sidecar struct {
	Total      int64  `json:"total"`
	ChunkSize  int64  `json:"chunkSize"`
	TotalCount int    `json:"chunkCount"`
	Done       []bool `json:"done"`
}

func sidecarPath(dest string) string { return dest + ".bbdl" }

// Fetch 把 urls（主地址 + 备用镜像）拉到 dest。
//
// urls 里任一地址都指向同一份内容，所以中途换源不影响续传。
func (d *Downloader) Fetch(ctx context.Context, urls []string, dest string, estimate int64) (int64, error) {
	if len(urls) == 0 {
		return 0, errors.New("没有可用的下载地址")
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return 0, err
	}

	total, err := d.probeSize(ctx, urls, estimate)
	if err != nil {
		return 0, err
	}
	if total <= 0 {
		return d.fetchSequential(ctx, urls, dest)
	}

	if d.ChunkSize <= 0 {
		d.ChunkSize = defaultChunkSize
	}
	count := int((total + d.ChunkSize - 1) / d.ChunkSize)

	st := d.loadSidecar(dest, total, count)
	if st == nil {
		// 状态文件与本次任务不匹配（长度变了或尺寸不同），重新开始。
		st = &sidecar{Total: total, ChunkSize: d.ChunkSize, TotalCount: count, Done: make([]bool, count)}
		_ = os.Remove(dest)
	}

	f, err := os.OpenFile(dest, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err := f.Truncate(total); err != nil {
		return 0, err
	}

	// 统计已完成字节（续传时非 0）
	var done int64
	for i, ok := range st.Done {
		if ok {
			done += d.chunkLen(total, i, count)
		}
	}

	// 每一块可能是「部分完成」的：简单起见按整块重下（块只有 2MiB，代价可接受）。
	todo := make([]int, 0, count)
	for i, ok := range st.Done {
		if !ok {
			todo = append(todo, i)
		}
	}

	par := d.Parallel
	if par <= 0 {
		par = defaultParallel
	}

	var (
		mu        sync.Mutex
		next      int
		firstErr  error
		stopFlag  atomic.Bool
		doneBytes = done
		started   = time.Now()
		baseDone  = done
	)

	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cur := atomic.LoadInt64(&doneBytes)
				el := time.Since(started).Seconds()
				var speed float64
				if el > 0.2 {
					speed = float64(cur-baseDone) / el
				}
				p := Progress{Done: cur, Total: total, Speed: speed}
				if speed > 1 {
					remain := float64(total-cur) / speed
					p.ETA = time.Duration(remain * float64(time.Second))
				}
				d.report(p)
				if cur >= total {
					return
				}
			}
		}
	}()

	var wg sync.WaitGroup
	for w := 0; w < par; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(todo) || stopFlag.Load() {
					mu.Unlock()
					return
				}
				idx := todo[next]
				next++
				mu.Unlock()

				if err := d.fetchChunk(ctx, urls, f, total, idx, count); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					stopFlag.Store(true)
					mu.Unlock()
					return
				}
				atomic.AddInt64(&doneBytes, d.chunkLen(total, idx, count))

				mu.Lock()
				st.Done[idx] = true
				_ = d.saveSidecar(dest, st)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	<-progressDone

	if ctx.Err() != nil {
		return atomic.LoadInt64(&doneBytes), ctx.Err()
	}
	if firstErr != nil {
		return atomic.LoadInt64(&doneBytes), firstErr
	}

	d.report(Progress{Done: total, Total: total})
	_ = os.Remove(sidecarPath(dest))
	return total, nil
}

// chunkLen 返回第 i 块应有的字节数。
func (d *Downloader) chunkLen(total int64, i, count int) int64 {
	start := int64(i) * d.ChunkSize
	end := start + d.ChunkSize
	if end > total {
		end = total
	}
	if start > total {
		return 0
	}
	return end - start
}

func (d *Downloader) report(p Progress) {
	if d.OnProgress != nil {
		d.OnProgress(p)
	}
}

func (d *Downloader) loadSidecar(dest string, total int64, count int) *sidecar {
	raw, err := os.ReadFile(sidecarPath(dest))
	if err != nil {
		return nil
	}
	var st sidecar
	if json.Unmarshal(raw, &st) != nil {
		return nil
	}
	if st.Total != total || st.TotalCount != count || len(st.Done) != count || st.ChunkSize != d.ChunkSize {
		return nil
	}
	return &st
}

func (d *Downloader) saveSidecar(dest string, st *sidecar) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := sidecarPath(dest) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, sidecarPath(dest))
}

// probeSize 探测流的总长度：先试候选地址的 Content-Range，失败则退回接口给的码率估算。
func (d *Downloader) probeSize(ctx context.Context, urls []string, estimate int64) (int64, error) {
	var lastErr error
	for _, u := range urls {
		n, err := d.Client.StreamSize(ctx, u)
		if err == nil && n > 0 {
			return n, nil
		}
		if err != nil {
			lastErr = err
		}
	}
	if estimate > 0 {
		d.log("  长度探测失败，改用码率估算值 %.1f MB", float64(estimate)/1048576)
		return estimate, nil
	}
	return -1, fmt.Errorf("无法确定流长度: %w", lastErr)
}

// fetchChunk 拉取并写入第 idx 块。任一块失败会依次换源重试。
func (d *Downloader) fetchChunk(ctx context.Context, urls []string, f *os.File, total int64, idx, count int) error {
	start := int64(idx) * d.ChunkSize
	end := start + d.ChunkSize - 1
	if end >= total {
		end = total - 1
	}

	var lastErr error
	for round := 0; round < maxRetryRound; round++ {
		for _, u := range urls {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			n, err := d.getRange(ctx, u, start, end, f)
			if err == nil && n == end-start+1 {
				return nil
			}
			if err != nil {
				lastErr = err
			} else {
				lastErr = fmt.Errorf("分块长度不符：期望 %d 实际 %d", end-start+1, n)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(round+1) * 300 * time.Millisecond):
		}
	}
	return fmt.Errorf("第 %d 块下载失败: %w", idx, lastErr)
}

// getRange 拉取 [start,end] 区间并写入文件的对应偏移。
func (d *Downloader) getRange(ctx context.Context, rawURL string, start, end int64, f *os.File) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	d.Client.SetHeaders(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := d.Client.HTTP().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 服务端忽略了 Range（返回 200 整份）时不能用 WriteAt，否则偏移全错。
	if resp.StatusCode == http.StatusOK && start > 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return 0, errors.New("服务端不支持 Range")
	}

	buf := make([]byte, 128<<10)
	off := start
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], off); werr != nil {
				return off - start, werr
			}
			off += int64(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return off - start, rerr
		}
	}
	return off - start, nil
}

// fetchSequential 在拿不到总长度时退化为一次性顺序下载。
func (d *Downloader) fetchSequential(ctx context.Context, urls []string, dest string) (int64, error) {
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, _ := f.Stat()
	offset := int64(0)
	if info != nil {
		offset = info.Size()
	}

	var lastErr error
	for round := 0; round < maxRetryRound; round++ {
		for _, u := range urls {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				lastErr = err
				continue
			}
			d.Client.SetHeaders(req)
			if offset > 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			}
			resp, err := d.Client.HTTP().Do(req)
			if err != nil {
				lastErr = err
				continue
			}
			if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
				resp.Body.Close()
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				continue
			}
			if offset > 0 && resp.StatusCode == http.StatusOK {
				offset = 0 // 服务端不支持续传，从头写
			}
			if offset > 0 {
				if _, err := f.Seek(offset, io.SeekStart); err != nil {
					resp.Body.Close()
					lastErr = err
					continue
				}
			}
			total := resp.ContentLength + offset
			start := time.Now()
			n, cerr := io.Copy(f, resp.Body)
			resp.Body.Close()
			offset += n
			if cerr == nil {
				return offset, nil
			}
			lastErr = cerr
			_ = total
			_ = start
		}
	}
	return offset, lastErr
}
