package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/linguo2625469/minimaxcode2api/internal/audioproto"
)

// fakeAudioServer 是 TTS 链路的最小假上游：synthesize_speech 回一个 output_url，
// 该 URL 指回同一个测试服务器上的 mp3 字节。
func fakeAudioServer(t *testing.T, mu *sync.Mutex, got *map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp/synthesize_speech":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			*got = body
			mu.Unlock()
			fmt.Fprintf(w, `{"code":0,"output_url":"http://%s/speech.mp3"}`, r.Host)
		case "/speech.mp3":
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write([]byte("ID3\x04\x00\x00\x00\x00\x00\x00fake-audio-payload"))
		default:
			http.NotFound(w, r)
		}
	}
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

// wavHeader 是 44 字节的 WAV 头 + 一点静音数据，够 sniffMime 识别。
func wavHeader() []byte {
	b := make([]byte, 64)
	copy(b[0:], "RIFF")
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	return b
}

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

// parseSSE 把响应体拆成 (event, data)；data 是 JSON 对象。
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

// ---------- speech ----------

func TestSpeechMapsOpenAIVoice(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got), "tok1")

	resp, raw := f.post(t, "/v1/audio/speech",
		`{"model":"tts-1","voice":"onyx","input":"你好"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "audio/") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if v := resp.Header.Get("X-MiniMax-Format"); v != "mp3" {
		t.Fatalf("X-MiniMax-Format = %q，期望 mp3", v)
	}
	if v := resp.Header.Get("X-MiniMax-Voice"); v != "male-qn-badao" {
		t.Fatalf("X-MiniMax-Voice = %q，期望 male-qn-badao（onyx 的映射）", v)
	}
	if v := resp.Header.Get("X-MiniMax-Voice-Alias"); v != "onyx" {
		t.Fatalf("X-MiniMax-Voice-Alias = %q，期望 onyx", v)
	}

	mu.Lock()
	payload := got
	mu.Unlock()
	if payload["voice_id"] != "male-qn-badao" {
		t.Fatalf("上游 voice_id = %v，期望 male-qn-badao", payload["voice_id"])
	}
	if _, ok := payload["speed"]; ok {
		t.Fatalf("未指定 speed 时不该传 speed，得到 %v", payload["speed"])
	}
}

func TestSpeechPassesThroughNativeVoice(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got), "tok1")

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
}

func TestSpeechVoiceObject(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got), "tok1")

	resp, raw := f.post(t, "/v1/audio/speech",
		`{"model":"tts-1","voice":{"id":"Nova"},"input":"hi"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if v := resp.Header.Get("X-MiniMax-Voice"); v != "female-shaonv" {
		t.Fatalf("X-MiniMax-Voice = %q，期望 female-shaonv", v)
	}
}

func TestSpeechSSE(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got), "tok1")

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

func TestSpeechUpstreamBilling(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":402,"message":"plan balance has run out"}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/audio/speech", `{"model":"tts-1","voice":"nova","input":"hi"}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("状态码 = %d，期望 402，body=%s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("insufficient_quota")) {
		t.Fatalf("402 应映射成 insufficient_quota，body=%s", raw)
	}
}

func TestSpeechRequiredFields(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got), "tok1")

	cases := []struct{ name, body, param string }{
		{"缺 model", `{"voice":"nova","input":"hi"}`, "model"},
		{"缺 voice", `{"model":"tts-1","input":"hi"}`, "voice"},
		{"空 voice 对象", `{"model":"tts-1","voice":{"id":""},"input":"hi"}`, "voice"},
	}
	for _, c := range cases {
		resp, raw := f.post(t, "/v1/audio/speech", c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400，body=%s", c.name, resp.StatusCode, raw)
		}
		var out struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.Error.Param != c.param {
			t.Fatalf("%s: param = %q，期望 %q", c.name, out.Error.Param, c.param)
		}
	}
	if f.upCalls() != 0 {
		t.Fatalf("必填校验失败不该打上游，实际 %d 次", f.upCalls())
	}
}

// 未声明的取值面必须 400，不能降级/钳位/静默忽略。
func TestSpeechRejectsUnsupportedSurface(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got), "tok1")

	longInput := fmt.Sprintf(`{"model":"tts-1","voice":"nova","input":%q}`,
		strings.Repeat("好", audioproto.MaxSpeechInput+1))
	cases := []struct{ name, body, param string }{
		{"空 input", `{"model":"tts-1","voice":"nova","input":"  "}`, "input"},
		{"input 超长", longInput, "input"},
		{"非 mp3 格式", `{"model":"tts-1","voice":"nova","input":"hi","response_format":"wav"}`, "response_format"},
		{"未知格式", `{"model":"tts-1","voice":"nova","input":"hi","response_format":"ogg"}`, "response_format"},
		{"speed 过大", `{"model":"tts-1","voice":"nova","input":"hi","speed":3}`, "speed"},
		{"speed 过小", `{"model":"tts-1","voice":"nova","input":"hi","speed":0.25}`, "speed"},
		{"instructions", `{"model":"tts-1","voice":"nova","input":"hi","instructions":"温柔"}`, "instructions"},
		{"stream_format", `{"model":"tts-1","voice":"nova","input":"hi","stream_format":"multipart"}`, "stream_format"},
		{"volume 越界", `{"model":"tts-1","voice":"nova","input":"hi","volume":11}`, "volume"},
		{"pitch 越界", `{"model":"tts-1","voice":"nova","input":"hi","pitch":13}`, "pitch"},
		{"emotion 越界", `{"model":"tts-1","voice":"nova","input":"hi","emotion":"excited"}`, "emotion"},
		{"未知字段", `{"model":"tts-1","voice":"nova","input":"hi","foo":1}`, "foo"},
	}
	for _, c := range cases {
		resp, raw := f.post(t, "/v1/audio/speech", c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400，body=%s", c.name, resp.StatusCode, raw)
		}
		var out struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.Error.Param != c.param {
			t.Fatalf("%s: param = %q，期望 %q，body=%s", c.name, out.Error.Param, c.param, raw)
		}
	}
	if f.upCalls() != 0 {
		t.Fatalf("未声明的取值不该打上游，实际 %d 次", f.upCalls())
	}
}

func TestSpeechRejectsWrongContentType(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeAudioServer(t, &mu, &got), "tok1")

	req, _ := http.NewRequest(http.MethodPost, f.front.URL+"/v1/audio/speech",
		strings.NewReader(`{"model":"tts-1","voice":"nova","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("状态码 = %d，期望 415", resp.StatusCode)
	}
}

// ---------- transcriptions ----------

func TestTranscriptionsTextFormat(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"model":           "whisper-1",
		"response_format": "text",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if string(raw) != "你好，这是一次兼容性测试。" {
		t.Fatalf("body = %q，期望纯文本", raw)
	}
}

func TestTranscriptionsVerboseJSON(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
		"model":           "whisper-1",
		"response_format": "verbose_json",
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
	// 上游没有真实数据，按规范不得附 segments / language / duration。
	for _, key := range []string{"segments", "language", "duration", "words"} {
		if _, ok := out[key]; ok {
			t.Fatalf("verbose_json 不应含伪造字段 %q: %v", key, out)
		}
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
		t.Fatalf("Content-Type = %q", ct)
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

func TestTranscriptionsRejectsUnsupportedFormats(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	for _, format := range []string{"srt", "vtt", "diarized_json"} {
		resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
			"model":           "whisper-1",
			"response_format": format,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400，body=%s", format, resp.StatusCode, raw)
		}
		var out struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.Error.Param != "response_format" {
			t.Fatalf("%s: param = %q，期望 response_format", format, out.Error.Param)
		}
	}
	if f.upCalls() != 0 {
		t.Fatalf("不支持的格式不该打上游，实际 %d 次", f.upCalls())
	}
}

func TestTranscriptionsRejectsUnsupportedFields(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	cases := []struct {
		field string
		value string
		param string
	}{
		{"language", "zh", "language"},
		{"prompt", "上下文", "prompt"},
		{"temperature", "0.2", "temperature"},
		{"timestamp_granularities[]", "word", "timestamp_granularities"},
		{"include[]", "logprobs", "include"},
		{"chunking_strategy", "auto", "chunking_strategy"},
		{"keywords[]", "MiniMax", "keywords"},
		{"known_speaker_names[]", "客服", "known_speaker_names"},
		{"unknown_vendor_field", "x", "unknown_vendor_field"},
	}
	for _, c := range cases {
		resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), map[string]string{
			"model": "whisper-1",
			c.field: c.value,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400，body=%s", c.field, resp.StatusCode, raw)
		}
		var out struct {
			Error struct {
				Param string `json:"param"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &out)
		if out.Error.Param != c.param {
			t.Fatalf("%s: param = %q，期望 %q，body=%s", c.field, out.Error.Param, c.param, raw)
		}
	}
	if f.upCalls() != 0 {
		t.Fatalf("不支持的字段不该打上游，实际 %d 次", f.upCalls())
	}
}

func TestTranscriptionsJSONUnknownField(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := f.post(t, "/v1/audio/transcriptions",
		`{"data":"UklGRg==","model":"whisper-1","foo":1}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte(`"param":"foo"`)) {
		t.Fatalf("应指向 param=foo，body=%s", raw)
	}
}

func TestTranscriptionsDuplicateFileRejected(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for i := 0; i < 2; i++ {
		fw, _ := mw.CreateFormFile("file", fmt.Sprintf("a%d.wav", i))
		_, _ = fw.Write(wavHeader())
	}
	_ = mw.WriteField("model", "whisper-1")
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, f.front.URL+"/v1/audio/transcriptions", &buf)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte(`"param":"file"`)) {
		t.Fatalf("应指向 param=file，body=%s", raw)
	}
	if f.upCalls() != 0 {
		t.Fatalf("重复 file 不该打上游，实际 %d 次", f.upCalls())
	}
}

func TestTranscriptionsRequiresModel(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	resp, raw := multipartAudio(t, f.front.URL+"/v1/audio/transcriptions", wavHeader(), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte(`"param":"model"`)) {
		t.Fatalf("应指向 param=model，body=%s", raw)
	}
}

func TestTranscriptionsWrongContentType(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, fakeTranscribeServer(t, &mu, &got), "tok1")

	req, _ := http.NewRequest(http.MethodPost, f.front.URL+"/v1/audio/transcriptions",
		strings.NewReader("--x--"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("状态码 = %d，期望 415", resp.StatusCode)
	}
}

// 上游凭据失效必须是 502：回 401 会让客户端以为自己的 key 错了。
func TestMediaUpstreamAuthMapsTo502(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":401,"message":"invalid token"}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/audio/speech", `{"model":"tts-1","voice":"nova","input":"hi"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", resp.StatusCode, raw)
	}
	if !bytes.Contains(raw, []byte("upstream_auth")) {
		t.Fatalf("错误码应为 upstream_auth，body=%s", raw)
	}
}

// ---------- 能力声明 ----------

func TestAudioCapabilities(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("能力接口不该打上游: %s", r.URL.Path)
	}, "tok1")

	resp, raw := f.get(t, "/v1/audio/capabilities")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Speech struct {
			Formats []string `json:"response_formats"`
			Speed   struct {
				Min float64 `json:"min"`
				Max float64 `json:"max"`
			} `json:"speed"`
			Instructions bool `json:"instructions"`
			Voices       struct {
				OpenAI map[string]string `json:"openai"`
			} `json:"voices"`
		} `json:"speech"`
		Transcription struct {
			Formats            []string          `json:"response_formats"`
			UnsupportedFormats map[string]string `json:"unsupported_response_formats"`
			Timestamps         bool              `json:"timestamps"`
		} `json:"transcription"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(out.Speech.Formats) != 1 || out.Speech.Formats[0] != "mp3" {
		t.Fatalf("speech.response_formats = %v，期望仅 mp3", out.Speech.Formats)
	}
	if out.Speech.Speed.Min != audioproto.SpeedMin || out.Speech.Speed.Max != audioproto.SpeedMax {
		t.Fatalf("speed 声明 = %+v", out.Speech.Speed)
	}
	if out.Speech.Instructions {
		t.Fatal("instructions 应声明为不支持")
	}
	if len(out.Speech.Voices.OpenAI) != 13 {
		t.Fatalf("OpenAI 音色映射数 = %d，期望 13", len(out.Speech.Voices.OpenAI))
	}
	if _, ok := out.Transcription.UnsupportedFormats["srt"]; !ok {
		t.Fatal("transcription 应声明 srt 不支持")
	}
	if out.Transcription.Timestamps {
		t.Fatal("timestamps 应声明为不支持")
	}
}

// 非幂等：上游 5xx 可能已经生成并计费，绝不能换号重试。
func TestSpeechDoesNotRetryAmbiguousFailure(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream boom", http.StatusInternalServerError)
	}, "a", "b", "c")

	resp, raw := f.post(t, "/v1/audio/speech", `{"model":"tts-1","voice":"nova","input":"hi"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", resp.StatusCode, raw)
	}
	if n := f.upCalls(); n != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1（非幂等不重试）", n)
	}
}
