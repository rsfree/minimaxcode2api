// Package quota 读取上游账号的「图片生成免费额度」。
//
// 端点：GET {origin}/minimax-cloud/api/v1/image/free-quota
//   - 只要 Authorization: Bearer <mmoat_…>，**不需要 x-signature**
//     （签名只作用于 /matrix 等业务接口，媒体与额度接口都不校验）；
//   - 返回次数口径：{total, used, remaining, active, start_ms, end_ms}；
//   - 与「5 小时/周窗口限额」「积分余额」是三个互不相干的池子 ——
//     实测：连续生成 12+ 张后 free-quota 的 used 仍是 1，而 5 小时窗口被打满。
//
// 本包只做纯 HTTP + 解析，缓存与 token 刷新由 server 层负责。
package quota

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ImageFree 是图片生成免费额度的快照。
type ImageFree struct {
	Total     int   `json:"total"`
	Used      int   `json:"used"`
	Remaining int   `json:"remaining"`
	Active    bool  `json:"active"`
	StartMs   int64 `json:"start_ms"`
	EndMs     int64 `json:"end_ms"`

	// FetchedAt 是本次取数的时刻，供面板显示"多久之前"。
	FetchedAt time.Time `json:"fetched_at"`
}

// Client 调额度接口。
type Client struct {
	HTTP *http.Client
}

func New(hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{HTTP: hc}
}

// Origin 从上游 base_url 推回站点根。
//
// 上游文本地址形如 https://agent.minimax.cn/mavis/api/v1/llm/v1，而额度接口在
// 同站的 /minimax-cloud 前缀下 —— 切 region 时两者一起走，所以从 base_url 推。
func Origin(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// ImageFree 取一次图片免费额度。
func (c *Client) ImageFree(ctx context.Context, origin, accessToken string) (*ImageFree, error) {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return nil, fmt.Errorf("quota: 无法从上游地址推导站点根")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/minimax-cloud/api/v1/image/free-quota", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	// 与主链路一致的 UA：部分网关会按 UA 分流。
	req.Header.Set("User-Agent", "MiniMaxAgent Mozilla/5.0")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("quota: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("quota: HTTP %d %s", resp.StatusCode, snippet(raw))
	}
	var q ImageFree
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, fmt.Errorf("quota: 响应不是合法 JSON: %w", err)
	}
	q.FetchedAt = time.Now()
	return &q, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	r := []rune(s)
	if len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}
