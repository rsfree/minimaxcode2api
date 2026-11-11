// Package upstream 是 MiniMax Code 的 LLM 网关客户端。
//
// 协议事实（已在真机上逐项验证）：
//
//	POST {base}/messages
//	Authorization: Bearer mmoat_xxx
//	anthropic-version: 2023-06-01
//	→ 标准 Anthropic Messages 响应（含 base_resp 附加字段），流式为标准 Anthropic SSE 事件。
//
// 也就是说这一层不需要任何"私有加密/签名"——x-signature 只作用于 /matrix、/minimax-cloud
// 那些业务接口，LLM 网关只认 Bearer。这一点决定了网关可以做得非常薄。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
)

// Client 上游客户端（连接池复用，长流不设总超时）。
type Client struct {
	cfg  *config.Holder
	http *http.Client
}

func New(cfg *config.Holder) *Client {
	return &Client{
		cfg: cfg,
		http: &http.Client{
			// 总超时由 context 控制（流式可能跑很久），这里只兜底连接层面。
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				MaxIdleConns:          64,
				MaxIdleConnsPerHost:   32,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 300 * time.Second,
			},
		},
	}
}

// Response 上游原始响应。Body 由调用方关闭。
type Response struct {
	StatusCode int
	Header     http.Header
	Body       io.ReadCloser
}

// Do 发送一次 Messages 请求。
// body 已经是 Anthropic 形态的 JSON（调用方负责序列化），原样转发可避免字段丢失。
func (c *Client) Do(ctx context.Context, accessToken string, body []byte, stream bool) (*Response, error) {
	cfg := c.cfg.Load()
	url := strings.TrimRight(cfg.Upstream.BaseURL, "/") + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("anthropic-version", cfg.Upstream.AnthropicVersion)
	req.Header.Set("User-Agent", cfg.Upstream.UserAgent)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	return &Response{StatusCode: resp.StatusCode, Header: resp.Header, Body: resp.Body}, nil
}

// APIError 上游返回的结构化错误。
type APIError struct {
	Status  int
	Type    string `json:"type"`
	Message string `json:"message"`
	// Anthropic 形态：{"type":"error","error":{"type":"...","message":"..."}}
	Detail struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
	// 极简形态：{"error":"invalid access token"}
	RawError json.RawMessage `json:"-"`
	raw      string
}

func (e *APIError) Error() string {
	msg := firstNonEmpty(e.Detail.Message, e.Message, e.raw)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("upstream %d: %s", e.Status, msg)
}

// Retryable 是否值得换号重试。
func (e *APIError) Retryable() bool {
	switch {
	case e.Status == http.StatusTooManyRequests:
		return true
	case e.Status >= 500:
		return true
	case e.Status == http.StatusUnauthorized, e.Status == http.StatusForbidden:
		// 401 = token 失效（刷新/换号），403 = 账号被限制。
		return true
	default:
		return false
	}
}

// AuthError token 层面失败，需要刷新或换号。
func (e *APIError) AuthError() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// ParseError 读取并解析错误响应。
func ParseError(status int, body io.Reader) *APIError {
	raw, _ := io.ReadAll(io.LimitReader(body, 64<<10))
	e := &APIError{Status: status, raw: strings.TrimSpace(string(raw))}
	if len(raw) == 0 {
		return e
	}
	var probe struct {
		Type    string          `json:"type"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		e.Type = probe.Type
		e.Message = probe.Message
		if len(probe.Error) > 0 {
			// error 可能是对象，也可能是字符串。
			var obj struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			if err := json.Unmarshal(probe.Error, &obj); err == nil && (obj.Type != "" || obj.Message != "") {
				e.Detail.Type = obj.Type
				e.Detail.Message = obj.Message
			} else {
				var s string
				if err := json.Unmarshal(probe.Error, &s); err == nil {
					e.Detail.Message = s
				}
			}
		}
	}
	return e
}

// ReadJSON 读取一个完整 JSON 响应（非流式）。
func ReadJSON(r *Response, out any) error {
	defer r.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode upstream json: %w (body: %s)", err, truncate(string(raw), 300))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
