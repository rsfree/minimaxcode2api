package media

import "encoding/json"

// 结果提取。
//
// 为什么不强类型解析：媒体网关 23 个工具里，只有本网关已在用的那几个的**出参**
// 是实测过的。新接入的工具（同步出片、抠图、超分、理解类）只有官方工具目录里的
// **入参** schema，出参结构没有公开文档。在没验证过的字段名上写死结构体，等于
// 把"猜"藏进类型里。
//
// 这里的策略是"按已知候选键按序取，取不到就返回空"，由调用方报错：
// 报错比返回一个字段名不对的空对象诚实得多。
//
// 候选键的出处：
//   - output_url / cdn_url / success_items[]：工具说明原文（"Result CDN URL in output_url"）
//     与已实测的 image_synthesize / batch_text_to_music 同族；
//   - results[].text：理解类工具的通用形态（web_search 的说明也写的是 ranked results）；
//   - error_msg / failed_items[]：已实测的失败形态。

// OutputURLs 从响应里提取产物 URL（保序去重）。
func OutputURLs(raw []byte) []string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(v any) {
		s, ok := v.(string)
		if !ok || s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}

	add(obj["output_url"])
	add(obj["cdn_url"])
	add(obj["url"])

	for _, arr := range []any{obj["success_items"], obj["data"], obj["results"], obj["items"]} {
		list, ok := arr.([]any)
		if !ok {
			continue
		}
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			add(m["output_url"])
			add(m["cdn_url"])
			add(m["url"])
		}
	}
	// data 也可能是单个对象。
	if m, ok := obj["data"].(map[string]any); ok {
		add(m["output_url"])
		add(m["cdn_url"])
		add(m["url"])
	}
	return out
}

// FailureText 提取上游给出的失败原因；没有则返回空。
func FailureText(raw []byte) string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	if list, ok := obj["failed_items"].([]any); ok && len(list) > 0 {
		if m, ok := list[0].(map[string]any); ok {
			for _, k := range []string{"error_msg", "error", "message"} {
				if s, ok := m[k].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	// 理解类工具把失败塞在 results[].error 里（success=false），HTTP 仍是 200：
	// 只看顶层 error 会把"图片取不到"报成"没返回文本"，把排查方向带偏。
	if list, ok := obj["results"].([]any); ok {
		for _, it := range list {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["error"].(string); ok && s != "" {
				return s
			}
		}
	}
	switch v := obj["error"].(type) {
	case string:
		if v != "" {
			return v
		}
	case map[string]any:
		if s, ok := v["message"].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// Texts 提取文本类结果（理解/描述类工具）。顺序与请求的输入顺序一致。
func Texts(raw []byte) []string {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	var out []string
	pick := func(m map[string]any) {
		for _, k := range []string{"text", "text_response", "content", "answer", "response", "description", "output"} {
			if s, ok := m[k].(string); ok && s != "" {
				out = append(out, s)
				return
			}
		}
	}
	for _, key := range []string{"results", "data", "success_items", "items", "outputs"} {
		list, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, it := range list {
			switch v := it.(type) {
			case string:
				if v != "" {
					out = append(out, v)
				}
			case map[string]any:
				pick(v)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	for _, k := range []string{"text", "transcription", "content", "answer", "description"} {
		if s, ok := obj[k].(string); ok && s != "" {
			return []string{s}
		}
	}
	return nil
}

// FirstArray 返回响应里第一个数组字段，用于出参结构未公开的检索类工具。
//
// 返回的布尔值表示"确实是找到了数组"，而不是"空数组"——调用方据此区分
// 「上游说没有结果」与「上游换了结构，我们没认出来」。
func FirstArray(raw []byte) ([]any, bool) {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return nil, false
	}
	for _, key := range []string{"results", "items", "data", "success_items", "outputs"} {
		if list, ok := obj[key].([]any); ok {
			return list, true
		}
	}
	return nil, false
}
