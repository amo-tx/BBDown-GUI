// Package bilibili 封装 B 站 Web 端接口。
//
// 设计要点：
//   - 只依赖标准库，HTTP 层自己管重试、风控提示与 Cookie。
//   - 所有需要签名的接口都走 WBI（见 wbi.go），签名参数由 signWBI 统一附加。
//   - 错误统一为 *APIError，调用方可以通过 Code 判断（如 -101 未登录、-404 不存在）。
package bilibili

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	// chromeUA 用真实浏览器 UA。B 站对非浏览器 UA 的风控更严（会返回 412）。
	chromeUA       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
	defaultReferer = "https://www.bilibili.com"
	apiBase        = "https://api.bilibili.com"
)

// 常见业务错误码，供调用方判断。
const (
	CodeOK          = 0
	CodeNotLoggedIn = -101
	CodeNotFound    = -404
	CodeRiskControl = -412
)

// APIError 表示接口返回的业务错误（HTTP 200 但 code != 0）。
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("接口返回错误 %d: %s", e.Code, e.Message)
}

// IsNotLoggedIn 判断错误是否为「未登录」。
func IsNotLoggedIn(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && (ae.Code == CodeNotLoggedIn || ae.Code == CodeRiskControl)
}

// Client 是一个带 Cookie 与重试的 B 站接口客户端。
type Client struct {
	hc     *http.Client
	Cookie string

	mu      sync.Mutex
	imgKey  string
	subKey  string
	keyTime time.Time
}

// NewClient 创建客户端。
func NewClient() *Client {
	tr := &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Client{hc: &http.Client{Transport: tr, Timeout: 45 * time.Second}}
}

// HTTP 暴露底层客户端，下载层需要复用连接池。
func (c *Client) HTTP() *http.Client { return c.hc }

// SetHeaders 给下载请求补齐 B 站要求的头（CDN 会校验 Referer）。
func (c *Client) SetHeaders(req *http.Request) {
	req.Header.Set("User-Agent", chromeUA)
	req.Header.Set("Referer", defaultReferer)
	req.Header.Set("Origin", "https://www.bilibili.com")
	if c.Cookie != "" {
		req.Header.Set("Cookie", c.Cookie)
	}
}

// ---------------------------------------------------------------------------
// 底层请求

// getRaw 发一个 GET 并返回响应体，带退避重试。
// retries 次失败后返回最后一次的错误。
func (c *Client) getRaw(ctx context.Context, rawURL string) ([]byte, error) {
	var lastErr error
	backoff := 400 * time.Millisecond

	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return nil, err
		}
		c.SetHeaders(req)
		req.Header.Set("Accept", "application/json, text/plain, */*")

		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()

		if rerr != nil {
			lastErr = rerr
			continue
		}
		// 5xx 与 429 值得重试；412 是风控，重试无用。
		if resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
		return body, nil
	}
	return nil, fmt.Errorf("请求失败（已重试）: %w", lastErr)
}

// get 请求 api.bilibili.com 上的接口并把 data 字段解码到 out。
func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	full := apiBase + path
	if q := params.Encode(); q != "" {
		full += "?" + q
	}
	return c.getURL(ctx, full, out)
}

// getSigned 与 get 相同，但先对参数做 WBI 签名。
func (c *Client) getSigned(ctx context.Context, path string, params url.Values, out any) error {
	if params == nil {
		params = url.Values{}
	}
	signed, err := c.signWBI(ctx, params)
	if err != nil {
		return err
	}
	return c.get(ctx, path, signed, out)
}

// getDataRaw 只取出 data 字段，**不因为业务码非 0 而报错**，同时把 code/message 返回。
//
// 存在的理由：/x/web-interface/nav 在未登录时会返回 code=-101（账号未登录），
// 但 data 里依然带着 wbi_img 两个密钥。如果按常规在 code!=0 时直接抛错，
// 就会永远取不到 WBI 密钥，导致后续所有签名接口全部 403。
func (c *Client) getDataRaw(ctx context.Context, path string, params url.Values) (json.RawMessage, int, string, error) {
	if params == nil {
		params = url.Values{}
	}
	full := apiBase + path
	if q := params.Encode(); q != "" {
		full += "?" + q
	}
	raw, err := c.getRaw(ctx, full)
	if err != nil {
		return nil, 0, "", err
	}
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, 0, "", fmt.Errorf("响应不是合法 JSON（可能是风控页）: %w", err)
	}
	return env.Data, env.Code, env.Message, nil
}

// getURL 直接请求完整 URL（调用方负责参数与签名）。
func (c *Client) getURL(ctx context.Context, full string, out any) error {
	raw, err := c.getRaw(ctx, full)
	if err != nil {
		return err
	}
	var env struct {
		Code    int             `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("响应不是合法 JSON（可能是风控页）: %w", err)
	}
	if env.Code != CodeOK {
		return &APIError{Code: env.Code, Message: env.Message}
	}
	if out == nil || len(env.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("解析 data 失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 小工具

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
