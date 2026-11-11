package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// get 发一个带鉴权的 GET（fixture 只提供了 POST 助手）。
func (f *fixture) get(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, f.front.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// 图片生成：请求要按槽位编号，响应要把 output_url 映射成 OpenAI 的 data[].url。
func TestImagesGeneration(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/image_synthesize" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"total_success":2,
			"success_items":[
				{"output_file":"slot_0.png","output_url":"https://cdn.test/a.png","has_image_output":true},
				{"output_file":"slot_1.png","output_url":"https://cdn.test/b.png","has_image_output":true}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations",
		`{"prompt":"一只橘猫","n":2,"size":"1792x1024","quality":"hd"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}

	mu.Lock()
	reqs, _ := got["requests"].([]any)
	mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("上游收到 %d 个槽位，期望 2，body=%v", len(reqs), got)
	}
	first, _ := reqs[0].(map[string]any)
	if first["output_file"] != "slot_0.png" {
		t.Fatalf("槽位名 = %v，期望 slot_0.png", first["output_file"])
	}
	if first["aspect_ratio"] != "16:9" {
		t.Fatalf("aspect_ratio = %v，1792x1024 应映射成 16:9", first["aspect_ratio"])
	}
	if first["resolution"] != "2K" {
		t.Fatalf("resolution = %v，quality=hd 应映射成 2K", first["resolution"])
	}

	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应: %v，body=%s", err, raw)
	}
	if len(out.Data) != 2 || out.Data[0].URL != "https://cdn.test/a.png" {
		t.Fatalf("data = %+v，期望两条 url", out.Data)
	}
}

// b64_json：必须按 magic bytes 判类型，而不是信上游的 Content-Type。
// 实测上游把 JPEG 标成 image/png，客户端按 header 解会坏。
func TestImagesB64SniffsRealType(t *testing.T) {
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png") // 故意撒谎：内容是 JPEG
		_, _ = w.Write(jpeg)
	}))
	defer cdn.Close()

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/image_synthesize" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"success_items":[{"output_file":"slot_0.png",
			"output_url":"`+cdn.URL+`/x.png","has_image_output":true}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations",
		`{"prompt":"x","response_format":"b64_json"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 {
		t.Fatalf("解析响应: %v，body=%s", err, raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil {
		t.Fatalf("b64 解码失败: %v", err)
	}
	if len(decoded) < 3 || decoded[0] != 0xFF || decoded[1] != 0xD8 || decoded[2] != 0xFF {
		t.Fatalf("解码后不是 JPEG 原始字节: % x", decoded[:min(6, len(decoded))])
	}
}

// 402 是计费拦截：必须原样映射成 402，且不能换号重试（换号也一样扣不到）。
func TestImagesBillingNotRetried(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":402,"message":"plan balance has run out"}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations", `{"prompt":"x"}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("状态码 = %d，期望 402，body=%s", resp.StatusCode, raw)
	}
	if n := f.upCalls(); n != 1 {
		t.Fatalf("上游被调用 %d 次，计费错误不应重试", n)
	}
	if !strings.Contains(string(raw), "insufficient_quota") {
		t.Fatalf("错误码未映射成 insufficient_quota: %s", raw)
	}
}

// 全部失败时不能装作成功，要把上游的第一条错误透出去。
func TestImagesAllFailedIs502(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"total_success":0,"total_failed":1,
			"success_items":[],"failed_items":[{"output_file":"slot_0.png","error_msg":"upstream internal error"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations", `{"prompt":"x"}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upstream internal error") {
		t.Fatalf("上游错误信息没透出: %s", raw)
	}
}

// 审核拒答要归因成 400 content_filter —— 重试无用，客户端应改内容。
func TestImagesContentFilterIs400(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"total_success":0,"total_failed":1,
			"success_items":[],"failed_items":[{"output_file":"slot_0.png","error_msg":"prompt 违规"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations", `{"prompt":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "content_filter") {
		t.Fatalf("应归因成 content_filter: %s", raw)
	}
}

// edits 入口：文本引用是非法载体，必须 400 且不打上游。
func TestImageEditsRejectsTextReference(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("非法载体不该打到上游")
	}, "tok1")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("image", "https://example.com/a.png")
	_ = mw.WriteField("prompt", "改成水彩")
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, f.front.URL+"/v1/images/edits", &buf)
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
}

// edits 入口：文件 part 折成 data URI 后走同一条 canonical。
func TestImageEditsFilePart(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"success_items":[{"output_file":"slot_0.png",
			"output_url":"https://cdn.test/out.png","has_image_output":true}]}`)
	}, "tok1")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("image", "a.png")
	_, _ = part.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0}) // JPEG 头
	_ = mw.WriteField("prompt", "改成水彩")
	_ = mw.Close()

	req, _ := http.NewRequest(http.MethodPost, f.front.URL+"/v1/images/edits", &buf)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}

	reqs, _ := got["requests"].([]any)
	if len(reqs) != 1 {
		t.Fatalf("上游槽位 = %v", got)
	}
	item, _ := reqs[0].(map[string]any)
	urls, _ := item["input_urls"].([]any)
	if len(urls) != 1 || !strings.HasPrefix(urls[0].(string), "data:image/jpeg;base64,") {
		t.Fatalf("文件未折成 data URI: %v", item["input_urls"])
	}
}

// 语音：上游给 CDN 链接，网关必须自己下载并以音频字节回应。
func TestSpeechReturnsAudioBytes(t *testing.T) {
	mp3 := []byte{0x49, 0x44, 0x33, 0x04, 0x00, 0x00, 0x00}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(mp3)
	}))
	defer cdn.Close()

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/synthesize_speech" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"output_url":"`+cdn.URL+`/a.mp3"}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/audio/speech", `{"input":"你好","voice":"female-shaonv"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type = %q，期望 audio/mpeg", ct)
	}
	if !bytes.Equal(raw, mp3) {
		t.Fatalf("音频字节被改动: % x", raw)
	}
}

// 转写：multipart 上传要转成 base64 内联（上游不收原始二进制流）。
func TestTranscriptionsMultipart(t *testing.T) {
	var mu sync.Mutex
	var seen map[string]any

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/listen_audio" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		seen = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"transcription":"这是测试语音"}`)
	}, "tok1")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = part.Write([]byte("RIFF____WAVEfmt "))
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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "这是测试语音") {
		t.Fatalf("转写结果没透出: %s", raw)
	}

	mu.Lock()
	info, _ := seen["audio_info"].(map[string]any)
	mu.Unlock()
	if info == nil || info["data"] == "" {
		t.Fatalf("上游没收到内联音频: %v", seen)
	}
	if info["mime_type"] != "audio/wav" {
		t.Fatalf("mime_type = %v，应按 magic bytes 判成 audio/wav", info["mime_type"])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// 音乐：默认回音频字节，response_format=url 时回 JSON。
func TestMusicBinaryAndURL(t *testing.T) {
	mp3 := []byte{0x49, 0x44, 0x33, 0x03, 0x00}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(mp3)
	}))
	defer cdn.Close()

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/batch_text_to_music" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"success_items":[{"output_file":"slot_0.mp3",
			"output_url":"`+cdn.URL+`/m.mp3"}],"failed_items":[]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/audio/music", `{"prompt":"轻松的钢琴背景音乐，温柔"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type = %q，期望 audio/mpeg", ct)
	}
	if !bytes.Equal(raw, mp3) {
		t.Fatalf("音频字节被改动: % x", raw)
	}

	resp, raw = f.post(t, "/v1/audio/music", `{"prompt":"轻松的钢琴背景音乐，温柔","response_format":"url"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(raw), "/m.mp3") {
		t.Fatalf("url 模式 = %d %s", resp.StatusCode, raw)
	}
}
