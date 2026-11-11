package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/videoproto"
	"github.com/linguo2625469/minimaxcode2api/internal/videotask"
)

const videoTaskPath = "/v1/contents/generations/tasks"

// 建任务：创建响应**只能有 id**（这是协议里最容易"顺手加字段"的违约点）。
func TestVideoCreateReturnsOnlyID(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mcp/submit_video_generation" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"task_id":"cgt-1","message":"submitted"}`)
	}, "tok1")

	resp, raw := f.post(t, videoTaskPath,
		`{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"海边灯塔，黄昏"}],"duration":6,"resolution":"720p","ratio":"16:9"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}

	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if len(out) != 1 || out["id"] != "cgt-1" {
		t.Fatalf("创建响应必须只有 id，实际 = %v", out)
	}
	if loc := resp.Header.Get("Location"); !strings.HasSuffix(loc, "/cgt-1") {
		t.Fatalf("Location = %q", loc)
	}

	// 折成上游方言：prompt 单串 + 768P + 6 秒。
	if got["prompt"] != "海边灯塔，黄昏" {
		t.Fatalf("上游 prompt = %v", got["prompt"])
	}
	if got["resolution"] != "768P" {
		t.Fatalf("720p 应映射为 768P，实际 %v", got["resolution"])
	}
	if got["duration"] != float64(6) {
		t.Fatalf("duration = %v", got["duration"])
	}
	if got["model"] != "MiniMax-Hailuo-2.3" {
		t.Fatalf("model = %v", got["model"])
	}
}

// 校验顺序必须是 content → model → 场景互斥 → 参数取值域。
func TestVideoCreateValidation(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("校验失败的请求不该打到上游")
	}, "tok1")

	cases := []struct {
		name  string
		body  string
		param string
	}{
		{"空 content", `{"model":"MiniMax-H3","content":[]}`, "content"},
		{"缺 model", `{"content":[{"type":"text","text":"x"}]}`, "model"},
		{"场景混用", `{"model":"MiniMax-H3","content":[{"type":"text","text":"x"},
			{"type":"image_url","image_url":{"url":"https://a/1.png"},"role":"first_frame"},
			{"type":"image_url","image_url":{"url":"https://a/2.png"},"role":"reference_image"}]}`, "content"},
		{"坏 resolution", `{"model":"MiniMax-H3","content":[{"type":"text","text":"x"}],"resolution":"999p"}`, "resolution"},
		{"坏 ratio", `{"model":"MiniMax-H3","content":[{"type":"text","text":"x"}],"ratio":"2:1"}`, "ratio"},
		{"frames 不支持", `{"model":"MiniMax-H3","content":[{"type":"text","text":"x"}],"frames":60}`, "frames"},
		{"duration 越界", `{"model":"MiniMax-H3","content":[{"type":"text","text":"x"}],"duration":30}`, "duration"},
		{"Hailuo 只收 6/10", `{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"x"}],"duration":8}`, "duration"},
		{"未知 model", `{"model":"seedance-2.5","content":[{"type":"text","text":"x"}]}`, "model"},
	}
	for _, c := range cases {
		resp, raw := f.post(t, videoTaskPath, c.body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: 状态码 = %d，期望 400，body=%s", c.name, resp.StatusCode, raw)
		}
		var out struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("%s: 解析错误体: %v", c.name, err)
		}
		// 四键信封：message/type/param/code，一个不多一个不少。
		if len(out.Error) != 4 {
			t.Fatalf("%s: 错误信封必须是 4 个键，实际 %v", c.name, out.Error)
		}
		if out.Error["param"] != c.param {
			t.Fatalf("%s: param = %v，期望 %s", c.name, out.Error["param"], c.param)
		}
	}
}

// 参数双载体：prompt 后缀必须被剥离并回填，且绝不能同时下发两套。
func TestVideoPromptSuffixNormalized(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"task_id":"cgt-2"}`)
	}, "tok1")

	resp, raw := f.post(t, videoTaskPath, `{"model":"MiniMax-H3",
		"content":[{"type":"text","text":"小猫打哈欠 --rs 720p --rt 16:9 --dur 6 --wm true"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	if got["prompt"] != "小猫打呵欠" && got["prompt"] != "小猫打哈欠" {
		t.Fatalf("prompt 里的后缀没被剥离: %q", got["prompt"])
	}
	if strings.Contains(got["prompt"].(string), "--") {
		t.Fatalf("prompt 残留参数后缀: %q", got["prompt"])
	}
	if got["resolution"] != "768P" || got["ratio"] != "16:9" || got["duration"] != float64(6) {
		t.Fatalf("后缀未回填: res=%v ratio=%v dur=%v", got["resolution"], got["ratio"], got["duration"])
	}
	if got["aigc_watermark"] != true {
		t.Fatalf("--wm true 未映射到 aigc_watermark: %v", got["aigc_watermark"])
	}
}

// body 字段与后缀冲突时，强校验优先，并且只发一套。
func TestVideoSuffixConflictBodyWins(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"task_id":"cgt-3"}`)
	}, "tok1")

	resp, raw := f.post(t, videoTaskPath, `{"model":"MiniMax-H3","resolution":"1080p",
		"content":[{"type":"text","text":"夜景 --rs 720p"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	// H3 的 1080p 换算成 2K（body 优先，后缀的 720p 应被忽略）。
	if got["resolution"] != "2K" {
		t.Fatalf("resolution = %v，期望 body 的 1080p 生效并换算成 2K", got["resolution"])
	}

	rec, _ := f.srv.VideoTasks.Get("cgt-3")
	if rec == nil || len(rec.Warnings) == 0 {
		t.Fatalf("冲突必须留 warning，实际 %+v", rec)
	}
}

// 402 只打一次上游，且保留 insufficient_quota（运维要能区分余额与参数错）。
func TestVideoCreateBillingOnce(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":402,"message":"this task costs 280 credits"}`)
	}, "tok1")

	resp, raw := f.post(t, videoTaskPath,
		`{"model":"MiniMax-Hailuo-2.3","content":[{"type":"text","text":"x"}],"duration":6}`)
	if resp.StatusCode != http.StatusPaymentRequired {
		t.Fatalf("状态码 = %d，期望 402，body=%s", resp.StatusCode, raw)
	}
	if n := f.upCalls(); n != 1 {
		t.Fatalf("上游调用 %d 次，计费错误不该重试", n)
	}
	if !strings.Contains(string(raw), "insufficient_quota") {
		t.Fatalf("没保留 insufficient_quota: %s", raw)
	}
}

// 查询：canonical 任务对象。重点验证「succeeded 但没有 url」不能提前报成功。
func TestVideoGetCanonicalObject(t *testing.T) {
	state := 0
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch state {
		case 0: // 已就绪但还没准备好下发
			io.WriteString(w, `{"code":503,"status":"succeeded","message":"the video is ready but could not be prepared for delivery"}`)
		default:
			io.WriteString(w, `{"code":0,"status":"succeeded","video_url":"https://cdn.test/v.mp4"}`)
		}
	}, "tok1")

	dur := 6
	f.srv.VideoTasks.Put(&videotask.Record{
		ID: "cgt-9", ClientModel: "MiniMax-Hailuo-2.3", UpstreamModel: videoproto.UpstreamHailuo,
		Status: videoproto.StatusQueued, CreatedAt: 1700000000, UpdatedAt: 1700000000,
		Params: videotask.Params{Duration: &dur, Resolution: "720p", Ratio: "16:9"},
	})

	resp, raw := f.get(t, videoTaskPath+"/cgt-9")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d body=%s", resp.StatusCode, raw)
	}
	var q map[string]any
	_ = json.Unmarshal(raw, &q)
	if q["status"] != "running" {
		t.Fatalf("status = %v，待下发时应报 running", q["status"])
	}
	content, _ := q["content"].(map[string]any)
	if content == nil || content["video_url"] != nil {
		t.Fatalf("未成功时 video_url 必须为 null，实际 %v", q["content"])
	}
	if q["error"] != nil {
		t.Fatalf("非失败态的 error 必须为 null，实际 %v", q["error"])
	}

	state = 1
	resp, raw = f.get(t, videoTaskPath+"/cgt-9")
	_ = json.Unmarshal(raw, &q)
	if q["status"] != "succeeded" {
		t.Fatalf("status = %v，期望 succeeded，body=%s", q["status"], raw)
	}
	content, _ = q["content"].(map[string]any)
	if content["video_url"] != "https://cdn.test/v.mp4" {
		t.Fatalf("video_url 未回填: %v", q["content"])
	}
	if q["model"] != "MiniMax-Hailuo-2.3" {
		t.Fatalf("model 必须回显客户端原名，实际 %v", q["model"])
	}
	// 参数回显不能丢（计费对账依据）。
	if q["duration"] != float64(6) || q["resolution"] != "720p" || q["ratio"] != "16:9" {
		t.Fatalf("参数回显缺失: %v", q)
	}
}

// DELETE：非终态 501（上游不支持取消），终态删记录。
func TestVideoDeleteSemantics(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("DELETE 不该打上游")
	}, "tok1")

	f.srv.VideoTasks.Put(&videotask.Record{ID: "run-1", Status: videoproto.StatusRunning, CreatedAt: 1})
	resp, raw := f.do(t, http.MethodDelete, videoTaskPath+"/run-1", "")
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("非终态 DELETE 状态码 = %d，期望 501，body=%s", resp.StatusCode, raw)
	}

	f.srv.VideoTasks.Put(&videotask.Record{ID: "done-1", Status: videoproto.StatusSucceeded, CreatedAt: 1})
	resp, raw = f.do(t, http.MethodDelete, videoTaskPath+"/done-1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("终态 DELETE 状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if _, ok := f.srv.VideoTasks.Get("done-1"); ok {
		t.Fatalf("终态 DELETE 后记录应被删除")
	}
}

// 列表：真实数据 + 倒序。
func TestVideoListRealData(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {}, "tok1")
	f.srv.VideoTasks.Put(&videotask.Record{ID: "a", Status: videoproto.StatusQueued, CreatedAt: 100})
	f.srv.VideoTasks.Put(&videotask.Record{ID: "b", Status: videoproto.StatusSucceeded, CreatedAt: 200, VideoURL: "u"})

	resp, raw := f.get(t, videoTaskPath)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("列表 = %+v", out)
	}
	if out.Items[0]["id"] != "b" {
		t.Fatalf("列表应按 created_at 倒序，首项 = %v", out.Items[0]["id"])
	}
}

// /api/v3 前缀等价（Ark SDK 改 base_url 即可用）。
func TestVideoArkPrefixAlias(t *testing.T) {
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"task_id":"cgt-ark"}`)
	}, "tok1")
	resp, raw := f.post(t, "/api/v3/contents/generations/tasks",
		`{"model":"MiniMax-H3","content":[{"type":"text","text":"x"}],"duration":4}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	if _, ok := f.srv.VideoTasks.Get("cgt-ark"); !ok {
		t.Fatalf("任务没落库")
	}
}

// vendor extras 必须透传（schema 的 additionalProperties:true 是承重的）。
func TestVideoVendorExtraPassthrough(t *testing.T) {
	var got map[string]any
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"task_id":"cgt-4"}`)
	}, "tok1")

	f.post(t, videoTaskPath, `{"model":"MiniMax-H3","content":[{"type":"text","text":"x"}],
		"duration":4,"vendor_flag":"on","nested":{"a":1}}`)
	if got["vendor_flag"] != "on" {
		t.Fatalf("未知顶层字段没透传: %v", got)
	}
}

// 未知状态必须收敛为非终态，不能当成功。
func TestMapStatusUnknown(t *testing.T) {
	got, un := videoproto.MapStatus("weird_state")
	if got != videoproto.StatusRunning || !un {
		t.Fatalf("MapStatus(weird_state) = %q,%v", got, un)
	}
}

// do 发任意方法（fixture 只内置了 post/get）。
func (f *fixture) do(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, f.front.URL+path, rd)
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

var _ = httptest.NewServer

// 同步折叠：预算内等到终态 → 200 + 完整任务对象（同一个序列化器）。
func TestVideoWaitReturnsTerminal(t *testing.T) {
	oldStart, oldMax := videoPollStart, videoPollMax
	videoPollStart, videoPollMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { videoPollStart, videoPollMax = oldStart, oldMax })

	var mu sync.Mutex
	polls := 0
	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		polls++
		n := polls
		mu.Unlock()
		if n < 2 {
			io.WriteString(w, `{"code":0,"status":"running"}`)
			return
		}
		io.WriteString(w, `{"code":0,"status":"succeeded","video_url":"https://cdn.test/v.mp4"}`)
	}, "tok1")

	f.srv.VideoTasks.Put(&videotask.Record{
		ID: "w-1", ClientModel: "MiniMax-Hailuo-2.3", UpstreamModel: videoproto.UpstreamHailuo,
		Status: videoproto.StatusQueued, CreatedAt: 1, UpdatedAt: 1,
	})

	resp, raw := f.get(t, videoTaskPath+"/w-1?wait=1&wait_timeout=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["status"] != "succeeded" {
		t.Fatalf("status = %v，期望 succeeded", out["status"])
	}
	content, _ := out["content"].(map[string]any)
	if content["video_url"] != "https://cdn.test/v.mp4" {
		t.Fatalf("video_url = %v", content["video_url"])
	}
	if len(out) == 2 {
		t.Fatalf("终态应回完整任务对象，不是 {id,status}: %s", raw)
	}
}

// 预算耗尽：202 + {id,status} + Location，不取消任务。
func TestVideoWaitDegradesTo202(t *testing.T) {
	oldStart, oldMax := videoPollStart, videoPollMax
	videoPollStart, videoPollMax = 10*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { videoPollStart, videoPollMax = oldStart, oldMax })

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"code":0,"status":"running"}`)
	}, "tok1")

	f.srv.VideoTasks.Put(&videotask.Record{
		ID: "w-2", ClientModel: "MiniMax-H3", UpstreamModel: videoproto.UpstreamH3,
		Status: videoproto.StatusRunning, CreatedAt: 1, UpdatedAt: 1,
	})

	resp, raw := f.get(t, videoTaskPath+"/w-2?wait=1&wait_timeout=1")
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("状态码 = %d，期望 202，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if len(out) != 2 || out["id"] != "w-2" || out["status"] != "running" {
		t.Fatalf("降级响应必须是 {id,status}，实际 %v", out)
	}
	if loc := resp.Header.Get("Location"); loc != videoTaskPath+"/w-2" {
		t.Fatalf("Location = %q", loc)
	}
	// 降级不等于取消：记录还在，状态仍是 running。
	if rec, ok := f.srv.VideoTasks.Get("w-2"); !ok || rec.Status != "running" {
		t.Fatalf("降级后任务记录被改动: %+v", rec)
	}
}

// 创建即折叠：POST ?wait=1 成功时回完整对象；不等时只回 id。
func TestVideoCreateWithWait(t *testing.T) {
	oldStart, oldMax := videoPollStart, videoPollMax
	videoPollStart, videoPollMax = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { videoPollStart, videoPollMax = oldStart, oldMax })

	f := newFixture(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/mcp/submit_video_generation":
			io.WriteString(w, `{"code":0,"task_id":"cgt-wait"}`)
		case "/mcp/query_video_generation":
			io.WriteString(w, `{"code":0,"status":"succeeded","video_url":"https://cdn.test/v.mp4"}`)
		default:
			http.NotFound(w, r)
		}
	}, "tok1")

	resp, raw := f.post(t, videoTaskPath+"?wait=1&wait_timeout=5",
		`{"model":"MiniMax-H3","content":[{"type":"text","text":"x"}],"duration":4}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["status"] != "succeeded" || out["id"] != "cgt-wait" {
		t.Fatalf("折叠失败: %s", raw)
	}
	if _, ok := out["content"]; !ok {
		t.Fatalf("折叠成功必须回完整任务对象: %s", raw)
	}
}

// 参数解析：默认 60 秒，钳位 [1,300]，wait=0/false 视为不等待。
func TestWaitBudgetParsing(t *testing.T) {
	mk := func(q string) (bool, time.Duration) {
		r := httptest.NewRequest(http.MethodGet, "/x"+q, nil)
		return waitBudget(r)
	}
	if on, _ := mk(""); on {
		t.Fatalf("没有 wait 参数时不该等待")
	}
	if on, _ := mk("?wait=0"); on {
		t.Fatalf("wait=0 不该等待")
	}
	if on, _ := mk("?wait=false"); on {
		t.Fatalf("wait=false 不该等待")
	}
	if on, d := mk("?wait=1"); !on || d != 60*time.Second {
		t.Fatalf("默认预算 = %v", d)
	}
	if _, d := mk("?wait=1&wait_timeout=9999"); d != 300*time.Second {
		t.Fatalf("上限应钳到 300s，实际 %v", d)
	}
	if _, d := mk("?wait=1&wait_timeout=-5"); d != time.Second {
		t.Fatalf("下限应钳到 1s，实际 %v", d)
	}
}
