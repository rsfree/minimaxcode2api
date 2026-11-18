package imageproto

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestParseWH 锁定 size 写法的兼容面。
//
// 背景：客户端/手写调用会写 4096×4096、1024*1024、1024 X 1024，
// 旧实现只按 'x' 切，这些写法一律解析失败并静默回落 1K。
func TestParseWH(t *testing.T) {
	cases := []struct {
		in      string
		w, h    int
		wantOK  bool
		comment string
	}{
		{"1024x1024", 1024, 1024, true, "OpenAI 官方写法"},
		{"4096×4096", 4096, 4096, true, "全角乘号（中文文档常见）"},
		{"1024*1024", 1024, 1024, true, "星号"},
		{"1024 X 1024", 1024, 1024, true, "大写 X + 空格"},
		{" 1536 x 1024 ", 1536, 1024, true, "首尾空格"},
		{"auto", 0, 0, false, "auto 不是尺寸"},
		{"abc", 0, 0, false, "完全无法解析"},
		{"1024", 0, 0, false, "缺高度"},
		{"0x1024", 0, 0, false, "宽度为 0"},
		{"-1024x1024", 0, 0, false, "负数"},
	}
	for _, c := range cases {
		w, h, ok := parseWH(c.in)
		if ok != c.wantOK || (ok && (w != c.w || h != c.h)) {
			t.Errorf("parseWH(%q) = %d,%d,%v；期望 %d,%d,%v（%s）",
				c.in, w, h, ok, c.w, c.h, c.wantOK, c.comment)
		}
	}
}

func TestShapeWithDefaultResolution(t *testing.T) {
	cases := []struct {
		size, quality, def string
		wantRatio, wantRes string
	}{
		// size 能解析 → size 优先于默认值
		{"4096×4096", "", "1K", "1:1", "4K"},
		{"1024x1024", "", "4K", "1:1", "1K"},
		{"1536x1024", "", "1K", "3:2", "2K"},
		// size 缺失 → 默认值生效
		{"", "", "4K", "1:1", "4K"},
		{"", "", "2K", "1:1", "2K"},
		{"", "", "auto", "1:1", ""},
		{"", "", "", "1:1", "1K"},
		// quality 优先于 size 与默认值
		{"4096x4096", "hd", "1K", "1:1", "2K"},
		{"1024x1024", "4k", "1K", "1:1", "4K"},
		// auto 不是尺寸，等同「不指定」：连比例也不写（方舟语义，交模型按 prompt 判断）
		{"auto", "", "2K", "", "2K"},
	}
	for _, c := range cases {
		r, res := Shape(c.size, c.quality, c.def)
		if r != c.wantRatio || res != c.wantRes {
			t.Errorf("Shape(%q,%q,def=%q) = %q,%q；期望 %q,%q",
				c.size, c.quality, c.def, r, res, c.wantRatio, c.wantRes)
		}
	}
}

func TestBuildRejectsUnparsableSize(t *testing.T) {
	req := &Request{Prompt: "x", Size: "not-a-size", N: 1}
	_, _, perr := req.Build("1K")
	if perr == nil {
		t.Fatal("无法解析的 size 应返回 400，实际放行")
	}
	if perr.Status != 400 || perr.Param != "size" {
		t.Fatalf("期望 400/size，实际 %d/%s (%s)", perr.Status, perr.Param, perr.Message)
	}
	if !strings.Contains(perr.Message, "4096") {
		t.Errorf("错误消息应给出写法示例，实际：%s", perr.Message)
	}
}

func TestBuildSizeAutoPasses(t *testing.T) {
	// OpenAI 的 size="auto" 表示交服务端决定，不能当非法值拒掉。
	req := &Request{Prompt: "x", Size: "auto", N: 1}
	payload, _, perr := req.Build("2K")
	if perr != nil {
		t.Fatalf("size=auto 不该报错：%s", perr.Message)
	}
	item := payload["requests"].([]map[string]any)[0]
	if got := item["resolution"]; got != "2K" {
		t.Errorf("size=auto 应套用默认档位 2K，实际 %v", got)
	}
}

func TestBuildDefaultResolutionAutoOmitsKey(t *testing.T) {
	req := &Request{Prompt: "x", N: 1}
	payload, _, perr := req.Build("auto")
	if perr != nil {
		t.Fatalf("Build 失败：%s", perr.Message)
	}
	item := payload["requests"].([]map[string]any)[0]
	if _, exists := item["resolution"]; exists {
		t.Errorf("默认档位 auto 时应整个省略 resolution 键，实际写了 %v", item["resolution"])
	}
}

func TestDecodeSizeVariantsFlowThrough(t *testing.T) {
	// 端到端：Decode → Build，确认 4096×4096 真的落到 4K/1:1。
	raw, _ := json.Marshal(map[string]any{"prompt": "x", "size": "4096×4096"})
	req, perr := Decode(raw)
	if perr != nil {
		t.Fatalf("Decode 失败：%s", perr.Message)
	}
	payload, _, perr := req.Build("1K")
	if perr != nil {
		t.Fatalf("Build 失败：%s", perr.Message)
	}
	item := payload["requests"].([]map[string]any)[0]
	if item["resolution"] != "4K" {
		t.Errorf("4096×4096 应映射到 4K，实际 %v", item["resolution"])
	}
	if item["aspect_ratio"] != "1:1" {
		t.Errorf("4096×4096 应映射到 1:1，实际 %v", item["aspect_ratio"])
	}
}

// TestSizeTierForm 锁定方舟协议的档位写法：size 可以直接是 1K/1.5K/2K/3K/4K/auto。
//
// 之前只认 WxH，方舟推荐写法 size="2K" 会被 400 —— 而 2K 恰恰是方舟文档的首选。
func TestSizeTierForm(t *testing.T) {
	cases := []struct {
		size    string
		def     string
		wantRes string
	}{
		{"1K", "4K", "1K"},   // 显式档位压过默认值
		{"2K", "1K", "2K"},
		{"4K", "1K", "4K"},
		{"1.5K", "1K", "2K"}, // 1536 长边 → 2K
		{"3K", "1K", "4K"},   // 3072 长边 → 4K
		{"auto", "2K", "2K"}, // auto = 不指定档位，回落默认
	}
	for _, c := range cases {
		ratio, res := Shape(c.size, "", c.def)
		if res != c.wantRes {
			t.Errorf("Shape(size=%q, def=%q) 档位 = %q，期望 %q", c.size, c.def, res, c.wantRes)
		}
		if ratio != "" {
			t.Errorf("档位模式下不应指定比例（方舟让模型按 prompt 判断），实际 %q", ratio)
		}
	}
}

func TestBuildAcceptsTierSizeAndOmitsRatio(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"prompt": "x", "size": "2K"})
	req, perr := Decode(raw)
	if perr != nil {
		t.Fatalf("Decode 失败：%s", perr.Message)
	}
	payload, _, perr := req.Build("1K")
	if perr != nil {
		t.Fatalf("size=2K 不该报错：%s", perr.Message)
	}
	item := payload["requests"].([]map[string]any)[0]
	if item["resolution"] != "2K" {
		t.Errorf("档位应为 2K，实际 %v", item["resolution"])
	}
	if _, exists := item["aspect_ratio"]; exists {
		t.Errorf("档位模式不该下发 aspect_ratio，实际 %v", item["aspect_ratio"])
	}
}

func TestBuildDropsArkOnlyKeys(t *testing.T) {
	// 方舟客户端必传 watermark=false；上游无此能力，应吞掉并点名。
	raw, _ := json.Marshal(map[string]any{
		"prompt": "x", "size": "2K", "watermark": false,
		"sequential_image_generation": "auto",
	})
	req, perr := Decode(raw)
	if perr != nil {
		t.Fatalf("Decode 失败：%s", perr.Message)
	}
	payload, warnings, perr := req.Build("1K")
	if perr != nil {
		t.Fatalf("Build 失败：%s", perr.Message)
	}
	if _, exists := payload["watermark"]; exists {
		t.Error("watermark 不该透传给上游")
	}
	if _, exists := payload["sequential_image_generation"]; exists {
		t.Error("sequential_image_generation 不该透传给上游")
	}
	joined := strings.Join(warnings, "|")
	if !strings.Contains(joined, "watermark") || !strings.Contains(joined, "sequential_image_generation") {
		t.Errorf("两个字段都应出现在 warnings 里，实际：%v", warnings)
	}
}
