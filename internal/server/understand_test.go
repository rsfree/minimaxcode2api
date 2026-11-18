package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// 图片问答：入参折成 image_info 数组，出参取 results[].text。
func TestDescribeImages(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/images_understand" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		io.WriteString(w, `{"code":0,"results":[{"text":"一只橘猫趴在窗台上"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/describe",
		`{"items":[{"url":"https://cdn.hailuoai.com/a.png","prompt":"这是什么"}],"prompt":"默认提示词"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}

	mu.Lock()
	list, _ := got["image_info"].([]any)
	mu.Unlock()
	if len(list) != 1 {
		t.Fatalf("上游收到 %v", got)
	}
	item, _ := list[0].(map[string]any)
	if item["url"] != "https://cdn.hailuoai.com/a.png" {
		t.Fatalf("url = %v", item["url"])
	}
	if item["prompt"] != "这是什么" {
		t.Fatalf("逐项 prompt 应优先，实际 %v", item["prompt"])
	}

	var out struct {
		Data []struct {
			Index int    `json:"index"`
			Text  string `json:"text"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Data) != 1 {
		t.Fatalf("解析响应: %v body=%s", err, raw)
	}
	if out.Data[0].Text != "一只橘猫趴在窗台上" {
		t.Fatalf("text = %q", out.Data[0].Text)
	}
}

// 没给 prompt 时要落到默认值，而不是发一个空 prompt 给上游。
func TestUnderstandDefaultPrompt(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"code":0,"results":[{"text":"好的"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/audio/understand", `{"items":[{"data":"UklGRg==","mime_type":"audio/wav"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	list, _ := got["audio_info"].([]any)
	item, _ := list[0].(map[string]any)
	p, _ := item["prompt"].(string)
	if strings.TrimSpace(p) == "" {
		t.Fatalf("默认 prompt 为空: %v", got)
	}
}

// 上游少给结果时必须显式告警，不能静默截断。
func TestUnderstandCountMismatchWarns(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"code":0,"results":[{"text":"只有一条"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/videos/understand",
		`{"items":[{"url":"https://cdn.hailuoai.com/1.mp4"},{"url":"https://cdn.hailuoai.com/2.mp4"}],"prompt":"描述"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(out.Warnings) == 0 {
		t.Fatalf("条数不一致却没有告警: %s", raw)
	}
}

// 入参校验：空 items、超过 10 项、缺 url/data 都 400。
func TestUnderstandValidation(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("校验失败的请求不该打到上游")
	}, "tok1")

	var many []string
	for i := 0; i < 11; i++ {
		many = append(many, `{"url":"https://cdn.test/`+string(rune('a'+i))+`.png"}`)
	}
	cases := []string{
		`{}`,
		`{"items":[]}`,
		`{"items":[{}]}`,
		`{"items":[` + strings.Join(many, ",") + `]}`,
	}
	for _, body := range cases {
		resp, raw := f.post(t, "/v1/images/describe", body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("body=%.60s 状态码 = %d，期望 400，resp=%s", body, resp.StatusCode, raw)
		}
	}
}

// 普通公网 URL 不被上游接受，网关要下载后内联成 base64。
func TestUnderstandFetchInlinesURL(t *testing.T) {
	png, _ := base64.StdEncoding.DecodeString(tinyPNG)
	cdn := newTinyServer(t, png)

	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/images_understand" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		io.WriteString(w, `{"code":0,"results":[{"success":true,"description":"红色","error":""}]}`)
	}, "tok1")
	f.srv.fetchAllowPrivate = true // httptest 起在 127.0.0.1

	resp, raw := f.post(t, "/v1/images/describe",
		`{"items":[{"url":"`+cdn+`/a.png","prompt":"什么颜色"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	mu.Lock()
	list, _ := got["image_info"].([]any)
	mu.Unlock()
	item, _ := list[0].(map[string]any)
	if _, hasURL := item["url"]; hasURL {
		t.Fatalf("公网 URL 不该直接下发给上游: %v", item)
	}
	if b64, _ := item["data"].(string); b64 == "" {
		t.Fatalf("应内联成 base64: %v", item)
	}
}

// SSRF：内网地址必须在拨号阶段就被拒。
func TestFetchExternalBlocksPrivate(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("不该打到上游")
	}, "tok1")
	for _, u := range []string{
		"http://127.0.0.1:9/x.png",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/x.png",
		"file:///etc/passwd",
	} {
		if _, _, err := f.srv.fetchExternal(context.Background(), u, 1<<20); err == nil {
			t.Fatalf("%s 应被拒", u)
		}
	}
}
