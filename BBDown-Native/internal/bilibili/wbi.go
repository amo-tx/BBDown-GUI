package bilibili

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// mixinKeyEncTab 是 B 站 WBI 签名的固定重排表（64 项）。
// 用法：把 imgKey+subKey 拼起来，按这张表取字符拼成"混淆密钥"。
var mixinKeyEncTab = [64]int{
	46, 47, 18, 2, 53, 8, 23, 32, 15, 50, 10, 31, 58, 3, 45, 35,
	27, 43, 5, 49, 33, 9, 42, 19, 29, 28, 14, 39, 12, 38, 41, 13,
	37, 48, 7, 16, 24, 55, 40, 61, 26, 17, 0, 1, 60, 51, 30, 4,
	22, 25, 54, 21, 56, 59, 6, 63, 57, 62, 11, 36, 20, 34, 44, 52,
}

// getMixinKey 按重排表从原始串生成混淆密钥，取前 32 位。
func getMixinKey(orig string) string {
	var sb strings.Builder
	sb.Grow(64)
	for _, idx := range mixinKeyEncTab {
		if idx < len(orig) {
			sb.WriteByte(orig[idx])
		}
	}
	s := sb.String()
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}

// navKeysResp 只需要 wbi_img 两个 URL。
type navKeysResp struct {
	WbiImg struct {
		ImgURL string `json:"img_url"`
		SubURL string `json:"sub_url"`
	} `json:"wbi_img"`
}

// wbiKeys 取当前的 img/sub key，并按小时缓存。
//
// key 每天轮换，但同一进程内没必要每次请求都去取一遍。
func (c *Client) wbiKeys(ctx context.Context) (imgKey, subKey string, err error) {
	c.mu.Lock()
	if c.imgKey != "" && time.Since(c.keyTime) < time.Hour {
		imgKey, subKey = c.imgKey, c.subKey
		c.mu.Unlock()
		return imgKey, subKey, nil
	}
	c.mu.Unlock()

	var nav navKeysResp
	// nav 接口本身不需要签名。注意未登录时它返回 code=-101，
	// 但 data.wbi_img 依然有效，所以这里必须用 getDataRaw 而不是 get。
	data, code, msg, err := c.getDataRaw(ctx, "/x/web-interface/nav", nil)
	if err != nil {
		return "", "", fmt.Errorf("获取 WBI 密钥失败: %w", err)
	}
	if err := json.Unmarshal(data, &nav); err != nil {
		return "", "", fmt.Errorf("WBI 密钥结构异常（code=%d %s）: %w", code, msg, err)
	}
	// URL 形如 https://i0.hdslb.com/bfs/wbi/7cd084941338484aae1ad9425b84077c.png
	imgKey = trimExt(path.Base(nav.WbiImg.ImgURL))
	subKey = trimExt(path.Base(nav.WbiImg.SubURL))
	if imgKey == "" || subKey == "" {
		return "", "", fmt.Errorf("WBI 密钥为空（code=%d %s），接口结构可能已变化", code, msg)
	}

	c.mu.Lock()
	c.imgKey, c.subKey, c.keyTime = imgKey, subKey, time.Now()
	c.mu.Unlock()
	return imgKey, subKey, nil
}

func trimExt(s string) string {
	if i := strings.LastIndexByte(s, '.'); i > 0 {
		return s[:i]
	}
	return s
}

// wbiEscape 按 B 站的规则编码：标准 urlencode，但先把 !'()* 从值里删掉。
//
// 这是官方前端的行为（filter 掉这几个字符再 urlencode），漏掉会导致
// w_rid 与服务端算出来的不一致，接口直接返回 -403。
func wbiEscape(s string) string {
	var sb strings.Builder
	sb.Grow(len(s))
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '!', '\'', '(', ')', '*':
			continue
		}
		sb.WriteByte(s[i])
	}
	return url.QueryEscape(sb.String())
}

// signWBI 给参数附加 wts 与 w_rid。
//
// 步骤：
//  1. 补 wts（当前秒级时间戳）
//  2. 按 key 升序拼接 k=v（值先过滤 !'()* 再 urlencode）
//  3. w_rid = md5(query + mixinKey)
func (c *Client) signWBI(ctx context.Context, params url.Values) (url.Values, error) {
	imgKey, subKey, err := c.wbiKeys(ctx)
	if err != nil {
		return nil, err
	}
	mixinKey := getMixinKey(imgKey + subKey)

	out := url.Values{}
	for k, vs := range params {
		for _, v := range vs {
			out.Add(k, v)
		}
	}
	out.Set("wts", strconv.FormatInt(time.Now().Unix(), 10))

	keys := make([]string, 0, len(out))
	for k := range out {
		if k == "w_rid" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(wbiEscape(k))
		sb.WriteByte('=')
		sb.WriteString(wbiEscape(out.Get(k)))
	}
	sum := md5.Sum([]byte(sb.String() + mixinKey))
	out.Set("w_rid", hex.EncodeToString(sum[:]))
	return out, nil
}
