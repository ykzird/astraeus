package streaming

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ykzird/astraeus/internal/ffmpegprocess"
)

// SubtitleTrack describes one subtitle stream in a media file.
type SubtitleTrack struct {
	// Index is the ffmpeg stream index, which is what selects the track when
	// extracting it.
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language,omitempty"`
	Title    string `json:"title,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
	// Text is false for image-based formats (PGS, VobSub), which cannot be
	// converted to WebVTT without optical character recognition.
	Text bool `json:"text"`
}

// textSubtitleCodecs are the codecs that can be rendered to WebVTT by ffmpeg
// without OCR.
var textSubtitleCodecs = map[string]bool{
	"subrip":    true,
	"srt":       true,
	"ass":       true,
	"ssa":       true,
	"mov_text":  true,
	"webvtt":    true,
	"text":      true,
	"ttml":      true,
	"subviewer": true,
	"microdvd":  true,
	"jacosub":   true,
	"sami":      true,
	"realtext":  true,
	"stl":       true,
	"vplayer":   true,
}

// IsTextSubtitle reports whether a subtitle codec can be converted to WebVTT
// without OCR.
func IsTextSubtitle(codec string) bool {
	return textSubtitleCodecs[strings.ToLower(strings.TrimSpace(codec))]
}

// DynamicRange is the transfer characteristic of the video: what a decoder has
// to do to turn the stored code values into light.
//
// It is deliberately about the transfer function alone. Wide gamut is not high
// dynamic range: a 10-bit BT.2020 SDR master is stored exactly like an SDR one
// and must not be tone mapped, so primaries do not enter into it.
type DynamicRange string

const (
	// RangeSDR is everything with a conventional transfer function, including
	// BT.2020 and 10-bit SDR material.
	RangeSDR DynamicRange = "sdr"
	// RangeHDR10 is PQ (SMPTE ST 2084), the transfer HDR10 and the Dolby Vision
	// base layer use.
	RangeHDR10 DynamicRange = "hdr10"
	// RangeHLG is ARIB STD-B67, the broadcast hybrid log-gamma.
	RangeHLG DynamicRange = "hlg"
)

// IsHDR reports whether delivering this video untouched would give an SDR
// client a picture whose code values it would interpret with the wrong transfer
// function - which is the whole reason tone mapping exists.
func (d DynamicRange) IsHDR() bool {
	return d == RangeHDR10 || d == RangeHLG
}

// AudioTrack describes one audio stream in a media file.
type AudioTrack struct {
	// Index is the ffmpeg stream index, and it is what selects the track: a
	// session maps it by global stream index, the same way subtitle extraction
	// does, so the number a client sends back is the number that appears here.
	Index       int    `json:"index"`
	Codec       string `json:"codec"`
	Channels    int    `json:"channels,omitempty"`
	BitrateKbps int    `json:"bitrate_kbps,omitempty"`
	Language    string `json:"language,omitempty"`
	Title       string `json:"title,omitempty"`
	// Default is the stream the file itself marks as default. With no explicit
	// selection this is the track delivered, because it is the one the file's
	// author intended to be heard.
	Default bool `json:"default,omitempty"`
}

// MediaInfo is the subset of technical properties the negotiation needs. It is
// produced by probing a MediaObject on disk.
type MediaInfo struct {
	Container  string `json:"container"`
	VideoCodec string `json:"video_codec"`
	// AudioCodec, AudioChannels and AudioBitrateKbps describe the track that
	// will be delivered by default: the one marked default, or the first. They
	// are the summary a client sees without choosing, and the negotiation
	// re-reads the selected track from AudioTracks when a client does choose.
	AudioCodec       string       `json:"audio_codec,omitempty"`
	AudioChannels    int          `json:"audio_channels,omitempty"`
	AudioBitrateKbps int          `json:"audio_bitrate_kbps,omitempty"`
	AudioTracks      []AudioTrack `json:"audio_tracks,omitempty"`
	Width            int          `json:"width,omitempty"`
	Height           int          `json:"height,omitempty"`
	// PixelFormat and BitDepth describe the decoded video. They matter because
	// a 10-bit stream is not playable in a browser even when its codec name is
	// one the browser claims to support.
	PixelFormat string `json:"pixel_format,omitempty"`
	BitDepth    int    `json:"bit_depth,omitempty"`
	// Colour description, as the file declares it. These are what decide
	// whether a transcode needs tone mapping, so they are reported rather than
	// inferred from the bit depth.
	ColorPrimaries string       `json:"color_primaries,omitempty"`
	ColorTransfer  string       `json:"color_transfer,omitempty"`
	ColorSpace     string       `json:"color_space,omitempty"`
	DynamicRange   DynamicRange `json:"dynamic_range,omitempty"`
	// DolbyVisionProfile is the profile from the stream's DOVI configuration
	// record, or 0 when the stream carries no Dolby Vision metadata. It is
	// reported beside the dynamic range rather than folded into it because
	// profile 8 usually has a PQ base layer that is already HDR10, while a
	// profile 5 base layer is IPTPQc2 and is not.
	DolbyVisionProfile int `json:"dolby_vision_profile,omitempty"`
	// DolbyVisionBaseLayerHDR10 is true when the Dolby Vision base layer is
	// signalled as HDR10-compatible (dv_bl_signal_compatibility_id >= 1), which
	// is what makes a re-encode of it honest HDR10 rather than a colour error.
	DolbyVisionBaseLayerHDR10 bool            `json:"dolby_vision_base_layer_hdr10,omitempty"`
	BitrateKbps               int             `json:"bitrate_kbps,omitempty"`
	DurationSeconds           float64         `json:"duration_seconds,omitempty"`
	Subtitles                 []SubtitleTrack `json:"subtitles,omitempty"`
}

// IsHDR reports whether the video needs tone mapping before an SDR client can
// display it correctly.
func (m *MediaInfo) IsHDR() bool {
	return m != nil && m.DynamicRange.IsHDR()
}

// ChosenAudioTrack resolves the audio track a client asked for.
//
// requested is an ffmpeg stream index, and 0 means "the server's choice": the
// track the file marks default, or the first. It reports the track, whether the
// file has any audio at all, and whether an explicit request had to be ignored
// because no stream carries that index - which the negotiation turns into a
// reason rather than a failure, since a track that is missing is not a reason to
// refuse the film.
//
// A MediaInfo built by hand rather than probed - as several tests do - has no
// track list but may carry the summary fields. It is treated as a file with one
// audio track, which is what those fields describe.
func (m *MediaInfo) ChosenAudioTrack(requested int) (track AudioTrack, hasAudio bool, ignored bool) {
	if m == nil {
		return AudioTrack{}, false, false
	}

	if len(m.AudioTracks) == 0 {
		if m.AudioCodec == "" {
			return AudioTrack{}, false, false
		}
		return AudioTrack{
			Codec:       m.AudioCodec,
			Channels:    m.AudioChannels,
			BitrateKbps: m.AudioBitrateKbps,
		}, true, requested > 0
	}

	fallback := m.AudioTracks[0]
	for _, candidate := range m.AudioTracks {
		if candidate.Default {
			fallback = candidate
			break
		}
	}

	if requested > 0 {
		for _, candidate := range m.AudioTracks {
			if candidate.Index == requested {
				return candidate, true, false
			}
		}
		return fallback, true, true
	}
	return fallback, true, false
}

// DirectPlayAudioTracks returns the audio tracks a player might open by itself.
//
// A file opened directly is a file whose stream selection this server does not
// control: the player picks. A file with one audio track has one possible answer,
// and one whose selected track is the first track has one too. A file whose
// default flag points past the first track has two, because a player that honours
// the flag and a player that takes the first stream will disagree - and the review
// measured the server assuming the former while browsers did the latter, which for
// an AC3-first/AAC-default MP4 is a film with no sound (S-16 of the 2026-10-09
// review).
//
// The caller uses this to require that *every* track a direct-playing client
// might choose is one it can decode. Remux and transcode are unaffected: there the
// server maps one stream explicitly, so what it decided is what plays.
func (m *MediaInfo) DirectPlayAudioTracks(chosen AudioTrack, trackChosen bool) []AudioTrack {
	if m == nil || len(m.AudioTracks) == 0 {
		return nil
	}
	if trackChosen {
		// The client named a track, so the server's mapping is what it gets.
		return []AudioTrack{chosen}
	}
	if len(m.AudioTracks) == 1 || m.AudioTracks[0].Index == chosen.Index {
		// One possible answer: the first track and the selected one are the same
		// track, so there is nothing for two players to disagree about. Returning
		// it once, and not a second copy, is what keeps the caller's check from
		// reporting the chosen track as a *disagreement* when it is simply
		// undecodable - two different problems that deserve two different reasons.
		return []AudioTrack{chosen}
	}
	return []AudioTrack{m.AudioTracks[0], chosen}
}

// AudioTrackByIndex returns the track with this ffmpeg stream index.
func (m *MediaInfo) AudioTrackByIndex(index int) (AudioTrack, bool) {
	if m == nil {
		return AudioTrack{}, false
	}
	for _, track := range m.AudioTracks {
		if track.Index == index {
			return track, true
		}
	}
	return AudioTrack{}, false
}

// SubtitleTrackByIndex returns the subtitle track with this ffmpeg stream index.
func (m *MediaInfo) SubtitleTrackByIndex(index int) (SubtitleTrack, bool) {
	if m == nil {
		return SubtitleTrack{}, false
	}
	for _, track := range m.Subtitles {
		if track.Index == index {
			return track, true
		}
	}
	return SubtitleTrack{}, false
}

// AudioTrackLabel names a track for a human, using whatever the file provides
// and falling back to the codec, which is always there.
func AudioTrackLabel(track AudioTrack) string {
	label := track.Language
	if track.Title != "" {
		label = track.Title
	}
	if label == "" {
		label = track.Codec
	}
	if track.Codec != "" && !strings.Contains(strings.ToLower(label), strings.ToLower(track.Codec)) {
		label += " " + track.Codec
	}
	if track.Channels > 0 {
		label += fmt.Sprintf(" %dch", track.Channels)
	}
	return label
}

// Prober inspects a media file.
type Prober interface {
	Probe(ctx context.Context, path string) (*MediaInfo, error)
}

// FFProbe is a Prober backed by the ffprobe binary.
type FFProbe struct {
	binary  string
	timeout time.Duration
}

// NewFFProbe creates an FFProbe using the given binary name or path.
func NewFFProbe(binary string) *FFProbe {
	if binary == "" {
		binary = "ffprobe"
	}
	return &FFProbe{binary: binary, timeout: 30 * time.Second}
}

// Available reports whether the ffprobe binary can be executed.
func (p *FFProbe) Available() bool {
	_, err := exec.LookPath(p.binary)
	return err == nil
}

// ffprobeStream mirrors one entry of ffprobe's "streams" array.
type ffprobeStream struct {
	Index          int    `json:"index"`
	CodecName      string `json:"codec_name"`
	CodecType      string `json:"codec_type"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	PixFmt         string `json:"pix_fmt"`
	Channels       int    `json:"channels"`
	BitRate        string `json:"bit_rate"`
	Duration       string `json:"duration"`
	ColorSpace     string `json:"color_space"`
	ColorTransfer  string `json:"color_transfer"`
	ColorPrimaries string `json:"color_primaries"`
	Disposition    struct {
		AttachedPic int `json:"attached_pic"`
		Default     int `json:"default"`
		Forced      int `json:"forced"`
	} `json:"disposition"`
	// SideDataList carries stream-level metadata that has no field of its own.
	// Dolby Vision is the one that matters here: ffprobe reports it as a "DOVI
	// configuration record" whose compatibility id says whether the base layer
	// is HDR10.
	SideDataList []ffprobeSideData `json:"side_data_list"`
	Tags         struct {
		Language string `json:"language"`
		Title    string `json:"title"`
	} `json:"tags"`
}

// ffprobeSideData mirrors one entry of a stream's side_data_list. The Dolby
// Vision record is the only one this project reads, and every field is optional
// so a record of a different type decodes to its zero value rather than failing.
type ffprobeSideData struct {
	SideDataType               string `json:"side_data_type"`
	DolbyVisionProfile         int    `json:"dv_profile"`
	DolbyVisionBLCompatibility int    `json:"dv_bl_signal_compatibility_id"`
}

// ffprobeOutput mirrors the JSON ffprobe emits for -show_format -show_streams.
type ffprobeOutput struct {
	Streams []ffprobeStream `json:"streams"`
	Format  struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
}

// Probe runs ffprobe and returns the technical properties of the file.
func (p *FFProbe) Probe(ctx context.Context, path string) (*MediaInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()

	args := []string{"-v", "error"}
	// The input is a path from a library, but ffmpeg treats an input as a URL and
	// reads a playlist-shaped file as instructions to fetch other URLs. S-17 of
	// the 2026-10-09 review: without this, a file in the library could make the
	// server fetch whatever it named.
	args = append(args, ffmpegprocess.Args()...)
	args = append(args,
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)

	cmd := exec.CommandContext(ctx, p.binary, args...)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("probing %s: %w", path, ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("probing %s: %s", path, message)
	}

	var output ffprobeOutput
	if err := json.Unmarshal([]byte(stdout.String()), &output); err != nil {
		return nil, fmt.Errorf("parsing ffprobe output for %s: %w", path, err)
	}

	return mediaInfoFromProbe(output, path)
}

// mediaInfoFromProbe turns ffprobe's JSON into a MediaInfo. It is separate from
// Probe so the parsing rules - which streams count, what a colour tag means -
// can be tested without running ffprobe.
func mediaInfoFromProbe(output ffprobeOutput, path string) (*MediaInfo, error) {
	info := &MediaInfo{Container: preferredContainer(path, output.Format.FormatName)}

	var videoSeen bool
	streamBitrate := 0
	for _, stream := range output.Streams {
		streamBitrate += atoiSafe(stream.BitRate)

		switch stream.CodecType {
		case "video":
			// Cover art is reported as a video stream; it is not the feature.
			if stream.Disposition.AttachedPic == 1 {
				continue
			}
			if !videoSeen {
				info.VideoCodec = stream.CodecName
				info.Width = stream.Width
				info.Height = stream.Height
				info.PixelFormat = stream.PixFmt
				info.BitDepth = bitDepthFromPixelFormat(stream.PixFmt)
				info.ColorSpace = stream.ColorSpace
				info.ColorTransfer = stream.ColorTransfer
				info.ColorPrimaries = stream.ColorPrimaries
				info.DynamicRange = classifyDynamicRange(stream.ColorTransfer)
				info.DolbyVisionProfile, info.DolbyVisionBaseLayerHDR10 = dolbyVisionFromSideData(stream.SideDataList)
				videoSeen = true
			}
		case "audio":
			if stream.CodecName == "" {
				continue
			}
			track := AudioTrack{
				Index:       stream.Index,
				Codec:       stream.CodecName,
				Channels:    stream.Channels,
				BitrateKbps: atoiSafe(stream.BitRate) / 1000,
				Language:    stream.Tags.Language,
				Title:       stream.Tags.Title,
				Default:     stream.Disposition.Default == 1,
			}
			info.AudioTracks = append(info.AudioTracks, track)
			// The summary describes the first track, which is what a client that
			// does not choose gets unless the file marks another as default; the
			// negotiation resolves that case from the list.
			if info.AudioCodec == "" {
				info.AudioCodec = track.Codec
				info.AudioChannels = track.Channels
				info.AudioBitrateKbps = track.BitrateKbps
			}
		case "subtitle":
			if stream.CodecName == "" {
				continue
			}
			info.Subtitles = append(info.Subtitles, SubtitleTrack{
				Index:    stream.Index,
				Codec:    stream.CodecName,
				Language: stream.Tags.Language,
				Title:    stream.Tags.Title,
				Default:  stream.Disposition.Default == 1,
				Forced:   stream.Disposition.Forced == 1,
				Text:     IsTextSubtitle(stream.CodecName),
			})
		}
	}

	if !videoSeen {
		return nil, fmt.Errorf("no playable video stream found in %s", path)
	}

	info.BitrateKbps = atoiSafe(output.Format.BitRate) / 1000
	if info.BitrateKbps == 0 {
		info.BitrateKbps = streamBitrate / 1000
	}

	info.DurationSeconds = atofSafe(output.Format.Duration)
	if info.DurationSeconds == 0 {
		for _, stream := range output.Streams {
			if stream.CodecType == "video" {
				if d := atofSafe(stream.Duration); d > 0 {
					info.DurationSeconds = d
					break
				}
			}
		}
	}

	return info, nil
}

// classifyDynamicRange maps a transfer characteristic onto the dynamic range a
// client has to be able to handle.
//
// Only the two transfers that actually mean "this is not SDR light" are
// classified as HDR. An unrecognised or absent transfer is treated as SDR on
// purpose: guessing HDR from a wide-gamut primaries tag or a 10-bit pixel format
// would tone map 10-bit BT.2020 SDR material, which is a real (if uncommon)
// mastering choice, and turning down the contrast of a correct picture is worse
// than leaving a mis-tagged HDR file to the old behaviour.
func classifyDynamicRange(transfer string) DynamicRange {
	switch strings.ToLower(strings.TrimSpace(transfer)) {
	case "smpte2084", "smpte-st-2084":
		return RangeHDR10
	case "arib-std-b67", "arib_std_b67":
		return RangeHLG
	default:
		return RangeSDR
	}
}

// dolbyVisionFromSideData extracts the DOVI configuration record, if there is
// one, and reports the profile and whether the base layer is HDR10-compatible.
//
// The compatibility id is the part that changes what the server may honestly
// do: profile 8 with a non-zero id has an HDR10 base layer that survives a
// re-encode, while profile 5 stores IPTPQc2 and does not.
func dolbyVisionFromSideData(entries []ffprobeSideData) (profile int, baseLayerHDR10 bool) {
	for _, entry := range entries {
		if !strings.Contains(strings.ToLower(entry.SideDataType), "dovi") {
			continue
		}
		if entry.DolbyVisionProfile <= 0 {
			continue
		}
		return entry.DolbyVisionProfile, entry.DolbyVisionBLCompatibility >= 1
	}
	return 0, false
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func atofSafe(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return f
}

// knownExtensions are file extensions whose container meaning is unambiguous.
// ffprobe reports "matroska,webm" for both .mkv and .webm files, so the
// extension is the only reliable way to tell them apart.
var knownExtensions = map[string]string{
	"mkv":  "matroska",
	"webm": "webm",
	"mp4":  "mp4",
	"m4v":  "mp4",
	"mov":  "mp4",
	"ts":   "mpegts",
	"m2ts": "mpegts",
	"avi":  "avi",
}

// preferredContainer picks the container the client capability check should use.
// The file extension wins when it is unambiguous; otherwise ffprobe's answer is
// used.
func preferredContainer(path, formatName string) string {
	idx := strings.LastIndex(path, ".")
	if idx != -1 {
		if container, ok := knownExtensions[strings.ToLower(path[idx+1:])]; ok {
			return container
		}
	}
	if formatName != "" {
		return formatName
	}
	if idx == -1 {
		return ""
	}
	return strings.ToLower(path[idx+1:])
}

// pixelDepthRe matches the bit depth ffmpeg appends to deep pixel formats:
// yuv420p10le, gray12be and friends. A plain yuv420p is 8-bit and does not
// match, because the depth digits are absent.
var pixelDepthRe = regexp.MustCompile(`(?:^|[a-z])(9|10|12|14|16)(?:le|be)?$`)

// bitDepthFromPixelFormat reports the bit depth of a decoded pixel format.
// It is a name-based heuristic, which is all ffprobe offers; formats whose
// digits describe the chroma layout rather than the depth are handled first.
func bitDepthFromPixelFormat(name string) int {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return 0
	}

	// nv12, nv16 and nv24 are 8-bit; their trailing digits are a layout, not a
	// depth, and would otherwise look like a 12- or 16-bit format.
	switch name {
	case "nv12", "nv21", "nv16", "nv24":
		return 8
	}

	// Semi-planar deep formats spell the depth first: p010le, p012le, p016le.
	for _, candidate := range []struct {
		prefix string
		depth  int
	}{{"p010", 10}, {"p012", 12}, {"p014", 14}, {"p016", 16}} {
		if strings.HasPrefix(name, candidate.prefix) {
			return candidate.depth
		}
	}

	if match := pixelDepthRe.FindStringSubmatch(name); match != nil {
		if depth, err := strconv.Atoi(match[1]); err == nil {
			return depth
		}
	}
	return 8
}
