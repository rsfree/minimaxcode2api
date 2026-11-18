package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/imageproto"
	"github.com/linguo2625469/minimaxcode2api/internal/media"
)

// 理解类接口：图 / 视频 / 音频问答。
//
//	POST /v1/images/describe      ⇄ images_understand（目录里叫 matrix_describe_images）
//	POST /v1/videos/understand    ⇄ videos_understand
//	POST /v1/audio/understand     ⇄ audios_understand
//
// 为什么不塞进 /v1/chat/completions：上游文本链路（/mavis/api/v1/llm/v1/messages）
// 只解 image_url 一种媒体块，视频/音频块能不能过是**未验证**的；而这三个工具是
// 上游明确提供的能力。把不确定的入口做成确定的路由，比赌 chat 端点认视频块稳。
//
// 出参结构上游未公开，这里按 media.Texts 的候选键提取；一条都认不出来时报 502，
// 而不是返回一个空文本让调用方以为"模型没说话"。

const maxUnderstandItems = 10

type understandItem struct {
	URL      string `json:"url"`
	Data     string `json:"data"`
	MimeType string `json:"mime_type"`
	Prompt   string `json:"prompt"`
}

type understandRequest struct {
	// Items 是本路由的 canonical 入参。
	Items []understandItem `json:"items"`
	// Prompt 是逐项的默认提示词，item.prompt 优先。
	Prompt string `json:"prompt"`
}

// handleDescribeImages 图片问答。
func (s *Server) handleDescribeImages(w http.ResponseWriter, r *http.Request) {
	s.runUnderstand(w, r, media.ToolDescribeImages, "image_info",
		"请描述这张图片的内容", "图片")
}

// handleVideosUnderstand 视频问答。
func (s *Server) handleVideosUnderstand(w http.ResponseWriter, r *http.Request) {
	s.runUnderstand(w, r, media.ToolVideosUnderstand, "video_info",
		"请描述这段视频的内容", "视频")
}

// handleAudioUnderstand 音频问答（与 /v1/audio/transcriptions 的区别：
// 那个是纯转写，这个是带提示词的理解）。
func (s *Server) handleAudioUnderstand(w http.ResponseWriter, r *http.Request) {
	s.runUnderstand(w, r, media.ToolAudiosUnderstand, "audio_info",
		"请转写并说明这段音频的内容", "音频")
}

// runUnderstand 是三个理解类端点的共同执行体。defaultPrompt 会在调用方没给
// 任何提示词时兜底 —— 上游这些工具是"带问题去看"的，空 prompt 语义不明确。
func (s *Server) runUnderstand(
	w http.ResponseWriter, r *http.Request,
	tool, wrapper, defaultPrompt, noun string,
) {
	start := time.Now()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxImageBody))
	if err != nil {
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_json", Message: "读取请求体失败: " + err.Error(),
		})
		return
	}
	var req understandRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_json", Message: "请求体不是合法 JSON: " + err.Error(),
		})
		return
	}
	if perr := s.validateUnderstand(&req, noun); perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeImageError(w, perr)
		return
	}

	items, perr := s.buildUnderstandItems(r.Context(), &req, defaultPrompt)
	if perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeImageError(w, perr)
		return
	}

	raw, err := s.callMedia(r.Context(), tool, map[string]any{wrapper: items})
	if err != nil {
		perr := imageErrorFromMedia(err)
		s.logMedia(r, "", perr.Status, start, err)
		if perr.Status == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "5")
		}
		writeImageError(w, perr)
		return
	}

	texts := media.Texts(raw)
	if len(texts) == 0 {
		msg := media.FailureText(raw)
		if msg == "" {
			msg = "上游没有返回可识别的文本结果（该工具出参未公开，可能已变更）"
		}
		s.logMedia(r, "", http.StatusBadGateway, start, errors.New(msg))
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "bad_upstream", Message: msg,
		})
		return
	}

	data := make([]map[string]any, 0, len(texts))
	for i, t := range texts {
		data = append(data, map[string]any{"index": i, "text": t})
	}
	out := map[string]any{"created": time.Now().Unix(), "data": data}
	// 条数对不上是"上游少给了"，必须说；静默截断会让调用方以为漏掉的那几条是空回答。
	if len(texts) != len(req.Items) {
		out["warnings"] = []string{fmt.Sprintf(
			"上游返回 %d 条结果，与请求的 %d 条不一致", len(texts), len(req.Items))}
	}
	s.logMedia(r, "", http.StatusOK, start, nil)
	writeJSON(w, http.StatusOK, out)
}

// buildUnderstandItems 把 items 折成上游的 *_info 数组。
//
// 关键一步：上游这些工具有两种媒体来源 —— 本环境的 cdn_url，或内联 base64，
// **不接受任意公网 URL**（实测原文见 fetchmedia.go）。所以客户端给的普通 URL
// 由网关下载后内联成 base64；这样理解类不受 upload_to_cdn 那 600KiB 的限制，
// 可以用到 maxInlineFetch。
func (s *Server) buildUnderstandItems(
	ctx context.Context, req *understandRequest, defaultPrompt string,
) ([]any, *imageproto.ProtocolError) {
	out := make([]any, 0, len(req.Items))
	for i, it := range req.Items {
		m := map[string]any{}
		u := strings.TrimSpace(it.URL)
		d := strings.TrimSpace(it.Data)
		switch {
		case d != "":
			m["data"] = d
			if v := strings.TrimSpace(it.MimeType); v != "" {
				m["mime_type"] = v
			}
		case isUpstreamCDN(u):
			m["url"] = u
		default:
			body, mime, err := s.fetchExternal(ctx, u, maxInlineFetch)
			if err != nil {
				return nil, &imageproto.ProtocolError{
					Status: http.StatusBadRequest, Type: "invalid_request_error",
					Param: fmt.Sprintf("items[%d].url", i), Code: "fetch_failed",
					Message: "取资源失败: " + err.Error(),
				}
			}
			m["data"] = base64.StdEncoding.EncodeToString(body)
			if mime != "" {
				m["mime_type"] = mime
			}
		}
		prompt := firstNonEmptyStr(strings.TrimSpace(it.Prompt), strings.TrimSpace(req.Prompt))
		if prompt == "" {
			prompt = defaultPrompt
		}
		m["prompt"] = prompt
		out = append(out, m)
	}
	return out, nil
}

func (s *Server) validateUnderstand(req *understandRequest, noun string) *imageproto.ProtocolError {
	if len(req.Items) == 0 {
		return &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Param: "items", Code: "invalid_request",
			Message: "items 至少一项",
		}
	}
	if len(req.Items) > maxUnderstandItems {
		return &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Param: "items", Code: "invalid_request",
			Message: fmt.Sprintf("items 最多 %d 项（上游限制），收到 %d", maxUnderstandItems, len(req.Items)),
		}
	}
	for i, it := range req.Items {
		if strings.TrimSpace(it.URL) == "" && strings.TrimSpace(it.Data) == "" {
			return &imageproto.ProtocolError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Param: fmt.Sprintf("items[%d]", i), Code: "invalid_request",
				Message: fmt.Sprintf("items[%d] 需要 url 或 data（%s）", i, noun),
			}
		}
	}
	return nil
}
