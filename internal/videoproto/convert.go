// Package videoproto 实现「异步视频生成协议」的 canonical 契约。
//
// 上游（MiniMax Code）是另一种方言：prompt 单字符串 + input_image_path +
// reference_*_paths，没有 content[]、没有 role、没有六态。本包只做纯函数转换：
//
//	client request  --Decode/Validate/Build-->  upstream payload
//	upstream result --MapStatus/TaskObject-->   canonical task object
//
// 契约见 ~/.workbuddy/skills/video-adapter（assets/video-protocol.schema.json）。
// 关键纪律：
//   - POST 200 只代表「受理」，创建响应**只能有 id**；
//   - content.video_url 只在 succeeded 时有值；
//   - 计费相关字段（resolution/duration）错值必须 400，绝不静默回落默认；
//   - 参数双载体（body 字段 vs prompt 后缀）必须归一到一种，只发强校验。
package videoproto

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// 上游模型名。
const (
	UpstreamH3     = "MiniMax-H3"
	UpstreamHailuo = "MiniMax-Hailuo-2.3"
)

// 六态。
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusExpired   = "expired"
	StatusCancelled = "cancelled"
)

// URLRef 是 {"url": "..."} 信封。
type URLRef struct {
	URL string `json:"url"`
}

// DraftRef 是 {"id": "..."}。
type DraftRef struct {
	ID string `json:"id"`
}

// ContentItem 是 content[] 的一项：type 选载体，role 选语义位置。
type ContentItem struct {
	Type      string    `json:"type"`
	Text      string    `json:"text,omitempty"`
	ImageURL  *URLRef   `json:"image_url,omitempty"`
	VideoURL  *URLRef   `json:"video_url,omitempty"`
	AudioURL  *URLRef   `json:"audio_url,omitempty"`
	DraftTask *DraftRef `json:"draft_task,omitempty"`
	Role      string    `json:"role,omitempty"`
}

// Request 是 canonical 请求。已知字段强类型，未知字段进 Extra 原样透传。
type Request struct {
	Model                 string         `json:"model"`
	Content               []ContentItem  `json:"content"`
	Resolution            string         `json:"resolution,omitempty"`
	Ratio                 string         `json:"ratio,omitempty"`
	Duration              *int           `json:"duration,omitempty"`
	Frames                *int           `json:"frames,omitempty"`
	GenerateAudio         *bool          `json:"generate_audio,omitempty"`
	Watermark             *bool          `json:"watermark,omitempty"`
	CameraFixed           *bool          `json:"camera_fixed,omitempty"`
	ReturnLastFrame       *bool          `json:"return_last_frame,omitempty"`
	Seed                  *int           `json:"seed,omitempty"`
	OutputFormat          string         `json:"output_format,omitempty"`
	Draft                 *bool          `json:"draft,omitempty"`
	ServiceTier           string         `json:"service_tier,omitempty"`
	CallbackURL           string         `json:"callback_url,omitempty"`
	ExecutionExpiresAfter *int           `json:"execution_expires_after,omitempty"`
	Priority              *int           `json:"priority,omitempty"`
	SafetyIdentifier      string         `json:"safety_identifier,omitempty"`
	Tools                 []any          `json:"tools,omitempty"`
	OmniReferenceTaskType string         `json:"omni_reference_task_type,omitempty"`
	Extra                 map[string]any `json:"-"`
}

// ProtocolError 是协议自有的四键错误信封携带的语义。
// type: invalid_request_error / api_error / upstream_error
type ProtocolError struct {
	Status  int
	Message string
	Type    string
	Param   string
	Code    string
}

func (e *ProtocolError) Error() string { return e.Message }

func invalid(param, code, format string, args ...any) *ProtocolError {
	msg := fmt.Sprintf(format, args...)
	return &ProtocolError{
		Status:  400,
		Message: msg,
		Type:    "invalid_request_error",
		Param:   param,
		Code:    code,
	}
}

var knownKeys = map[string]bool{
	"model": true, "content": true, "resolution": true, "ratio": true,
	"duration": true, "frames": true, "generate_audio": true, "watermark": true,
	"camera_fixed": true, "return_last_frame": true, "seed": true,
	"output_format": true, "draft": true, "service_tier": true,
	"callback_url": true, "execution_expires_after": true, "priority": true,
	"safety_identifier": true, "tools": true, "omni_reference_task_type": true,
}

// Decode 解析请求体，并把未知键收进 Extra（vendor extras 必须透传）。
func Decode(raw []byte) (*Request, *ProtocolError) {
	var probe any
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, invalid("", "invalid_json", "请求体不是合法 JSON: %v", err)
	}
	if _, ok := probe.(map[string]any); !ok {
		return nil, invalid("", "invalid_request", "请求体必须是 JSON 对象")
	}

	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, invalid("", "invalid_request", "请求体结构不合法: %v", err)
	}

	var all map[string]any
	_ = json.Unmarshal(raw, &all)
	for k, v := range all {
		if knownKeys[k] {
			continue
		}
		if req.Extra == nil {
			req.Extra = map[string]any{}
		}
		req.Extra[k] = v
	}
	return &req, nil
}

// Validate 按协议规定的判定顺序校验：content → model → 场景互斥 → 参数取值域。
func (r *Request) Validate() *ProtocolError {
	// 1) content
	if len(r.Content) == 0 {
		return invalid("content", "invalid_request", "'content' 必须至少包含一项")
	}
	for i, it := range r.Content {
		switch it.Type {
		case "text":
			// 空串是「无所谓」表达，后面归一化时删掉；这里只在整项都是空文本时判空。
			if strings.TrimSpace(it.Text) == "" {
				continue
			}
		case "image_url":
			if it.ImageURL == nil || strings.TrimSpace(it.ImageURL.URL) == "" {
				return invalid(fmt.Sprintf("content[%d].image_url.url", i), "invalid_request",
					"content[%d] 的 image_url.url 不能为空", i)
			}
		case "video_url":
			if it.VideoURL == nil || strings.TrimSpace(it.VideoURL.URL) == "" {
				return invalid(fmt.Sprintf("content[%d].video_url.url", i), "invalid_request",
					"content[%d] 的 video_url.url 不能为空", i)
			}
			if it.Role != "reference_video" {
				return invalid(fmt.Sprintf("content[%d].role", i), "invalid_request",
					"video_url 的 role 必须是 reference_video")
			}
		case "audio_url":
			if it.AudioURL == nil || strings.TrimSpace(it.AudioURL.URL) == "" {
				return invalid(fmt.Sprintf("content[%d].audio_url.url", i), "invalid_request",
					"content[%d] 的 audio_url.url 不能为空", i)
			}
			if it.Role != "reference_audio" {
				return invalid(fmt.Sprintf("content[%d].role", i), "invalid_request",
					"audio_url 的 role 必须是 reference_audio")
			}
		case "draft_task":
			if it.DraftTask == nil || strings.TrimSpace(it.DraftTask.ID) == "" {
				return invalid(fmt.Sprintf("content[%d].draft_task.id", i), "invalid_request",
					"content[%d] 的 draft_task.id 不能为空", i)
			}
		default:
			return invalid(fmt.Sprintf("content[%d].type", i), "invalid_request",
				"content[%d] 的 type 不支持: %q（只接受 text/image_url/video_url/audio_url/draft_task）", i, it.Type)
		}
	}

	// 2) model
	if strings.TrimSpace(r.Model) == "" {
		return invalid("model", "invalid_request", "'model' 不能为空")
	}

	// 3) 场景互斥：首帧/首尾帧 vs 全模态参考
	var frames, refs int
	for _, it := range r.Content {
		if it.Type != "image_url" {
			continue
		}
		switch it.Role {
		case "reference_image":
			refs++
		case "first_frame", "last_frame":
			frames++
		default:
			frames++ // 省略 role 只能理解为单图首帧
		}
	}
	if refs > 0 && frames > 0 {
		return invalid("content", "invalid_request",
			"首帧/首尾帧与全模态参考不能混用：请把图片统一标成 reference_image，或只用 first_frame/last_frame")
	}

	// 4) 参数取值域（计费相关，错值必须响亮）
	if r.Frames != nil {
		return invalid("frames", "unsupported_parameter",
			"当前上游不支持 frames，请改用 duration")
	}
	if r.Duration != nil && *r.Duration < -1 {
		return invalid("duration", "invalid_request", "duration 必须 >= -1")
	}
	if r.Resolution != "" {
		switch r.Resolution {
		case "480p", "720p", "1080p", "4k":
		default:
			return invalid("resolution", "invalid_request",
				"resolution 只接受 480p/720p/1080p/4k，收到 %q", r.Resolution)
		}
	}
	if r.Ratio != "" {
		switch r.Ratio {
		case "16:9", "4:3", "1:1", "3:4", "9:16", "21:9", "adaptive":
		default:
			return invalid("ratio", "invalid_request",
				"ratio 只接受 16:9/4:3/1:1/3:4/9:16/21:9/adaptive，收到 %q", r.Ratio)
		}
	}
	if r.OutputFormat != "" && r.OutputFormat != "mp4" && r.OutputFormat != "mov" {
		return invalid("output_format", "invalid_request", "output_format 只接受 mp4/mov")
	}
	if r.ServiceTier != "" && r.ServiceTier != "default" && r.ServiceTier != "flex" {
		return invalid("service_tier", "invalid_request", "service_tier 只接受 default/flex")
	}
	return nil
}

// Build 把 canonical 请求折成上游提交体。
//
// 返回的 warnings 是「客户端写了但上游不支持/被覆盖」的字段。它们必须回给调用方，
// 静默丢弃会让调用方以为参数生效了 —— 这是协议里点名的最坏处置。
func (r *Request) Build() (payload map[string]any, upstreamModel string, warnings []string, perr *ProtocolError) {
	upstreamModel, ok := ResolveModel(r.Model)
	if !ok {
		return nil, "", nil, invalid("model", "unsupported_model",
			"不支持的 model %q；可用：%s、%s（或别名 h3 / h2.3）", r.Model, UpstreamH3, UpstreamHailuo)
	}

	// 文本：拼成上游的单段 prompt。后缀参数在这里剥离并回填。
	var texts []string
	for _, it := range r.Content {
		if it.Type != "text" {
			continue
		}
		cleaned, params, warns := ParseSuffix(it.Text)
		warnings = append(warnings, warns...)
		if strings.TrimSpace(cleaned) != "" {
			texts = append(texts, cleaned)
		}
		// 后缀回填：body 已有值则以 body 为准（强校验优先）。
		for k, v := range params {
			if w := r.applySuffix(k, v); w != "" {
				warnings = append(warnings, w)
			}
		}
	}
	prompt := strings.TrimSpace(strings.Join(texts, "\n\n"))
	if prompt == "" {
		return nil, "", nil, invalid("content", "invalid_request",
			"上游要求至少一段非空文本提示词（content[] 里的 type=text）")
	}

	p := map[string]any{"model": upstreamModel, "prompt": prompt}

	// 图片：按 role 分流到首帧 / 尾帧 / 参考图。
	var firstFrame, lastFrame string
	var refImages []string
	var refVideos, refAudios []string
	for _, it := range r.Content {
		if it.Type != "image_url" {
			continue
		}
		u := it.ImageURL.URL
		switch it.Role {
		case "reference_image":
			refImages = append(refImages, u)
		case "last_frame":
			lastFrame = u
		case "first_frame":
			if firstFrame != "" {
				warnings = append(warnings, "content 里给了多张 first_frame，只采用第一张")
				continue
			}
			firstFrame = u
		default:
			if firstFrame != "" {
				// 多图省略 role => 语义含糊，协议建议拒绝而不是猜。
				return nil, "", nil, invalid("content", "invalid_request",
					"多张图片必须显式给 role（first_frame / last_frame / reference_image），不能省略")
			}
			firstFrame = u
		}
	}
	for _, it := range r.Content {
		switch it.Type {
		case "video_url":
			refVideos = append(refVideos, it.VideoURL.URL)
		case "audio_url":
			refAudios = append(refAudios, it.AudioURL.URL)
		case "draft_task":
			return nil, "", nil, invalid("content", "unsupported_parameter",
				"上游不支持 draft_task（样片任务引用）")
		}
	}

	if firstFrame != "" {
		p["input_image_path"] = firstFrame
	}
	if lastFrame != "" {
		if firstFrame == "" {
			return nil, "", nil, invalid("content", "invalid_request",
				"给了 last_frame 就必须同时给 first_frame")
		}
		p["last_frame_image_path"] = lastFrame
	}
	if len(refImages) > 0 {
		if upstreamModel != UpstreamH3 {
			return nil, "", nil, invalid("content", "unsupported_parameter",
				"%s 不支持 reference_image，只有 %s 支持全模态参考", upstreamModel, UpstreamH3)
		}
		p["reference_image_paths"] = refImages
	}
	if len(refVideos) > 0 {
		if upstreamModel != UpstreamH3 {
			return nil, "", nil, invalid("content", "unsupported_parameter",
				"%s 不支持 reference_video", upstreamModel)
		}
		// 上游对公网 URL 要求同时给时长；拿不到就别猜。
		if d, ok := r.Extra["reference_video_duration_seconds"]; ok {
			p["reference_video_duration_seconds"] = d
		} else {
			return nil, "", nil, invalid("content", "unsupported_parameter",
				"引用公网视频时必须额外提供 reference_video_duration_seconds（上游用它做校验，网关无法可靠探测远程时长）")
		}
		p["reference_video_paths"] = refVideos
	}
	if len(refAudios) > 0 {
		if upstreamModel != UpstreamH3 {
			return nil, "", nil, invalid("content", "unsupported_parameter",
				"%s 不支持 reference_audio", upstreamModel)
		}
		if len(refImages) == 0 && len(refVideos) == 0 {
			return nil, "", nil, invalid("content", "invalid_request",
				"reference_audio 不能单独使用，必须配合 reference_image 或 reference_video")
		}
		p["reference_audio_paths"] = refAudios
	}
	if firstFrame != "" && (len(refImages) > 0 || len(refVideos) > 0 || len(refAudios) > 0) {
		return nil, "", nil, invalid("content", "invalid_request",
			"首帧/首尾帧与全模态参考不能混用")
	}

	// 分辨率：档位不是像素，换算必须在合法档位内，绝不回落默认。
	if r.Resolution != "" {
		mapped, warn, perr := mapResolution(upstreamModel, r.Resolution)
		if perr != nil {
			return nil, "", nil, perr
		}
		p["resolution"] = mapped
		if warn != "" {
			warnings = append(warnings, warn)
		}
	}
	if r.Ratio != "" {
		if upstreamModel == UpstreamH3 && r.Ratio == "adaptive" &&
			firstFrame == "" && len(refImages) == 0 && len(refVideos) == 0 {
			return nil, "", nil, invalid("ratio", "invalid_request",
				"%s 的文生视频必须给具体比例（21:9/16:9/4:3/1:1/3:4/9:16），不能用 adaptive", upstreamModel)
		}
		p["ratio"] = r.Ratio
	}
	if r.Duration != nil {
		d := *r.Duration
		if d == -1 {
			return nil, "", nil, invalid("duration", "unsupported_parameter",
				"当前上游不支持 duration=-1（模型自选时长），请给具体秒数")
		}
		if perr := validateDuration(upstreamModel, d); perr != nil {
			return nil, "", nil, perr
		}
		p["duration"] = d
	}
	if r.Watermark != nil {
		p["aigc_watermark"] = *r.Watermark
	}

	// 上游没有的能力：记 warning 后丢弃（不静默）。
	warnUnsupported := func(cond bool, name, note string) {
		if cond {
			warnings = append(warnings, name+" 上游不支持，已忽略"+note)
		}
	}
	warnUnsupported(r.GenerateAudio != nil, "generate_audio",
		"（"+upstreamModel+" 的音频由模型/提示词决定，无独立开关）")
	warnUnsupported(r.CameraFixed != nil, "camera_fixed", "")
	warnUnsupported(r.ReturnLastFrame != nil, "return_last_frame", "")
	warnUnsupported(r.Seed != nil, "seed", "")
	warnUnsupported(r.OutputFormat != "", "output_format", "")
	warnUnsupported(r.Draft != nil, "draft", "")
	warnUnsupported(r.ServiceTier != "", "service_tier", "")
	warnUnsupported(r.ExecutionExpiresAfter != nil, "execution_expires_after", "")
	warnUnsupported(r.Priority != nil, "priority", "")
	warnUnsupported(r.SafetyIdentifier != "", "safety_identifier", "")
	warnUnsupported(len(r.Tools) > 0, "tools", "")
	warnUnsupported(r.OmniReferenceTaskType != "", "omni_reference_task_type", "")
	warnUnsupported(r.CallbackURL != "", "callback_url",
		"（本网关暂不做回调推送，请用 GET 轮询）")

	// 未知 vendor extra 原样透传，除我们已消费的时长字段。
	for k, v := range r.Extra {
		if k == "reference_video_duration_seconds" {
			continue
		}
		if _, exists := p[k]; exists {
			continue
		}
		p[k] = v
	}
	sort.Strings(warnings)
	return p, upstreamModel, warnings, nil
}

// applySuffix 把一个后缀参数写回请求；返回非空表示需要提示的 warning。
func (r *Request) applySuffix(key, val string) string {
	conflict := func(name string) string {
		return "prompt 后缀的 " + key + " 被 body 字段 " + name + " 覆盖（强校验优先）"
	}
	switch key {
	case "resolution":
		if r.Resolution != "" {
			return conflict("resolution")
		}
		r.Resolution = strings.ToLower(val)
	case "ratio":
		if r.Ratio != "" {
			return conflict("ratio")
		}
		r.Ratio = val
	case "duration":
		if r.Duration != nil {
			return conflict("duration")
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return "prompt 后缀 --dur 的值不是整数: " + val
		}
		r.Duration = &n
	case "watermark":
		if r.Watermark != nil {
			return conflict("watermark")
		}
		b := strings.EqualFold(val, "true") || val == "1"
		r.Watermark = &b
	case "camera_fixed":
		if r.CameraFixed != nil {
			return conflict("camera_fixed")
		}
		b := strings.EqualFold(val, "true") || val == "1"
		r.CameraFixed = &b
	case "seed":
		if r.Seed != nil {
			return conflict("seed")
		}
		n, err := strconv.Atoi(val)
		if err != nil {
			return "prompt 后缀 --seed 的值不是整数: " + val
		}
		r.Seed = &n
	}
	return ""
}

// suffixAlias 把后缀短名与全名都归一到一个 canonical 字段名。
var suffixAlias = map[string]string{
	"rs": "resolution", "resolution": "resolution",
	"rt": "ratio", "ratio": "ratio",
	"dur": "duration", "duration": "duration",
	"cf": "camera_fixed", "camerafixed": "camera_fixed",
	"wm": "watermark", "watermark": "watermark",
	"seed": "seed",
}

// ParseSuffix 解析并剥离 prompt 尾部的 --rs/--rt/--dur 参数。
//
// 只认「第一个以 -- 开头的 token 起到结尾」这一段；前面是正文。
// 不认识的 flag 记 warning 并丢弃 —— 但绝不把后缀留在 prompt 里，
// 否则模型会把它当画面文字读出来。
func ParseSuffix(text string) (clean string, params map[string]string, warnings []string) {
	fields := strings.Fields(text)
	start := -1
	for i, f := range fields {
		if strings.HasPrefix(f, "--") {
			start = i
			break
		}
	}
	if start < 0 {
		return text, nil, nil
	}
	params = map[string]string{}
	// 尾部若没有完整成对，说明是畸形后缀：整段剥离并告警。
	bad := false
	for i := start; i < len(fields); i++ {
		tok := fields[i]
		if !strings.HasPrefix(tok, "--") {
			bad = true
			continue
		}
		name := strings.ToLower(strings.TrimLeft(tok, "-"))
		key, ok := suffixAlias[name]
		if !ok {
			warnings = append(warnings, "未识别的 prompt 后缀: "+tok+"（已从提示词中剔除）")
			// 若下一 token 是值，跳过它
			if i+1 < len(fields) && !strings.HasPrefix(fields[i+1], "--") {
				i++
			}
			continue
		}
		if i+1 >= len(fields) || strings.HasPrefix(fields[i+1], "--") {
			warnings = append(warnings, "prompt 后缀 "+tok+" 缺少取值（已剔除）")
			continue
		}
		params[key] = fields[i+1]
		i++
	}
	if bad {
		warnings = append(warnings, "prompt 后缀包含无法解析的片段，已整段剥离")
	}
	return strings.TrimSpace(strings.Join(fields[:start], " ")), params, warnings
}

// ResolveModel 把客户端模型名映射成上游模型。
func ResolveModel(m string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "minimax-h3", "h3", "minimax-h3-max", "h3-max":
		return UpstreamH3, true
	case "minimax-hailuo-2.3", "hailuo-2.3", "h2.3", "minimax-hailuo-23", "hailuo":
		return UpstreamHailuo, true
	}
	return "", false
}

// mapResolution 把 canonical 档位换成上游档位。
// 上游只有 768P/1080P/2K，480p 没有对应档且换档会改变账单 —— 直接拒绝。
func mapResolution(model, want string) (mapped string, warning string, perr *ProtocolError) {
	switch want {
	case "480p":
		return "", "", invalid("resolution", "unsupported_parameter",
			"上游最低档是 768P，没有 480p；请改用 720p")
	case "720p":
		return "768P", "", nil
	case "1080p":
		if model == UpstreamH3 {
			return "2K", "1080p 已换算为 " + UpstreamH3 + " 支持的 2K 档", nil
		}
		return "1080P", "", nil
	case "4k":
		if model == UpstreamH3 {
			return "2K", "4k 超出 " + UpstreamH3 + " 上限，已换算为 2K", nil
		}
		return "1080P", "4k 超出 " + UpstreamHailuo + " 上限，已换算为 1080P", nil
	}
	return "", "", nil
}

func validateDuration(model string, d int) *ProtocolError {
	if model == UpstreamH3 {
		if d < 4 || d > 15 {
			return invalid("duration", "invalid_request",
				"%s 的 duration 支持 4-15 秒，收到 %d", UpstreamH3, d)
		}
		return nil
	}
	if d != 6 && d != 10 {
		return invalid("duration", "invalid_request",
			"%s 的 duration 只支持 6 或 10 秒，收到 %d", UpstreamHailuo, d)
	}
	return nil
}

// MapStatus 把上游状态收敛成六态。未知值一律当 running（非终态），
// 绝不当成 succeeded —— 否则会提前落库、提前释放槽位。
func MapStatus(raw string) (status string, unnormalized bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "queued", "pending", "created":
		return StatusQueued, false
	case "running", "processing", "in_progress":
		return StatusRunning, false
	case "succeeded", "success", "completed":
		return StatusSucceeded, false
	case "failed", "failure", "error":
		return StatusFailed, false
	case "expired", "timeout", "timed_out":
		return StatusExpired, false
	case "cancelled", "canceled":
		return StatusCancelled, false
	}
	return StatusRunning, true
}

// IsTerminal 判断是否终态。
func IsTerminal(status string) bool {
	switch status {
	case StatusSucceeded, StatusFailed, StatusExpired, StatusCancelled:
		return true
	}
	return false
}

// NormalizeDataURI 把 data URI 的媒体类型段转小写（协议要求小写格式段）。
func NormalizeDataURI(s string) string {
	i := strings.Index(s, ";base64,")
	if !strings.HasPrefix(s, "data:") || i < 0 {
		return s
	}
	head := s[:i]
	if j := strings.Index(head, "/"); j >= 0 {
		head = head[:j+1] + strings.ToLower(head[j+1:])
	}
	return head + s[i:]
}
