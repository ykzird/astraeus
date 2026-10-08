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

// videoPlan is what a session intends to do to the picture, reduced to the
// parts that change ffmpeg's arguments.
type videoPlan struct {
	// Height is the target height, 0 when the picture needs no scaling.
	Height int
	// ToneMap converts HDR to SDR, which by definition produces 8-bit BT.709.
	ToneMap bool
	// HDRPixelFormat is the pixel format verified for a 10-bit HDR output. It is
	// empty for SDR output and is what makes a plan an HDR plan, so there is no
	// separate flag to disagree with it.
	HDRPixelFormat string
	// TargetRange is the dynamic range the output is meant to have. It is only
	// consulted when HDRPixelFormat is set, to pick the transfer function the
	// output is tagged with.
	TargetRange DynamicRange
	// BitrateKbps is the ceiling the video is held to, 0 when the source's own
	// rate is acceptable.
	BitrateKbps int
}

// HDR reports whether this plan keeps high dynamic range.
func (p videoPlan) HDR() bool { return p.HDRPixelFormat != "" }

// videoEncoderFor returns the encoder and the picture plan a decision will use.
//
// Choosing them together is deliberate: an HDR decision must be encoded by the
// encoder that was verified to produce 10-bit, and that is not necessarily the
// encoder the ordinary preference order picks - a host whose QuickSync cannot
// do 10-bit still has libx265 that can.
func videoEncoderFor(decision Decision, server ServerCapability) (string, videoPlan, error) {
	plans, encoder, err := videoPlansFor(decision, server)
	if err != nil {
		return "", videoPlan{}, err
	}
	return encoder, plans[0], nil
}

// videoPlansFor returns the encoder and one plan per rendition. A session with
// no ladder has exactly one plan, so the single-rendition path and the ladder
// path share every decision about colour, scaling and rate control.
func videoPlansFor(decision Decision, server ServerCapability) ([]videoPlan, string, error) {
	renditions := decision.Renditions
	if len(renditions) == 0 {
		renditions = []Rendition{{Height: decision.TargetHeight, BitrateKbps: decision.TargetBitrateKbps}}
	}

	planFor := func(rendition Rendition) videoPlan {
		return videoPlan{
			Height:      rendition.Height,
			ToneMap:     decision.ToneMap,
			TargetRange: decision.TargetDynamicRange,
			BitrateKbps: rendition.BitrateKbps,
		}
	}

	codec := decision.TargetVideoCodec
	if decision.TargetDynamicRange.IsHDR() {
		support, ok := EncoderForHDR(codec, server)
		if !ok {
			return nil, "", fmt.Errorf("no verified 10-bit HDR encoder for video codec %q", codec)
		}
		plans := make([]videoPlan, 0, len(renditions))
		for _, rendition := range renditions {
			plan := planFor(rendition)
			plan.HDRPixelFormat = support.PixelFormat
			plans = append(plans, plan)
		}
		return plans, support.Encoder, nil
	}

	encoder := EncoderFor(codec, server)
	if encoder == "" {
		return nil, "", fmt.Errorf("no ffmpeg encoder available for video codec %q", codec)
	}
	plans := make([]videoPlan, 0, len(renditions))
	for _, rendition := range renditions {
		plans = append(plans, planFor(rendition))
	}
	return plans, encoder, nil
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

// toneMapFilterChain converts HDR to SDR BT.709 in software.
//
// The shape is the documented one: linearise through zscale, hand float samples
// to the tonemap filter, then tag the result as conventional limited-range
// BT.709. The intermediate zscale=p=bt709 is what keeps the primaries
// conversion in linear light; applying it after the tone map would run the
// matrix over already-compressed values.
//
// hable is the compressor. It rolls highlights off smoothly instead of clipping
// them, which is what stops a window or a lamp becoming a white hole, and desat
// reins in the chroma that a luminance-only tone map would leave
// over-saturated.
//
// The colour properties are the zscale stages' doing, and they are what the
// output ends up tagged with: ffmpeg writes the encoder's VUI from the frame
// properties rather than from -color_primaries. A PQ source tone mapped through
// this chain and encoded with both libx264 and libx265 came out tagged
// bt709/bt709/bt709, which is what makes this chain's output verifiable rather
// than merely plausible.
//
// It is a software chain on purpose. libplacebo tone maps better and
// understands BT.2390 and Dolby Vision, but it needs a working Vulkan device,
// and on a host without one it fails the whole transcode rather than falling
// back - which is exactly the trade this project refuses to make.
const toneMapFilterChain = "zscale=t=linear,format=gbrpf32le,zscale=p=bt709," +
	"tonemap=tonemap=hable:desat=2,zscale=t=bt709:m=bt709:r=tv,format=yuv420p"

// hdrTagFilter states an HDR output's colour on the frames themselves.
//
// This is a filter rather than -color_primaries/-color_trc for a measured
// reason: those options did not reach the output at all. A 10-bit source
// re-encoded with them came back with unknown primaries and unknown transfer,
// because ffmpeg writes the encoder's VUI from the frame properties instead; and
// passing them beside -x265-params broke tags that -x265-params alone had got
// right. A player handed an untagged PQ stream reads it as BT.709 and shows
// exactly the washed-out picture this work exists to remove, so the tags are set
// where they are actually read. Verified through to the HLS segment a player
// fetches.
func hdrTagFilter(dynamicRange DynamicRange) string {
	return fmt.Sprintf("setparams=color_primaries=bt2020:color_trc=%s:colorspace=bt2020nc:range=limited",
		hdrTransferName(dynamicRange))
}

// videoFilters renders the -vf chain for a plan and an encoder.
//
// The two arrangements differ because VAAPI scales on the GPU after uploading
// and cannot convert a transfer function itself (tonemap_vaapi exists but has
// never been run on hardware here), so it tone maps in software first and
// scales afterwards. Everything else scales first and then tone maps: the float
// conversion is the expensive part, and doing it on a 1080p frame instead of a
// 4K one costs a quarter as much for a picture nobody can tell apart.
func videoFilters(encoder string, plan videoPlan) []string {
	scale := ""
	if plan.Height > 0 {
		scale = fmt.Sprintf("scale=-2:%d", plan.Height)
	}

	var filters []string
	if strings.HasSuffix(encoder, "_vaapi") {
		if plan.ToneMap {
			filters = append(filters, toneMapFilterChain)
		}
		if plan.HDR() {
			filters = append(filters, hdrTagFilter(plan.TargetRange))
		}
		format := "nv12"
		if plan.HDR() {
			format = plan.HDRPixelFormat
		}
		filters = append(filters, "format="+format, "hwupload")
		if plan.Height > 0 {
			filters = append(filters, fmt.Sprintf("scale_vaapi=w=-2:h=%d", plan.Height))
		}
		return filters
	}

	if scale != "" {
		filters = append(filters, scale)
	}
	if plan.ToneMap {
		filters = append(filters, toneMapFilterChain)
	}
	if plan.HDR() {
		filters = append(filters, hdrTagFilter(plan.TargetRange))
	}
	return filters
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
//
// videoStreamSuffix renders the stream specifier that makes an option apply to
// one rendition of a ladder.
//
// A single-rendition session is left unsuffixed so its command line stays the
// readable thing it always was, and because ffmpeg's "applies to every stream of
// this type" default is exactly right when there is only one. A ladder has to be
// explicit: without a specifier every option would land on the first video
// stream and both rungs would come out the same size and rate, which is a ladder
// in name only. (Verified while building this: two rungs without specifiers both
// came out 640x360.)
func videoStreamSuffix(streams, index int) string {
	if streams <= 1 {
		return ""
	}
	return fmt.Sprintf(":%d", index)
}

// encoderOutputArgs returns the encoder options for one rendition of a session.
//
// streams is how many renditions this session has, and index which one these
// options describe; they only change the argument spelling, never the settings,
// so a single-rendition session is byte-for-byte what it was before ladders
// existed.
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
func encoderOutputArgs(encoder string, plan videoPlan, dev encoderDevice, streams, index int) []string {
	suffix := videoStreamSuffix(streams, index)
	args := encoderVideoArgs(encoder, suffix)
	if filters := encoderFilterArgs(encoder, plan, suffix); len(filters) > 0 {
		args = append(args, filters...)
	}
	return append(args, encoderTrailerArgs(encoder, plan, suffix)...)
}

// encoderBurnedVideoArgs is the video half of a burn session's options. The
// picture filters are missing on purpose: when an image subtitle is burned in,
// the whole chain lives in a -filter_complex graph, and ffmpeg refuses to attach
// -vf to the same output. A burn is always a single rendition, so there is no
// stream specifier either.
func encoderBurnedVideoArgs(encoder string, plan videoPlan) []string {
	return append(encoderVideoArgs(encoder, ""), encoderTrailerArgs(encoder, plan, "")...)
}

// encoderVideoArgs is the codec and rate-control half of the video options,
// which a burn session shares with an ordinary one.
func encoderVideoArgs(encoder, suffix string) []string {
	option := func(name string) string { return name + suffix }
	switch {
	case strings.HasSuffix(encoder, "_nvenc"):
		return []string{option("-c:v"), encoder, option("-preset"), nvencPreset,
			option("-tune"), "hq", option("-rc"), "vbr", option("-cq"), "22"}
	case strings.HasSuffix(encoder, "_qsv"):
		return []string{option("-c:v"), encoder, option("-preset"), "veryfast", option("-global_quality"), "22"}
	case strings.HasSuffix(encoder, "_vaapi"):
		return []string{option("-c:v"), encoder}
	case strings.HasSuffix(encoder, "_amf"):
		return []string{option("-c:v"), encoder, option("-quality"), "balanced",
			option("-rc"), "cqp", option("-qp_i"), "22", option("-qp_p"), "22", option("-qp_b"), "22"}
	case strings.HasSuffix(encoder, "_videotoolbox"):
		return []string{option("-c:v"), encoder, option("-q:v"), "60"}
	case encoder == "libx264" || encoder == "libx265":
		return []string{option("-c:v"), encoder, option("-preset"), "veryfast", option("-crf"), "21"}
	case encoder == "libvpx-vp9":
		return []string{option("-c:v"), encoder, option("-crf"), "31", option("-b:v"), "0"}
	case encoder == "libsvtav1" || encoder == "libaom-av1":
		return []string{option("-c:v"), encoder, option("-crf"), "30"}
	default:
		return []string{option("-c:v"), encoder}
	}
}

// encoderFilterArgs renders the picture filter chain as the option spelling the
// output needs.
func encoderFilterArgs(encoder string, plan videoPlan, suffix string) []string {
	filters := videoFilters(encoder, plan)
	if len(filters) == 0 {
		return nil
	}
	// -vf for a single rendition, where it has always been used, and the
	// specifier form only where a specifier is needed: -vf:0 is easy to get
	// wrong, and -filter:v:N is the spelling that was verified.
	filterFlag := "-vf"
	if suffix != "" {
		filterFlag = "-filter:v" + suffix
	}
	return []string{filterFlag, strings.Join(filters, ",")}
}

// encoderTrailerArgs is the ceiling and pixel format, which follow the filters.
func encoderTrailerArgs(encoder string, plan videoPlan, suffix string) []string {
	var args []string
	if cap := bitrateCapArgs(plan, suffix); len(cap) > 0 {
		args = append(args, cap...)
	}
	if format := outputPixelFormat(encoder, plan); format != "" {
		args = append(args, "-pix_fmt"+suffix, format)
	}
	return args
}

// bitrateCapArgs renders a bitrate ceiling as a VBV constraint.
//
// -maxrate with -bufsize is the one spelling every family here understands: it
// bounds the rate without dictating it, so a software CRF encode keeps choosing
// its own quality and simply cannot exceed the ceiling, and a hardware encoder's
// constant-quality mode is bounded the same way. Asking for -b:v instead would
// turn quality-driven encodes into fixed-rate ones for no benefit.
//
// Measured on this host: the same 9 Mbps source encoded at 3.3 Mbps uncapped came
// out at 605 kbps with a 500 kbps ceiling, muxing overhead included. The bufsize
// is twice the ceiling, which is the usual compromise - smaller makes the rate
// snap to the limit and the picture visibly pump, larger lets a burst overshoot.
func bitrateCapArgs(plan videoPlan, suffix string) []string {
	if plan.BitrateKbps <= 0 {
		return nil
	}
	return []string{
		"-maxrate" + suffix, fmt.Sprintf("%dk", plan.BitrateKbps),
		"-bufsize" + suffix, fmt.Sprintf("%dk", plan.BitrateKbps*2),
	}
}

// nvencPreset is the speed/quality preset for NVENC. The range is p1 (fastest,
// worst) to p7 (slowest, best); p4 is the documented default and the balanced
// choice, and the encoding quality is set by -cq rather than by the preset.
const nvencPreset = "p4"

// outputPixelFormat pins the encoder's output to a format the client can decode.
//
// This matters more than it looks. Left alone, ffmpeg preserves the source's
// bit depth, so a 10-bit source transcodes to 10-bit H.264 "High 10" - which no
// browser decodes through Media Source Extensions. The stream then attaches,
// fetches its segments, and silently never plays: no media error, no console
// message, just a stalled player. So the default is 8-bit, and 10-bit is asked
// for only when the plan is deliberately keeping HDR.
//
// VAAPI is the exception and returns nothing: its upload filter already pins
// the surface format, and the encoder consumes hardware surfaces, so a -pix_fmt
// would ask for software frames instead of describing the output.
func outputPixelFormat(encoder string, plan videoPlan) string {
	switch {
	case strings.HasSuffix(encoder, "_vaapi"):
		return ""
	case plan.HDR():
		return plan.HDRPixelFormat
	case isHardwareEncoder(encoder):
		// NVENC, QuickSync, AMF and VideoToolbox all accept nv12 natively.
		return "nv12"
	default:
		return "yuv420p"
	}
}

// hdrPixelFormatCandidates lists the 10-bit pixel formats an encoder may accept,
// most likely first.
//
// The order is a guess; the probe is not. Hardware encoders differ in which
// surface format they will take for 10-bit, and a build without 10-bit support
// takes neither, so the startup probe tries each and records the one that
// worked. VAAPI has one candidate because its format is the upload filter's, not
// a -pix_fmt.
func hdrPixelFormatCandidates(encoder string) []string {
	switch {
	case strings.HasSuffix(encoder, "_vaapi"):
		return []string{"p010le"}
	case isHardwareEncoder(encoder):
		return []string{"p010le", "yuv420p10le"}
	default:
		return []string{"yuv420p10le"}
	}
}

// EncoderForHDR returns the encoder and pixel format to use for a 10-bit HDR
// stream of the given codec, and whether this host has one.
//
// It reads the list the startup probe filled in by encoding something, so an
// encoder that is listed by ffmpeg but cannot produce 10-bit here is not
// offered. Reporting no HDR encoder is a normal outcome: the negotiation then
// tone maps to SDR and says why.
func EncoderForHDR(codec string, server ServerCapability) (HDREncoder, bool) {
	for _, candidate := range videoEncoderCandidates[NormaliseVideoCodec(codec)] {
		for _, support := range server.HDRVideoEncoders {
			if support.Encoder == candidate {
				return support, true
			}
		}
	}
	return HDREncoder{}, false
}

// HDREncoder is a video encoder this host was actually able to drive with a
// 10-bit stream, and the pixel format it accepted.
type HDREncoder struct {
	Encoder     string `json:"encoder"`
	PixelFormat string `json:"pixel_format"`
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
func probeEncoder(ctx context.Context, ffmpegBin, encoder string, dev encoderDevice, plan videoPlan) error {
	probeCtx, cancel := context.WithTimeout(ctx, encoderProbeTimeout)
	defer cancel()

	args := []string{"-hide_banner", "-loglevel", "error"}
	args = append(args, encoderInputArgs(encoder, dev)...)
	args = append(args, "-f", "lavfi", "-i", "testsrc=size=320x240:rate=5:duration=0.2")
	args = append(args, encoderOutputArgs(encoder, plan, dev, 1, 0)...)
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

// probeEncoderSDR verifies that an encoder can open a session at all.
func probeEncoderSDR(ctx context.Context, ffmpegBin, encoder string, dev encoderDevice) error {
	return probeEncoder(ctx, ffmpegBin, encoder, dev, videoPlan{Height: encoderProbeHeight})
}

// probeHDRSupport reports whether an encoder can produce a 10-bit stream, and
// with which pixel format. The failure of every candidate is not an error worth
// surfacing on its own; it means "this encoder is 8-bit here", and the
// negotiation already explains the consequence.
func probeHDRSupport(ctx context.Context, ffmpegBin, encoder string, dev encoderDevice) (HDREncoder, error) {
	var lastErr error
	for _, format := range hdrPixelFormatCandidates(encoder) {
		err := probeEncoder(ctx, ffmpegBin, encoder, dev, videoPlan{
			Height:         encoderProbeHeight,
			HDRPixelFormat: format,
		})
		if err == nil {
			return HDREncoder{Encoder: encoder, PixelFormat: format}, nil
		}
		lastErr = err
	}
	return HDREncoder{}, lastErr
}

// encoderWorks reports whether an encoder can actually open a session.
func encoderWorks(ctx context.Context, ffmpegBin, encoder string, dev encoderDevice) bool {
	return probeEncoderSDR(ctx, ffmpegBin, encoder, dev) == nil
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
