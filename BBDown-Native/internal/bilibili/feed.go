package bilibili

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// FeedItem 是「刷视频」列表里的一张卡片。字段都来自接口现成的东西，
// 不额外请求详情——刷列表要快，点进去再取流。
type FeedItem struct {
	Bvid     string `json:"bvid"`
	Title    string `json:"title"`
	Cover    string `json:"cover"`
	Owner    string `json:"owner"`
	Mid      int64  `json:"mid"`
	Duration int    `json:"duration"`
	View     int64  `json:"view"`
}

// 两个接口返回结构不一样，各自解到自己的 DTO，再统一成 FeedItem。
type popularResp struct {
	List []struct {
		Bvid     string `json:"bvid"`
		Title    string `json:"title"`
		Pic      string `json:"pic"`
		Duration int    `json:"duration"`
		Owner    struct {
			Mid  int64  `json:"mid"`
			Name string `json:"name"`
		} `json:"owner"`
		Stat struct {
			View int64 `json:"view"`
		} `json:"stat"`
	} `json:"list"`
}

type rcmdResp struct {
	List []struct {
		Bvid     string `json:"bvid"`
		Title    string `json:"title"`
		Pic      string `json:"pic"`
		Duration int    `json:"duration"`
		Owner    struct {
			Mid  int64  `json:"mid"`
			Name string `json:"name"`
		} `json:"owner"`
		Stat struct {
			View int64 `json:"view"`
		} `json:"stat"`
	} `json:"item"`
}

// Popular 取热门榜（免登录可用）。pn 从 1 开始，每页固定 20 条。
func (c *Client) Popular(ctx context.Context, pn int) ([]FeedItem, error) {
	if pn < 1 {
		pn = 1
	}
	p := url.Values{}
	p.Set("pn", strconv.Itoa(pn))
	p.Set("ps", "20")
	var resp popularResp
	if err := c.get(ctx, "/x/web-interface/popular", p, &resp); err != nil {
		return nil, err
	}
	out := make([]FeedItem, 0, len(resp.List))
	for _, v := range resp.List {
		if v.Bvid == "" {
			continue
		}
		out = append(out, FeedItem{
			Bvid: v.Bvid, Title: v.Title, Cover: httpsCover(v.Pic),
			Owner: v.Owner.Name, Mid: v.Owner.Mid,
			Duration: v.Duration, View: v.Stat.View,
		})
	}
	return out, nil
}

// Recommended 取首页个性化推荐（需登录）。接口每次刷新会换一批，
// 所以没有分页概念，靠 freshIdx 递增告诉 B 站「给我下一批」。
func (c *Client) Recommended(ctx context.Context, freshIdx int) ([]FeedItem, error) {
	if freshIdx < 1 {
		freshIdx = 1
	}
	p := url.Values{}
	p.Set("feed_version", "V8")
	p.Set("fresh_idx", strconv.Itoa(freshIdx))
	p.Set("fresh_idx_1h", strconv.Itoa(freshIdx))
	p.Set("brush", "0")
	p.Set("fetch_row", "4")
	p.Set("homepage_ver", "1")
	p.Set("ps", "12")
	var resp rcmdResp
	if err := c.getSigned(ctx, "/x/web-interface/wbi/index/top/feed/rcmd", p, &resp); err != nil {
		return nil, fmt.Errorf("个性化推荐：%w", err)
	}
	out := make([]FeedItem, 0, len(resp.List))
	for _, v := range resp.List {
		if v.Bvid == "" {
			continue
		}
		out = append(out, FeedItem{
			Bvid: v.Bvid, Title: v.Title, Cover: httpsCover(v.Pic),
			Owner: v.Owner.Name, Mid: v.Owner.Mid,
			Duration: v.Duration, View: v.Stat.View,
		})
	}
	return out, nil
}

// httpsCover 把接口给的 http:// 封面换成 https://。
// 本地服务页是 http 的，但混合内容里 http 图片仍可能被浏览器拦，
// 统一 https 更稳；B 站封面域名两种协议都支持。
func httpsCover(s string) string {
	if len(s) > 7 && s[:7] == "http://" {
		return "https://" + s[7:]
	}
	return s
}
