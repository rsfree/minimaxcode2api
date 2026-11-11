// Package media 调用 MiniMax Code 的媒体网关。
//
// 文本走 /mavis/api/v1/llm/v1/messages（Anthropic Messages 协议），媒体走
// /mavis/api/v1/mcp/<tool> —— 是两条独立通道，鉴权相同（Bearer，无需签名）。
//
// 一个必须记住的差异：**媒体接口永远返回 HTTP 200**，业务错误放在 body 的
// code 字段里。所以调用方不能只看状态码，必须解析 code：
//
//	0    成功
//	400  参数问题（换号也一样失败，直接透出）
//	402  计费拦截（余额/套餐不够，重试无意义）
//	401  账号失效（该换号）
//
// 忽略这一点的后果是：余额不足被当成"成功"，客户端拿到一个没有 output_url
// 的空响应。
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// 工具名（对应上游 /mavis/api/v1/mcp/<tool>）。
const (
	ToolImageSynthesize  = "image_synthesize"
	ToolVideoSubmit      = "submit_video_generation"
	ToolVideoQuery       = "query_video_generation"
	ToolSpeech           = "synthesize_speech"
	ToolTranscribe       = "listen_audio"
	ToolVoiceList        = "get_voice_list"
	ToolMusic            = "batch_text_to_music"
	ToolTextToAudio      = "batch_text_to_audio"
	ToolImagesUnderstand = "images_understand"
)

// maxBody 限制单次响应体大小。媒体接口只回 URL 与元数据，8MB 足够宽裕；
// 真有大 body 说明上游变了，宁可报错也不要吃满内存。
const maxBody = 8 << 20

// Client 媒体网关客户端。
type Client struct {
	HTTP      *http.Client
	UserAgent string
}

// New 构造客户端。timeout 覆盖生成类工具（服务端可能跑数分钟）。
func New(timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	return &Client{
		HTTP:      &http.Client{Timeout: timeout},
		UserAgent: "MiniMaxAgent",
	}
}

// Error 上游业务错误。
type Error struct {
	// HTTPStatus 传输层状态码；业务错误时为 200。
	HTTPStatus int
	// Code 上游 body 里的业务码（400/401/402/...）。
	Code int
	// Message 上游原话。402 的原文很长但信息量足，原样透给调用方更有用。
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("media upstream: http=%d code=%d %s", e.HTTPStatus, e.Code, e.Message)
}

// IsBilling 是否是计费拦截（不该重试）。
func (e *Error) IsBilling() bool { return e.Code == http.StatusPaymentRequired }

// IsRequest 是否是请求本身的错（不该换号）。
func (e *Error) IsRequest() bool {
	return e.Code == http.StatusBadRequest || e.Code == http.StatusUnprocessableEntity
}

// IsAuth 是否是凭据问题（该刷新/换号）。
func (e *Error) IsAuth() bool {
	return e.HTTPStatus == http.StatusUnauthorized || e.HTTPStatus == http.StatusForbidden ||
		e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden
}

// IsPoll 是否是"稍后再查"（视频转码/下发准备中）。
//
// 这类错误必须原样传给客户端并保留轮询状态：换号或重新提交都会打乱
// 已经计费的任务。
func (e *Error) IsPoll() bool { return e.Code == http.StatusServiceUnavailable }

// Envelope 所有 mcp 响应的公共外壳。
type Envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Call 调一个媒体工具，返回原始 JSON（已确认 code==0）。
func (c *Client) Call(ctx context.Context, baseURL, token, tool string, payload any) (json.RawMessage, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal media payload: %w", err)
	}
	url := strings.TrimRight(baseURL, "/") + "/" + tool
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("media request %s: %w", tool, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read media response %s: %w", tool, err)
	}
	if resp.StatusCode != http.StatusOK {
		return raw, &Error{
			HTTPStatus: resp.StatusCode,
			Code:       resp.StatusCode,
			Message:    truncate(string(raw), 400),
		}
	}

	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("media response %s 不是合法 JSON: %w", tool, err)
	}
	if env.Code != 0 {
		// 注意：即使 code != 0 也把 raw 带回去。视频轮询会出现
		// code=503 + status=succeeded（"已就绪但还没准备好下发"），
		// 调用方必须能看到 status 才知道该继续轮询而不是判失败。
		return raw, &Error{HTTPStatus: resp.StatusCode, Code: env.Code, Message: env.Message}
	}
	return raw, nil
}

// StatusOf 把 media.Error 映射成对外 HTTP 状态码。
func StatusOf(err error) int {
	var me *Error
	if errors.As(err, &me) {
		switch {
		case me.IsBilling():
			return http.StatusPaymentRequired
		case me.IsAuth():
			return http.StatusUnauthorized
		case me.IsRequest():
			return http.StatusBadRequest
		case me.HTTPStatus == http.StatusTooManyRequests:
			return http.StatusTooManyRequests
		}
	}
	return http.StatusBadGateway
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
