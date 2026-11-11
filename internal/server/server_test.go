package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/config"
	"github.com/linguo2625469/minimaxcode2api/internal/oauth"
	"github.com/linguo2625469/minimaxcode2api/internal/pool"
	"github.com/linguo2625469/minimaxcode2api/internal/reqlog"
	"github.com/linguo2625469/minimaxcode2api/internal/store"
	"github.com/linguo2625469/minimaxcode2api/internal/upstream"
	"github.com/linguo2625469/minimaxcode2api/internal/usage"
)

const testKey = "sk-test-key"

// anthropicMessageOK 一条最小的 Anthropic Messages 非流式响应。
const anthropicMessageOK = `{
  "id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M3.1-Flash",
  "content":[{"type":"text","text":"pong"}],
  "stop_reason":"end_turn",
  "usage":{"input_tokens":11,"output_tokens":3,"cache_read_input_tokens":4}
}`

type fixture struct {
	srv     *Server
	front   *httptest.Server
	upHits  func() []string // 上游按顺序收到的 access token
	upCalls func() int
}

// newFixture 起一套「前端 + 假上游」链路，账号按给定 token 注入。
func newFixture(t *testing.T, up http.HandlerFunc, tokens ...string) *fixture {
	t.Helper()

	var mu sync.Mutex
	var hits []string
	upSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		hits = append(hits, tok)
		mu.Unlock()
		up(w, r)
	}))
	t.Cleanup(upSrv.Close)

	dir := t.TempDir()
	cfg := config.Default()
	cfg.APIKey = testKey
	cfg.Upstream.BaseURL = upSrv.URL
	cfg.Pool.Strategy = "round_robin"
	cfg.Logging.RequestArchiveEnabled = false
	// DataDir 必须隔离：视频任务库按 DataDir 落盘，共用会让用例互相污染。
	cfg.DataDir = dir
	holder := config.NewHolder(cfg)
	st, err := store.Open(filepath.Join(dir, "accounts.json"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	exp := time.Now().Add(6 * time.Hour).UnixMilli()
	for i, tok := range tokens {
		if err := st.Add(&store.Account{
			ID:          fmt.Sprintf("acct-%d", i),
			Label:       fmt.Sprintf("账号%d", i),
			AccessToken: tok,
			ExpiresAtMs: exp,
		}); err != nil {
			t.Fatalf("store.Add: %v", err)
		}
	}

	tr, err := usage.Open(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatalf("usage.Open: %v", err)
	}

	s := New(holder, st, pool.New(holder, st), oauth.New(holder), upstream.New(holder), tr, reqlog.New(50))
	front := httptest.NewServer(s.Handler())
	t.Cleanup(front.Close)

	return &fixture{
		srv:   s,
		front: front,
		upHits: func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), hits...)
		},
		upCalls: func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(hits)
		},
	}
}

func (f *fixture) post(t *testing.T, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.front.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, raw
}

// 坏 token 401 → 必须自动换到下一个账号，客户端只看到成功。
func TestFailoverOn401(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.Header.Get("Authorization"), "bad") {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":"invalid access token"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicMessageOK)
	}, "good", "bad") // round_robin 从 cands[1] 起选，坏号放第二位才能保证第一次就打到它

	resp, raw := f.post(t, "/v1/chat/completions",
		`{"model":"MiniMax-M3.1-Flash","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	choices, _ := out["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices 数量 = %d，期望 1", len(choices))
	}
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "pong" {
		t.Fatalf("content = %v，期望 pong", msg["content"])
	}
	hits := f.upHits()
	// 不假设选号顺序（round_robin 起点、weighted 随机都合法），只校验语义：
	// 两个账号各打一次，坏号在前，最终成功的是好号。
	if len(hits) != 2 {
		t.Fatalf("上游调用次数 = %d，期望 2（坏号失败后换号），序列 = %v", len(hits), hits)
	}
	if hits[len(hits)-1] != "good" {
		t.Fatalf("最后一次上游调用用了 %q，期望换到 good", hits[len(hits)-1])
	}
	sawBad := false
	for _, h := range hits {
		if h == "bad" {
			sawBad = true
		}
	}
	if !sawBad {
		t.Fatalf("坏号没被调用过，无法证明发生过换号：%v", hits)
	}
	// usage 要把缓存命中算进 prompt_tokens（OpenAI 语义）。
	usg, _ := out["usage"].(map[string]any)
	if got := usg["prompt_tokens"]; got != float64(15) {
		t.Fatalf("prompt_tokens = %v，期望 15（11+4 缓存）", got)
	}
	if got := usg["total_tokens"]; got != float64(18) {
		t.Fatalf("total_tokens = %v，期望 18", got)
	}
}

// 400 是请求本身的问题，换号也没用 —— 必须原样透出且只打一次上游。
func TestBadRequestNotRetried(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens too large"}}`)
	}, "a", "b")

	resp, raw := f.post(t, "/v1/chat/completions",
		`{"model":"MiniMax-M3.1-Flash","messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400，body=%s", resp.StatusCode, raw)
	}
	if n := f.upCalls(); n != 1 {
		t.Fatalf("上游调用次数 = %d，期望 1（不该换号）", n)
	}
	if !strings.Contains(string(raw), "max_tokens too large") {
		t.Fatalf("错误信息未透出: %s", raw)
	}
}

// 全部账号都 401 → 返回 401，且不无限重试。
func TestAllAccountsUnauthorized(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"invalid access token"}`)
	}, "a", "b")

	resp, raw := f.post(t, "/v1/chat/completions",
		`{"model":"MiniMax-M3.1-Flash","messages":[{"role":"user","content":"hi"}]}`)

	// 下游的 sk-mmx 已经验过，失效的是网关自己的上游账号 —— 必须是 502。
	// 回 401 会让客户端以为自己 key 错了，重配/换 key 都无救。
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 = %d，期望 502，body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "upstream_auth") {
		t.Fatalf("错误码应为 upstream_auth，body=%s", raw)
	}
	// param 是 OpenAI 错误对象的固定成员，缺失会让严格 SDK 反序列化报警。
	if !strings.Contains(string(raw), `"param"`) {
		t.Fatalf("错误对象缺 param 字段: %s", raw)
	}
	if n := f.upCalls(); n != 2 {
		t.Fatalf("上游调用次数 = %d，期望 2（每个账号各一次）", n)
	}
}

const upstreamSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"MiniMax-M3.1-Flash","usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想一下"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig-abc"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"北京\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":12}}

event: message_stop
data: {"type":"message_stop"}
`

// Anthropic SSE → OpenAI SSE：思考进 reasoning_content、工具调用增量拼装、结尾 usage。
func TestStreamConversion(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, upstreamSSE)
	}, "good")

	resp, raw := f.post(t, "/v1/chat/completions",
		`{"model":"MiniMax-M3.1-Flash","stream":true,"stream_options":{"include_usage":true},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}

	var reasoning, text, args, finish string
	var calls []map[string]any
	var usageChunk map[string]any
	done := false

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			done = true
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("分片不是 JSON: %v (%s)", err, payload)
		}
		if _, ok := chunk["usage"]; ok {
			usageChunk = chunk
		}
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			ch := c.(map[string]any)
			if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
				finish = fr
			}
			delta, _ := ch["delta"].(map[string]any)
			if v, ok := delta["reasoning_content"].(string); ok {
				reasoning += v
			}
			if v, ok := delta["content"].(string); ok {
				text += v
			}
			tcs, _ := delta["tool_calls"].([]any)
			for _, tc := range tcs {
				call := tc.(map[string]any)
				calls = append(calls, call)
				if fn, ok := call["function"].(map[string]any); ok {
					if v, ok := fn["arguments"].(string); ok {
						args += v
					}
				}
			}
		}
	}

	if !done {
		t.Fatal("缺少 [DONE] 结尾")
	}
	if reasoning != "先想一下" {
		t.Fatalf("reasoning_content = %q，期望 %q", reasoning, "先想一下")
	}
	if text != "你好" {
		t.Fatalf("content = %q，期望 %q", text, "你好")
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q，期望 tool_calls", finish)
	}
	if len(calls) == 0 {
		t.Fatal("没有收到 tool_calls 分片")
	}
	first := calls[0]
	if first["index"] != float64(0) {
		t.Fatalf("tool_calls index = %v，期望 0", first["index"])
	}
	if first["id"] != "toolu_1" {
		t.Fatalf("tool_call id = %v，期望 toolu_1", first["id"])
	}
	if args != `{"city":"北京"}` {
		t.Fatalf("拼装后的 arguments = %q", args)
	}
	if usageChunk == nil {
		t.Fatal("include_usage 时缺少 usage 分片")
	}
	usg := usageChunk["usage"].(map[string]any)
	if usg["completion_tokens"] != float64(12) {
		t.Fatalf("completion_tokens = %v，期望 12", usg["completion_tokens"])
	}
	if usg["prompt_tokens"] != float64(10) {
		t.Fatalf("prompt_tokens = %v，期望 10", usg["prompt_tokens"])
	}
}

// 上游私有信封字段要被清掉，但其它未知字段必须留着。
func TestStripEnvelope(t *testing.T) {
	in := `{"id":"msg_1","content":[],"base_resp":{"status_code":0},"x_new_field":123}`
	out := stripEnvelope([]byte(in))
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("输出不是 JSON: %v", err)
	}
	if _, ok := got["base_resp"]; ok {
		t.Fatal("base_resp 未被清除")
	}
	if got["x_new_field"] != float64(123) {
		t.Fatalf("未知字段被误删: %v", got)
	}
	// 没有 base_resp 时必须是原样字节（不做无意义的重排）。
	raw := []byte(`{"id":"msg_2"}`)
	if string(stripEnvelope(raw)) != string(raw) {
		t.Fatal("没有 base_resp 时不该改动响应体")
	}
	// 非 JSON 输入原样返回。
	bad := []byte(`not json`)
	if string(stripEnvelope(bad)) != string(bad) {
		t.Fatal("非 JSON 输入不该被改动")
	}
}

// /v1/messages 直通：只改写 model，其余字段原样透传。
func TestMessagesPassthrough(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var got map[string]any
		_ = json.Unmarshal(raw, &got)
		if got["model"] != "MiniMax-M3" {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":"model 未改写: %v"}`, got["model"])
			return
		}
		if _, ok := got["thinking"]; ok {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"M3.1 强制思考时不该传 thinking"}`)
			return
		}
		// system 里的 cache_control 必须保留。
		sys, _ := got["system"].([]any)
		if len(sys) == 0 {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":"system 丢了"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, anthropicMessageOK)
	}, "good")

	resp, raw := f.post(t, "/v1/messages", `{
		"model":"M3","max_tokens":64,
		"system":[{"type":"text","text":"你是助手","cache_control":{"type":"ephemeral"}}],
		"messages":[{"role":"user","content":"hi"}]}`)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["type"] != "message" {
		t.Fatalf("type = %v，期望 message", out["type"])
	}
}

// 未知路径必须回 JSON 404：曾经因为 "OPTIONS /" 是唯一能匹配 /v1/nope 的 pattern，
// Go 的 ServeMux 判成 method 不匹配，回了纯文本 405（客户端会以为路径存在但方法错了）。
func TestUnknownRouteJSON404(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("未知路径不该打到上游: %s", r.URL.Path)
	}, "tok1")

	call := func(method, path string) (*http.Response, []byte) {
		req, err := http.NewRequest(method, f.front.URL+path, nil)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, raw
	}

	resp, raw := call(http.MethodGet, "/v1/nope")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404，body=%s", resp.StatusCode, raw)
	}
	if !strings.Contains(string(raw), "unknown_route") {
		t.Fatalf("应回 unknown_route 信封，body=%s", raw)
	}

	// 浏览器预检：必须 204 且放行 DELETE（视频 DELETE 端点依赖它）。
	opt, _ := call(http.MethodOptions, "/v1/contents/generations/tasks/x")
	if opt.StatusCode != http.StatusNoContent {
		t.Fatalf("OPTIONS 状态码 = %d，期望 204", opt.StatusCode)
	}
	if got := opt.Header.Get("Access-Control-Allow-Methods"); !strings.Contains(got, "DELETE") {
		t.Fatalf("Allow-Methods = %q，缺 DELETE", got)
	}

	// 兜底的 "/" 不能吃掉更具体的路由。
	if h, _ := call(http.MethodGet, "/healthz"); h.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d，被兜底 handler 吃掉了", h.StatusCode)
	}
}
