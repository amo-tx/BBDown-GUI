package bilibili

import (
	"context"
	"fmt"
)

// danmakuBase 是弹幕 XML 的地址。它不在 api.bilibili.com 上，
// 而是独立的 comment.bilibili.com，响应也不是 JSON 信封。
const danmakuBase = "https://comment.bilibili.com"

// FetchDanmakuXML 拉取某个 cid 的完整弹幕 XML。
//
// 这个接口会返回 Content-Encoding: deflate（且是裸 deflate，没有 zlib 头），
// 解压交给 FetchBytes 统一处理。
func (c *Client) FetchDanmakuXML(ctx context.Context, cid int64) ([]byte, error) {
	if cid <= 0 {
		return nil, fmt.Errorf("cid 无效：%d", cid)
	}
	body, err := c.FetchBytes(ctx, fmt.Sprintf("%s/%d.xml", danmakuBase, cid))
	if err != nil {
		return nil, fmt.Errorf("下载弹幕失败：%w", err)
	}
	if !looksLikeXML(body) {
		return nil, fmt.Errorf("弹幕接口没有返回 XML（可能被风控，前 120 字节：%s）",
			truncate(string(body), 120))
	}
	return body, nil
}
