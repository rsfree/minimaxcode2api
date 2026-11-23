package audioproto

import "testing"

func TestResolveVoice(t *testing.T) {
	cases := []struct {
		in     string
		wantID string
		mapped bool
	}{
		{"alloy", "male-qn-qingse", true},
		{"Nova", "female-shaonv", true},
		{"ONYX", "male-qn-badao", true},
		{"  shimmer  ", "female-tianmei", true},
		{"ballad", "male-qn-badao-jingpin", true},
		{"cedar", "male-qn-jingying-jingpin", true},
		// 上游音色原样透传，且大小写保持（voice_id 大小写敏感）。
		{"female-yujie", "female-yujie", false},
		{"English_Graceful_Lady", "English_Graceful_Lady", false},
		{"", "", false},
		{"   ", "", false},
	}
	for _, c := range cases {
		id, mapped := ResolveVoice(c.in)
		if id != c.wantID || mapped != c.mapped {
			t.Fatalf("ResolveVoice(%q) = %q,%v，期望 %q,%v", c.in, id, mapped, c.wantID, c.mapped)
		}
	}
}

func TestOpenAIVoicesIntegrity(t *testing.T) {
	// 映射目标必须落在上游 8 个基础预置音色（含 -jingpin 变体）内：
	// 拼错一个 id 上游会静默回落默认音色，从响应上根本看不出来。
	allowed := map[string]bool{
		"male-qn-qingse": true, "male-qn-jingying": true,
		"male-qn-badao": true, "male-qn-daxuesheng": true,
		"female-shaonv": true, "female-yujie": true,
		"female-chengshu": true, "female-tianmei": true,
	}
	if n := len(OpenAIVoices()); n != 13 {
		t.Fatalf("OpenAI 别名数 = %d，期望 13", n)
	}
	for alias, id := range OpenAIVoices() {
		if alias != lower(alias) {
			t.Fatalf("别名 %q 必须是小写", alias)
		}
		base := id
		if len(base) > len("-jingpin") && base[len(base)-len("-jingpin"):] == "-jingpin" {
			base = base[:len(base)-len("-jingpin")]
		}
		if !allowed[base] {
			t.Fatalf("别名 %q 映射到未知音色 %q", alias, id)
		}
	}
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 'a' - 'A'
		}
	}
	return string(b)
}

func TestSupportedSpeechFormat(t *testing.T) {
	cases := []struct {
		in  string
		out string
		ok  bool
	}{
		{"", "mp3", true},
		{"mp3", "mp3", true},
		{"MP3", "mp3", true},
		// OpenAI 合法但本部署未声明：必须 400，不能降级成 mp3。
		{"wav", "wav", false},
		{"pcm", "pcm", false},
		{"opus", "opus", false},
		{"aac", "aac", false},
		{"flac", "flac", false},
		{"ogg", "ogg", false},
	}
	for _, c := range cases {
		out, ok := SupportedSpeechFormat(c.in)
		if out != c.out || ok != c.ok {
			t.Fatalf("SupportedSpeechFormat(%q) = %q,%v，期望 %q,%v", c.in, out, ok, c.out, c.ok)
		}
	}
}

func TestValidateSpeed(t *testing.T) {
	cases := []struct {
		in float64
		v  float64
		ok bool
	}{
		{0, 0, true},
		{1, 1, true},
		{0.5, 0.5, true},
		{2, 2, true},
		// 超出声明区间：不接受、不钳位。
		{0.25, 0, false},
		{0.4, 0, false},
		{2.1, 0, false},
		{3, 0, false},
		{4, 0, false},
		{-1, 0, false},
	}
	for _, c := range cases {
		v, ok := ValidateSpeed(c.in)
		if v != c.v || ok != c.ok {
			t.Fatalf("ValidateSpeed(%v) = %v,%v，期望 %v,%v", c.in, v, ok, c.v, c.ok)
		}
	}
}

func TestValidateExtensions(t *testing.T) {
	if !ValidateVolume(0) || !ValidateVolume(10) || ValidateVolume(-0.1) || ValidateVolume(10.1) {
		t.Fatal("volume 边界校验错误")
	}
	if !ValidatePitch(-12) || !ValidatePitch(12) || ValidatePitch(-13) || ValidatePitch(13) {
		t.Fatal("pitch 边界校验错误")
	}
	for _, s := range []string{"", "happy", "SAD", "neutral"} {
		if !ValidateEmotion(s) {
			t.Fatalf("emotion %q 应合法", s)
		}
	}
	for _, s := range []string{"excited", "calm"} {
		if ValidateEmotion(s) {
			t.Fatalf("emotion %q 应拒绝", s)
		}
	}
}

func TestTranscriptionFormats(t *testing.T) {
	known := map[string]bool{
		"":              true,
		"json":          true,
		"TEXT":          true,
		"verbose_json":  true,
		"srt":           true,
		"vtt":           true,
		"diarized_json": true,
		"yaml":          false,
	}
	for in, wantKnown := range known {
		format, ok := NormalizeTranscriptionFormat(in)
		if ok != wantKnown {
			t.Fatalf("NormalizeTranscriptionFormat(%q) known = %v，期望 %v", in, ok, wantKnown)
		}
		if wantKnown && !SupportedTranscriptionFormat(format) {
			switch format {
			case TranscribeSRT, TranscribeVTT, TranscribeDiarizedJSON:
				// 合法但本部署不支持，符合预期。
			default:
				t.Fatalf("%q 应被声明为支持", format)
			}
		}
	}
}
