package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bbdown-native/internal/app"
)

// maxLogLines 是日志环形缓冲的上限。长任务（几十个分P）会打出上万行，
// 不设上限的话每次轮询都要序列化一大堆没人看的历史。
const maxLogLines = 3000

type logLine struct {
	I  int    `json:"i"`
	T  string `json:"t"`
	Lv string `json:"lv"`
	S  string `json:"s"`
}

type outItem struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// taskState 是任务的全部可观测状态。所有字段都由 s.mu 保护。
type taskState struct {
	running bool
	state   string // idle / running / done / failed / stopped
	stage   string
	label   string
	errText string

	done  int64
	total int64
	speed float64
	eta   float64

	steps   map[string]string
	outputs []outItem

	logs    []logLine
	logBase int // logs[0] 的序号
	nextLog int

	cancel context.CancelFunc
}

// reset 回到「还没跑过」的状态。
func (t *taskState) reset() {
	t.running = false
	t.state = "idle"
	t.stage = "等待任务…"
	t.label = ""
	t.errText = ""
	t.done, t.total = 0, 0
	t.speed, t.eta = 0, 0
	t.steps = map[string]string{"parse": "", "video": "", "audio": "", "mux": ""}
	t.outputs = nil
	t.cancel = nil
}

// resetForRun 开新任务：进度与产物清零，但日志留着 —— 用户通常想连
// 解析阶段的输出一起看到。
func (t *taskState) resetForRun() {
	t.reset()
	t.state = "running"
	t.running = true
	t.stage = "准备中…"
}

func (t *taskState) addLog(lv, s string) {
	t.nextLog++
	t.logs = append(t.logs, logLine{
		I:  t.nextLog,
		T:  time.Now().Format("15:04:05"),
		Lv: lv,
		S:  s,
	})
	if n := len(t.logs) - maxLogLines; n > 0 {
		t.logs = append(t.logs[:0], t.logs[n:]...)
		t.logBase += n
	}
}

// since 之后的日志。光标小于缓冲起点时从起点开始给。
func (t *taskState) logsSince(since int) ([]logLine, int) {
	if since < t.logBase {
		since = t.logBase
	}
	out := make([]logLine, 0, 32)
	for _, l := range t.logs {
		if l.I > since {
			out = append(out, l)
		}
	}
	return out, t.nextLog
}

// applyStage 把中文阶段名映射到「解析 → 视频 → 音频 → 封装」四步进度条。
func (t *taskState) applyStage(stage string) {
	mark := func(k, v string) { t.steps[k] = v }
	clear := func(keys ...string) {
		for _, k := range keys {
			t.steps[k] = ""
		}
	}
	switch {
	case strings.Contains(stage, "封装"):
		mark("parse", "done")
		mark("video", "done")
		mark("audio", "done")
		mark("mux", "active")
	case strings.Contains(stage, "音频"):
		mark("parse", "done")
		mark("video", "done")
		mark("audio", "active")
		clear("mux")
	case strings.Contains(stage, "视频"):
		mark("parse", "done")
		mark("video", "active")
		clear("audio", "mux")
	default:
		// 取流 / 探测体积 / 换下一个分P —— 都算作「解析」阶段。
		mark("parse", "active")
		clear("video", "audio", "mux")
	}
}

func (t *taskState) percent() float64 {
	if t.total <= 0 {
		return 0
	}
	p := float64(t.done) / float64(t.total) * 100
	if p > 100 {
		p = 100
	}
	return p
}

// ---------------------------------------------------------------------------
// 引擎回调（都在后台 goroutine 上跑，必须加锁）

func (s *Server) onStage(stage string) {
	s.mu.Lock()
	s.task.stage = stage
	s.task.applyStage(stage)
	s.mu.Unlock()
}

func (s *Server) onLog(line string) {
	s.mu.Lock()
	s.task.addLog(levelOf(line), line)
	s.mu.Unlock()
}

func (s *Server) onProgress(p app.JobProgress) {
	s.mu.Lock()
	s.task.done = p.Done
	s.task.total = p.Total
	s.task.speed = p.Speed
	s.task.eta = p.ETA.Seconds()
	s.task.label = p.Label
	s.mu.Unlock()
}

func (s *Server) onOutput(path string) {
	var size int64
	if fi, err := os.Stat(path); err == nil {
		size = fi.Size()
	}
	s.mu.Lock()
	s.task.outputs = append(s.task.outputs, outItem{
		Name: filepath.Base(path),
		Path: path,
		Size: size,
	})
	s.mu.Unlock()
}

// logf 写一行日志（服务层自己的话）。
func (s *Server) logf(format string, args ...any) {
	s.onLog(fmt.Sprintf(format, args...))
}

// levelOf 从措辞里猜一个颜色档次 —— 引擎的日志没有级别字段，
// 靠这几个前缀区分足够了。
func levelOf(line string) string {
	switch {
	case strings.HasPrefix(line, "✓"):
		return "ok"
	case strings.HasPrefix(line, "✗"):
		return "err"
	case strings.Contains(line, "失败"), strings.Contains(line, "错误"),
		strings.Contains(line, "警告"):
		return "warn"
	}
	return ""
}

// ---------------------------------------------------------------------------
// 接口

type taskDTO struct {
	State   string            `json:"state"`
	Running bool              `json:"running"`
	Stage   string            `json:"stage"`
	Label   string            `json:"label"`
	Done    int64             `json:"done"`
	Total   int64             `json:"total"`
	Speed   float64           `json:"speed"`
	ETA     float64           `json:"eta"`
	Percent float64           `json:"percent"`
	Steps   map[string]string `json:"steps"`
	Outputs []outItem         `json:"outputs"`
	Error   string            `json:"error"`
	Logs    []logLine         `json:"logs"`
	LogFrom int               `json:"log_from"`
	LogAll  int               `json:"log_all"`
}

func (s *Server) apiTask(w http.ResponseWriter, r *http.Request) {
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))

	s.mu.Lock()
	logs, all := s.task.logsSince(since)
	dto := taskDTO{
		State:   s.task.state,
		Running: s.task.running,
		Stage:   s.task.stage,
		Label:   s.task.label,
		Done:    s.task.done,
		Total:   s.task.total,
		Speed:   s.task.speed,
		ETA:     s.task.eta,
		Percent: s.task.percent(),
		Steps:   copySteps(s.task.steps),
		Outputs: append([]outItem(nil), s.task.outputs...),
		Error:   s.task.errText,
		Logs:    logs,
		LogFrom: all,
		LogAll:  all,
	}
	s.mu.Unlock()

	writeJSON(w, dto)
}

func copySteps(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

type downloadReq struct {
	Specs      []string `json:"specs"`
	Quality    int      `json:"quality"`
	CodecOrder string   `json:"codec_order"`
	Parallel   int      `json:"parallel"`
	OutDir     string   `json:"out_dir"`
}

func (s *Server) apiDownload(w http.ResponseWriter, r *http.Request) {
	var in downloadReq
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "请求格式不对：%v", err)
		return
	}

	s.mu.Lock()
	if s.task.running {
		s.mu.Unlock()
		writeErr(w, http.StatusConflict, "已经有任务在跑，先等它结束或点停止")
		return
	}
	items := append([]*app.Resolved(nil), s.items...)
	s.mu.Unlock()

	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest, "请先解析视频地址")
		return
	}

	s.mu.Lock()
	s.task.resetForRun()
	s.mu.Unlock()
	s.onStage("解析")

	go s.runDownload(items, in)
	writeJSON(w, map[string]any{"ok": true, "targets": len(items)})
}

func (s *Server) runDownload(items []*app.Resolved, in downloadReq) {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.task.cancel = cancel
	s.mu.Unlock()
	defer cancel()

	s.logf("开始任务：%d 个目标", len(items))

	var firstErr error
	for i, item := range items {
		if ctx.Err() != nil {
			break
		}
		spec := ""
		if i < len(in.Specs) {
			spec = strings.TrimSpace(in.Specs[i])
		}
		if len(items) > 1 {
			s.logf("—— 第 %d/%d 个目标：%s", i+1, len(items), item.DisplayName())
		}
		opt := app.Options{
			PageSpec: spec,
			OutDir:   in.OutDir,
			Quality:  in.Quality,
			Codec:    in.CodecOrder,
			Parallel: in.Parallel,
		}
		if err := s.runner.Download(ctx, item, opt); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	s.mu.Lock()
	s.task.running = false
	s.task.cancel = nil
	nOut := len(s.task.outputs)
	switch {
	case ctx.Err() != nil:
		s.task.state = "stopped"
		s.task.stage = "已停止"
	case firstErr != nil:
		s.task.state = "failed"
		s.task.stage = "任务结束（有失败）"
		s.task.errText = firstErr.Error()
	default:
		s.task.state = "done"
		s.task.stage = "全部完成"
		for k := range s.task.steps {
			s.task.steps[k] = "done"
		}
	}
	s.mu.Unlock()

	switch {
	case ctx.Err() != nil:
		s.logf("任务已停止")
	case firstErr != nil:
		s.logf("✗ 任务结束：%v", firstErr)
	default:
		s.logf("✓ 全部完成，共 %d 个文件", nOut)
	}
}

func (s *Server) apiStop(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cancel := s.task.cancel
	s.mu.Unlock()
	if cancel == nil {
		writeJSON(w, map[string]any{"ok": true, "note": "当前没有任务"})
		return
	}
	cancel()
	s.logf("正在停止…")
	writeJSON(w, map[string]any{"ok": true})
}
