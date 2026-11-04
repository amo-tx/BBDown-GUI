package bilibili

import (
	"encoding/json"
	"strings"
	"testing"
)

// 结论接口的 data.model_result 形态随版本漂移：老版本是内嵌 JSON 字符串，
// 新版本直接是对象。用两份仿真响应把 decodeModelResult 的两条分派路径锁住。
func TestAISummaryDecodeModelResult(t *testing.T) {
	inner := `{"summary":"这是摘要","outline":[{"title":"章节A","part":"开场","timestamp":0,"content":[{"timestamp":12,"content":"要点"}]}]}`

	// 老形态：字符串
	wire := []byte(`{"code":0,"message":"0","data":{"code":0,"model_result":` +
		`"` + strings.ReplaceAll(inner, `"`, `\"`) + `"` +
		`,"model_type":"prompt_first","too_long_request":false}}`)
	var env struct {
		Code int            `json:"code"`
		Data conclusionResp `json:"data"`
	}
	if err := json.Unmarshal(wire, &env); err != nil {
		t.Fatalf("外壳解码失败: %v", err)
	}
	if env.Data.Code != 0 {
		t.Fatalf("内层 code 应为 0，拿到 %d", env.Data.Code)
	}
	res, err := decodeModelResult(env.Data.ModelResult)
	if err != nil {
		t.Fatalf("字符串形态解码失败: %v", err)
	}
	if res.Summary != "这是摘要" || len(res.Outline) != 1 || res.Outline[0].Part != "开场" {
		t.Fatalf("解码结果不对：%+v", res)
	}
	if res.Outline[0].Content[0].Timestamp != 12 || res.Outline[0].Content[0].Content != "要点" {
		t.Fatalf("要点解码不对：%+v", res.Outline[0].Content)
	}

	// 新形态：对象
	res2, err := decodeModelResult(json.RawMessage(inner))
	if err != nil {
		t.Fatalf("对象形态解码失败: %v", err)
	}
	if res2.Summary != "这是摘要" || len(res2.Outline) != 1 {
		t.Fatalf("对象形态解码结果不对：%+v", res2)
	}

	// 空数据：应当是「没有总结」而不是崩溃
	if _, err := decodeModelResult(nil); err == nil {
		t.Fatalf("空数据应报「没有总结」")
	}
}
