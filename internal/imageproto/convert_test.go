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
		ratio, res := Shape(c.size, c.quality, "")
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
