package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// 媒体入参的来源归一。
//
// 上游的事实（2026-11-17 实测）：`image_info[].url` / `image_url` / `input_image.url`
// 都**不是**"任意公网 URL"，而是"本环境上传后的 cdn_url"或内联 base64：
//
//	image_info.url could not be used as a media source; download the resource in your
//	local or sandbox workspace first, then call matrix_get_upload_url …
//
// 客户端手里通常只有一个 URL，所以网关代劳：下载 → 上传 CDN（或内联 base64）。
//
// 这是本服务里**唯一**由调用方指定目标地址的出站请求，必须带 SSRF 防护：
//   - 只允许 http/https；
//   - 先解析域名给出明确报错，再用 Dialer.Control 在**实际拨号的那个 IP** 上复核
//     （防止 DNS rebinding：先解析出公网 IP 再换成内网 IP）；
//   - 重定向逐跳复核；
//   - 限制大小与超时。

const (
	// maxCDNUpload 是上游 upload_to_cdn 对 base64 的硬上限。
	maxCDNUpload = 600 << 10
	// maxInlineFetch 是"下载后内联成 base64"的上限（理解类工具走这条路，没有 CDN 门槛）。
	maxInlineFetch = 20 << 20
	// fetchTimeout 覆盖一次普通图片下载。
	fetchTimeout = 30 * time.Second
)

// isUpstreamCDN 判断是否已经是上游自己的媒体地址。
//
// 只放过 cdn.hailuoai.com：这是实测输出 URL 的域名（国际版未见）。其它域名一律
// 下载再上传 —— 宁可多一次往返，也不赌上游认不认。
func isUpstreamCDN(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "cdn.hailuoai.com" || strings.HasSuffix(host, ".cdn.hailuoai.com")
}

// fetchExternal 下载调用方给的公网资源。
func (s *Server) fetchExternal(ctx context.Context, raw string, limit int64) ([]byte, string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, "", fmt.Errorf("URL 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, "", fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	// 先解析一次：解析不出来 / 明确是内网时给出可读的错误。
	// 真正的强制拦截在下面的 Dialer.Control（拨号时的真实 IP）。
	if err := s.checkHostPublic(u.Hostname()); err != nil {
		return nil, "", err
	}

	client := &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout: 10 * time.Second,
				Control: func(network, address string, _ syscall.RawConn) error {
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						return err
					}
					ip := net.ParseIP(host)
					if ip == nil {
						return errors.New("无法解析目标地址")
					}
					if !s.fetchAllowPrivate && blockedIP(ip) {
						return fmt.Errorf("拒绝连接内网地址 %s（SSRF 防护）", ip)
					}
					return nil
				},
			}).DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("重定向次数过多")
			}
			return s.checkHostPublic(req.URL.Hostname())
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("下载失败: HTTP %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, "", err
	}
	if int64(len(body)) > limit {
		return nil, "", fmt.Errorf("资源超过 %d MiB 上限", limit>>20)
	}
	if len(body) == 0 {
		return nil, "", errors.New("下载到的是空内容")
	}
	return body, sniffMime(body, resp.Header.Get("Content-Type")), nil
}

// checkHostPublic 解析域名并检查每一个 IP。
func (s *Server) checkHostPublic(host string) error {
	if s.fetchAllowPrivate {
		return nil
	}
	if host == "" {
		return errors.New("URL 缺少主机名")
	}
	if ip := net.ParseIP(host); ip != nil {
		if blockedIP(ip) {
			return fmt.Errorf("拒绝访问内网地址 %s（SSRF 防护）", ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("域名解析失败: %w", err)
	}
	if len(ips) == 0 {
		return errors.New("域名没有解析到任何地址")
	}
	for _, ip := range ips {
		if blockedIP(ip) {
			return fmt.Errorf("拒绝访问内网地址 %s（SSRF 防护）", ip)
		}
	}
	return nil
}

func blockedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast()
}
