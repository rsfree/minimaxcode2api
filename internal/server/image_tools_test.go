package server

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// newTinyServer 起一个只回固定字节的 CDN，用于 b64_json 的下载断言。
func newTinyServer(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 1×1 透明 PNG，足够让 magic bytes 判定成立。
const tinyPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

// 抠图：URL 直传，uuid 不该被中转（上游能自己取）。
func TestRemoveBackgroundDirectURL(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/remove_image_background" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		io.WriteString(w, `{"code":0,"output_url":"https://cdn.test/out.png"}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/remove-background", `{"image":"https://cdn.hailuoai.com/a.jpg"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	mu.Lock()
	if got["image_url"] != "https://cdn.hailuoai.com/a.jpg" {
		t.Fatalf("image_url = %v", got["image_url"])
	}
	mu.Unlock()

	var out struct {
		Data []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) != 1 {
		t.Fatalf("解析响应: %v body=%s", err, raw)
	}
	if out.Data[0].URL != "https://cdn.test/out.png" {
		t.Fatalf("url = %s", out.Data[0].URL)
	}
}

// data URI / 裸 base64 必须先经 upload_to_cdn 换成 cdn_url 再调工具。
func TestRemoveBackgroundDataURIUsesCDN(t *testing.T) {
	var mu sync.Mutex
	var uploads, toolCalls int
	var toolPayload map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp/upload_to_cdn":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			uploads++
			if body["mime_type"] != "image/png" {
				t.Errorf("mime_type = %v，应按 magic bytes 判成 image/png", body["mime_type"])
			}
			if _, ok := body["data"].(string); !ok {
				t.Errorf("上传缺少 base64 data: %v", body)
			}
			mu.Unlock()
			io.WriteString(w, `{"code":0,"cdn_url":"https://cdn.test/up.png"}`)
		case "/mcp/remove_image_background":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			toolCalls++
			toolPayload = body
			mu.Unlock()
			io.WriteString(w, `{"code":0,"output_url":"https://cdn.test/out.png"}`)
		default:
			http.NotFound(w, r)
		}
	}, "tok1")

	body := `{"image":"data:image/png;base64,` + tinyPNG + `"}`
	resp, raw := f.post(t, "/v1/images/remove-background", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if uploads != 1 || toolCalls != 1 {
		t.Fatalf("uploads=%d toolCalls=%d，期望各 1 次", uploads, toolCalls)
	}
	if toolPayload["image_url"] != "https://cdn.test/up.png" {
		t.Fatalf("工具收到的 image_url = %v，应为上传后的 cdn_url", toolPayload["image_url"])
	}
}

// b64_json：回的是字节，且按 magic bytes 认定类型。
func TestUpscaleReturnsB64(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(tinyPNG)
	cdn := newTinyServer(t, png)
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/enhance_image" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"code":0,"output_url":"`+cdn+`/up.png"}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/upscale",
		`{"image":"https://cdn.hailuoai.com/a.png","response_format":"b64_json"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Data []struct {
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) == 0 {
		t.Fatalf("解析响应: %v body=%s", err, raw)
	}
	decoded, err := base64.StdEncoding.DecodeString(out.Data[0].B64)
	if err != nil || len(decoded) < 8 || string(decoded[1:4]) != "PNG" {
		t.Fatalf("b64 解码后不是 PNG: %v", err)
	}
}

// 以图搜图：出参结构未公开，按第一个数组字段透出。
func TestReverseImageSearchPassthrough(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/image_reverse_search" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"code":0,"results":[{"title":"相似图","url":"https://img.test/1.jpg"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/reverse-search", `{"image":"https://cdn.hailuoai.com/a.jpg"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Results) != 1 {
		t.Fatalf("results 未透出: %v body=%s", err, raw)
	}
	if out.Results[0]["url"] != "https://img.test/1.jpg" {
		t.Fatalf("results[0] = %v", out.Results[0])
	}
}

// 入参校验：没有图、response_format 非法都要 400，且不打上游。
func TestImageToolValidation(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("校验失败的请求不该打到上游")
	}, "tok1")
	cases := []string{
		`{}`,
		`{"image":"","image_url":""}`,
		`{"image":"https://cdn.hailuoai.com/a.png","response_format":"png"}`,
	}
	for _, body := range cases {
		resp, raw := f.post(t, "/v1/images/remove-background", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body=%s 状态码 = %d，期望 400，resp=%s", body, resp.StatusCode, raw)
		}
	}
}

// 非 CDN 的公网 URL：网关要先下载、再上传换 cdn_url，才发给抠图工具。
func TestRemoveBackgroundFetchesAndUploadsNonCDN(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(tinyPNG)
	src := newTinyServer(t, png)

	var mu sync.Mutex
	var uploads int
	var toolPayload map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/mcp/upload_to_cdn":
			mu.Lock()
			uploads++
			mu.Unlock()
			io.WriteString(w, `{"code":0,"cdn_url":"https://cdn.hailuoai.com/up.png"}`)
		case "/mcp/remove_image_background":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			toolPayload = body
			mu.Unlock()
			io.WriteString(w, `{"code":0,"output_url":"https://cdn.hailuoai.com/out.png"}`)
		default:
			http.NotFound(w, r)
		}
	}, "tok1")
	f.srv.fetchAllowPrivate = true

	resp, raw := f.post(t, "/v1/images/remove-background",
		`{"image":"`+src+`/photo.png"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	mu.Lock()
	defer mu.Unlock()
	if uploads != 1 {
		t.Fatalf("uploads=%d，期望 1 次中转", uploads)
	}
	if toolPayload["image_url"] != "https://cdn.hailuoai.com/up.png" {
		t.Fatalf("工具收到 %v", toolPayload["image_url"])
	}
}
