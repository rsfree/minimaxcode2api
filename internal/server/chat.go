package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/convert"
	"github.com/linguo2625469/minimaxcode2api/internal/modelcat"
	"github.com/linguo2625469/minimaxcode2api/internal/reqlog"
	"github.com/linguo2625469/minimaxcode2api/internal/upstream"
)

// handleChatCompletions OpenAI 兼容聊天入口。
//
// 完整链路：解析 → 解析模型别名 → 构造 Anthropic 请求 → 选号打上游
// → 流式/非流式转换 → 记账。
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
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

	var req convert.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		entry.Status = http.StatusBadRequest
		// 区分「JSON 语法坏」与「类型不对」：messages 传成字符串时 JSON 本身
		// 是合法的，报 invalid_json 会让调用方去查转义而不是字段类型。
		var typeErr *json.UnmarshalTypeError
		code := "invalid_json"
		if errors.As(err, &typeErr) {
			code = "invalid_request"
		}
		entry.Error = "请求体解析失败: " + err.Error()
		writeOpenAIError(w, http.StatusBadRequest, code, entry.Error)
		return
	}
	if len(req.Messages) == 0 {
		entry.Status = http.StatusBadRequest
		entry.Error = "messages 不能为空"
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", entry.Error)
		return
	}

	cfg := s.Cfg.Load()
	model := s.resolveModel(req.Model)
	if s.isHidden(model) {
		entry.Status = http.StatusNotFound
		entry.Error = "模型已隐藏: " + model
		writeOpenAIError(w, http.StatusNotFound, "model_not_found", entry.Error)
		return
	}
	// 清单里同时有图片/视频模型，发错入口就直接指路，不要透传给上游。
	if kind := s.kindOf(model); kind != modelcat.Chat {
		entry.Status = http.StatusBadRequest
		entry.Error = misrouteMessage(model, kind)
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", entry.Error)
		return
	}
	entry.Model = model
	entry.Stream = req.Stream

	capability, known := convert.CapabilityOf(model)
	payload, err := convert.BuildAnthropicRequest(&req, convert.RequestOptions{
		Model:          model,
		MaxTokens:      cfg.Defaults.MaxTokens,
		ThinkingPolicy: cfg.Defaults.ThinkingPolicy,
		Capability:     capability,
		Known:          known,
	})
	if err != nil {
		entry.Status = http.StatusBadRequest
		entry.Error = err.Error()
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		entry.Status = http.StatusInternalServerError
		entry.Error = err.Error()
		writeOpenAIError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}

	res, apiErr, err := s.doUpstream(r.Context(), body, req.Stream)
	if err != nil {
		status, code := upstreamStatus(err)
		msg := err.Error()
		if apiErr != nil {
			msg = apiErr.Error()
		}
		entry.Status = status
		entry.Error = msg
		if code == "no_account" {
			w.Header().Set("Retry-After", "5")
		}
		writeOpenAIError(w, status, code, msg)
		return
	}
	defer res.Lease.Release()

	entry.Account = s.accountLabel(res.Acct.ID)
	entry.AccountID = res.Acct.ID
	entry.Attempts = res.Attempts
	entry.Status = http.StatusOK

	if req.Stream {
		includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
		p, o, c, err := s.writeOpenAIStream(w, r, res, model, includeUsage)
		promptTokens, outputTokens, cachedTokens = p, o, c
		if err != nil {
			entry.Error = err.Error()
			entry.Status = 499 // 客户端断开/上游中断，语义上比 200 更准确
			log.Printf("[stream] %s: %v", entry.Model, err)
		}
		return
	}

	resp, err := decodeAnthropic(res.Resp)
	if err != nil {
		entry.Status = http.StatusBadGateway
		entry.Error = err.Error()
		writeOpenAIError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	out := convert.ToOpenAI(resp, model, "")
	promptTokens = out.Usage.PromptTokens
	outputTokens = out.Usage.CompletionTokens
	if out.Usage.PromptTokensDetails != nil {
		cachedTokens = out.Usage.PromptTokensDetails.CachedTokens
	}
	s.Store.TouchUsage(res.Acct.ID, promptTokens, outputTokens)
	writeJSON(w, http.StatusOK, out)
}

// decodeAnthropic 读取并校验一个非流式上游响应。
func decodeAnthropic(resp *upstream.Response) (*convert.AnthropicResponse, error) {
	var out convert.AnthropicResponse
	if err := upstream.ReadJSON(resp, &out); err != nil {
		return nil, err
	}
	if out.BaseResp != nil && out.BaseResp.StatusCode != 0 {
		return nil, fmt.Errorf("上游业务错误 %d: %s", out.BaseResp.StatusCode, out.BaseResp.StatusMsg)
	}
	if len(out.Content) == 0 && out.StopReason == "" {
		return nil, fmt.Errorf("上游返回空响应")
	}
	return &out, nil
}

// writeOpenAIStream 把 Anthropic SSE 转成 OpenAI 流式分片。
// 返回最终 usage（用于记账）。
func (s *Server) writeOpenAIStream(w http.ResponseWriter, r *http.Request, res *upstreamResult, model string, includeUsage bool) (int, int, int, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return 0, 0, 0, fmt.Errorf("响应流不支持 flush")
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	conv := convert.NewStreamConverter(model, "")
	reader := bufio.NewReaderSize(res.Resp.Body, 64<<10)

	writeData := func(payload []byte) error {
		if _, err := w.Write([]byte("data: ")); err != nil {
			return err
		}
		if _, err := w.Write(payload); err != nil {
			return err
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	var streamErr error
	err := readSSE(reader, func(ev sseEvent) error {
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		default:
		}
		switch ev.Event {
		case "error":
			streamErr = parseStreamError(ev.Data)
			// 已经写了 200 头，只能把错误作为分片发出。
			errBody, _ := json.Marshal(map[string]any{
				"error": map[string]any{
					"message": streamErr.Error(),
					"type":    "upstream_error",
					"code":    "upstream_stream_error",
				},
			})
			_ = writeData(errBody)
			return streamErr
		default:
			for _, chunk := range conv.Handle(ev.Event, ev.Data) {
				if err := writeData(chunk); err != nil {
					return err
				}
			}
			return nil
		}
	})

	if err == nil {
		if includeUsage {
			if u := conv.EncodeUsage(); u != nil {
				_ = writeData(u)
			}
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}
	if streamErr != nil {
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}

	s.Store.TouchUsage(res.Acct.ID, conv.PromptTokens(), conv.OutputTokens())
	return conv.PromptTokens(), conv.OutputTokens(), 0, err
}

// clientInfo 取调用方 IP 与 UA（面板日志用）。
func (s *Server) clientInfo(r *http.Request) (string, string) {
	if !s.Cfg.Load().Logging.CaptureClientInfo {
		return "", ""
	}
	ip := r.Header.Get("CF-Connecting-IP")
	if ip == "" {
		ip = r.Header.Get("X-Forwarded-For")
		if i := strings.Index(ip, ","); i > 0 {
			ip = ip[:i]
		}
	}
	if ip == "" {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err == nil {
			ip = host
		} else {
			ip = r.RemoteAddr
		}
	}
	ua := r.Header.Get("User-Agent")
	if len(ua) > 160 {
		ua = ua[:160]
	}
	return strings.TrimSpace(ip), ua
}

func (s *Server) accountLabel(id string) string {
	if a, ok := s.Store.Get(id); ok {
		if a.Label != "" {
			return a.Label
		}
		return a.ID
	}
	return id
}
