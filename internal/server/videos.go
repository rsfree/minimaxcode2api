package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/media"
	"github.com/linguo2625469/minimaxcode2api/internal/videoproto"
	"github.com/linguo2625469/minimaxcode2api/internal/videotask"
)

// 视频接口按「异步视频生成协议」（video-adapter）实现：
//
//	POST   /contents/generations/tasks       建任务，只回 {id}
//	GET    /contents/generations/tasks/{id}  查任务，回 canonical 任务对象
//	GET    /contents/generations/tasks       列表（真实数据，不是空壳）
//	DELETE /contents/generations/tasks/{id}  终态任务删记录；非终态 501
//
// 同一份 taskObject() 序列化器同时服务查询与列表 —— 两个出口各写一套是
// 「回包字段对不上」的根源，这里从结构上避免。

const maxVideoBody = 64 << 20

// createVideoTask 建任务。POST 200 只代表受理，成功与否要看后续 status。
func (s *Server) createVideoTask(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// 预算从请求入口开始计时：提交 + 等待共享同一个预算，否则实际耗时
	// 会变成「提交耗时 + wait_timeout」，客户端算不准超时。
	wait, waitFor := waitBudget(r)
	var deadline time.Time
	if wait {
		deadline = start.Add(waitFor)
	}
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
	var payload map[string]any
	var upstreamModel string
	var warnings []string
	if perr == nil {
		payload, upstreamModel, warnings, perr = req.Build()
	}
	if perr != nil {
		s.logMedia(r, "", perr.Status, start, errors.New(perr.Message))
		writeProtoError(w, perr)
		return
	}

	respRaw, err := s.callMedia(r.Context(), media.ToolVideoSubmit, payload)
	if err != nil {
		s.logMedia(r, req.Model, http.StatusInternalServerError, start, err)
		writeProtoError(w, protoFromMedia(err))
		return
	}
	var sub struct {
		TaskID  string `json:"task_id"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(respRaw, &sub); err != nil || sub.TaskID == "" {
		msg := "上游没有返回 task_id"
		if sub.Message != "" {
			msg += ": " + sub.Message
		}
		writeProtoError(w, &videoproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "no_task_id", Message: msg,
		})
		return
	}

	now := time.Now().Unix()
	rec := &videotask.Record{
		ID:            sub.TaskID,
		ClientModel:   req.Model,
		UpstreamModel: upstreamModel,
		Status:        videoproto.StatusQueued,
		CreatedAt:     now,
		UpdatedAt:     now,
		Params: videotask.Params{
			Resolution:            req.Resolution,
			Ratio:                 req.Ratio,
			Duration:              req.Duration,
			GenerateAudio:         req.GenerateAudio,
			Seed:                  req.Seed,
			ServiceTier:           req.ServiceTier,
			SafetyIdentifier:      req.SafetyIdentifier,
			ExecutionExpiresAfter: req.ExecutionExpiresAfter,
			Priority:              req.Priority,
			OutputFormat:          req.OutputFormat,
			Watermark:             req.Watermark,
		},
		Warnings: warnings,
	}
	s.VideoTasks.Put(rec)
	for _, warn := range warnings {
		log.Printf("video task %s 警告: %s", rec.ID, warn)
	}

	s.logMedia(r, req.Model, http.StatusOK, start, nil)

	// 同步折叠：?wait=1 时在预算内等终态；超时降级 202（不取消任务）。
	// 不等待时严格按契约只回 id，warnings 挂在任务对象上（GET 可见）。
	if wait {
		rec = s.waitForVideoTask(r.Context(), rec, deadline)
		writeWaitResult(w, r, rec)
		return
	}
	w.Header().Set("Location", prefixOf(r)+"/contents/generations/tasks/"+rec.ID)
	writeJSON(w, http.StatusOK, map[string]any{"id": rec.ID})
}

// 轮询退避参数。抽成变量是为了测试能把等待压到毫秒级。
var (
	videoPollStart = 2 * time.Second
	videoPollMax   = 10 * time.Second
)

// prefixOf 判断客户端用的是哪套前缀，Location 回同样的。
func prefixOf(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/api/v3/") {
		return "/api/v3"
	}
	return "/v1"
}

// waitBudget 解析同步折叠参数。
//
//	?wait=1&wait_timeout=60   最多在服务端等 60 秒
//
// 上限 300 秒：再长就不是"折叠"而是挂连接了，客户端应该自己轮询。
func waitBudget(r *http.Request) (bool, time.Duration) {
	q := r.URL.Query()
	v := strings.ToLower(strings.TrimSpace(q.Get("wait")))
	if v == "" || v == "0" || v == "false" {
		return false, 0
	}
	sec := 60
	if raw := strings.TrimSpace(q.Get("wait_timeout")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			sec = n
		}
	}
	if sec < 1 {
		sec = 1
	}
	if sec > 300 {
		sec = 300
	}
	return true, time.Duration(sec) * time.Second
}

// refreshVideoTask 查一次上游并把结果合并进记录。
//
// 这是唯一一处「上游状态 → canonical 状态」的地方：查询端点、同步门都走它，
// 避免两条路径对同一份响应给出不同解释。
func (s *Server) refreshVideoTask(ctx context.Context, rec *videotask.Record) (*videotask.Record, *videoproto.ProtocolError) {
	raw, err := s.callMedia(ctx, media.ToolVideoQuery, map[string]any{
		"task_id":          rec.ID,
		"model":            rec.UpstreamModel,
		"output_file_path": "videos/" + rec.ID + ".mp4",
	})

	var q struct {
		Code     int     `json:"code"`
		Message  string  `json:"message"`
		Status   string  `json:"status"`
		VideoURL string  `json:"video_url"`
		Progress float64 `json:"progress"`
		Usage    *struct {
			CompletionTokens int64 `json:"completion_tokens"`
			TotalTokens      int64 `json:"total_tokens"`
		} `json:"usage"`
		Data struct {
			Status   string `json:"status"`
			VideoURL string `json:"video_url"`
		} `json:"data"`
	}
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &q); uerr != nil && err == nil {
			return rec, &videoproto.ProtocolError{
				Status: http.StatusBadGateway, Type: "upstream_error",
				Code: "bad_upstream", Message: "上游响应无法解析: " + uerr.Error(),
			}
		}
	}

	// 观测期失败（网络/5xx）不该被当成任务失败，直接透出可重试的错误。
	var me *media.Error
	if err != nil && !(errors.As(err, &me) && me.IsPoll()) {
		return rec, protoFromMedia(err)
	}

	status, unnormalized := videoproto.MapStatus(firstNonEmptyStr(q.Status, q.Data.Status))
	url := firstNonEmptyStr(q.VideoURL, q.Data.VideoURL)

	// 上游「已就绪但还没准备好下发」：status=succeeded 但没 url，仍是进行中。
	if status == videoproto.StatusSucceeded && url == "" {
		status = videoproto.StatusRunning
	}
	if status == videoproto.StatusFailed {
		rec.ErrorCode = runtimeErrorCode(q.Message)
		rec.ErrorMessage = firstNonEmptyStr(q.Message, "上游任务失败（未给出原因）")
	}
	if status == videoproto.StatusSucceeded {
		rec.VideoURL = url
		rec.ErrorCode, rec.ErrorMessage = "", ""
	}
	rec.Status = status
	rec.Unnormalized = unnormalized
	rec.UpdatedAt = time.Now().Unix()
	if q.Usage != nil {
		rec.HasUsage = true
		rec.UsageTotal = q.Usage.TotalTokens
	}
	s.VideoTasks.Put(rec)
	return rec, nil
}

// waitForVideoTask 同步折叠：在预算内一直查到终态。
//
// 预算耗尽是**降级**（202 + 当前状态 + Location），不是失败、更不取消任务。
// 中途的传输错误也吞掉继续等 —— 一次网络抖动不该把 60 秒的等待判死。
func (s *Server) waitForVideoTask(ctx context.Context, rec *videotask.Record, deadline time.Time) *videotask.Record {
	if videoproto.IsTerminal(rec.Status) {
		return rec
	}
	interval := videoPollStart
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			return rec
		}
		wait := interval
		if wait > remain {
			wait = remain
		}
		select {
		case <-ctx.Done():
			return rec
		case <-time.After(wait):
		}

		// 单次查询也要被预算约束：上游一次查询可能要 1-2 秒，
		// 不设超时的话最后一次查询会把总耗时顶出 wait_timeout。
		left := time.Until(deadline)
		if left <= 0 {
			return rec
		}
		qctx, cancel := context.WithTimeout(ctx, left)
		next, perr := s.refreshVideoTask(qctx, rec)
		cancel()
		if perr != nil {
			// 观测期错误可重试：记一笔继续等，预算用完自然降级。
			log.Printf("video task %s 轮询出错（继续等待）: %s", rec.ID, perr.Message)
		} else {
			rec = next
			if videoproto.IsTerminal(rec.Status) {
				return rec
			}
		}
		interval = time.Duration(float64(interval) * 1.5)
		if interval > videoPollMax {
			interval = videoPollMax
		}
	}
}

// writeWaitResult 终态回任务对象；未终态回 202 + {id,status} + Location。
func writeWaitResult(w http.ResponseWriter, r *http.Request, rec *videotask.Record) {
	if videoproto.IsTerminal(rec.Status) {
		writeJSON(w, http.StatusOK, taskObject(rec))
		return
	}
	w.Header().Set("Location", prefixOf(r)+"/contents/generations/tasks/"+rec.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{"id": rec.ID, "status": rec.Status})
}

// getVideoTask 查询任务，返回 canonical 任务对象。
// 带 ?wait=1 时先做同步折叠，超时降级 202。
func (s *Server) getVideoTask(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	wait, waitFor := waitBudget(r)
	var deadline time.Time
	if wait {
		deadline = start.Add(waitFor)
	}
	id := strings.TrimSpace(r.PathValue("id"))
	rec, ok := s.VideoTasks.Get(id)
	if !ok {
		writeProtoError(w, &videoproto.ProtocolError{
			Status: http.StatusNotFound, Type: "invalid_request_error",
			Param: "id", Code: "task_not_found",
			Message: "没有这个任务：" + id + "（网关只认识自己创建过的任务）",
		})
		return
	}

	rec, perr := s.refreshVideoTask(r.Context(), rec)
	if perr != nil {
		writeProtoError(w, perr)
		return
	}

	if wait && !videoproto.IsTerminal(rec.Status) {
		rec = s.waitForVideoTask(r.Context(), rec, deadline)
		writeWaitResult(w, r, rec)
		return
	}
	writeJSON(w, http.StatusOK, taskObject(rec))
}

// listVideoTasks 列表端点（真实数据）。协议允许不实现，但既然本地有记录就老实返回。
func (s *Server) listVideoTasks(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	recs, total := s.VideoTasks.List(limit)
	items := make([]map[string]any, 0, len(recs))
	for _, rec := range recs {
		items = append(items, taskObject(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

// deleteVideoTask 只有终态任务能删本地记录；非终态返回 501。
//
// 上游没有取消接口（running 明确不支持 DELETE），所以这里不假装能取消 ——
// 把「删记录」和「取消任务」两件事分开，避免调用方以为任务停了还在计费。
func (s *Server) deleteVideoTask(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	rec, ok := s.VideoTasks.Get(id)
	if !ok {
		writeProtoError(w, &videoproto.ProtocolError{
			Status: http.StatusNotFound, Type: "invalid_request_error",
			Param: "id", Code: "task_not_found", Message: "没有这个任务：" + id,
		})
		return
	}
	if !videoproto.IsTerminal(rec.Status) {
		writeProtoError(w, &videoproto.ProtocolError{
			Status: http.StatusNotImplemented, Type: "api_error",
			Code: "unsupported_operation",
			Message: "上游不支持取消运行中的任务（当前状态 " + rec.Status +
				"）；只能等它到终态后再删记录",
		})
		return
	}
	s.VideoTasks.Delete(id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

// taskObject 是唯一的 canonical 序列化器：查询、列表都走它。
func taskObject(rec *videotask.Record) map[string]any {
	content := map[string]any{
		"video_url":      nil,
		"last_frame_url": nil,
	}
	if rec.VideoURL != "" {
		content["video_url"] = rec.VideoURL
	}
	if rec.LastFrameURL != "" {
		content["last_frame_url"] = rec.LastFrameURL
	}

	var errObj any
	if rec.ErrorCode != "" || rec.ErrorMessage != "" {
		errObj = map[string]any{
			"code":    firstNonEmptyStr(rec.ErrorCode, "upstream_error"),
			"message": rec.ErrorMessage,
		}
	}

	out := map[string]any{
		"id":         rec.ID,
		"model":      rec.ClientModel,
		"status":     rec.Status,
		"content":    content,
		"error":      errObj,
		"created_at": rec.CreatedAt,
		"updated_at": rec.UpdatedAt,
	}
	p := rec.Params
	if p.Resolution != "" {
		out["resolution"] = p.Resolution
	}
	if p.Ratio != "" {
		out["ratio"] = p.Ratio
	}
	if p.Duration != nil {
		out["duration"] = *p.Duration
	}
	if p.GenerateAudio != nil {
		out["generate_audio"] = *p.GenerateAudio
	}
	if p.Watermark != nil {
		out["watermark"] = *p.Watermark
	}
	if p.Seed != nil {
		out["seed"] = *p.Seed
	}
	if p.ServiceTier != "" {
		out["service_tier"] = p.ServiceTier
	}
	if p.SafetyIdentifier != "" {
		out["safety_identifier"] = p.SafetyIdentifier
	}
	if p.ExecutionExpiresAfter != nil {
		out["execution_expires_after"] = *p.ExecutionExpiresAfter
	}
	if p.Priority != nil {
		out["priority"] = *p.Priority
	}
	if p.OutputFormat != "" {
		out["output_format"] = p.OutputFormat
	}
	if rec.HasUsage {
		out["usage"] = map[string]any{
			"completion_tokens": rec.UsageTotal,
			"total_tokens":      rec.UsageTotal,
		}
	}
	if len(rec.Warnings) > 0 {
		out["warnings"] = rec.Warnings
	}
	if rec.Unnormalized {
		out["status_unnormalized"] = true
	}
	return out
}

// writeProtoError 输出协议的四键错误信封（message / type / param / code）。
func writeProtoError(w http.ResponseWriter, perr *videoproto.ProtocolError) {
	status := perr.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	typ := perr.Type
	if typ == "" {
		// 视频协议的错误 type 只有 invalid_request_error / api_error 两类，
		// 5xx 不能报成 invalid_request_error（会让客户端去改请求）。
		if status >= 500 {
			typ = "api_error"
		} else {
			typ = "invalid_request_error"
		}
	}
	writeFourKeyError(w, status, typ, perr.Param, perr.Code, perr.Message)
}

// protoFromMedia 按「谁的错」把媒体网关错误翻成协议错误。
//
// 归因纪律：客户端参数错 → 400；渠道凭据 → 502；额度 → 402 且保留
// insufficient_quota（运维要能一眼看出是余额而不是请求写错）。
func protoFromMedia(err error) *videoproto.ProtocolError {
	var me *media.Error
	if !errors.As(err, &me) {
		return &videoproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "upstream_unreachable", Message: err.Error(),
		}
	}
	switch {
	case me.IsBilling():
		return &videoproto.ProtocolError{
			Status: http.StatusPaymentRequired, Type: "api_error",
			Code: "insufficient_quota", Message: billingHint(me.Message),
		}
	case me.IsRequest():
		return &videoproto.ProtocolError{
			Status: http.StatusBadRequest, Type: "invalid_request_error",
			Code: "invalid_request", Message: me.Message,
		}
	case me.IsAuth():
		return &videoproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "api_error",
			Code: "upstream_auth", Message: "上游账号不可用: " + me.Message,
		}
	case me.IsPoll():
		return &videoproto.ProtocolError{
			Status: http.StatusBadGateway, Type: "upstream_error",
			Code: "not_ready", Message: me.Message,
		}
	}
	return &videoproto.ProtocolError{
		Status: http.StatusBadGateway, Type: "upstream_error",
		Code: "upstream_error", Message: me.Message,
	}
}

// runtimeErrorCode 把运行期错误归因到协议类别。
// 审核类必须落在 content_filter —— 重试无用，客户端要据此提示用户改内容。
func runtimeErrorCode(msg string) string {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "sensitive"), strings.Contains(m, "risk"),
		strings.Contains(m, "content security"), strings.Contains(m, "content_filter"),
		strings.Contains(m, "审核"):
		return "content_filter"
	case strings.Contains(m, "expired"), strings.Contains(m, "timeout"), strings.Contains(m, "timed out"):
		return "expired"
	}
	return "upstream_error"
}
