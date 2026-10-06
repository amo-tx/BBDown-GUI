// Package subtitle 把 B 站字幕 JSON 转成通用的 SRT。
//
// 为什么不直接存 JSON：B 站的字幕 JSON 只有它自己的播放器认识，
// 而 SRT 是所有播放器、剪辑软件都能读的通用格式。两边都存是最实用的做法
// （JSON 保留原始时间轴信息，SRT 拿来直接用）。
//
// 本包**不碰网络**，只做纯文本变换，方便单测。
package subtitle

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Cue 是一条字幕。
type Cue struct {
	From float64 // 起始秒
	To   float64 // 结束秒
	Text string
}

// Track 是一份字幕。
type Track struct {
	Lang    string // 语言代码，如 zh-CN / ai-zh
	LangDoc string // 语言说明，如「中文（简体）」
	AI      bool   // 是否机器生成
	Cues    []Cue

	// raw 是原始 JSON，原样保留。
	// 直接存字节而不是重新 marshal：一来能保证「.json 就是服务端那份」，
	// 二来服务端可能加了我们现在没建模的字段，重新序列化会把它们抹掉。
	raw []byte
}

// rawFile 对应 B 站字幕 JSON 的结构。
//
// 真实形态（aisubtitle.hdslb.com 上那份）：
//
//	{"font_size":0.4,"font_color":"#FFFFFF",
//	 "body":[{"from":0.0,"to":2.5,"location":2,"content":"你好"},…]}
type rawFile struct {
	Body []struct {
		From    float64 `json:"from"`
		To      float64 `json:"to"`
		Content string  `json:"content"`
	} `json:"body"`
}

// ParseJSON 解析 B 站字幕 JSON。
func ParseJSON(data []byte) (*Track, error) {
	// 有些响应会带 UTF-8 BOM，会直接让 json.Unmarshal 失败。
	data = trimBOM(data)

	var f rawFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("字幕 JSON 解析失败: %w", err)
	}

	tr := &Track{Cues: make([]Cue, 0, len(f.Body)), raw: data}
	for _, b := range f.Body {
		text := normalize(b.Content)
		if text == "" {
			continue
		}
		from := math.Max(0, b.From)
		to := b.To
		// 时间轴坏掉的条目直接丢掉：SRT 里 from >= to 会让播放器行为诡异。
		if to <= from {
			continue
		}
		tr.Cues = append(tr.Cues, Cue{From: from, To: to, Text: text})
	}
	return tr, nil
}

// normalize 规整一条字幕的正文。
func normalize(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	// 去掉行首尾空白，但保留中间换行（有些字幕靠换行分行显示）。
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

// SRT 输出 SRT 文本。
//
// SRT 的时间格式是 HH:MM:SS,mmm（毫秒 + 逗号），和 ASS 的
// H:MM:SS.CC（厘秒 + 点）不是一回事，别混。
func (t *Track) SRT() []byte {
	var b strings.Builder
	n := 0
	for _, c := range t.Cues {
		n++
		fmt.Fprintf(&b, "%d\n%s --> %s\n%s\n\n", n, srtTime(c.From), srtTime(c.To), c.Text)
	}
	return []byte(b.String())
}

// srtTime 把秒格式化成 SRT 的 HH:MM:SS,mmm。
func srtTime(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	ms := int(math.Round(sec * 1000))
	h := ms / 3600000
	ms -= h * 3600000
	m := ms / 60000
	ms -= m * 60000
	s := ms / 1000
	ms -= s * 1000
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, ms)
}

// Raw 返回服务端给的原始 JSON（没解析成功过的话就是空）。
func (t *Track) Raw() []byte { return t.raw }

// Label 返回一个适合拼进文件名的标签，如 "zh-CN" 或 "ai-zh"。
func (t *Track) Label() string {
	if t.Lang != "" {
		return SanitizeLabel(t.Lang)
	}
	if t.AI {
		return "ai"
	}
	return "sub"
}

// SanitizeLabel 去掉文件名里不能用的字符。
func SanitizeLabel(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|', ' ':
			return '_'
		}
		return r
	}, s)
}
