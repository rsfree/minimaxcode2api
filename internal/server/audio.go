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

// maxAudioUpload 是转写接口的单文件上限，对齐 OpenAI（25MB）。
// 上游只收 URL / 内联 base64，超限的请求在网关侧就拦掉，不白占一次槽位。
const maxAudioUpload = 25 << 20

// maxSpeechBody 是 /v1/audio/speech 的 JSON 上限。OpenAI 侧 input 上限 4096 字符，
// 1MB 足够宽松；不设上限会让超大 body 先进内存再被拒（skill 的陷阱 #9）。
const maxSpeechBody = 1 << 20

// maxSpeechInput 是 OpenAI 的 input 字符上限（上游 TTS 可到 10000，北向仍按 4096）。
const maxSpeechInput = 4096

// speechSSEChunk 是 SSE 音频分片大小。16KiB 原始字节 → 约 21.8KiB base64，
// 对长文本既不会产生上千个事件，也不会让单个事件过大。
const speechSSEChunk = 16 << 10

// voiceRef 兼容 OpenAI 的两种 voice 写法：字符串，或自定义音色对象 {"id":"..."}。
// 2026 版 SDK 的 Voice 类型就是 string | VoiceID，只按 string 解析会直接 400。
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
		if err := json.Unmarshal(b, &obj); err != nil {
			return err
		}
		v.ID, v.Raw = obj.ID, obj.ID
		return nil
	default:
		return errors.New(`voice 必须是字符串或 {"id":"..."}`)
	}
}

// speechRequest OpenAI /v1/audio/speech 的字段。
//
// 除 OpenAI 原生字段外还额外接受 volume / pitch / emotion（上游能力多于 OpenAI），
// 以及 instructions（OpenAI 的 gpt-4o-mini-tts 用，上游无等价物，接受但忽略）。
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

// handleSpeech 文本转语音（OpenAI 语义）。
//
// 上游给的是 CDN URL，网关必须自己下载再回吐：OpenAI 客户端期望响应体就是
// 音频，而不是一个 JSON 里的链接。
//
// 兼容性取舍（详见 internal/audioproto）：
//   - voice 支持字符串与 {"id":"..."}；OpenAI 音色名按性别/语调映射到上游预置音色，
//     其余取值原样透传（上游有 327 个音色，不做白名单）。
//   - response_format 只认 OpenAI 六种取值；上游只能出 mp3，mp3 之外的请求按 mp3
//     返回，并用 X-MiniMax-Format-Requested 标注。
//   - stream_format=sse 按 OpenAI 事件格式回 speech.audio.delta / speech.audio.done
//     （音频已整体生成，这里做的是分片下发，不是上游边生成边推）。
//   - speed 收敛到上游 [0.5, 2.0]，收敛时回 X-MiniMax-Speed。
func (s *Server) handleSpeech(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req speechRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxSpeechBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "", "request_too_large",
				fmt.Sprintf("请求体超过 %d KB（OpenAI 上限）", maxSpeechBody>>10))
			return
		}
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	text := strings.TrimSpace(req.Input)
	if text == "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "input", "invalid_request", "input 不能为空")
		return
	}
	if n := len([]rune(text)); n > maxSpeechInput {
		writeFourKeyError(w, http.StatusBadRequest, "", "input", "invalid_request",
			fmt.Sprintf("input 最多 %d 字符（OpenAI 上限），收到 %d", maxSpeechInput, n))
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
	// 接收 ≠ 支持：上游没有 instructions 的等价参数，不能静默吞掉，
	// 在响应头里点名（skill 的「接收≠支持」纪律；不 400 是为了兼容客户端）。
	var notForwarded []string
	if strings.TrimSpace(req.Instructions) != "" {
		notForwarded = append(notForwarded, "instructions")
	}
	streamFormat := strings.ToLower(strings.TrimSpace(req.StreamFormat))
	useSSE := streamFormat == "sse"
	if streamFormat != "" && !useSSE && streamFormat != "audio" {
		writeFourKeyError(w, http.StatusBadRequest, "", "stream_format", "unsupported_parameter",
			"stream_format 只支持 sse / audio，收到 "+streamFormat)
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
	voiceID, mappedVoice := audioproto.ResolveVoice(req.Voice.ID)
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
	h.Set("X-MiniMax-Format", actual)
	if len(notForwarded) > 0 {
		h.Set("X-MiniMax-Not-Forwarded", strings.Join(notForwarded, ","))
	}
	if !native {
		// 显式降级：客户端要 wav/pcm/…，上游只出 mp3。请求本身合法，不报错，
		// 但必须让调用方在响应头里看到这件事，别以为拿到了想要的格式。
		h.Set("X-MiniMax-Format-Requested", format)
	}
	if voiceID != "" {
		h.Set("X-MiniMax-Voice", voiceID)
	}
	if mappedVoice {
		h.Set("X-MiniMax-Voice-Alias", strings.TrimSpace(req.Voice.Raw))
	}
	if clamped {
		h.Set("X-MiniMax-Speed", strconv.FormatFloat(speed, 'f', -1, 64))
	}

	if useSSE {
		// OpenAI 的 SSE 形态：event: speech.audio.delta + base64 分片，
		// 以 speech.audio.done 结束。注意这条分支没有 Content-Length。
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no") // nginx 不缓冲，否则分片会被攒成一坨
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
			"type": "speech.audio.done",
			// 上游不返回 token 用量，不编数字；OpenAI 的 usage 结构保留但置零。
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}

	h.Set("Content-Type", mime)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Content-Disposition", `inline; filename="speech.`+safeExtension(actual)+`"`)
	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	_, _ = w.Write(body)
}

// transcriptionRequest 是转写接口的公共元数据（multipart 与 JSON 两种入参共用）。
//
// 2026 版 OpenAI 的参数面比上游宽得多。这里的原则是「字段全收、能力按上游」：
// 上游没有对应参数的字段照样解析，不因解析失败 400；真正影响输出的字段
// （response_format / stream）在 handler 里单独处理，其余忽略并在文档里写清。
type transcriptionRequest struct {
	Model          string   `json:"model"`
	Language       string   `json:"language"`
	Languages      []string `json:"languages"`
	Prompt         string   `json:"prompt"`
	ResponseFormat string   `json:"response_format"`
	Stream         bool     `json:"stream"`

	Temperature            *float64        `json:"temperature"`
	TimestampGranularities []string        `json:"timestamp_granularities"`
	Include                []string        `json:"include"`
	ChunkingStrategy       json.RawMessage `json:"chunking_strategy"`
	Keywords               []string        `json:"keywords"`
	KnownSpeakerNames      []string        `json:"known_speaker_names"`
	KnownSpeakerReferences []string        `json:"known_speaker_references"`
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
// response_format：json / text / verbose_json / diarized_json（降级）/ srt / vtt
// （单条字幕，时间轴 = 0 → 音频真实时长）。stream=true 回 OpenAI 的
// transcript.text.delta / transcript.text.done 事件流。
func (s *Server) handleTranscriptions(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	info := map[string]any{}
	var meta transcriptionRequest
	var audioBytes []byte // 仅用于 srt/vtt 算时长，不参与上游调用
	ct := r.Header.Get("Content-Type")

	switch {
	case strings.HasPrefix(ct, "multipart/form-data"):
		// 整体请求体也要限长：不加 MaxBytesReader 时，multipart 解析会把
		// 超大请求全量落到临时文件上再被我们拒掉。
		r.Body = http.MaxBytesReader(w, r.Body, maxAudioUpload+(1<<20))
		if err := r.ParseMultipartForm(maxAudioUpload); err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "file", "request_too_large",
					fmt.Sprintf("音频超过 %d MB（OpenAI 上限）", maxAudioUpload>>20))
				return
			}
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "invalid_form", err.Error())
			return
		}
		// r.FormFile 只取第一个 file；多传的会被静默丢掉（skill 的陷阱 #6），
		// 对转写这种「传错文件就全错」的接口必须显式拒绝。
		if r.MultipartForm != nil && len(r.MultipartForm.File["file"]) > 1 {
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "duplicate_file",
				fmt.Sprintf("一次只能上传一个 file，收到 %d 个", len(r.MultipartForm.File["file"])))
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
			writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "file", "request_too_large",
				fmt.Sprintf("音频超过 %d MB（OpenAI 上限）", maxAudioUpload>>20))
			return
		}
		audioBytes = data
		info["data"] = base64.StdEncoding.EncodeToString(data)
		info["mime_type"] = sniffMime(data, hdr.Header.Get("Content-Type"))
		meta.Model = r.FormValue("model")
		meta.Language = r.FormValue("language")
		meta.Prompt = r.FormValue("prompt")
		meta.ResponseFormat = r.FormValue("response_format")
		meta.Stream = parseFlexBool(r.FormValue("stream"))
		if v := strings.TrimSpace(r.FormValue("temperature")); v != "" {
			if f, err := strconv.ParseFloat(v, 64); err == nil {
				meta.Temperature = &f
			}
		}
		if v := strings.TrimSpace(r.FormValue("chunking_strategy")); v != "" {
			// multipart 里只有字符串形态；原样记下来，仅用于「未下发」标注。
			meta.ChunkingStrategy = json.RawMessage(strconv.Quote(v))
		}
		meta.TimestampGranularities = formList(r, "timestamp_granularities[]", "timestamp_granularities")
		meta.Include = formList(r, "include[]", "include")
		meta.Languages = formList(r, "languages[]", "languages")
		meta.Keywords = formList(r, "keywords[]", "keywords")
		meta.KnownSpeakerNames = formList(r, "known_speaker_names[]", "known_speaker_names")
		meta.KnownSpeakerReferences = formList(r, "known_speaker_references[]", "known_speaker_references")
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
				writeFourKeyError(w, http.StatusRequestEntityTooLarge, "", "data", "request_too_large",
					fmt.Sprintf("音频超过 %d MB（OpenAI 上限）", maxAudioUpload>>20))
				return
			}
			writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		switch {
		case strings.TrimSpace(body.Data) != "":
			rawData := strings.TrimSpace(body.Data)
			info["data"] = rawData
			info["mime_type"] = firstNonEmptyStr(strings.TrimSpace(body.MimeType), "audio/wav")
			audioBytes, _ = decodeAudioData(rawData)
		case strings.TrimSpace(body.AudioURL) != "" || strings.TrimSpace(body.URL) != "":
			info["url"] = firstNonEmptyStr(strings.TrimSpace(body.AudioURL), strings.TrimSpace(body.URL))
		default:
			writeFourKeyError(w, http.StatusBadRequest, "", "file", "missing_audio",
				"需要 file（multipart）或 audio_url / data（JSON）")
			return
		}
		meta = body.transcriptionRequest
	}

	// 必填字段先校验：OpenAI 的 model 是必填（旧版这里默认 whisper-1，
	// 会把「客户端漏传」伪装成成功，skill 的契约要求显式拒绝）。
	if strings.TrimSpace(meta.Model) == "" {
		writeFourKeyError(w, http.StatusBadRequest, "", "model", "invalid_request", "model 不能为空")
		return
	}
	format, ok := audioproto.NormalizeTranscriptionFormat(meta.ResponseFormat)
	if !ok {
		writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "invalid_request",
			"response_format 只支持 json / text / srt / verbose_json / vtt / diarized_json，收到 "+format)
		return
	}
	// 字幕需要真实时长：上游不给，网关从上传字节里算（mp3 / wav / mp4）。
	// 算不出来就明确拒绝，不编时间轴；这个检查放在调上游之前，避免白花一次额度。
	// stream=true 时只产出文本增量事件，不需要时长，也就不做这个校验。
	var subtitleDur time.Duration
	if !meta.Stream && (format == audioproto.TranscribeSRT || format == audioproto.TranscribeVTT) {
		if len(audioBytes) == 0 {
			writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "unsupported_response_format",
				"response_format="+format+" 需要音频字节来推算时长；JSON 的 url 入参请改用 multipart 或 data（base64）")
			return
		}
		d, ok := audioproto.MediaDuration(audioBytes)
		if !ok {
			writeFourKeyError(w, http.StatusBadRequest, "", "response_format", "unsupported_response_format",
				"response_format="+format+" 需要音频时长，当前容器无法解析（支持 mp3 / wav / m4a / mp4）；请改用 json / text / verbose_json")
			return
		}
		subtitleDur = d
	}
	if p := strings.TrimSpace(meta.Prompt); p != "" {
		info["prompt"] = p
	}
	// 接收 ≠ 支持：这些字段北向收下，但上游 listen_audio 没有对应参数。
	// 在响应头里逐项点名，避免客户端以为「传了就生效」。
	var notForwarded []string
	if strings.TrimSpace(meta.Language) != "" {
		notForwarded = append(notForwarded, "language")
	}
	if len(meta.Languages) > 0 {
		notForwarded = append(notForwarded, "languages")
	}
	if meta.Temperature != nil {
		notForwarded = append(notForwarded, "temperature")
	}
	if len(meta.TimestampGranularities) > 0 {
		notForwarded = append(notForwarded, "timestamp_granularities")
	}
	if len(meta.Include) > 0 {
		notForwarded = append(notForwarded, "include")
	}
	if len(bytes.TrimSpace(meta.ChunkingStrategy)) > 0 && string(bytes.TrimSpace(meta.ChunkingStrategy)) != "null" {
		notForwarded = append(notForwarded, "chunking_strategy")
	}
	if len(meta.Keywords) > 0 {
		notForwarded = append(notForwarded, "keywords")
	}
	if len(meta.KnownSpeakerNames) > 0 {
		notForwarded = append(notForwarded, "known_speaker_names")
	}
	if len(meta.KnownSpeakerReferences) > 0 {
		notForwarded = append(notForwarded, "known_speaker_references")
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
	if len(notForwarded) > 0 {
		w.Header().Set("X-MiniMax-Not-Forwarded", strings.Join(notForwarded, ","))
	}

	// stream 优先于 response_format：OpenAI 的流式转写只产出文本增量事件。
	if meta.Stream {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if format != audioproto.TranscribeJSON && format != audioproto.TranscribeText {
			w.Header().Set("X-MiniMax-Response-Format", audioproto.TranscribeJSON)
		}
		flusher, _ := w.(http.Flusher)
		// 上游一次性给全文，这里发一个 delta 即全文，再发 done。事件结构按
		// OpenAI 的 TranscriptionStreamEvent，SDK 的 Stream 能直接解析。
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
		// OpenAI 的 text 形态就是纯文本，不是 JSON。SDK 会按这个 Content-Type
		// 决定返回 str 还是对象，包成 JSON 会让调用方拿到一坨字符串化的 JSON。
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, text)
	case audioproto.TranscribeVerboseJSON:
		warnings := []string{
			"上游 ASR 不返回时间戳与分段：segments 为空，duration 省略；" +
				"language 回显的是请求里的提示值，不是检测结果",
		}
		if len(meta.TimestampGranularities) > 0 {
			warnings = append(warnings, "timestamp_granularities 已接收，但上游不提供词级/段级时间戳")
		}
		if len(meta.Include) > 0 {
			warnings = append(warnings, "include=logprobs 已接收，但上游不返回对数概率")
		}
		out := map[string]any{
			"task":     "transcribe",
			"text":     text,
			"segments": []any{},
			"warnings": warnings,
		}
		if lang := strings.ToLower(strings.TrimSpace(meta.Language)); lang != "" {
			out["language"] = lang
		}
		writeJSON(w, http.StatusOK, out)
	case audioproto.TranscribeSRT, audioproto.TranscribeVTT:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, audioproto.Subtitles(format, text, subtitleDur))
	case audioproto.TranscribeDiarizedJSON:
		// 上游没有说话人分离：字段对齐（segments 存在），但不伪造 speaker。
		w.Header().Set("X-MiniMax-Response-Format", audioproto.TranscribeJSON)
		writeJSON(w, http.StatusOK, map[string]any{
			"task": "transcribe", "text": text, "segments": []any{},
			"warnings": []string{"上游不支持说话人分离：segments 为空，没有 speaker 字段"},
		})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"text": text})
	}
}

// safeExtension 只允许 [A-Za-z0-9]{1,10}，否则回 bin。
// Content-Disposition 的 filename 不能拿上游 mime 的后缀直接拼（skill 的陷阱 #7）。
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

// writeSSE 写一个 OpenAI 形态的事件。data 里的 type 由调用方给，
// 确保与 event: 行一致（SDK 同时看 event 与 JSON 的 type 字段）。
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

// parseFlexBool 接受 multipart 里常见的 true/1/on（SDK 发 "true"）。
func parseFlexBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// formList 读 multipart 的数组字段。OpenAI SDK 用 `field[]` 形式重复提交。
func formList(r *http.Request, keys ...string) []string {
	var out []string
	for _, k := range keys {
		out = append(out, r.Form[k]...)
	}
	return out
}

// decodeAudioData 解码 base64；顺带兼容 data URL（data:audio/wav;base64,...）。
func decodeAudioData(s string) ([]byte, bool) {
	if strings.HasPrefix(s, "data:") {
		if i := strings.Index(s, ","); i > 0 {
			s = s[i+1:]
		}
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, false
	}
	return b, true
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
