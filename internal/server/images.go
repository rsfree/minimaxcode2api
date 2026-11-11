package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/imageproto"
	"github.com/linguo2625469/minimaxcode2api/internal/media"
)

// 图片接口按 OpenAI 图像协议的 canonical 契约实现：
//
//	POST /v1/images/generations
//
// 只保留这一个入口：canonical 就是 JSON，图片引用用字符串（URL / data URI /
// 裸 base64），不引 multipart —— 少一个载体前门就少一套「哪些字段能传」的歧义，
// 也不用替客户端去公网取图。

const maxImageBody = 64 << 20

// imageSynthResponse 是上游 image_synthesize 的响应（槽位制）。
type imageSynthResponse struct {
	Summary        string `json:"summary"`
	TotalRequested int    `json:"total_requested"`
	TotalSuccess   int    `json:"total_success"`
	TotalFailed    int    `json:"total_failed"`
	SuccessItems   []struct {
		OutputFile     string `json:"output_file"`
		OutputURL      string `json:"output_url"`
		TextResponse   string `json:"text_response"`
		HasImageOutput bool   `json:"has_image_output"`
		CleanOSSKey    string `json:"clean_oss_key"`
		VisibleOSSKey  string `json:"visible_oss_key"`
	} `json:"success_items"`
	FailedItems []struct {
		OutputFile string `json:"output_file"`
		ErrCode    any    `json:"err_code"`
		ErrorMsg   string `json:"error_msg"`
	} `json:"failed_items"`
}

// handleImages 文生图 / 图生图（JSON 入口）。
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxImageBody))
	if err != nil {
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_json", Message: "读取请求体失败: " + err.Error(),
		})
		return
	}
	req, perr := imageproto.Decode(raw)
	if perr == nil {
		var payload map[string]any
		var warnings []string
		payload, warnings, perr = req.Build()
		if perr == nil {
			s.runImageGeneration(w, r, req, payload, warnings, start)
			return
		}
	}
	s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
	writeImageError(w, perr)
}

// runImageGeneration 是两条前门共用的执行体。
func (s *Server) runImageGeneration(
	w http.ResponseWriter, r *http.Request,
	req *imageproto.Request, payload map[string]any, warnings []string, start time.Time,
) {
	raw, err := s.callMedia(r.Context(), media.ToolImageSynthesize, payload)
	if err != nil {
		s.logMedia(r, req.Model, http.StatusBadGateway, start, err)
		perr := imageErrorFromMedia(err)
		if perr.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "5")
		}
		writeImageError(w, perr)
		return
	}

	var resp imageSynthResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "bad_upstream", Message: "上游响应无法解析: " + err.Error(),
		})
		return
	}

	// 上游把「模型拒答」表现为 success=false + text_response，而不是 HTTP 错误。
	if len(resp.SuccessItems) == 0 {
		msg := "图片生成失败"
		code := "generation_failed"
		status := http.StatusBadGateway
		if len(resp.FailedItems) > 0 {
			msg = firstNonEmptyStr(resp.FailedItems[0].ErrorMsg, msg)
			if isContentRejection(msg) {
				code, status = "content_filter", http.StatusBadRequest
			}
		}
		s.logMedia(r, req.Model, status, start, errors.New(msg))
		writeImageError(w, &imageproto.ProtocolError{
			Status: status, Type: errorType(status), Code: code, Message: msg,
		})
		return
	}

	wantB64 := req.ResponseFormat == "b64_json"
	type datum struct {
		URL string `json:"url,omitempty"`
		B64 string `json:"b64_json,omitempty"`
	}
	data := make([]datum, 0, len(resp.SuccessItems))
	for _, it := range resp.SuccessItems {
		if !it.HasImageOutput || it.OutputURL == "" {
			continue
		}
		if wantB64 {
			body, _, ferr := s.fetchBytes(r.Context(), it.OutputURL)
			if ferr != nil {
				writeImageError(w, &imageproto.ProtocolError{
					Status: http.StatusBadGateway, Type: "upstream_error",
					Code: "download_failed", Message: ferr.Error(),
				})
				return
			}
			data = append(data, datum{B64: base64.StdEncoding.EncodeToString(body)})
		} else {
			data = append(data, datum{URL: it.OutputURL})
		}
	}
	if len(data) == 0 {
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "empty_result", Message: "上游称成功但没有产物（可能被内容策略过滤）",
		})
		return
	}

	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	out := map[string]any{"created": time.Now().Unix(), "data": data}
	// warnings 是附加字段（协议允许透传附加键），只在非空时出现。
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, out)
}

// writeImageError 输出四键错误信封（message / type / param / code）。
func writeImageError(w http.ResponseWriter, perr *imageproto.ProtocolError) {
	status := perr.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	typ := perr.Type
	if typ == "" {
		typ = errorType(status)
	}
	writeFourKeyError(w, status, typ, perr.Param, perr.Code, perr.Message)
}

// imageErrorFromMedia 按「谁的错」翻译上游错误。
//
// 401/403 是服务方凭证问题 → 502（客户端改请求没用）；429 保留退避信号；
// 402 是配额/限额触顶 → 402 + insufficient_quota，让运维一眼区分
// 「限额了」与「请求写错了」。限额通常会重置，客户端应稍后重试。
func imageErrorFromMedia(err error) *imageproto.ProtocolError {
	var me *media.Error
	if !errors.As(err, &me) {
		return &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "upstream_unreachable", Message: err.Error(),
		}
	}
	switch {
	case me.IsBilling():
		return &imageproto.ProtocolError{
			Status: http.StatusPaymentRequired, Type: "api_error",
			Code: "insufficient_quota", Message: billingHint(me.Message),
		}
	case me.IsRequest():
		return &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_request", Message: me.Message,
		}
	case me.IsAuth():
		return &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "api_error",
			Code: "upstream_auth", Message: "上游账号不可用: " + me.Message,
		}
	case me.Code == http.StatusTooManyRequests || me.HTTPStatus == http.StatusTooManyRequests:
		return &imageproto.ProtocolError{
			Status: http.StatusTooManyRequests, Type: "api_error",
			Code: "rate_limited", Message: me.Message,
		}
	}
	return &imageproto.ProtocolError{
		Status: http.StatusBadGateway, Type: "upstream_error",
		Code: "upstream_error", Message: me.Message,
	}
}

// isContentRejection 判定上游的"拒答"文案（审核类）。
func isContentRejection(msg string) bool {
	m := strings.ToLower(msg)
	for _, kw := range []string{"sensitive", "risk", "content security", "content policy", "审核", "违规"} {
		if strings.Contains(m, kw) {
			return true
		}
	}
	return false
}

// handleVoices 暴露可用音色。
func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	raw, err := s.callMedia(r.Context(), media.ToolVoiceList, map[string]any{})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	var resp struct {
		AvailableVoices []struct {
			VoiceID   string `json:"voice_id"`
			VoiceName string `json:"voice_name"`
		} `json:"available_voices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(resp.AvailableVoices))
	for _, v := range resp.AvailableVoices {
		out = append(out, map[string]any{"id": v.VoiceID, "name": v.VoiceName, "object": "voice"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}
