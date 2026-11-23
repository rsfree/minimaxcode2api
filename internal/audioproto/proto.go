// Package audioproto 声明本部署的语音能力面，并把 OpenAI 骨架字段收敛到上游。
//
// 原则（来自 audio-adapter 规范）：**北向只承诺契约，不承诺厂商能力**。
// OpenAI 文档里的枚举、默认值、数值区间都只是参考；本部署实际接受什么，
// 以本文件的声明为准，并同步写在 docs/audio-capabilities.md。
//
// 本部署的声明（不是 OpenAI 的参考值）：
//
//	speech.response_format  只有 mp3（上游 synthesize_speech 没有 format 参数）
//	speech.speed            0.5–2.0（上游 schema 原文），超出直接 400，不钳位
//	speech.input            1–10000 字符（上游官方 TTS 上限）
//	speech.voice            13 个 OpenAI 别名 + 任意原生 voice_id 直传
//	speech.instructions     不支持（上游无等价参数）→ 400
//	speech.stream_format    audio / sse（sse 为网关扩展）
//	speech 扩展字段          volume / pitch / emotion（上游能力，OpenAI 没有）
//
//	transcriptions.response_format  json / text / verbose_json；srt/vtt 需要真实分段，
//	                                上游不提供 → 400；diarized_json 无说话人分离 → 400
//	transcriptions.language/prompt/temperature/timestamp_granularities/...  → 400
package audioproto

import "strings"

// ---------- 音色 ----------

// openAIVoices 是 OpenAI 预置音色名 → MiniMax 预置 voice_id 的近似映射。
//
// 为什么只能"近似"：OpenAI 没有公开这 13 个音色的声学参数，能对齐的只有
// 性别、年龄感与语调（如 onyx 低沉男声、nova 清亮女声）。这里挑的是上游
// 8 个基础预置音色（male-qn-* / female-*）及其 -jingpin 变体，中文场景下
// 稳定可用；调用方要精确音色时直接传 voice_id（会原样透传，不走本表）。
//
// 键必须小写：OpenAI 客户端常把音色名转小写，客户端手写时又可能首字母大写。
var openAIVoices = map[string]string{
	"alloy":   "male-qn-qingse",           // 中性、年轻；同时是上游默认音色
	"echo":    "male-qn-jingying",         // 清晰男声
	"fable":   "male-qn-jingying-jingpin", // 叙述感男声（beta）
	"onyx":    "male-qn-badao",            // 低沉、权威
	"ash":     "male-qn-daxuesheng",       // 年轻男声
	"verse":   "male-qn-qingse-jingpin",   // 通用男声
	"ballad":  "male-qn-badao-jingpin",    // 温暖男声
	"nova":    "female-shaonv",            // 清亮女声
	"shimmer": "female-tianmei",           // 柔和女声
	"sage":    "female-yujie",             // 沉静女声
	"coral":   "female-chengshu",          // 温暖成熟女声
	"marin":   "female-shaonv-jingpin",    // 2026 新增自然女声
	"cedar":   "male-qn-jingying-jingpin", // 2026 新增自然男声
}

// ResolveVoice 把 voice 字段解析成上游 voice_id。
//
// 命中 OpenAI 别名时第二个返回值为 true；未命中的一律原样透传 —— 上游有
// 327 个预置音色且可能存在克隆音色，网关不做白名单校验。
func ResolveVoice(name string) (id string, mapped bool) {
	n := strings.TrimSpace(name)
	if n == "" {
		return "", false
	}
	if id, ok := openAIVoices[strings.ToLower(n)]; ok {
		return id, true
	}
	return n, false
}

// OpenAIVoices 返回别名表的副本（供文档/测试/能力接口使用）。
func OpenAIVoices() map[string]string {
	out := make(map[string]string, len(openAIVoices))
	for k, v := range openAIVoices {
		out[k] = v
	}
	return out
}

// ---------- speech 参数面 ----------

// FormatMP3 是本部署唯一声明的输出格式。
const FormatMP3 = "mp3"

// MaxSpeechInput 是上游官方 TTS 的文本上限。
const MaxSpeechInput = 10000

const (
	// SpeedMin / SpeedMax 是上游 synthesize_speech schema 声明的区间。
	SpeedMin = 0.5
	SpeedMax = 2.0
	// 上游没有声明的扩展字段边界。
	VolumeMin = 0.0
	VolumeMax = 10.0
	PitchMin  = -12
	PitchMax  = 12
)

// SupportedSpeechFormat 本部署只声明 mp3。
//
// 返回 (规整后的取值, 是否支持)。空值按默认 mp3。
func SupportedSpeechFormat(s string) (format string, ok bool) {
	f := strings.ToLower(strings.TrimSpace(s))
	if f == "" {
		f = FormatMP3
	}
	return f, f == FormatMP3
}

// ValidateSpeed 校验 speed 是否落在本部署声明的区间内。
//
// 0 视为"未指定"（JSON 缺省与显式 0 不可区分）。超出 [0.5, 2.0] 返回 false，
// 调用方必须 400 —— 不做钳位：钳位后返回 200 属于静默降级。
func ValidateSpeed(v float64) (float64, bool) {
	if v == 0 {
		return 0, true
	}
	if v < SpeedMin || v > SpeedMax {
		return 0, false
	}
	return v, true
}

// ValidateVolume 校验扩展字段 volume。
func ValidateVolume(v float64) bool { return v >= VolumeMin && v <= VolumeMax }

// ValidatePitch 校验扩展字段 pitch。
func ValidatePitch(v int) bool { return v >= PitchMin && v <= PitchMax }

// emotions 是上游 synthesize_speech 声明的情绪枚举。
var emotions = map[string]bool{
	"happy": true, "sad": true, "angry": true, "fearful": true,
	"disgusted": true, "surprised": true, "neutral": true,
}

// ValidateEmotion 校验扩展字段 emotion（空值表示不指定）。
func ValidateEmotion(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	return s == "" || emotions[s]
}

// Emotions 返回上游声明的情绪枚举（供能力接口/文档使用）。
func Emotions() []string {
	return []string{"happy", "sad", "angry", "fearful", "disgusted", "surprised", "neutral"}
}

// ---------- transcriptions 参数面 ----------

const (
	TranscribeJSON        = "json"
	TranscribeText        = "text"
	TranscribeVerboseJSON = "verbose_json"
	// 以下格式是 OpenAI 合法的取值，但本部署没有真实数据可渲染。
	TranscribeSRT          = "srt"
	TranscribeVTT          = "vtt"
	TranscribeDiarizedJSON = "diarized_json"
)

var openAITranscribeFormats = map[string]bool{
	TranscribeJSON: true, TranscribeText: true, TranscribeVerboseJSON: true,
	TranscribeSRT: true, TranscribeVTT: true, TranscribeDiarizedJSON: true,
}

// NormalizeTranscriptionFormat 归一化 response_format。
//
// known=false 表示不是 OpenAI 的合法取值（400 参数错误）。
// known=true 但 SupportedTranscriptionFormat=false 表示取值合法、本部署不支持
// （400 并说明原因：srt/vtt 需要真实分段，diarized_json 需要说话人分离）。
func NormalizeTranscriptionFormat(s string) (format string, known bool) {
	f := strings.ToLower(strings.TrimSpace(s))
	if f == "" {
		return TranscribeJSON, true
	}
	return f, openAITranscribeFormats[f]
}

// SupportedTranscriptionFormat 本部署实际能渲染的三种。
func SupportedTranscriptionFormat(format string) bool {
	return format == TranscribeJSON || format == TranscribeText || format == TranscribeVerboseJSON
}
