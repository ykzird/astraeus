package streaming

import (
	"strings"
	"testing"
)

func TestClientCapability_Normalise(t *testing.T) {
	t.Parallel()

	capability := ClientCapability{
		Containers:  []string{"MP4", "mp4", ""},
		VideoCodecs: []string{"H265", "avc", "H.265"},
		AudioCodecs: []string{"AAC", "eac-3", "aac"},
	}.Normalise()

	if got := strings.Join(capability.VideoCodecs, ","); got != "hevc,h264" {
		t.Errorf("video codecs = %q, want %q", got, "hevc,h264")
	}
	if got := strings.Join(capability.AudioCodecs, ","); got != "aac,eac3" {
		t.Errorf("audio codecs = %q, want %q", got, "aac,eac3")
	}
	if got := strings.Join(capability.Containers, ","); got != "mp4" {
		t.Errorf("containers = %q, want %q", got, "mp4")
	}
}

func TestClientCapability_Supports(t *testing.T) {
	t.Parallel()

	capability := BrowserCapability().Normalise()

	tests := []struct {
		name       string
		check      func() bool
		wantResult bool
	}{
		{"h264 video", func() bool { return capability.SupportsVideo("h264") }, true},
		{"AVC alias", func() bool { return capability.SupportsVideo("avc1") }, true},
		{"hevc video", func() bool { return capability.SupportsVideo("hevc") }, false},
		{"aac audio", func() bool { return capability.SupportsAudio("aac") }, true},
		{"dts audio", func() bool { return capability.SupportsAudio("dts") }, false},
		{"mp4 container", func() bool { return capability.SupportsContainer("mp4") }, true},
		{"matroska container", func() bool { return capability.SupportsContainer("matroska") }, false},
		{"comma separated container list with a match",
			func() bool { return capability.SupportsContainer("matroska,webm") }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.check(); got != tt.wantResult {
				t.Errorf("got %v, want %v", got, tt.wantResult)
			}
		})
	}
}

func TestClientCapability_PreferredCodecs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		capability ClientCapability
		wantVideo  string
		wantAudio  string
	}{
		{
			name:       "prefers h264 and aac when both are offered",
			capability: ClientCapability{VideoCodecs: []string{"vp9", "h264"}, AudioCodecs: []string{"opus", "aac"}},
			wantVideo:  "h264",
			wantAudio:  "aac",
		},
		{
			name:       "falls back to the client's own first entry",
			capability: ClientCapability{VideoCodecs: []string{"theora"}, AudioCodecs: []string{"speex"}},
			wantVideo:  "theora",
			wantAudio:  "speex",
		},
		{
			name:       "empty lists produce empty preferences",
			capability: ClientCapability{},
			wantVideo:  "",
			wantAudio:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			capability := tt.capability.Normalise()
			if got := capability.PreferredVideoCodec(); got != tt.wantVideo {
				t.Errorf("preferred video = %q, want %q", got, tt.wantVideo)
			}
			if got := capability.PreferredAudioCodec(); got != tt.wantAudio {
				t.Errorf("preferred audio = %q, want %q", got, tt.wantAudio)
			}
		})
	}
}

func TestClientCapability_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		capability ClientCapability
		wantErr    bool
	}{
		{name: "browser profile is valid", capability: BrowserCapability()},
		{name: "no video codecs", capability: ClientCapability{Containers: []string{"mp4"}, AudioCodecs: []string{"aac"}}, wantErr: true},
		{name: "no containers", capability: ClientCapability{VideoCodecs: []string{"h264"}, AudioCodecs: []string{"aac"}}, wantErr: true},
		{name: "no audio codecs", capability: ClientCapability{VideoCodecs: []string{"h264"}, Containers: []string{"mp4"}}, wantErr: true},
		{
			name: "negative limit",
			capability: ClientCapability{
				VideoCodecs: []string{"h264"},
				AudioCodecs: []string{"aac"},
				Containers:  []string{"mp4"},
				MaxHeight:   -1,
			},
			wantErr: true,
		},
		{
			name: "negative preferred height",
			capability: ClientCapability{
				VideoCodecs:     []string{"h264"},
				AudioCodecs:     []string{"aac"},
				Containers:      []string{"mp4"},
				PreferredHeight: -1,
			},
			wantErr: true,
		},
		{
			name: "a preferred height is a valid manifest",
			capability: ClientCapability{
				VideoCodecs:     []string{"h264"},
				AudioCodecs:     []string{"aac"},
				Containers:      []string{"mp4"},
				PreferredHeight: 720,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.capability.Validate()
			if tt.wantErr && err == nil {
				t.Error("expected an error")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestClientCapability_HDRSupport covers the contradiction a manifest can carry:
// HDR is stored at ten bits or more, so declaring HDR while capping the decoder
// at eight is asking for something that cannot exist.
func TestClientCapability_HDRSupport(t *testing.T) {
	t.Parallel()

	base := ClientCapability{
		Containers:  []string{"hls"},
		VideoCodecs: []string{"hevc"},
		AudioCodecs: []string{"aac"},
		MaxBitDepth: 10,
	}

	hdr := base
	hdr.SupportsHDR = true
	if err := hdr.Validate(); err != nil {
		t.Errorf("an HDR manifest at 10 bits should be valid: %v", err)
	}

	// An unrestricted bit depth is not a contradiction: it says nothing.
	unrestricted := base
	unrestricted.SupportsHDR = true
	unrestricted.MaxBitDepth = 0
	if err := unrestricted.Validate(); err != nil {
		t.Errorf("HDR with no declared bit depth cap should be valid: %v", err)
	}

	contradictory := base
	contradictory.SupportsHDR = true
	contradictory.MaxBitDepth = 8
	err := contradictory.Validate()
	if err == nil {
		t.Fatal("HDR with an 8-bit cap is self-contradictory and must be refused")
	}
	if !strings.Contains(err.Error(), "supports_hdr") {
		t.Errorf("the error should name the offending field, got %v", err)
	}

	// Normalise must not quietly drop the declaration, which would deliver
	// tone-mapped SDR to a client that can do better.
	if !hdr.Normalise().SupportsHDR {
		t.Error("Normalise dropped SupportsHDR")
	}
}

// TestBrowserCapability_IsSDR pins the default. Nothing about an arbitrary
// browser proves it can render HDR, and assuming it would hand a PQ stream to a
// compositor that shows it washed out - the bug this work fixes.
func TestBrowserCapability_IsSDR(t *testing.T) {
	t.Parallel()

	if BrowserCapability().SupportsHDR {
		t.Error("the browser profile must not claim HDR support on the client's behalf")
	}
}

// TestClientCapability_PreferredVideoCodecForHDR checks the HDR-specific
// preference, which must never fall back to a codec that cannot carry HDR.
func TestClientCapability_PreferredVideoCodecForHDR(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		codecs []string
		want   string
	}{
		{name: "H.264 alone has no HDR answer", codecs: []string{"h264"}, want: ""},
		{name: "HEVC is preferred over AV1", codecs: []string{"av1", "hevc"}, want: "hevc"},
		{name: "HEVC beats H.264 even though H.264 is listed", codecs: []string{"h264", "hevc"}, want: "hevc"},
		{name: "AV1 when there is no HEVC", codecs: []string{"h264", "av1"}, want: "av1"},
		{name: "VP9 is better than nothing", codecs: []string{"h264", "vp9"}, want: "vp9"},
		{name: "an empty list has no answer", codecs: nil, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			capability := ClientCapability{VideoCodecs: tt.codecs}
			if got := capability.PreferredVideoCodecForHDR(); got != tt.want {
				t.Errorf("PreferredVideoCodecForHDR(%v) = %q, want %q", tt.codecs, got, tt.want)
			}
		})
	}
}

// TestClientCapability_BitrateFloor covers a limit that is not a small budget
// but an unplayable one, refused as malformed rather than encoded into a
// slideshow.
func TestClientCapability_BitrateFloor(t *testing.T) {
	t.Parallel()

	base := ClientCapability{
		Containers:  []string{"hls"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
	}

	tooLow := base
	tooLow.MaxBitrateKbps = 50
	if err := tooLow.Validate(); err == nil {
		t.Error("a 50 kbps limit should be refused as malformed")
	}

	atFloor := base
	atFloor.MaxBitrateKbps = minVideoBitrateKbps
	if err := atFloor.Validate(); err != nil {
		t.Errorf("the floor itself should be accepted: %v", err)
	}

	unlimited := base
	unlimited.MaxBitrateKbps = 0
	if err := unlimited.Validate(); err != nil {
		t.Errorf("an omitted limit means unrestricted and must be valid: %v", err)
	}
}
