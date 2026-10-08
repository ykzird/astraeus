package streaming

import (
	"strings"
	"testing"
)

func TestClassifyDynamicRange(t *testing.T) {
	t.Parallel()

	tests := []struct {
		transfer string
		want     DynamicRange
	}{
		{"smpte2084", RangeHDR10},
		{"SMPTE2084", RangeHDR10},
		{" smpte2084 ", RangeHDR10},
		{"arib-std-b67", RangeHLG},
		{"bt709", RangeSDR},
		{"bt2020-10", RangeSDR},
		{"", RangeSDR},
		{"unknown", RangeSDR},
	}

	for _, tt := range tests {
		if got := classifyDynamicRange(tt.transfer); got != tt.want {
			t.Errorf("classifyDynamicRange(%q) = %q, want %q", tt.transfer, got, tt.want)
		}
	}
}

// TestClassifyDynamicRange_DoesNotGuessFromWideGamut pins the conservative
// choice: a 10-bit BT.2020 master with an SDR transfer is not HDR, and tone
// mapping it would visibly reduce the contrast of a correct picture.
func TestClassifyDynamicRange_DoesNotGuessFromWideGamut(t *testing.T) {
	t.Parallel()

	if got := classifyDynamicRange("bt2020-10"); got != RangeSDR {
		t.Errorf("bt2020-10 classified as %q; wide gamut alone must not mean HDR", got)
	}
}

func TestDolbyVisionFromSideData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		entries       []ffprobeSideData
		wantProfile   int
		wantHDR10Base bool
	}{
		{
			name: "profile 8 with an HDR10 base layer",
			entries: []ffprobeSideData{{
				SideDataType:               "DOVI configuration record",
				DolbyVisionProfile:         8,
				DolbyVisionBLCompatibility: 1,
			}},
			wantProfile:   8,
			wantHDR10Base: true,
		},
		{
			name: "profile 5 has no HDR10 base layer",
			entries: []ffprobeSideData{{
				SideDataType:       "DOVI configuration record",
				DolbyVisionProfile: 5,
			}},
			wantProfile:   5,
			wantHDR10Base: false,
		},
		{
			name:    "no side data at all",
			entries: nil,
		},
		{
			name: "unrelated side data is ignored",
			entries: []ffprobeSideData{
				{SideDataType: "Mastering display metadata"},
				{SideDataType: "Content light level metadata"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			profile, hdr10 := dolbyVisionFromSideData(tt.entries)
			if profile != tt.wantProfile || hdr10 != tt.wantHDR10Base {
				t.Errorf("dolbyVisionFromSideData = (%d, %v), want (%d, %v)",
					profile, hdr10, tt.wantProfile, tt.wantHDR10Base)
			}
		})
	}
}

// hdrProbeOutput is the shape ffprobe returns for one of the real 4K films in
// the development library: 10-bit HEVC, PQ, with a Dolby Vision profile 8
// record. It is the fixture the whole dynamic-range path was written against.
func hdrProbeOutput() ffprobeOutput {
	var stream ffprobeStream
	stream.CodecName = "hevc"
	stream.CodecType = "video"
	stream.Width = 3840
	stream.Height = 1640
	stream.PixFmt = "yuv420p10le"
	stream.ColorSpace = "bt2020nc"
	stream.ColorTransfer = "smpte2084"
	stream.ColorPrimaries = "bt2020"
	stream.SideDataList = []ffprobeSideData{{
		SideDataType:               "DOVI configuration record",
		DolbyVisionProfile:         8,
		DolbyVisionBLCompatibility: 1,
	}}

	var audio ffprobeStream
	audio.CodecName = "eac3"
	audio.CodecType = "audio"
	audio.Channels = 6

	output := ffprobeOutput{Streams: []ffprobeStream{stream, audio}}
	output.Format.FormatName = "matroska,webm"
	output.Format.Duration = "7025.024"
	output.Format.BitRate = "38000000"
	return output
}

func TestMediaInfoFromProbe_ReadsDynamicRangeAndDolbyVision(t *testing.T) {
	t.Parallel()

	info, err := mediaInfoFromProbe(hdrProbeOutput(), "/media/film.mkv")
	if err != nil {
		t.Fatalf("mediaInfoFromProbe: %v", err)
	}

	if info.DynamicRange != RangeHDR10 {
		t.Errorf("dynamic range = %q, want %q", info.DynamicRange, RangeHDR10)
	}
	if !info.IsHDR() {
		t.Error("a PQ source must report IsHDR")
	}
	if info.DolbyVisionProfile != 8 {
		t.Errorf("Dolby Vision profile = %d, want 8", info.DolbyVisionProfile)
	}
	if !info.DolbyVisionBaseLayerHDR10 {
		t.Error("profile 8 with compatibility id 1 has an HDR10 base layer")
	}
	if info.ColorTransfer != "smpte2084" || info.ColorPrimaries != "bt2020" || info.ColorSpace != "bt2020nc" {
		t.Errorf("colour tags = %q/%q/%q, want smpte2084/bt2020/bt2020nc",
			info.ColorTransfer, info.ColorPrimaries, info.ColorSpace)
	}
	if info.BitDepth != 10 {
		t.Errorf("bit depth = %d, want 10", info.BitDepth)
	}
	if info.Container != "matroska" {
		t.Errorf("container = %q, want matroska", info.Container)
	}
}

// TestMediaInfoFromProbe_SkipsCoverArt guards the rule that the first *real*
// video stream wins: an attached picture is a JPEG poster, and probing it would
// report the film as an 8-bit mjpeg at 1000x1500.
func TestMediaInfoFromProbe_SkipsCoverArt(t *testing.T) {
	t.Parallel()

	var cover ffprobeStream
	cover.CodecName = "mjpeg"
	cover.CodecType = "video"
	cover.Width = 1000
	cover.Height = 1500
	cover.PixFmt = "yuvj420p"
	cover.Disposition.AttachedPic = 1

	output := hdrProbeOutput()
	output.Streams = append([]ffprobeStream{cover}, output.Streams...)

	info, err := mediaInfoFromProbe(output, "/media/film.mkv")
	if err != nil {
		t.Fatalf("mediaInfoFromProbe: %v", err)
	}
	if info.VideoCodec != "hevc" {
		t.Errorf("video codec = %q, want hevc - the cover art is not the feature", info.VideoCodec)
	}
	if info.DynamicRange != RangeHDR10 {
		t.Errorf("dynamic range = %q, want %q", info.DynamicRange, RangeHDR10)
	}
}

func TestMediaInfoFromProbe_RejectsAFileWithNoVideo(t *testing.T) {
	t.Parallel()

	var audio ffprobeStream
	audio.CodecName = "aac"
	audio.CodecType = "audio"

	output := ffprobeOutput{Streams: []ffprobeStream{audio}}
	if _, err := mediaInfoFromProbe(output, "/media/audio-only.mkv"); err == nil {
		t.Fatal("expected an error for a file with no video stream")
	} else if !strings.Contains(err.Error(), "no playable video stream") {
		t.Errorf("error = %v, want it to name the missing video stream", err)
	}
}

func TestMediaInfoIsHDR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		info *MediaInfo
		want bool
	}{
		{&MediaInfo{DynamicRange: RangeHDR10}, true},
		{&MediaInfo{DynamicRange: RangeHLG}, true},
		{&MediaInfo{DynamicRange: RangeSDR, BitDepth: 10}, false},
		{&MediaInfo{}, false},
		{nil, false},
	}

	for _, tt := range tests {
		if got := tt.info.IsHDR(); got != tt.want {
			t.Errorf("IsHDR(%+v) = %v, want %v", tt.info, got, tt.want)
		}
	}
}
