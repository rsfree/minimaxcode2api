package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/media"
)

// 视频模型。H3 支持原生音频与多模态参考，但按 M Plan 周额度/账户额度计费；
// Hailuo-2.3 走另一条额度。两者参数不通用，所以模型必须显式传或走默认。
const (
	videoModelH3     = "MiniMax-H3"
	videoModelHailuo = "MiniMax-Hailuo-2.3"
)

// videoRequest 提交视频任务。字段对齐上游 submit_video_generation，并加了
// image_url / last_frame_image_url 这种更符合直觉的名字。
type videoRequest struct {
	Model             string `json:"model"`
	Prompt            string `json:"prompt"`
	Duration          int    `json:"duration"`
	Resolution        string `json:"resolution"`
	Ratio             string `json:"ratio"`
	ImageURL          string `json:"image_url"`
	LastFrameImageURL string `json:"last_frame_image_url"`
	ReferenceType     string `json:"reference_type"`
	AIGCWatermark     *bool  `json:"aigc_watermark"`
	// CallbackURL 上游不支持回调，保留字段是为了让客户端迁移时不会静默丢参数。
	CallbackURL string `json:"callback_url"`
}

type videoSubmitResponse struct {
	TaskID string `json:"task_id"`
	Code   int    `json:"code"`
	Msg    string `json:"message"`
}

type videoQueryResponse struct {
	TaskID   string  `json:"task_id"`
	Status   string  `json:"status"`
	Progress float64 `json:"progress"`
	VideoURL string  `json:"video_url"`
	Message  string  `json:"message"`
	// 有些响应会把业务体嵌在 data 里，两个位置都读一次。
	Data struct {
		TaskID   string  `json:"task_id"`
		Status   string  `json:"status"`
		Progress float64 `json:"progress"`
		VideoURL string  `json:"video_url"`
	} `json:"data"`
}

// handleVideoCreate 提交一个异步视频任务。
//
// 语义与上游一致：提交是终态操作 —— 失败就是失败，不自动重试（上游按次计费，
// 重试可能扣两次钱，而且任务可能已经在队列里）。换号只发生在鉴权失败。
func (s *Server) handleVideoCreate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req videoRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt 不能为空")
		return
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = videoModelHailuo
	}
	if model != videoModelH3 && model != videoModelHailuo {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"model 只支持 "+videoModelH3+" 或 "+videoModelHailuo)
		return
	}

	payload := map[string]any{"model": model, "prompt": prompt}
	if req.Duration > 0 {
		payload["duration"] = req.Duration
	}
	if v := strings.TrimSpace(req.Resolution); v != "" {
		payload["resolution"] = strings.ToUpper(v)
	}
	if v := strings.TrimSpace(req.Ratio); v != "" {
		payload["ratio"] = v
	}
	first := strings.TrimSpace(firstNonEmptyStr(req.ImageURL))
	if first != "" {
		payload["input_image_path"] = first
	}
	if v := strings.TrimSpace(req.LastFrameImageURL); v != "" {
		payload["last_frame_image_path"] = v
	}
	if v := strings.TrimSpace(req.ReferenceType); v != "" {
		payload["reference_type"] = v
	}
	if req.AIGCWatermark != nil {
		payload["aigc_watermark"] = *req.AIGCWatermark
	}

	raw, err := s.callMedia(r.Context(), media.ToolVideoSubmit, payload)
	if err != nil {
		s.logMedia(r, model, http.StatusInternalServerError, start, err)
		writeMediaError(w, err)
		return
	}
	var resp videoSubmitResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	if resp.TaskID == "" {
		writeOpenAIError(w, http.StatusBadGateway, "no_task_id", "上游没有返回 task_id: "+truncateStr(resp.Msg, 200))
		return
	}

	// 记住 task -> model，轮询时客户端不必再传 model。
	s.videoTasks.Store(resp.TaskID, model)
	s.logMedia(r, model, http.StatusOK, start, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      resp.TaskID,
		"task_id": resp.TaskID,
		"object":  "video.generation",
		"model":   model,
		"status":  "queued",
		"created": time.Now().Unix(),
	})
}

// handleVideoGet 轮询任务。上游状态：queued / running / succeeded / failed / cancelled。
func (s *Server) handleVideoGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "缺少 task id")
		return
	}
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		if v, ok := s.videoTasks.Load(id); ok {
			model, _ = v.(string)
		}
	}
	if model == "" {
		model = videoModelHailuo
	}

	raw, err := s.callMedia(r.Context(), media.ToolVideoQuery, map[string]any{
		"task_id":          id,
		"model":            model,
		"output_file_path": "videos/" + id + ".mp4",
	})
	var q videoQueryResponse
	if len(raw) > 0 {
		if uerr := json.Unmarshal(raw, &q); uerr != nil && err == nil {
			writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", uerr.Error())
			return
		}
	}
	status := firstNonEmptyStr(q.Status, q.Data.Status)
	// 上游可能 code=503 但 status=succeeded（"视频已就绪，还没准备好下发，
	// 稍后查同一个 task_id，别重新提交"）。这种情况不是失败，必须继续轮询；
	// 若原样回 succeeded，客户端会立刻去取 url 而拿不到，直接判失败。
	pending := false
	var me *media.Error
	if errors.As(err, &me) && me.IsPoll() {
		pending = true
	} else if err != nil && status == "" {
		writeMediaError(w, err)
		return
	}
	url := firstNonEmptyStr(q.VideoURL, q.Data.VideoURL)
	progress := q.Progress
	if progress == 0 {
		progress = q.Data.Progress
	}
	reportStatus := status
	if pending && url == "" {
		// 还没拿到可下载地址：对客户端而言仍是"进行中"，
		// 否则它会立刻去取 url 落空，把已完成的任务判成失败。
		reportStatus = "running"
	}

	out := map[string]any{
		"id":              id,
		"task_id":         id,
		"object":          "video.generation",
		"model":           model,
		"status":          reportStatus,
		"upstream_status": status,
		"progress":        progress,
	}
	if url != "" {
		out["url"] = url
		out["video_url"] = url
	}
	if q.Message != "" {
		out["message"] = q.Message
	}
	if reportStatus == "succeeded" || reportStatus == "failed" || reportStatus == "cancelled" {
		s.videoTasks.Delete(id)
	}
	writeJSON(w, http.StatusOK, out)
}

func truncateStr(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
