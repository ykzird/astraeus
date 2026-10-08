package streaming

import (
	"context"
	"fmt"
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
	// TargetAudioChannels is set when the source carries more channels than the
	// client accepts and the audio is being re-encoded anyway.
	TargetAudioChannels int `json:"target_audio_channels,omitempty"`
	// TargetAudioStreamIndex is the ffmpeg stream index of the audio track this
	// session delivers, so a client can confirm which one it got. Zero when the
	// entity has no audio.
	TargetAudioStreamIndex int `json:"target_audio_stream_index,omitempty"`
	// TargetBitrateKbps is the ceiling the delivered video is held to, set when
	// the client declared a total bitrate limit. It is the client's limit minus
	// the audio allowance, so the whole stream fits rather than the video alone.
	TargetBitrateKbps int `json:"target_bitrate_kbps,omitempty"`
	// Renditions is an adaptive bitrate ladder, populated only when the client
	// asked to adapt rather than pinning a height. The first entry is the
	// largest. An empty list means one rendition, described entirely by the
	// fields above.
	Renditions []Rendition `json:"renditions,omitempty"`
	// TargetDynamicRange is the dynamic range of the video this decision
	// delivers: always "sdr" unless an HDR source is being passed through to a
	// client that declared HDR support.
	TargetDynamicRange DynamicRange `json:"target_dynamic_range,omitempty"`
	// ToneMap is set when the video is being re-encoded specifically to convert
	// HDR to SDR. It is separate from TargetDynamicRange because it selects the
	// filter chain: a 10-bit HDR source re-encoded for another reason still
	// needs 10-bit HDR output, not a tone map.
	ToneMap bool `json:"tone_map,omitempty"`
	// BurnedSubtitleIndex is the image subtitle stream this decision composites
	// into the picture, zero when none is. Burning a bitmap into the video is a
	// re-encode by definition - there is no way to composite into copied bits -
	// so a decision with this set is always a single-rendition transcode.
	BurnedSubtitleIndex int `json:"burned_subtitle_index,omitempty"`

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

	// Which audio track this session delivers. Resolved first, because every
	// audio decision below - codec, channel count, bitrate share - is about the
	// track that was asked for, not about whichever one happens to be first.
	audioTrack, hasAudio, trackRequestIgnored := info.ChosenAudioTrack(capability.AudioTrackIndex)
	if hasAudio {
		decision.TargetAudioStreamIndex = audioTrack.Index
		switch {
		case trackRequestIgnored:
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("this file has no audio track with stream index %d, so the default track (%s) is being delivered",
					capability.AudioTrackIndex, AudioTrackLabel(audioTrack)))
		case capability.AudioTrackIndex > 0:
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("the client asked for audio track %d (%s)",
					audioTrack.Index, AudioTrackLabel(audioTrack)))
		}
	}

	videoCompatible := clientSupportsVideo(capability, info.VideoCodec)
	if !videoCompatible {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("client cannot decode video codec %q", info.VideoCodec))
	}

	audioCompatible := !hasAudio || capability.SupportsAudio(audioTrack.Codec)
	if !audioCompatible {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("client cannot decode audio codec %q", audioTrack.Codec))
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
		hasAudio && audioTrack.Channels > capability.MaxAudioChannels
	if channelsTooMany {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("audio track %d has %d channels but the client accepts at most %d",
				audioTrack.Index, audioTrack.Channels, capability.MaxAudioChannels))
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

	// An HDR source handed to a client that has not declared HDR support is not
	// merely dimmer: PQ and HLG code values interpreted as SDR describe
	// different light, so the picture comes out washed out or crushed. This is
	// a transcoding reason of its own, independent of codec and bit depth.
	hdrMismatch := info.IsHDR() && !capability.SupportsHDR
	if hdrMismatch {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("source is %s but the client does not declare HDR support, so it cannot be shown as it was mastered",
				dynamicRangeLabel(info)))
	}

	// A bitrate limit the source exceeds is also a reason to re-encode: there is
	// no way to honour it while copying the bits. An unknown source bitrate is
	// deliberately not treated as exceeding it, because guessing would transcode
	// files that probably fit - but it is worth saying, since the limit then
	// cannot be shown to hold.
	bitrateTooHigh := false
	if capability.MaxBitrateKbps > 0 {
		switch {
		case info.BitrateKbps <= 0:
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("the source bitrate is unknown, so the client's %d kbps limit cannot be checked; a re-encode would be held to it",
					capability.MaxBitrateKbps))
		case info.BitrateKbps > capability.MaxBitrateKbps:
			bitrateTooHigh = true
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("source is %d kbps but the client accepts at most %d kbps",
					info.BitrateKbps, capability.MaxBitrateKbps))
		}
	}

	// A chosen track cannot come from direct play. Direct play serves the
	// original file, and the player then picks a track itself - usually the
	// file's default, which is precisely the one the client said it did not
	// want. Repackaging delivers the chosen stream without re-encoding
	// anything, so that is what happens instead.
	trackChosen := hasAudio && capability.AudioTrackIndex > 0 && !trackRequestIgnored

	// An image-based subtitle cannot be a selectable track: the browser has no
	// way to render a picture timed to the video, so the only way to show one is
	// to composite it into the picture while re-encoding. A text track named
	// here is *not* burned - it is delivered as a track, which is better in
	// every way - and a stream index the file does not have burns nothing.
	burning := false
	if capability.BurnSubtitleIndex > 0 {
		track, found := info.SubtitleTrackByIndex(capability.BurnSubtitleIndex)
		switch {
		case !found:
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("this file has no subtitle track with stream index %d, so nothing is burned into the picture",
					capability.BurnSubtitleIndex))
		case track.Text:
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("subtitle track %d is text-based (%s), so it is delivered as a selectable track rather than burned into the picture",
					track.Index, track.Codec))
		default:
			burning = true
			decision.BurnedSubtitleIndex = track.Index
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("image subtitle track %d (%s) is burned into the picture, which needs a re-encode because a bitmap cannot be composited into copied bits",
					track.Index, track.Codec))
		}
	}

	switch {
	case !videoCompatible || !audioCompatible || needsDownscale || tooDeep || channelsTooMany || hdrMismatch || bitrateTooHigh || burning:
		decision.Mode = ModeTranscode
	case trackChosen:
		decision.Mode = ModeRemux
	case containerCompatible:
		decision.Mode = ModeDirectPlay
	default:
		decision.Mode = ModeRemux
	}

	// Video action.
	if !videoCompatible || needsDownscale || tooDeep || hdrMismatch || bitrateTooHigh || burning {
		decision.VideoAction = ActionTranscode
		decision.TargetVideoCodec = capability.PreferredVideoCodec()
		// An HDR source that has to be re-encoded should land in a codec that can
		// carry HDR. The ordinary preference puts H.264 first because it is the
		// most widely decodable, but 10-bit H.264 is not an HDR delivery format:
		// no browser or television treats it as one, so a client that asked for
		// HDR would get washed-out SDR with a profile name it cannot use.
		if info.IsHDR() && capability.SupportsHDR {
			if hdrCodec := capability.PreferredVideoCodecForHDR(); hdrCodec != "" && hdrCodec != decision.TargetVideoCodec {
				decision.Reasons = append(decision.Reasons,
					fmt.Sprintf("%q cannot carry HDR, so the re-encode targets %q instead",
						decision.TargetVideoCodec, hdrCodec))
				decision.TargetVideoCodec = hdrCodec
			}
		}
		if decision.TargetVideoCodec == "" {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons, "client declared no video codecs to transcode into")
		}
		if needsDownscale {
			decision.TargetHeight = targetHeight
		}
		if hdrMismatch {
			decision.ToneMap = true
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("%s is tone mapped to SDR for this client", dynamicRangeLabel(info)))
		}
	}
	decision.TargetDynamicRange = deliveryRangeFor(info.DynamicRange, decision.ToneMap)

	// Say which way the dynamic range went, whichever branch produced it. A
	// decision to re-encode an HDR film into HDR and a decision to copy it
	// untouched look identical in the response otherwise.
	if info.IsHDR() && !hdrMismatch {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("the client supports HDR, so the stream keeps %s", dynamicRangeLabel(info)))
	}

	if decision.VideoAction == ActionTranscode && info.DolbyVisionProfile > 0 {
		decision.Reasons = append(decision.Reasons, dolbyVisionReencodeNote(info))
	}

	// Whether this decision is a ladder has to be known before the audio is
	// decided, because a ladder cannot copy the audio: each rung needs its own
	// elementary stream, and ffmpeg's HLS muxer refuses to place one copied
	// stream in two variants ("Same elementary stream found more than once in
	// two different variant definitions"). With ffmpeg 5.1 and 6.1 that refusal
	// is not an error the session survives: the master playlist comes out with
	// fewer rungs than the decision promised, so a client never sees the lower
	// ones. ffmpeg 9 tolerates it, which is why this went unnoticed until the
	// container's own ffmpeg was asked to build a ladder.
	//
	// The rungs depend on the height and not on the bitrate budget, so this is
	// knowable here, before the audio's share of that budget is computed from
	// the action.
	ladderTop := decision.TargetHeight
	if ladderTop <= 0 {
		ladderTop = info.Height
	}
	laddering := decision.Deliverable &&
		decision.VideoAction == ActionTranscode &&
		!capability.pinsOneRendition() &&
		!burning &&
		len(videoLadder(ladderTop, 0)) > 1

	// Audio action.
	switch {
	case !hasAudio:
		decision.AudioAction = ActionNone
	case !audioCompatible || channelsTooMany:
		decision.AudioAction = ActionTranscode
		decision.TargetAudioCodec = capability.PreferredAudioCodec()
		if decision.TargetAudioCodec == "" {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons, "client declared no audio codecs to transcode into")
		}
		if channelsTooMany {
			decision.TargetAudioChannels = capability.MaxAudioChannels
		}
	case laddering:
		decision.AudioAction = ActionTranscode
		decision.TargetAudioCodec = capability.PreferredAudioCodec()
		if decision.TargetAudioCodec == "" {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons, "client declared no audio codecs to transcode into")
		}
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("the audio is re-encoded rather than copied because a ladder gives each rung its own stream, and ffmpeg will not put one copied stream in two variants"))
	}

	// Work out what the client's total bitrate limit leaves for video. The audio
	// keeps its share - either the source's own rate, because it is being
	// copied, or the rate this server encodes it at - so that what the client
	// receives fits the limit rather than the video alone fitting it.
	audio := audioAllowanceKbps(audioTrack, hasAudio, decision.AudioAction)
	budget := 0
	if capability.MaxBitrateKbps > 0 {
		budget = capability.MaxBitrateKbps - audio
		if decision.VideoAction == ActionTranscode && budget < minVideoBitrateKbps {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("the client's %d kbps limit leaves %d kbps for video after %d kbps of audio, which is too little to encode",
					capability.MaxBitrateKbps, budget, audio))
			budget = 0
		}
	}

	// A ladder, when the client asked to adapt rather than pin a height. An
	// explicit preferred_height is such a request, and max_height alongside it
	// is the ceiling the ladder stays under; max_height on its own still means
	// one rendition, which is what a client that wants determinism asks for. A
	// burn is deliberately excluded: the subtitle is composited once, in one
	// filter graph, so a burned session is a single rendition. Offering rungs
	// here would mean either burning only the top one or compositing per rung,
	// and neither is what the client asked for.
	laddered := false
	if laddering {
		if ladder := videoLadder(ladderTop, budget); len(ladder) > 1 {
			decision.Renditions = ladder
			laddered = true
			// The fields that describe "the video" keep describing the largest
			// rung, so a client reading only them still sees the top of the
			// ladder rather than an unset height.
			decision.TargetHeight = ladder[0].Height
			decision.TargetBitrateKbps = ladder[0].BitrateKbps
			// The reason states what was asked for and then what was produced,
			// so a top rung below the preference is legible: the first clause
			// is the request, the second the outcome.
			if capability.PreferredHeight > 0 {
				decision.Reasons = append(decision.Reasons,
					fmt.Sprintf("the client asked for a ladder topped at %dp rather than one fixed rendition, so it gets a %d-rung ladder from %dp down to %dp",
						capability.PreferredHeight, len(ladder), ladder[0].Height, ladder[len(ladder)-1].Height))
			} else {
				decision.Reasons = append(decision.Reasons,
					fmt.Sprintf("the client did not pin a height, so it gets a %d-rung ladder from %dp down to %dp",
						len(ladder), ladder[0].Height, ladder[len(ladder)-1].Height))
			}
		}
	}

	// A preference that a burn has turned into one rendition deserves its own
	// sentence: the client asked to adapt and got a single encode, and the burn
	// reason alone does not say that the preference was what chose its height.
	if burning && capability.PreferredHeight > 0 && decision.VideoAction == ActionTranscode {
		decision.Reasons = append(decision.Reasons,
			fmt.Sprintf("the burn is one composited picture, so the %dp preference selects a single rendition rather than a ladder",
				capability.PreferredHeight))
	}

	// A single rendition gets an explicit ceiling, but only when the limit
	// actually constrains it. A browser profile's 120 Mbps is a declaration that
	// nothing is refused, not a request to cap the encode at 119808 kbps, and
	// saying so would be noise in the reason that a client reads to find out why
	// it got what it got. (A ladder needs no separate ceiling: each rung carries
	// its own.)
	if !laddered && decision.Deliverable && decision.VideoAction == ActionTranscode && budget > 0 {
		if info.BitrateKbps <= 0 || budget < info.BitrateKbps {
			decision.TargetBitrateKbps = budget
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("the video is held to %d kbps so the stream fits the client's %d kbps limit alongside %d kbps of audio",
					budget, capability.MaxBitrateKbps, audio))
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
		switch {
		case trackChosen && containerCompatible:
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("remux: the client asked for audio track %d, which direct play cannot isolate, so the streams are copied into HLS unchanged",
					audioTrack.Index))
		default:
			decision.Reasons = append(decision.Reasons,
				"remux: the streams are compatible but the container is not, so they are copied into HLS unchanged")
		}
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
	ceiling := capability.heightCeiling()
	if ceiling <= 0 && capability.MaxWidth <= 0 {
		return 0, false
	}

	limit := info.Height
	if ceiling > 0 && ceiling < limit {
		limit = ceiling
	}
	if capability.MaxWidth > 0 && info.Width > capability.MaxWidth {
		if byWidth := capability.MaxWidth * info.Height / info.Width; byWidth < limit {
			limit = byWidth
		}
	}

	if limit >= info.Height {
		return 0, false
	}

	// H.264 and HEVC with 4:2:0 require even dimensions, and the scale filter's
	// "-2" only makes the *width* even, so the height has to be made even here.
	// Rounding down, never up: rounding up would exceed the client's box.
	if limit%2 != 0 {
		limit--
	}
	if limit < 2 {
		limit = 2
	}
	return limit, true
}

// describeBox renders a client's resolution limit for a human. A preferred
// height is part of that limit: a client asking for a 720p ladder top has
// constrained the output just as a 720 box would, and saying "no resolution
// limit" while downscaling to 720 would be a lie.
func describeBox(capability ClientCapability) string {
	ceiling := capability.heightCeiling()

	box := "no resolution limit"
	switch {
	case capability.MaxWidth > 0 && ceiling > 0:
		box = fmt.Sprintf("%dx%d", capability.MaxWidth, ceiling)
	case ceiling > 0:
		box = fmt.Sprintf("%dp", ceiling)
	case capability.MaxWidth > 0:
		box = fmt.Sprintf("%dpx wide", capability.MaxWidth)
	}
	if capability.PreferredHeight > 0 {
		box += ", asking for a ladder rather than one fixed rendition"
	}
	return box
}

// Rendition is one rung of an adaptive bitrate ladder: a height and the ceiling
// the video at that height is held to.
type Rendition struct {
	Height int `json:"height"`
	// BitrateKbps is this rung's ceiling, which is what a client's player uses
	// to decide whether it can afford the rung.
	BitrateKbps int `json:"bitrate_kbps,omitempty"`
}

// maxLadderRungs bounds how many encodes one session may run. Each rung is a
// separate encode of the same source, so a ladder costs roughly what its rungs
// cost added together; three keeps a household server usable while still giving
// a player somewhere to drop to.
const maxLadderRungs = 3

// minLadderHeight is the smallest rung worth encoding.
const minLadderHeight = 240

// ladderBitrateKbps is the ceiling a rung at this height is given, before a
// client's own limit scales it. The numbers are the conventional H.264 ladder -
// roughly 0.1 bits per pixel per frame at 30fps, rounded to familiar values -
// and they exist so that a lower rung is genuinely cheaper to send rather than
// merely smaller.
func ladderBitrateKbps(height int) int {
	switch {
	case height >= 2160:
		return 14_000
	case height >= 1440:
		return 7_000
	case height >= 1080:
		return 4_500
	case height >= 720:
		return 2_500
	case height >= 480:
		return 1_200
	case height >= 360:
		return 700
	default:
		return 400
	}
}

// videoLadder builds the rungs for a source delivered at topHeight, within an
// optional total video budget in kbps (0 meaning no limit).
//
// The shape is deliberately simple - the top height, two thirds of it, half of
// it - because the point of a ladder is that a player can step down and see a
// difference, not that the steps are mathematically optimal. Rungs that would be
// too small to encode or too close to the rung above to be a real choice are
// dropped, and a budget scales every rung's ceiling rather than flattening them
// onto one number.
func videoLadder(topHeight, videoBudgetKbps int) []Rendition {
	if topHeight < minLadderHeight {
		return nil
	}

	heights := make([]int, 0, maxLadderRungs)
	for _, candidate := range []int{topHeight, topHeight * 2 / 3, topHeight / 2} {
		// Even heights only: 4:2:0 chroma needs them, and an odd rung would
		// make ffmpeg round it anyway, silently disagreeing with the numbers
		// this decision reports.
		if candidate%2 != 0 {
			candidate--
		}
		if candidate < minLadderHeight {
			continue
		}
		if len(heights) > 0 && candidate > heights[len(heights)-1]*9/10 {
			continue
		}
		heights = append(heights, candidate)
		if len(heights) == maxLadderRungs {
			break
		}
	}
	if len(heights) < 2 {
		return nil
	}

	// A budget the top rung cannot afford scales the whole ladder, so the rungs
	// stay in proportion instead of collapsing onto the limit.
	top := ladderBitrateKbps(heights[0])
	scale := 1.0
	if videoBudgetKbps > 0 && videoBudgetKbps < top {
		scale = float64(videoBudgetKbps) / float64(top)
	}

	renditions := make([]Rendition, 0, len(heights))
	for _, height := range heights {
		bitrate := int(float64(ladderBitrateKbps(height))*scale + 0.5)
		if bitrate < minVideoBitrateKbps {
			bitrate = minVideoBitrateKbps
		}
		renditions = append(renditions, Rendition{Height: height, BitrateKbps: bitrate})
	}
	return renditions
}

// minVideoBitrateKbps is the smallest video ceiling this server will accept. A
// limit that leaves less than this for the picture is not a delivery decision,
// it is a request that cannot be met, and saying so beats encoding something
// nobody can watch.
const minVideoBitrateKbps = 100

// defaultAudioAllowanceKbps is what the audio is assumed to take when it is
// being re-encoded, or when it is copied from a container that does not state
// its bitrate (Matroska often does not). It matches the -b:a this server passes
// to ffmpeg for a re-encoded track; if that changes, this should change with it.
const defaultAudioAllowanceKbps = 192

// audioAllowanceKbps is how much of the client's bitrate limit the delivered
// audio will use: the source's own rate when it is copied and the container
// states it, nothing when there is no audio, and the encode target otherwise.
// It is the *chosen* track's rate that counts, because that is the one being
// copied.
func audioAllowanceKbps(track AudioTrack, hasAudio bool, audioAction Action) int {
	switch {
	case !hasAudio || audioAction == ActionNone:
		return 0
	case audioAction == ActionCopy:
		if track.BitrateKbps > 0 {
			return track.BitrateKbps
		}
		return defaultAudioAllowanceKbps
	default:
		return defaultAudioAllowanceKbps
	}
}

// dynamicRangeLabel names a source's dynamic range for a human, including the
// Dolby Vision profile when the stream carries one.
func dynamicRangeLabel(info *MediaInfo) string {
	if info == nil {
		return "unknown dynamic range"
	}
	label := "SDR"
	switch info.DynamicRange {
	case RangeHDR10:
		label = "HDR10 (PQ)"
	case RangeHLG:
		label = "HLG"
	}
	if info.DolbyVisionProfile > 0 {
		label += fmt.Sprintf(" with Dolby Vision profile %d", info.DolbyVisionProfile)
	}
	return label
}

// deliveryRangeFor reports the dynamic range the delivered video will have.
// Anything that is not a pass-through of an HDR source is SDR, including an
// unknown range, so the field is never empty.
func deliveryRangeFor(source DynamicRange, toneMap bool) DynamicRange {
	if toneMap || !source.IsHDR() {
		return RangeSDR
	}
	return source
}

// dolbyVisionReencodeNote explains what re-encoding does to Dolby Vision.
//
// This is the difference between a caveat and a colour error. A profile 8 base
// layer is HDR10, so dropping the dynamic metadata costs the extra highlights
// Dolby Vision would have added and nothing else. A profile 5 base layer is
// IPTPQc2, a different colour encoding entirely, so a tone mapper told to treat
// it as PQ is reading the wrong numbers - and saying so is the only honest
// thing to do when the alternative is refusing to play the file at all.
func dolbyVisionReencodeNote(info *MediaInfo) string {
	if info.DolbyVisionBaseLayerHDR10 {
		return fmt.Sprintf("re-encoding does not carry the Dolby Vision profile %d dynamic metadata, "+
			"only its HDR10 base layer", info.DolbyVisionProfile)
	}
	return fmt.Sprintf("Dolby Vision profile %d stores IPTPQc2 rather than PQ, which this server cannot "+
		"convert; the tone-mapped colour will be approximate", info.DolbyVisionProfile)
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
	FFmpegAvailable  bool `json:"ffmpeg_available"`
	FFprobeAvailable bool `json:"ffprobe_available"`
	// HardwareAcceleration names the hardware encoder families this host can
	// actually use, best first. A machine can have more than one - an Intel
	// iGPU beside an NVIDIA card - so it is a list. Selection does not read it:
	// EncoderFor reads VideoEncoders, which holds only what was verified, so a
	// family cannot be offered that the hardware check rejected.
	HardwareAcceleration []string `json:"hardware_acceleration,omitempty"`
	VideoEncoders        []string `json:"video_encoders,omitempty"`
	// HDRVideoEncoders lists the video encoders that produced a 10-bit stream
	// here, each with the pixel format it accepted. A video encoder that works
	// at 8 bits may still refuse 10, and an HDR transcode that is offered on
	// the strength of the 8-bit result would fail at the first frame - so this
	// is a separate, separately proved list rather than a boolean on the list
	// above.
	HDRVideoEncoders []HDREncoder `json:"hdr_video_encoders,omitempty"`
	// RenderNode is the DRM render node VAAPI needs, empty when there is none.
	RenderNode string `json:"render_node,omitempty"`
	// RejectedEncoders lists the hardware encoders ffmpeg offers but this host
	// cannot use, with ffmpeg's own complaint. Without it, a machine with an
	// unusable GPU looks identical to one with no GPU at all.
	RejectedEncoders []EncoderRejection `json:"rejected_encoders,omitempty"`
	AudioEncoders    []string           `json:"audio_encoders,omitempty"`
	HLS              bool               `json:"hls"`
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
	// transcode works at all. The probe uses the same options a real session
	// would, so a flag this build rejects is caught here.
	compiled := ParseEncoders(stdout.String())
	capability.AudioEncoders = audioEncodersOnly(compiled)

	capability.RenderNode = firstRenderNode(deviceDir)
	device := encoderDevice{RenderNode: capability.RenderNode}

	for _, encoder := range videoEncodersOnly(compiled) {
		if !isHardwareEncoder(encoder) {
			capability.VideoEncoders = append(capability.VideoEncoders, encoder)
		} else {
			if err := probeEncoderSDR(ctx, ffmpegBin, encoder, device); err != nil {
				capability.RejectedEncoders = append(capability.RejectedEncoders, EncoderRejection{
					Encoder: encoder,
					Reason:  err.Error(),
				})
				continue
			}
			capability.VideoEncoders = append(capability.VideoEncoders, encoder)
		}

		// An encoder that works at 8 bits is a candidate for HDR, not proof of
		// it: 10-bit surfaces are a separate capability, and on one host the
		// hardware encoder lacks it while the software one has it.
		if support, err := probeHDRSupport(ctx, ffmpegBin, encoder, device); err == nil {
			capability.HDRVideoEncoders = append(capability.HDRVideoEncoders, support)
		}
	}

	capability.HardwareAcceleration = hardwareFamilies(capability.VideoEncoders)

	return capability
}

// knownVideoCodecs and knownAudioCodecs are the vocabulary a client may declare.
// A manifest naming something outside it is a malformed request, not an
// unsupported one, and saying so is friendlier than letting the name reach
// ffmpeg and come back as a 500.
var knownVideoCodecs = []string{
	"h264", "hevc", "av1", "vp8", "vp9", "mpeg2", "mpeg4",
	"vc1", "theora", "prores", "dnxhd", "wmv3", "flv1", "h263",
}

var knownAudioCodecs = []string{
	"aac", "ac3", "eac3", "dts", "truehd", "opus", "vorbis",
	"mp3", "flac", "alac", "mp2", "wmav2", "pcm_s16le", "pcm_s24le",
}

// audioEncoderPreference maps a target audio codec onto the encoders that can
// produce it, most portable first.
var audioEncoderPreference = map[string][]string{
	"aac":    {"aac"},
	"opus":   {"libopus"},
	"mp3":    {"libmp3lame"},
	"vorbis": {"libvorbis"},
	"ac3":    {"ac3"},
	"eac3":   {"eac3"},
	"flac":   {"flac"},
}

// AudioEncoderFor returns the ffmpeg encoder for an audio codec, or "" when this
// server cannot produce it.
func AudioEncoderFor(codec string, server ServerCapability) string {
	for _, candidate := range audioEncoderPreference[NormaliseAudioCodec(codec)] {
		if containsFold(server.AudioEncoders, candidate) {
			return candidate
		}
	}
	return ""
}

// NegotiateForServer narrows a negotiation to what this server can actually
// deliver.
//
// Negotiate is pure: it compares the client against the media and knows nothing
// about the host. That is the right shape, but it means it can choose a target
// codec no encoder here produces - a client that only accepts AV1 on a build
// without an AV1 encoder - and the failure would surface much later as ffmpeg
// exiting with a confusing message. This wrapper retargets to another codec the
// client accepts when one is available, and otherwise marks the decision
// undeliverable so the caller can answer 409 with a reason.
func NegotiateForServer(info *MediaInfo, capability ClientCapability, server ServerCapability) Decision {
	decision := Negotiate(info, capability)
	if !decision.Deliverable || decision.Mode != ModeTranscode {
		return decision
	}

	// Keeping HDR needs an encoder that was verified to produce 10-bit here. If
	// there is none, the honest fallback is the one a client that cannot do HDR
	// would get anyway: tone map to SDR, which every client that lists an 8-bit
	// codec can play. Refusing instead would make the file unplayable on a
	// machine that is perfectly able to show it in SDR.
	if decision.VideoAction == ActionTranscode && decision.TargetDynamicRange.IsHDR() {
		if _, ok := EncoderForHDR(decision.TargetVideoCodec, server); !ok {
			decision.ToneMap = true
			decision.TargetDynamicRange = deliveryRangeFor(info.DynamicRange, true)
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("this server has no verified 10-bit encoder for %q, so %s is tone mapped to SDR instead",
					decision.TargetVideoCodec, dynamicRangeLabel(info)))
		}
	}

	if decision.VideoAction == ActionTranscode && EncoderFor(decision.TargetVideoCodec, server) == "" {
		if alternative := encodableVideoCodec(capability, server); alternative != "" {
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("this server cannot encode %q; using %q instead",
					decision.TargetVideoCodec, alternative))
			decision.TargetVideoCodec = alternative
		} else {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons,
				"this server has no encoder for any video codec the client accepts")
		}
	}

	if decision.AudioAction == ActionTranscode && AudioEncoderFor(decision.TargetAudioCodec, server) == "" {
		if alternative := encodableAudioCodec(capability, server); alternative != "" {
			decision.Reasons = append(decision.Reasons,
				fmt.Sprintf("this server cannot encode %q audio; using %q instead",
					decision.TargetAudioCodec, alternative))
			decision.TargetAudioCodec = alternative
		} else {
			decision.Deliverable = false
			decision.Reasons = append(decision.Reasons,
				"this server has no encoder for any audio codec the client accepts")
		}
	}

	return decision
}

// encodableVideoCodec returns the client's most preferred video codec this
// server can encode, or "".
func encodableVideoCodec(capability ClientCapability, server ServerCapability) string {
	for _, codec := range videoCodecPreference {
		if capability.SupportsVideo(codec) && EncoderFor(codec, server) != "" {
			return codec
		}
	}
	return ""
}

// encodableAudioCodec returns the client's most preferred audio codec this
// server can encode, or "".
func encodableAudioCodec(capability ClientCapability, server ServerCapability) string {
	for _, codec := range audioCodecPreference {
		if capability.SupportsAudio(codec) && AudioEncoderFor(codec, server) != "" {
			return codec
		}
	}
	return ""
}
