package streaming

import (
	"strings"
	"testing"
)

func TestNegotiate(t *testing.T) {
	t.Parallel()

	browser := BrowserCapability()

	tests := []struct {
		name             string
		info             *MediaInfo
		capability       ClientCapability
		wantMode         PlaybackMode
		wantVideoAction  Action
		wantAudioAction  Action
		wantVideoCodec   string
		wantAudioCodec   string
		wantHeight       int
		wantDeliverable  bool
		wantReasonSubstr string
	}{
		{
			name:            "fully compatible source plays directly",
			info:            &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080},
			capability:      browser,
			wantMode:        ModeDirectPlay,
			wantVideoAction: ActionCopy,
			wantAudioAction: ActionCopy,
			wantDeliverable: true,
		},
		{
			name:             "incompatible container is remuxed without re-encoding",
			info:             &MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080},
			capability:       browser,
			wantMode:         ModeRemux,
			wantVideoAction:  ActionCopy,
			wantAudioAction:  ActionCopy,
			wantDeliverable:  true,
			wantReasonSubstr: "container",
		},
		{
			name:            "unsupported video codec forces a video transcode only",
			info:            &MediaInfo{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac", Width: 3840, Height: 2160},
			capability:      browser,
			wantMode:        ModeTranscode,
			wantVideoAction: ActionTranscode,
			wantAudioAction: ActionCopy,
			wantVideoCodec:  "h264",
			wantDeliverable: true,
		},
		{
			name:            "unsupported audio codec forces an audio transcode only",
			info:            &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "dts", Width: 1920, Height: 1080},
			capability:      browser,
			wantMode:        ModeTranscode,
			wantVideoAction: ActionCopy,
			wantAudioAction: ActionTranscode,
			wantAudioCodec:  "aac",
			wantDeliverable: true,
		},
		{
			name:             "resolution above the client limit triggers a downscale",
			info:             &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 3840, Height: 2160},
			capability:       ClientCapability{Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, MaxHeight: 1080, SupportsHLS: true},
			wantMode:         ModeTranscode,
			wantVideoAction:  ActionTranscode,
			wantAudioAction:  ActionCopy,
			wantVideoCodec:   "h264",
			wantHeight:       1080,
			wantDeliverable:  true,
			wantReasonSubstr: "1080",
		},
		{
			name:            "silent video needs no audio handling",
			info:            &MediaInfo{Container: "mp4", VideoCodec: "h264", Width: 1280, Height: 720},
			capability:      browser,
			wantMode:        ModeDirectPlay,
			wantVideoAction: ActionCopy,
			wantAudioAction: ActionNone,
			wantDeliverable: true,
		},
		{
			name:             "client without HLS is told the stream is undeliverable",
			info:             &MediaInfo{Container: "matroska", VideoCodec: "h264", AudioCodec: "aac"},
			capability:       ClientCapability{Containers: []string{"mp4"}, VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}, SupportsHLS: false},
			wantMode:         ModeRemux,
			wantDeliverable:  false,
			wantReasonSubstr: "HLS",
		},
		{
			name:            "unknown source codec is treated as incompatible",
			info:            &MediaInfo{Container: "mp4", VideoCodec: "", AudioCodec: "aac"},
			capability:      browser,
			wantMode:        ModeTranscode,
			wantVideoAction: ActionTranscode,
			wantDeliverable: true,
		},
		{
			name:            "nil media info is not deliverable",
			info:            nil,
			capability:      browser,
			wantDeliverable: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			decision := Negotiate(tt.info, tt.capability)

			if decision.Mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", decision.Mode, tt.wantMode)
			}
			if decision.Deliverable != tt.wantDeliverable {
				t.Errorf("deliverable = %v, want %v (reasons: %v)", decision.Deliverable, tt.wantDeliverable, decision.Reasons)
			}
			if tt.wantVideoAction != "" && decision.VideoAction != tt.wantVideoAction {
				t.Errorf("video action = %q, want %q", decision.VideoAction, tt.wantVideoAction)
			}
			if tt.wantAudioAction != "" && decision.AudioAction != tt.wantAudioAction {
				t.Errorf("audio action = %q, want %q", decision.AudioAction, tt.wantAudioAction)
			}
			if tt.wantVideoCodec != "" && decision.TargetVideoCodec != tt.wantVideoCodec {
				t.Errorf("target video codec = %q, want %q", decision.TargetVideoCodec, tt.wantVideoCodec)
			}
			if tt.wantAudioCodec != "" && decision.TargetAudioCodec != tt.wantAudioCodec {
				t.Errorf("target audio codec = %q, want %q", decision.TargetAudioCodec, tt.wantAudioCodec)
			}
			if tt.wantHeight != 0 && decision.TargetHeight != tt.wantHeight {
				t.Errorf("target height = %d, want %d", decision.TargetHeight, tt.wantHeight)
			}
			if len(decision.Reasons) == 0 {
				t.Error("a decision must always explain itself")
			}
			if tt.wantReasonSubstr != "" {
				joined := strings.Join(decision.Reasons, "; ")
				if !strings.Contains(joined, tt.wantReasonSubstr) {
					t.Errorf("reasons %q do not mention %q", joined, tt.wantReasonSubstr)
				}
			}
		})
	}
}

func TestNegotiate_DirectPlayKeepsTheSourceContainer(t *testing.T) {
	t.Parallel()

	decision := Negotiate(
		&MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac"},
		BrowserCapability(),
	)
	if decision.Container != "mp4" {
		t.Errorf("container = %q, want the source container mp4", decision.Container)
	}
}

func TestNegotiate_SegmentedModesUseHLS(t *testing.T) {
	t.Parallel()

	for _, info := range []*MediaInfo{
		{Container: "matroska", VideoCodec: "h264", AudioCodec: "aac"},
		{Container: "mp4", VideoCodec: "hevc", AudioCodec: "aac"},
	} {
		decision := Negotiate(info, BrowserCapability())
		if decision.Mode == ModeDirectPlay {
			t.Fatalf("expected a segmented mode for %+v", info)
		}
		if decision.Container != "hls" {
			t.Errorf("container = %q, want hls", decision.Container)
		}
	}
}

func TestParseEncoders(t *testing.T) {
	t.Parallel()

	// A trimmed but realistic `ffmpeg -encoders` excerpt.
	output := `
Encoders:
 V..... = Video
 ------
 V....D libx264              libx264 H.264 / AVC / MPEG-4 AVC (codec h264)
 V....D libx265              libx265 H.265 / HEVC (codec hevc)
 V..... h264_qsv             H.264 video encoder (codec h264)
 V..... h264_vaapi           H.264/AVC (VAAPI) (codec h264)
 V....D libvpx-vp9           libvpx VP9 (codec vp9)
 A....D aac                  AAC (Advanced Audio Coding)
`

	encoders := ParseEncoders(output)
	want := []string{"aac", "h264_qsv", "h264_vaapi", "libvpx-vp9", "libx264", "libx265"}

	if len(encoders) != len(want) {
		t.Fatalf("encoders = %v, want %v", encoders, want)
	}
	for i := range want {
		if encoders[i] != want[i] {
			t.Errorf("encoder[%d] = %q, want %q", i, encoders[i], want[i])
		}
	}
}

func TestEncoderFor(t *testing.T) {
	t.Parallel()

	software := ServerCapability{VideoEncoders: []string{"libx264", "libx265"}}
	quickSync := ServerCapability{VideoEncoders: []string{"h264_qsv", "hevc_qsv", "libx264"}, HardwareAcceleration: "qsv"}
	vaapiOnly := ServerCapability{VideoEncoders: []string{"h264_vaapi"}, HardwareAcceleration: "vaapi"}
	// An encoder can be compiled into ffmpeg without a usable device being
	// present; that must not be mistaken for a working hardware path.
	compiledButUnusable := ServerCapability{VideoEncoders: []string{"h264_qsv", "libx264"}}
	none := ServerCapability{}

	tests := []struct {
		name   string
		codec  string
		server ServerCapability
		want   string
	}{
		{name: "software h264", codec: "h264", server: software, want: "libx264"},
		{name: "quick sync is preferred when available", codec: "h264", server: quickSync, want: "h264_qsv"},
		{name: "quick sync hevc", codec: "hevc", server: quickSync, want: "hevc_qsv"},
		{name: "vaapi fallback", codec: "h264", server: vaapiOnly, want: "h264_vaapi"},
		{name: "hardware is skipped for a codec it cannot do", codec: "vp9", server: quickSync, want: "libvpx-vp9"},
		{name: "compiled-in hardware without a device falls back to software", codec: "h264", server: compiledButUnusable, want: "libx264"},
		{name: "unknown codec", codec: "theora", server: software, want: ""},
		{name: "nothing available", codec: "h264", server: none, want: "libx264"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := EncoderFor(tt.codec, tt.server); got != tt.want {
				t.Errorf("EncoderFor(%q) = %q, want %q", tt.codec, got, tt.want)
			}
		})
	}
}

func TestBuildFFmpegArgs(t *testing.T) {
	t.Parallel()

	software := ManagerConfig{SegmentSeconds: 6, Server: ServerCapability{VideoEncoders: []string{"libx264"}}}
	quickSync := ManagerConfig{SegmentSeconds: 4, Server: ServerCapability{VideoEncoders: []string{"h264_qsv", "libx264"}, HardwareAcceleration: "qsv"}}

	tests := []struct {
		name      string
		decision  Decision
		cfg       ManagerConfig
		wantParts []string
		denyParts []string
		wantErr   bool
	}{
		{
			name: "remux copies both streams",
			decision: Decision{
				Mode: ModeRemux, Deliverable: true,
				VideoAction: ActionCopy, AudioAction: ActionCopy,
			},
			cfg:       software,
			wantParts: []string{"-c:v copy", "-c:a copy", "-f hls", "-hls_time 6", "playlist.m3u8", "seg%05d.ts"},
		},
		{
			name: "transcode picks the software encoder with a scale filter",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "aac", TargetHeight: 720,
			},
			cfg:       software,
			wantParts: []string{"-c:v libx264", "-crf 21", "-vf scale=-2:720", "-c:a aac", "-b:a 192k"},
		},
		{
			name: "transcode prefers quick sync when the host has it",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionCopy,
				TargetVideoCodec: "h264",
			},
			cfg:       quickSync,
			wantParts: []string{"-c:v h264_qsv", "-global_quality 22", "-hls_time 4"},
			denyParts: []string{"libx264"},
		},
		{
			name: "silent output disables audio mapping",
			decision: Decision{
				Mode: ModeRemux, Deliverable: true,
				VideoAction: ActionCopy, AudioAction: ActionNone,
			},
			cfg:       software,
			wantParts: []string{"-an"},
		},
		{
			name: "direct play has no ffmpeg invocation",
			decision: Decision{
				Mode: ModeDirectPlay, Deliverable: true,
				VideoAction: ActionCopy, AudioAction: ActionCopy,
			},
			cfg:     software,
			wantErr: true,
		},
		{
			name: "unknown target codec is an error, not a broken command",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionCopy,
				TargetVideoCodec: "theora",
			},
			cfg:     software,
			wantErr: true,
		},
		{
			name: "unsupported action is an error",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: Action("explode"), AudioAction: ActionCopy,
			},
			cfg:     software,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			args, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", tt.decision, tt.cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got args %v", args)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildFFmpegArgs: %v", err)
			}

			joined := strings.Join(args, " ")
			for _, part := range tt.wantParts {
				if !strings.Contains(joined, part) {
					t.Errorf("args %q do not contain %q", joined, part)
				}
			}
			for _, part := range tt.denyParts {
				if strings.Contains(joined, part) {
					t.Errorf("args %q unexpectedly contain %q", joined, part)
				}
			}
			if !strings.Contains(joined, "-i /media/movie.mkv") {
				t.Errorf("args %q do not reference the input file", joined)
			}
		})
	}
}

func TestNamedSegmentRe_RejectsTraversal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "playlist", input: "playlist.m3u8", want: true},
		{name: "segment", input: "seg00001.ts", want: true},
		{name: "parent traversal", input: "../astraeus.db", want: false},
		{name: "nested path", input: "sub/seg00001.ts", want: false},
		{name: "absolute path", input: "/etc/passwd", want: false},
		{name: "wrong extension", input: "playlist.m3u8.bak", want: false},
		{name: "segment index not padded", input: "seg1.ts", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NamedSegmentRe.MatchString(tt.input); got != tt.want {
				t.Errorf("MatchString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// TestBuildFFmpegArgs_PinsBrowserCompatiblePixelFormat is the regression test
// for a real 4K 10-bit HDR source that produced 10-bit H.264 "High 10": the
// stream attached to MSE, fetched its segments, and then never played, with no
// error anywhere.
func TestBuildFFmpegArgs_PinsBrowserCompatiblePixelFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		encoder    string
		wantPixFmt string
	}{
		{name: "software h264", encoder: "libx264", wantPixFmt: "yuv420p"},
		{name: "software hevc", encoder: "libx265", wantPixFmt: "yuv420p"},
		{name: "quick sync", encoder: "h264_qsv", wantPixFmt: "nv12"},
		{name: "vaapi", encoder: "h264_vaapi", wantPixFmt: "nv12"},
		{name: "vp9", encoder: "libvpx-vp9", wantPixFmt: "yuv420p"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := ManagerConfig{
				SegmentSeconds: 6,
				Server:         ServerCapability{VideoEncoders: []string{tt.encoder}, HardwareAcceleration: "qsv"},
			}
			if tt.encoder == "h264_vaapi" {
				cfg.Server.HardwareAcceleration = "vaapi"
			}

			args, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
				Mode:        ModeTranscode,
				Deliverable: true,
				VideoAction: ActionTranscode,
				AudioAction: ActionTranscode,
				TargetVideoCodec: map[string]string{
					"libx264": "h264", "h264_qsv": "h264", "h264_vaapi": "h264",
					"libx265": "hevc", "libvpx-vp9": "vp9",
				}[tt.encoder],
				TargetAudioCodec: "aac",
			}, cfg)
			if err != nil {
				t.Fatalf("BuildFFmpegArgs: %v", err)
			}

			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "-pix_fmt "+tt.wantPixFmt) {
				t.Errorf("args do not pin -pix_fmt %s, so a 10-bit source would stay 10-bit:\n%s",
					tt.wantPixFmt, joined)
			}
		})
	}
}

func TestBuildFFmpegArgs_ForcesKeyframesAtSegmentBoundaries(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{
		SegmentSeconds: 4,
		Server:         ServerCapability{VideoEncoders: []string{"libx264"}},
	}

	// A transcode must cut on the requested boundary, otherwise the first
	// segment is a full GOP (~10s) away and playback starts late.
	transcode, err := BuildFFmpegArgs("/tmp/s", "/m.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionCopy, TargetVideoCodec: "h264",
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	if joined := strings.Join(transcode, " "); !strings.Contains(joined, "n_forced*4") {
		t.Errorf("transcode does not force keyframes on the segment boundary:\n%s", joined)
	}

	// A remux cannot re-time keyframes; the source's own are all there is.
	remux, err := BuildFFmpegArgs("/tmp/s", "/m.mkv", Decision{
		Mode: ModeRemux, Deliverable: true,
		VideoAction: ActionCopy, AudioAction: ActionCopy,
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	joined := strings.Join(remux, " ")
	if strings.Contains(joined, "-force_key_frames") {
		t.Errorf("a stream copy must not try to force keyframes:\n%s", joined)
	}
	if strings.Contains(joined, "-pix_fmt") {
		t.Errorf("a stream copy must not touch the pixel format:\n%s", joined)
	}
}

func TestBitDepthFromPixelFormat(t *testing.T) {
	t.Parallel()

	tests := []struct {
		format string
		want   int
	}{
		{format: "yuv420p", want: 8},
		{format: "yuvj420p", want: 8},
		{format: "yuv422p", want: 8},
		{format: "nv12", want: 8}, // the digits are a layout, not a depth
		{format: "nv16", want: 8}, // 8-bit 4:2:2 despite the "16"
		{format: "nv24", want: 8},
		{format: "yuv420p10le", want: 10},
		{format: "yuv422p10le", want: 10},
		{format: "yuv444p12le", want: 12},
		{format: "gray10le", want: 10},
		{format: "p010le", want: 10},
		{format: "p016le", want: 16},
		{format: "", want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.format, func(t *testing.T) {
			t.Parallel()
			if got := bitDepthFromPixelFormat(tt.format); got != tt.want {
				t.Errorf("bitDepthFromPixelFormat(%q) = %d, want %d", tt.format, got, tt.want)
			}
		})
	}
}

// TestNegotiate_DeepColourForcesATranscode covers the case a codec-name-only
// check gets wrong: the browser accepts "h264", but not 10-bit H.264, which it
// will fetch and then silently fail to decode.
func TestNegotiate_DeepColourForcesATranscode(t *testing.T) {
	t.Parallel()

	// 1080p, so that bit depth is the only thing that can force a transcode.
	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, PixelFormat: "yuv420p10le", BitDepth: 10,
	}

	decision := Negotiate(info, BrowserCapability())
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want %q for a 10-bit source", decision.Mode, ModeTranscode)
	}
	if decision.VideoAction != ActionTranscode {
		t.Errorf("video action = %q, want transcode", decision.VideoAction)
	}
	joined := strings.Join(decision.Reasons, "; ")
	if !strings.Contains(joined, "10-bit") {
		t.Errorf("reasons do not mention the bit depth: %s", joined)
	}

	// An 8-bit client that declares it can take 10-bit is served without a
	// re-encode, so the check is a real negotiation and not a blanket refusal.
	capability := BrowserCapability()
	capability.MaxBitDepth = 10
	if got := Negotiate(info, capability).Mode; got == ModeTranscode {
		t.Errorf("mode = %q, want no transcode when the client accepts 10-bit", got)
	}
}

// TestNegotiate_RespectsBothResolutionLimits covers a widescreen source: fitting
// a 2.35:1 film to a 1080-high box makes it about 2530 wide, which still breaks
// a client that declared a 1920 width.
func TestNegotiate_RespectsBothResolutionLimits(t *testing.T) {
	t.Parallel()

	capability := ClientCapability{
		Containers:  []string{"hls"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
		MaxWidth:    1920,
		MaxHeight:   1080,
		SupportsHLS: true,
		MaxBitDepth: 8,
	}

	tests := []struct {
		name       string
		info       *MediaInfo
		wantScale  bool
		wantHeight int
	}{
		{
			name:       "widescreen 4K is limited by width, not height",
			info:       &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 3840, Height: 1640},
			wantScale:  true,
			wantHeight: 820, // 1920 * 1640 / 3840
		},
		{
			name:       "16:9 4K is limited by height",
			info:       &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 3840, Height: 2160},
			wantScale:  true,
			wantHeight: 1080,
		},
		{
			name:      "already inside the box is left alone",
			info:      &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 1280, Height: 720},
			wantScale: false,
		},
		{
			// 1920 * 1600 / 3832 truncates to 801, an odd height, which x264
			// refuses outright: "height not divisible by 2 (1918x801)".
			name:       "a scope source whose width-derived height is odd",
			info:       &MediaInfo{Container: "mp4", VideoCodec: "hevc", AudioCodec: "eac3", Width: 3832, Height: 1600},
			wantScale:  true,
			wantHeight: 800,
		},
		{
			name:       "anamorphic-style source with no limits is untouched",
			info:       &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 2560, Height: 1080},
			wantScale:  true,
			wantHeight: 810, // 1920 * 1080 / 2560
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			decision := Negotiate(tt.info, capability)
			if tt.wantScale {
				if decision.VideoAction != ActionTranscode {
					t.Fatalf("video action = %q, want transcode", decision.VideoAction)
				}
				if decision.TargetHeight != tt.wantHeight {
					t.Errorf("target height = %d, want %d", decision.TargetHeight, tt.wantHeight)
				}
			} else if decision.TargetHeight != 0 {
				t.Errorf("target height = %d, want no scaling", decision.TargetHeight)
			}
		})
	}
}

// TestNegotiate_DownmixesMoreChannelsThanTheClientAccepts covers the failure
// that stopped a real 5.1 film from playing at all: Chromium rejects a 5.1 AAC
// SourceBuffer, and the rejected audio append takes the video down with it.
func TestNegotiate_DownmixesMoreChannelsThanTheClientAccepts(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "ac3",
		Width: 1920, Height: 1080, AudioChannels: 6,
	}

	decision := Negotiate(info, BrowserCapability())
	if decision.AudioAction != ActionTranscode {
		t.Fatalf("audio action = %q, want transcode", decision.AudioAction)
	}
	if decision.TargetAudioChannels != 2 {
		t.Errorf("target audio channels = %d, want 2", decision.TargetAudioChannels)
	}
	if joined := strings.Join(decision.Reasons, "; "); !strings.Contains(joined, "6 channels") {
		t.Errorf("reasons do not mention the channel count: %s", joined)
	}

	// A client that accepts the channels is not downmixed.
	capability := BrowserCapability()
	capability.MaxAudioChannels = 6
	capability.AudioCodecs = []string{"ac3"}
	if got := Negotiate(info, capability).TargetAudioChannels; got != 0 {
		t.Errorf("target audio channels = %d, want no downmix for a client that accepts 5.1", got)
	}
}

func TestBuildFFmpegArgs_DownmixesWhenNegotiated(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{SegmentSeconds: 4, Server: ServerCapability{VideoEncoders: []string{"libx264"}}}

	args, err := BuildFFmpegArgs("/tmp/s", "/m.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac", TargetAudioChannels: 2,
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	if joined := strings.Join(args, " "); !strings.Contains(joined, "-ac 2") {
		t.Errorf("args do not downmix to stereo:\n%s", joined)
	}
}

// TestTargetHeightIsAlwaysEven is the invariant behind the reported failure:
// the scale filter's -2 makes the width even but leaves the height alone, so an
// odd computed height reaches the encoder and x264 refuses to open.
func TestTargetHeightIsAlwaysEven(t *testing.T) {
	t.Parallel()

	capability := ClientCapability{MaxWidth: 1920, MaxHeight: 1080}

	for width := 1000; width <= 4000; width += 7 {
		for height := 400; height <= 2200; height += 6 {
			info := &MediaInfo{Width: width, Height: height}
			got, needed := targetHeightFor(info, capability)
			if !needed {
				continue
			}
			if got%2 != 0 {
				t.Fatalf("targetHeightFor(%dx%d) = %d, which is odd", width, height, got)
			}
			if got > capability.MaxHeight {
				t.Fatalf("targetHeightFor(%dx%d) = %d, above the declared %d",
					width, height, got, capability.MaxHeight)
			}
			if scaled := capability.MaxWidth * got / height; scaled > capability.MaxWidth {
				t.Fatalf("targetHeightFor(%dx%d) = %d yields %d wide, above the declared %d",
					width, height, got, scaled, capability.MaxWidth)
			}
		}
	}
}

// TestBuildFFmpegArgsAt_SeeksOnTheInput covers the offset that makes a quality
// change or a seek into unproduced content resume where the viewer is. -ss must
// come before -i, otherwise ffmpeg decodes everything up to the offset first,
// which on a 4K film means minutes of nothing.
func TestBuildFFmpegArgsAt_SeeksOnTheInput(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{SegmentSeconds: 4, Server: ServerCapability{VideoEncoders: []string{"libx264"}}}
	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}

	args, err := BuildFFmpegArgsAt("/tmp/s", "/media/movie.mkv", decision, cfg, 901.5)
	if err != nil {
		t.Fatalf("BuildFFmpegArgsAt: %v", err)
	}

	seek, input := -1, -1
	for i, arg := range args {
		switch arg {
		case "-ss":
			seek = i
		case "-i":
			input = i
		}
	}
	if seek == -1 {
		t.Fatalf("no -ss in:\n%s", strings.Join(args, " "))
	}
	if seek > input {
		t.Errorf("-ss must precede -i so ffmpeg seeks the input; got -ss at %d, -i at %d", seek, input)
	}
	if args[seek+1] != "901.500" {
		t.Errorf("seek offset = %q, want %q", args[seek+1], "901.500")
	}

	// From the beginning, no -ss at all.
	plain, err := BuildFFmpegArgsAt("/tmp/s", "/media/movie.mkv", decision, cfg, 0)
	if err != nil {
		t.Fatalf("BuildFFmpegArgsAt: %v", err)
	}
	if strings.Contains(strings.Join(plain, " "), "-ss") {
		t.Errorf("a zero offset should not emit -ss:\n%s", strings.Join(plain, " "))
	}
}
