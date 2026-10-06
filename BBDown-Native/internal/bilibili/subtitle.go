package bilibili

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// SubtitleMeta 是一条可用字幕的元信息。
type SubtitleMeta struct {
	ID     int64  `json:"id"`
	Lan    string `json:"lan"`     // 如 zh-CN / ai-zh
	LanDoc string `json:"lan_doc"` // 如「中文（简体）」
	URL    string `json:"url"`     // 字幕 JSON 的地址
	Type   int    `json:"type"`    // 0 = 上传，1 = 机器生成
	// AI 标记机器生成。等价于 lan 以 "ai-" 开头，但服务端两个字段不一定一致。
	AI bool `json:"ai"`
}

// playerV2Resp 是 /x/player/wbi/v2 里我们关心的部分。
type playerV2Resp struct {
	NeedLoginSubtitle bool `json:"need_login_subtitle"`
	Subtitle          struct {
		Lan       string `json:"lan"`
		LanDoc    string `json:"lan_doc"`
		Subtitles []struct {
			ID          int64  `json:"id"`
			Lan         string `json:"lan"`
			LanDoc      string `json:"lan_doc"`
			SubtitleURL string `json:"subtitle_url"`
			Type        int    `json:"type"`
			AIStatus    int    `json:"ai_status"`
		} `json:"subtitles"`
	} `json:"subtitle"`
}

// SubtitleList 拿某个分P 可用的字幕列表。
//
// 注意：B 站对字幕做了登录限制。未登录时接口会把 subtitles 返回成空数组，
// 并在 need_login_subtitle 里置 true。这种情况不算错误，但必须让调用方
// 能区分「真的没有字幕」和「要登录才能看字幕」，否则用户会一头雾水。
func (c *Client) SubtitleList(ctx context.Context, info *VideoInfo, cid int64) ([]SubtitleMeta, error) {
	if info == nil {
		return nil, fmt.Errorf("缺少稿件信息")
	}
	params := url.Values{}
	params.Set("cid", strconv.FormatInt(cid, 10))
	if info.Bvid != "" {
		params.Set("bvid", info.Bvid)
	} else {
		params.Set("aid", strconv.FormatInt(info.Aid, 10))
	}

	var resp playerV2Resp
	// 这个接口要求 WBI 签名；个别网络下未签名的老路径也能过，所以兜一层。
	if err := c.getSigned(ctx, "/x/player/wbi/v2", params, &resp); err != nil {
		if err2 := c.get(ctx, "/x/player/v2", params, &resp); err2 != nil {
			return nil, err
		}
	}

	if len(resp.Subtitle.Subtitles) == 0 {
		if resp.NeedLoginSubtitle && c.Cookie == "" {
			return nil, ErrSubtitleNeedsLogin
		}
		return nil, nil
	}

	out := make([]SubtitleMeta, 0, len(resp.Subtitle.Subtitles))
	for _, s := range resp.Subtitle.Subtitles {
		u := httpsURL(s.SubtitleURL)
		if u == "" {
			continue
		}
		out = append(out, SubtitleMeta{
			ID:     s.ID,
			Lan:    s.Lan,
			LanDoc: s.LanDoc,
			URL:    u,
			Type:   s.Type,
			AI:     s.AIStatus == 1 || len(s.Lan) >= 3 && s.Lan[:3] == "ai-",
		})
	}
	return out, nil
}

// ErrSubtitleNeedsLogin 表示「有字幕，但要登录才能拿到」。
//
// 单独定义成哨兵错误，好让上层把它和「这个视频压根没字幕」区分开 ——
// 后者是正常情况，不该在界面上报警告。
var ErrSubtitleNeedsLogin = fmt.Errorf("字幕需要登录后才能获取")

// FetchSubtitleJSON 下载字幕文件本体。
func (c *Client) FetchSubtitleJSON(ctx context.Context, subURL string) ([]byte, error) {
	body, err := c.FetchBytes(ctx, subURL)
	if err != nil {
		return nil, fmt.Errorf("下载字幕失败：%w", err)
	}
	return body, nil
}
