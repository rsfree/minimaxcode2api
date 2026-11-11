package server

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/convert"
	"github.com/linguo2625469/minimaxcode2api/internal/reqlog"
)

// handleMessages Anthropic Messages 直通入口（Claude Code / Anthropic SDK 可直接接）。
//
// 这一条不做协议翻译：客户端本来就是 Anthropic 形态，网关只负责
// 模型别名解析、选号、token 刷新与记账，把上游响应原样透回。
// 这样处理的好处是 tool_use / thinking / cache_control 全部零损耗。
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	entry := reqlog.Entry{Time: start, Method: r.Method, Path: r.URL.Path}
	entry.ClientIP, entry.UA = s.clientInfo(r)

	var promptTokens, outputTokens, cachedTokens int
	defer func() {
		entry.Duration = time.Since(start).Milliseconds()
		entry.Prompt, entry.Output, entry.Cached = promptTokens, outputTokens, cachedTokens
		s.Logs.Add(entry)
		s.Usage.Record(entry.Model, entry.Account, promptTokens, outputTokens, cachedTokens, time.Since(start), entry.Status >= 400)
		s.markUsageDirty()
	}()

	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = err.Error()
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = "请求体不是合法 JSON: " + err.Error()
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", entry.Error)
		return
	}

	model := s.resolveModel(probe.Model)
	entry.Model = model
	entry.Stream = probe.Stream

	// 只改写 model 字段，其余原样保留。
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		mv, _ := json.Marshal(model)
		obj["model"] = mv
		if body, err := json.Marshal(obj); err == nil {
			raw = body
		}
	}

	res, apiErr, err := s.doUpstream(r.Context(), raw, probe.Stream)
	if err != nil {
		status, _ := upstreamStatus(err)
		msg := err.Error()
		if apiErr != nil {
			msg = apiErr.Error()
		}
		entry.Status = status
		entry.Error = msg
		writeAnthropicError(w, status, "api_error", msg)
		return
	}
	defer res.Lease.Release()

	entry.Account = s.accountLabel(res.Acct.ID)
	entry.AccountID = res.Acct.ID
	entry.Attempts = res.Attempts
	entry.Status = http.StatusOK

	if probe.Stream {
		p, o, c := s.pipeAnthropicStream(w, r, res)
		promptTokens, outputTokens, cachedTokens = p, o, c
		return
	}

	defer res.Resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Resp.Body, 32<<20))
	if err != nil {
		entry.Status = http.StatusBadGateway
		entry.Error = err.Error()
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	// 上游会在末尾塞一个 base_resp 业务状态字段，Anthropic SDK 不认识；
	// 直通时删掉它，其余字段（含我们没建模的新字段）一律原样保留。
	body = stripEnvelope(body)

	var parsed convert.AnthropicResponse
	if err := json.Unmarshal(body, &parsed); err == nil {
		promptTokens = parsed.Usage.InputTokens + parsed.Usage.CacheCreationInputTokens + parsed.Usage.CacheReadInputTokens
		outputTokens = parsed.Usage.OutputTokens
		cachedTokens = parsed.Usage.CacheReadInputTokens
		s.Store.TouchUsage(res.Acct.ID, promptTokens, outputTokens)
	}
	if ct := res.Resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	w.WriteHeader(res.Resp.StatusCode)
	_, _ = w.Write(body)
}

// stripEnvelope 删掉上游私有信封字段（base_resp）。
//
// 只碰这一个键：其它未知字段保持原样，避免"顺手清理"把上游新增的
// 有用字段弄丢；解析失败时原样返回，宁可多透一个字段也不能弄坏响应。
func stripEnvelope(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if _, ok := obj["base_resp"]; !ok {
		return body
	}
	delete(obj, "base_resp")
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// pipeAnthropicStream 原样透传上游 SSE，同时顺手统计 usage。
func (s *Server) pipeAnthropicStream(w http.ResponseWriter, r *http.Request, res *upstreamResult) (int, int, int) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "响应流不支持 flush")
		return 0, 0, 0
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var prompt, output, cached int
	reader := bufio.NewReaderSize(res.Resp.Body, 64<<10)

	_ = readSSE(reader, func(ev sseEvent) error {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}
		// 记账：只解析关心的两个事件，其余原样转发。
		if ev.Event == "message_start" {
			var m struct {
				Message struct {
					Usage convert.AnthropicUsage `json:"usage"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Data, &m) == nil {
				prompt = m.Message.Usage.InputTokens + m.Message.Usage.CacheCreationInputTokens + m.Message.Usage.CacheReadInputTokens
				cached = m.Message.Usage.CacheReadInputTokens
			}
		}
		if ev.Event == "message_delta" {
			var m struct {
				Usage convert.AnthropicUsage `json:"usage"`
			}
			if json.Unmarshal(ev.Data, &m) == nil && m.Usage.OutputTokens > 0 {
				output = m.Usage.OutputTokens
			}
		}

		if ev.Event != "" {
			if _, err := w.Write([]byte("event: " + ev.Event + "\n")); err != nil {
				return err
			}
		}
		if _, err := w.Write([]byte("data: ")); err != nil {
			return err
		}
		if _, err := w.Write(ev.Data); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	})

	s.Store.TouchUsage(res.Acct.ID, prompt, output)
	return prompt, output, cached
}
