// Package audioproto 定义 OpenAI 音频协议与上游能力之间的映射规则。
//
// OpenAI 与上游（MiniMax Code 的 MCP 媒体工具）并不对等，差异全部集中在这里：
//
//	OpenAI 字段                          上游
//	speech.voice = alloy/echo/…          synthesize_speech.voice_id = male-qn-qingse 等预置音色
//	speech.speed = 0.25–4.0              speed = 0.5–2.0
//	speech.response_format = mp3/…       **只能出 mp3**（schema 里没有 format 字段）
//	transcriptions.response_format=srt   listen_audio **不返回时间戳**
//
// 处理原则分三档，handler 只按这里的结论执行：
//
//	能等价映射   → 映射（音色、speed 区间）
//	不能等价     → 显式降级，并在响应头标注实际格式（mp3 之外的 response_format）
//	根本做不到   → 明确 400，不假装支持（srt / vtt）
package audioproto

import "strings"

// ---------- 音色 ----------

// openAIVoices 是 OpenAI 预置音色名 → MiniMax 预置 voice_id 的近似映射。
//
// 为什么只能"近似"：OpenAI 没有公开这 11 个音色的声学参数，能对齐的只有
// 性别、年龄感与语调（如 onyx 低沉男声、nova 清亮女声）。这里挑的是上游
// 8 个基础预置音色（male-qn-* / female-*）及其 -jingpin 变体，中文场景下
// 稳定可用；调用方要精确音色时直接传 voice_id（会原样透传，不走本表）。
//
// 键必须小写：OpenAI 客户端常把音色名转小写，客户端手写时又可能首字母大写。
var openAIVoices = map[string]string{
	"alloy":   "male-qn-qingse",           // 中性、年轻；同时是上游默认音色，保持既有行为
	"echo":    "male-qn-jingying",         // 清晰男声
	"fable":   "male-qn-jingying-jingpin", // 叙述感男声（beta 音色，音色更饱满）
	"onyx":    "male-qn-badao",            // 低沉、权威
	"ash":     "male-qn-daxuesheng",       // 年轻男声
	"verse":   "male-qn-qingse-jingpin",   // 通用男声
	"ballad":  "male-qn-badao-jingpin",    // 温暖男声
	"nova":    "female-shaonv",            // 清亮女声
	"shimmer": "female-tianmei",           // 柔和女声
	"sage":    "female-yujie",             // 沉静女声
	"coral":   "female-chengshu",          // 温暖成熟女声
	"marin":   "female-shaonv-jingpin",    // 2026 新增的自然女声
	"cedar":   "male-qn-jingying-jingpin", // 2026 新增的自然男声
}

// ResolveVoice 把 voice 字段解析成上游 voice_id。
//
// 命中 OpenAI 别名时第二个返回值为 true（handler 据此回报 X-MiniMax-Voice-Alias）；
// 未命中的一律原样透传 —— 上游有 327 个音色，网关不做白名单校验，否则
// 用户自建/克隆音色会被误伤。
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

// OpenAIVoices 返回别名表的副本（供文档/测试使用）。
func OpenAIVoices() map[string]string {
	out := make(map[string]string, len(openAIVoices))
	for k, v := range openAIVoices {
		out[k] = v
	}
	return out
}

// ---------- response_format（speech） ----------

// FormatMP3 是上游唯一能产出的音频格式。
const FormatMP3 = "mp3"

// openAISpeechFormats 是 OpenAI /v1/audio/speech 的合法取值。
var openAISpeechFormats = map[string]bool{
	"mp3": true, "opus": true, "aac": true, "flac": true, "wav": true, "pcm": true,
}

// NormalizeSpeechFormat 归一化 response_format。
//
// 返回值：
//
//	format 规整后的格式名（空值按 OpenAI 默认 mp3）
//	native 上游能否原生输出该格式（当前只有 mp3 为 true）
//	ok     是否为 OpenAI 合法取值（false 时调用方应回 400）
//
// 注意 native=false 不代表报错：mp3 之外的格式按"降级为 mp3 + 响应头标注"处理，
// 这是 2026-11-21 与用户确认的取舍（上游 TTS 无 format 参数，转码需要额外依赖）。
func NormalizeSpeechFormat(s string) (format string, native bool, ok bool) {
	f := strings.ToLower(strings.TrimSpace(s))
	if f == "" {
		f = FormatMP3
	}
	if !openAISpeechFormats[f] {
		return f, false, false
	}
	return f, f == FormatMP3, true
}

// ---------- speed ----------

const (
	// OpenAI 允许的 speed 区间。
	OpenAISpeedMin = 0.25
	OpenAISpeedMax = 4.0
	// 上游 synthesize_speech 的 speed 区间（schema 原文 [0.5, 2.0]）。
	UpstreamSpeedMin = 0.5
	UpstreamSpeedMax = 2.0
)

// ClampSpeed 校验并收敛 speed。
//
// 0 视为"未指定"（JSON 缺省与显式 0 不可区分，OpenAI 也不接受 0）。
// 超出 OpenAI 区间 → ok=false（调用方回 400）。
// 在 OpenAI 区间但超出上游区间 → 收敛并置 clamped=true（响应头标注实际值），
// 因为 3.0 这类请求在上游会直接 400，收敛比整条请求失败更有用。
func ClampSpeed(v float64) (speed float64, clamped bool, ok bool) {
	if v == 0 {
		return 0, false, true
	}
	if v < OpenAISpeedMin || v > OpenAISpeedMax {
		return 0, false, false
	}
	switch {
	case v < UpstreamSpeedMin:
		return UpstreamSpeedMin, true, true
	case v > UpstreamSpeedMax:
		return UpstreamSpeedMax, true, true
	default:
		return v, false, true
	}
}

// ---------- response_format（transcriptions） ----------

const (
	TranscribeJSON        = "json"
	TranscribeText        = "text"
	TranscribeVerboseJSON = "verbose_json"
	TranscribeSRT         = "srt"
	TranscribeVTT         = "vtt"
	// TranscribeDiarizedJSON 是 2026 版 OpenAI 的说话人分离格式。
	// 上游没有 diarization，网关降级成 json + 空 segments（字段在，能力不在）。
	TranscribeDiarizedJSON = "diarized_json"
)

var openAITranscribeFormats = map[string]bool{
	TranscribeJSON: true, TranscribeText: true, TranscribeVerboseJSON: true,
	TranscribeSRT: true, TranscribeVTT: true, TranscribeDiarizedJSON: true,
}

// NormalizeTranscriptionFormat 归一化 response_format，空值按 OpenAI 默认 json。
//
// ok=false 表示不是 OpenAI 的合法取值（400 invalid_request）。
// srt / vtt 属于合法取值但上游做不到 —— 用 NeedsTimestamps 判定后单独报错，
// 与"写了乱值"区分开，错误信息才能说到点上。
func NormalizeTranscriptionFormat(s string) (format string, ok bool) {
	f := strings.ToLower(strings.TrimSpace(s))
	if f == "" {
		return TranscribeJSON, true
	}
	return f, openAITranscribeFormats[f]
}

// NeedsTimestamps 判定该格式是否要求逐句时间戳（上游 listen_audio 不提供）。
func NeedsTimestamps(format string) bool {
	return format == TranscribeSRT || format == TranscribeVTT
}
