package server

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// fakeAudioServer 是 TTS 链路的最小假上游：synthesize_speech 回一个 output_url，
// 该 URL 指回同一个测试服务器上的 mp3 字节。
func fakeAudioServer(t *testing.T, mu *sync.Mutex, got *map[string]any, fail bool) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp/synthesize_speech":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*got = body
			mu.Unlock()
			if fail {
				io.WriteString(w, `{"code":402,"message":"plan balance has run out"}`)
				return
			}
			fmt.Fprintf(w, `{"code":0,"output_url":"http://%s/speech.mp3"}`, r.Host)
		case "/speech.mp3":
			// 最小可嗅探的 mp3 头（ID3v2）。
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("ID3\x04\x00\x00\x00\x00\x00\x00fake-audio-payload"))
		default:
			http.NotFound(w, r)
		}
	}
}

func TestSpeechMapsOpenAIVoiceAndHeaders(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got, false), "tok1")

	resp, raw := f.post(t, "/v1/audio/speech",
		`{"model":"tts-1","voice":"onyx","input":"你好","response_format":"wav","speed":3}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "audio/") {
		t.Fatalf("Content-Type = %q，期望 audio/*", ct)
	}
	// 上游只能出 mp3：必须把"请求了 wav 但拿到 mp3"写在响应头里。
	if v := resp.Header.Get("X-MiniMax-Format"); v != "mp3" {
		t.Fatalf("X-MiniMax-Format = %q，期望 mp3", v)
	}
	if v := resp.Header.Get("X-MiniMax-Format-Requested"); v != "wav" {
		t.Fatalf("X-MiniMax-Format-Requested = %q，期望 wav", v)
	}
	if v := resp.Header.Get("X-MiniMax-Voice"); v != "male-qn-badao" {
		t.Fatalf("X-MiniMax-Voice = %q，期望 male-qn-badao（onyx 的映射）", v)
	}
	if v := resp.Header.Get("X-MiniMax-Voice-Alias"); v != "onyx" {
		t.Fatalf("X-MiniMax-Voice-Alias = %q，期望 onyx", v)
	}
	if v := resp.Header.Get("X-MiniMax-Speed"); v != "2" {
		t.Fatalf("X-MiniMax-Speed = %q，期望 2（speed 3 收敛到上游上限）", v)
	}

	mu.Lock()
	payload := got
	mu.Unlock()
	if payload["voice_id"] != "male-qn-badao" {
		t.Fatalf("上游 voice_id = %v，期望 male-qn-badao", payload["voice_id"])
	}
	if payload["speed"] != float64(2) {
		t.Fatalf("上游 speed = %v，期望 2", payload["speed"])
	}
	if payload["text"] != "你好" {
		t.Fatalf("上游 text = %v，期望 你好", payload["text"])
	}
}

func TestSpeechPassesThroughNativeVoice(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got, false), "tok1")

	resp, raw := f.post(t, "/v1/audio/speech",
		`{"model":"tts-1","voice":"English_Graceful_Lady","input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if v := resp.Header.Get("X-MiniMax-Voice"); v != "English_Graceful_Lady" {
		t.Fatalf("X-MiniMax-Voice = %q", v)
	}
	if v := resp.Header.Get("X-MiniMax-Voice-Alias"); v != "" {
		t.Fatalf("原生音色不该带 alias 头，得到 %q", v)
	}
	if v := resp.Header.Get("X-MiniMax-Format-Requested"); v != "" {
		t.Fatalf("默认 mp3 不该带降级头，得到 %q", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["voice_id"] != "English_Graceful_Lady" {
		t.Fatalf("上游 voice_id = %v，应原样透传", got["voice_id"])
	}
	if _, ok := got["speed"]; ok {
		t.Fatalf("未指定 speed 时不该传 speed，得到 %v", got["speed"])
	}
}

func TestSpeechUpstreamBilling(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got, true), "tok1")

	resp, raw := f.post(t, "/v1/audio/speech", `{"input":"hi"}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("状态码 = %d，期望 402，body=%s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("insufficient_quota")) {
		t.Fatalf("402 应映射成 insufficient_quota，body=%s", raw)
	}
}

func TestSpeechRejectsInvalidParams(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got, false), "tok1")

	cases := []struct {
		name  string
		body  string
		param string
	}{
		{"空 input", `{"input":"  "}`, "input"},
		{"非法 response_format", `{"input":"hi","response_format":"ogg"}`, "response_format"},
		{"speed 越界", `{"input":"hi","speed":5}`, "speed"},
		{"非法 stream_format", `{"input":"hi","stream_format":"multipart"}`, "stream_format"},
	}
	for _, c := range cases {
		resp, raw := f.post(t, "/v1/audio/speech", c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400，body=%s", c.name, resp.StatusCode, raw)
		}
		var out struct {
			Error struct {
				Param string `json:"param"`
				Code  string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: 响应不是 JSON: %v", c.name, err)
		}
		if out.Error.Param != c.param {
			t.Fatalf("%s: param = %q，期望 %q", c.name, out.Error.Param, c.param)
		}
	}
	if f.upCalls() != 0 {
		t.Fatalf("参数校验失败时不该打上游，实际 %d 次", f.upCalls())
	}
}

// multipartAudio 组一个 OpenAI 形态的转写请求（file + model + response_format）。
func multipartAudio(t *testing.T, path string, audio []byte, fields map[string]string) (*http.Response, []byte) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "sample.wav")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := fw.Write(audio); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, path, &buf)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

func fakeTranscribeServer(t *testing.T, mu *sync.Mutex, got *map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/listen_audio" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		*got = body
		mu.Unlock()
		io.WriteString(w, `{"code":0,"transcription":"你好，这是一次兼容性测试。"}`)
	}
}

// wavHeader 是 44 字节的合法 WAV 头 + 一点静音数据，够 sniffMime 识别。
func wavHeader() []byte {
	b := make([]byte, 64)
	copy(b[0:], "RIFF")
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	return b
}

func TestTranscriptionsTextFormat(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"model":           "whisper-1",
		"response_format": "text",
		"language":        "zh",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q，text 格式必须是 text/plain", ct)
	}
	if string(raw) != "你好，这是一次兼容性测试。" {
		t.Fatalf("body = %q，期望纯文本，不能是 JSON", raw)
	}

	mu.Lock()
	defer mu.Unlock()
	info, _ := got["audio_info"].(map[string]any)
	if info == nil || info["mime_type"] != "audio/wav" {
		t.Fatalf("上游 audio_info 不对: %v", got)
	}
	if info["prompt"] != nil {
		t.Fatalf("未传 prompt 时不该带上游: %v", info["prompt"])
	}
}

func TestTranscriptionsVerboseJSON(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"response_format": "verbose_json",
		"language":        "ZH",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["task"] != "transcribe" || out["text"] != "你好，这是一次兼容性测试。" {
		t.Fatalf("verbose_json 关键字段不对: %v", out)
	}
	if lang, _ := out["language"].(string); lang != "zh" {
		t.Fatalf("language = %v，期望回显 zh", out["language"])
	}
	if segs, ok := out["segments"].([]any); !ok || len(segs) != 0 {
		t.Fatalf("segments 必须是空数组（上游无时间戳），得到 %v", out["segments"])
	}
	if _, ok := out["warnings"]; !ok {
		t.Fatal("verbose_json 应带 warnings 说明时间戳缺失")
	}
}

// 容器识别不出来时 srt 仍然要拒绝：宁可不给字幕，也不编时间轴。
func TestTranscriptionsSRTRejectsUnparsableContainer(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"response_format": "srt",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Error.Code != "unsupported_response_format" {
		t.Fatalf("code = %q，期望 unsupported_response_format", out.Error.Code)
	}
	if f.upCalls() != 0 {
		t.Fatalf("算不出时长应在网关侧拒绝，不该打上游；实际 %d 次", f.upCalls())
	}
}

// validWAV 生成 2 秒 16kHz/16bit/mono 的合法 WAV（带 fmt 与 data chunk）。
func validWAV() []byte {
	const sampleRate, channels, bits = 16000, 1, 16
	dataSize := sampleRate * channels * bits / 8 * 2
	b := make([]byte, 44+dataSize)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+dataSize))
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], channels)
	binary.LittleEndian.PutUint32(b[24:], sampleRate)
	binary.LittleEndian.PutUint32(b[28:], sampleRate*channels*bits/8)
	binary.LittleEndian.PutUint16(b[32:], channels*bits/8)
	binary.LittleEndian.PutUint16(b[34:], bits)
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(dataSize))
	return b
}

func TestTranscriptionsSRT(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", validWAV(), map[string]string{
		"response_format": "srt",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	want := "1\n00:00:00,000 --> 00:00:02,000\n你好，这是一次兼容性测试。\n"
	if string(raw) != want {
		t.Fatalf("SRT 内容 = %q，期望 %q", raw, want)
	}
}

func TestTranscriptionsDiarizedJSONDegrades(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"response_format": "diarized_json",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if _, ok := out["segments"].([]any); !ok {
		t.Fatalf("diarized_json 必须保留 segments 字段（空数组），得到 %v", out)
	}
	if resp.Header.Get("X-MiniMax-Response-Format") != "json" {
		t.Fatalf("降级应回 X-MiniMax-Response-Format: json，得到 %q",
			resp.Header.Get("X-MiniMax-Response-Format"))
	}
}

// parseSSE 把响应体拆成 (event, data) 列表；data 是 JSON 对象。
func parseSSE(t *testing.T, raw []byte) []struct {
	Event string
	Data  map[string]any
} {
	t.Helper()
	var out []struct {
		Event string
		Data  map[string]any
	}
	for _, block := range strings.Split(string(raw), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" || block == "data: [DONE]" {
			continue
		}
		var ev struct {
			Event string
			Data  map[string]any
		}
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			}
			if strings.HasPrefix(line, "data: ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev.Data); err != nil {
					t.Fatalf("SSE data 不是 JSON: %v（block=%q）", err, block)
				}
			}
		}
		out = append(out, ev)
	}
	return out
}

func TestSpeechSSE(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got, false), "tok1")

	resp, raw := f.post(t, "/v1/audio/speech",
		`{"model":"tts-1","voice":"nova","input":"你好","stream_format":"sse"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，SSE 必须是 text/event-stream", ct)
	}
	if !strings.Contains(string(raw), "[DONE]") {
		t.Fatal("SSE 结尾应有 data: [DONE]")
	}
	events := parseSSE(t, raw)
	if len(events) < 2 {
		t.Fatalf("事件数 = %d，至少应有 delta + done", len(events))
	}
	var audio []byte
	for _, ev := range events {
		switch ev.Event {
		case "speech.audio.delta":
			chunk, err := base64.StdEncoding.DecodeString(ev.Data["audio"].(string))
			if err != nil {
				t.Fatalf("delta.audio 不是 base64: %v", err)
			}
			audio = append(audio, chunk...)
		case "speech.audio.done":
			if ev.Data["type"] != "speech.audio.done" {
				t.Fatalf("done.type = %v", ev.Data["type"])
			}
		default:
			t.Fatalf("未知事件 %q", ev.Event)
		}
	}
	if !strings.HasPrefix(string(audio), "ID3") {
		t.Fatalf("分片拼回来的音频不对: %q", audio)
	}
}

func TestSpeechVoiceObject(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got, false), "tok1")

	resp, raw := f.post(t, "/v1/audio/speech",
		`{"model":"tts-1","voice":{"id":"Nova"},"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if v := resp.Header.Get("X-MiniMax-Voice"); v != "female-shaonv" {
		t.Fatalf("X-MiniMax-Voice = %q，期望 female-shaonv", v)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["voice_id"] != "female-shaonv" {
		t.Fatalf("上游 voice_id = %v", got["voice_id"])
	}
}

func TestTranscriptionsStream(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"model":           "gpt-4o-transcribe",
		"response_format": "json",
		"stream":          "true",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q，stream=true 必须是 text/event-stream", ct)
	}
	events := parseSSE(t, raw)
	if len(events) != 2 {
		t.Fatalf("事件数 = %d，期望 delta + done", len(events))
	}
	if events[0].Event != "transcript.text.delta" || events[0].Data["delta"] != "你好，这是一次兼容性测试。" {
		t.Fatalf("delta 事件不对: %+v", events[0])
	}
	if events[1].Event != "transcript.text.done" || events[1].Data["text"] != "你好，这是一次兼容性测试。" {
		t.Fatalf("done 事件不对: %+v", events[1])
	}
}

func TestTranscriptionsStreamIgnoresSubtitleFormat(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	// stream=true 只产出文本事件，不该因为容器解析不出时长而 400。
	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"response_format": "srt",
		"stream":          "true",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
}

func TestTranscriptionsAcceptsExtendedFields(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	// 2026 版 OpenAI 的字段：网关必须全部收下，不因不认识而 400。
	resp, raw := f.post(t, "/v1/audio/transcriptions", `{
		"data":"UklGRg==","mime_type":"audio/wav","model":"gpt-4o-transcribe",
		"language":"zh","languages":["zh","en"],"prompt":"上下文",
		"temperature":0.2,"timestamp_granularities":["word","segment"],
		"include":["logprobs"],"chunking_strategy":"auto",
		"keywords":["MiniMax"],"known_speaker_names":["客服"],
		"known_speaker_references":[],"response_format":"verbose_json"
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	warns, _ := out["warnings"].([]any)
	if len(warns) < 2 {
		t.Fatalf("timestamp_granularities / include 应各产生一条 warning，得到 %v", out["warnings"])
	}
}

func TestTranscriptionsJSONRejectsMissingFile(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := f.post(t, "/v1/audio/transcriptions", `{"model":"whisper-1"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("missing_audio")) {
		t.Fatalf("错误码应为 missing_audio，body=%s", raw)
	}
}
