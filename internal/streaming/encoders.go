package streaming

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// This file owns everything about video encoders: which ones exist, which
// hardware families they belong to, how they are preferred, and the ffmpeg
// arguments each needs.
//
// The probe that verifies an encoder at startup and the code that builds a real
// session's command line both render their options from the functions here.
// That is deliberate: they previously had separate copies, and the copies
// disagreed - a real VAAPI session was built without the -vaapi_device that the
// probe passed, so the probe could pass while playback failed.

// hardwareFamily is a group of encoders sharing a vendor's API.
type hardwareFamily struct {
	name string
	// suffix is what the encoders of this family end with, which is how ffmpeg
	// names them: h264_nvenc, hevc_vaapi, av1_amf.
	suffix string
}

// hardwareFamilyPreference orders the families, best first, for a host that has
// more than one - an NVIDIA card beside an Intel iGPU is a common arrangement.
// NVENC leads because on comparable hardware it is the fastest and best of
// them; the software encoders are not here because they are not a family.
var hardwareFamilyPreference = []hardwareFamily{
	{name: "nvenc", suffix: "_nvenc"},
	{name: "qsv", suffix: "_qsv"},
	{name: "videotoolbox", suffix: "_videotoolbox"},
	{name: "vaapi", suffix: "_vaapi"},
	{name: "amf", suffix: "_amf"},
}

// hardwareFamilyOf reports which family an encoder belongs to, if any.
func hardwareFamilyOf(encoder string) (hardwareFamily, bool) {
	for _, family := range hardwareFamilyPreference {
		if strings.HasSuffix(encoder, family.suffix) {
			return family, true
		}
	}
	return hardwareFamily{}, false
}

// isHardwareEncoder reports whether an encoder is backed by a device. Hardware
// encoders are the ones that have to prove they work before being offered;
// software encoders are trusted once ffmpeg lists them.
func isHardwareEncoder(name string) bool {
	_, ok := hardwareFamilyOf(name)
	return ok
}

// hardwareFamilies lists the families present in a verified encoder list, in
// preference order. A host can have several, which is why the capability reports
// a list rather than a single name.
func hardwareFamilies(encoders []string) []string {
	families := make([]string, 0, len(hardwareFamilyPreference))
	for _, family := range hardwareFamilyPreference {
		for _, encoder := range encoders {
			if strings.HasSuffix(encoder, family.suffix) {
				families = append(families, family.name)
				break
			}
		}
	}
	return families
}

// videoEncoderCandidates lists, per codec, the encoders that can produce it in
// preference order: hardware before software, and within hardware the family
// order above.
//
// This table is the single source of truth for which encoders this project knows
// about. EncoderFor reads it to choose, and videoEncodersOnly reads it to decide
// which of ffmpeg's listed encoders are ours, so a new encoder cannot be added
// to one and forgotten in the other.
var videoEncoderCandidates = map[string][]string{
	"h264": {
		"h264_nvenc", "h264_qsv", "h264_videotoolbox", "h264_vaapi", "h264_amf",
		"libx264",
	},
	"hevc": {
		"hevc_nvenc", "hevc_qsv", "hevc_videotoolbox", "hevc_vaapi", "hevc_amf",
		"libx265",
	},
	"vp9": {
		"libvpx-vp9",
	},
	"av1": {
		"av1_nvenc", "av1_qsv", "av1_videotoolbox", "av1_vaapi", "av1_amf",
		"libsvtav1", "libaom-av1",
	},
}

// EncoderFor returns the encoder to use for a codec on this host, or "" when the
// host has none.
//
// It reads ServerCapability.VideoEncoders, which holds only encoders that were
// verified by running one. That list is the authority; nothing else needs to be
// consulted, so a family can never be selected that the hardware check rejected.
func EncoderFor(codec string, server ServerCapability) string {
	for _, candidate := range videoEncoderCandidates[NormaliseVideoCodec(codec)] {
		if containsFold(server.VideoEncoders, candidate) {
			return candidate
		}
	}
	return ""
}

// videoEncodersOnly keeps the encoders this project can actually target.
func videoEncodersOnly(encoders []string) []string {
	known := make(map[string]bool)
	for _, candidates := range videoEncoderCandidates {
		for _, name := range candidates {
			known[name] = true
		}
	}

	kept := make([]string, 0, len(encoders))
	for _, encoder := range encoders {
		if known[encoder] {
			kept = append(kept, encoder)
		}
	}
	return kept
}

// audioEncodersOnly keeps the audio encoders this project knows how to ask for.
// Unlike the video encoders these are not proved by running one: an audio
// encoder that is listed is reliable, and proving each would slow startup for no
// real gain.
func audioEncodersOnly(encoders []string) []string {
	known := map[string]bool{
		"aac": true, "libopus": true, "libmp3lame": true, "libvorbis": true,
		"ac3": true, "eac3": true, "flac": true, "libfdk_aac": true,
	}
	kept := make([]string, 0, len(encoders))
	for _, encoder := range encoders {
		if known[encoder] {
			kept = append(kept, encoder)
		}
	}
	sort.Strings(kept)
	return kept
}

// encoderDevice is the host resource an encoder may need.
type encoderDevice struct {
	// RenderNode is a DRM render node such as /dev/dri/renderD128, empty when
	// none was found.
	RenderNode string
}

// encoderInputArgs returns the options that must precede the input.
//
// Only VAAPI has any, and it needs them: its upload filter has no device to
// upload frames to without -vaapi_device, and the option is global, so putting
// it after -i is not the same thing. ffmpeg reports "a hardware device reference
// is required to upload frames to" when it is missing.
func encoderInputArgs(encoder string, dev encoderDevice) []string {
	if encoder == "" || !strings.HasSuffix(encoder, "_vaapi") || dev.RenderNode == "" {
		return nil
	}
	return []string{"-vaapi_device", dev.RenderNode}
}

// encoderOutputArgs returns the encoder options for a session.
//
// Each family has its own vocabulary for the same intent - "good quality, let
// the bitrate follow" - and they are not interchangeable:
//
//   - NVENC takes a preset from p1..p7 and constant quality through
//     "-rc vbr -cq N". No -b:v, so quality decides the bitrate.
//   - QSV uses -global_quality.
//   - VAAPI has no quality knob at all; it takes a hardware surface through the
//     upload filter and encodes it.
//   - AMF uses -quality for the speed/quality preset and QP values for constant
//     quality, which it infers from the presence of -qp_*.
//   - VideoToolbox is quality-driven through -q:v. -allow_sw stays off, so a
//     machine without the hardware says so instead of quietly using its CPU.
func encoderOutputArgs(encoder string, targetHeight int, dev encoderDevice) []string {
	softwareScale := ""
	if targetHeight > 0 {
		softwareScale = fmt.Sprintf("scale=-2:%d", targetHeight)
	}

	var args []string
	switch {
	case strings.HasSuffix(encoder, "_nvenc"):
		args = []string{"-c:v", encoder, "-preset", nvencPreset, "-tune", "hq", "-rc", "vbr", "-cq", "22"}
		if softwareScale != "" {
			args = append(args, "-vf", softwareScale)
		}
	case strings.HasSuffix(encoder, "_qsv"):
		args = []string{"-c:v", encoder, "-preset", "veryfast", "-global_quality", "22"}
		if softwareScale != "" {
			args = append(args, "-vf", softwareScale)
		}
	case strings.HasSuffix(encoder, "_vaapi"):
		// Software scaling happens before the upload, hardware scaling after it.
		filter := "format=nv12,hwupload"
		if targetHeight > 0 {
			filter += fmt.Sprintf(",scale_vaapi=w=-2:h=%d", targetHeight)
		}
		args = []string{"-c:v", encoder, "-vf", filter}
	case strings.HasSuffix(encoder, "_amf"):
		args = []string{"-c:v", encoder, "-quality", "balanced",
			"-rc", "cqp", "-qp_i", "22", "-qp_p", "22", "-qp_b", "22"}
		if softwareScale != "" {
			args = append(args, "-vf", softwareScale)
		}
	case strings.HasSuffix(encoder, "_videotoolbox"):
		args = []string{"-c:v", encoder, "-q:v", "60"}
		if softwareScale != "" {
			args = append(args, "-vf", softwareScale)
		}
	case encoder == "libx264" || encoder == "libx265":
		args = []string{"-c:v", encoder, "-preset", "veryfast", "-crf", "21"}
		if softwareScale != "" {
			args = append(args, "-vf", softwareScale)
		}
	case encoder == "libvpx-vp9":
		args = []string{"-c:v", encoder, "-crf", "31", "-b:v", "0"}
		if softwareScale != "" {
			args = append(args, "-vf", softwareScale)
		}
	case encoder == "libsvtav1" || encoder == "libaom-av1":
		args = []string{"-c:v", encoder, "-crf", "30"}
		if softwareScale != "" {
			args = append(args, "-vf", softwareScale)
		}
	default:
		args = []string{"-c:v", encoder}
	}

	if format := outputPixelFormat(encoder); format != "" {
		args = append(args, "-pix_fmt", format)
	}
	return args
}

// nvencPreset is the speed/quality preset for NVENC. The range is p1 (fastest,
// worst) to p7 (slowest, best); p4 is the documented default and the balanced
// choice, and the encoding quality is set by -cq rather than by the preset.
const nvencPreset = "p4"

// outputPixelFormat pins the encoder's output to a format browsers can decode.
//
// This matters more than it looks. Left alone, ffmpeg preserves the source's
// bit depth, so a 10-bit HDR source transcodes to 10-bit H.264 "High 10" - which
// no browser decodes through Media Source Extensions. The stream then attaches,
// fetches its segments, and silently never plays: no media error, no console
// message, just a stalled player.
//
// VAAPI is the exception and returns nothing: its upload filter already pins
// nv12, and the encoder consumes hardware surfaces, so a -pix_fmt would ask for
// software frames instead of describing the output.
func outputPixelFormat(encoder string) string {
	switch {
	case strings.HasSuffix(encoder, "_vaapi"):
		return ""
	case isHardwareEncoder(encoder):
		// NVENC, QuickSync, AMF and VideoToolbox all accept nv12 natively.
		return "nv12"
	default:
		return "yuv420p"
	}
}

// encoderProbeHeight is the target height used when verifying an encoder. It is
// deliberately not zero: a non-zero height exercises the scaling filter too, so
// a scaling path the encoder cannot support is caught at startup rather than
// during playback.
const encoderProbeHeight = 180

// encoderProbeTimeout bounds a single capability probe.
const encoderProbeTimeout = 20 * time.Second

// probeEncoder runs one encode with the options a real session would use and
// reports why it failed, if it did.
//
// The reason is the point. On a machine with an Intel GPU or an NVIDIA card,
// "h264_qsv is listed but cannot open a session: Error creating a MFX session"
// is the difference between a fixable driver problem and a mystery. An encoder
// that is silently dropped is the least useful outcome for whoever has to
// diagnose it.
func probeEncoder(ctx context.Context, ffmpegBin, encoder string, dev encoderDevice) error {
	probeCtx, cancel := context.WithTimeout(ctx, encoderProbeTimeout)
	defer cancel()

	args := []string{"-hide_banner", "-loglevel", "error"}
	args = append(args, encoderInputArgs(encoder, dev)...)
	args = append(args, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=5:duration=0.2")
	args = append(args, encoderOutputArgs(encoder, encoderProbeHeight, dev)...)
	args = append(args, "-f", "null", "-")

	stderr := &boundedBuffer{limit: probeStderrLimit}
	probe := exec.CommandContext(probeCtx, ffmpegBin, args...)
	probe.Stdout = io.Discard
	probe.Stderr = stderr

	if err := probe.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, firstComplaint(stderr.String()))
	}
	return nil
}

// encoderWorks reports whether an encoder can actually open a session.
func encoderWorks(ctx context.Context, ffmpegBin, encoder string, dev encoderDevice) bool {
	return probeEncoder(ctx, ffmpegBin, encoder, dev) == nil
}

// EncoderRejection records a hardware encoder that ffmpeg lists but this host
// cannot use, and what it said about it.
type EncoderRejection struct {
	Encoder string `json:"encoder"`
	Reason  string `json:"reason"`
}

// probeStderrLimit bounds what a probe captures. A broken encoder can produce a
// great deal of output and only the first useful line is reported.
const probeStderrLimit = 4096

// firstComplaint picks the line most likely to explain a failure: the first
// non-empty one, which is where ffmpeg states the cause before it unwinds.
func firstComplaint(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// ffmpeg prefixes its lines with the component and an address.
		if idx := strings.Index(line, "] "); strings.HasPrefix(line, "[") && idx != -1 {
			line = strings.TrimSpace(line[idx+2:])
		}
		return line
	}
	return "ffmpeg gave no reason"
}

// boundedBuffer keeps at most limit bytes and silently drops the rest, so a
// runaway encoder cannot balloon the process while we diagnose it.
type boundedBuffer struct {
	limit int
	buf   strings.Builder
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if remaining := b.limit - b.buf.Len(); remaining > 0 {
		if len(p) > remaining {
			b.buf.Write(p[:remaining])
		} else {
			b.buf.Write(p)
		}
	}
	// Report the full length so ffmpeg does not treat this as a short write.
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }

// firstRenderNode finds a DRM render node, which is what VAAPI requires.
func firstRenderNode(deviceDir string) string {
	entries, err := os.ReadDir(deviceDir)
	if err != nil {
		return ""
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "renderD") {
			return filepath.Join(deviceDir, entry.Name())
		}
	}
	return ""
}
