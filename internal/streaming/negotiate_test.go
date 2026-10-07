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
			wantReasonSubstr: "1080p",
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
