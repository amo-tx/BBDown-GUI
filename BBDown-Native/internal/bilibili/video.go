package bilibili

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Page 是稿件的一个分P。
type Page struct {
	Index    int    // 第几 P，从 1 开始
	Cid      int64  // 弹幕/取流用的 cid
	Title    string // 分P标题（单 P 稿件时等于视频标题）
	Duration int    // 秒
}

// VideoInfo 是解析结果。
type VideoInfo struct {
	Target    Target
	Title     string
	Owner     string
	Cover     string
	Desc      string
	Duration  int // 秒
	PubTime   time.Time
	Pages     []Page
	IsBangumi bool
	SeasonID  int64
	// Aid 是稿件号（数字），番剧用于取流。
	Aid int64
	// Bvid 仅普通稿件有。
	Bvid string
	// DefaultPage 是界面上应该默认选中的分P序号（1 起）。
	// 来源：链接里的 ?p= 参数，或番剧链接直接指向的 ep。
	DefaultPage int
}

// DisplayTitle 返回适合做文件名的标题（去掉路径非法字符）。
func (v *VideoInfo) DisplayTitle() string { return SanitizeFileName(v.Title) }

// SanitizeFileName 去掉 Windows 文件名里不允许的字符。
func SanitizeFileName(s string) string {
	repl := func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return '_'
		}
		if r < 0x20 {
			return '_'
		}
		return r
	}
	out := strings.Map(repl, s)
	out = strings.TrimRight(out, " .")
	if len(out) > 120 {
		// 按 rune 截断，避免切碎多字节字符。
		r := []rune(out)
		if len(r) > 120 {
			out = string(r[:120])
		}
	}
	if out == "" {
		out = "未命名"
	}
	return out
}

// VideoInfo 解析目标对应的稿件/剧集信息。
func (c *Client) VideoInfo(ctx context.Context, t Target) (*VideoInfo, error) {
	if t.Short {
		return nil, fmt.Errorf("短链需要先解析：%s", t.Raw)
	}
	if t.Kind == KindBangumi {
		return c.bangumiInfo(ctx, t)
	}
	return c.videoInfo(ctx, t)
}

// --- 普通投稿 ---------------------------------------------------------------

type viewResp struct {
	Bvid     string `json:"bvid"`
	Aid      int64  `json:"aid"`
	Title    string `json:"title"`
	Pic      string `json:"pic"`
	Desc     string `json:"desc"`
	Duration int    `json:"duration"`
	Pubdate  int64  `json:"pubdate"`
	Owner    struct {
		Name string `json:"name"`
	} `json:"owner"`
	Pages []struct {
		Cid      int64  `json:"cid"`
		Page     int    `json:"page"`
		Part     string `json:"part"`
		Duration int    `json:"duration"`
	} `json:"pages"`
}

func (c *Client) videoInfo(ctx context.Context, t Target) (*VideoInfo, error) {
	params := url.Values{}
	if strings.HasPrefix(t.ID, "BV") {
		params.Set("bvid", t.ID)
	} else {
		params.Set("aid", strings.TrimPrefix(strings.ToLower(t.ID), "av"))
	}

	var vr viewResp
	err := c.get(ctx, "/x/web-interface/view", params, &vr)
	if err != nil {
		// 少数情况下 view 接口也要求签名，退一步重试。
		if err2 := c.getSigned(ctx, "/x/web-interface/view", params, &vr); err2 != nil {
			return nil, err
		}
	}

	if vr.Title == "" {
		return nil, fmt.Errorf("解析结果为空，可能是无效地址或稿件已失效")
	}

	info := &VideoInfo{
		Target:   t,
		Title:    vr.Title,
		Owner:    vr.Owner.Name,
		Cover:    vr.Pic,
		Desc:     vr.Desc,
		Duration: vr.Duration,
		PubTime:  time.Unix(vr.Pubdate, 0),
		Aid:      vr.Aid,
		Bvid:     vr.Bvid,
	}
	for _, p := range vr.Pages {
		idx := p.Page
		if idx == 0 {
			idx = len(info.Pages) + 1
		}
		title := p.Part
		if title == "" {
			title = vr.Title
		}
		info.Pages = append(info.Pages, Page{
			Index:    idx,
			Cid:      p.Cid,
			Title:    title,
			Duration: p.Duration,
		})
	}
	if len(info.Pages) == 0 {
		return nil, fmt.Errorf("稿件没有可用分P")
	}
	if info.Bvid == "" {
		info.Bvid = t.ID
	}
	info.DefaultPage = 1
	if t.Page > 0 && t.Page <= len(info.Pages) {
		info.DefaultPage = t.Page
	}
	return info, nil
}

// --- 番剧 / 剧集 ------------------------------------------------------------

type pgcResp struct {
	Title      string `json:"title"`
	SeasonID   int64  `json:"season_id"`
	Cover      string `json:"cover"`
	Evaluate   string `json:"evaluate"`
	Episodes   []struct {
		ID        int64  `json:"id"`
		Aid       int64  `json:"aid"`
		Bvid      string `json:"bvid"`
		Cid       int64  `json:"cid"`
		Title     string `json:"title"`      // 例如 "第1话"
		LongTitle string `json:"long_title"` // 例如 "起点"
		Duration  int    `json:"duration"`   // 毫秒
	} `json:"episodes"`
}

func (c *Client) bangumiInfo(ctx context.Context, t Target) (*VideoInfo, error) {
	params := url.Values{}
	low := strings.ToLower(t.ID)
	isEp := strings.HasPrefix(low, "ep")
	wantEp := ""
	if isEp {
		wantEp = strings.TrimPrefix(low, "ep")
		params.Set("ep_id", wantEp)
	} else {
		params.Set("season_id", strings.TrimPrefix(low, "ss"))
	}

	var pr pgcResp
	if err := c.get(ctx, "/pgc/view/web/season", params, &pr); err != nil {
		return nil, err
	}
	if len(pr.Episodes) == 0 {
		return nil, fmt.Errorf("剧集信息为空，可能是无效地址或地区限制")
	}

	info := &VideoInfo{
		Target:      t,
		Title:       pr.Title,
		Cover:       pr.Cover,
		Desc:        pr.Evaluate,
		IsBangumi:   true,
		SeasonID:    pr.SeasonID,
		DefaultPage: 1,
	}
	for i, ep := range pr.Episodes {
		title := ep.Title
		if ep.LongTitle != "" {
			title = ep.Title + " " + ep.LongTitle
		}
		info.Pages = append(info.Pages, Page{
			Index:    i + 1,
			Cid:      ep.Cid,
			Title:    strings.TrimSpace(title),
			Duration: ep.Duration / 1000,
		})
		// 链接直接指向某一话时，默认选中那一话。
		if wantEp != "" && strconv.FormatInt(ep.ID, 10) == wantEp {
			info.DefaultPage = i + 1
		}
	}
	info.Duration = info.Pages[info.DefaultPage-1].Duration
	if info.Title == "" {
		info.Title = t.ID
	}
	return info, nil
}

// PickPages 根据用户选择（"1,3-5" 这种）返回分P。
// 空字符串表示全部。
func (v *VideoInfo) PickPages(spec string) ([]Page, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return v.Pages, nil
	}
	want := map[int]bool{}
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.IndexByte(part, '-'); i > 0 {
			a, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			b, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil || a > b {
				return nil, fmt.Errorf("分P范围写法不对：%s", part)
			}
			for n := a; n <= b; n++ {
				want[n] = true
			}
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("分P写法不对：%s", part)
		}
		want[n] = true
	}

	var out []Page
	for _, p := range v.Pages {
		if want[p.Index] {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("选中的分P都不存在")
	}
	return out, nil
}
