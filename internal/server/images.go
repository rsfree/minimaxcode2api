package server

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/minimaxcode2api/internal/media"
)

// imageSynthItem 是上游 image_synthesize 的一个槽位。
//
// output_file 只是服务端的槽位名（用返回的 output_file 对应回请求），
// 真正的文件名由上游生成，所以这里按 slot_i 编号。
type imageSynthItem struct {
	Prompt      string   `json:"prompt"`
	OutputFile  string   `json:"output_file"`
	InputURLs   []string `json:"input_urls,omitempty"`
	AspectRatio string   `json:"aspect_ratio,omitempty"`
	Resolution  string   `json:"resolution,omitempty"`
}

type imageSynthResponse struct {
	Summary        string `json:"summary"`
	TotalRequested int    `json:"total_requested"`
	TotalSuccess   int    `json:"total_success"`
	TotalFailed    int    `json:"total_failed"`
	SuccessItems   []struct {
		OutputFile     string `json:"output_file"`
		OutputURL      string `json:"output_url"`
		TextResponse   string `json:"text_response"`
		HasImageOutput bool   `json:"has_image_output"`
		CleanOSSKey    string `json:"clean_oss_key"`
		VisibleOSSKey  string `json:"visible_oss_key"`
	} `json:"success_items"`
	FailedItems []struct {
		OutputFile string `json:"output_file"`
		ErrCode    any    `json:"err_code"`
		ErrorMsg   string `json:"error_msg"`
	} `json:"failed_items"`
}

// imagesRequest 兼容 OpenAI /v1/images/generations 的常用字段，
// 额外接受 MiniMax 原生的 aspect_ratio / resolution / image(参考图)。
type imagesRequest struct {
	Model          string `json:"model"`
	Prompt         string `json:"prompt"`
	N              int    `json:"n"`
	Size           string `json:"size"`
	Quality        string `json:"quality"`
	Style          string `json:"style"`
	ResponseFormat string `json:"response_format"`
	AspectRatio    string `json:"aspect_ratio"`
	Resolution     string `json:"resolution"`
	// Image / ImageURL 作为图生图的参考图，支持公网 URL 与 data URI。
	Image    string `json:"image"`
	ImageURL string `json:"image_url"`
	User     string `json:"user"`
}

// handleImages 处理 OpenAI 图片生成。
//
// 与上游的字段差异：上游一次最多 10 个槽位，槽位名必须由调用方指定
// （output_file），返回时按槽位名对应回来；成功/失败是分开的两个数组，
// 所以部分失败也要能正确回填。
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	var req imagesRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "prompt 不能为空")
		return
	}
	n := req.N
	if n <= 0 {
		n = 1
	}
	if n > 10 {
		n = 10 // 上游单次上限
	}

	ratio, resolution := resolveImageShape(req)
	items := make([]imageSynthItem, 0, n)
	ref := strings.TrimSpace(firstNonEmptyStr(req.ImageURL, req.Image))
	for i := 0; i < n; i++ {
		it := imageSynthItem{
			Prompt:      prompt,
			OutputFile:  "slot_" + strconv.Itoa(i) + ".png",
			AspectRatio: ratio,
			Resolution:  resolution,
		}
		if ref != "" {
			it.InputURLs = []string{ref}
		}
		items = append(items, it)
	}

	raw, err := s.callMedia(r.Context(), media.ToolImageSynthesize, map[string]any{"requests": items})
	if err != nil {
		s.logMedia(r, req.Model, http.StatusInternalServerError, start, err)
		writeMediaError(w, err)
		return
	}

	var resp imageSynthResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", "上游响应无法解析: "+err.Error())
		return
	}

	// 上游 failed_items 里没有 output_url，只有错误码；全失败时把第一条例外透出去。
	if len(resp.SuccessItems) == 0 {
		msg := "图片生成失败"
		if len(resp.FailedItems) > 0 {
			msg = resp.FailedItems[0].ErrorMsg
			if msg == "" {
				msg = "上游未返回任何成功项"
			}
		}
		s.logMedia(r, req.Model, http.StatusBadGateway, start, nil)
		writeOpenAIError(w, http.StatusBadGateway, "generation_failed", msg)
		return
	}

	wantB64 := strings.EqualFold(req.ResponseFormat, "b64_json")
	type datum struct {
		URL           string `json:"url,omitempty"`
		B64           string `json:"b64_json,omitempty"`
		RevisedPrompt string `json:"revised_prompt,omitempty"`
	}
	data := make([]datum, 0, len(resp.SuccessItems))
	for _, it := range resp.SuccessItems {
		if !it.HasImageOutput || it.OutputURL == "" {
			continue
		}
		d := datum{RevisedPrompt: prompt}
		if wantB64 {
			body, mime, ferr := s.fetchBytes(r.Context(), it.OutputURL)
			if ferr != nil {
				writeOpenAIError(w, http.StatusBadGateway, "download_failed", ferr.Error())
				return
			}
			_ = mime
			d.B64 = base64.StdEncoding.EncodeToString(body)
		} else {
			d.URL = it.OutputURL
		}
		data = append(data, d)
	}
	if len(data) == 0 {
		writeOpenAIError(w, http.StatusBadGateway, "generation_failed", "上游称成功但没有产物 URL")
		return
	}

	s.logMedia(r, req.Model, http.StatusOK, start, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"created": time.Now().Unix(),
		"data":    data,
	})
}

// resolveImageShape 从 size/quality 推出上游的 aspect_ratio + resolution 档位。
//
// 上游只认 1K/2K/4K 三档，不认具体像素；所以这里做一次"降级映射"：
// 按像素比找最近的合法比例，按最长边选档位。返回的档位只影响生成质量，
// 实际输出像素由上游决定（实测 2K+16:9 = 2752x1536）。
func resolveImageShape(req imagesRequest) (string, string) {
	ratio := strings.TrimSpace(req.AspectRatio)
	resolution := strings.ToUpper(strings.TrimSpace(req.Resolution))

	if ratio == "" {
		ratio = ratioFromSize(req.Size)
	}
	if ratio == "" {
		ratio = "1:1"
	}
	if resolution == "" {
		switch strings.ToLower(strings.TrimSpace(req.Quality)) {
		case "hd", "high", "2k":
			resolution = "2K"
		case "4k", "ultra":
			resolution = "4K"
		default:
			resolution = "1K"
		}
	}
	switch resolution {
	case "1K", "2K", "4K":
	default:
		resolution = "1K"
	}
	return ratio, resolution
}

func ratioFromSize(size string) string {
	parts := strings.SplitN(strings.ToLower(strings.TrimSpace(size)), "x", 2)
	if len(parts) != 2 {
		return ""
	}
	w, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	h, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || w <= 0 || h <= 0 {
		return ""
	}
	allowed := []struct {
		label string
		value float64
	}{
		{"21:9", 21.0 / 9.0}, {"16:9", 16.0 / 9.0}, {"4:3", 4.0 / 3.0},
		{"1:1", 1}, {"3:4", 3.0 / 4.0}, {"9:16", 9.0 / 16.0},
	}
	target := math.Log(float64(w) / float64(h))
	best, bestDiff := "1:1", math.MaxFloat64
	for _, a := range allowed {
		if d := math.Abs(math.Log(a.value) - target); d < bestDiff {
			best, bestDiff = a.label, d
		}
	}
	return best
}

// logMedia 记一条请求日志（媒体调用没有 token 用量，只记路径与耗时）。
func (s *Server) logMedia(r *http.Request, model string, status int, start time.Time, err error) {
	entry := reqlogEntry(r, model, status, start, err)
	s.Logs.Add(entry)
}

// handleVoices 暴露可用音色（面板与 TTS 客户端用来选 voice）。
func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	raw, err := s.callMedia(r.Context(), media.ToolVoiceList, map[string]any{})
	if err != nil {
		writeMediaError(w, err)
		return
	}
	var resp struct {
		AvailableVoices []struct {
			VoiceID   string `json:"voice_id"`
			VoiceName string `json:"voice_name"`
		} `json:"available_voices"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "bad_upstream", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(resp.AvailableVoices))
	for _, v := range resp.AvailableVoices {
		out = append(out, map[string]any{"id": v.VoiceID, "name": v.VoiceName, "object": "voice"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}
