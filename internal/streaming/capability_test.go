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
