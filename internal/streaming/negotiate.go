package streaming

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
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
	// TargetAudioChannels is set when the source carries more channels than the
	// client accepts and the audio is being re-encoded anyway.
	TargetAudioChannels int `json:"target_audio_channels,omitempty"`

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

	targetHeight, needsDownscale := targetHeightFor(info, capability)
	if needsDownscale {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("source is %dx%d but the client accepts at most %s",
				info.Width, info.Height, describeBox(capability)))
	}

	channelsTooMany := capability.MaxAudioChannels > 0 &&
		info.AudioChannels > capability.MaxAudioChannels
	if channelsTooMany {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("source audio has %d channels but the client accepts at most %d",
				info.AudioChannels, capability.MaxAudioChannels))
	}

	// A codec name the client accepts does not mean it can decode this stream:
	// 10-bit H.264 ("High 10") is refused by every browser's media pipeline.
	tooDeep := info.BitDepth > 8 &&
		(capability.MaxBitDepth == 0 || info.BitDepth > capability.MaxBitDepth)
	if tooDeep {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("source is %d-bit but the client decodes at most %d-bit",
				info.BitDepth, capability.MaxBitDepth))
	}

	switch {
	case !videoCompatible || !audioCompatible || needsDownscale || tooDeep || channelsTooMany:
		decision.Mode = ModeTranscode
	case containerCompatible:
		decision.Mode = ModeDirectPlay
	default:
		decision.Mode = ModeRemux
	}

	// Video action.
	if !videoCompatible || needsDownscale || tooDeep {
		decision.VideoAction = ActionTranscode
		decision.TargetVideoCodec = capability.PreferredVideoCodec()
		if decision.TargetVideoCodec == "" {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons, "client declared no video codecs to transcode into")
		}
		if needsDownscale {
			decision.TargetHeight = targetHeight
		}
	}

	// Audio action.
	if info.AudioCodec == "" {
		decision.AudioAction = ActionNone
	} else if !audioCompatible || channelsTooMany {
		decision.AudioAction = ActionTranscode
		decision.TargetAudioCodec = capability.PreferredAudioCodec()
		if decision.TargetAudioCodec == "" {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons, "client declared no audio codecs to transcode into")
		}
		if channelsTooMany {
			decision.TargetAudioChannels = capability.MaxAudioChannels
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

// targetHeightFor reports the height to scale to so the output fits inside the
// client's declared box, or 0 when no scaling is needed.
//
// Both limits matter. A 2.35:1 film scaled to fit a 1080-high limit comes out
// roughly 2530 wide, which breaks a client that also declared a 1920 width.
// The tighter of the two, expressed in the source's aspect ratio, wins.
func targetHeightFor(info *MediaInfo, capability ClientCapability) (int, bool) {
	if info.Height <= 0 || info.Width <= 0 {
		return 0, false
	}
	if capability.MaxHeight <= 0 && capability.MaxWidth <= 0 {
		return 0, false
	}

	limit := info.Height
	if capability.MaxHeight > 0 && capability.MaxHeight < limit {
		limit = capability.MaxHeight
	}
	if capability.MaxWidth > 0 && info.Width > capability.MaxWidth {
		if byWidth := capability.MaxWidth * info.Height / info.Width; byWidth < limit {
			limit = byWidth
		}
	}

	if limit >= info.Height {
		return 0, false
	}
	if limit < 2 {
		limit = 2
	}
	return limit, true
}

// describeBox renders a client's resolution limit for a human.
func describeBox(capability ClientCapability) string {
	switch {
	case capability.MaxWidth > 0 && capability.MaxHeight > 0:
		return fmt.Sprintf("%dx%d", capability.MaxWidth, capability.MaxHeight)
	case capability.MaxHeight > 0:
		return fmt.Sprintf("%dp", capability.MaxHeight)
	case capability.MaxWidth > 0:
		return fmt.Sprintf("%dpx wide", capability.MaxWidth)
	default:
		return "no resolution limit"
	}
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

// DetectServerCapability inspects the host.
//
// Hardware encoders are verified by running one, never inferred. Being listed
// in `ffmpeg -encoders` only means the encoder was compiled in, and the presence
// of a device node does not mean it can be driven: an AMD machine exposes
// /dev/dri exactly as an Intel one does, so a QuickSync encoder can be selected
// on a host where QuickSync cannot work at all. Both signals together are still
// not proof, and trusting them produces a server that fails every transcode
// with "Error creating a MFX session".
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

	// Software encoders are trusted once listed; hardware encoders have to
	// prove themselves, because the failure mode of a wrong guess is that no
	// transcode works at all.
	for _, encoder := range videoEncodersOnly(ParseEncoders(stdout.String())) {
		if isHardwareEncoder(encoder) && !encoderWorks(ctx, ffmpegBin, encoder, deviceDir) {
			continue
		}
		capability.VideoEncoders = append(capability.VideoEncoders, encoder)
	}

	switch {
	case containsFold(capability.VideoEncoders, "h264_qsv"),
		containsFold(capability.VideoEncoders, "hevc_qsv"):
		capability.HardwareAcceleration = "qsv"
	case containsFold(capability.VideoEncoders, "h264_vaapi"),
		containsFold(capability.VideoEncoders, "hevc_vaapi"):
		capability.HardwareAcceleration = "vaapi"
	}

	return capability
}

// encoderTime out bounds a single capability probe.
const encoderProbeTimeout = 20 * time.Second

// encoderWorks encodes a fraction of a second to nowhere. A missing driver, an
// unusable device or the wrong GPU vendor all fail here, which is exactly the
// condition that matters.
func encoderWorks(ctx context.Context, ffmpegBin, encoder, deviceDir string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, encoderProbeTimeout)
	defer cancel()

	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=5:duration=0.2",
	}

	if strings.HasSuffix(encoder, "_vaapi") {
		// VAAPI needs a render node pointed at explicitly.
		device, ok := firstRenderNode(deviceDir)
		if !ok {
			return false
		}
		args = append(args, "-vaapi_device", device, "-vf", "format=nv12,hwupload")
	}

	args = append(args, "-c:v", encoder, "-f", "null", "-")

	probe := exec.CommandContext(probeCtx, ffmpegBin, args...)
	probe.Stdout = io.Discard
	probe.Stderr = io.Discard
	return probe.Run() == nil
}

// firstRenderNode finds a DRM render node, which is what VAAPI requires.
func firstRenderNode(deviceDir string) (string, bool) {
	entries, err := os.ReadDir(deviceDir)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "renderD") {
			return filepath.Join(deviceDir, entry.Name()), true
		}
	}
	return "", false
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
