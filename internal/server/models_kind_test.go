package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// /v1/models 是客户端的**能力发现面**：只列对话模型时，图片/视频模型在选择器里
// 根本不存在；列了却不带类别，客户端又会把视频模型发到 /v1/chat/completions。
// 这条用例同时钉住「怎么算全」和「必须带 type」。
func TestModelsCoverImageAndVideo(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("模型清单不该打到上游: %s", r.URL.Path)
	}, "tok1")

	resp, raw := f.get(t, "/v1/models")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}

	var out struct {
		Data []struct {
			ID      string   `json:"id"`
			Type    string   `json:"type"`
			Context int      `json:"context_window"`
			Aliases []string `json:"aliases"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v，body=%s", err, raw)
	}

	got := map[string]string{}
	ctx := map[string]int{}
	for _, m := range out.Data {
		if m.Type == "" {
			t.Fatalf("%s: 缺 type —— 客户端无法判断该发哪个端点", m.ID)
		}
		got[m.ID] = m.Type
		ctx[m.ID] = m.Context
	}

	want := map[string]string{
		"MiniMax-M3.1-Flash": "chat",
		"MiniMax-M3":         "chat",
		"MiniMax-Image":      "image",
		"MiniMax-H3":         "video",
		"MiniMax-Hailuo-2.3": "video",
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Fatalf("%s: type = %q，期望 %q（实际清单=%v）", id, got[id], kind, got)
		}
	}

	// 对话模型要有上下文；媒体模型不能有（否则客户端会以为它们能吃 messages）。
	for id := range want {
		if want[id] == "chat" && ctx[id] <= 0 {
			t.Fatalf("%s: 对话模型缺 context_window", id)
		}
		if want[id] != "chat" && ctx[id] != 0 {
			t.Fatalf("%s: 媒体模型不该带 context_window=%d", id, ctx[id])
		}
	}
}

// 媒体模型的别名也要能查详情：/v1/models/h3 与 /v1/models/MiniMax-H3 等价。
func TestModelAliasLookupMedia(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("模型查询不该打到上游")
	}, "tok1")

	cases := map[string]string{
		"h3":       "MiniMax-H3",
		"h2.3":     "MiniMax-Hailuo-2.3",
		"hailuo":   "MiniMax-Hailuo-2.3",
		"image":    "MiniMax-Image",
		"image-01": "MiniMax-Image",
	}
	for alias, wantID := range cases {
		resp, raw := f.get(t, "/v1/models/"+alias)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: 状态码 = %d，body=%s", alias, resp.StatusCode, raw)
		}
		var out struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || out.ID != wantID {
			t.Fatalf("%s: id = %q err=%v，期望 %q", alias, out.ID, err, wantID)
		}
	}
}

// 媒体模型发到对话入口：必须 400 指路，而不是透传给上游换来一句看不懂的参数错误。
func TestChatRejectsMediaModel(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("媒体模型不该打到对话上游: %s", r.URL.Path)
	}, "tok1")

	resp, raw := f.post(t, "/v1/chat/completions",
		`{"model":"MiniMax-H3","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if body := string(raw); !strings.Contains(body, "/v1/contents/generations/tasks") {
		t.Fatalf("错误信息应指向视频端点，body=%s", body)
	}

	// Anthropic 入口同一套闸门。
	resp, raw = f.post(t, "/v1/messages",
		`{"model":"MiniMax-Image","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("/v1/messages 状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if body := string(raw); !strings.Contains(body, "/v1/images/generations") {
		t.Fatalf("/v1/messages 错误信息应指向图片端点，body=%s", body)
	}
}

// 图片入口：已知的对话/视频模型要 400；**清单外的未知写法仍然放行**。
//
// 后者是刻意保留的宽松：上游 image_synthesize 没有 model 参数，这个字段纯装饰，
// 而生态里不少客户端会写死 dall-e-3 / gpt-image-1。为一个不生效的字段拒绝请求
// 只会把能用的客户端挡在门外。
func TestImagesModelKindGate(t *testing.T) {
	hits := 0
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"total_success":1,"success_items":[
			{"output_file":"slot_0.png","output_url":"https://cdn.test/a.png","has_image_output":true}]}`))
	}, "tok1")

	resp, raw := f.post(t, "/v1/images/generations", `{"model":"MiniMax-M3","prompt":"x"}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("对话模型发图片入口 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if hits != 0 {
		t.Fatalf("被拦下的请求不该打到上游，hits=%d", hits)
	}

	resp, raw = f.post(t, "/v1/images/generations", `{"model":"dall-e-3","prompt":"x"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("未知模型应放行（装饰字段），状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if hits != 1 {
		t.Fatalf("放行的请求应打到上游一次，hits=%d", hits)
	}
}
