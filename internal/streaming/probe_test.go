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
	audio.BitRate = "448000"

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
	// The audio's own rate is what a client's total-bitrate limit has to leave
	// room for when the audio is being copied.
	if info.AudioBitrateKbps != 448 {
		t.Errorf("audio bitrate = %d, want 448", info.AudioBitrateKbps)
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

// multiTrackInfo is a film with two audio tracks where the *second* is marked
// default - the case that makes "the first stream wins" wrong even when nobody
// asks for anything.
func multiTrackInfo() *MediaInfo {
	return &MediaInfo{
		Container: "matroska", VideoCodec: "h264",
		AudioCodec: "ac3", AudioChannels: 6, AudioBitrateKbps: 448,
		Width: 1920, Height: 1080, BitDepth: 8, BitrateKbps: 8_000,
		AudioTracks: []AudioTrack{
			{Index: 1, Codec: "aac", Channels: 2, BitrateKbps: 128},
			{Index: 3, Codec: "ac3", Channels: 6, BitrateKbps: 448, Language: "eng", Default: true},
		},
	}
}

func TestMediaInfoFromProbe_ReadsEveryAudioTrack(t *testing.T) {
	t.Parallel()

	output := hdrProbeOutput()
	second := ffprobeStream{CodecName: "ac3", CodecType: "audio", Channels: 6, BitRate: "448000"}
	second.Index = 3
	second.Disposition.Default = 1
	second.Tags.Language = "eng"
	output.Streams = append(output.Streams, second)

	info, err := mediaInfoFromProbe(output, "/media/film.mkv")
	if err != nil {
		t.Fatalf("mediaInfoFromProbe: %v", err)
	}
	if len(info.AudioTracks) != 2 {
		t.Fatalf("audio tracks = %+v, want two", info.AudioTracks)
	}
	if info.AudioTracks[0].Codec != "eac3" || info.AudioTracks[1].Codec != "ac3" {
		t.Errorf("track codecs = %q, %q, want eac3 then ac3",
			info.AudioTracks[0].Codec, info.AudioTracks[1].Codec)
	}
	if info.AudioTracks[1].Index != 3 || info.AudioTracks[1].Language != "eng" || !info.AudioTracks[1].Default {
		t.Errorf("second track = %+v, want index 3, eng, default", info.AudioTracks[1])
	}
	// The summary still describes the first track: it is what a client that does
	// not choose sees, and the negotiation is what applies the default.
	if info.AudioCodec != "eac3" || info.AudioBitrateKbps != 448 {
		t.Errorf("summary = %q/%d, want the first track eac3/448", info.AudioCodec, info.AudioBitrateKbps)
	}
}

func TestChosenAudioTrack(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		info        *MediaInfo
		requested   int
		wantIndex   int
		wantCodec   string
		wantHas     bool
		wantIgnored bool
	}{
		{
			name:      "no selection takes the track the file marks default",
			info:      multiTrackInfo(),
			wantIndex: 3, wantCodec: "ac3", wantHas: true,
		},
		{
			name:      "an explicit index is honoured even when it is not the default",
			info:      multiTrackInfo(),
			requested: 1,
			wantIndex: 1, wantCodec: "aac", wantHas: true,
		},
		{
			name:      "an index the file does not have falls back to the default and says so",
			info:      multiTrackInfo(),
			requested: 9,
			wantIndex: 3, wantCodec: "ac3", wantHas: true, wantIgnored: true,
		},
		{
			name:      "a file with no default marked uses the first track",
			info:      &MediaInfo{AudioCodec: "aac", AudioChannels: 2, AudioTracks: []AudioTrack{{Index: 1, Codec: "aac"}}},
			wantIndex: 1, wantCodec: "aac", wantHas: true,
		},
		{
			name:    "a file with no audio has none to choose",
			info:    &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioTracks: []AudioTrack{}},
			wantHas: false,
		},
		{
			name:      "a hand-built MediaInfo with only the summary is one track",
			info:      &MediaInfo{AudioCodec: "aac", AudioChannels: 2, AudioBitrateKbps: 128},
			wantCodec: "aac", wantHas: true,
		},
		{
			name:        "a hand-built MediaInfo cannot satisfy an explicit request",
			info:        &MediaInfo{AudioCodec: "aac", AudioChannels: 2},
			requested:   3,
			wantCodec:   "aac",
			wantHas:     true,
			wantIgnored: true,
		},
		{
			name:    "nil media has no audio",
			info:    nil,
			wantHas: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			track, has, ignored := tt.info.ChosenAudioTrack(tt.requested)
			if has != tt.wantHas {
				t.Fatalf("hasAudio = %v, want %v", has, tt.wantHas)
			}
			if ignored != tt.wantIgnored {
				t.Errorf("ignored = %v, want %v", ignored, tt.wantIgnored)
			}
			if !tt.wantHas {
				return
			}
			if track.Index != tt.wantIndex || track.Codec != tt.wantCodec {
				t.Errorf("track = %+v, want index %d codec %q", track, tt.wantIndex, tt.wantCodec)
			}
		})
	}
}

func TestAudioTrackLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		track AudioTrack
		want  string
	}{
		{track: AudioTrack{Codec: "ac3", Channels: 6, Language: "eng"}, want: "eng ac3 6ch"},
		{track: AudioTrack{Codec: "aac", Channels: 2, Title: "Commentary"}, want: "Commentary aac 2ch"},
		{track: AudioTrack{Codec: "aac", Language: "English (aac)"}, want: "English (aac)"},
		{track: AudioTrack{Codec: "opus"}, want: "opus"},
	}

	for _, tt := range tests {
		if got := AudioTrackLabel(tt.track); got != tt.want {
			t.Errorf("AudioTrackLabel(%+v) = %q, want %q", tt.track, got, tt.want)
		}
	}
}
