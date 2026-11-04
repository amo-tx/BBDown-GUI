package bilibili

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
)

// ---------------------------------------------------------------------------
// AI 视频总结（view/conclusion）
//
// B 站对相当一部分稿件跑过大模型摘要：网页播放器右侧的「AI 视频总结」
// 就是这个接口出的数据。用它而不是自己接大模型，意味着：
//   - 零依赖（不用 API key、不用下载模型），符合本程序单文件的定位；
//   - 摘要由 B 站生成，已经带好章节大纲和时间戳。
// 代价是覆盖面有限：没有跑过摘要的稿件（太短、太新、纯音乐等）接口会
// 返回业务码非 0，调用方要把它当成「正常没有」而不是故障。

// AISummaryPoint 是大纲里的一个时间戳条目。
type AISummaryPoint struct {
	Timestamp int64  `json:"timestamp"` // 秒
	Content   string `json:"content"`
}

// AISummaryPart 是大纲的一个章节。
type AISummaryPart struct {
	Title   string           `json:"title"`   // 章节标题（个别版本用这个字段）
	Part    string           `json:"part"`    // 章节标题（主流字段）
	Content []AISummaryPoint `json:"content"` // 章节内的要点
}

// AISummaryResult 是一份 AI 视频总结。
type AISummaryResult struct {
	Summary string          `json:"summary"`
	Outline []AISummaryPart `json:"outline"`
}

// conclusionResp 是 /x/web-interface/view/conclusion/get 的外壳。
// data.model_result 的形态**随版本漂移**：老版本是一段内嵌 JSON 字符串，
// 新版本直接是对象。用 RawMessage 接住后再按类型分派。
type conclusionResp struct {
	ModelResult json.RawMessage `json:"model_result"`
	Code        int             `json:"code"`
	Message     string          `json:"message"`
}

// AISummary 拿某个分P 的 AI 视频总结。
//
// cid 决定总结针对哪一P；多P 稿件每P 各有一份。接口要 WBI 签名，
// 未登录时多数稿件也能拿到（B 站按风控放行），拿不到时错误信息要
// 能让用户分清「没有总结」和「需要登录」。
func (c *Client) AISummary(ctx context.Context, info *VideoInfo, cid int64) (*AISummaryResult, error) {
	if info == nil {
		return nil, fmt.Errorf("缺少稿件信息")
	}
	params := url.Values{}
	params.Set("cid", strconv.FormatInt(cid, 10))
	params.Set("up_mid", strconv.FormatInt(info.Mid, 10))
	if info.Bvid != "" {
		params.Set("bvid", info.Bvid)
	} else {
		params.Set("aid", strconv.FormatInt(info.Aid, 10))
	}

	var resp conclusionResp
	if err := c.getSigned(ctx, "/x/web-interface/view/conclusion/get", params, &resp); err != nil {
		return nil, err
	}
	// 外壳 code=0 时内层还有自己的 code：-1 / -404 都表示这份稿件
	// 没有总结（太短 / 未跑大模型），归成同一句人话；其他码原样带出去。
	if resp.Code != CodeOK {
		if resp.Code == -1 || resp.Code == -404 {
			return nil, fmt.Errorf("该视频没有 AI 总结（B 站未对它生成摘要）")
		}
		msg := resp.Message
		if msg == "" || msg == "0" {
			msg = fmt.Sprintf("内层码 %d", resp.Code)
		}
		return nil, fmt.Errorf("AI 总结暂不可用：%s", msg)
	}

	return decodeModelResult(resp.ModelResult)
}

// decodeModelResult 兼容 model_result 的两种形态（字符串 / 对象）。
func decodeModelResult(raw json.RawMessage) (*AISummaryResult, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("该视频没有 AI 总结（B 站未对它生成摘要）")
	}
	var res AISummaryResult
	if raw[0] == '"' {
		var inner string
		if err := json.Unmarshal(raw, &inner); err != nil {
			return nil, fmt.Errorf("AI 总结数据解析失败: %w", err)
		}
		raw = []byte(inner)
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("AI 总结数据解析失败: %w", err)
	}
	if res.Summary == "" && len(res.Outline) == 0 {
		return nil, fmt.Errorf("该视频没有 AI 总结（B 站未对它生成摘要）")
	}
	return &res, nil
}
