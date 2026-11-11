package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/linguo2625469/minimaxcode2api/internal/pool"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
	"github.com/linguo2625469/minimaxcode2api/internal/upstream"
)

// sseEvent 一条 SSE 事件。
type sseEvent struct {
	Event string
	Data  []byte
}

// readSSE 逐事件解析 SSE 流。
//
// 按规范处理：事件由空行分隔；data 可多行；只关心 data 与 event，
// id/retry 一律忽略（上游也不用）。读到 EOF 时把残留事件交出去，
// 避免最后一帧没有尾随空行时被吞掉。
func readSSE(r *bufio.Reader, fn func(sseEvent) error) error {
	var ev string
	var data []byte

	flush := func() error {
		if len(data) == 0 {
			ev = ""
			return nil
		}
		e := sseEvent{Event: ev, Data: data}
		ev = ""
		data = nil
		return fn(e)
	}

	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if err := flush(); err != nil {
					return err
				}
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				d := strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
				if len(data) > 0 {
					data = append(data, '\n')
				}
				data = append(data, d...)
			case strings.HasPrefix(line, ":"):
				// 注释/心跳，忽略。
			}
		}
		if err != nil {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// upstreamStreamError 上游在流里回的 error 事件。
type upstreamStreamError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e *upstreamStreamError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "upstream stream error"
}

// parseStreamError 尝试把 error 事件解析成结构化错误。
func parseStreamError(data []byte) *upstreamStreamError {
	var probe struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return &upstreamStreamError{Message: string(data)}
	}
	e := &upstreamStreamError{Type: probe.Error.Type, Message: firstNonEmptyStr(probe.Error.Message, probe.Message)}
	if e.Type == "" {
		e.Type = "api_error"
	}
	return e
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// upstreamResult 一次成功的上游调用。Lease 必须由调用方释放，
// 流式期间保持「在途」状态，避免并发闸门把账号误判为空闲。
type upstreamResult struct {
	Resp     *upstream.Response
	Acct     store.Account
	Lease    *pool.Lease
	Attempts int
}

// doUpstream 选号 → 取新鲜 token → 打上游；失败自动换号重试。
//
// 重试边界：
//   - 401/403 先对**同一个账号**强制刷新 token 再打一次（token 过期是最常见原因）；
//   - 429/5xx/网络错误：冷却该账号，换下一个号；
//   - 400 这类请求本身的问题不换号（换号也一样失败），直接把上游错误透回去。
func (s *Server) doUpstream(ctx context.Context, body []byte, stream bool) (*upstreamResult, *upstream.APIError, error) {
	const maxAttempts = 3
	exclude := map[string]bool{}
	var lastErr error
	var lastAPI *upstream.APIError

	for attempt := 0; attempt < maxAttempts; attempt++ {
		lease, err := s.Pool.Pick(exclude)
		if err != nil {
			if lastErr != nil {
				return nil, lastAPI, lastErr
			}
			return nil, nil, err
		}
		acct := lease.Account

		var resp *upstream.Response
		for authRetry := 0; authRetry < 2; authRetry++ {
			fresh, ferr := s.ensureFresh(ctx, acct, authRetry > 0)
			if ferr != nil {
				lastErr = ferr
				resp = nil
				break
			}
			acct = fresh
			resp, err = s.Up.Do(ctx, acct.AccessToken, body, stream)
			if err != nil {
				lastErr = err
				s.Pool.Failure(acct.ID, 0)
				resp = nil
				break
			}
			if resp.StatusCode == 200 {
				s.Pool.Success(acct.ID)
				return &upstreamResult{Resp: resp, Acct: acct, Lease: lease, Attempts: attempt + 1}, nil, nil
			}
			apiErr := upstream.ParseError(resp.StatusCode, resp.Body)
			resp.Body.Close()
			lastAPI = apiErr
			lastErr = apiErr

			if apiErr.AuthError() && authRetry == 0 && acct.RefreshToken != "" {
				s.noteAccountError(acct.ID, apiErr)
				continue // 强制刷新后同账号再试
			}
			s.Pool.Failure(acct.ID, apiErr.Status)
			s.noteAccountError(acct.ID, apiErr)
			resp = nil
			break
		}

		lease.Release()
		exclude[acct.ID] = true

		// 请求本身非法（400/404/422）→ 换号也没用，直接返回。
		if lastAPI != nil && !lastAPI.Retryable() {
			return nil, lastAPI, lastAPI
		}
	}
	if lastErr == nil {
		lastErr = errors.New("上游未返回可用响应")
	}
	return nil, lastAPI, lastErr
}

// noteAccountError 把最近一次错误写进账号，面板直接可见。
func (s *Server) noteAccountError(id string, e *upstream.APIError) {
	if e == nil {
		return
	}
	msg := e.Error()
	if len(msg) > 300 {
		msg = msg[:300]
	}
	s.Store.Update(id, func(a *store.Account) {
		a.LastError = msg
	})
}
