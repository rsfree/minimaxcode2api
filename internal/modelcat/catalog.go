// Package modelcat 是「对外模型清单」的唯一真源。
//
// 为什么要类别（Kind）而不是一张纯字符串清单：
// /v1/models 是客户端的**能力发现面**。只列对话模型，图片/视频模型在客户端选择器里
// 根本不存在；而把它们无差别塞进同一个列表，客户端又会把视频模型发给
// /v1/chat/completions —— 上游只会回一句难懂的参数错误。类别就是"这个 id 该发到
// 哪个端点"的答案，服务端据此在错路由时直接 400 指路。
//
// 纪律：清单里只放**本网关真的受理**的模型。
//   - 视频两个 id 由 videoproto.ResolveModel 实际受理，catalog_test 钉住两边一致；
//   - 图片上游 image_synthesize **没有 model 参数**（见 README），所以
//     MiniMax-Image 是"能力标签"而不是上游型号：列它是为了让客户端能把图片能力
//     配成一个可选的 model id，请求里带不带它都走同一条上游链路。
package modelcat

import "strings"

// 类别取值。与端点一一对应，不要新增"混合"类别。
const (
	Chat  = "chat"  // POST /v1/chat/completions、/v1/messages
	Image = "image" // POST /v1/images/generations
	Video = "video" // POST /v1/contents/generations/tasks
)

// Info 描述一个对外模型。
type Info struct {
	ID string
	// Kind 决定客户端该发到哪个端点。
	Kind string
	// Aliases 是同一模型的其他写法（大小写不敏感）。别名只做解析，不出现在列表里。
	Aliases []string
	// 以下三项只对 Chat 有意义；媒体模型为 0 / 空。
	ContextWindow int
	MaxOutput     int
	// Thinking: forced_adaptive（只能自适应，禁不掉）| switchable（可关可开）
	Thinking string
}

// 顺序即 /v1/models 的输出顺序：对话在前，媒体在后。
var catalog = []Info{
	{
		ID: "MiniMax-M3.1-Flash", Kind: Chat,
		Aliases:       []string{"M3.1-Flash", "m3.1-flash", "MiniMax-M3.1"},
		ContextWindow: 512000, MaxOutput: 128000, Thinking: "forced_adaptive",
	},
	{
		ID: "MiniMax-M3", Kind: Chat,
		Aliases:       []string{"M3", "minimax-m3"},
		ContextWindow: 512000, MaxOutput: 128000, Thinking: "switchable",
	},
	{
		// 能力标签，不是上游型号（上游无 model 参数）。别名覆盖三种常见写法：
		// 客户端 UI 的「H3 Image」、官方图片模型名、以及最朴素的 image。
		ID: "MiniMax-Image", Kind: Image,
		Aliases: []string{"image", "h3-image", "image-01"},
	},
	{
		ID: "MiniMax-H3", Kind: Video,
		Aliases: []string{"h3", "h3-max", "minimax-h3-max"},
	},
	{
		ID: "MiniMax-Hailuo-2.3", Kind: Video,
		Aliases: []string{"h2.3", "hailuo", "hailuo-2.3", "minimax-hailuo-23"},
	},
}

// All 返回全部模型的副本（调用方可随意改）。
func All() []Info {
	out := make([]Info, len(catalog))
	copy(out, catalog)
	return out
}

// ByID 精确匹配 id（大小写不敏感）——用于配置里写死的 id 归一。
func ByID(id string) (Info, bool) {
	for _, m := range catalog {
		if strings.EqualFold(m.ID, id) {
			return m, true
		}
	}
	return Info{}, false
}

// Lookup 按 id 或别名解析（大小写不敏感）。
//
// 必须比 ByID 宽：客户端拿到 /v1/models 的 id 之外的写法（h3、M3）来探测时，
// 只有别名也认，才不会 404 或被当成"未知模型"放行到上游。
func Lookup(name string) (Info, bool) {
	s := strings.TrimSpace(name)
	if s == "" {
		return Info{}, false
	}
	if m, ok := ByID(s); ok {
		return m, true
	}
	for _, m := range catalog {
		for _, a := range m.Aliases {
			if strings.EqualFold(a, s) {
				return m, true
			}
		}
	}
	return Info{}, false
}

// KindOf 返回模型类别；不在清单里的返回 false（调用方按自己的默认处置）。
func KindOf(name string) (string, bool) {
	if m, ok := Lookup(name); ok {
		return m.Kind, true
	}
	return "", false
}
