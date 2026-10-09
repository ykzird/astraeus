// Package subtitles extracts text subtitle tracks from media files as WebVTT,
// which is the only subtitle format browsers render natively. Image-based PGS
// tracks are decoded and read into WebVTT too, with OCR.
package subtitles

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

// ErrUnsupportedFormat is returned for image-based subtitle formats (PGS,
// VobSub) when nothing can turn their pictures into text. OCR makes PGS
// readable, so this is what an install without an OCR engine still returns.
var ErrUnsupportedFormat = errors.New("subtitle format is not text-based")

// ErrNoCues is returned when a track produced an empty subtitle file.
var ErrNoCues = errors.New("subtitle track contained no cues")

// Config configures a Service.
type Config struct {
	// FFmpegBin is the ffmpeg executable.
	FFmpegBin string
	// FFprobeBin is the ffprobe executable, which the image-subtitle path uses
	// to identify a track's codec and to read the codec private data that
	// carries a VobSub palette. It defaults to "ffprobe".
	FFprobeBin string
	// TesseractBin is the OCR executable used for image-based subtitle tracks.
	// It is optional: when it is missing the service still converts text
	// tracks, and an image track keeps its refusal instead of failing.
	TesseractBin string
	// OCRLanguage is the tesseract language, e.g. "eng". Empty uses tesseract's
	// own default.
	OCRLanguage string
	// CacheDir stores the extracted WebVTT files.
	CacheDir string
	// Timeout bounds a single extraction.
	Timeout time.Duration
	Logger  *slog.Logger
}

// Service extracts and caches WebVTT subtitles.
type Service struct {
	ffmpegBin    string
	ffprobeBin   string
	tesseractBin string
	ocrLanguage  string
	cacheDir     string
	timeout      time.Duration
	logger       *slog.Logger
}

// New creates a Service and prepares its cache directory.
func New(cfg Config) (*Service, error) {
	if cfg.FFmpegBin == "" {
		cfg.FFmpegBin = "ffmpeg"
	}
	if cfg.FFprobeBin == "" {
		cfg.FFprobeBin = "ffprobe"
	}
	if cfg.TesseractBin == "" {
		cfg.TesseractBin = "tesseract"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("subtitles: CacheDir is required")
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating subtitle cache %s: %w", cfg.CacheDir, err)
	}

	return &Service{
		ffmpegBin:    cfg.FFmpegBin,
		ffprobeBin:   cfg.FFprobeBin,
		tesseractBin: cfg.TesseractBin,
		ocrLanguage:  cfg.OCRLanguage,
		cacheDir:     cfg.CacheDir,
		timeout:      cfg.Timeout,
		logger:       cfg.Logger,
	}, nil
}

// CacheDir reports where converted subtitles are stored.
func (s *Service) CacheDir() string { return s.cacheDir }

// Convert renders one subtitle track to WebVTT and returns the cached file
// path. The cache key includes the source file's size and modification time, so
// a replaced file is re-extracted rather than served from a stale conversion.
func (s *Service) Convert(ctx context.Context, mediaPath string, trackIndex int) (string, error) {
	if trackIndex < 0 {
		return "", fmt.Errorf("subtitle track index must not be negative, got %d", trackIndex)
	}

	info, err := os.Stat(mediaPath)
	if err != nil {
		return "", fmt.Errorf("inspecting %s: %w", mediaPath, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory", mediaPath)
	}

	target := filepath.Join(s.cacheDir, s.cacheKey(mediaPath, info, trackIndex)+".vtt")
	if cached, err := os.Stat(target); err == nil && cached.Size() > 0 {
		s.logger.DebugContext(ctx, "serving cached subtitles", "track", trackIndex)
		return target, nil
	}

	if err := s.extract(ctx, mediaPath, trackIndex, target); err != nil {
		return "", err
	}
	return target, nil
}

// extract runs ffmpeg and commits the result atomically.
func (s *Service) extract(ctx context.Context, mediaPath string, trackIndex int, target string) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	tmp, err := os.CreateTemp(s.cacheDir, "extract-*.vtt")
	if err != nil {
		return fmt.Errorf("creating temporary subtitle file: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("closing temporary subtitle file: %w", err)
	}
	defer func() { _ = os.Remove(tmpName) }()

	cmd := exec.CommandContext(ctx, s.ffmpegBin,
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", mediaPath,
		"-map", "0:"+strconv.Itoa(trackIndex),
		"-f", "webvtt",
		tmpName,
	)
	output, runErr := cmd.CombinedOutput()
	if runErr != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("extracting subtitles from %s: %w", mediaPath, ctx.Err())
		}
		message := string(output)
		if message == "" {
			message = runErr.Error()
		}
		return fmt.Errorf("extracting subtitle track %d from %s: %s", trackIndex, mediaPath, message)
	}

	// ffmpeg writes a bare header for a track with no cues; treating that as a
	// failure gives the client a clear answer instead of an empty overlay.
	written, err := os.Stat(tmpName)
	if err != nil {
		return fmt.Errorf("reading extracted subtitles: %w", err)
	}
	if written.Size() <= int64(len("WEBVTT\n")) {
		return fmt.Errorf("%w (track %d)", ErrNoCues, trackIndex)
	}

	if err := os.Rename(tmpName, target); err != nil {
		return fmt.Errorf("committing subtitle file: %w", err)
	}

	s.logger.InfoContext(ctx, "extracted subtitle track",
		"track", trackIndex, "bytes", written.Size(), "source", filepath.Base(mediaPath))
	return nil
}

// cacheKey derives a stable file name from the source identity and the track.
func (s *Service) cacheKey(mediaPath string, info os.FileInfo, trackIndex int) string {
	return s.cacheKeyFor(mediaPath, info, trackIndex, "text")
}

// cacheKeyFor folds the conversion mode into the key as well, so an OCR result
// can never be served for a text extraction of the same track or the reverse.
func (s *Service) cacheKeyFor(mediaPath string, info os.FileInfo, trackIndex int, mode string) string {
	abs, err := filepath.Abs(mediaPath)
	if err != nil {
		abs = mediaPath
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%d\x00%d\x00%s",
		abs, info.Size(), info.ModTime().UnixNano(), trackIndex, mode))
	return hex.EncodeToString(sum[:16])
}
