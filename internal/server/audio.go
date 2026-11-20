package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/audioproto"
	"github.com/linguo2625469/minimaxcode2api/internal/media"
)

// maxAudioUpload 是转写接口的单文件上限，对齐 OpenAI（25MB）。
// 上游只收 URL / 内联 base64，超限的请求在网关侧就拦掉，不白占一次槽位。
const maxAudioUpload = 25 << 20

// speechRequest OpenAI /v1/audio/speech 的字段。
//
// 除 OpenAI 原生字段外还额外接受 volume / pitch / emotion（上游能力多于 OpenAI），
// 以及 instructions（OpenAI 的 gpt-4o-mini-tts 用，上游无等价物，接受但忽略）。
type speechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          string   `json:"voice"`
	Speed          float64  `json:"speed"`
	Volume         *float64 `json:"volume"`
	Pitch          *int     `json:"pitch"`
	Emotion        string   `json:"emotion"`
	ResponseFormat string   `json:"response_format"`
	Instructions   string   `json:"instructions"`
	StreamFormat   string   `json:"stream_format"`
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
//
// 兼容性取舍（详见 internal/audioproto）：
//   - voice 支持 OpenAI 的 11 个音色名，按性别/语调映射到上游预置音色；
//     非别名的取值一律原样透传（上游有 327 个音色，不做白名单）。
//   - response_format 只认 OpenAI 六种取值；上游只能出 mp3，所以 mp3 之外的
//     请求按 mp3 返回，并用 X-MiniMax-Format-Requested 标注"你要的没做到"。
//   - speed 收敛到上游 [0.5, 2.0]，收敛时回 X-MiniMax-Speed。
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req speechRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	text := strings.TrimSpace(req.Input)
	if text == "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "input", "invalid_request", "input 不能为空")
		return
	}
	if sf := strings.TrimSpace(req.StreamFormat); sf != "" && !strings.EqualFold(sf, "audio") {
		writeFourKeyError(w, http.StatusBadRequest, "", "stream_format", "unsupported_parameter",
			"stream_format="+sf+" 不支持：网关回完整音频字节，不回 SSE 事件流")
		return
	}
	format, native, ok := audioproto.NormalizeSpeechFormat(req.ResponseFormat)
	if !ok {
		writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "invalid_request",
			"response_format 只支持 OpenAI 的 mp3 / opus / aac / flac / wav / pcm，收到 "+format)
		return
	}
	speed, clamped, ok := audioproto.ClampSpeed(req.Speed)
	if !ok {
		writeFourKeyError(w, http.StatusBadRequest, "", "speed", "invalid_request",
			"speed 必须在 0.25–4.0 之间")
		return
	}

	payload := map[string]any{"text": text}
	voiceID, mappedVoice := audioproto.ResolveVoice(req.Voice)
	if voiceID != "" {
		payload["voice_id"] = voiceID
	}
	if speed > 0 {
		payload["speed"] = speed
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
		s.logMedia(r, req.Model, http.StatusBadGateway, start, err)
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	if resp.OutputURL == "" {
		msg := firstNonEmptyStr(resp.Error, resp.Message, "上游没有返回 output_url")
		s.logMedia(r, req.Model, http.StatusBadGateway, start, errors.New(msg))
		writeOpenAIError(w, http.StatusBadGateway, "no_audio", msg)
		return
	}

	body, mime, ferr := s.fetchBytes(r.Context(), resp.OutputURL)
	if ferr != nil {
		s.logMedia(r, req.Model, http.StatusBadGateway, start, ferr)
		writeOpenAIError(w, http.StatusBadGateway, "download_failed", ferr.Error())
		return
	}
	if !strings.HasPrefix(mime, "audio/") {
		mime = "audio/mpeg"
	}
	// 以实际字节为准，不假设一定是 mp3：上游哪天换了编码，头里的信息仍然诚实。
	actual := audioproto.FormatMP3
	switch {
	case strings.Contains(mime, "wav"):
		actual = "wav"
	case strings.Contains(mime, "mpeg"), strings.Contains(mime, "mp3"):
		actual = "mp3"
	default:
		actual = strings.TrimPrefix(mime, "audio/")
	}

	h := w.Header()
	h.Set("Content-Type", mime)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Content-Disposition", `inline; filename="speech.`+actual+`"`)
	h.Set("X-MiniMax-Format", actual)
	if !native {
		// 显式降级：客户端要 wav/pcm/…，上游只出 mp3。请求本身合法，不报错，
		// 但必须让调用方在响应头里看到这件事，别以为拿到了想要的格式。
		h.Set("X-MiniMax-Format-Requested", format)
	}
	if voiceID != "" {
		h.Set("X-MiniMax-Voice", voiceID)
	}
	if mappedVoice {
		h.Set("X-MiniMax-Voice-Alias", strings.TrimSpace(req.Voice))
	}
	if clamped {
		h.Set("X-MiniMax-Speed", strconv.FormatFloat(speed, 'f', -1, 64))
	}
	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	_, _ = w.Write(body)
}

// transcriptionRequest 是转写接口的公共元数据（multipart 与 JSON 两种入参共用）。
type transcriptionRequest struct {
	Model          string `json:"model"`
	Language       string `json:"language"`
	Prompt         string `json:"prompt"`
	ResponseFormat string `json:"response_format"`
}

// handleTranscriptions 语音转文字。
//
// 入参支持两种形态：
//   - multipart/form-data（OpenAI 原生，文件字段 file + model/language/prompt/response_format）
//   - JSON {"audio_url": "..."} 或 {"data": "<base64>", "mime_type": "audio/wav"}
//
// 上游只接受 URL 或内联 base64，不接受原始二进制流，所以 multipart 的文件
// 会被转成 base64 内联上传。
//
// response_format：json / text / verbose_json 实现；srt / vtt 明确 400 ——
// 上游 listen_audio 不返回时间戳，硬编会造成"字幕时间轴全错"这种更难查的问题。
// language 上游没有该参数，只做接收与回显，不谎称做了语言限定。
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	info := map[string]any{}
	var meta transcriptionRequest
	ct := r.Header.Get("Content-Type")

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		// 整体请求体也要限长：不加 MaxBytesReader 时，multipart 解析会把
		// 超大请求全量落到临时文件上再被我们拒掉。
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioUpload+(1<<20))
		if err := r.ParseMultipartForm(maxAudioUpload); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "file", "file_too_large",
					fmt.Sprintf("音频超过 %d MB（OpenAI 上限）", maxAudioUpload>>20))
				return
			}
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_form", err.Error())
			return
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "missing_file",
				"multipart 里缺少 file 字段")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxAudioUpload+1))
		if err != nil {
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "read_failed", err.Error())
			return
		}
		if len(data) == 0 {
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "empty_file", "音频文件为空")
			return
		}
		if len(data) > maxAudioUpload {
			writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "file", "file_too_large",
				fmt.Sprintf("音频超过 %d MB（OpenAI 上限）", maxAudioUpload>>20))
			return
		}
		info["data"] = base64.StdEncoding.EncodeToString(data)
		info["mime_type"] = sniffMime(data, hdr.Header.Get("Content-Type"))
		meta.Model = r.FormValue("model")
		meta.Language = r.FormValue("language")
		meta.Prompt = r.FormValue("prompt")
		meta.ResponseFormat = r.FormValue("response_format")
	default:
		var body struct {
			AudioURL string `json:"audio_url"`
			URL      string `json:"url"`
			Data     string `json:"data"`
			MimeType string `json:"mime_type"`
			transcriptionRequest
		}
		// base64 比原文件大 4/3，留一点余量。
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioUpload*4/3+(1<<20))
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "data", "file_too_large",
					fmt.Sprintf("音频超过 %d MB（OpenAI 上限）", maxAudioUpload>>20))
				return
			}
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
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "missing_audio",
				"需要 file（multipart）或 audio_url / data（JSON）")
			return
		}
		meta = body.transcriptionRequest
	}

	format, ok := audioproto.NormalizeTranscriptionFormat(meta.ResponseFormat)
	if !ok {
		writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "invalid_request",
			"response_format 只支持 json / text / verbose_json / srt / vtt，收到 "+format)
		return
	}
	if audioproto.NeedsTimestamps(format) {
		writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "unsupported_response_format",
			"response_format="+format+" 需要逐句时间戳，上游 ASR 不提供；请改用 json / text / verbose_json")
		return
	}
	if p := strings.TrimSpace(meta.Prompt); p != "" {
		info["prompt"] = p
	}
	model := firstNonEmptyStr(strings.TrimSpace(meta.Model), "whisper-1")

	raw, err := s.callMedia(r.Context(), media.ToolTranscribe, map[string]any{"audio_info": info})
	if err != nil {
		s.logMedia(r, model, http.StatusInternalServerError, start, err)
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
		s.logMedia(r, model, http.StatusBadGateway, start, err)
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	text := firstNonEmptyStr(resp.Transcription, resp.Text)
	if text == "" && resp.Error != "" {
		s.logMedia(r, model, http.StatusBadGateway, start, errors.New(resp.Error))
		writeOpenAIError(w, http.StatusBadGateway, "transcribe_failed", resp.Error)
		return
	}
	s.logMedia(r, model, http.StatusOK, start, nil)

	switch format {
	case audioproto.TranscribeText:
		// OpenAI 的 text 形态就是纯文本，不是 JSON。SDK 会按这个 Content-Type
		// 决定返回 str 还是对象，包成 JSON 会让调用方拿到一坨字符串化的 JSON。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
	case audioproto.TranscribeVerboseJSON:
		out := map[string]any{
			"task":     "transcribe",
			"text":     text,
			"segments": []any{},
			"warnings": []string{
				"上游 ASR 不返回时间戳与分段：segments 为空，duration 省略；" +
					"language 回显的是请求里的提示值，不是检测结果",
			},
		}
		if lang := strings.ToLower(strings.TrimSpace(meta.Language)); lang != "" {
			out["language"] = lang
		}
		writeJSON(w, http.StatusOK, out)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"text": text})
	}
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
