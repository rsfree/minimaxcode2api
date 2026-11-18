package server

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// 同步视频：一次调用直达 output_url，请求体与异步任务同一套 canonical。
func TestSyncVideoSingleCall(t *testing.T) {
	var mu sync.Mutex
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/gen_videos" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"success_items":[{"output_url":"https://cdn.test/v.mp4"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/contents/generations",
		`{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"海边灯塔，黄昏"}],"duration":6,"resolution":"720p"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}

	mu.Lock()
	reqs, _ := got["requests"].([]any)
	mu.Unlock()
	if len(reqs) != 1 {
		t.Fatalf("上游收到 %d 项，期望 1，body=%v", len(reqs), got)
	}
	item, _ := reqs[0].(map[string]any)
	if item["prompt"] != "海边灯塔，黄昏" {
		t.Fatalf("prompt = %v", item["prompt"])
	}
	if item["model"] != "MiniMax-Hailuo-2.3" {
		t.Fatalf("model = %v", item["model"])
	}
	if item["resolution"] != "768P" {
		t.Fatalf("resolution = %v，720p 应映射成 768P", item["resolution"])
	}
	if item["duration"] != float64(6) {
		t.Fatalf("duration = %v", item["duration"])
	}

	var out struct {
		Created int64 `json:"created"`
		Data    []struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析响应: %v，body=%s", err, raw)
	}
	if len(out.Data) != 1 || out.Data[0].URL != "https://cdn.test/v.mp4" {
		t.Fatalf("data = %+v，期望一条 url", out.Data)
	}
}

// 首帧图必须折成 {url|data} 信封，并带 reference_type=first_frame。
func TestSyncVideoFirstFrame(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"code":0,"success_items":[{"output_url":"https://cdn.test/v.mp4"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/contents/generations",
		`{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"让它动起来"},
			{"type":"image_url","image_url":{"url":"https://cdn.hailuoai.com/f.png"}}],"duration":6}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	reqs, _ := got["requests"].([]any)
	item, _ := reqs[0].(map[string]any)
	img, _ := item["input_image"].(map[string]any)
	if img["url"] != "https://cdn.hailuoai.com/f.png" {
		t.Fatalf("input_image = %v，cdn_url 应原样下发", item["input_image"])
	}
	if item["reference_type"] != "first_frame" {
		t.Fatalf("reference_type = %v", item["reference_type"])
	}
}

// 同步工具是"老视频工具"，只支持 Hailuo-2.3 —— 这条是上游 400 原文实测出来的，
// 不是设计选择。H3 / H3-Max 必须指回异步两段式。
func TestSyncVideoRejectsH3Family(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("不该打到上游")
	}, "tok1")

	for _, model := range []string{"MiniMax-H3", "h3-max"} {
		resp, raw := f.post(t, "/v1/contents/generations",
			`{"model":"`+model+`","content":[{"type":"text","text":"x"}],"duration":6}`)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d body=%s", model, resp.StatusCode, raw)
		}
		var out struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: 解析错误体: %v", model, err)
		}
		if out.Error["param"] != "model" || out.Error["code"] != "unsupported_model" {
			t.Fatalf("%s: 错误 = %v", model, out.Error)
		}
		msg, _ := out.Error["message"].(string)
		if !strings.Contains(msg, "contents/generations/tasks") {
			t.Fatalf("%s: 错误没指路异步: %v", model, msg)
		}
	}
}

// 同步接口的窄能力面：越界必须 400，不能悄悄降级。
func TestSyncVideoRejections(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("校验失败的请求不该打到上游")
	}, "tok1")

	cases := []struct {
		name  string
		body  string
		param string
	}{
		{"8 秒越界", `{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"x"}],"duration":8}`, "duration"},
		{"1080P 只配 6 秒", `{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"x"}],"resolution":"1080p","duration":10}`, "duration"},
		{"尾帧不支持", `{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"x"},
			{"type":"image_url","image_url":{"url":"https://a/1.png"},"role":"last_frame"}]}`, "content"},
		{"全模态参考不支持", `{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"x"},
			{"type":"image_url","image_url":{"url":"https://a/1.png"},"role":"reference_image"}]}`, "content"},
	}
	for _, c := range cases {
		resp, raw := f.post(t, "/v1/contents/generations", c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400，body=%s", c.name, resp.StatusCode, raw)
		}
		var out struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: 解析错误体: %v", c.name, err)
		}
		if out.Error["param"] != c.param {
			t.Fatalf("%s: param = %v，期望 %s", c.name, out.Error["param"], c.param)
		}
	}
}

// 同步工具没有 ratio：写了要告警，而且不能出现在上游载荷里。
func TestSyncVideoRatioWarnsInsteadOfSilentDrop(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"code":0,"success_items":[{"output_url":"https://cdn.test/v.mp4"}]}`)
	}, "tok1")

	resp, raw := f.post(t, "/v1/contents/generations",
		`{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"x"}],"ratio":"16:9","duration":6}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	reqs, _ := got["requests"].([]any)
	item, _ := reqs[0].(map[string]any)
	if _, exists := item["ratio"]; exists {
		t.Fatalf("同步工具不接受 ratio，却下发了: %v", item["ratio"])
	}
	var out struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if !strings.Contains(strings.Join(out.Warnings, "|"), "ratio") {
		t.Fatalf("ratio 被静默丢弃，warnings = %v", out.Warnings)
	}
}

// 异步链路下 H3-Max 的专属约束：480P 合法、最短 5 秒。
func TestAsyncVideoH3MaxResolution(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"task_id":"cgt-max"}`)
	}, "tok1")

	resp, raw := f.post(t, videoTaskPath,
		`{"model":"MiniMax-H3-Max","content":[{"type":"text","text":"x"}],"duration":5,"resolution":"480p"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("480P 是 H3-Max 的合法档位，状态码 = %d body=%s", resp.StatusCode, raw)
	}
	if got["model"] != "MiniMax-H3-Max" || got["resolution"] != "480P" {
		t.Fatalf("上游载荷 model=%v resolution=%v", got["model"], got["resolution"])
	}

	resp, raw = f.post(t, videoTaskPath,
		`{"model":"MiniMax-H3-Max","content":[{"type":"text","text":"x"}],"resolution":"720p","duration":4}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("H3-Max 最短 5 秒，4 秒应被拒，状态码 = %d body=%s", resp.StatusCode, raw)
	}
}
