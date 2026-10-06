package bilibili

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// FetchBytes 下载一个任意 URL 的原始字节，自动处理 gzip / deflate。
//
// 存在的理由：B 站的图片床（i0.hdslb.com）与字幕床（aisubtitle.hdslb.com）
// 都可能返回 Content-Encoding: deflate，而 Go 的 http.Transport **只**自动解
// gzip（且必须是自己加上去的 Accept-Encoding）。不自己处理就会拿到一堆二进制。
func (c *Client) FetchBytes(ctx context.Context, rawURL string) ([]byte, error) {
	if rawURL == "" {
		return nil, fmt.Errorf("地址为空")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	c.SetHeaders(req)
	req.Header.Set("Accept", "*/*")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求失败：%w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return readMaybeCompressed(resp)
}

// readMaybeCompressed 按 Content-Encoding 解压响应体。
//
// 头部缺失有两种可能：本来就明文，或者 Go 已经替我们把 gzip 解掉了
// （那时 resp.Uncompressed 为真、头部也被删掉了）。两种情况都直接读原 body。
func readMaybeCompressed(resp *http.Response) ([]byte, error) {
	const limit = 64 << 20 // 弹幕与字幕再大也不会到 64MB，超出必然是异常响应
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))

	switch enc {
	case "":
		return io.ReadAll(io.LimitReader(resp.Body, limit))
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("解压 gzip 失败：%w", err)
		}
		defer zr.Close()
		return io.ReadAll(io.LimitReader(zr, limit))
	case "deflate":
		return readDeflate(resp.Body, limit)
	default:
		return nil, fmt.Errorf("不支持的压缩方式：%s", enc)
	}
}

// readDeflate 处理 deflate。
//
// HTTP 里的 "deflate" 有歧义：规范说是 zlib 包装（2 字节头 + 尾部校验），
// 但不少服务器（B 站弹幕接口就是）直接给**裸 deflate**。
// 先按 zlib 试，失败再按裸 deflate 试 —— curl --compressed 也是这么做的。
func readDeflate(r io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, fmt.Errorf("读取响应失败：%w", err)
	}

	if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
		defer zr.Close()
		if out, err := io.ReadAll(io.LimitReader(zr, limit)); err == nil {
			return out, nil
		}
	}

	fr := flate.NewReader(bytes.NewReader(raw))
	defer fr.Close()
	out, err := io.ReadAll(io.LimitReader(fr, limit))
	if err != nil {
		return nil, fmt.Errorf("解压 deflate 失败：%w", err)
	}
	return out, nil
}

// looksLikeXML 粗略判断内容像不像 XML，用来把「风控返回的 HTML」挡在解析之前。
func looksLikeXML(b []byte) bool {
	head := bytes.TrimSpace(b)
	if len(head) > 512 {
		head = head[:512]
	}
	return bytes.HasPrefix(head, []byte("<?xml")) || bytes.HasPrefix(head, []byte("<i>"))
}

// httpsURL 把协议相对地址（//i0.hdslb.com/...）与明文 http 统一升到 https。
//
// B 站的图片床返回的是 http 地址，直接拿去请求会被部分网络环境拦掉，
// 而 i0.hdslb.com 本身是支持 https 的。
func httpsURL(u string) string {
	switch {
	case strings.HasPrefix(u, "//"):
		return "https:" + u
	case strings.HasPrefix(u, "http://"):
		return "https://" + strings.TrimPrefix(u, "http://")
	default:
		return u
	}
}
