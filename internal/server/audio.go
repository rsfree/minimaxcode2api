package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/media"
)

// speechRequest OpenAI /v1/audio/speech 的字段，额外接受 emotion。
type speechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	Speed          float64  `json:"speed"`
	Volume         *float64 `json:"volume"`
	Pitch          *int     `json:"pitch"`
	Emotion        string   `json:"emotion"`
	ResponseFormat string   `json:"response_format"`
}

type speechResponse struct {
	OutputURL     string `json:"output_url"`
	CleanOSSKey   string `json:"clean_oss_key"`
	VisibleOSSKey string `json:"visible_oss_key"`
	Error         string `json:"error"`
	Message       string `json:"message"`
}

// handleSpeech 文本转语音，直接回音频字节（OpenAI 语义）。
//
// 上游给的是 CDN URL，网关必须自己下载再回吐：OpenAI 客户端期望响应体就是
// 音频，而不是一个 JSON 里的链接。
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req speechRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	text := strings.TrimSpace(req.Input)
	if text == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "input 不能为空")
		return
	}

	payload := map[string]any{"text": text}
	if v := strings.TrimSpace(req.Voice); v != "" {
		payload["voice_id"] = v
	}
	if req.Speed > 0 {
		payload["speed"] = req.Speed
	}
	if req.Volume != nil {
		payload["volume"] = *req.Volume
	}
	if req.Pitch != nil {
		payload["pitch"] = *req.Pitch
	}
	if v := strings.TrimSpace(req.Emotion); v != "" {
		payload["emotion"] = v
	}

	raw, err := s.callMedia(r.Context(), media.ToolSpeech, payload)
	if err != nil {
		s.logMedia(r, req.Model, http.StatusInternalServerError, start, err)
		writeMediaError(w, err)
		return
	}
	var resp speechResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	if resp.OutputURL == "" {
		msg := firstNonEmptyStr(resp.Error, resp.Message, "上游没有返回 output_url")
		writeOpenAIError(w, http.StatusBadGateway, "no_audio", msg)
		return
	}

	body, mime, ferr := s.fetchBytes(r.Context(), resp.OutputURL)
	if ferr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "download_failed", ferr.Error())
		return
	}
	if !strings.HasPrefix(mime, "audio/") {
		mime = "audio/mpeg"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("Content-Disposition", `inline; filename="speech.mp3"`)
	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	_, _ = w.Write(body)
}

// handleTranscriptions 语音转文字。
//
// 同时支持两种入参：
//   - multipart/form-data（OpenAI 原生，字段名 file）
//   - JSON {"audio_url": "..."} 或 {"data": "<base64>", "mime_type": "audio/wav"}
//
// 上游只接受 URL 或内联 base64，不接受原始二进制流，所以 multipart 的
// 文件会被转成 base64 内联上传。
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	info := map[string]any{}
	ct := r.Header.Get("Content-Type")
	prompt := ""

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(64 << 20); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_form", err.Error())
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "missing_file", "multipart 里缺少 file 字段")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 32<<20))
		if err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "read_failed", err.Error())
			return
		}
		mime := hdr.Header.Get("Content-Type")
		info["data"] = base64.StdEncoding.EncodeToString(data)
		info["mime_type"] = sniffMime(data, mime)
		prompt = r.FormValue("prompt")
	default:
		var body struct {
			AudioURL string `json:"audio_url"`
			URL      string `json:"url"`
			Data     string `json:"data"`
			MimeType string `json:"mime_type"`
			Prompt   string `json:"prompt"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		switch {
		case strings.TrimSpace(body.Data) != "":
			info["data"] = strings.TrimSpace(body.Data)
			info["mime_type"] = firstNonEmptyStr(strings.TrimSpace(body.MimeType), "audio/wav")
		case strings.TrimSpace(body.AudioURL) != "" || strings.TrimSpace(body.URL) != "":
			info["url"] = firstNonEmptyStr(strings.TrimSpace(body.AudioURL), strings.TrimSpace(body.URL))
		default:
			writeOpenAIError(w, http.StatusBadRequest, "missing_audio", "需要 audio_url 或 data")
			return
		}
		prompt = body.Prompt
	}
	if prompt != "" {
		info["prompt"] = prompt
	}

	raw, err := s.callMedia(r.Context(), media.ToolTranscribe, map[string]any{"audio_info": info})
	if err != nil {
		s.logMedia(r, "whisper-1", http.StatusInternalServerError, start, err)
		writeMediaError(w, err)
		return
	}
	var resp struct {
		Transcription string `json:"transcription"`
		Text          string `json:"text"`
		Error         string `json:"error"`
		Message       string `json:"message"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	text := firstNonEmptyStr(resp.Transcription, resp.Text)
	if text == "" && resp.Error != "" {
		writeOpenAIError(w, http.StatusBadGateway, "transcribe_failed", resp.Error)
		return
	}
	s.logMedia(r, "whisper-1", http.StatusOK, start, nil)
	writeJSON(w, http.StatusOK, map[string]any{"text": text})
}

// musicRequest 音乐生成。上游 prompt 要求 10-300 字，lyrics 10-600 字。
type musicRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	Lyrics         string `json:"lyrics"`
	Format         string `json:"format"`
	SampleRate     int    `json:"sample_rate"`
	Bitrate        int    `json:"bitrate"`
	ResponseFormat string `json:"response_format"`
}

// handleMusic 生成音乐。
//
// 默认直接回音频字节（与 /v1/audio/speech 一致），
// response_format=url 时回 JSON，方便客户端自己拉。
func (s *Server) handleMusic(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req musicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if len([]rune(prompt)) < 10 {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt 至少 10 个字（上游限制 10-300）")
		return
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "mp3"
	}
	item := map[string]any{
		"prompt":      prompt,
		"output_file": "slot_0." + format,
		"format":      format,
	}
	if v := strings.TrimSpace(req.Lyrics); v != "" {
		item["lyrics"] = v
	}
	if req.SampleRate > 0 {
		item["sample_rate"] = req.SampleRate
	}
	if req.Bitrate > 0 {
		item["bitrate"] = req.Bitrate
	}

	raw, err := s.callMedia(r.Context(), media.ToolMusic, map[string]any{"requests": []any{item}})
	if err != nil {
		s.logMedia(r, req.Model, http.StatusInternalServerError, start, err)
		writeMediaError(w, err)
		return
	}
	var resp struct {
		SuccessItems []struct {
			OutputFile string `json:"output_file"`
			OutputURL  string `json:"output_url"`
		} `json:"success_items"`
		FailedItems []struct {
			ErrorMsg string `json:"error_msg"`
		} `json:"failed_items"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	if len(resp.SuccessItems) == 0 || resp.SuccessItems[0].OutputURL == "" {
		msg := "音乐生成失败"
		if len(resp.FailedItems) > 0 && resp.FailedItems[0].ErrorMsg != "" {
			msg = resp.FailedItems[0].ErrorMsg
		}
		writeOpenAIError(w, http.StatusBadGateway, "generation_failed", msg)
		return
	}
	url := resp.SuccessItems[0].OutputURL
	if strings.EqualFold(req.ResponseFormat, "url") {
		s.logMedia(r, req.Model, http.StatusOK, start, nil)
		writeJSON(w, http.StatusOK, map[string]any{
			"created": time.Now().Unix(),
			"data":    []map[string]any{{"url": url}},
		})
		return
	}

	body, mime, ferr := s.fetchBytes(r.Context(), url)
	if ferr != nil {
		writeOpenAIError(w, http.StatusBadGateway, "download_failed", ferr.Error())
		return
	}
	if !strings.HasPrefix(mime, "audio/") {
		mime = "audio/mpeg"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	_, _ = w.Write(body)
}
