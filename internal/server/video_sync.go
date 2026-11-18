package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/media"
	"github.com/linguo2625469/minimaxcode2api/internal/videoproto"
)

// 同步视频：一次调用直达产物。
//
//	POST /v1/contents/generations
//
// 上游 gen_videos 工具自己在服务端轮询（≤20 分钟）并直接回 output_url，
// 适合 Dify / n8n 这类不写轮询逻辑的客户端。请求体与异步任务**同一套 canonical**，
// 只是参数面更窄（见 videoproto.BuildSync）。
//
// 响应刻意对齐 /v1/images/generations（created + data[].url），而不是任务对象：
// 它没有 task_id，伪装成"可轮询的任务"只会让客户端去 GET 一个不存在的 id。
func (s *Server) syncVideoGeneration(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxVideoBody))
	if err != nil {
		writeProtoError(w, &videoproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_json", Message: "读取请求体失败: " + err.Error(),
		})
		return
	}

	req, perr := videoproto.Decode(raw)
	if perr == nil {
		perr = req.Validate()
	}
	var item map[string]any
	var warnings []string
	if perr == nil {
		item, warnings, perr = req.BuildSync()
	}
	// 校验通过后再归一图源：反过来的话，"尾帧不支持"这类能力错误会因为先去下载
	// 图片而变成 fetch_failed，param 也会指错。
	if perr == nil {
		perr = s.normalizeSyncImage(r.Context(), item)
	}
	if perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeProtoError(w, perr)
		return
	}

	respRaw, cerr := s.callMedia(r.Context(), media.ToolGenVideos,
		map[string]any{"requests": []any{item}})
	if cerr != nil {
		s.logMedia(r, req.Model, http.StatusBadGateway, start, cerr)
		writeProtoError(w, protoFromMedia(cerr))
		return
	}

	urls := media.OutputURLs(respRaw)
	if len(urls) == 0 {
		msg := media.FailureText(respRaw)
		if msg == "" {
			msg = "上游称成功但没有产物（响应里没有 output_url）"
		}
		s.logMedia(r, req.Model, http.StatusBadGateway, start, errors.New(msg))
		writeProtoError(w, &videoproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "empty_result", Message: msg,
		})
		return
	}

	out := map[string]any{
		"created": time.Now().Unix(),
		"model":   req.Model,
		"data":    []map[string]any{{"url": urls[0]}},
	}
	// warnings 是附加字段；协议允许透传附加键，只在非空时出现，
	// 否则空数组会让人以为"出过问题但没记下来"。
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	writeJSON(w, http.StatusOK, out)
}

// normalizeSyncImage 把载荷里的首帧换成 cdn_url。
//
// 同步工具的 input_image.url 只认 cdn_url（上游 schema 原文："CDN/OSS URL from a
// completed upload"）；公网 URL 必须先下载上传。异步链路的 input_image_path 语义
// 未实测，不在这里一并改。
func (s *Server) normalizeSyncImage(ctx context.Context, item map[string]any) *videoproto.ProtocolError {
	img, ok := item["input_image"].(map[string]any)
	if !ok {
		return nil
	}
	u, _ := img["url"].(string)
	if u == "" || isUpstreamCDN(u) {
		return nil
	}
	cdn, perr := s.cdnURL(ctx, u)
	if perr != nil {
		return &videoproto.ProtocolError{
			Status: perr.Status, Type: perr.Type,
			Param: perr.Param, Code: perr.Code, Message: perr.Message,
		}
	}
	img["url"] = cdn
	return nil
}
