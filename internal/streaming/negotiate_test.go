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
	quickSync := ServerCapability{VideoEncoders: []string{"h264_qsv", "hevc_qsv", "libx264", "libvpx-vp9"}, HardwareAcceleration: []string{"qsv"}}
	vaapiOnly := ServerCapability{VideoEncoders: []string{"h264_vaapi"}, HardwareAcceleration: []string{"vaapi"}}
	// An encoder can be compiled into ffmpeg without a usable device being
	// present. Detection rejects it and records why, so it is absent from
	// VideoEncoders - which is the only list selection reads, and the reason a
	// rejected encoder cannot be chosen by mistake.
	rejectedHardware := ServerCapability{
		VideoEncoders: []string{"libx264"},
		RejectedEncoders: []EncoderRejection{
			{Encoder: "h264_qsv", Reason: "Error creating a MFX session: -9."},
		},
	}
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
		{name: "rejected hardware falls back to software", codec: "h264", server: rejectedHardware, want: "libx264"},
		{name: "unknown codec", codec: "theora", server: software, want: ""},
		{name: "nothing available", codec: "h264", server: none, want: ""},
		{name: "a software encoder that is not present is never assumed", codec: "h264", server: ServerCapability{VideoEncoders: []string{"libx265"}}, want: ""},
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

// TestNegotiateForServer_ResolvesTheAudioEncoder is the regression test for S-7
// at the point where the two names are actually distinguished.
//
// Negotiate is pure and only knows codec names, so it leaves TargetAudioEncoder
// empty. NegotiateForServer has the server capability and is the only place that
// can turn "the client wants Opus" into "run libopus" - and until this was
// fixed it checked the encoder and then threw the answer away, so the session
// passed the codec name to -c:a and ffmpeg refused to run its experimental
// native encoder.
func TestNegotiateForServer_ResolvesTheAudioEncoder(t *testing.T) {
	t.Parallel()

	server := ServerCapability{
		VideoEncoders: []string{"libx264"},
		AudioEncoders: []string{"aac", "libopus", "libvorbis", "libmp3lame"},
	}

	tests := []struct {
		name      string
		audio     string
		wantCodec string
		wantEnc   string
	}{
		{name: "opus needs libopus", audio: "opus", wantCodec: "opus", wantEnc: "libopus"},
		{name: "vorbis needs libvorbis", audio: "vorbis", wantCodec: "vorbis", wantEnc: "libvorbis"},
		{name: "mp3 needs libmp3lame", audio: "mp3", wantCodec: "mp3", wantEnc: "libmp3lame"},
		{name: "aac is its own encoder", audio: "aac", wantCodec: "aac", wantEnc: "aac"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// A source in a codec the browser cannot copy, so audio and video
			// both transcode.
			info := &MediaInfo{
				Width: 1920, Height: 1080, VideoCodec: "ffv1", AudioCodec: "pcm_s16le",
				Container: "matroska", DurationSeconds: 60,
			}
			capability := ClientCapability{
				Containers: []string{"mp4"}, VideoCodecs: []string{"h264"},
				AudioCodecs: []string{tt.audio}, MaxAudioChannels: 2,
				// Without this the decision is undeliverable and negotiation
				// returns before the audio encoder is resolved at all, which
				// would make this test pass for the wrong reason.
				SupportsHLS: true,
			}

			decision := NegotiateForServer(info, capability, server)
			if decision.AudioAction != ActionTranscode {
				t.Fatalf("audio action = %q, want a transcode", decision.AudioAction)
			}
			if decision.TargetAudioCodec != tt.wantCodec {
				t.Fatalf("audio codec = %q, want %q", decision.TargetAudioCodec, tt.wantCodec)
			}
			if decision.TargetAudioEncoder != tt.wantEnc {
				t.Errorf("audio encoder = %q, want %q: the codec name is not the encoder name",
					decision.TargetAudioEncoder, tt.wantEnc)
			}
		})
	}
}

// TestNegotiate_LeavesTheEncoderUnresolved guards the pure/total split: Negotiate
// has no server to consult, so it must not invent an encoder name.
func TestNegotiate_LeavesTheEncoderUnresolved(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Width: 1920, Height: 1080, VideoCodec: "ffv1", AudioCodec: "pcm_s16le",
		Container: "matroska", DurationSeconds: 60,
	}
	capability := ClientCapability{
		Containers: []string{"mp4"}, VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"opus"}, MaxAudioChannels: 2,
	}

	decision := Negotiate(info, capability)
	if decision.AudioAction == ActionTranscode && decision.TargetAudioEncoder != "" {
		t.Errorf("pure Negotiate resolved %q with no server to check against",
			decision.TargetAudioEncoder)
	}
}

func TestBuildFFmpegArgs(t *testing.T) {
	t.Parallel()

	software := ManagerConfig{SegmentSeconds: 6, Server: ServerCapability{VideoEncoders: []string{"libx264"}}}
	quickSync := ManagerConfig{SegmentSeconds: 4, Server: ServerCapability{VideoEncoders: []string{"h264_qsv", "libx264"}, HardwareAcceleration: []string{"qsv"}}}

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
			// S-15: a forced keyframe on NVENC is not an IDR frame unless the
			// family's own flag says so, and only an IDR frame is a key the HLS
			// muxer cuts at. NVENC spells it with a hyphen.
			name: "nvenc gets its forced-idr spelling",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "aac",
			},
			cfg:       ManagerConfig{SegmentSeconds: 6, Server: ServerCapability{VideoEncoders: []string{"h264_nvenc"}, AudioEncoders: []string{"aac"}}},
			wantParts: []string{"-c:v h264_nvenc", "-forced-idr 1", "-force_key_frames expr:gte(t,n_forced*6)"},
		},
		{
			// QSV and AMF spell the same option with an underscore.
			name: "qsv gets its forced_idr spelling",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "aac",
			},
			cfg:       ManagerConfig{SegmentSeconds: 6, Server: ServerCapability{VideoEncoders: []string{"h264_qsv"}, AudioEncoders: []string{"aac"}}},
			wantParts: []string{"-c:v h264_qsv", "-forced_idr 1"},
			denyParts: []string{"-forced-idr"},
		},
		{
			// The encoders that already cut where they are told must not be
			// given an option they do not know: ffmpeg treats an unknown
			// option as an error, not a no-op.
			name: "software and vaapi get no forced-IDR flag",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "aac",
			},
			cfg:       ManagerConfig{SegmentSeconds: 6, Server: ServerCapability{VideoEncoders: []string{"libx264", "h264_vaapi"}, AudioEncoders: []string{"aac"}}},
			wantParts: []string{"-force_key_frames expr:gte(t,n_forced*6)"},
			denyParts: []string{"-forced-idr", "-forced_idr"},
		},
		{
			// S-7 of the 2026-10-09 review. ffmpeg's "opus" is its native
			// experimental encoder, which refuses to run without -strict -2;
			// the encoder that works is "libopus". NegotiatorForServer now
			// resolves the codec into an encoder and the session passes that.
			name: "an opus target passes the encoder, not the codec name",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "opus",
				TargetAudioEncoder: "libopus",
			},
			cfg:       software,
			wantParts: []string{"-c:a libopus"},
			denyParts: []string{"-c:a opus "},
		},
		{
			name: "a vorbis target passes libvorbis",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "vorbis",
				TargetAudioEncoder: "libvorbis",
			},
			cfg:       software,
			wantParts: []string{"-c:a libvorbis"},
			denyParts: []string{"-c:a vorbis "},
		},
		{
			// A decision built by hand carries no resolved encoder, which is
			// what the pure Negotiate tests produce. The builder has to fall
			// back to the codec's known encoder rather than passing the codec
			// name straight through.
			name: "a hand-built decision with only a codec still gets an encoder",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "opus",
			},
			cfg:       software,
			wantParts: []string{"-c:a libopus"},
			denyParts: []string{"-c:a opus "},
		},
		{
			name: "an aac target is unchanged",
			decision: Decision{
				Mode: ModeTranscode, Deliverable: true,
				VideoAction: ActionTranscode, AudioAction: ActionTranscode,
				TargetVideoCodec: "h264", TargetAudioCodec: "aac",
				TargetAudioEncoder: "aac",
			},
			cfg:       software,
			wantParts: []string{"-c:a aac", "-b:a 192k"},
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
		// The ladder's files. Widening this allowlist is the one change here
		// that could open a hole, so every new shape is pinned from both sides.
		{name: "master playlist", input: "master.m3u8", want: true},
		{name: "variant playlist", input: "playlist_0.m3u8", want: true},
		{name: "two-digit variant", input: "playlist_11.m3u8", want: true},
		{name: "variant segment", input: "seg0_00001.ts", want: true},
		{name: "variant segment, two digits", input: "seg11_00001.ts", want: true},
		{name: "leading slash on a master", input: "/master.m3u8", want: false},
		{name: "traversal in a variant name", input: "../playlist_0.m3u8", want: false},
		{name: "nested variant playlist", input: "sub/playlist_0.m3u8", want: false},
		{name: "three-digit variant", input: "playlist_123.m3u8", want: false},
		{name: "segment without a variant index padded", input: "seg0_1.ts", want: false},
		{name: "playlist that is not a variant", input: "playlist_backup.m3u8", want: false},
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

	// Every family has to end up 8-bit, because no browser decodes 10-bit H.264
	// through Media Source Extensions. Software and most hardware encoders are
	// told directly; VAAPI gets there through its upload filter instead, since
	// it consumes hardware surfaces and a -pix_fmt would ask for software frames.
	tests := []struct {
		name       string
		encoder    string
		codec      string
		wantPixFmt string
	}{
		{name: "software h264", encoder: "libx264", codec: "h264", wantPixFmt: "yuv420p"},
		{name: "software hevc", encoder: "libx265", codec: "hevc", wantPixFmt: "yuv420p"},
		{name: "vp9", encoder: "libvpx-vp9", codec: "vp9", wantPixFmt: "yuv420p"},
		{name: "nvenc", encoder: "h264_nvenc", codec: "h264", wantPixFmt: "nv12"},
		{name: "quick sync", encoder: "h264_qsv", codec: "h264", wantPixFmt: "nv12"},
		{name: "amf", encoder: "h264_amf", codec: "h264", wantPixFmt: "nv12"},
		{name: "videotoolbox", encoder: "h264_videotoolbox", codec: "h264", wantPixFmt: "nv12"},
		{name: "vaapi pins the format in its filter", encoder: "h264_vaapi", codec: "h264", wantPixFmt: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := ManagerConfig{
				SegmentSeconds: 6,
				Server:         ServerCapability{VideoEncoders: []string{tt.encoder}},
			}

			args, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
				Mode:             ModeTranscode,
				Deliverable:      true,
				VideoAction:      ActionTranscode,
				AudioAction:      ActionTranscode,
				TargetVideoCodec: tt.codec,
				TargetAudioCodec: "aac",
			}, cfg)
			if err != nil {
				t.Fatalf("BuildFFmpegArgs: %v", err)
			}

			joined := strings.Join(args, " ")
			if tt.wantPixFmt == "" {
				if strings.Contains(joined, "-pix_fmt") {
					t.Errorf("VAAPI should not be given -pix_fmt; its filter pins the format:\n%s", joined)
				}
				if !strings.Contains(joined, "format=nv12,hwupload") {
					t.Errorf("VAAPI has no upload filter, so it would receive software frames:\n%s", joined)
				}
				return
			}
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

func TestAudioEncoderFor(t *testing.T) {
	t.Parallel()

	server := ServerCapability{AudioEncoders: []string{"aac", "libopus", "libmp3lame"}}

	tests := []struct {
		codec string
		want  string
	}{
		{codec: "aac", want: "aac"},
		{codec: "opus", want: "libopus"},
		{codec: "mp3", want: "libmp3lame"},
		{codec: "ac3", want: ""}, // not offered by this host
		{codec: "totally-bogus", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			t.Parallel()
			if got := AudioEncoderFor(tt.codec, server); got != tt.want {
				t.Errorf("AudioEncoderFor(%q) = %q, want %q", tt.codec, got, tt.want)
			}
		})
	}
}

// TestNegotiateForServer covers the gap the pure negotiation cannot see: the
// client and the media may agree on a codec that this host cannot encode.
func TestNegotiateForServer(t *testing.T) {
	t.Parallel()

	// 4K HEVC that has to be re-encoded for a browser-shaped client.
	info := &MediaInfo{
		Container: "matroska", VideoCodec: "hevc", AudioCodec: "dts",
		Width: 3840, Height: 1600, BitDepth: 10, AudioChannels: 6,
	}

	t.Run("retargets to a codec the host can encode", func(t *testing.T) {
		t.Parallel()

		// The client's most preferred codecs are ones this host cannot encode,
		// but it accepts a lower-preference codec that the host can.
		capability := BrowserCapability()
		capability.VideoCodecs = []string{"hevc", "vp9"}
		capability.AudioCodecs = []string{"ac3", "mp3"}
		server := ServerCapability{
			VideoEncoders: []string{"libvpx-vp9"},
			AudioEncoders: []string{"libmp3lame"},
		}

		decision := NegotiateForServer(info, capability, server)
		if !decision.Deliverable {
			t.Fatalf("undeliverable: %v", decision.Reasons)
		}
		if decision.TargetVideoCodec != "vp9" {
			t.Errorf("target video codec = %q, want vp9", decision.TargetVideoCodec)
		}
		if decision.TargetAudioCodec != "mp3" {
			t.Errorf("target audio codec = %q, want mp3", decision.TargetAudioCodec)
		}
		joined := strings.Join(decision.Reasons, "; ")
		if !strings.Contains(joined, "cannot encode") {
			t.Errorf("the substitution should be explained: %s", joined)
		}
	})

	t.Run("undeliverable when nothing the client accepts can be encoded", func(t *testing.T) {
		t.Parallel()

		capability := BrowserCapability()
		capability.VideoCodecs = []string{"av1"}
		capability.AudioCodecs = []string{"opus"}
		server := ServerCapability{VideoEncoders: []string{"libx264"}, AudioEncoders: []string{"aac"}}

		decision := NegotiateForServer(info, capability, server)
		if decision.Deliverable {
			t.Fatal("a host with no AV1 encoder must not claim it can deliver AV1")
		}
		if joined := strings.Join(decision.Reasons, "; "); !strings.Contains(joined, "no encoder") {
			t.Errorf("the refusal should explain itself: %s", joined)
		}
	})

	t.Run("a decision that needs no transcode is untouched", func(t *testing.T) {
		t.Parallel()

		playable := &MediaInfo{Container: "mp4", VideoCodec: "h264", AudioCodec: "aac", Width: 1920, Height: 1080}
		decision := NegotiateForServer(playable, BrowserCapability(), ServerCapability{})
		if decision.Mode != ModeDirectPlay {
			t.Errorf("mode = %q, want direct play", decision.Mode)
		}
	})
}

func TestValidateRejectsUnknownCodecNames(t *testing.T) {
	t.Parallel()

	valid := ClientCapability{
		Containers:  []string{"hls"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid manifest was rejected: %v", err)
	}

	badVideo := valid
	badVideo.VideoCodecs = []string{"h264", "definitely-not-a-codec"}
	if err := badVideo.Validate(); err == nil {
		t.Error("an unknown video codec name should be refused before it reaches ffmpeg")
	}

	badAudio := valid
	badAudio.AudioCodecs = []string{"totally-bogus"}
	if err := badAudio.Validate(); err == nil {
		t.Error("an unknown audio codec name should be refused before it reaches ffmpeg")
	}

	// A codec this host cannot encode is still a legitimate thing to declare;
	// it is a negotiation outcome, not a malformed request.
	unsupported := valid
	unsupported.VideoCodecs = []string{"av1"}
	if err := unsupported.Validate(); err != nil {
		t.Errorf("a real codec the host cannot encode must not be rejected as invalid: %v", err)
	}
}

// ---- dynamic range ---------------------------------------------------------

// hdrCapability is a client that can render HDR: ten bits or more, and it says
// so. It accepts HLS because most of these cases force a re-encode.
func hdrCapability() ClientCapability {
	return ClientCapability{
		Containers:       []string{"mp4", "hls"},
		VideoCodecs:      []string{"h264", "hevc"},
		AudioCodecs:      []string{"aac", "eac3"},
		MaxWidth:         3840,
		MaxHeight:        2160,
		MaxBitDepth:      10,
		MaxAudioChannels: 6,
		SupportsHDR:      true,
		SupportsHLS:      true,
	}
}

// hdrFilm is one of the real 4K films: 10-bit HEVC, PQ, BT.2020, Dolby Vision
// profile 8 with an HDR10 base layer, in Matroska.
func hdrFilm() *MediaInfo {
	return &MediaInfo{
		Container: "matroska", VideoCodec: "hevc", AudioCodec: "eac3",
		Width: 3840, Height: 1640, PixelFormat: "yuv420p10le", BitDepth: 10,
		ColorSpace: "bt2020nc", ColorTransfer: "smpte2084", ColorPrimaries: "bt2020",
		DynamicRange:              RangeHDR10,
		DolbyVisionProfile:        8,
		DolbyVisionBaseLayerHDR10: true,
	}
}

// TestNegotiate_ToneMapsHDRForAnSDRClient is the case the whole increment is
// about: a 10-bit PQ film and a browser. Before this, the bit-depth rule forced
// a transcode into 8-bit BT.709 with no transfer conversion, and the picture came
// out washed out - visibly wrong, not merely low quality.
func TestNegotiate_ToneMapsHDRForAnSDRClient(t *testing.T) {
	t.Parallel()

	decision := Negotiate(hdrFilm(), BrowserCapability())

	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode", decision.Mode)
	}
	if !decision.ToneMap {
		t.Error("an HDR source delivered to a client without HDR support must be tone mapped")
	}
	if decision.TargetDynamicRange != RangeSDR {
		t.Errorf("target dynamic range = %q, want %q", decision.TargetDynamicRange, RangeSDR)
	}

	reasons := strings.Join(decision.Reasons, "; ")
	if !strings.Contains(reasons, "HDR") {
		t.Errorf("the reasons do not mention HDR, so a surprising decision is undebuggable: %s", reasons)
	}
	if !strings.Contains(reasons, "tone map") {
		t.Errorf("the reasons do not say the picture is tone mapped: %s", reasons)
	}
	// The Dolby Vision base layer is HDR10, so the honest caveat is the loss of
	// the dynamic metadata - not a colour error.
	if !strings.Contains(reasons, "Dolby Vision profile 8") {
		t.Errorf("the reasons do not name the Dolby Vision profile: %s", reasons)
	}
}

// TestNegotiate_SDRDeepColourIsNotToneMapped is the other direction, and the
// reason dynamic range is classified from the transfer function alone: a 10-bit
// BT.2020 SDR master needs an 8-bit SDR output, but tone mapping it would reduce
// the contrast of a picture that was never HDR.
func TestNegotiate_SDRDeepColourIsNotToneMapped(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, PixelFormat: "yuv420p10le", BitDepth: 10,
		ColorSpace: "bt2020nc", ColorTransfer: "bt2020-10", ColorPrimaries: "bt2020",
		DynamicRange: RangeSDR,
	}

	decision := Negotiate(info, BrowserCapability())
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode for 10-bit into an 8-bit client", decision.Mode)
	}
	if decision.ToneMap {
		t.Error("a 10-bit SDR source must not be tone mapped")
	}
	if decision.TargetDynamicRange != RangeSDR {
		t.Errorf("target dynamic range = %q, want %q", decision.TargetDynamicRange, RangeSDR)
	}
}

// TestNegotiate_HDRClientKeepsHDRThroughAReEncode covers a client that can show
// HDR but cannot take this file as it stands - here the container is not one it
// opens, and neither is the audio codec accepted, so the video is re-encoded.
// Converting to SDR would throw away the one thing the client asked for.
func TestNegotiate_HDRClientKeepsHDRThroughAReEncode(t *testing.T) {
	t.Parallel()

	capability := hdrCapability()
	capability.Containers = []string{"hls"}

	info := hdrFilm()
	info.VideoCodec = "vp9" // not in the client's list, so the video is re-encoded
	info.AudioCodec = "aac"

	decision := Negotiate(info, capability)

	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode", decision.Mode)
	}
	if decision.VideoAction != ActionTranscode {
		t.Fatalf("video action = %q, want transcode", decision.VideoAction)
	}
	if decision.ToneMap {
		t.Error("a client that supports HDR must not be handed a tone-mapped picture")
	}
	if decision.TargetDynamicRange != RangeHDR10 {
		t.Errorf("target dynamic range = %q, want %q", decision.TargetDynamicRange, RangeHDR10)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "keeps HDR10") {
		t.Errorf("the reasons should say HDR is kept: %s", strings.Join(decision.Reasons, "; "))
	}
}

// TestNegotiate_HDRClientDirectPlaysTheOriginal checks that an HDR path does not
// cost a re-encode when nothing else requires one: copying the file preserves
// both the HDR and the Dolby Vision metadata exactly.
func TestNegotiate_HDRClientDirectPlaysTheOriginal(t *testing.T) {
	t.Parallel()

	info := hdrFilm()
	info.Container = "mp4"
	info.AudioCodec = "eac3"

	decision := Negotiate(info, hdrCapability())

	if decision.Mode != ModeDirectPlay {
		t.Fatalf("mode = %q, want direct play: %s", decision.Mode, strings.Join(decision.Reasons, "; "))
	}
	if decision.VideoAction != ActionCopy {
		t.Errorf("video action = %q, want copy", decision.VideoAction)
	}
	if decision.ToneMap {
		t.Error("direct play must not tone map")
	}
	if decision.TargetDynamicRange != RangeHDR10 {
		t.Errorf("target dynamic range = %q, want %q", decision.TargetDynamicRange, RangeHDR10)
	}
}

// TestNegotiate_DolbyVisionProfile5IsFlagged covers the stream this server
// cannot convert correctly. Refusing to play it would be worse than playing it
// with approximate colour, but the approximation has to be stated.
func TestNegotiate_DolbyVisionProfile5IsFlagged(t *testing.T) {
	t.Parallel()

	info := hdrFilm()
	info.DolbyVisionProfile = 5
	info.DolbyVisionBaseLayerHDR10 = false

	decision := Negotiate(info, BrowserCapability())
	if !decision.ToneMap {
		t.Fatal("a profile 5 source still has to be tone mapped for an SDR client")
	}

	reasons := strings.Join(decision.Reasons, "; ")
	if !strings.Contains(reasons, "IPTPQc2") {
		t.Errorf("the reasons should name what cannot be converted: %s", reasons)
	}
	if !strings.Contains(reasons, "approximate") {
		t.Errorf("the reasons should say the colour will be approximate: %s", reasons)
	}
}

// TestNegotiateForServer_FallsBackToToneMappingWithoutAVerifiedHDREncoder is the
// honest degradation: a client asks for HDR, the host has no 10-bit encoder that
// was proved to work, and the answer is a playable SDR stream with the reason
// attached rather than a failure or an HDR stream that dies at the first frame.
func TestNegotiateForServer_FallsBackToToneMappingWithoutAVerifiedHDREncoder(t *testing.T) {
	t.Parallel()

	capability := hdrCapability()
	capability.Containers = []string{"hls"}
	info := hdrFilm()
	info.VideoCodec = "vp9"
	info.AudioCodec = "aac"

	server := ServerCapability{VideoEncoders: []string{"libx264", "libx265"}}

	decision := NegotiateForServer(info, capability, server)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode", decision.Mode)
	}
	if decision.TargetDynamicRange != RangeSDR || !decision.ToneMap {
		t.Fatalf("without a verified 10-bit encoder the stream must be tone mapped, got range %q tonemap %v",
			decision.TargetDynamicRange, decision.ToneMap)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "no verified 10-bit encoder") {
		t.Errorf("the fallback must say why HDR was dropped: %s", strings.Join(decision.Reasons, "; "))
	}
	if !decision.Deliverable {
		t.Errorf("the fallback is playable and must stay deliverable: %s", strings.Join(decision.Reasons, "; "))
	}
}

// TestNegotiateForServer_KeepsHDRWhenTheEncoderWasVerified is the other half:
// with a proved 10-bit encoder, the client's request is honoured.
func TestNegotiateForServer_KeepsHDRWhenTheEncoderWasVerified(t *testing.T) {
	t.Parallel()

	capability := hdrCapability()
	capability.Containers = []string{"hls"}
	info := hdrFilm()
	info.VideoCodec = "vp9"
	info.AudioCodec = "aac"

	server := ServerCapability{
		VideoEncoders:    []string{"libx264", "libx265"},
		HDRVideoEncoders: []HDREncoder{{Encoder: "libx265", PixelFormat: "yuv420p10le"}},
	}

	decision := NegotiateForServer(info, capability, server)
	if decision.ToneMap || decision.TargetDynamicRange != RangeHDR10 {
		t.Fatalf("a verified 10-bit encoder should keep HDR, got range %q tonemap %v",
			decision.TargetDynamicRange, decision.ToneMap)
	}
	if decision.TargetVideoCodec != "hevc" {
		t.Errorf("target codec = %q, want hevc", decision.TargetVideoCodec)
	}
}

// hdrClientWithOnlyH264 is the dead end in the other direction: the client can
// show HDR but only decodes H.264, which has no meaningful 10-bit HDR form.
func hdrClientWithOnlyH264() ClientCapability {
	capability := hdrCapability()
	capability.Containers = []string{"hls"}
	capability.VideoCodecs = []string{"h264"}
	return capability
}

// hdrSourceNeedingATranscode is an HDR film the browser profile cannot copy:
// VP9 video and no audio in a container it does not accept, so the decision is
// a real re-encode rather than a copy.
func hdrSourceNeedingATranscode() *MediaInfo {
	info := hdrFilm()
	info.VideoCodec = "vp9"
	info.AudioCodec = "aac"
	return info
}

// hostileHDRServer advertises libx264 as a verified 10-bit HDR encoder, which
// is what this host really reports before the S-6 fix: libx264 does produce
// yuv420p10le when probed. The old test used a server with an empty
// HDRVideoEncoders list, which is exactly the condition that hid the bug.
func hostileHDRServer() ServerCapability {
	return ServerCapability{
		VideoEncoders:    []string{"libx264"},
		AudioEncoders:    []string{"aac"},
		HDRVideoEncoders: []HDREncoder{{Encoder: "libx264", PixelFormat: "yuv420p10le"}},
	}
}

// TestNegotiate_HDRClientWithOnlyH264IsToneMapped is the S-6 regression test at
// the layer that must not depend on the host.
//
// The bug was that pure negotiation kept an HDR range while targeting H.264,
// and only tone mapped by accident when the server capability list happened to
// have no HDR encoders. This asserts the rule on the codec alone, with no
// server in the picture at all.
func TestNegotiate_HDRClientWithOnlyH264IsToneMapped(t *testing.T) {
	t.Parallel()

	decision := Negotiate(hdrSourceNeedingATranscode(), hdrClientWithOnlyH264())
	if decision.VideoAction != ActionTranscode {
		t.Fatalf("video action = %q, want a transcode", decision.VideoAction)
	}
	if decision.TargetVideoCodec != "h264" {
		t.Fatalf("target codec = %q, want h264", decision.TargetVideoCodec)
	}
	if !decision.ToneMap || decision.TargetDynamicRange != RangeSDR {
		t.Errorf("H.264 cannot carry HDR, so this must be tone mapped to SDR: "+
			"tonemap=%v range=%q\nreasons: %s",
			decision.ToneMap, decision.TargetDynamicRange, strings.Join(decision.Reasons, "; "))
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "cannot carry") {
		t.Errorf("the decision should say why the range changed: %s",
			strings.Join(decision.Reasons, "; "))
	}
}

// TestNegotiate_HDRCapableCodecKeepsTheRange is the control: the rule must not
// tone map a decode the client can actually use.
func TestNegotiate_HDRCapableCodecKeepsTheRange(t *testing.T) {
	t.Parallel()

	capability := hdrClientWithOnlyH264()
	capability.VideoCodecs = []string{"h264", "hevc"}

	decision := Negotiate(hdrSourceNeedingATranscode(), capability)
	if decision.TargetVideoCodec != "hevc" {
		t.Fatalf("target codec = %q, want hevc: the HDR-capable codec should win", decision.TargetVideoCodec)
	}
	if decision.ToneMap || decision.TargetDynamicRange != RangeHDR10 {
		t.Errorf("an HDR-capable target must keep the range: tonemap=%v range=%q",
			decision.ToneMap, decision.TargetDynamicRange)
	}
}

// TestNegotiateForServer_HDRClientWithOnlyH264IsToneMapped covers the same dead
// end once a server is in the picture, and adds the case that used to hide the
// bug: a server that lists libx264 as a 10-bit HDR encoder.
func TestNegotiateForServer_HDRClientWithOnlyH264IsToneMapped(t *testing.T) {
	t.Parallel()

	capability := hdrClientWithOnlyH264()
	info := hdrSourceNeedingATranscode()
	server := hostileHDRServer()

	decision := NegotiateForServer(info, capability, server)
	if !decision.Deliverable {
		t.Fatalf("H.264 SDR is still playable and must be delivered: %s",
			strings.Join(decision.Reasons, "; "))
	}
	if !decision.ToneMap || decision.TargetDynamicRange != RangeSDR {
		t.Errorf("with only H.264 available the stream must be tone mapped to SDR, got range %q tonemap %v",
			decision.TargetDynamicRange, decision.ToneMap)
	}
	if decision.TargetVideoCodec != "h264" {
		t.Errorf("target codec = %q, want h264", decision.TargetVideoCodec)
	}
}

// ---- bitrate ---------------------------------------------------------------

// TestNegotiate_BitrateLimitForcesATranscode covers the field that used to be
// accepted and ignored. A source the client can technically decode but cannot
// afford has to be re-encoded: there is no way to honour a bitrate limit while
// copying the bits.
func TestNegotiate_BitrateLimitForcesATranscode(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.MaxBitrateKbps = 20_000

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8,
		BitrateKbps: 40_000, AudioBitrateKbps: 448,
	}

	decision := Negotiate(info, capability)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode for a source over the client's limit", decision.Mode)
	}
	if decision.VideoAction != ActionTranscode {
		t.Errorf("video action = %q, want transcode", decision.VideoAction)
	}
	// The audio is copied, so its own bitrate is what the ceiling must leave
	// room for - not a guess.
	want := 20_000 - 448
	if decision.TargetBitrateKbps != want {
		t.Errorf("target bitrate = %d, want %d", decision.TargetBitrateKbps, want)
	}
	reasons := strings.Join(decision.Reasons, "; ")
	if !strings.Contains(reasons, "40000 kbps") || !strings.Contains(reasons, "20000 kbps") {
		t.Errorf("the reasons should name both bitrates: %s", reasons)
	}
}

// TestNegotiate_BitrateLimitIsIgnoredWhenTheSourceFits is the guard against
// turning every request into a transcode: a limit the source already satisfies
// changes nothing.
func TestNegotiate_BitrateLimitIsIgnoredWhenTheSourceFits(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.MaxBitrateKbps = 20_000

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, BitrateKbps: 5_000,
	}

	decision := Negotiate(info, capability)
	if decision.Mode != ModeDirectPlay {
		t.Fatalf("mode = %q, want direct play: %s", decision.Mode, strings.Join(decision.Reasons, "; "))
	}
	if decision.TargetBitrateKbps != 0 {
		t.Errorf("target bitrate = %d, want none when the source already fits", decision.TargetBitrateKbps)
	}
}

// TestNegotiate_UnknownSourceBitrateDoesNotForceATranscode pins the conservative
// choice. Treating an unknown rate as over the limit would transcode files that
// probably fit; saying so is the honest middle ground.
func TestNegotiate_UnknownSourceBitrateDoesNotForceATranscode(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.MaxBitrateKbps = 20_000

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8,
	}

	decision := Negotiate(info, capability)
	if decision.Mode != ModeDirectPlay {
		t.Fatalf("mode = %q, want direct play when the source bitrate is unknown", decision.Mode)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "bitrate is unknown") {
		t.Errorf("the reasons should admit the limit cannot be checked: %s", strings.Join(decision.Reasons, "; "))
	}
}

// TestNegotiate_BitrateLimitBelowTheAudioIsRefused covers a limit that cannot be
// met at all. Encoding a picture into what is left would produce something
// nobody can watch, so the request is refused with the arithmetic spelled out.
func TestNegotiate_BitrateLimitBelowTheAudioIsRefused(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.MaxBitrateKbps = 300

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8,
		BitrateKbps: 40_000, AudioBitrateKbps: 448,
	}

	decision := Negotiate(info, capability)
	if decision.Deliverable {
		t.Fatal("a limit that leaves nothing for video must not be called deliverable")
	}
	reasons := strings.Join(decision.Reasons, "; ")
	if !strings.Contains(reasons, "too little") {
		t.Errorf("the reasons should explain the shortfall: %s", reasons)
	}
	// The arithmetic has to be in the reason, including when it goes negative:
	// a 300 kbps limit against 448 kbps of audio leaves -148 for the picture.
	if !strings.Contains(reasons, "-148 kbps") {
		t.Errorf("the reasons should show the arithmetic rather than a rounded guess: %s", reasons)
	}
}

// TestNegotiate_BitrateCeilingLeavesRoomForReEncodedAudio covers the other
// allowance: when the audio is being re-encoded, the ceiling reserves what this
// server encodes it at rather than the source's rate.
func TestNegotiate_BitrateCeilingLeavesRoomForReEncodedAudio(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.MaxBitrateKbps = 2_000

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "ac3",
		Width: 1920, Height: 1080, BitDepth: 8,
		BitrateKbps: 40_000, AudioChannels: 6, AudioBitrateKbps: 640,
	}

	decision := Negotiate(info, capability)
	if decision.AudioAction != ActionTranscode {
		t.Fatalf("audio action = %q, want transcode for 5.1 into a stereo client", decision.AudioAction)
	}
	if want := 2_000 - defaultAudioAllowanceKbps; decision.TargetBitrateKbps != want {
		t.Errorf("target bitrate = %d, want %d (the re-encode target, not the source's 640)",
			decision.TargetBitrateKbps, want)
	}
}

// TestNegotiate_BitrateCeilingWithNoAudioLeavesItAllForVideo covers an entity
// with no audio track: nothing needs reserving.
func TestNegotiate_BitrateCeilingWithNoAudioLeavesItAllForVideo(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.MaxBitrateKbps = 3_000

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264",
		Width: 1920, Height: 1080, BitDepth: 8, BitrateKbps: 40_000,
	}

	decision := Negotiate(info, capability)
	if decision.AudioAction != ActionNone {
		t.Fatalf("audio action = %q, want none", decision.AudioAction)
	}
	if decision.TargetBitrateKbps != 3_000 {
		t.Errorf("target bitrate = %d, want the whole limit", decision.TargetBitrateKbps)
	}
}

// ---- adaptive bitrate ladder -----------------------------------------------

// TestVideoLadder pins the shape of a ladder: the top height, two thirds, half -
// with rungs that are too small or too close together dropped, because a rung a
// player cannot tell apart from the one above is a wasted encode.
func TestVideoLadder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		top    int
		budget int
		want   []Rendition
	}{
		{
			name: "1080p gives three rungs",
			top:  1080,
			want: []Rendition{
				{Height: 1080, BitrateKbps: 4500},
				{Height: 720, BitrateKbps: 2500},
				{Height: 540, BitrateKbps: 1200},
			},
		},
		{
			name: "below the smallest rung there is no ladder",
			top:  300,
			want: nil,
		},
		{
			name: "a short ladder keeps only distinct rungs",
			top:  360,
			want: []Rendition{
				{Height: 360, BitrateKbps: 700},
				{Height: 240, BitrateKbps: 400},
			},
		},
		{
			name: "odd heights are made even, as 4:2:0 requires",
			top:  721,
			want: []Rendition{
				{Height: 720, BitrateKbps: 2500},
				{Height: 480, BitrateKbps: 1200},
				{Height: 360, BitrateKbps: 700},
			},
		},
		{
			name:   "a budget scales every rung rather than flattening them",
			top:    1080,
			budget: 2250,
			want: []Rendition{
				{Height: 1080, BitrateKbps: 2250},
				{Height: 720, BitrateKbps: 1250},
				{Height: 540, BitrateKbps: 600},
			},
		},
		{
			name:   "a budget that leaves less than the floor is raised to it",
			top:    1080,
			budget: 50,
			want: []Rendition{
				{Height: 1080, BitrateKbps: minVideoBitrateKbps},
				{Height: 720, BitrateKbps: minVideoBitrateKbps},
				{Height: 540, BitrateKbps: minVideoBitrateKbps},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := videoLadder(tt.top, tt.budget)
			if len(got) != len(tt.want) {
				t.Fatalf("ladder = %+v, want %+v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("rung %d = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
			// A ladder is only useful if it descends.
			for i := 1; i < len(got); i++ {
				if got[i].Height >= got[i-1].Height {
					t.Errorf("rung %d (%d) is not below rung %d (%d)", i, got[i].Height, i-1, got[i-1].Height)
				}
			}
		})
	}
}

// TestNegotiate_BuildsALadderOnlyWhenNoHeightIsPinned pins the contract behind
// the player's quality menu: a manifest that names a height is asking for one
// rendition, and one that omits it is asking to adapt.
func TestNegotiate_BuildsALadderOnlyWhenNoHeightIsPinned(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "vp9", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, BitrateKbps: 6_000,
	}

	adaptive := hdrCapability()
	adaptive.SupportsHDR = false
	adaptive.MaxBitDepth = 8
	adaptive.MaxHeight = 0
	adaptive.MaxWidth = 0

	decision := Negotiate(info, adaptive)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode: %s", decision.Mode, strings.Join(decision.Reasons, "; "))
	}
	if len(decision.Renditions) < 2 {
		t.Fatalf("expected a ladder, got %+v", decision.Renditions)
	}
	if decision.Renditions[0].Height != 1080 {
		t.Errorf("the top rung is %d, want the source height 1080", decision.Renditions[0].Height)
	}
	// The single-value fields keep describing the top of the ladder, so a client
	// that reads only those still sees a coherent answer.
	if decision.TargetHeight != 1080 {
		t.Errorf("target height = %d, want the top rung", decision.TargetHeight)
	}
	if decision.TargetBitrateKbps != decision.Renditions[0].BitrateKbps {
		t.Errorf("target bitrate = %d, want the top rung's %d",
			decision.TargetBitrateKbps, decision.Renditions[0].BitrateKbps)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "ladder") {
		t.Errorf("the reasons should say a ladder was built: %s", strings.Join(decision.Reasons, "; "))
	}

	// A pinned height means one rendition, which is what the quality menu asks
	// for and what it has to keep getting.
	pinned := adaptive
	pinned.MaxHeight = 720
	if got := Negotiate(info, pinned); len(got.Renditions) != 0 {
		t.Errorf("a pinned height must produce one rendition, got %+v", got.Renditions)
	}
}

// TestNegotiate_PreferredHeightBuildsALadderToppedThere is the contract for a
// quality choice expressed as a preference: the client will not go above the
// height it asked for, but its player is free to step down, so it gets a ladder
// rather than one encode. max_height on its own keeps its historical meaning of
// pinning exactly one rendition, and alongside a preference it is the ceiling
// that ladder stays under.
func TestNegotiate_PreferredHeightBuildsALadderToppedThere(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "vp9", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, BitrateKbps: 6_000,
	}

	adaptive := hdrCapability()
	adaptive.SupportsHDR = false
	adaptive.MaxBitDepth = 8
	adaptive.MaxWidth = 0
	adaptive.MaxHeight = 0

	preferred := adaptive
	preferred.PreferredHeight = 720

	decision := Negotiate(info, preferred)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode: %s", decision.Mode, strings.Join(decision.Reasons, "; "))
	}
	if len(decision.Renditions) < 2 {
		t.Fatalf("a preferred height must still build a ladder, got %+v", decision.Renditions)
	}
	if decision.Renditions[0].Height != 720 {
		t.Errorf("the top rung is %d, want the preferred 720", decision.Renditions[0].Height)
	}
	if decision.TargetHeight != 720 {
		t.Errorf("target height = %d, want the top rung 720", decision.TargetHeight)
	}
	for i := 1; i < len(decision.Renditions); i++ {
		if decision.Renditions[i].Height >= decision.Renditions[i-1].Height {
			t.Errorf("the ladder does not descend: %+v", decision.Renditions)
		}
	}
	if reasons := strings.Join(decision.Reasons, "; "); !strings.Contains(reasons, "720p") {
		t.Errorf("the reasons should name the preference: %s", reasons)
	}

	// max_height alongside a preference is the ceiling the ladder stays under.
	capped := adaptive
	capped.MaxHeight = 1080
	capped.PreferredHeight = 480
	cappedDecision := Negotiate(info, capped)
	if len(cappedDecision.Renditions) < 2 {
		t.Fatalf("a capped preference must still ladder, got %+v", cappedDecision.Renditions)
	}
	if cappedDecision.Renditions[0].Height != 480 {
		t.Errorf("the top rung is %d, want the preferred 480 under the 1080 ceiling",
			cappedDecision.Renditions[0].Height)
	}

	// And max_height on its own keeps its historical meaning: one rendition.
	pinned := adaptive
	pinned.MaxHeight = 720
	if got := Negotiate(info, pinned); len(got.Renditions) != 0 {
		t.Errorf("max_height alone must still pin one rendition, got %+v", got.Renditions)
	}
}

// TestNegotiate_PreferredHeightIsALimitNotAdvice is the trap a preference would
// otherwise fall into: on a source the client could direct-play untouched, a
// preference that only advised the ladder would leave the 4K original delivered
// whole. It has to constrain the output like any other limit.
func TestNegotiate_PreferredHeightIsALimitNotAdvice(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 3840, Height: 2160, BitDepth: 8, BitrateKbps: 20_000,
	}
	capability := ClientCapability{
		Containers:       []string{"mp4", "hls"},
		VideoCodecs:      []string{"h264"},
		AudioCodecs:      []string{"aac"},
		MaxBitDepth:      8,
		MaxAudioChannels: 2,
		SupportsHLS:      true,
		PreferredHeight:  720,
	}

	decision := Negotiate(info, capability)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want a transcode: a 720p preference cannot be honoured by direct play (%s)",
			decision.Mode, strings.Join(decision.Reasons, "; "))
	}
	if decision.TargetHeight != 720 {
		t.Errorf("target height = %d, want 720", decision.TargetHeight)
	}
	if len(decision.Renditions) < 2 || decision.Renditions[0].Height != 720 {
		t.Errorf("renditions = %+v, want a ladder topped at 720", decision.Renditions)
	}
}

// TestNegotiate_BurnKeepsOneRenditionWithAPreference pins the interaction the
// two features have: a burn is one composited picture, so a preference cannot
// turn it into a ladder, and the reasons say why the preference chose a single
// height instead of a top rung.
func TestNegotiate_BurnKeepsOneRenditionWithAPreference(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, DurationSeconds: 600,
		AudioTracks: []AudioTrack{{Index: 1, Codec: "aac", Channels: 2}},
		Subtitles:   []SubtitleTrack{{Index: 3, Codec: "hdmv_pgs_subtitle", Text: false}},
	}
	capability := ClientCapability{
		Containers: []string{"matroska"}, VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true, Subtitles: true,
		BurnSubtitleIndex: 3, PreferredHeight: 1080,
	}

	decision := Negotiate(info, capability)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want a transcode", decision.Mode)
	}
	if len(decision.Renditions) != 0 {
		t.Errorf("a burn is one composited picture, so a preference cannot make it a ladder: %+v",
			decision.Renditions)
	}
	if reasons := strings.Join(decision.Reasons, "; "); !strings.Contains(reasons, "preference selects a single rendition") {
		t.Errorf("the reasons should explain why the preference did not ladder: %s", reasons)
	}
}

// TestNegotiate_ALadderReencodesTheAudio pins the rule the container's ffmpeg
// forced: a ladder gives every rung its own audio stream, because the HLS muxer
// refuses to place one copied elementary stream in two variants. On ffmpeg 5.1
// and 6.1 that refusal silently costs rungs from the master playlist, so a
// client never sees the lower ones. A single rendition still copies.
func TestNegotiate_ALadderReencodesTheAudio(t *testing.T) {
	t.Parallel()

	// AAC in, AAC accepted: without a ladder this is a copy.
	info := &MediaInfo{
		Container: "matroska", VideoCodec: "vp9", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, BitrateKbps: 6_000,
		AudioBitrateKbps: 128,
	}
	adaptive := hdrCapability()
	adaptive.SupportsHDR = false
	adaptive.MaxBitDepth = 8
	adaptive.MaxWidth = 0
	adaptive.MaxHeight = 0

	ladder := Negotiate(info, adaptive)
	if len(ladder.Renditions) < 2 {
		t.Fatalf("expected a ladder, got %+v", ladder.Renditions)
	}
	if ladder.AudioAction != ActionTranscode {
		t.Errorf("a ladder copied the audio (%q); each rung needs its own stream", ladder.AudioAction)
	}
	if reasons := strings.Join(ladder.Reasons, "; "); !strings.Contains(reasons, "will not put one copied stream in two variants") {
		t.Errorf("the reasons should say why the audio is re-encoded: %s", reasons)
	}

	// The same source pinned to one rendition keeps the copy: the limitation
	// belongs to the ladder, not to the audio.
	pinned := adaptive
	pinned.MaxHeight = 720
	single := Negotiate(info, pinned)
	if len(single.Renditions) != 0 {
		t.Fatalf("a pinned height must be one rendition, got %+v", single.Renditions)
	}
	if single.AudioAction != ActionCopy {
		t.Errorf("a single rendition transcoded the audio (%q); copying is still right there", single.AudioAction)
	}
}

// TestNegotiate_LadderRespectsTheBitrateLimit checks that a ladder is scaled by
// the client's total limit rather than ignoring it.
func TestNegotiate_LadderRespectsTheBitrateLimit(t *testing.T) {
	t.Parallel()

	capability := hdrCapability()
	capability.SupportsHDR = false
	capability.MaxBitDepth = 8
	capability.MaxHeight = 0
	capability.MaxWidth = 0
	capability.MaxBitrateKbps = 2_000

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "vp9", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, BitrateKbps: 6_000, AudioBitrateKbps: 128,
	}

	decision := Negotiate(info, capability)
	if len(decision.Renditions) < 2 {
		t.Fatalf("expected a ladder, got %+v", decision.Renditions)
	}
	// The audio is re-encoded, not copied: a ladder gives each rung its own
	// audio stream, and ffmpeg's HLS muxer refuses to place one copied stream in
	// two variants. So the top rung may use the limit less what this server
	// encodes the audio at, not less the source's own rate.
	if got, want := decision.Renditions[0].BitrateKbps, 2_000-defaultAudioAllowanceKbps; got != want {
		t.Errorf("top rung = %d kbps, want %d (the limit less the encoded audio)", got, want)
	}
	for _, rung := range decision.Renditions {
		if rung.BitrateKbps > decision.Renditions[0].BitrateKbps {
			t.Errorf("rung %+v exceeds the top rung", rung)
		}
	}
}

// TestSessionPlaylistName covers which file a client is told to open.
func TestSessionPlaylistName(t *testing.T) {
	t.Parallel()

	single := Decision{Mode: ModeTranscode, VideoAction: ActionTranscode}
	if got := sessionPlaylistName(single); got != MediaPlaylistName {
		t.Errorf("single rendition playlist = %q, want %q", got, MediaPlaylistName)
	}

	ladder := Decision{
		Mode: ModeTranscode, VideoAction: ActionTranscode,
		Renditions: []Rendition{{Height: 720, BitrateKbps: 2500}, {Height: 360, BitrateKbps: 700}},
	}
	if got := sessionPlaylistName(ladder); got != MasterPlaylistName {
		t.Errorf("ladder playlist = %q, want %q", got, MasterPlaylistName)
	}

	// A session built without a name - by a test double, or a future caller -
	// must still yield a file a player can open rather than an empty path.
	if got := (&Session{}).PlaylistFile(); got != MediaPlaylistName {
		t.Errorf("Session without a playlist name = %q, want %q", got, MediaPlaylistName)
	}
}

// TestVariantStreamMap covers the ffmpeg incantation that pairs rungs with their
// audio. Getting it wrong is how a ladder ends up as two copies of one stream.
func TestVariantStreamMap(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		withAudio bool
		count     int
		want      string
	}{
		{name: "two rungs with audio", withAudio: true, count: 2, want: "v:0,a:0 v:1,a:1"},
		{name: "three rungs with audio", withAudio: true, count: 3, want: "v:0,a:0 v:1,a:1 v:2,a:2"},
		{name: "video only", withAudio: false, count: 2, want: "v:0 v:1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := variantStreamMap(tt.withAudio, tt.count); got != tt.want {
				t.Errorf("variantStreamMap(%v, %d) = %q, want %q", tt.withAudio, tt.count, got, tt.want)
			}
		})
	}
}

// TestBuildFFmpegArgs_Ladder covers the command line a ladder produces. Every
// per-rendition option needs its stream specifier: without one, all of them land
// on the first rung and the ladder is a ladder in name only.
func TestBuildFFmpegArgs_Ladder(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{
		SegmentSeconds: 4,
		Server:         ServerCapability{VideoEncoders: []string{"libx264"}},
	}
	args, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true, Container: "hls",
		VideoAction: ActionTranscode, AudioAction: ActionCopy, TargetVideoCodec: "h264",
		Renditions: []Rendition{
			{Height: 720, BitrateKbps: 2500},
			{Height: 360, BitrateKbps: 700},
		},
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"-c:v:0 libx264", "-c:v:1 libx264",
		"-filter:v:0 scale=-2:720", "-filter:v:1 scale=-2:360",
		"-maxrate:0 2500k", "-maxrate:1 700k",
		"-force_key_frames:0 expr:gte(t,n_forced*4)", "-force_key_frames:1 expr:gte(t,n_forced*4)",
		"-pix_fmt:0 yuv420p", "-pix_fmt:1 yuv420p",
		"-master_pl_name master.m3u8",
		"-var_stream_map v:0,a:0 v:1,a:1",
		"-hls_segment_filename /tmp/session/seg%v_%05d.ts",
		"/tmp/session/playlist_%v.m3u8",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ladder command line is missing %q:\n%s", want, joined)
		}
	}
	// libx264 needs no forced-IDR flag: it already produces the frame the muxer
	// cuts on. Adding one would be passing an option it does not know (S-15).
	for _, unwanted := range []string{"-forced-idr", "-forced_idr"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("a software ladder must not carry %q:\n%s", unwanted, joined)
		}
	}
	// Each rung maps the source once; two rungs and one audio stream each is
	// four maps, and any fewer would feed a rung the wrong input.
	if got := strings.Count(joined, "-map 0:v:0"); got != 2 {
		t.Errorf("video maps = %d, want one per rung:\n%s", got, joined)
	}
	if got := strings.Count(joined, "-map 0:a:0?"); got != 2 {
		t.Errorf("audio maps = %d, want one per rung:\n%s", got, joined)
	}

	// A single-rendition session must not grow a master playlist or specifiers:
	// its command line is the one that has always worked.
	single, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true, Container: "hls",
		VideoAction: ActionTranscode, AudioAction: ActionCopy, TargetVideoCodec: "h264",
		TargetHeight: 720, TargetBitrateKbps: 2500,
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	singleJoined := strings.Join(single, " ")
	for _, unwanted := range []string{"-master_pl_name", "-var_stream_map", "-c:v:0", "-vf scale=-2:720,zscale"} {
		if strings.Contains(singleJoined, unwanted) {
			t.Errorf("a single rendition should not contain %q:\n%s", unwanted, singleJoined)
		}
	}
	for _, want := range []string{"-c:v libx264", "-vf scale=-2:720", "-maxrate 2500k", "playlist.m3u8"} {
		if !strings.Contains(singleJoined, want) {
			t.Errorf("single rendition is missing %q:\n%s", want, singleJoined)
		}
	}
}

// ---- audio track selection -------------------------------------------------

// TestNegotiate_DeliversTheDefaultAudioTrack covers a file whose second track is
// the one marked default: "the first stream wins" delivers the wrong language on
// a file that says which one it means.
func TestNegotiate_DeliversTheDefaultAudioTrack(t *testing.T) {
	t.Parallel()

	decision := Negotiate(multiTrackInfo(), BrowserCapability())
	if decision.TargetAudioStreamIndex != 3 {
		t.Errorf("audio stream = %d, want the default track's 3", decision.TargetAudioStreamIndex)
	}
	if decision.AudioAction != ActionTranscode {
		t.Errorf("audio action = %q, want transcode: the browser cannot take AC-3 5.1",
			decision.AudioAction)
	}
}

// TestNegotiate_HonoursAChosenAudioTrack covers an explicit request for the
// track that is not the default, including the checks that have to follow the
// choice rather than the summary: this one is stereo AAC, which the browser
// takes as it is.
func TestNegotiate_HonoursAChosenAudioTrack(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.AudioTrackIndex = 1

	info := multiTrackInfo()
	// Make the file directly playable so the *only* reason to repackage is the
	// track choice, which is the case that would otherwise be missed.
	info.Container = "mp4"

	decision := Negotiate(info, capability)
	if decision.TargetAudioStreamIndex != 1 {
		t.Fatalf("audio stream = %d, want the requested 1", decision.TargetAudioStreamIndex)
	}
	if decision.AudioAction != ActionCopy {
		t.Errorf("audio action = %q, want copy: track 1 is stereo AAC", decision.AudioAction)
	}
	// Direct play serves the whole file and lets the player pick, so a chosen
	// track has to be repackaged instead - without re-encoding anything.
	if decision.Mode != ModeRemux {
		t.Fatalf("mode = %q, want remux for a chosen track in an otherwise playable file", decision.Mode)
	}
	if decision.VideoAction != ActionCopy {
		t.Errorf("video action = %q, want copy", decision.VideoAction)
	}
	reasons := strings.Join(decision.Reasons, "; ")
	if !strings.Contains(reasons, "audio track 1") {
		t.Errorf("the reasons should name the chosen track: %s", reasons)
	}
}

// TestNegotiate_ChosenTrackChecksApplyToThatTrack covers the trap of validating
// the summary instead of the choice: track 1 is stereo AAC and track 3 is AC-3
// 5.1, so the same file needs a different audio decision depending on which was
// asked for.
func TestNegotiate_ChosenTrackChecksApplyToThatTrack(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.Containers = []string{"hls"}

	// Choosing the stereo AAC track needs no audio re-encode, only repackaging.
	stereo := capability
	stereo.AudioTrackIndex = 1
	if got := Negotiate(multiTrackInfo(), stereo); got.AudioAction != ActionCopy {
		t.Errorf("track 1 audio action = %q, want copy", got.AudioAction)
	}

	// Choosing the 5.1 AC-3 track needs a re-encode and a downmix, because that
	// is what the chosen track is.
	surround := capability
	surround.AudioTrackIndex = 3
	decision := Negotiate(multiTrackInfo(), surround)
	if decision.AudioAction != ActionTranscode {
		t.Errorf("track 3 audio action = %q, want transcode", decision.AudioAction)
	}
	if decision.TargetAudioChannels != 2 {
		t.Errorf("target audio channels = %d, want a downmix to 2", decision.TargetAudioChannels)
	}
}

// TestNegotiate_MissingAudioTrackFallsBackAndSaysSo covers the defensive path:
// the API refuses an unknown index with a 400, but a direct caller of Negotiate
// gets the default track and a reason rather than silence or a failure.
func TestNegotiate_MissingAudioTrackFallsBackAndSaysSo(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability()
	capability.AudioTrackIndex = 9

	decision := Negotiate(multiTrackInfo(), capability)
	if !decision.Deliverable {
		t.Fatalf("a missing track is not a reason to refuse the film: %s", strings.Join(decision.Reasons, "; "))
	}
	if decision.TargetAudioStreamIndex != 3 {
		t.Errorf("audio stream = %d, want the default track's 3", decision.TargetAudioStreamIndex)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "no audio track with stream index 9") {
		t.Errorf("the reasons should say the request was ignored: %s", strings.Join(decision.Reasons, "; "))
	}
}

// TestBuildFFmpegArgs_MapsTheChosenAudioStream pins the mapping, which is the
// step that decides whether the chosen track is the one that arrives.
func TestBuildFFmpegArgs_MapsTheChosenAudioStream(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{
		SegmentSeconds: 4,
		Server:         ServerCapability{VideoEncoders: []string{"libx264"}},
	}

	chosen, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true, Container: "hls",
		VideoAction: ActionTranscode, AudioAction: ActionCopy, TargetVideoCodec: "h264",
		TargetAudioStreamIndex: 3,
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	if joined := strings.Join(chosen, " "); !strings.Contains(joined, "-map 0:3?") {
		t.Errorf("chosen track is not mapped by its stream index:\n%s", joined)
	}

	// With no resolved index the mapping falls back to the first audio stream,
	// which is what a hand-built decision means.
	fallback, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true, Container: "hls",
		VideoAction: ActionTranscode, AudioAction: ActionCopy, TargetVideoCodec: "h264",
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	if joined := strings.Join(fallback, " "); !strings.Contains(joined, "-map 0:a:0?") {
		t.Errorf("the default mapping should be the first audio stream:\n%s", joined)
	}

	// And a ladder maps the chosen track for every rung, not just the first.
	ladder, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true, Container: "hls",
		VideoAction: ActionTranscode, AudioAction: ActionCopy, TargetVideoCodec: "h264",
		TargetAudioStreamIndex: 3,
		Renditions:             []Rendition{{Height: 720, BitrateKbps: 2500}, {Height: 360, BitrateKbps: 700}},
	}, cfg)
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	if got := strings.Count(strings.Join(ladder, " "), "-map 0:3?"); got != 2 {
		t.Errorf("chosen track mapped %d times, want once per rung:\n%s", got, strings.Join(ladder, " "))
	}
}

// TestNegotiate_BurnsAnImageSubtitle is the contract for a bitmap subtitle: a
// browser cannot render one as a track, so asking for it means compositing it
// into the picture, which is a single-rendition re-encode.
func TestNegotiate_BurnsAnImageSubtitle(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, DurationSeconds: 600,
		AudioTracks: []AudioTrack{{Index: 1, Codec: "aac", Channels: 2}},
		Subtitles: []SubtitleTrack{
			{Index: 2, Codec: "subrip", Text: true, Language: "en"},
			{Index: 3, Codec: "hdmv_pgs_subtitle", Text: false, Language: "fr"},
		},
	}
	capability := ClientCapability{
		Containers: []string{"matroska"}, VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true, Subtitles: true,
		BurnSubtitleIndex: 3,
	}

	decision := Negotiate(info, capability)
	if decision.Mode != ModeTranscode || decision.VideoAction != ActionTranscode {
		t.Fatalf("mode = %q action = %q, want a transcode: a bitmap cannot be composited into copied bits",
			decision.Mode, decision.VideoAction)
	}
	if decision.BurnedSubtitleIndex != 3 {
		t.Errorf("burned index = %d, want 3", decision.BurnedSubtitleIndex)
	}
	// No height was pinned, but a burn is one composited picture: a ladder here
	// would mean burning only one rung.
	if len(decision.Renditions) != 0 {
		t.Errorf("renditions = %+v, want a single rendition for a burn", decision.Renditions)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "burned into the picture") {
		t.Errorf("the reasons do not say the subtitle is burned in: %v", decision.Reasons)
	}
}

// TestNegotiate_DoesNotBurnATextSubtitle pins the other half: a text track named
// for burning is delivered as a selectable track instead, because that is
// better than irreversibly burning it in, and the reason says which happened.
func TestNegotiate_DoesNotBurnATextSubtitle(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, DurationSeconds: 600,
		AudioTracks: []AudioTrack{{Index: 1, Codec: "aac", Channels: 2}},
		Subtitles:   []SubtitleTrack{{Index: 2, Codec: "subrip", Text: true, Language: "en"}},
	}
	capability := ClientCapability{
		Containers: []string{"matroska"}, VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true, Subtitles: true,
		BurnSubtitleIndex: 2,
	}

	decision := Negotiate(info, capability)
	if decision.BurnedSubtitleIndex != 0 {
		t.Errorf("burned index = %d, want none: a text track is not burned", decision.BurnedSubtitleIndex)
	}
	if decision.VideoAction == ActionTranscode {
		t.Error("a text track named for burning forced a re-encode; it should be delivered as a track")
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "text-based") {
		t.Errorf("the reasons do not explain why nothing was burned: %v", decision.Reasons)
	}
}

// TestNegotiate_BurnIndexThatDoesNotExist pins the third case: an index the file
// does not have burns nothing and says so, rather than failing the session.
func TestNegotiate_BurnIndexThatDoesNotExist(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, DurationSeconds: 600,
		AudioTracks: []AudioTrack{{Index: 1, Codec: "aac", Channels: 2}},
		Subtitles:   []SubtitleTrack{{Index: 3, Codec: "hdmv_pgs_subtitle", Text: false}},
	}
	capability := ClientCapability{
		Containers: []string{"matroska"}, VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true, Subtitles: true,
		BurnSubtitleIndex: 9,
	}

	decision := Negotiate(info, capability)
	if decision.BurnedSubtitleIndex != 0 {
		t.Errorf("burned index = %d, want none for an index the file lacks", decision.BurnedSubtitleIndex)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "no subtitle track with stream index 9") {
		t.Errorf("the reasons do not name the missing track: %v", decision.Reasons)
	}
}

// TestBuildFFmpegArgs_BurnsAnImageSubtitle asserts the shape of the command that
// does the compositing: the picture is the filter graph's output, the subtitle
// comes from a second opening of the input, and no -vf is attached (ffmpeg
// refuses -vf on an output that already has a -filter_complex).
func TestBuildFFmpegArgs_BurnsAnImageSubtitle(t *testing.T) {
	t.Parallel()

	cfg := ManagerConfig{SegmentSeconds: 6, Server: ServerCapability{VideoEncoders: []string{"libx264"}}}
	decision := Decision{
		Mode: ModeTranscode, Deliverable: true, Container: "hls",
		VideoAction: ActionTranscode, AudioAction: ActionCopy,
		TargetVideoCodec: "h264", TargetHeight: 720, BurnedSubtitleIndex: 3,
	}

	args, err := BuildFFmpegArgsAt("/tmp/session", "/media/movie.mkv", decision, cfg, 901.5)
	if err != nil {
		t.Fatalf("BuildFFmpegArgsAt: %v", err)
	}
	line := strings.Join(args, " ")

	if got := strings.Count(line, "-i /media/movie.mkv"); got != 2 {
		t.Errorf("input opened %d times, want twice: the subtitle needs its own input\n%s", got, line)
	}
	if got := strings.Count(line, "-ss 901.500"); got != 2 {
		t.Errorf("offset applied %d times, want to both inputs so the cue timeline lines up\n%s", got, line)
	}
	for _, want := range []string{
		"-filter_complex [0:v:0]scale=-2:720[base];[1:3]format=rgba[burn0];[burn0][base]scale2ref=flags=neighbor[burn][base2];[base2][burn]overlay=format=auto[v]",
		"-map [v]",
		"-c:v libx264",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q from:\n%s", want, line)
		}
	}
	if strings.Contains(line, "-vf") {
		t.Errorf("-vf cannot share an output with -filter_complex:\n%s", line)
	}
}

// TestBuildFFmpegArgs_HardwareLadderCarriesTheForcedIDRSpellingPerRendition
// guards the ladder spelling for S-15.
//
// Every encoder option on a ladder needs a stream specifier, or it lands on the
// first video stream and the remaining rungs keep the native GOP - the exact
// problem the forced keyframe exists to remove, left in place for every rung but
// one.
func TestBuildFFmpegArgs_HardwareLadderCarriesTheForcedIDRSpellingPerRendition(t *testing.T) {
	t.Parallel()

	args, err := BuildFFmpegArgs("/tmp/session", "/media/movie.mkv", Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
		Renditions: []Rendition{
			{Height: 1080, BitrateKbps: 5000},
			{Height: 720, BitrateKbps: 2500},
		},
	}, ManagerConfig{
		SegmentSeconds: 4,
		Server: ServerCapability{
			VideoEncoders: []string{"h264_nvenc"},
			AudioEncoders: []string{"aac"},
		},
	})
	if err != nil {
		t.Fatalf("BuildFFmpegArgs: %v", err)
	}
	joined := strings.Join(args, " ")

	for _, want := range []string{"-forced-idr:0 1", "-forced-idr:1 1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("a ladder rung is missing the specifier on %q:\n%s", want, joined)
		}
	}
	// The unsuffixed form would apply to the first video stream only, which is
	// what the specifier exists to avoid.
	if strings.Contains(joined, "-forced-idr 1") {
		t.Errorf("the forced-IDR flag was emitted without a specifier on a ladder:\n%s", joined)
	}
}

// TestNegotiate_WhichRequestsGetALadder is the regression test for D-5.
//
// playback.md contradicted itself and the code. It said an omitted body means
// "the built-in browser profile", and the profile it described caps at 1920x1080;
// it also said a request naming neither height adapts into a ladder. But
// BrowserCapability sets MaxHeight and leaves PreferredHeight unset, and that
// combination is a *pin* by the page's own rule - "max_height alone gives me
// exactly this" - so a body-less request never gets a ladder. The UI, which omits
// the height fields entirely, does.
//
// The behaviour is right and is what a body-less caller should want: a stable,
// cheap answer that adapts to the viewer's quality menu rather than a ladder
// chosen without knowing the source. The documentation was wrong, and this pins
// the four cases so the two cannot disagree again.
func TestNegotiate_WhichRequestsGetALadder(t *testing.T) {
	t.Parallel()

	// 4K H.264 the browser profile can decode but must downscale to its ceiling,
	// which is a transcode and therefore a case where a ladder is possible.
	info := &MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 3840, Height: 2160, BitDepth: 8, DurationSeconds: 600,
	}

	tests := []struct {
		name        string
		capability  ClientCapability
		wantLadder  bool
		wantTopRung int
		description string
	}{
		{
			name:       "the built-in profile pins one rendition",
			capability: BrowserCapability(),
			wantLadder: false,
			description: "a body-less request. max_height alone is a pin, so the answer is " +
				"one encode at the profile's ceiling rather than a ladder",
		},
		{
			// The source has to need encoding for a ladder to be possible at all: a
			// source the client can take as-is is direct play or a remux, and a
			// remux copies the video rather than building rungs. The UI's request
			// is this shape - no heights, and a source needing work.
			name: "no heights at all is a ladder",
			capability: ClientCapability{
				Containers: []string{"hls"}, VideoCodecs: []string{"h264"},
				AudioCodecs: []string{"aac"}, SupportsHLS: true,
			},
			wantLadder: true,
			description: "the UI's request: it sends no height, so it adapts as far as the " +
				"source allows",
		},
		{
			name: "a preferred height is a ladder topped there",
			capability: ClientCapability{
				Containers: []string{"hls"}, VideoCodecs: []string{"h264"},
				AudioCodecs: []string{"aac"}, SupportsHLS: true,
				MaxHeight: 1080, PreferredHeight: 720,
			},
			wantLadder:  true,
			wantTopRung: 720,
			description: "what the quality menu sends when a viewer picks a setting",
		},
		{
			name: "a max height alone is a pin",
			capability: ClientCapability{
				Containers: []string{"hls"}, VideoCodecs: []string{"h264"},
				AudioCodecs: []string{"aac"}, SupportsHLS: true,
				MaxHeight: 720,
			},
			wantLadder:  false,
			wantTopRung: 720,
			description: "the deterministic request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The source is chosen per case: a client that can take the video as-is
			// is direct play or a remux, and neither builds rungs. HEVC is the codec
			// the browser profile cannot decode, so it forces the encode that a
			// ladder needs.
			info := info
			if tt.wantLadder && len(tt.capability.VideoCodecs) == 1 &&
				tt.capability.VideoCodecs[0] == "h264" && tt.capability.MaxHeight == 0 {
				info = &MediaInfo{
					Container: "matroska", VideoCodec: "hevc", AudioCodec: "aac",
					Width: 3840, Height: 2160, BitDepth: 8, DurationSeconds: 600,
				}
			}

			decision := Negotiate(info, tt.capability.Normalise())
			if decision.VideoAction != ActionTranscode {
				t.Fatalf("video action = %q, want a transcode: %s", decision.VideoAction, tt.description)
			}

			gotLadder := len(decision.Renditions) > 1
			if gotLadder != tt.wantLadder {
				t.Errorf("got %d renditions, want %s: %s",
					len(decision.Renditions), map[bool]string{true: "a ladder", false: "one"}[tt.wantLadder],
					tt.description)
			}
			if tt.wantTopRung > 0 && decision.TargetHeight != tt.wantTopRung {
				t.Errorf("target height = %d, want %d", decision.TargetHeight, tt.wantTopRung)
			}
		})
	}
}

// TestNegotiate_BurnInIsCodecAgnostic is the regression test for D-15.
//
// The burn decision takes the default branch for any non-text track, so DVB
// subtitles are burned like PGS and VobSub — but the 400 message for a *text*
// track said "only image subtitles (PGS, VobSub) are burned in", and README.md
// said DVB is "burn-in only" as though that had been shown. Both are claims the
// project had not verified: the burn integration test uses a PGS fixture, and
// nothing exercised DVB through this path.
//
// What can be verified without a DVB burn fixture is the decision itself, and
// that is what this pins: the rule is "not text", not a list. A codec the project
// has no decoder for is still a bitmap that cannot go into copied bits.
func TestNegotiate_BurnInIsCodecAgnostic(t *testing.T) {
	t.Parallel()

	for _, codec := range []string{
		"hdmv_pgs_subtitle",
		"dvd_subtitle", // VobSub
		"dvb_subtitle",
		"xsub", // a codec with no support anywhere in this project
	} {
		t.Run(codec, func(t *testing.T) {
			t.Parallel()

			info := &MediaInfo{
				Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
				Width: 1920, Height: 1080, BitDepth: 8, DurationSeconds: 600,
				AudioTracks: []AudioTrack{{Index: 1, Codec: "aac", Channels: 2}},
				Subtitles: []SubtitleTrack{
					{Index: 3, Codec: codec, Text: false, Language: "fr"},
				},
			}
			capability := ClientCapability{
				Containers: []string{"matroska"}, VideoCodecs: []string{"h264"},
				AudioCodecs: []string{"aac"}, SupportsHLS: true,
				BurnSubtitleIndex: 3,
			}

			decision := Negotiate(info, capability)
			if decision.BurnedSubtitleIndex != 3 {
				t.Errorf("burned index = %d, want 3: the burn rule is \"not text\", not a "+
					"list of codecs", decision.BurnedSubtitleIndex)
			}
			if decision.Mode != ModeTranscode {
				t.Errorf("mode = %q, want a transcode: a bitmap cannot be composited into "+
					"copied bits", decision.Mode)
			}
			// The reason names the codec it is about, so a log reader can tell why.
			reasons := strings.Join(decision.Reasons, "; ")
			if !strings.Contains(reasons, codec) {
				t.Errorf("the reasons do not name %s: %s", codec, reasons)
			}
		})
	}
}

// TestNegotiate_TextTrackRefusalNamesTheWholeRule pins the message D-15 found to
// be narrower than the code. It is what a client sees when it asks for a text
// track to be burned, so it has to describe the actual rule.
func TestNegotiate_TextTrackRefusalNamesTheWholeRule(t *testing.T) {
	t.Parallel()

	info := &MediaInfo{
		Container: "matroska", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 8, DurationSeconds: 600,
		AudioTracks: []AudioTrack{{Index: 1, Codec: "aac", Channels: 2}},
		Subtitles:   []SubtitleTrack{{Index: 2, Codec: "subrip", Text: true, Language: "en"}},
	}
	capability := ClientCapability{
		Containers: []string{"matroska"}, VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true,
		BurnSubtitleIndex: 2,
	}

	decision := Negotiate(info, capability)
	if decision.BurnedSubtitleIndex != 0 {
		t.Fatalf("burned index = %d, want 0: a text track is delivered, not burned",
			decision.BurnedSubtitleIndex)
	}
	reasons := strings.Join(decision.Reasons, "; ")
	// The old message claimed PGS and VobSub were the burnable set, which is not
	// the rule the default branch implements.
	if strings.Contains(reasons, "only image subtitles") {
		t.Errorf("the refusal claims a fixed list of burnable codecs; the rule is that a "+
			"text track is delivered and a bitmap is burned: %s", reasons)
	}
}

// TestNegotiate_BitDepthZeroMeansUnrestricted is the regression test for S-14.
//
// The field documents zero as "unrestricted", and the check treated it as "at
// most 0-bit" - so every source deeper than 8 bits became a transcode, with a
// reason no client can mean. The browser protection does not depend on the zero
// value: it comes from the built-in profile setting 8 explicitly, which the test
// below pins alongside this one.
func TestNegotiate_BitDepthZeroMeansUnrestricted(t *testing.T) {
	t.Parallel()

	// 10-bit HEVC, which a browser cannot decode as H.264 High 10 but which a
	// client that declares no depth limit is saying it can.
	info := &MediaInfo{
		Container: "matroska", VideoCodec: "hevc", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 10, DurationSeconds: 600,
	}

	// A client that declares a depth is held to it: 10-bit is over its 8.
	strict := ClientCapability{
		Containers: []string{"hls"}, VideoCodecs: []string{"hevc"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true,
		MaxBitDepth: 8,
	}.Normalise()
	if !reasonSays(strict, info, "bit") {
		t.Errorf("a client asking for at most 8-bit was given a 10-bit source without a "+
			"reason; reasons: %v", Negotiate(info, strict).Reasons)
	}

	// A client that leaves it zero is not.
	open := ClientCapability{
		Containers: []string{"hls"}, VideoCodecs: []string{"hevc"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true,
	}.Normalise()
	decision := Negotiate(info, open)
	for _, reason := range decision.Reasons {
		if strings.Contains(reason, "bit") && strings.Contains(reason, "0-bit") {
			t.Errorf("the reason claims a client decodes at most 0-bit, which is not "+
				"something a client can mean: %s", reason)
		}
	}
}

// TestNegotiate_BuiltInProfileStillRefusesDeepVideo guards the protection the
// zero value must not be mistaken for.
func TestNegotiate_BuiltInProfileStillRefusesDeepVideo(t *testing.T) {
	t.Parallel()

	// mp4 rather than matroska, on purpose: the browser profile does not accept
	// matroska, so a matroska fixture is refused for its container and this test
	// would pass without the bit-depth check doing anything. The fixture has to
	// make bit depth the only obstacle, or it asserts nothing.
	info := &MediaInfo{
		Container: "mp4", VideoCodec: "h264", AudioCodec: "aac",
		Width: 1920, Height: 1080, BitDepth: 10, DurationSeconds: 600,
	}
	// The browser profile sets MaxBitDepth to 8 rather than leaving it zero, and
	// that is what stops a 10-bit H.264 being direct-played into a stalled player.
	decision := Negotiate(info, BrowserCapability().Normalise())
	if decision.Mode == ModeDirectPlay {
		t.Errorf("the built-in profile direct-played a 10-bit source; mode = %q", decision.Mode)
	}
	if !strings.Contains(strings.Join(decision.Reasons, "; "), "bit") {
		t.Errorf("the refusal does not say it is about bit depth: %v", decision.Reasons)
	}
}

// reasonSays reports whether a negotiation produced a reason mentioning a word.
func reasonSays(capability ClientCapability, info *MediaInfo, word string) bool {
	return strings.Contains(strings.Join(Negotiate(info, capability).Reasons, "; "), word)
}
