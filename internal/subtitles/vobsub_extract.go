// The OCR path's demuxing step: it has to hand the decoder both halves of a
// VobSub track. A Matroska source already holds them, so its subtitle stream is
// copied across whole and the palette travels with it. Any other source is
// demuxed with ffmpeg and framed here with the raw SPU packets, and the codec
// private carrying the palette is read out separately with ffprobe, because an
// image-only copy drops it. An extraction that loses the palette cannot be
// decoded at all, which is why the palette is a required half of the result.

package subtitles

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/ykzird/astraeus/internal/ffmpegprocess"
	"strings"
)

// extraction is the result of pulling an image subtitle track out of its
// container: the file to read, the palette its container supplied (empty for a
// Matroska source, whose codec private carries its own), and how to remove the
// file when the caller is finished with it.
type extraction struct {
	path    string
	palette string
	cleanup func()
}

// probeSubtitleStream reads the two ffprobe facts a VobSub extraction needs:
// the container ffmpeg sees, and the subtitle stream's codec private, which is
// the .idx-style text that carries the palette. It is one call rather than two
// so a source is opened once.
type subtitleStream struct {
	container    string
	codecPrivate string
}

func (s *Service) probeSubtitleStream(ctx context.Context, mediaPath string, ordinal int) (subtitleStream, error) {
	var found subtitleStream

	format := exec.CommandContext(ctx, s.ffprobeBin,
		"-v", "error", "-show_entries", "format=format_name",
		"-of", "default=noprint_wrappers=1:nokey=1", mediaPath)
	output, err := format.Output()
	if err != nil {
		return found, fmt.Errorf("identifying the container of %s: %w", mediaPath, err)
	}
	found.container = strings.TrimSpace(string(output))

	stream := exec.CommandContext(ctx, s.ffprobeBin,
		"-v", "error",
		"-select_streams", "s:"+strconv.Itoa(ordinal),
		"-show_entries", "stream=extradata",
		// `-show_data` is what prints extradata at all. Without it the entry is
		// requested, reported as present and left empty, so a container whose
		// palette lives there - an MPEG program stream, anything HandBrake wrote -
		// was refused with "carries no palette in its container". The palette was
		// there; ffprobe was told not to print it (L-12 of the 2026-10-09 review).
		"-show_data",
		"-of", "default=noprint_wrappers=1:nokey=1",
		mediaPath)
	output, err = stream.Output()
	if err != nil {
		return found, fmt.Errorf("reading the codec private data of subtitle stream %d of %s: %w", ordinal, mediaPath, err)
	}
	found.codecPrivate = strings.TrimSpace(string(output))
	return found, nil
}

// extractVobSubStream obtains a VobSub track in a form the decoder can read,
// together with the palette its container supplied.
func (s *Service) extractVobSubStream(ctx context.Context, mediaPath string, ordinal int) (extraction, error) {
	source, err := s.probeSubtitleStream(ctx, mediaPath, ordinal)
	if err != nil {
		return extraction{}, err
	}
	if !strings.Contains(source.container, "matroska") && source.codecPrivate == "" {
		return extraction{}, fmt.Errorf("%w: subtitle stream %d of %s carries no palette in its container",
			ErrUnsupportedFormat, ordinal, mediaPath)
	}

	tmp, err := os.CreateTemp(s.cacheDir, "vobsub-*")
	if err != nil {
		return extraction{}, fmt.Errorf("creating a temporary image subtitle stream: %w", err)
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := tmp.Close(); err != nil {
		cleanup()
		return extraction{}, fmt.Errorf("closing the temporary image subtitle stream: %w", err)
	}

	if strings.Contains(source.container, "matroska") {
		if err := s.remuxVobSubMatroska(ctx, mediaPath, ordinal, name); err != nil {
			cleanup()
			return extraction{}, err
		}
		return extraction{path: name, cleanup: cleanup}, nil
	}

	if err := s.demuxVobSubPackets(ctx, mediaPath, ordinal, name); err != nil {
		cleanup()
		return extraction{}, err
	}
	return extraction{path: name, palette: source.codecPrivate, cleanup: cleanup}, nil
}

// remuxVobSubMatroska copies the chosen subtitle stream into a standalone
// Matroska file, which keeps the codec private that holds the palette.
func (s *Service) remuxVobSubMatroska(ctx context.Context, mediaPath string, ordinal int, target string) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	// A library file is a path, but ffmpeg reads an input as a URL (S-17).
	args = append(args, ffmpegprocess.Args()...)
	args = append(args,
		"-i", mediaPath,
		"-map", "0:s:"+strconv.Itoa(ordinal),
		"-c:s", "copy",
		"-f", "matroska",
		target,
	)
	cmd := exec.CommandContext(ctx, s.ffmpegBin, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extracting image subtitle stream %d from %s: %s", ordinal, mediaPath, commandMessage(output, ctx, err))
	}
	return nil
}

// demuxVobSubPackets writes the track's raw SPU packets into the extractor's
// framed form. ffmpeg is asked for the stream as data, which strips the
// container's own framing without decoding the pictures.
func (s *Service) demuxVobSubPackets(ctx context.Context, mediaPath string, ordinal int, target string) error {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	// A library file is a path, but ffmpeg reads an input as a URL (S-17).
	args = append(args, ffmpegprocess.Args()...)
	args = append(args,
		"-i", mediaPath,
		"-map", "0:s:"+strconv.Itoa(ordinal),
		"-c", "copy",
		"-f", "data",
		target,
	)
	cmd := exec.CommandContext(ctx, s.ffmpegBin, args...)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extracting image subtitle stream %d from %s: %s", ordinal, mediaPath, commandMessage(output, ctx, err))
	}

	blob, err := os.ReadFile(target)
	if err != nil {
		return fmt.Errorf("reading the extracted image subtitle stream: %w", err)
	}
	framed, err := frameVobSubPackets(blob)
	if err != nil {
		return err
	}
	return os.WriteFile(target, framed, 0o600)
}

// frameVobSubPackets puts a length in front of each SPU in a demuxed blob.
//
// The data muxer concatenates packets with no boundaries, and a packet's own
// first two bytes are not its length, so the length has to come from somewhere
// else. It does: the second two-byte field of a record is the offset of its
// control sequence, which is also the record's total length (the two are the
// same number in every sample this project has). Walking that is how the
// records are separated, and the walk refuses a record whose control offset
// does not land on another record or on the end.
func frameVobSubPackets(blob []byte) ([]byte, error) {
	var framed bytes.Buffer
	for offset := 0; offset < len(blob); {
		if offset+4 > len(blob) {
			return nil, fmt.Errorf("%w: the extracted stream ends inside a packet header", ErrInvalidVobSub)
		}
		length := int(binary.BigEndian.Uint16(blob[offset+2 : offset+4]))
		if length <= 4 || offset+length > len(blob) {
			// A record that does not fit is the padding at the end of a PES
			// payload rather than another cue, so the walk stops cleanly.
			break
		}
		record := blob[offset : offset+length]
		var size [2]byte
		binary.BigEndian.PutUint16(size[:], uint16(length))
		framed.Write(size[:])
		framed.Write(record)
		offset += length
	}
	if framed.Len() == 0 {
		return nil, fmt.Errorf("%w: the extracted stream held no subtitle packets", ErrInvalidVobSub)
	}
	return framed.Bytes(), nil
}

// commandMessage picks the most useful text out of a failed command: the
// command's own output when it has any, the context's error when it ran out of
// time, and the process error otherwise.
func commandMessage(output []byte, ctx context.Context, runErr error) string {
	if ctx.Err() != nil {
		return ctx.Err().Error()
	}
	if message := strings.TrimSpace(string(output)); message != "" {
		return message
	}
	return runErr.Error()
}
