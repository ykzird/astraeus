// OCR for image-based subtitle tracks.
//
// A PGS track carries pictures, so a browser cannot render it as a track and
// the previous answer was to composite it into the video. This path reads the
// pictures instead: the stream is demuxed, decoded into bitmaps, and each cue's
// bitmap is handed to tesseract, which returns the words. The result is a
// WebVTT file, which the player can toggle, restyle and search like any other
// subtitle.
//
// The OCR engine is a runtime dependency and is treated honestly: when it is
// not installed, ConvertImage refuses the track exactly as the server did
// before (ErrUnsupportedFormat) rather than failing midway, and the caller
// advertises no URL for image tracks.

package subtitles

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ocrPageSegmentation tells tesseract to treat the image as one uniform block
// of text. A subtitle is one or two lines and nothing else, and the automatic
// mode reads a single line as a page and returns nothing; 6 reads both.
const ocrPageSegmentation = "6"

// ocrMergeWindow is how close two cues with identical words must be to be
// folded into one. A PGS stream re-composes a caption when its palette changes,
// which would otherwise produce a run of duplicate cues.
const ocrMergeWindow = 40 * time.Millisecond

// OCRSupportsCodec reports whether this OCR path can read a given image
// subtitle codec. Only HDMV PGS is implemented: it is the format the parser
// decodes and the one this project can build a fixture for. VobSub
// (dvd_subtitle) is also a bitmap format, but it lives in a different
// container with a different palette, so it stays burn-only rather than being
// advertised and then failing when the extractor rejects it.
func OCRSupportsCodec(codec string) bool {
	return strings.EqualFold(codec, "hdmv_pgs_subtitle")
}

// OCRReady reports whether the OCR engine is present. A caller uses it to
// decide whether an image track can be offered as text or must be burned in.
func (s *Service) OCRReady() bool {
	if s.tesseractBin == "" {
		return false
	}
	_, err := exec.LookPath(s.tesseractBin)
	return err == nil
}

// ConvertImage renders an image-based subtitle track as WebVTT by reading the
// text out of its bitmaps, and returns the cached file path. Without an OCR
// engine it returns ErrUnsupportedFormat, which is the refusal the server gave
// before OCR existed.
func (s *Service) ConvertImage(ctx context.Context, mediaPath string, trackIndex int) (string, error) {
	if trackIndex < 0 {
		return "", fmt.Errorf("subtitle track index must not be negative, got %d", trackIndex)
	}
	// Checked before touching the media: an install without tesseract keeps the
	// old answer rather than starting work it cannot finish.
	if !s.OCRReady() {
		return "", fmt.Errorf("%w: track %d is image-based and reading it needs the %q program, which is not installed",
			ErrUnsupportedFormat, trackIndex, s.tesseractBin)
	}
	info, err := os.Stat(mediaPath)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", mediaPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", mediaPath)
	}

	target := filepath.Join(s.cacheDir, s.cacheKeyFor(mediaPath, info, trackIndex, "ocr")+".vtt")
	if cached, err := os.Stat(target); err == nil && cached.Size() > 0 {
		s.logger.DebugContext(ctx, "serving cached OCR subtitles", "track", trackIndex)
		return target, nil
	}

	if err := s.ocrExtract(ctx, mediaPath, trackIndex, target); err != nil {
		return "", err
	}
	return target, nil
}

// ocrCue is one recognised caption.
type ocrCue struct {
	start time.Duration
	end   time.Duration
	text  string
}

// ocrExtract demuxes the image subtitle stream, decodes it and recognises every
// cue, then commits the WebVTT atomically.
func (s *Service) ocrExtract(ctx context.Context, mediaPath string, trackIndex int, target string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	supPath, cleanup, err := s.extractPGSStream(ctx, mediaPath, trackIndex)
	if err != nil {
		return err
	}
	defer cleanup()

	file, err := os.Open(supPath)
	if err != nil {
		return fmt.Errorf("opening the extracted image subtitle stream: %w", err)
	}
	cues, parseErr := ParsePGS(file)
	closeErr := file.Close()
	if parseErr != nil {
		return fmt.Errorf("decoding image subtitle track %d of %s: %w", trackIndex, mediaPath, parseErr)
	}
	if closeErr != nil {
		return fmt.Errorf("closing the extracted image subtitle stream: %w", closeErr)
	}

	recognised := make([]ocrCue, 0, len(cues))
	for _, cue := range cues {
		if cue.Image == nil {
			continue
		}
		text, err := s.recognise(ctx, cue.Image)
		if err != nil {
			return err
		}
		if text == "" {
			continue
		}
		recognised = mergeOCRCue(recognised, ocrCue{start: cue.Start, end: cue.End, text: text})
	}
	if len(recognised) == 0 {
		return fmt.Errorf("%w (OCR found no text in track %d)", ErrNoCues, trackIndex)
	}

	tmp, err := os.CreateTemp(s.cacheDir, "ocr-*.vtt")
	if err != nil {
		return fmt.Errorf("creating temporary subtitle file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(buildWebVTT(recognised)); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing recognised subtitles: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing recognised subtitles: %w", err)
	}
	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("committing recognised subtitles: %w", err)
	}

	s.logger.InfoContext(ctx, "recognised image subtitle track",
		"track", trackIndex, "cues", len(recognised), "source", filepath.Base(mediaPath))
	return nil
}

// extractPGSStream copies the chosen subtitle stream out of the media file into
// a raw .sup, which is what the decoder reads. ffmpeg demuxes but does not
// decode, so the bitmap that arrives is the one the file holds.
func (s *Service) extractPGSStream(ctx context.Context, mediaPath string, trackIndex int) (string, func(), error) {
	tmp, err := os.CreateTemp(s.cacheDir, "pgs-*.sup")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating a temporary image subtitle stream: %w", err)
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if err := tmp.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("closing the temporary image subtitle stream: %w", err)
	}

	cmd := exec.CommandContext(ctx, s.ffmpegBin,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", mediaPath,
		"-map", "0:"+strconv.Itoa(trackIndex),
		"-c:s", "copy",
		"-f", "sup",
		name,
	)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		cleanup()
		if ctx.Err() != nil {
			return "", func() {}, fmt.Errorf("extracting image subtitle track %d from %s: %w", trackIndex, mediaPath, ctx.Err())
		}
		message := string(output)
		if message == "" {
			message = runErr.Error()
		}
		return "", func() {}, fmt.Errorf("extracting image subtitle track %d from %s: %s", trackIndex, mediaPath, message)
	}
	return name, cleanup, nil
}

// recognise writes one cue bitmap as a PNG and reads the words back with
// tesseract. An image with no recognisable text returns an empty string, which
// the caller turns into a dropped cue.
func (s *Service) recognise(ctx context.Context, bitmap *image.RGBA) (string, error) {
	file, err := os.CreateTemp(s.cacheDir, "ocr-*.png")
	if err != nil {
		return "", fmt.Errorf("creating a temporary OCR image: %w", err)
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()

	if err := png.Encode(file, renderForOCR(bitmap)); err != nil {
		_ = file.Close()
		return "", fmt.Errorf("encoding a subtitle bitmap for OCR: %w", err)
	}
	if err := file.Close(); err != nil {
		return "", fmt.Errorf("closing a subtitle bitmap for OCR: %w", err)
	}

	args := []string{name, "stdout", "--psm", ocrPageSegmentation}
	if s.ocrLanguage != "" {
		args = append(args, "-l", s.ocrLanguage)
	}
	cmd := exec.CommandContext(ctx, s.tesseractBin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, runErr := cmd.Output()
	if runErr != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("reading text from an image subtitle: %w", ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = runErr.Error()
		}
		return "", fmt.Errorf("reading text from an image subtitle: %s", message)
	}
	return cleanOCRText(string(output)), nil
}

// renderForOCR turns a subtitle bitmap into what a text recogniser expects:
// dark glyphs on a white page.
//
// Subtitles are light text, often with a dark outline, on a transparent
// background. Compositing over black keeps the light glyphs and drops the dark
// outline, and inverting turns that into ink on paper. A margin is added
// because tesseract segments a line better when it does not touch the edge.
func renderForOCR(src *image.RGBA) *image.Gray {
	const margin = 8

	bounds := src.Bounds()
	dst := image.NewGray(image.Rect(0, 0, bounds.Dx()+2*margin, bounds.Dy()+2*margin))
	for i := range dst.Pix {
		dst.Pix[i] = 0xff
	}

	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			// RGBA() returns the channels already multiplied by alpha, which is
			// exactly the composite over black that this wants.
			r, g, b, a := src.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
			if a == 0 {
				continue
			}
			luma := (299*r + 587*g + 114*b) / 1000
			dst.SetGray(margin+x, margin+y, color.Gray{Y: 255 - uint8(luma>>8)})
		}
	}
	return dst
}

// cleanOCRText drops blank lines and collapses runs of whitespace, which
// tesseract introduces freely.
func cleanOCRText(raw string) string {
	lines := strings.Split(raw, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// mergeOCRCue folds a cue into the previous one when they carry the same words
// and are not separated by a noticeable gap.
func mergeOCRCue(cues []ocrCue, next ocrCue) []ocrCue {
	if n := len(cues); n > 0 {
		last := &cues[n-1]
		if last.text == next.text && next.start-last.end <= ocrMergeWindow {
			last.end = next.end
			return cues
		}
	}
	return append(cues, next)
}

// buildWebVTT renders recognised cues as a WebVTT document.
func buildWebVTT(cues []ocrCue) string {
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, cue := range cues {
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n",
			vttTimestamp(cue.start), vttTimestamp(cue.end), escapeVTT(cue.text))
	}
	return b.String()
}

// vttTimestamp renders a duration as HH:MM:SS.mmm.
func vttTimestamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	ms := d.Milliseconds()
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3_600_000, (ms/60_000)%60, (ms/1000)%60, ms%1000)
}

// escapeVTT escapes the three characters WebVTT reads as markup, so recognised
// text is always shown as text. A recogniser can return any character, and a
// stray '<' would otherwise start a cue tag.
func escapeVTT(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}
