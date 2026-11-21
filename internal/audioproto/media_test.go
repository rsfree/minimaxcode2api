package audioproto

import (
	"encoding/binary"
	"testing"
	"time"
)

// mp3Frames 拼 n 个 MPEG1 Layer III 128kbps/44.1kHz 帧（每帧 1152 个采样）。
// 帧长 = 144*128000/44100 = 417 字节，单帧时长 = 1152/44100 ≈ 26.122ms。
func mp3Frames(n int) []byte {
	const frameLen = 417
	out := make([]byte, 0, frameLen*n)
	for i := 0; i < n; i++ {
		f := make([]byte, frameLen)
		copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
		out = append(out, f...)
	}
	return out
}

func wavBytes(sampleRate, channels, bits int, seconds float64) []byte {
	bytesPerSample := bits / 8
	dataSize := int(float64(sampleRate*channels*bytesPerSample) * seconds)
	b := make([]byte, 44+dataSize)
	copy(b[0:], "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(36+dataSize))
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], uint16(channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(b[28:], uint32(sampleRate*channels*bytesPerSample))
	binary.LittleEndian.PutUint16(b[32:], uint16(channels*bytesPerSample))
	binary.LittleEndian.PutUint16(b[34:], uint16(bits))
	copy(b[36:], "data")
	binary.LittleEndian.PutUint32(b[40:], uint32(dataSize))
	return b
}

// mp4Bytes 拼一个最小 ISO BMFF：ftyp + moov/mvhd(timescale=1000, duration=5000)。
func mp4Bytes() []byte {
	mvhd := make([]byte, 100)
	mvhd[0] = 0 // version 0
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], 5000)
	moov := make([]byte, 8+len(mvhd))
	binary.BigEndian.PutUint32(moov[0:], uint32(len(moov)))
	copy(moov[4:], "moov")
	copy(moov[8:], mvhd)
	// mvhd 的 box 头要补在内容前：上面 mvhd 是内容，这里重新拼正确的 box。
	mvhdBox := make([]byte, 8+len(mvhd))
	binary.BigEndian.PutUint32(mvhdBox[0:], uint32(len(mvhdBox)))
	copy(mvhdBox[4:], "mvhd")
	copy(mvhdBox[8:], mvhd)

	moov = make([]byte, 8+len(mvhdBox))
	binary.BigEndian.PutUint32(moov[0:], uint32(len(moov)))
	copy(moov[4:], "moov")
	copy(moov[8:], mvhdBox)

	ftyp := make([]byte, 16)
	binary.BigEndian.PutUint32(ftyp[0:], 16)
	copy(ftyp[4:], "ftyp")
	copy(ftyp[8:], "M4A ")

	return append(ftyp, moov...)
}

func TestMediaDurationMP3(t *testing.T) {
	d, ok := MediaDuration(mp3Frames(10))
	if !ok {
		t.Fatal("MP3 时长解析失败")
	}
	frames := 10
	want := time.Duration(float64(frames*1152) / 44100 * float64(time.Second))
	if diff := d - want; diff > time.Millisecond || diff < -time.Millisecond {
		t.Fatalf("时长 = %v，期望 ≈ %v", d, want)
	}
}

func TestMediaDurationWAV(t *testing.T) {
	d, ok := MediaDuration(wavBytes(16000, 1, 16, 4))
	if !ok {
		t.Fatal("WAV 时长解析失败")
	}
	if diff := d - 4*time.Second; diff > 10*time.Millisecond || diff < -10*time.Millisecond {
		t.Fatalf("时长 = %v，期望 ≈ 4s", d)
	}
}

func TestMediaDurationMP4(t *testing.T) {
	d, ok := MediaDuration(mp4Bytes())
	if !ok {
		t.Fatal("MP4 时长解析失败")
	}
	if d != 5*time.Second {
		t.Fatalf("时长 = %v，期望 5s", d)
	}
}

func TestMediaDurationUnknown(t *testing.T) {
	if _, ok := MediaDuration([]byte("not audio at all")); ok {
		t.Fatal("非音频数据不该解析成功")
	}
	// 单帧 MP3 信息量不足，宁可失败也不报一个不可靠的时长。
	if _, ok := MediaDuration(mp3Frames(1)); ok {
		t.Fatal("单帧 MP3 不该被当作有效时长")
	}
}

func TestSubtitles(t *testing.T) {
	srt := Subtitles(TranscribeSRT, "你好\n世界", 12*time.Second+345*time.Millisecond)
	wantSRT := "1\n00:00:00,000 --> 00:00:12,345\n你好\n世界\n"
	if srt != wantSRT {
		t.Fatalf("SRT = %q，期望 %q", srt, wantSRT)
	}

	vtt := Subtitles(TranscribeVTT, "你好", 2*time.Second)
	wantVTT := "WEBVTT\n\n00:00:00.000 --> 00:00:02.000\n你好\n"
	if vtt != wantVTT {
		t.Fatalf("VTT = %q，期望 %q", vtt, wantVTT)
	}
}
