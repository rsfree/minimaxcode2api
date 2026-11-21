package audioproto

import (
	"encoding/binary"
	"fmt"
	"time"
)

// 音频时长与字幕生成。
//
// 为什么网关要自己算时长：OpenAI 的 srt / vtt 需要逐句时间轴，而上游
// listen_audio 的响应里连 duration 都没有（实测只有 transcription 一个字段）。
// 网关能做到的"最诚实"的近似是：把整段转写当成**一条字幕**，时间轴取
// 0 → 音频真实时长。时长从上传字节里解析，不额外打上游。
//
// 覆盖 mp3 / wav / mp4(m4a) 三种容器；其余容器算不出时长时由调用方明确报错，
// 不编一个假的结束时间。

// MediaDuration 解析音频时长。第二个返回值为 false 表示无法从该容器读取。
func MediaDuration(b []byte) (time.Duration, bool) {
	switch {
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WAVE":
		return wavDuration(b)
	case len(b) >= 3 && (string(b[:3]) == "ID3" || (b[0] == 0xFF && b[1]&0xE0 == 0xE0)):
		return mp3Duration(b)
	case len(b) >= 12 && (string(b[4:8]) == "ftyp" || string(b[4:8]) == "moov"):
		return mp4Duration(b)
	default:
		return 0, false
	}
}

// ---------- WAV ----------

func wavDuration(b []byte) (time.Duration, bool) {
	var byteRate, dataSize int
	for i := 12; i+8 <= len(b); {
		id := string(b[i : i+4])
		size := int(binary.LittleEndian.Uint32(b[i+4 : i+8]))
		body := i + 8
		switch id {
		case "fmt ":
			// byteRate 在 fmt chunk 的第 8..12 字节（sampleRate*channels*bits/8）。
			if body+12 <= len(b) {
				byteRate = int(binary.LittleEndian.Uint32(b[body+8 : body+12]))
			}
		case "data":
			dataSize = size
			if dataSize <= 0 || body+dataSize > len(b) {
				dataSize = len(b) - body
			}
			if byteRate > 0 && dataSize > 0 {
				return time.Duration(float64(dataSize) / float64(byteRate) * float64(time.Second)), true
			}
		}
		step := size + (size & 1) // chunk 按偶数字节对齐
		if step < 0 || body+step <= i {
			break
		}
		i = body + step
	}
	return 0, false
}

// ---------- MP3 ----------

var (
	mpeg1BitrateL3   = [15]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320}
	mpeg2BitrateL3   = [15]int{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160}
	mpeg1SampleRate  = [3]int{44100, 48000, 32000}
	mpeg2SampleRate  = [3]int{22050, 24000, 16000}
	mpeg25SampleRate = [3]int{11025, 12000, 8000}
)

// mp3Duration 按帧累加（天然兼容 VBR）。
//
// 只解析 Layer III 的帧头；遇到非帧字节就前移一格重新找同步字，
// 连续 4KB 找不到帧才放弃（防止把尾部 ID3v1 / 垃圾数据扫成天文数字）。
func mp3Duration(b []byte) (time.Duration, bool) {
	i := 0
	if len(b) >= 10 && string(b[:3]) == "ID3" {
		size := int(b[6]&0x7f)<<21 | int(b[7]&0x7f)<<14 | int(b[8]&0x7f)<<7 | int(b[9]&0x7f)
		i = 10 + size
		if b[5]&0x10 != 0 {
			i += 10 // footer
		}
	}
	var total float64
	frames, miss := 0, 0
	for i+4 <= len(b) {
		if b[i] != 0xFF || b[i+1]&0xE0 != 0xE0 {
			i++
			miss++
			if frames > 0 && miss > 4096 {
				break
			}
			continue
		}
		ver := (b[i+1] >> 3) & 0x03
		layer := (b[i+1] >> 1) & 0x03
		brIdx := int(b[i+2] >> 4)
		srIdx := int((b[i+2] >> 2) & 0x03)
		if layer != 0x01 || ver == 0x01 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
			i++
			miss++
			continue
		}
		var bitrate, sampleRate, samples, frameLen int
		switch ver {
		case 3: // MPEG1
			bitrate = mpeg1BitrateL3[brIdx] * 1000
			sampleRate = mpeg1SampleRate[srIdx]
			samples = 1152
			frameLen = 144*bitrate/sampleRate + int((b[i+2]>>1)&0x01)
		case 2: // MPEG2
			bitrate = mpeg2BitrateL3[brIdx] * 1000
			sampleRate = mpeg2SampleRate[srIdx]
			samples = 576
			frameLen = 72*bitrate/sampleRate + int((b[i+2]>>1)&0x01)
		case 0: // MPEG2.5
			bitrate = mpeg2BitrateL3[brIdx] * 1000
			sampleRate = mpeg25SampleRate[srIdx]
			samples = 576
			frameLen = 72*bitrate/sampleRate + int((b[i+2]>>1)&0x01)
		default:
			i++
			continue
		}
		if frameLen <= 0 {
			i++
			continue
		}
		total += float64(samples) / float64(sampleRate)
		frames++
		miss = 0
		i += frameLen
	}
	if frames < 2 {
		return 0, false
	}
	return time.Duration(total * float64(time.Second)), true
}

// ---------- MP4 / M4A ----------

func mp4Duration(b []byte) (time.Duration, bool) {
	moov, ok := findBox(b, "moov")
	if !ok {
		return 0, false
	}
	mvhd, ok := findBox(moov, "mvhd")
	if !ok || len(mvhd) < 20 {
		return 0, false
	}
	var timescale uint32
	var duration uint64
	if mvhd[0] == 1 {
		if len(mvhd) < 32 {
			return 0, false
		}
		timescale = binary.BigEndian.Uint32(mvhd[20:24])
		duration = binary.BigEndian.Uint64(mvhd[24:32])
	} else {
		timescale = binary.BigEndian.Uint32(mvhd[12:16])
		duration = uint64(binary.BigEndian.Uint32(mvhd[16:20]))
	}
	if timescale == 0 || duration == 0 {
		return 0, false
	}
	return time.Duration(float64(duration) / float64(timescale) * float64(time.Second)), true
}

// findBox 在 ISO BMFF 的 box 序列里找第一个指定类型的 box，返回其内容（不含头）。
func findBox(b []byte, typ string) ([]byte, bool) {
	for i := 0; i+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[i : i+4]))
		kind := string(b[i+4 : i+8])
		hdr := 8
		if size == 1 { // 64 位长度
			if i+16 > len(b) {
				return nil, false
			}
			size = int(binary.BigEndian.Uint64(b[i+8 : i+16]))
			hdr = 16
		} else if size == 0 {
			size = len(b) - i
		}
		if size < hdr || i+size > len(b) {
			return nil, false
		}
		if kind == typ {
			return b[i+hdr : i+size], true
		}
		i += size
	}
	return nil, false
}

// ---------- 字幕 ----------

// Subtitles 把整段文本渲染成单条字幕（SRT / VTT）。
//
// 时间轴是 0 → 音频真实时长：这是上游没有逐句时间戳时唯一不撒谎的近似。
// 调用方必须先确认 MediaDuration 成功，否则不要在 dur<=0 的情况下调用。
func Subtitles(format, text string, dur time.Duration) string {
	if dur <= 0 {
		dur = time.Second
	}
	text = trimTrailingNewlines(text)
	switch format {
	case TranscribeSRT:
		return "1\n" + timestamp(0, true) + " --> " + timestamp(dur, true) + "\n" + text + "\n"
	case TranscribeVTT:
		return "WEBVTT\n\n" + timestamp(0, false) + " --> " + timestamp(dur, false) + "\n" + text + "\n"
	default:
		return text
	}
}

func timestamp(d time.Duration, comma bool) string {
	ms := d.Milliseconds()
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms %= 3_600_000
	m := ms / 60_000
	ms %= 60_000
	s := ms / 1000
	ms %= 1000
	sep := "."
	if comma {
		sep = ","
	}
	return fmt.Sprintf("%02d:%02d:%02d%s%03d", h, m, s, sep, ms)
}

func trimTrailingNewlines(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
