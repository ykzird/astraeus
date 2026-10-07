// Package streaming implements the Astraeus media engine: it decides how a
// media file should be delivered to a particular client and produces the
// segmented stream when the source cannot be played directly.
package streaming

import (
	"fmt"
	"strings"
)

// ClientCapability is a client's declaration of what it can play. It is what
// makes proactive negotiation possible: the client describes itself once, and
// the server decides instead of guessing.
type ClientCapability struct {
	// Containers the client can open, e.g. "mp4", "webm", "hls".
	Containers []string `json:"containers"`
	// VideoCodecs, e.g. "h264", "hevc", "av1", "vp9".
	VideoCodecs []string `json:"video_codecs"`
	// AudioCodecs, e.g. "aac", "ac3", "eac3", "opus".
	AudioCodecs []string `json:"audio_codecs"`
	// MaxWidth and MaxHeight are the largest dimensions the client will
	// accept; zero means unrestricted.
	MaxWidth  int `json:"max_width"`
	MaxHeight int `json:"max_height"`
	// MaxBitrateKbps caps the acceptable bitrate; zero means unrestricted.
	MaxBitrateKbps int `json:"max_bitrate_kbps"`
	// MaxBitDepth is the deepest video the client can decode. Browsers cannot
	// decode 10-bit H.264 even though they accept the codec name, so this is
	// what stops a 10-bit source being direct-played into a stalled player.
	// Zero means unrestricted.
	MaxBitDepth int `json:"max_bit_depth"`
	// MaxAudioChannels is the most channels the client can decode. Chromium's
	// media pipeline refuses a 5.1 AAC SourceBuffer, and browsers output stereo
	// in practice, so this defaults to 2 for browser clients. Zero means
	// unrestricted.
	MaxAudioChannels int `json:"max_audio_channels"`
	// SupportsHLS enables the segmented delivery modes.
	SupportsHLS bool `json:"supports_hls"`
	// Subtitles indicates the client can render subtitle tracks.
	Subtitles bool `json:"subtitles"`
}

// BrowserCapability returns the profile of a typical modern HTML5 browser.
// It is the default the API falls back to when a client sends none.
//
// The 1080p ceiling is deliberate. Browsers can only be relied on to decode
// H.264 up to 1080p in software; 4K playback needs hardware decoding that is
// not universally available, and a stream the decoder cannot handle stalls
// with no useful error. Capping here also avoids re-encoding a 4K source at
// 4K, which costs roughly four times the CPU of 1080p for content usually
// shown in a window. A client that wants more can ask for it in its manifest.
func BrowserCapability() ClientCapability {
	return ClientCapability{
		Containers:       []string{"mp4", "webm", "hls"},
		VideoCodecs:      []string{"h264", "vp9", "av1"},
		AudioCodecs:      []string{"aac", "opus", "mp3", "vorbis"},
		MaxWidth:         1920,
		MaxHeight:        1080,
		MaxBitrateKbps:   120_000,
		MaxBitDepth:      8,
		MaxAudioChannels: 2,
		SupportsHLS:      true,
		Subtitles:        true,
	}
}

// Validate rejects a capability declaration the server cannot act on, rather
// than silently producing an unplayable decision.
func (c ClientCapability) Validate() error {
	if len(c.VideoCodecs) == 0 {
		return fmt.Errorf("capability must list at least one video codec")
	}
	if len(c.Containers) == 0 {
		return fmt.Errorf("capability must list at least one container")
	}
	if len(c.AudioCodecs) == 0 {
		return fmt.Errorf("capability must list at least one audio codec")
	}
	if c.MaxWidth < 0 || c.MaxHeight < 0 || c.MaxBitrateKbps < 0 {
		return fmt.Errorf("capability limits must not be negative")
	}
	return nil
}

// Normalise lowercases and canonicalises the codec and container names so that
// "H265", "h.265" and "hevc" are all understood.
func (c ClientCapability) Normalise() ClientCapability {
	return ClientCapability{
		Containers:       normaliseAll(c.Containers, normaliseContainer),
		VideoCodecs:      normaliseAll(c.VideoCodecs, NormaliseVideoCodec),
		AudioCodecs:      normaliseAll(c.AudioCodecs, NormaliseAudioCodec),
		MaxWidth:         c.MaxWidth,
		MaxHeight:        c.MaxHeight,
		MaxBitrateKbps:   c.MaxBitrateKbps,
		MaxBitDepth:      c.MaxBitDepth,
		MaxAudioChannels: c.MaxAudioChannels,
		SupportsHLS:      c.SupportsHLS,
		Subtitles:        c.Subtitles,
	}
}

// SupportsVideo reports whether the client can decode the given video codec.
func (c ClientCapability) SupportsVideo(codec string) bool {
	return containsFold(c.VideoCodecs, NormaliseVideoCodec(codec))
}

// SupportsAudio reports whether the client can decode the given audio codec.
func (c ClientCapability) SupportsAudio(codec string) bool {
	return containsFold(c.AudioCodecs, NormaliseAudioCodec(codec))
}

// SupportsContainer reports whether the client can open the container. ffprobe
// reports containers as a comma-separated list ("matroska,webm"), so each is
// tested.
func (c ClientCapability) SupportsContainer(container string) bool {
	for _, name := range strings.Split(container, ",") {
		if containsFold(c.Containers, normaliseContainer(name)) {
			return true
		}
	}
	return false
}

// videoCodecPreference is the order in which the server picks a target codec
// when it must re-encode. Wide hardware support comes first.
var videoCodecPreference = []string{"h264", "hevc", "vp9", "av1"}

// audioCodecPreference is the equivalent order for audio.
var audioCodecPreference = []string{"aac", "opus", "mp3", "ac3", "eac3", "flac"}

// PreferredVideoCodec returns the client's most broadly supported video codec,
// or "" when the client declared none.
func (c ClientCapability) PreferredVideoCodec() string {
	return firstPreferred(c.VideoCodecs, videoCodecPreference)
}

// PreferredAudioCodec returns the client's most broadly supported audio codec.
func (c ClientCapability) PreferredAudioCodec() string {
	return firstPreferred(c.AudioCodecs, audioCodecPreference)
}

func firstPreferred(available, preference []string) string {
	for _, want := range preference {
		if containsFold(available, want) {
			return want
		}
	}
	if len(available) > 0 {
		return available[0]
	}
	return ""
}

// NormaliseVideoCodec maps ffmpeg and common client names onto one vocabulary.
func NormaliseVideoCodec(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "h264", "avc", "avc1", "h.264", "x264":
		return "h264"
	case "h265", "hevc", "h.265", "hvc1", "hev1":
		return "hevc"
	case "av1", "av01":
		return "av1"
	case "vp8":
		return "vp8"
	case "vp9":
		return "vp9"
	case "mpeg2video", "mpeg2":
		return "mpeg2"
	case "vc1":
		return "vc1"
	default:
		return strings.ToLower(strings.TrimSpace(codec))
	}
}

// NormaliseAudioCodec maps audio codec names onto one vocabulary.
func NormaliseAudioCodec(codec string) string {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "aac", "aac_latm", "mp4a":
		return "aac"
	case "ac3", "ac-3":
		return "ac3"
	case "eac3", "ec-3", "eac-3":
		return "eac3"
	case "dts", "dca":
		return "dts"
	case "truehd":
		return "truehd"
	case "opus":
		return "opus"
	case "vorbis":
		return "vorbis"
	case "mp3", "mp3float":
		return "mp3"
	case "flac":
		return "flac"
	default:
		return strings.ToLower(strings.TrimSpace(codec))
	}
}

// normaliseContainer maps container names onto the short form clients use.
func normaliseContainer(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "mov", "mp4", "m4v", "mp4v", "isom", "iso", "avc1", "dash":
		return "mp4"
	case "matroska", "mkv":
		return "matroska"
	case "webm":
		return "webm"
	case "mpegts", "ts", "m2ts":
		return "mpegts"
	case "avi":
		return "avi"
	case "hls", "m3u8", "application/vnd.apple.mpegurl":
		return "hls"
	default:
		return strings.ToLower(strings.TrimSpace(name))
	}
}

func normaliseAll(values []string, normalise func(string) string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		normalised := normalise(value)
		if seen[normalised] {
			continue
		}
		seen[normalised] = true
		out = append(out, normalised)
	}
	return out
}

func containsFold(haystack []string, needle string) bool {
	for _, item := range haystack {
		if strings.EqualFold(strings.TrimSpace(item), needle) {
			return true
		}
	}
	return false
}
