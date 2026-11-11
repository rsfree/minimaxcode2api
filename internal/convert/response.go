package convert

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// randomID 生成短随机 id（hex）。
func randomID(n int) string {
	b := make([]byte, (n+1)/2)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n)
	}
	return hex.EncodeToString(b)[:n]
}

// NewCompletionID 生成 OpenAI 风格的响应 id。
func NewCompletionID() string { return "chatcmpl-" + randomID(24) }

// MapFinishReason Anthropic stop_reason → OpenAI finish_reason。
func MapFinishReason(stop string, hasToolCalls bool) string {
	if hasToolCalls || stop == "tool_use" {
		return "tool_calls"
	}
	switch stop {
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence", "":
		return "stop"
	default:
		return "stop"
	}
}

// ToOpenAI 非流式：Anthropic 响应 → OpenAI ChatCompletion。
func ToOpenAI(resp *AnthropicResponse, model string, id string) *ChatResponse {
	if id == "" {
		id = NewCompletionID()
	}
	msg := ChatMsg{Role: "assistant"}
	var text, reasoning strings.Builder
	var toolCalls []ChatToolCall

	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			tc := ChatToolCall{ID: b.ID, Type: "function"}
			tc.Function.Name = b.Name
			tc.Function.Arguments = string(input)
			toolCalls = append(toolCalls, tc)
		}
	}

	msg.Content = text.String()
	msg.ReasoningContent = reasoning.String()
	msg.ToolCalls = toolCalls

	usage := ChatUsage{
		PromptTokens:     promptTokens(resp.Usage),
		CompletionTokens: resp.Usage.OutputTokens,
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if resp.Usage.CacheReadInputTokens > 0 {
		usage.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: resp.Usage.CacheReadInputTokens}
	}
	if resp.Usage.OutputTokensDetails != nil && resp.Usage.OutputTokensDetails.ThinkingTokens > 0 {
		usage.CompletionTokensDetails = &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{ReasoningTokens: resp.Usage.OutputTokensDetails.ThinkingTokens}
	}

	return &ChatResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   firstNonEmpty(model, resp.Model),
		Choices: []ChatChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: MapFinishReason(resp.StopReason, len(toolCalls) > 0),
		}},
		Usage: usage,
	}
}

// promptTokens Anthropic 的输入计量要加上缓存命中/写入部分，才对得上 OpenAI 语义。
func promptTokens(u AnthropicUsage) int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// ---------- 流式 ----------

// StreamChunk 一个 OpenAI 流式分片。
type StreamChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []ChunkChoice `json:"choices"`
	Usage   *ChatUsage    `json:"usage,omitempty"`
}

type ChunkChoice struct {
	Index        int        `json:"index"`
	Delta        ChunkDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
	Logprobs     any        `json:"logprobs,omitempty"`
}

type ChunkDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []ChunkToolCall `json:"tool_calls,omitempty"`
}

type ChunkToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// StreamConverter 把 Anthropic SSE 事件转成 OpenAI 分片。
//
// 它是有状态的：Anthropic 的 tool_use 输入是 input_json_delta 片段，
// 必须映射到同一个 OpenAI tool_calls index；thinking 需要单独走 reasoning_content。
type StreamConverter struct {
	id      string
	model   string
	created int64

	sentRole  bool
	blockType map[int]string // anthropic content block index → type
	toolIndex map[int]int    // anthropic block index → openai tool_calls index
	toolCount int

	StopReason string
	usageIn    int
	usageOut   int
	cacheRead  int
	thinkTok   int

	finished bool
}

func NewStreamConverter(model, id string) *StreamConverter {
	if id == "" {
		id = NewCompletionID()
	}
	return &StreamConverter{
		id:        id,
		model:     model,
		created:   time.Now().Unix(),
		blockType: map[int]string{},
		toolIndex: map[int]int{},
	}
}

func (c *StreamConverter) chunk(delta ChunkDelta, finish *string) *StreamChunk {
	return &StreamChunk{
		ID:      c.id,
		Object:  "chat.completion.chunk",
		Created: c.created,
		Model:   c.model,
		Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
	}
}

func (c *StreamConverter) encode(chunks ...*StreamChunk) [][]byte {
	out := make([][]byte, 0, len(chunks))
	for _, ch := range chunks {
		raw, err := json.Marshal(ch)
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out
}

// Handle 处理一个 SSE 事件，返回需要写给客户端的 data 负载列表（不含 "data: " 前缀）。
func (c *StreamConverter) Handle(event string, data []byte) [][]byte {
	if len(data) == 0 {
		return nil
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil
	}
	if event == "" {
		event = head.Type
	}

	switch event {
	case "message_start":
		var ev struct {
			Message struct {
				ID    string         `json:"id"`
				Model string         `json:"model"`
				Usage AnthropicUsage `json:"usage"`
			} `json:"message"`
		}
		_ = json.Unmarshal(data, &ev)
		if ev.Message.Model != "" && c.model == "" {
			c.model = ev.Message.Model
		}
		c.usageIn = ev.Message.Usage.InputTokens
		c.cacheRead = ev.Message.Usage.CacheReadInputTokens
		if ev.Message.Usage.CacheCreationInputTokens > 0 {
			c.usageIn += ev.Message.Usage.CacheCreationInputTokens
		}
		// 首帧带 role，部分客户端（如 Cherry Studio）依赖它初始化消息体。
		c.sentRole = true
		return c.encode(c.chunk(ChunkDelta{Role: "assistant", Content: strPtr("")}, nil))

	case "content_block_start":
		var ev struct {
			Index        int             `json:"index"`
			ContentBlock json.RawMessage `json:"content_block"`
		}
		_ = json.Unmarshal(data, &ev)
		var block struct {
			Type     string `json:"type"`
			ID       string `json:"id"`
			Name     string `json:"name"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		}
		_ = json.Unmarshal(ev.ContentBlock, &block)
		c.blockType[ev.Index] = block.Type

		switch block.Type {
		case "text":
			if block.Text != "" {
				return c.encode(c.chunk(ChunkDelta{Content: strPtr(block.Text)}, nil))
			}
			return nil
		case "thinking":
			if block.Thinking != "" {
				return c.encode(c.chunk(ChunkDelta{ReasoningContent: strPtr(block.Thinking)}, nil))
			}
			return nil
		case "tool_use":
			idx := c.toolCount
			c.toolCount++
			c.toolIndex[ev.Index] = idx
			tc := ChunkToolCall{Index: idx, ID: block.ID, Type: "function"}
			tc.Function.Name = block.Name
			return c.encode(c.chunk(ChunkDelta{ToolCalls: []ChunkToolCall{tc}}, nil))
		}
		return nil

	case "content_block_delta":
		var ev struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(data, &ev)
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text == "" {
				return nil
			}
			return c.encode(c.chunk(ChunkDelta{Content: strPtr(ev.Delta.Text)}, nil))
		case "thinking_delta":
			if ev.Delta.Thinking == "" {
				return nil
			}
			return c.encode(c.chunk(ChunkDelta{ReasoningContent: strPtr(ev.Delta.Thinking)}, nil))
		case "input_json_delta":
			idx, ok := c.toolIndex[ev.Index]
			if !ok {
				return nil
			}
			tc := ChunkToolCall{Index: idx}
			tc.Function.Arguments = ev.Delta.PartialJSON
			return c.encode(c.chunk(ChunkDelta{ToolCalls: []ChunkToolCall{tc}}, nil))
		case "signature_delta":
			// 思考签名对下游无意义，丢弃。
			return nil
		}
		return nil

	case "content_block_stop":
		return nil

	case "message_delta":
		var ev struct {
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage AnthropicUsage `json:"usage"`
		}
		_ = json.Unmarshal(data, &ev)
		c.StopReason = ev.Delta.StopReason
		if ev.Usage.OutputTokens > 0 {
			c.usageOut = ev.Usage.OutputTokens
		}
		if ev.Usage.InputTokens > 0 {
			c.usageIn = ev.Usage.InputTokens + ev.Usage.CacheCreationInputTokens + ev.Usage.CacheReadInputTokens
		}
		if ev.Usage.CacheReadInputTokens > 0 {
			c.cacheRead = ev.Usage.CacheReadInputTokens
		}
		if ev.Usage.OutputTokensDetails != nil {
			c.thinkTok = ev.Usage.OutputTokensDetails.ThinkingTokens
		}
		return nil

	case "message_stop":
		c.finished = true
		finish := MapFinishReason(c.StopReason, c.toolCount > 0)
		return c.encode(c.chunk(ChunkDelta{}, &finish))

	case "ping":
		return nil

	case "error":
		// 交给上层转成 error 事件；这里返回空，避免被当成正常分片。
		return nil
	}
	return nil
}

// Usage 生成最终 usage 分片（stream_options.include_usage 时使用）。
func (c *StreamConverter) Usage() *StreamChunk {
	u := &ChatUsage{
		PromptTokens:     c.usageIn,
		CompletionTokens: c.usageOut,
	}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	if c.cacheRead > 0 {
		u.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: c.cacheRead}
	}
	if c.thinkTok > 0 {
		u.CompletionTokensDetails = &struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		}{ReasoningTokens: c.thinkTok}
	}
	return &StreamChunk{
		ID:      c.id,
		Object:  "chat.completion.chunk",
		Created: c.created,
		Model:   c.model,
		Choices: []ChunkChoice{},
		Usage:   u,
	}
}

// EncodeUsage 序列化 usage 分片。
func (c *StreamConverter) EncodeUsage() []byte {
	raw, err := json.Marshal(c.Usage())
	if err != nil {
		return nil
	}
	return raw
}

// Finished 是否已收到 message_stop。
func (c *StreamConverter) Finished() bool { return c.finished }

// PromptTokens / OutputTokens 暴露给用量记账。
func (c *StreamConverter) PromptTokens() int { return c.usageIn }
func (c *StreamConverter) OutputTokens() int { return c.usageOut }

func strPtr(s string) *string { return &s }
