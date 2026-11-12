package imageproto

import (
	"testing"
)

func decode(t *testing.T, body string) (*Request, *ProtocolError) {
	t.Helper()
	return Decode([]byte(body))
}

// 只有四类被拒；其余一律归一化。判定顺序 n → response_format → image → mask → prompt。
func TestDecodeRejects(t *testing.T) {
	cases := []struct {
		name, body, param string
	}{
		{"体不是对象", `[1,2]`, ""},
		{"image 空串", `{"prompt":"x","image":""}`, "image"},
		{"image 元素非字符串", `{"prompt":"x","image":[1]}`, "image[0]"},
		{"image 元素空串", `{"prompt":"x","image":["ok",""]}`, "image[1]"},
		{"mask 非字符串", `{"prompt":"x","image":"a.png","mask":5}`, "mask"},
		{"mask 无 image", `{"prompt":"x","mask":"m.png"}`, "image"},
		{"无图无 prompt", `{}`, "prompt"},
	}
	for _, c := range cases {
		_, perr := decode(t, c.body)
		if perr == nil || perr.Status != 400 {
			t.Fatalf("%s: 期望 400，实际 %+v", c.name, perr)
		}
		if perr.Param != c.param {
			t.Fatalf("%s: param = %q，期望 %q", c.name, perr.Param, c.param)
		}
	}
}

// n 的非法值必须回落 1，不报错。
func TestNNormalization(t *testing.T) {
	cases := map[string]int{
		`{"prompt":"x"}`: 1, `{"prompt":"x","n":3}`: 3,
		`{"prompt":"x","n":0}`: 1, `{"prompt":"x","n":-3}`: 1,
		`{"prompt":"x","n":2.5}`: 1, `{"prompt":"x","n":2.0}`: 1,
		`{"prompt":"x","n":"3"}`: 1, `{"prompt":"x","n":true}`: 1,
		`{"prompt":"x","n":null}`: 1,
	}
	for body, want := range cases {
		req, perr := decode(t, body)
		if perr != nil {
			t.Fatalf("%s: %+v", body, perr)
		}
		if req.N != want {
			t.Fatalf("%s: n = %d，期望 %d", body, req.N, want)
		}
	}
}

// response_format 只认两个值，其余删键（不干预上游形态）。
func TestResponseFormatNormalization(t *testing.T) {
	for _, body := range []string{
		`{"prompt":"x","response_format":""}`,
		`{"prompt":"x","response_format":"png"}`,
		`{"prompt":"x","response_format":null}`,
		`{"prompt":"x","response_format":{"type":"url"}}`,
	} {
		req, perr := decode(t, body)
		if perr != nil || req.ResponseFormat != "" {
			t.Fatalf("%s: response_format = %q，期望删键", body, req.ResponseFormat)
		}
	}
	req, _ := decode(t, `{"prompt":"x","response_format":"b64_json"}`)
	if req.ResponseFormat != "b64_json" {
		t.Fatalf("b64_json 未被识别")
	}
}

// image 的三种写法：string / string[] / null。
func TestImageFieldShapes(t *testing.T) {
	req, perr := decode(t, `{"prompt":"x","image":"https://a/1.png"}`)
	if perr != nil || len(req.Image) != 1 {
		t.Fatalf("单串写法: %+v", req)
	}
	req, perr = decode(t, `{"prompt":"x","image":["https://a/1.png","https://a/2.png"]}`)
	if perr != nil || len(req.Image) != 2 {
		t.Fatalf("数组写法: %+v", req)
	}
	req, perr = decode(t, `{"prompt":"x","image":null}`)
	if perr != nil || len(req.Image) != 0 {
		t.Fatalf("null 应视为文生图: %+v", req)
	}
	req, perr = decode(t, `{"prompt":"x","image":[]}`)
	if perr != nil || len(req.Image) != 0 {
		t.Fatalf("[] 应视为文生图: %+v", req)
	}
}

// 上游强制要求 prompt：image-only 是协议合法但本渠道做不到，必须显式 400。
func TestBuildImageOnlyRejected(t *testing.T) {
	req, perr := decode(t, `{"image":"https://a/1.png"}`)
	if perr != nil {
		t.Fatalf("canonical 层应接受 image-only: %+v", perr)
	}
	_, _, perr = req.Build()
	if perr == nil || perr.Code != "unsupported_parameter" || perr.Param != "prompt" {
		t.Fatalf("image-only 应报 unsupported_parameter/prompt，实际 %+v", perr)
	}
}

// mask / 超量参考图 / 样式都要显式处理，不静默丢。
func TestBuildRejectsAndWarnings(t *testing.T) {
	req, _ := decode(t, `{"prompt":"x","image":"a.png","mask":"m.png"}`)
	if _, _, perr := req.Build(); perr == nil || perr.Param != "mask" {
		t.Fatalf("mask 应 400，实际 %+v", perr)
	}

	req, _ = decode(t, `{"prompt":"x","image":["1","2","3","4","5"]}`)
	if _, _, perr := req.Build(); perr == nil || perr.Param != "image" {
		t.Fatalf("5 张参考图应 400，实际 %+v", perr)
	}

	req, _ = decode(t, `{"prompt":"x","n":12,"style":"vivid"}`)
	payload, warns, perr := req.Build()
	if perr != nil {
		t.Fatalf("Build: %+v", perr)
	}
	items := payload["requests"].([]map[string]any)
	if len(items) != 10 {
		t.Fatalf("n=12 应钳到 10，实际 %d", len(items))
	}
	if len(warns) < 2 {
		t.Fatalf("应至少两条 warning（n 钳位 + style），实际 %v", warns)
	}
}

// size/quality → 档位与比例。档位是像素预算，不是自由 WxH。
func TestShape(t *testing.T) {
	cases := []struct{ size, quality, ratio, res string }{
		{"1024x1024", "", "1:1", "1K"},
		{"1792x1024", "", "16:9", "2K"},
		{"1280x720", "", "16:9", "1K"},
		{"1024x1792", "", "9:16", "2K"},
		{"4096x4096", "", "1:1", "4K"},
		{"", "hd", "1:1", "2K"},
		{"4096x4096", "hd", "1:1", "2K"},
		{"", "", "1:1", "1K"},
	}
	for _, c := range cases {
		ratio, res := Shape(c.size, c.quality)
		if ratio != c.ratio || res != c.res {
			t.Fatalf("Shape(%q,%q) = %s/%s，期望 %s/%s", c.size, c.quality, ratio, res, c.ratio, c.res)
		}
	}
}

// vendor extras 必须透传（additionalProperties:true 是承重的）。
func TestExtrasPassthrough(t *testing.T) {
	req, _ := decode(t, `{"prompt":"x","vendor_flag":"on","nested":{"a":1}}`)
	payload, _, perr := req.Build()
	if perr != nil {
		t.Fatalf("Build: %+v", perr)
	}
	if payload["vendor_flag"] != "on" {
		t.Fatalf("extras 未透传: %v", payload)
	}
	if _, ok := payload["nested"].(map[string]any); !ok {
		t.Fatalf("嵌套 extras 未透传: %v", payload["nested"])
	}
}

// 上游原生字段必须进 items —— 之前它们掉进 Extras 被透传到顶层，上游直接忽略，
// 表现为"请求 21:9 却回 1:1"（真实回归）。
func TestAspectRatioAndResolutionReachItems(t *testing.T) {
	req, perr := decode(t, `{"prompt":"x","aspect_ratio":"21:9","resolution":"4K"}`)
	if perr != nil {
		t.Fatalf("Decode: %+v", perr)
	}
	payload, _, perr := req.Build()
	if perr != nil {
		t.Fatalf("Build: %+v", perr)
	}
	if payload["aspect_ratio"] != nil || payload["resolution"] != nil {
		t.Fatalf("原生字段不能留在顶层（上游不读）: %v", payload)
	}
	item := payload["requests"].([]map[string]any)[0]
	if item["aspect_ratio"] != "21:9" || item["resolution"] != "4K" {
		t.Fatalf("槽位未带上原生字段: %v", item)
	}
}

// 比例吸附：任意 W:H 都要落到实测支持的比例之一，而不是让上游运行时失败。
func TestSnapRatio(t *testing.T) {
	cases := map[string]string{
		"21:9": "21:9", "16:9": "16:9", "4:3": "4:3", "1:1": "1:1",
		"3:4": "3:4", "9:16": "9:16",
		"2:1": "16:9", "7:3": "21:9", "16/9": "16:9",
		// 实测原生支持，直通不吸附（2026-11-12 直连上游验证）
		"3:2": "3:2", "5:4": "5:4", "4:5": "4:5", "2:3": "2:3",
		// 上游不支持，降级到最近邻
		"3:1": "21:9", "1:3": "9:16",
		"1.85:1": "16:9",
	}
	for in, want := range cases {
		got, ok := SnapRatio(in)
		if !ok || got != want {
			t.Fatalf("SnapRatio(%q) = %q,%v，期望 %q", in, got, ok, want)
		}
	}
	if _, ok := SnapRatio("abc"); ok {
		t.Fatalf("abc 不该可解析")
	}
}

// 档位吸附：常见 p 写法与裸像素都要落到 1K/2K/4K。
func TestSnapResolution(t *testing.T) {
	cases := map[string]string{
		"1K": "1K", "2K": "2K", "4K": "4K", "2k": "2K",
		"480p": "1K", "720p": "1K", "1080p": "1K",
		"1440p": "2K", "2160p": "4K", "8K": "4K",
		"1024": "1K", "2048": "2K", "4096": "4K",
	}
	for in, want := range cases {
		got, ok := SnapResolution(in)
		if !ok || got != want {
			t.Fatalf("SnapResolution(%q) = %q,%v，期望 %q", in, got, ok, want)
		}
	}
	if _, ok := SnapResolution("abc"); ok {
		t.Fatalf("abc 不该可解析")
	}
}

// 吸附后要写进槽位并留 warning；完全无法解析才 400。
func TestAspectRatioSnapped(t *testing.T) {
	req, _ := decode(t, `{"prompt":"x","aspect_ratio":"2:1","resolution":"8K"}`)
	payload, warns, perr := req.Build()
	if perr != nil {
		t.Fatalf("Build: %+v", perr)
	}
	item := payload["requests"].([]map[string]any)[0]
	if item["aspect_ratio"] != "16:9" || item["resolution"] != "4K" {
		t.Fatalf("未吸附到合法值: %v", item)
	}
	if len(warns) != 2 {
		t.Fatalf("两个字段都该有 warning，实际 %v", warns)
	}

	req, _ = decode(t, `{"prompt":"x","aspect_ratio":"abc"}`)
	if _, _, perr := req.Build(); perr == nil || perr.Status != 400 {
		t.Fatalf("无法解析的比例应 400，实际 %+v", perr)
	}
}

// 同一个数字必须只有一个答案：size 推导与 resolution 吸附不能分叉。
// 曾经 size=1536x1024 判 2K，而 resolution="1536" 判 1K（两套阈值表）。
func TestSizeAndResolutionAgree(t *testing.T) {
	for _, longest := range []string{
		"512", "1024", "1280", "1376", "1400", "1401", "1440", "1536",
		"2048", "2752", "2800", "2801", "3840", "4096", "5504", "8192",
	} {
		fromSize := resolutionFromSize(longest + "x100")
		fromRes, ok := SnapResolution(longest)
		if !ok {
			t.Fatalf("SnapResolution(%q) 无法解析", longest)
		}
		if fromSize != fromRes {
			t.Fatalf("长边 %s：size 推导=%s，resolution 吸附=%s —— 必须一致",
				longest, fromSize, fromRes)
		}
	}
}

// size 的比例也走同一份比例表（ratioFromSize 复用 SnapRatio）。
func TestRatioFromSizeSnaps(t *testing.T) {
	cases := map[string]string{
		"1024x1024": "1:1", "1920x1080": "16:9", "3840x1600": "21:9",
		// 1024x1536 是精确 2:3，扩表后不再被吸附成 3:4（1K 档会拿到 848×1264）
		"1024x1536": "2:3", "1080x1920": "9:16", "1200x896": "4:3",
		"1500x1000": "3:2", "1280x1024": "5:4", "1000x1250": "4:5",
		"": "",
	}
	for size, want := range cases {
		if got := ratioFromSize(size); got != want {
			t.Fatalf("ratioFromSize(%q) = %q，期望 %q", size, got, want)
		}
	}
}

// Auto 是 MiniMax 客户端的默认档位（截图里的「比例 Auto / 4K」）。
// 语义是「不指定，交给上游按 prompt 决定」：必须解析成功，且槽位里
// 完全不要出现 aspect_ratio / resolution 这两个键 —— 写空串仍然算指定。
func TestBuildAutoOmitsShapeKeys(t *testing.T) {
	req, perr := decode(t, `{"prompt":"海边灯塔","aspect_ratio":"auto","resolution":"Auto"}`)
	if perr != nil {
		t.Fatalf("auto 不该被拒: %+v", perr)
	}
	payload, warns, perr := req.Build()
	if perr != nil {
		t.Fatalf("Build: %+v", perr)
	}
	if len(warns) == 0 {
		t.Fatal("auto 应给出 warnings，说明由上游决定")
	}
	items := payload["requests"].([]map[string]any)
	if len(items) != 1 {
		t.Fatalf("槽位数 = %d，期望 1", len(items))
	}
	if _, ok := items[0]["aspect_ratio"]; ok {
		t.Fatalf("aspect_ratio 不该出现: %+v", items[0])
	}
	if _, ok := items[0]["resolution"]; ok {
		t.Fatalf("resolution 不该出现: %+v", items[0])
	}
	if items[0]["prompt"] != "海边灯塔" || items[0]["output_file"] == "" {
		t.Fatalf("其余字段应保留: %+v", items[0])
	}
}
