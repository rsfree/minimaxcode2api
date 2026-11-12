// Package convert 负责 OpenAI Chat Completions 与 Anthropic Messages 之间的双向转换。
//
// 为什么不用现成的 openai→anthropic 库：MiniMax Code 上游有两个非标准约束，
// 通用库会踩到：
//   - MiniMax-M3.1-Flash 是 forced adaptive thinking，传 thinking.type=disabled 直接 400；
//     MiniMax-M3 则支持 switchable。必须按模型分流，不能"统一关思考"。
//   - 工具调用走 Anthropic 原生 tool_use / tool_result，且要求 user/assistant 严格交替，
//     OpenAI 的连续 tool 消息必须合并进同一条 user 消息，否则上游 400。
//
// 这里把"协议事实"集中在一个包里，handler 只做编排，便于单测与后续跟进上游变化。
package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/linguo2625469/minimaxcode2api/internal/modelcat"
)

// ---------- Anthropic 形态 ----------

type AnthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	System        []ContentBlock       `json:"system,omitempty"`
	Messages      []AnthropicMessage   `json:"messages"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Thinking      *Thinking            `json:"thinking,omitempty"`
	Metadata      map[string]any       `json:"metadata,omitempty"`
}

type AnthropicMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// thinking
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	// image
	Source *ImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`

	// cache_control（prompt caching）
	CacheControl *CacheControl `json:"cache_control,omitempty"`
}

type CacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type AnthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

// Thinking 思考开关。type: enabled | disabled | adaptive。
type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// AnthropicResponse 非流式响应。
type AnthropicResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	Role         string         `json:"role"`
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason"`
	StopSequence string         `json:"stop_sequence"`
	Usage        AnthropicUsage `json:"usage"`
	// BaseResp 上游附加字段（status_code 非 0 时代表业务错误）。
	BaseResp *struct {
		StatusCode int    `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp,omitempty"`
}

type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokensDetails      *struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

// ---------- OpenAI 形态 ----------

type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []ChatMessage `json:"messages"`

	MaxTokens           *int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int            `json:"max_completion_tokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	TopP                *float64        `json:"top_p,omitempty"`
	Stop                json.RawMessage `json:"stop,omitempty"`
	Stream              bool            `json:"stream,omitempty"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Tools      []ChatTool      `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	// ReasoningEffort none|minimal|low|medium|high|xhigh|max（各家客户端写法不一）。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// Reasoning 部分客户端（OpenRouter 风格）用对象传。
	Reasoning *struct {
		Effort    string `json:"effort,omitempty"`
		MaxTokens int    `json:"max_tokens,omitempty"`
	} `json:"reasoning,omitempty"`
	ParallelToolCalls *bool  `json:"parallel_tool_calls,omitempty"`
	User              string `json:"user,omitempty"`
	N                 int    `json:"n,omitempty"`
}

type ChatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
	Name    string          `json:"name,omitempty"`
	// assistant 工具调用
	ToolCalls []ChatToolCall `json:"tool_calls,omitempty"`
	// tool 角色回填
	ToolCallID string `json:"tool_call_id,omitempty"`
	// 部分客户端会把上一轮思考也回填，尽量利用（上游 thinking block 需要 signature，
	// 没有 signature 的回填反而是噪声，这里默认丢弃，见 BuildAnthropicRequest 注释）。
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type ChatToolCall struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Index    *int   `json:"index,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type ChatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   ChatUsage    `json:"usage"`
}

type ChatChoice struct {
	Index        int     `json:"index"`
	Message      ChatMsg `json:"message"`
	FinishReason string  `json:"finish_reason"`
	Logprobs     any     `json:"logprobs"`
}

type ChatMsg struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChatToolCall `json:"tool_calls,omitempty"`
	// Refusal 保持 null，兼容要求字段齐全的客户端。
	Refusal any `json:"refusal,omitempty"`
}

type ChatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details,omitempty"`
}

// ---------- 模型能力 ----------

// ModelCapability 描述一个上游模型的思考能力。
type ModelCapability struct {
	// Thinking: forced_adaptive（只能自适应，禁不掉）| switchable（可关可开）
	Thinking string
	// MaxOutput 输出上限。
	MaxOutput int
	// ContextWindow 上下文窗口。
	ContextWindow int
}

// CapabilityOf 返回**对话模型**的能力；未知模型与非对话模型一律 ok=false，
// 按"不要碰 thinking"处理。
//
// 真源在 modelcat：这里只是把「清单」投影成「转换器要的三个字段」。曾经这份表
// 独立维护过一遍，加模型时漏改一处就会让 thinking 策略静默失效。
func CapabilityOf(model string) (ModelCapability, bool) {
	info, ok := modelcat.ByID(model)
	if !ok || info.Kind != modelcat.Chat {
		return ModelCapability{}, false
	}
	return ModelCapability{
		Thinking:      info.Thinking,
		MaxOutput:     info.MaxOutput,
		ContextWindow: info.ContextWindow,
	}, true
}

// ---------- 请求转换 ----------

// RequestOptions 转换时的网关侧决策。
type RequestOptions struct {
	Model          string
	MaxTokens      int
	ThinkingPolicy string // auto | off | on
	Capability     ModelCapability
	Known          bool
}

// BuildAnthropicRequest 把 OpenAI 请求翻成 Anthropic 请求。
func BuildAnthropicRequest(in *ChatRequest, opt RequestOptions) (*AnthropicRequest, error) {
	if in == nil {
		return nil, fmt.Errorf("empty request")
	}
	out := &AnthropicRequest{Model: opt.Model, Stream: in.Stream}

	maxTokens := opt.MaxTokens
	if in.MaxCompletionTokens != nil && *in.MaxCompletionTokens > 0 {
		maxTokens = *in.MaxCompletionTokens
	} else if in.MaxTokens != nil && *in.MaxTokens > 0 {
		maxTokens = *in.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 8192
	}
	out.MaxTokens = maxTokens

	out.Temperature = in.Temperature
	out.TopP = in.TopP
	if stops := parseStop(in.Stop); len(stops) > 0 {
		out.StopSequences = stops
	}

	// thinking 必须与 max_tokens 协同：budget 必须小于 max_tokens。
	effort := strings.ToLower(strings.TrimSpace(in.ReasoningEffort))
	if effort == "" && in.Reasoning != nil {
		effort = strings.ToLower(strings.TrimSpace(in.Reasoning.Effort))
	}
	budget := 0
	if in.Reasoning != nil && in.Reasoning.MaxTokens > 0 {
		budget = in.Reasoning.MaxTokens
	}
	out.Thinking = decideThinking(opt, effort, budget, maxTokens)

	// system 与 messages 拆分。
	system, msgs, err := convertMessages(in.Messages)
	if err != nil {
		return nil, err
	}
	out.System = system
	out.Messages = msgs

	// 工具。
	for _, t := range in.Tools {
		if t.Type != "" && t.Type != "function" {
			continue // 内置工具（web_search 等）上游不支持，直接忽略
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out.Tools = append(out.Tools, AnthropicTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	if len(out.Tools) > 0 {
		out.ToolChoice = parseToolChoice(in.ToolChoice, len(out.Tools))
	}

	if in.User != "" {
		out.Metadata = map[string]any{"user_id": in.User}
	}
	return out, nil
}

// decideThinking 按模型能力 + 网关策略 + 客户端 effort 决定 thinking 参数。
//
// forced_adaptive 模型：一律不传（传 disabled 会被 400 拒绝，传 enabled 非协议）。
// switchable 模型：policy=on 或 effort 明确要求思考时开，否则关。
func decideThinking(opt RequestOptions, effort string, budget, maxTokens int) *Thinking {
	capability := opt.Capability
	if !opt.Known {
		// 未知模型不猜：让服务端默认。
		return nil
	}
	if capability.Thinking == "forced_adaptive" {
		return nil
	}

	switch effort {
	case "none", "minimal":
		return &Thinking{Type: "disabled"}
	case "low":
		if budget <= 0 {
			budget = 2048
		}
	case "medium":
		if budget <= 0 {
			budget = 8192
		}
	case "high", "xhigh", "max":
		if budget <= 0 {
			budget = 16384
		}
	}

	if budget <= 0 {
		if opt.ThinkingPolicy != "on" {
			// auto / off：OpenAI 客户端通常只要正文，关掉更省更快。
			return &Thinking{Type: "disabled"}
		}
		budget = 8192
	}

	// budget_tokens 必须严格小于 max_tokens，且要给正文留 512。
	// max_tokens 太小（< 1536）时根本放不下最小思考预算——此时关掉思考，
	// 比发一个 budget >= max_tokens 的组合去换 400 更合理。
	const minBudget, reserve = 1024, 512
	limit := maxTokens - reserve
	if limit < minBudget {
		return &Thinking{Type: "disabled"}
	}
	if budget > limit {
		budget = limit
	}
	if budget < minBudget {
		budget = minBudget
	}
	return &Thinking{Type: "enabled", BudgetTokens: budget}
}

// convertMessages 拆分 system、归一 tool 消息、合并同角色相邻消息。
func convertMessages(in []ChatMessage) ([]ContentBlock, []AnthropicMessage, error) {
	var system []ContentBlock
	var out []AnthropicMessage

	appendBlocks := func(role string, blocks []ContentBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, AnthropicMessage{Role: role, Content: blocks})
	}

	for _, m := range in {
		switch strings.ToLower(m.Role) {
		case "system", "developer":
			text, err := contentToText(m.Content)
			if err != nil {
				return nil, nil, err
			}
			if strings.TrimSpace(text) != "" {
				system = append(system, ContentBlock{Type: "text", Text: text})
			}

		case "user":
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return nil, nil, err
			}
			appendBlocks("user", blocks)

		case "assistant":
			var blocks []ContentBlock
			if text, err := contentToText(m.Content); err != nil {
				return nil, nil, err
			} else if text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			}
			for _, tc := range m.ToolCalls {
				args := strings.TrimSpace(tc.Function.Arguments)
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					// 上游要合法 JSON；坏参数直接包成字符串，避免整条请求 400。
					b, _ := json.Marshal(map[string]string{"_raw": args})
					args = string(b)
				}
				blocks = append(blocks, ContentBlock{
					Type:  "tool_use",
					ID:    firstNonEmpty(tc.ID, "toolu_"+randomID(12)),
					Name:  tc.Function.Name,
					Input: json.RawMessage(args),
				})
			}
			appendBlocks("assistant", blocks)

		case "tool", "function":
			// OpenAI 的 tool 消息 → Anthropic user 消息里的 tool_result。
			text, err := contentToText(m.Content)
			if err != nil {
				return nil, nil, err
			}
			block := ContentBlock{Type: "tool_result", ToolUseID: m.ToolCallID}
			if text != "" {
				b, _ := json.Marshal(text)
				block.Content = b
			} else {
				block.Content = json.RawMessage(`""`)
			}
			appendBlocks("user", []ContentBlock{block})

		default:
			// 未知角色当 user 处理，比整条请求失败更友好。
			blocks, err := userBlocks(m.Content)
			if err != nil {
				return nil, nil, err
			}
			appendBlocks("user", blocks)
		}
	}

	// Anthropic 要求首条是 user。
	if len(out) > 0 && out[0].Role == "assistant" {
		out = append([]AnthropicMessage{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "Continue."}}}}, out...)
	}
	if len(out) == 0 {
		out = []AnthropicMessage{{Role: "user", Content: []ContentBlock{{Type: "text", Text: "Continue."}}}}
	}
	return system, out, nil
}

// userBlocks 把 OpenAI user content 转成 Anthropic blocks。
func userBlocks(raw json.RawMessage) ([]ContentBlock, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		return []ContentBlock{{Type: "text", Text: s}}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		// image_url
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
		// 部分客户端直接用 input_audio / file 等，忽略
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		// 不是标准结构：整体序列化当文本，保证不丢内容。
		return []ContentBlock{{Type: "text", Text: string(raw)}}, nil
	}
	var out []ContentBlock
	for _, p := range parts {
		switch p.Type {
		case "text", "input_text":
			if strings.TrimSpace(p.Text) != "" {
				out = append(out, ContentBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			if b, ok := imageBlock(p.ImageURL.URL); ok {
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// imageBlock 把 data URI / http URL 转成 Anthropic image block。
func imageBlock(url string) (ContentBlock, bool) {
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		semi := strings.Index(rest, ";base64,")
		if semi < 0 {
			return ContentBlock{}, false
		}
		mediaType := rest[:semi]
		data := rest[semi+len(";base64,"):]
		if mediaType == "" || data == "" {
			return ContentBlock{}, false
		}
		return ContentBlock{Type: "image", Source: &ImageSource{
			Type: "base64", MediaType: mediaType, Data: data,
		}}, true
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return ContentBlock{Type: "image", Source: &ImageSource{Type: "url", URL: url}}, true
	}
	return ContentBlock{}, false
}

func contentToText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" || p.Type == "input_text" || p.Type == "" {
				b.WriteString(p.Text)
			}
		}
		return b.String(), nil
	}
	return string(raw), nil
}

func parseStop(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s != "" {
			return []string{s}
		}
		return nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err == nil {
		return arr
	}
	return nil
}

func parseToolChoice(raw json.RawMessage, toolCount int) *AnthropicToolChoice {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &AnthropicToolChoice{Type: "auto"}
		case "required", "any":
			return &AnthropicToolChoice{Type: "any"}
		case "none":
			return &AnthropicToolChoice{Type: "none"}
		}
		return nil
	}
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil {
		if obj.Function.Name != "" {
			return &AnthropicToolChoice{Type: "tool", Name: obj.Function.Name}
		}
		if obj.Type == "any" || obj.Type == "required" {
			return &AnthropicToolChoice{Type: "any"}
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
