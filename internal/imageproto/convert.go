// Package imageproto 实现 OpenAI 图像协议的 canonical 契约。
//
// 四个入口（generations / edits / chat / responses）最终收敛成同一份 canonical
// 请求，再由这里折成上游方言（MiniMax image_synthesize 的槽位制）。
//
// 本包只做纯函数：Decode（含归一化）→ Validate → Build。网络、下载、转存都在
// server 层，保证入口折叠逻辑可单测。
//
// 关键口径（来自 openai-image-adapter）：
//   - 只有四类请求被拒：体不是对象 / image·mask 为空串或非字符串 / 无图且 prompt 空白
//     / mask 有值但无 image；判定顺序 n → response_format → image → mask → prompt。
//   - n 非法（"3"、0、-3、2.5、true、null）回落 1，不报错。
//   - response_format 只认 url|b64_json，其余删键（= 不干预上游形态）。
//   - driver 之外的附加字段一律透传。
package imageproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Request 是 canonical 请求。
type Request struct {
	Prompt         string
	Image          []string // nil = 文生图
	Mask           string
	N              int
	ResponseFormat string // "" = 未指定或非法（删键）
	Model          string
	Size           string
	Quality        string
	// AspectRatio / Resolution 是 MiniMax 上游的原生档位（1K/2K/4K + 六种比例），
	// 不属于 OpenAI 协议。它们必须显式进 items —— 当成 vendor extra 透传到顶层
	// 会被上游忽略，表现为"请求了 21:9 却回 1:1"。
	AspectRatio string
	Resolution  string
	Style       string
	User        string
	Extras      map[string]any
}

// ProtocolError 是四键错误信封的语义。
type ProtocolError struct {
	Status  int
	Message string
	Type    string
	Param   string
	Code    string
}

func (e *ProtocolError) Error() string { return e.Message }

func badRequest(param, code, format string, args ...any) *ProtocolError {
	return &ProtocolError{
		Status:  400,
		Message: fmt.Sprintf(format, args...),
		Type:    "invalid_request_error",
		Param:   param,
		Code:    code,
	}
}

var knownKeys = map[string]bool{
	"prompt": true, "image": true, "mask": true, "n": true,
	"response_format": true, "model": true, "size": true,
	"quality": true, "style": true, "user": true,
	"aspect_ratio": true, "resolution": true,
}

// Decode 解析请求体并按协议顺序归一化。
func Decode(raw []byte) (*Request, *ProtocolError) {
	// UseNumber 保留字面量：`2` 与 `2.0` 在 Go 默认解码里都是 float64，
	// 但协议要求区分（2.0 视为 float，回落 1）。丢了字面量就分不出来。
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var probe any
	if err := dec.Decode(&probe); err != nil {
		return nil, badRequest("", "invalid_json", "请求体不是合法 JSON: %v", err)
	}
	obj, ok := probe.(map[string]any)
	if !ok {
		return nil, badRequest("", "invalid_request", "请求体必须是 JSON 对象")
	}

	req := &Request{N: 1}
	for k, v := range obj {
		if knownKeys[k] {
			continue
		}
		if req.Extras == nil {
			req.Extras = map[string]any{}
		}
		req.Extras[k] = v
	}

	// 顺序 1：n。非法值一律回落 1 —— 调用方在说"这张字段无所谓"，
	// 报错只会让一次本来能成功的请求失败。
	req.N = normalizeN(obj["n"])

	// 顺序 2：response_format。只认两个值，其余删键（不干预上游形态）。
	if v, ok := obj["response_format"]; ok {
		if s, isStr := v.(string); isStr {
			switch s {
			case "url", "b64_json":
				req.ResponseFormat = s
			}
		}
	}

	// 顺序 3：image。null / [] 视为没传；空串与非字符串是 400。
	if v, ok := obj["image"]; ok {
		refs, perr := decodeImageField(v)
		if perr != nil {
			return nil, perr
		}
		req.Image = refs
	}

	// 顺序 4：mask。
	if v, ok := obj["mask"]; ok {
		switch t := v.(type) {
		case nil:
			// 当作没传
		case string:
			if strings.TrimSpace(t) == "" {
				return nil, badRequest("mask", "invalid_request", "mask 不能为空字符串；不需要蒙版就直接省略该字段")
			}
			req.Mask = t
		default:
			return nil, badRequest("mask", "invalid_request", "mask 必须是字符串（URL / data URI / 裸 base64）")
		}
	}

	// 顺序 5：prompt。字符串以外的类型直接拒 —— 强转会把 true 变成 "True" 发出去。
	if v, ok := obj["prompt"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, badRequest("prompt", "invalid_request", "prompt 必须是字符串")
		}
		req.Prompt = strings.TrimSpace(s)
	}

	for key, dst := range map[string]*string{
		"model": &req.Model, "size": &req.Size, "quality": &req.Quality,
		"style": &req.Style, "user": &req.User,
		"aspect_ratio": &req.AspectRatio, "resolution": &req.Resolution,
	} {
		if v, ok := obj[key]; ok {
			if s, isStr := v.(string); isStr {
				*dst = strings.TrimSpace(s)
			}
		}
	}

	// 归一化完成后再做跨字段判断。
	if req.Mask != "" && len(req.Image) == 0 {
		return nil, badRequest("image", "invalid_request", "给了 mask 就必须同时给 image")
	}
	if len(req.Image) == 0 && req.Prompt == "" {
		return nil, badRequest("prompt", "invalid_request",
			"文生图必须给 prompt（有 image 时 prompt 才可以省略）")
	}
	return req, nil
}

// decodeImageField 处理 string | string[] | null 三种写法。
func decodeImageField(v any) ([]string, *ProtocolError) {
	switch t := v.(type) {
	case nil:
		return nil, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, badRequest("image", "invalid_request",
				"image 不能为空字符串；文生图请省略该字段或传 null")
		}
		return []string{t}, nil
	case []any:
		out := make([]string, 0, len(t))
		for i, el := range t {
			s, ok := el.(string)
			if !ok {
				return nil, badRequest(fmt.Sprintf("image[%d]", i), "invalid_request",
					"image[%d] 必须是字符串（URL / data URI / 裸 base64）", i)
			}
			if strings.TrimSpace(s) == "" {
				return nil, badRequest(fmt.Sprintf("image[%d]", i), "invalid_request",
					"image[%d] 不能为空字符串", i)
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil, nil // [] = 文生图
		}
		return out, nil
	default:
		return nil, badRequest("image", "invalid_request",
			"image 必须是字符串或字符串数组")
	}
}

// normalizeN 只接受 JSON 整数且 >=1 的值，其余回落 1。
// 注意 2.0 也会回落：JSON 数据模型里它虽是整数，但服务端 isinstance(int)
// 会判成 float —— 协议把这条写成了已知偏差，这里跟服务端保持一致。
func normalizeN(v any) int {
	num, ok := v.(json.Number)
	if !ok {
		return 1
	}
	lit := num.String()
	if strings.ContainsAny(lit, ".eE") {
		return 1 // 2.0 是 float，按协议回落 1
	}
	n, err := strconv.Atoi(lit)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// 上游单次最多 10 个槽位。
const maxSlots = 10

// Build 把 canonical 折成上游 image_synthesize 的请求体。
//
// 返回 warnings：被显式忽略的能力（mask、style 等）。静默丢弃是最坏处置，
// 要么 400，要么点名 —— 这里选择点名，因为 style 属于"可降级"的输出开关。
func (r *Request) Build() (payload map[string]any, warnings []string, perr *ProtocolError) {
	// 上游（MiniMax image_synthesize）强制要求非空 prompt，即使带参考图。
	// 协议允许 image-only，但本渠道做不到 —— 显式 400，而不是偷偷编一个 prompt。
	if r.Prompt == "" && len(r.Image) > 0 {
		return nil, nil, badRequest("prompt", "unsupported_parameter",
			"本渠道的图生图也要求 prompt（上游强制非空），请描述想怎么改")
	}
	if r.Mask != "" {
		return nil, nil, badRequest("mask", "unsupported_parameter",
			"上游不支持 mask（局部重绘），请改用 prompt 描述修改范围")
	}
	if len(r.Image) > 4 {
		return nil, nil, badRequest("image", "invalid_request",
			"参考图最多 4 张（上游限制），收到 %d 张", len(r.Image))
	}

	n := r.N
	if n > maxSlots {
		n = maxSlots
		warnings = append(warnings, fmt.Sprintf("n=%d 超过上游单次上限，已按 %d 张生成", r.N, maxSlots))
	}

	ratio, resolution := Shape(r.Size, r.Quality)
	// 客户端 UI 的「Auto」= 不指定档位，交给上游按 prompt 决定。
	// 这是 MiniMax 客户端的默认档位：既不能当非法值 400，也不能静默换成
	// 1:1 —— 用户选了「自动」却每张都是方图，比报错更难发现。
	if strings.EqualFold(strings.TrimSpace(r.AspectRatio), "auto") {
		r.AspectRatio = ""
		ratio = ""
		warnings = append(warnings, "aspect_ratio=auto：不指定比例，由上游按 prompt 决定")
	}
	if strings.EqualFold(strings.TrimSpace(r.Resolution), "auto") {
		r.Resolution = ""
		resolution = ""
		warnings = append(warnings, "resolution=auto：不指定档位，由上游决定")
	}
	// 上游原生字段优先于 size/quality 推导（显式 > 推导）。
	//
	// 这里做「客户端吸附」而不是严格拒绝：上游确实会吸附，但方式不可靠 ——
	// 实测 `abc`/`8K` 静默回落到 16:9/1K（悄悄改档位等于悄悄改账单），
	// 而 `2:1`/`7:3` 这类"看着合法"的比例会直接生成失败。所以由我们先吸附成
	// 合法的六种比例 / 三档，再下发，并把吸附结果回给调用方。
	if r.AspectRatio != "" {
		snapped, ok := SnapRatio(r.AspectRatio)
		if !ok {
			return nil, nil, badRequest("aspect_ratio", "invalid_request",
				"aspect_ratio 无法解析：%q（请写成 W:H，如 16:9）", r.AspectRatio)
		}
		if snapped != r.AspectRatio {
			warnings = append(warnings, fmt.Sprintf(
				"aspect_ratio %q 已吸附为 %s（上游只输出六种比例，像素按 16 栅格对齐）",
				r.AspectRatio, snapped))
		}
		ratio = snapped
	}
	if r.Resolution != "" {
		snapped, ok := SnapResolution(r.Resolution)
		if !ok {
			return nil, nil, badRequest("resolution", "invalid_request",
				"resolution 无法解析：%q（请用 1K/2K/4K，或 720p/1080p/1440p/2160p 这类写法）",
				r.Resolution)
		}
		if strings.ToUpper(r.Resolution) != snapped {
			warnings = append(warnings, fmt.Sprintf(
				"resolution %q 已吸附为 %s（上游只有 1K/2K/4K 三档）", r.Resolution, snapped))
		}
		resolution = snapped
	}
	items := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		item := map[string]any{
			"prompt":      r.Prompt,
			"output_file": "slot_" + strconv.Itoa(i) + ".png",
		}
		// 空值 = 客户端明确要 Auto，此时**整个键都不写**，不是写空串。
		if ratio != "" {
			item["aspect_ratio"] = ratio
		}
		if resolution != "" {
			item["resolution"] = resolution
		}
		if len(r.Image) > 0 {
			item["input_urls"] = r.Image
		}
		items = append(items, item)
	}

	if r.Style != "" {
		warnings = append(warnings, "style 上游不支持，已忽略（用 prompt 描述风格）")
	}

	payload = map[string]any{"requests": items}
	// vendor extras 原样透传（同名冲突时不覆盖协议字段）。
	for k, v := range r.Extras {
		if _, exists := payload[k]; exists {
			continue
		}
		payload[k] = v
	}
	return payload, warnings, nil
}

// Shape 把 size/quality 映射成上游的 (aspect_ratio, resolution)。
//
// 上游是"档位 × 比例"的矩阵，不是自由 WxH：档位 1K/2K/4K，比例 21:9/16:9/4:3/
// 1:1/3:4/9:16。所以自由像素必须先换算，换算不了就按最近档 —— 绝不能原样发。
func Shape(size, quality string) (ratio, resolution string) {
	ratio = ratioFromSize(size)
	if ratio == "" {
		ratio = "1:1"
	}
	switch strings.ToLower(strings.TrimSpace(quality)) {
	case "hd", "high", "2k":
		resolution = "2K"
	case "4k", "ultra":
		resolution = "4K"
	default:
		resolution = resolutionFromSize(size)
	}
	return ratio, resolution
}

// sixRatios 是上游实际输出的六种比例（像素按 16 栅格对齐，比例本身是吸附值）。
var sixRatios = []struct {
	label string
	value float64
}{
	{"21:9", 21.0 / 9.0}, {"16:9", 16.0 / 9.0}, {"4:3", 4.0 / 3.0},
	{"1:1", 1}, {"3:4", 3.0 / 4.0}, {"9:16", 9.0 / 16.0},
}

// SnapRatio 把任意 "W:H"（也接受 "W/H"）吸附到最接近的合法比例。
//
// 用对数距离而不是差值：2:1 与 16:9 的差是 0.22，与 21:9 的差是 0.33，
// 但按比例尺它们分别差 11% 与 14%，人眼与模型都更接近 16:9。
func SnapRatio(v string) (string, bool) {
	s := strings.TrimSpace(v)
	s = strings.ReplaceAll(s, "/", ":")
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return "", false
	}
	w, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	h, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return "", false
	}
	target := math.Log(w / h)
	best, bestDiff := "1:1", math.MaxFloat64
	for _, r := range sixRatios {
		if d := math.Abs(math.Log(r.value) - target); d < bestDiff {
			best, bestDiff = r.label, d
		}
	}
	return best, true
}

// SnapResolution 把常见写法吸附到 1K/2K/4K 三档。
//
// 按「最长边像素」判：<1200 → 1K（1K 的 16:9 实测就是 1376×768），
// <2000 → 2K，其余 → 4K。所以 720p/1080p 落 1K、1440p 落 2K、2160p/8K 落 4K。
//
// 注意 `8K` 是 8000 级别而不是数字 8：带 k/K 后缀要乘 1024，否则会被误判成 1K。
func SnapResolution(v string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return "", false
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return "", false
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil || num <= 0 {
		return "", false
	}
	// 三种写法语义不同，必须分开判：
	//   `4k`    → 长边 ≈ 4096（乘 1024 后再按长边判）
	//   `1080p` → 1080 是**高度**，按高度判（1K 档的 16:9 实测就是 1376×768）
	//   裸数字  → 当长边像素看
	switch suffix := s[i:]; {
	case strings.HasPrefix(suffix, "k"):
		return resolutionFromLongest(num * 1024), true
	case strings.HasPrefix(suffix, "p"):
		switch {
		case num <= 1200:
			return "1K", true
		case num <= 1800:
			return "2K", true
		default:
			return "4K", true
		}
	default:
		return resolutionFromLongest(num), true
	}
}

// resolutionFromLongest 是**唯一**的「长边像素 → 档位」阈值表。
//
// 阈值来自实测输出（三档都是像素预算，不是固定边长）：
//
//	1K ≈ 1.05MP（1:1=1024²、16:9=1376×768、21:9=1584×672）
//	2K ≈ 4.2MP （16:9=2752×1536）
//	4K ≈ 16.9MP（16:9=5504×3072）
//
// 所以分界取档位边长的几何中点附近：1400 / 2800。
// 单独抽出来是为了避免「size 走一套阈值、resolution 走另一套」——
// 曾经 1536x1024 判 2K 而 resolution="1536" 判 1K，同一个数字两个答案。
func resolutionFromLongest(longest float64) string {
	switch {
	case longest > 2800:
		return "4K"
	case longest > 1400:
		return "2K"
	default:
		return "1K"
	}
}

// resolutionFromSize 按最长边选档位。阈值来自实测输出：
// 1K 的 16:9 是 1376×768，2K 的 16:9 是 2752×1536。
func resolutionFromSize(size string) string {
	w, h, ok := parseWH(size)
	if !ok {
		return "1K"
	}
	longest := w
	if h > longest {
		longest = h
	}
	return resolutionFromLongest(float64(longest))
}

// ratioFromSize 找最近的合法比例。上游只认六种比例，所以 size 也要吸附。
// 直接复用 SnapRatio，避免两处各维护一份比例表（改一处忘一处就会分叉）。
func ratioFromSize(size string) string {
	w, h, ok := parseWH(size)
	if !ok {
		return ""
	}
	best, _ := SnapRatio(strconv.Itoa(w) + ":" + strconv.Itoa(h))
	return best
}

func parseWH(size string) (int, int, bool) {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(size)), "x", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return 0, 0, false
	}
	return w, h, true
}
