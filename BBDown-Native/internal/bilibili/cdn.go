package bilibili

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// mirrorHosts 是 B 站常用的镜像主机。当原始地址被 CDN 拒绝（403）时依次替换尝试。
//
// 原理：upos-sz-xxx.bilivideo.com 这类主机在不同网络下连通性差异很大，
// 换一个主机往往就能下载。BBDown 的 --force-replace-host 做的就是这件事。
var mirrorHosts = []string{
	"upos-sz-mirrorcos.bilivideo.com",
	"upos-sz-mirrorali.bilivideo.com",
	"upos-sz-mirrorhw.bilivideo.com",
	"upos-sz-mirroraliov.bilivideo.com",
	"upos-sz-mirrorcosov.bilivideo.com",
	"upos-sz-mirrorhwov.bilivideo.com",
	"upos-sz-mirror08c.bilivideo.com",
	"upos-sz-mirror08h.bilivideo.com",
	"upos-sz-mirrorbfs.bilivideo.com",
	"upos-hz-mirrorakam.akamaized.net",
}

// ReplaceHost 把 URL 的主机替换成指定的镜像主机。
func ReplaceHost(rawURL, host string) (string, error) {
	i := strings.Index(rawURL, "://")
	if i < 0 {
		return "", fmt.Errorf("地址格式不对: %s", rawURL)
	}
	rest := rawURL[i+3:]
	j := strings.IndexByte(rest, '/')
	if j < 0 {
		return "", fmt.Errorf("地址里没有路径: %s", rawURL)
	}
	return rawURL[:i+3] + host + rest[j:], nil
}

// MirrorURLs 返回某个地址的所有候选形态：原地址 + 各镜像主机。
func MirrorURLs(rawURL string) []string {
	out := []string{rawURL}
	for _, h := range mirrorHosts {
		if u, err := ReplaceHost(rawURL, h); err == nil {
			out = append(out, u)
		}
	}
	return out
}

// ProbeRange 拉取指定字节数，用于验证 CDN 是否可用以及测量首字节延迟。
// 顺带确认服务端支持 Range（离线下载要靠它做断点续传）。
func ProbeRange(ctx context.Context, c *Client, rawURL string, n int64) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	c.SetHeaders(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))

	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	got, err := io.Copy(io.Discard, io.LimitReader(resp.Body, n))
	if err != nil {
		return got, err
	}
	return got, nil
}

// StreamSize 用 Range 请求探测流的真实总字节数。
// 返回 -1 表示服务端没给 Content-Range。
func (c *Client) StreamSize(ctx context.Context, rawURL string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return -1, err
	}
	c.SetHeaders(req)
	req.Header.Set("Range", "bytes=0-0")

	resp, err := c.hc.Do(req)
	if err != nil {
		return -1, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	if cr := resp.Header.Get("Content-Range"); cr != "" {
		// 形如 "bytes 0-0/12345678"
		if i := strings.LastIndexByte(cr, '/'); i >= 0 {
			var total int64
			if _, err := fmt.Sscanf(cr[i+1:], "%d", &total); err == nil && total > 0 {
				return total, nil
			}
		}
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength, nil
	}
	return -1, nil
}

// DefaultTimeout 是单次请求的默认超时。
const DefaultTimeout = 45 * time.Second
