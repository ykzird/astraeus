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

// MediaInfo is the subset of technical properties the negotiation needs. It is
// produced by probing a MediaObject on disk.
type MediaInfo struct {
	Container  string `json:"container"`
	VideoCodec string `json:"video_codec"`
	AudioCodec string `json:"audio_codec,omitempty"`
	// AudioChannels is the channel count of the first audio stream. Chromium
	// refuses a 5.1 AAC SourceBuffer, so this has to be negotiable.
	AudioChannels int `json:"audio_channels,omitempty"`
	Width         int `json:"width,omitempty"`
	Height        int `json:"height,omitempty"`
	// PixelFormat and BitDepth describe the decoded video. They matter because
	// a 10-bit stream is not playable in a browser even when its codec name is
	// one the browser claims to support.
	PixelFormat     string          `json:"pixel_format,omitempty"`
	BitDepth        int             `json:"bit_depth,omitempty"`
	BitrateKbps     int             `json:"bitrate_kbps,omitempty"`
	DurationSeconds float64         `json:"duration_seconds,omitempty"`
	Subtitles       []SubtitleTrack `json:"subtitles,omitempty"`
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
	Index       int    `json:"index"`
	CodecName   string `json:"codec_name"`
	CodecType   string `json:"codec_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	PixFmt      string `json:"pix_fmt"`
	Channels    int    `json:"channels"`
	BitRate     string `json:"bit_rate"`
	Duration    string `json:"duration"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
		Default     int `json:"default"`
		Forced      int `json:"forced"`
	} `json:"disposition"`
	Tags struct {
		Language string `json:"language"`
		Title    string `json:"title"`
	} `json:"tags"`
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

	cmd := exec.CommandContext(ctx, p.binary,
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)

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
				videoSeen = true
			}
		case "audio":
			if info.AudioCodec == "" {
				info.AudioCodec = stream.CodecName
				info.AudioChannels = stream.Channels
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
