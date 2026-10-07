package streaming

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

// PlaybackMode is how the server intends to deliver a MediaObject.
type PlaybackMode string

const (
	// ModeDirectPlay serves the original file untouched. Segmenting is done by
	// the client, so no server-side work is needed.
	ModeDirectPlay PlaybackMode = "direct_play"
	// ModeRemux repackages the original streams into HLS without re-encoding.
	// The picture and sound quality are bit-identical to the source.
	ModeRemux PlaybackMode = "remux"
	// ModeTranscode re-encodes the streams that the client cannot handle.
	ModeTranscode PlaybackMode = "transcode"
)

// Action is what happens to one stream during delivery.
type Action string

const (
	ActionCopy      Action = "copy"
	ActionTranscode Action = "transcode"
	ActionNone      Action = "none"
)

// Decision is the outcome of capability negotiation.
type Decision struct {
	Mode PlaybackMode `json:"mode"`
	// Deliverable is false when the client's declaration makes delivery
	// impossible (for example it needs re-encoding but cannot play HLS).
	Deliverable bool   `json:"deliverable"`
	Container   string `json:"container"`

	VideoAction Action `json:"video_action"`
	AudioAction Action `json:"audio_action"`

	TargetVideoCodec string `json:"target_video_codec,omitempty"`
	TargetAudioCodec string `json:"target_audio_codec,omitempty"`
	TargetHeight     int    `json:"target_height,omitempty"`

	// Reasons explains every choice, in order. This is what makes a surprising
	// decision debuggable instead of mysterious.
	Reasons []string `json:"reasons"`
}

// Negotiate decides how to deliver a media file to a client. It is pure: the
// same inputs always produce the same decision, which is what makes the
// behaviour testable and the KPI measurements comparable.
func Negotiate(info *MediaInfo, capability ClientCapability) Decision {
	capability = capability.Normalise()

	decision := Decision{
		Deliverable: true,
		VideoAction: ActionCopy,
		AudioAction: ActionCopy,
		Reasons:     []string{},
	}

	if info == nil {
		decision.Deliverable = false
		decision.Reasons = append(decision.Reasons, "no media information available")
		return decision
	}

	videoCompatible := clientSupportsVideo(capability, info.VideoCodec)
	if !videoCompatible {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("client cannot decode video codec %q", info.VideoCodec))
	}

	audioCompatible := info.AudioCodec == "" || capability.SupportsAudio(info.AudioCodec)
	if !audioCompatible {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("client cannot decode audio codec %q", info.AudioCodec))
	}

	containerCompatible := capability.SupportsContainer(info.Container)
	if !containerCompatible {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("client cannot open container %q", info.Container))
	}

	needsDownscale := capability.MaxHeight > 0 && info.Height > capability.MaxHeight
	if needsDownscale {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("source is %dp but the client accepts at most %dp", info.Height, capability.MaxHeight))
	}

	switch {
	case !videoCompatible || !audioCompatible || needsDownscale:
		decision.Mode = ModeTranscode
	case containerCompatible:
		decision.Mode = ModeDirectPlay
	default:
		decision.Mode = ModeRemux
	}

	// Video action.
	if !videoCompatible || needsDownscale {
		decision.VideoAction = ActionTranscode
		decision.TargetVideoCodec = capability.PreferredVideoCodec()
		if decision.TargetVideoCodec == "" {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons, "client declared no video codecs to transcode into")
		}
		if needsDownscale {
			decision.TargetHeight = capability.MaxHeight
		}
	}

	// Audio action.
	if info.AudioCodec == "" {
		decision.AudioAction = ActionNone
	} else if !audioCompatible {
		decision.AudioAction = ActionTranscode
		decision.TargetAudioCodec = capability.PreferredAudioCodec()
		if decision.TargetAudioCodec == "" {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons, "client declared no audio codecs to transcode into")
		}
	}

	if decision.Mode == ModeDirectPlay {
		decision.Container = info.Container
		decision.Reasons = append(decision.Reasons,
			"direct play: the client supports the source container, video and audio")
		return decision
	}

	decision.Container = "hls"
	if !capability.SupportsHLS {
		decision.Deliverable = false
		decision.Reasons = append(decision.Reasons,
			"the source needs repackaging or re-encoding but the client does not support HLS")
		return decision
	}

	if decision.Mode == ModeRemux {
		decision.Reasons = append(decision.Reasons,
			"remux: the streams are compatible but the container is not, so they are copied into HLS unchanged")
	} else {
		decision.Reasons = append(decision.Reasons,
			"transcode: at least one stream is incompatible with the client")
	}

	return decision
}

// clientSupportsVideo treats an unknown source codec as incompatible rather
// than optimistically assuming the client can play it.
func clientSupportsVideo(capability ClientCapability, codec string) bool {
	if strings.TrimSpace(codec) == "" {
		return false
	}
	return capability.SupportsVideo(codec)
}

// ServerCapability describes what this server can actually do: which encoders
// exist and whether a hardware acceleration path is usable.
type ServerCapability struct {
	FFmpegAvailable      bool     `json:"ffmpeg_available"`
	FFprobeAvailable     bool     `json:"ffprobe_available"`
	HardwareAcceleration string   `json:"hardware_acceleration,omitempty"`
	VideoEncoders        []string `json:"video_encoders,omitempty"`
	HLS                  bool     `json:"hls"`
}

// encoderLineRe matches one line of `ffmpeg -encoders` output. Requiring an
// identifier after the six flag characters skips the legend lines such as
// " V..... = Video".
var encoderLineRe = regexp.MustCompile(`(?m)^\s*[A-Z.]{6}\s+([A-Za-z0-9_][A-Za-z0-9_.-]*)`)

// ParseEncoders extracts encoder names from `ffmpeg -encoders` output.
func ParseEncoders(output string) []string {
	matches := encoderLineRe.FindAllStringSubmatch(output, -1)
	encoders := make([]string, 0, len(matches))
	for _, match := range matches {
		encoders = append(encoders, match[1])
	}
	sort.Strings(encoders)
	return encoders
}

// DetectServerCapability inspects the host. dockerDeviceDir is checked for the
// presence of a hardware transcoding device, which is what makes QuickSync or
// VAAPI usable.
func DetectServerCapability(ctx context.Context, ffmpegBin, ffprobeBin, deviceDir string) ServerCapability {
	if deviceDir == "" {
		deviceDir = "/dev/dri"
	}

	capability := ServerCapability{HLS: true}

	if _, err := exec.LookPath(ffprobeBin); err == nil {
		capability.FFprobeAvailable = true
	}
	if _, err := exec.LookPath(ffmpegBin); err != nil {
		return capability
	}
	capability.FFmpegAvailable = true

	cmd := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-encoders")
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return capability
	}
	capability.VideoEncoders = videoEncodersOnly(ParseEncoders(stdout.String()))

	hasDevice := false
	if entries, err := os.ReadDir(deviceDir); err == nil && len(entries) > 0 {
		hasDevice = true
	}

	switch {
	case hasDevice && containsFold(capability.VideoEncoders, "h264_qsv"):
		capability.HardwareAcceleration = "qsv"
	case hasDevice && containsFold(capability.VideoEncoders, "h264_vaapi"):
		capability.HardwareAcceleration = "vaapi"
	}

	return capability
}

// videoEncodersOnly keeps the encoders this project can actually target.
func videoEncodersOnly(encoders []string) []string {
	known := map[string]bool{
		"h264_qsv": true, "hevc_qsv": true,
		"h264_vaapi": true, "hevc_vaapi": true,
		"libx264": true, "libx265": true,
		"libvpx-vp9": true, "libaom-av1": true, "libsvtav1": true,
	}
	kept := make([]string, 0, len(encoders))
	for _, encoder := range encoders {
		if known[encoder] {
			kept = append(kept, encoder)
		}
	}
	return kept
}

// EncoderFor returns the ffmpeg encoder to use for a target codec, preferring
// hardware acceleration when the host offers a usable path.
//
// Note that an encoder appearing in `ffmpeg -encoders` only means it was
// compiled in; a working QuickSync or VAAPI path additionally requires the
// device, which is what ServerCapability.HardwareAcceleration records.
func EncoderFor(codec string, server ServerCapability) string {
	codec = NormaliseVideoCodec(codec)

	preferred := map[string][]string{
		"h264": {"h264_qsv", "h264_vaapi", "libx264"},
		"hevc": {"hevc_qsv", "hevc_vaapi", "libx265"},
		"vp9":  {"libvpx-vp9"},
		"av1":  {"libsvtav1", "libaom-av1"},
	}[codec]

	hardwareSuffix := map[string]string{
		"qsv":   "_qsv",
		"vaapi": "_vaapi",
	}[server.HardwareAcceleration]

	for _, candidate := range preferred {
		if isHardwareEncoder(candidate) {
			if hardwareSuffix == "" || !strings.HasSuffix(candidate, hardwareSuffix) {
				continue
			}
			if !containsFold(server.VideoEncoders, candidate) {
				continue
			}
		}
		return candidate
	}
	return ""
}

func isHardwareEncoder(name string) bool {
	return strings.HasSuffix(name, "_qsv") || strings.HasSuffix(name, "_vaapi")
}
