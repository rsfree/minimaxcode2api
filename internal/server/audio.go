package server

import (
	"bytes"
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

// 本文件的参数面以 internal/audioproto 的声明为唯一事实源；
// 面向调用方的能力清单见 GET /v1/audio/capabilities 与 docs/audio-capabilities.md。
//
// audio-adapter 规范的三条不变量：
//  1. 北向只承诺契约，不承诺厂商能力；
//  2. 厂商差异在适配层收敛；
//  3. 宁可显式拒绝，不可静默降级 —— 所以这里没有"降级响应头"，
//     不支持的格式/语速/指令一律 400。

const (
	// maxAudioUpload 是转写单文件上限（对齐 OpenAI 参考值 25MB）。
	maxAudioUpload = 25 << 20
	// maxSpeechBody 是 /v1/audio/speech 的 JSON 上限。
	maxSpeechBody = 1 << 20
	// speechSSEChunk 是 SSE 音频分片大小（16KiB 原始字节 → 约 21.8KiB base64）。
	speechSSEChunk = 16 << 10
)

// voiceRef 兼容 OpenAI 的两种 voice 写法：字符串，或自定义音色对象 {"id":"..."}。
type voiceRef struct {
	ID  string
	Raw string
}

func (v *voiceRef) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		v.ID, v.Raw = s, s
		return nil
	case '{':
		var obj struct {
			ID string `json:"id"`
		}
		// 对象内部同样拒绝未声明字段，避免 {"id":"x","foo":1} 被静默接受。
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&obj); err != nil {
			return err
		}
		v.ID, v.Raw = obj.ID, obj.ID
		return nil
	default:
		return errors.New(`voice 必须是字符串或 {"id":"..."}`)
	}
}

// speechRequest 是本部署声明的 speech 参数面。
//
// 骨架字段来自 OpenAI；volume / pitch / emotion 是南向扩展（上游能力多于 OpenAI）。
// 注意：DisallowUnknownFields 会让任何未声明的 JSON 字段直接 400，
// 这是规范要求的「未声明即拒绝」，不是疏漏。
type speechRequest struct {
	Model          string   `json:"model"`
	Input          string   `json:"input"`
	Voice          voiceRef `json:"voice"`
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

// handleSpeech 文本转语音（OpenAI 骨架 + 本部署声明的取值面）。
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	if ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type"))); ct != "" &&
		!strings.HasPrefix(ct, "application/json") {
		writeFourKeyError(w, http.StatusUnsupportedMediaType, "", "", "",
			"Content-Type 必须是 application/json，收到 "+ct)
		return
	}
	var req speechRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxSpeechBody)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		switch {
		case errors.As(err, &mbe):
			writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "", "request_too_large",
				fmt.Sprintf("请求体超过 %d KB", maxSpeechBody>>10))
		case unknownJSONField(err) != "":
			field := unknownJSONField(err)
			writeFourKeyError(w, http.StatusBadRequest, "", field, "invalid_request",
				"未声明的字段 "+field+"（见 GET /v1/audio/capabilities）")
		default:
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		}
		return
	}

	text := strings.TrimSpace(req.Input)
	if text == "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "input", "invalid_request", "input 不能为空")
		return
	}
	if n := len([]rune(text)); n > audioproto.MaxSpeechInput {
		writeFourKeyError(w, http.StatusBadRequest, "", "input", "invalid_request",
			fmt.Sprintf("input 最多 %d 字符（本部署上限），收到 %d", audioproto.MaxSpeechInput, n))
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "model", "invalid_request", "model 不能为空")
		return
	}
	if strings.TrimSpace(req.Voice.ID) == "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "voice", "invalid_request",
			`voice 不能为空：字符串或 {"id":"..."} 都接受`)
		return
	}
	streamFormat := strings.ToLower(strings.TrimSpace(req.StreamFormat))
	if streamFormat != "" && streamFormat != "audio" && streamFormat != "sse" {
		writeFourKeyError(w, http.StatusBadRequest, "", "stream_format", "invalid_request",
			"stream_format 只支持 audio / sse，收到 "+streamFormat)
		return
	}
	format, ok := audioproto.SupportedSpeechFormat(req.ResponseFormat)
	if !ok {
		writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "invalid_request",
			"本部署只支持 response_format=mp3（上游 synthesize_speech 没有 format 参数），收到 "+format)
		return
	}
	speed, ok := audioproto.ValidateSpeed(req.Speed)
	if !ok {
		writeFourKeyError(w, http.StatusBadRequest, "", "speed", "invalid_request",
			fmt.Sprintf("speed 只支持 %.1f–%.1f（上游区间），收到 %v",
				audioproto.SpeedMin, audioproto.SpeedMax, req.Speed))
		return
	}
	if strings.TrimSpace(req.Instructions) != "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "instructions", "invalid_request",
			"本部署不支持 instructions：上游 synthesize_speech 没有等价参数")
		return
	}
	if req.Volume != nil && !audioproto.ValidateVolume(*req.Volume) {
		writeFourKeyError(w, http.StatusBadRequest, "", "volume", "invalid_request",
			fmt.Sprintf("volume 只支持 %.0f–%.0f，收到 %v", audioproto.VolumeMin, audioproto.VolumeMax, *req.Volume))
		return
	}
	if req.Pitch != nil && !audioproto.ValidatePitch(*req.Pitch) {
		writeFourKeyError(w, http.StatusBadRequest, "", "pitch", "invalid_request",
			fmt.Sprintf("pitch 只支持 %d–%d，收到 %d", audioproto.PitchMin, audioproto.PitchMax, *req.Pitch))
		return
	}
	if !audioproto.ValidateEmotion(req.Emotion) {
		writeFourKeyError(w, http.StatusBadRequest, "", "emotion", "invalid_request",
			"emotion 只支持 happy / sad / angry / fearful / disgusted / surprised / neutral")
		return
	}

	payload := map[string]any{"text": text}
	voiceID, mappedVoice := audioproto.ResolveVoice(req.Voice.ID)
	payload["voice_id"] = voiceID
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

	h := w.Header()
	h.Set("X-MiniMax-Format", audioproto.FormatMP3)
	h.Set("X-MiniMax-Voice", voiceID)
	if mappedVoice {
		h.Set("X-MiniMax-Voice-Alias", strings.TrimSpace(req.Voice.Raw))
	}

	if streamFormat == "sse" {
		// 网关扩展：上游只给完整 mp3，这里按 OpenAI 的 speech.audio.delta 分片下发。
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		s.logMedia(r, req.Model, http.StatusOK, start, nil)
		flusher, _ := w.(http.Flusher)
		for i := 0; i < len(body); i += speechSSEChunk {
			end := i + speechSSEChunk
			if end > len(body) {
				end = len(body)
			}
			writeSSE(w, flusher, "speech.audio.delta", map[string]any{
				"type":  "speech.audio.delta",
				"audio": base64.StdEncoding.EncodeToString(body[i:end]),
			})
		}
		writeSSE(w, flusher, "speech.audio.done", map[string]any{
			"type":  "speech.audio.done",
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}

	h.Set("Content-Type", mime)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Content-Disposition", `inline; filename="speech.`+safeExtension(audioproto.FormatMP3)+`"`)
	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	_, _ = w.Write(body)
}

// transcriptionRequest 列出北向骨架里与转写相关的字段。
//
// language / prompt / temperature / timestamp_granularities / include /
// chunking_strategy / keywords / known_speaker_* 在这里只是为了给出**精确的 400**；
// 上游 listen_audio 没有对应参数，声明支持就等于静默降级。
type transcriptionRequest struct {
	Model          string `json:"model"`
	ResponseFormat string `json:"response_format"`
	Stream         bool   `json:"stream"`

	Language               string          `json:"language"`
	Languages              []string        `json:"languages"`
	Prompt                 string          `json:"prompt"`
	Temperature            *float64        `json:"temperature"`
	TimestampGranularities []string        `json:"timestamp_granularities"`
	Include                []string        `json:"include"`
	ChunkingStrategy       json.RawMessage `json:"chunking_strategy"`
	Keywords               []string        `json:"keywords"`
	KnownSpeakerNames      []string        `json:"known_speaker_names"`
	KnownSpeakerReferences []string        `json:"known_speaker_references"`
}

// unsupportedTranscriptionReason 已知但本部署不支持的字段 → 拒绝原因。
func unsupportedTranscriptionReason(field string) (string, bool) {
	field = strings.TrimSuffix(field, "[]")
	switch field {
	case "language":
		return "上游 listen_audio 没有语言参数，language 无法生效", true
	case "languages":
		return "上游 listen_audio 没有语言参数，languages 无法生效", true
	case "prompt":
		return "上游把 prompt 当作“分析指令”，与 OpenAI 的词汇提示语义不同，本部署不支持", true
	case "temperature":
		return "上游 listen_audio 没有采样温度参数", true
	case "timestamp_granularities":
		return "上游 listen_audio 不返回时间戳", true
	case "include":
		return "上游 listen_audio 不返回 logprobs", true
	case "chunking_strategy":
		return "上游 listen_audio 没有分块策略", true
	case "keywords":
		return "上游 listen_audio 不支持关键词提示", true
	case "known_speaker_names", "known_speaker_references":
		return "上游 listen_audio 不支持说话人注册/参考", true
	default:
		return "", false
	}
}

// checkUnsupportedMeta 返回第一个「已赋值但不支持」的字段。
func checkUnsupportedMeta(meta transcriptionRequest) (string, string) {
	cases := []struct {
		field string
		set   bool
	}{
		{"language", strings.TrimSpace(meta.Language) != ""},
		{"languages", len(meta.Languages) > 0},
		{"prompt", strings.TrimSpace(meta.Prompt) != ""},
		{"temperature", meta.Temperature != nil},
		{"timestamp_granularities", len(meta.TimestampGranularities) > 0},
		{"include", len(meta.Include) > 0},
		{"chunking_strategy", len(bytes.TrimSpace(meta.ChunkingStrategy)) > 0 &&
			string(bytes.TrimSpace(meta.ChunkingStrategy)) != "null"},
		{"keywords", len(meta.Keywords) > 0},
		{"known_speaker_names", len(meta.KnownSpeakerNames) > 0},
		{"known_speaker_references", len(meta.KnownSpeakerReferences) > 0},
	}
	for _, c := range cases {
		if c.set {
			reason, _ := unsupportedTranscriptionReason(c.field)
			return c.field, reason
		}
	}
	return "", ""
}

// handleTranscriptions 语音转文字（OpenAI 骨架 + 本部署声明的取值面）。
//
// 入参：multipart/form-data（OpenAI 原生）或 JSON {audio_url|url|data, mime_type}
// （网关扩展，便于 relay 场景）。其余 Content-Type 一律 415。
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	info := map[string]any{}
	var meta transcriptionRequest
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioUpload+(1<<20))
		if err := r.ParseMultipartForm(maxAudioUpload); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "file", "request_too_large",
					fmt.Sprintf("音频超过 %d MB", maxAudioUpload>>20))
				return
			}
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_request", err.Error())
			return
		}
		// 文件字段：只允许一个 file。
		for name, files := range r.MultipartForm.File {
			if name != "file" {
				writeFourKeyError(w, http.StatusBadRequest, "", name, "invalid_request",
					"未知的文件字段 "+name+"（只接受 file）")
				return
			}
			if len(files) > 1 {
				writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_request",
					fmt.Sprintf("一次只能上传一个 file，收到 %d 个", len(files)))
				return
			}
		}
		// 普通字段：先按声明校验，再读值。
		for name, vals := range r.MultipartForm.Value {
			nonEmpty := false
			for _, v := range vals {
				if strings.TrimSpace(v) != "" {
					nonEmpty = true
					break
				}
			}
			if !nonEmpty {
				continue
			}
			switch strings.TrimSuffix(name, "[]") {
			case "model", "response_format", "stream":
				// 声明支持
			default:
				if reason, ok := unsupportedTranscriptionReason(name); ok {
					writeFourKeyError(w, http.StatusBadRequest, "", strings.TrimSuffix(name, "[]"),
						"invalid_request", reason)
					return
				}
				writeFourKeyError(w, http.StatusBadRequest, "", name, "invalid_request",
					"未声明的字段 "+name+"（见 GET /v1/audio/capabilities）")
				return
			}
		}
		file, hdr, err := r.FormFile("file")
		if err != nil {
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_request",
				"multipart 里缺少 file 字段")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxAudioUpload+1))
		if err != nil {
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_request", err.Error())
			return
		}
		if len(data) == 0 {
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_request", "音频文件为空")
			return
		}
		if len(data) > maxAudioUpload {
			writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "file", "request_too_large",
				fmt.Sprintf("音频超过 %d MB", maxAudioUpload>>20))
			return
		}
		info["data"] = base64.StdEncoding.EncodeToString(data)
		info["mime_type"] = sniffMime(data, hdr.Header.Get("Content-Type"))
		meta.Model = r.FormValue("model")
		meta.ResponseFormat = r.FormValue("response_format")
		meta.Stream = parseFlexBool(r.FormValue("stream"))

	case ct == "" || strings.HasPrefix(ct, "application/json"):
		var body struct {
			AudioURL string `json:"audio_url"`
			URL      string `json:"url"`
			Data     string `json:"data"`
			MimeType string `json:"mime_type"`
			transcriptionRequest
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioUpload*4/3+(1<<20))
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			var mbe *http.MaxBytesError
			switch {
			case errors.As(err, &mbe):
				writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "data", "request_too_large",
					fmt.Sprintf("请求体超过 %d MB", (maxAudioUpload*4/3)>>20))
			case unknownJSONField(err) != "":
				field := unknownJSONField(err)
				writeFourKeyError(w, http.StatusBadRequest, "", field, "invalid_request",
					"未声明的字段 "+field+"（见 GET /v1/audio/capabilities）")
			default:
				writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
			}
			return
		}
		switch {
		case strings.TrimSpace(body.Data) != "":
			info["data"] = strings.TrimSpace(body.Data)
			info["mime_type"] = firstNonEmptyStr(strings.TrimSpace(body.MimeType), "audio/wav")
		case strings.TrimSpace(body.AudioURL) != "" || strings.TrimSpace(body.URL) != "":
			info["url"] = firstNonEmptyStr(strings.TrimSpace(body.AudioURL), strings.TrimSpace(body.URL))
		default:
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_request",
				"需要 file（multipart）或 audio_url / data（JSON）")
			return
		}
		meta = body.transcriptionRequest

	default:
		writeFourKeyError(w, http.StatusUnsupportedMediaType, "", "", "",
			"Content-Type 必须是 multipart/form-data 或 application/json，收到 "+ct)
		return
	}

	if field, reason := checkUnsupportedMeta(meta); field != "" {
		writeFourKeyError(w, http.StatusBadRequest, "", field, "invalid_request", reason)
		return
	}
	if strings.TrimSpace(meta.Model) == "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "model", "invalid_request", "model 不能为空")
		return
	}
	format, known := audioproto.NormalizeTranscriptionFormat(meta.ResponseFormat)
	if !known {
		writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "invalid_request",
			"未知的 response_format: "+format)
		return
	}
	if !audioproto.SupportedTranscriptionFormat(format) {
		reason := ""
		switch format {
		case audioproto.TranscribeSRT, audioproto.TranscribeVTT:
			reason = "需要真实分段，上游 listen_audio 不返回时间戳；请改用 json / text / verbose_json"
		case audioproto.TranscribeDiarizedJSON:
			reason = "需要说话人分离，上游不提供；请改用 json / text / verbose_json"
		}
		writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "invalid_request",
			"本部署不支持 response_format="+format+"："+reason)
		return
	}
	model := strings.TrimSpace(meta.Model)

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

	if meta.Stream {
		// 网关扩展（OpenAI 的流式转写事件）：上游一次给全文，只发一个 delta + done。
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		flusher, _ := w.(http.Flusher)
		writeSSE(w, flusher, "transcript.text.delta", map[string]any{
			"type": "transcript.text.delta", "delta": text,
		})
		writeSSE(w, flusher, "transcript.text.done", map[string]any{
			"type": "transcript.text.done", "text": text,
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}

	switch format {
	case audioproto.TranscribeText:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
	case audioproto.TranscribeVerboseJSON:
		// 上游没有真实的分段/语言/时长，按规范只输出有真实数据的字段。
		writeJSON(w, http.StatusOK, map[string]any{"task": "transcribe", "text": text})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"text": text})
	}
}

// handleAudioCapabilities 返回本部署的语音能力声明（唯一事实源是 audioproto）。
func (s *Server) handleAudioCapabilities(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "audio.capabilities",
		"speech": map[string]any{
			"models": map[string]any{
				"mode": "passthrough", "upstream": "speech-2.8-hd",
				"note": "上游忽略 model；任意非空字符串都走同一模型",
			},
			"voices": map[string]any{
				"openai":             audioproto.OpenAIVoices(),
				"native_passthrough": true,
				"list_endpoint":      "/v1/voices",
			},
			"input":            map[string]any{"max_chars": audioproto.MaxSpeechInput},
			"response_formats": []string{audioproto.FormatMP3},
			"speed": map[string]any{
				"min": audioproto.SpeedMin, "max": audioproto.SpeedMax, "default": 1.0,
			},
			"instructions":  false,
			"stream_format": []string{"audio", "sse"},
			"extensions": map[string]any{
				"volume":  map[string]any{"min": audioproto.VolumeMin, "max": audioproto.VolumeMax},
				"pitch":   map[string]any{"min": audioproto.PitchMin, "max": audioproto.PitchMax},
				"emotion": audioproto.Emotions(),
			},
			"limits": map[string]any{"json_body_bytes": maxSpeechBody},
		},
		"transcription": map[string]any{
			"models": map[string]any{
				"mode": "passthrough", "upstream": "asr-1.0",
				"note": "上游忽略 model；任意非空字符串都走同一模型",
			},
			"response_formats": []string{
				audioproto.TranscribeJSON, audioproto.TranscribeText, audioproto.TranscribeVerboseJSON,
			},
			"unsupported_response_formats": map[string]string{
				audioproto.TranscribeSRT:          "上游不返回时间戳",
				audioproto.TranscribeVTT:          "上游不返回时间戳",
				audioproto.TranscribeDiarizedJSON: "上游不支持说话人分离",
			},
			"stream":     true,
			"timestamps": false,
			"language":   false,
			"prompt":     false,
			"limits":     map[string]any{"file_bytes": maxAudioUpload},
		},
	})
}

// ---------- 小工具 ----------

// unknownJSONField 从 DisallowUnknownFields 的错误里取出字段名。
func unknownJSONField(err error) string {
	const prefix = `json: unknown field "`
	msg := err.Error()
	i := strings.Index(msg, prefix)
	if i < 0 {
		return ""
	}
	rest := msg[i+len(prefix):]
	if j := strings.IndexByte(rest, '"'); j > 0 {
		return rest[:j]
	}
	return ""
}

// safeExtension 只允许 [A-Za-z0-9]{1,10}，否则回 bin。
func safeExtension(s string) string {
	if len(s) == 0 || len(s) > 10 {
		return "bin"
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		return "bin"
	}
	return s
}

// writeSSE 写一个 OpenAI 形态的事件。
func writeSSE(w http.ResponseWriter, flusher http.Flusher, event string, data any) {
	b, err := json.Marshal(data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if flusher != nil {
		flusher.Flush()
	}
}

// parseFlexBool 接受 multipart 里常见的 true/1/on。
func parseFlexBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
