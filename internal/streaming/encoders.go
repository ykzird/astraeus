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
	"strconv"
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

// evenDimensions is the scale expression that keeps a re-encode inside what
// 4:2:0 encoders accept.
//
// It exists because "scale only when the picture is too big" leaves the one case
// that matters: a source that already fits the client's box is re-encoded
// untouched, and libx264 then refuses an odd dimension outright with "height not
// divisible by 2" - an Xvid or MPEG-4 rip at 640x271 is the shape that finds
// this, and it failed the session rather than playing (S-4 of the 2026-10-09
// review). ffmpeg will not fix it for us either: it inserts no scaler, because
// the frame is already yuv420p and only the encoder objects to the size.
//
// Rounding down, never up, so a re-encode can come in under the client's box but
// never over it. The expression is evaluated per frame, so it is a no-op - and
// costs nothing - on a source that is already even.
const evenDimensions = "scale=trunc(iw/2)*2:trunc(ih/2)*2"

// videoFilters renders the filter chain for a plan and an encoder, up to the
// point where an encoder can accept the frames.
//
// The returned bool reports whether the chain ends in software frames that a
// hardware encoder still has to receive. That distinction exists for subtitle
// burn-in: the compositing filters are software-only (the review's S-5), so
// uploading before the overlay hands hardware frames to a software filter and
// ffmpeg refuses with "Impossible to convert between the formats supported by
// the filter 'Parsed_scale2ref_3' and the filter 'auto_scale_2'". The burn graph
// therefore takes the software part, overlays, and then applies
// hardwareUploadFilters - which is why the upload is not simply the last entry
// of one list.
//
// The two arrangements differ because VAAPI scales on the GPU after uploading
// and cannot convert a transfer function itself (tonemap_vaapi exists but has
// never been run on hardware here), so it tone maps in software first and
// scales afterwards. Everything else scales first and then tone maps: the float
// conversion is the expensive part, and doing it on a 1080p frame instead of a
// 4K one costs a quarter as much for a picture nobody can tell apart.
//
// Every re-encode gets at least evenDimensions, whether or not it needs a
// downscale, because the two are different questions.
func videoFilters(encoder string, plan videoPlan) ([]string, bool) {
	if !strings.HasSuffix(encoder, "_vaapi") {
		scale := evenDimensions
		if plan.Height > 0 {
			scale = fmt.Sprintf("scale=-2:%d", plan.Height)
		}
		var filters []string
		if scale != "" {
			filters = append(filters, scale)
		}
		if plan.ToneMap {
			filters = append(filters, toneMapFilterChain)
		}
		if plan.HDR() {
			filters = append(filters, hdrTagFilter(plan.TargetRange))
		}
		return filters, false
	}

	// VAAPI: everything that has to happen in software, in the order it has to
	// happen. The upload and the hardware scale follow in
	// hardwareUploadFilters, so a software compositing stage can go between.
	var filters []string
	if plan.ToneMap {
		filters = append(filters, toneMapFilterChain)
	}
	if plan.HDR() {
		filters = append(filters, hdrTagFilter(plan.TargetRange))
	}
	// The upload needs a software frame in the format hwupload knows how to
	// convert, and that conversion is left off here because
	// hardwareUploadFilters opens with it: for an ordinary session the two are
	// adjacent, so this avoids emitting "format=nv12,format=nv12", and for a
	// burn session the conversion has to happen after the overlay anyway.
	//
	// The format is nv12 and not yuv420p, which is not cosmetic: feeding
	// hwupload yuv420p on this host produced "Terminating thread with return
	// code -5" and an empty encode, which the startup probe correctly read as a
	// broken encoder and rejected - taking the whole hardware path down with it.
	return filters, true
}

// hardwareUploadFilters moves software frames onto the VAAPI surface and scales
// them there. It is the tail of a VAAPI chain, and for a burn session it is
// applied after the overlay rather than before it - which is why the conversion
// it opens with lives here rather than in videoFilters.
//
// The conversion is not assumed to be already done, because the compositing
// filters in a burn graph can hand back a different format; a conversion that
// is already correct costs nothing.
func hardwareUploadFilters(plan videoPlan) []string {
	format := "nv12"
	if plan.HDR() {
		format = plan.HDRPixelFormat
	}
	filters := []string{"format=" + format, "hwupload"}
	if plan.Height > 0 {
		return append(filters, fmt.Sprintf("scale_vaapi=w=-2:h=%d", plan.Height))
	}
	// VAAPI needs its own spelling, and it needs the same evenness: the
	// hardware scaler rounds, but relying on that is not a guarantee the way an
	// explicit trunc is.
	return append(filters, "scale_vaapi=w=trunc(iw/2)*2:h=trunc(ih/2)*2")
}

// videoFilterChain joins the software chain and, when the encoder needs it, the
// hardware upload that follows.
func videoFilterChain(encoder string, plan videoPlan) []string {
	filters, needsUpload := videoFilters(encoder, plan)
	if needsUpload {
		filters = append(filters, hardwareUploadFilters(plan)...)
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
	args := encoderVideoArgs(encoder, suffix, plan)
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
	return append(encoderVideoArgs(encoder, "", plan), encoderTrailerArgs(encoder, plan, "")...)
}

// encoderVideoArgs is the codec and constant-quality half of the video options,
// which a burn session shares with an ordinary one.
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
// When the client declared a ceiling, the family's capped mode is used instead
// of its constant-quality one. That is not a matter of style: a constant-quality
// hardware encode ignores -maxrate on its own, which is S-1 of the 2026-10-09
// review - a 308 kbps target shipped at 5.4 Mbps. The two modes are mutually
// exclusive, because -rc cqp and -rc vbr_peak cannot both apply, so the cap
// decides which one is rendered rather than being appended to the other.
func encoderVideoArgs(encoder, suffix string, plan videoPlan) []string {
	option := func(name string) string { return name + suffix }
	capped := plan.BitrateKbps > 0
	switch {
	case strings.HasSuffix(encoder, "_nvenc"):
		args := []string{option("-c:v"), encoder, option("-preset"), nvencPreset, option("-tune"), "hq"}
		if !capped {
			args = append(args, option("-rc"), "vbr", option("-cq"), "22")
		}
		return append(args, rateControlArgs(encoder, plan, suffix)...)
	case strings.HasSuffix(encoder, "_qsv"):
		args := []string{option("-c:v"), encoder, option("-preset"), "veryfast"}
		if !capped {
			args = append(args, option("-global_quality"), "22")
		}
		return append(args, rateControlArgs(encoder, plan, suffix)...)
	case strings.HasSuffix(encoder, "_vaapi"):
		return append([]string{option("-c:v"), encoder}, rateControlArgs(encoder, plan, suffix)...)
	case strings.HasSuffix(encoder, "_amf"):
		args := []string{option("-c:v"), encoder, option("-quality"), "balanced"}
		if !capped {
			// The constant-quality mode: AMF infers it from the QP values.
			args = append(args, option("-rc"), "cqp",
				option("-qp_i"), "22", option("-qp_p"), "22", option("-qp_b"), "22")
		}
		return append(args, rateControlArgs(encoder, plan, suffix)...)
	case strings.HasSuffix(encoder, "_videotoolbox"):
		return []string{option("-c:v"), encoder, option("-q:v"), "60"}
	case encoder == "libx264" || encoder == "libx265":
		return append([]string{option("-c:v"), encoder, option("-preset"), "veryfast", option("-crf"), "21"},
			rateControlArgs(encoder, plan, suffix)...)
	case encoder == "libvpx-vp9":
		return append([]string{option("-c:v"), encoder, option("-crf"), "31", option("-b:v"), "0"},
			rateControlArgs(encoder, plan, suffix)...)
	case encoder == "libsvtav1" || encoder == "libaom-av1":
		return append([]string{option("-c:v"), encoder, option("-crf"), "30"},
			rateControlArgs(encoder, plan, suffix)...)
	default:
		return append([]string{option("-c:v"), encoder}, rateControlArgs(encoder, plan, suffix)...)
	}
}

// rateControlArgs renders the bitrate ceiling for one encoder family.
//
// The families do not share a spelling, and the difference is not cosmetic:
// a hardware encoder's constant-quality mode treats -maxrate as advice and
// ignores it, so the ceiling has to be expressed in that family's own rate
// control. Measured on this host against a 6 s 720p source with a 308 kbps
// video target:
//
//	spelling                              produced
//	------------------------------------  --------
//	h264_vaapi -maxrate 308k -bufsize 616k   5255 kbps   (ignored)
//	h264_vaapi -rc_mode VBR -b:v 308k ...      688 kbps
//	h264_vaapi -rc_mode QVBR -b:v 308k ...    1097 kbps
//	libx264 -crf 21 -maxrate 308k ...          344 kbps
//
// So VAAPI gets an explicit -b:v and a VBR rate-control mode. A software
// encoder needs no -b:v: -maxrate bounds a CRF encode without dictating its
// rate, which is what makes a quality-driven ladder possible at all.
//
// The bufsize is twice the ceiling, which is the usual compromise - smaller
// makes the rate snap to the limit and the picture visibly pump, larger lets a
// burst overshoot.
//
// QSV, AMF, NVENC and VideoToolbox spellings are reasoned from their documented
// option sets rather than measured, because this project has run none of them on
// hardware. That is not left to faith: probeEncoder verifies the ceiling on the
// machine at startup and rejects a family that does not honour it.
func rateControlArgs(encoder string, plan videoPlan, suffix string) []string {
	if plan.BitrateKbps <= 0 {
		return nil
	}
	option := func(name string) string { return name + suffix }
	ceiling := fmt.Sprintf("%dk", plan.BitrateKbps)
	bufsize := fmt.Sprintf("%dk", plan.BitrateKbps*2)

	switch {
	case strings.HasSuffix(encoder, "_vaapi"):
		return []string{option("-rc_mode"), "VBR", option("-b:v"), ceiling,
			option("-maxrate"), ceiling, option("-bufsize"), bufsize}
	case strings.HasSuffix(encoder, "_qsv"):
		// -global_quality alone is ICQ, which ignores a ceiling; VBR plus -b:v
		// is the mode that does not.
		return []string{option("-b:v"), ceiling, option("-maxrate"), ceiling,
			option("-bufsize"), bufsize}
	case strings.HasSuffix(encoder, "_amf"):
		// -rc cqp ignores -b:v, so the capped mode replaces it.
		return []string{option("-rc"), "vbr_peak", option("-b:v"), ceiling,
			option("-maxrate"), ceiling, option("-bufsize"), bufsize}
	case strings.HasSuffix(encoder, "_nvenc"):
		// NVENC's VBR mode honours -maxrate, but only once a target exists.
		return []string{option("-b:v"), ceiling, option("-maxrate"), ceiling,
			option("-bufsize"), bufsize}
	default:
		return []string{option("-maxrate"), ceiling, option("-bufsize"), bufsize}
	}
}

// encoderFilterArgs renders the picture filter chain as the option spelling the
// output needs.
func encoderFilterArgs(encoder string, plan videoPlan, suffix string) []string {
	filters := videoFilterChain(encoder, plan)
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

// encoderTrailerArgs is the pixel format, which follows the filters.
func encoderTrailerArgs(encoder string, plan videoPlan, suffix string) []string {
	var args []string
	if format := outputPixelFormat(encoder, plan); format != "" {
		args = append(args, "-pix_fmt"+suffix, format)
	}
	return args
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

	args := []string{"-hide_banner", "-loglevel", "info"}
	args = append(args, encoderInputArgs(encoder, dev)...)
	args = append(args, "-f", "lavfi", "-i",
		fmt.Sprintf("testsrc=size=320x240:rate=10:duration=%d", probeSourceSeconds))
	args = append(args, encoderOutputArgs(encoder, plan, dev, 1, 0)...)
	args = append(args, "-f", "null", "-")

	stderr := &boundedBuffer{limit: probeStderrLimit}
	probe := exec.CommandContext(probeCtx, ffmpegBin, args...)
	probe.Stdout = io.Discard
	probe.Stderr = stderr

	if err := probe.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, firstComplaint(stderr.String()))
	}
	if err := verifyProbeCeiling(encoder, plan, stderr.String()); err != nil {
		return err
	}
	return nil
}

// probeSourceSeconds is how much material the probe encodes. Long enough that
// the byte count is large compared with ffmpeg's KiB rounding, short enough to
// stay a startup-cost footnote.
const probeSourceSeconds = 1

// probeCapTolerance is how far above the declared ceiling a probe encode may
// land before the family is called out. The null muxer reports whole KiB, the
// encoder needs a moment to converge, and container overhead is not free, so an
// exact bound would reject working encoders. Two-and-a-half times the ceiling
// still catches the failure this check exists for: a family that ignores the
// ceiling entirely runs two orders of magnitude over.
const probeCapTolerance = 2.5

// verifyProbeCeiling checks that an encoder actually honours the bitrate ceiling
// it was handed.
//
// This is the second half of the S-1 fix, and the half that makes the first half
// honest. The rate-control spellings for QSV, AMF, NVENC and VideoToolbox are
// reasoned from their documented option sets, not measured, because this project
// has never run them on hardware. An encoder that accepts the options and
// ignores them would otherwise ship as "capped": the master playlist would
// advertise a rung the segments do not honour, and the first person to notice
// would be a viewer on a slow link.
//
// So a family that cannot hold a ceiling is rejected at startup exactly like one
// that cannot open a session, with the measured rate in the rejection reason.
// Software encoders are exempt: they hold the ceiling with -maxrate alone, which
// the integration suite measures on every run.
func verifyProbeCeiling(encoder string, plan videoPlan, output string) error {
	if plan.BitrateKbps <= 0 || !isHardwareEncoder(encoder) {
		return nil
	}
	measured, ok := probeVideoKib(output)
	if !ok {
		// The line is missing rather than wrong, which happens when ffmpeg's
		// own reporting changes. Silence would be worse than a rejection.
		return fmt.Errorf("could not read the probe's output size, so the %d kbps ceiling is unverified",
			plan.BitrateKbps)
	}

	measuredKbps := int(float64(measured*8) / probeSourceSeconds)
	limit := int(float64(plan.BitrateKbps) * probeCapTolerance)
	if measuredKbps > limit {
		return fmt.Errorf("ignores the bitrate ceiling: asked for %d kbps, encoded %d kbps",
			plan.BitrateKbps, measuredKbps)
	}
	return nil
}

// probeVideoKib reads the video byte count out of ffmpeg's null-muxer summary
// ("video:3KiB audio:0KiB ..."). The last match wins, in case the line appears
// more than once in a longer log.
var probeVideoKibPattern = regexp.MustCompile(`video:(\d+)KiB`)

func probeVideoKib(output string) (int, bool) {
	matches := probeVideoKibPattern.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return 0, false
	}
	value, err := strconv.Atoi(matches[len(matches)-1][1])
	if err != nil {
		return 0, false
	}
	return value, true
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
