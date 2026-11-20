package server

import (
	"context"
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

// 图像工具：抠图 / 2× 超分 / 以图搜图。
//
//	POST /v1/images/remove-background
//	POST /v1/images/upscale
//	POST /v1/images/reverse-search
//
// 入参只有一种载体：JSON 里的 image 字段（URL / data URI / 裸 base64），与
// /v1/images/generations 的 image 同一套解析规则 —— 不引 multipart，少一个前门
// 就少一套"哪些字段能传"的歧义。
//
// 上游约束（工具说明原文）：抠图与超分只吃**已上传的 cdn_url**，不吃裸 base64。
// 所以本地图要先经上游 upload_to_cdn 中转（≤600KiB）。更大的图本网关不代传：
// 官方给的是 OSS 签名直传，网关不代持对象存储，直接把限制说清楚比发一个注定
// 失败的请求好。

// maxUploadBase64 是上游 upload_to_cdn 对 base64 的硬上限；
// 原始字节上限按 3/4 折算（base64 会膨胀 4/3）。
const (
	maxUploadBase64 = 600 << 10
	maxCDNRaw       = maxUploadBase64 / 4 * 3
)

type imageToolRequest struct {
	Image          string `json:"image"`
	ImageURL       string `json:"image_url"`
	ResponseFormat string `json:"response_format"`
}

// handleRemoveBackground 抠图，回透明 PNG。
func (s *Server) handleRemoveBackground(w http.ResponseWriter, r *http.Request) {
	s.runImageFileTool(w, r, media.ToolRemoveBackground, "remove_background")
}

// handleUpscale 2× 超分（上游固定 2 倍，没有档位可调）。
func (s *Server) handleUpscale(w http.ResponseWriter, r *http.Request) {
	s.runImageFileTool(w, r, media.ToolEnhanceImage, "upscale")
}

// handleReverseImageSearch 以图搜图。出参结构上游未公开，按"第一个数组字段"透出。
func (s *Server) handleReverseImageSearch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	req, perr := s.readImageToolRequest(r)
	var raw []byte
	if perr == nil {
		var cdn string
		cdn, perr = s.cdnURL(r.Context(), req.source())
		if perr == nil {
			raw, perr = s.callImageTool(r, media.ToolReverseImageSearch,
				map[string]any{"image_url": cdn})
		}
	}
	if perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeImageError(w, perr)
		return
	}

	list, ok := media.FirstArray(raw)
	if !ok {
		err := errors.New("上游响应里没有结果数组")
		s.logMedia(r, "", http.StatusBadGateway, start, err)
		writeImageError(w, &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code:    "bad_upstream",
			Message: "上游返回的结构里没有结果数组（该工具出参未公开，可能已变更）",
		})
		return
	}
	s.logMedia(r, "", http.StatusOK, start, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"results": list,
	})
}

// runImageFileTool 是抠图与超分的共同执行体（两者只差上游工具名）。
func (s *Server) runImageFileTool(w http.ResponseWriter, r *http.Request, tool, typ string) {
	start := time.Now()
	req, perr := s.readImageToolRequest(r)
	if perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeImageError(w, perr)
		return
	}
	cdn, perr := s.cdnURL(r.Context(), req.source())
	if perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeImageError(w, perr)
		return
	}
	raw, perr := s.callImageTool(r, tool, map[string]any{"image_url": cdn})
	if perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeImageError(w, perr)
		return
	}

	urls := media.OutputURLs(raw)
	if len(urls) == 0 {
		msg := media.FailureText(raw)
		if msg == "" {
			msg = "上游称成功但没有产物（响应里没有 output_url）"
		}
		status, code := http.StatusBadGateway, typ+"_failed"
		if isContentRejection(msg) {
			status, code = http.StatusUnavailableForLegalReasons, "content_filter"
		}
		s.logMedia(r, "", status, start, errors.New(msg))
		writeImageError(w, &imageproto.ProtocolError{
			Status: status, Type: errorType(status), Code: code, Message: msg,
		})
		return
	}

	if req.ResponseFormat == "b64_json" {
		body, _, ferr := s.fetchBytes(r.Context(), urls[0])
		if ferr != nil {
			s.logMedia(r, "", http.StatusBadGateway, start, ferr)
			writeImageError(w, &imageproto.ProtocolError{
				Status: http.StatusBadGateway, Type: "upstream_error",
				Code: "download_failed", Message: ferr.Error(),
			})
			return
		}
		s.logMedia(r, "", http.StatusOK, start, nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"created": time.Now().Unix(),
			"data":    []map[string]any{{"b64_json": base64.StdEncoding.EncodeToString(body)}},
		})
		return
	}

	s.logMedia(r, "", http.StatusOK, start, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    []map[string]any{{"url": urls[0]}},
	})
}

// callImageTool 调上游并把错误翻成图像协议错误。
func (s *Server) callImageTool(r *http.Request, tool string, payload any) ([]byte, *imageproto.ProtocolError) {
	raw, err := s.callMedia(r.Context(), tool, payload)
	if err != nil {
		return nil, imageErrorFromMedia(err)
	}
	return raw, nil
}

// readImageToolRequest 解析并校验入参。
func (s *Server) readImageToolRequest(r *http.Request) (*imageToolRequest, *imageproto.ProtocolError) {
	var req imageToolRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, maxImageBody))
	if err != nil {
		return nil, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_json", Message: "读取请求体失败: " + err.Error(),
		}
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_json", Message: "请求体不是合法 JSON: " + err.Error(),
		}
	}
	if req.source() == "" {
		return nil, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Param: "image", Code: "invalid_request",
			Message: "需要 image（URL / data URI / 裸 base64）或 image_url",
		}
	}
	rf := strings.TrimSpace(req.ResponseFormat)
	if rf != "" && rf != "url" && rf != "b64_json" {
		return nil, &imageproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Param: "response_format", Code: "invalid_request",
			Message: "response_format 只接受 url / b64_json，收到 " + rf,
		}
	}
	req.ResponseFormat = rf
	return &req, nil
}

func (q *imageToolRequest) source() string {
	return firstNonEmptyStr(strings.TrimSpace(q.ImageURL), strings.TrimSpace(q.Image))
}

// cdnURL 把图片来源统一成上游要的 cdn_url。
//
// 上游只认「本环境上传后的 cdn_url」，任意公网 URL 会被拒（实测原文见
// fetchmedia.go 的注释）。所以这里三种来源统一归一：
//
//   - 已是 cdn.hailuoai.com 的地址 → 直接用；
//   - 其它 http(s) → 下载（带 SSRF 防护）→ 上传换成 cdn_url；
//   - data URI / 裸 base64 → 上传换成 cdn_url。
//
// 为什么不把裸 base64 直接发给抠图工具：工具入参只有 image_url，没有 data 字段。
func (s *Server) cdnURL(ctx context.Context, src string) (string, *imageproto.ProtocolError) {
	if isUpstreamCDN(src) {
		return src, nil
	}
	var data []byte
	var mime string
	switch {
	case strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://"):
		body, m, err := s.fetchExternal(ctx, src, maxCDNRaw*8)
		if err != nil {
			return "", &imageproto.ProtocolError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Param: "image", Code: "fetch_failed", Message: "取图失败: " + err.Error(),
			}
		}
		if len(body) > maxCDNRaw {
			return "", imageTooLarge()
		}
		data, mime = body, m
	default:
		var ok bool
		mime, data, ok = decodeImageSource(src)
		if !ok {
			return "", &imageproto.ProtocolError{
				Status: http.StatusBadRequest, Type: "invalid_request_error",
				Param: "image", Code: "invalid_image",
				Message: "image 既不是 http(s) URL，也不是合法的 data URI / base64",
			}
		}
	}
	return s.uploadBytes(ctx, data, mime)
}

// uploadBytes 把字节传到上游 CDN，换回 cdn_url。
func (s *Server) uploadBytes(ctx context.Context, data []byte, mime string) (string, *imageproto.ProtocolError) {
	enc := base64.StdEncoding.EncodeToString(data)
	if len(enc) > maxUploadBase64 {
		return "", imageTooLarge()
	}
	raw, err := s.callMedia(ctx, media.ToolUploadToCDN, map[string]any{
		"data":      enc,
		"filename":  "upload" + extForMime(mime),
		"mime_type": mime,
	})
	if err != nil {
		return "", imageErrorFromMedia(err)
	}
	urls := media.OutputURLs(raw)
	if len(urls) == 0 {
		return "", &imageproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "upload_failed", Message: "上传到 CDN 失败：响应里没有 cdn_url",
		}
	}
	return urls[0], nil
}

func imageTooLarge() *imageproto.ProtocolError {
	return &imageproto.ProtocolError{
		Status: http.StatusBadRequest, Type: "invalid_request_error",
		Param: "image", Code: "image_too_large",
		Message: "图片超过 600KiB base64（约 450KiB 原始字节，上游 upload_to_cdn 的上限）；" +
			"请改传上游 cdn_url，或把图压小",
	}
}

// decodeImageSource 解析 data URI 或裸 base64，并按 magic bytes 校正 MIME。
func decodeImageSource(src string) (string, []byte, bool) {
	mime := ""
	payload := src
	if strings.HasPrefix(src, "data:") {
		i := strings.Index(src, ",")
		if i < 0 {
			return "", nil, false
		}
		head := src[len("data:"):i]
		if j := strings.Index(head, ";"); j >= 0 {
			mime = head[:j]
			if !strings.HasSuffix(head, ";base64") {
				return "", nil, false
			}
		} else {
			return "", nil, false
		}
		payload = src[i+1:]
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(payload))
	if err != nil || len(data) == 0 {
		return "", nil, false
	}
	// 上游对 MIME 敏感，实测 JPEG 被标成 PNG 的情况很常见 —— 一律以字节为准。
	sniffed := sniffMime(data, mime)
	if !strings.HasPrefix(sniffed, "image/") {
		return "", nil, false
	}
	return sniffed, data, true
}

func extForMime(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	default:
		return ".jpg"
	}
}
