package bilibili

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// 画质编号（B 站 qn / dash.video[].id）。
const (
	Q8K        = 127
	QDolbyHDR  = 126
	QHDR       = 125
	Q4K        = 120
	Q1080P60   = 116
	Q1080PPlus = 112
	Q1080P     = 80
	Q720P      = 64
	Q480P      = 32
	Q360P      = 16
	Q240P      = 6
)

// 编码编号。
const (
	CodecAVC  = 7
	CodecHEVC = 12
	CodecAV1  = 13
)

var qualityName = map[int]string{
	Q8K: "8K 超高清", QDolbyHDR: "杜比视界", QHDR: "HDR 真彩",
	Q4K: "4K 超清", Q1080P60: "1080P60", Q1080PPlus: "1080P 高码率",
	Q1080P: "1080P 高清", Q720P: "720P 高清", Q480P: "480P 清晰",
	Q360P: "360P 流畅", Q240P: "240P 极速",
}

// QualityName 返回画质的中文名，未知编号返回 "qn<id>"。
func QualityName(q int) string {
	if n, ok := qualityName[q]; ok {
		return n
	}
	return "qn" + strconv.Itoa(q)
}

// CodecName 返回编码名。
func CodecName(codecs string) string {
	c := strings.ToLower(codecs)
	switch {
	case strings.HasPrefix(c, "avc"), strings.HasPrefix(c, "avc1"):
		return "AVC"
	case strings.HasPrefix(c, "hev"), strings.HasPrefix(c, "hvc"):
		return "HEVC"
	case strings.HasPrefix(c, "av01"):
		return "AV1"
	}
	return codecs
}

// Stream 是一条 DASH 流（视频或音频）。
type Stream struct {
	ID        int      // 视频：画质编号；音频：音频编号
	BaseURL   string   // 主地址
	BackupURL []string // 备用地址
	Bandwidth int      // 码率 bit/s，用来估体积与挑流
	MimeType  string
	Codecs    string
	Width     int
	Height    int
	FrameRate string
	// Codecid 是编码编号（7/12/13），来自接口的 codecid 字段。
	Codecid int
}

// IsAudio 判断这条流是不是音频。
func (s Stream) IsAudio() bool {
	return s.Height == 0 && strings.HasPrefix(strings.ToLower(s.MimeType), "audio")
}

// EstimateSize 按码率估算体积（字节）。
func (s Stream) EstimateSize(durationSec int) int64 {
	return int64(float64(s.Bandwidth) / 8.0 * float64(durationSec))
}

// PlayURL 是一次取流的结果。
type PlayURL struct {
	Quality       int      // 实际返回的最高画质
	AcceptQuality []int    // 可选画质
	Duration      int      // 秒
	Video         []Stream // 视频流，按码率升序
	Audio         []Stream // 音频流，按码率升序
}

// URLs 返回该流的候选地址（主 + 备用）。
func (s Stream) URLs() []string {
	out := make([]string, 0, len(s.BackupURL)+1)
	if s.BaseURL != "" {
		out = append(out, s.BaseURL)
	}
	out = append(out, s.BackupURL...)
	return out
}

type playResp struct {
	Quality    int   `json:"quality"`
	Timelength int   `json:"timelength"` // 毫秒
	AcceptQual []int `json:"accept_quality"`
	Dash       *struct {
		Duration int `json:"duration"`
		Video    []struct {
			ID        int      `json:"id"`
			BaseURL   string   `json:"baseUrl"`
			BaseURL2  string   `json:"base_url"`
			BackupURL []string `json:"backupUrl"`
			Backup2   []string `json:"backup_url"`
			Bandwidth int      `json:"bandwidth"`
			MimeType  string   `json:"mimeType"`
			MimeType2 string   `json:"mime_type"`
			Codecs    string   `json:"codecs"`
			Width     int      `json:"width"`
			Height    int      `json:"height"`
			FrameRate string   `json:"frameRate"`
			FrameRt2  string   `json:"frame_rate"`
			Codecid   int      `json:"codecid"`
		} `json:"video"`
		Audio []struct {
			ID        int      `json:"id"`
			BaseURL   string   `json:"baseUrl"`
			BaseURL2  string   `json:"base_url"`
			BackupURL []string `json:"backupUrl"`
			Backup2   []string `json:"backup_url"`
			Bandwidth int      `json:"bandwidth"`
			MimeType  string   `json:"mimeType"`
			MimeType2 string   `json:"mime_type"`
			Codecs    string   `json:"codecs"`
		} `json:"audio"`
	} `json:"dash"`
}

// fnvalDASH 是各能力位的按位或：
//
//	16=DASH  64=HDR  128=4K  256=杜比音频  512=杜比视界  1024=8K  2048=AV1
//
// 加起来正好是 4048（内含 16，即 DASH 本身）。
// 注意别再额外加一遍——相加会进位，把 16 这个 DASH 位"顶掉"，
// 接口会直接返回 -400 请求错误。
const fnvalDASH = 4048

// PlayURL 取流。page 指明要取哪一分P（Index 从 1 开始，0 表示第一个）。
func (c *Client) PlayURL(ctx context.Context, info *VideoInfo, page Page) (*PlayURL, error) {
	params := url.Values{}
	params.Set("cid", strconv.FormatInt(page.Cid, 10))
	params.Set("qn", strconv.Itoa(Q8K)) // 请求最高，实际由账号权限决定
	params.Set("fnval", strconv.Itoa(fnvalDASH))
	params.Set("fnver", "0")
	params.Set("fourk", "1")

	path := "/x/player/wbi/playurl"
	if info.IsBangumi {
		path = "/pgc/player/web/playurl"
		params.Set("ep_id", strings.TrimPrefix(strings.ToLower(info.Target.ID), "ep"))
		params.Set("season_id", strconv.FormatInt(info.SeasonID, 10))
	} else {
		if info.Bvid != "" {
			params.Set("bvid", info.Bvid)
		} else {
			params.Set("avid", strconv.FormatInt(info.Aid, 10))
		}
	}

	var pr playResp
	err := c.getSigned(ctx, path, params, &pr)
	if err != nil {
		return nil, err
	}
	if pr.Dash == nil || len(pr.Dash.Video) == 0 {
		return nil, fmt.Errorf("接口没有返回 DASH 流（可能需要登录，或该稿件不支持）")
	}

	out := &PlayURL{Quality: pr.Quality, AcceptQuality: pr.AcceptQual}
	if pr.Dash.Duration > 0 {
		out.Duration = pr.Dash.Duration
	} else {
		out.Duration = pr.Timelength / 1000
	}
	if out.Duration == 0 {
		out.Duration = page.Duration
	}

	for _, v := range pr.Dash.Video {
		out.Video = append(out.Video, Stream{
			ID:        v.ID,
			BaseURL:   firstNonEmpty(v.BaseURL, v.BaseURL2),
			BackupURL: append(append([]string{}, v.BackupURL...), v.Backup2...),
			Bandwidth: v.Bandwidth,
			MimeType:  firstNonEmpty(v.MimeType, v.MimeType2),
			Codecs:    v.Codecs,
			Width:     v.Width,
			Height:    v.Height,
			FrameRate: firstNonEmpty(v.FrameRate, v.FrameRt2),
			Codecid:   v.Codecid,
		})
	}
	for _, a := range pr.Dash.Audio {
		out.Audio = append(out.Audio, Stream{
			ID:        a.ID,
			BaseURL:   firstNonEmpty(a.BaseURL, a.BaseURL2),
			BackupURL: append(append([]string{}, a.BackupURL...), a.Backup2...),
			Bandwidth: a.Bandwidth,
			MimeType:  firstNonEmpty(a.MimeType, a.MimeType2),
			Codecs:    a.Codecs,
		})
	}

	// 按码率升序，方便后面按"最高/指定"挑选。
	sort.Slice(out.Video, func(i, j int) bool {
		if out.Video[i].Bandwidth != out.Video[j].Bandwidth {
			return out.Video[i].Bandwidth < out.Video[j].Bandwidth
		}
		return out.Video[i].ID < out.Video[j].ID
	})
	sort.Slice(out.Audio, func(i, j int) bool { return out.Audio[i].Bandwidth < out.Audio[j].Bandwidth })

	return out, nil
}

// SelectVideo 按「画质优先级 + 编码优先级」挑一条视频流。
//
// quality 是用户排好序的画质编号（从高到低），codecs 是编码偏好（如 "hevc,av1,avc"）。
// 两者都为空的默认：支持的最高画质 + 优先 HEVC（同画质下体积比 AVC 小不少）。
func (p *PlayURL) SelectVideo(quality []int, codecs []string) (Stream, error) {
	if len(p.Video) == 0 {
		return Stream{}, fmt.Errorf("没有可用的视频流")
	}
	if len(codecs) == 0 {
		codecs = []string{"hevc", "avc", "av1"}
	}

	// 按画质优先级组：第一轮找最高优先画质，找不到就降级。
	qs := quality
	if len(qs) == 0 {
		// 默认：可用流里画质最高的
		best := 0
		for _, v := range p.Video {
			if v.ID > best {
				best = v.ID
			}
		}
		qs = []int{best}
	}

	for _, q := range qs {
		cand := make([]Stream, 0, 4)
		for _, v := range p.Video {
			if v.ID == q {
				cand = append(cand, v)
			}
		}
		if len(cand) == 0 {
			continue
		}
		for _, want := range codecs {
			for _, v := range cand {
				if strings.EqualFold(CodecName(v.Codecs), strings.ToUpper(want)) {
					return v, nil
				}
			}
		}
		return cand[len(cand)-1], nil // 该画质下没有指定编码，退而取码率最高的一条
	}
	// 全都没命中：取最高画质里码率最高的。
	return p.Video[len(p.Video)-1], nil
}

// SelectAudio 挑音轨。默认取码率最高的。
func (p *PlayURL) SelectAudio() (Stream, error) {
	if len(p.Audio) == 0 {
		return Stream{}, fmt.Errorf("没有可用的音频流")
	}
	best := p.Audio[0]
	for _, a := range p.Audio {
		if a.Bandwidth > best.Bandwidth {
			best = a
		}
	}
	return best, nil
}
