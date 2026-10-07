package subtitles

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestService(t *testing.T, ffmpeg string) *Service {
	t.Helper()

	service, err := New(Config{
		FFmpegBin: ffmpeg,
		CacheDir:  t.TempDir(),
		Timeout:   30 * time.Second,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

func TestNew_RequiresCacheDir(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{Logger: testLogger()}); err == nil {
		t.Fatal("expected an error when CacheDir is empty")
	}
}

func TestConvert_RejectsMissingFile(t *testing.T) {
	t.Parallel()

	service := newTestService(t, "ffmpeg")

	_, err := service.Convert(context.Background(), filepath.Join(t.TempDir(), "nope.mkv"), 2)
	if err == nil {
		t.Fatal("expected an error for a file that does not exist")
	}
}

func TestConvert_RejectsDirectory(t *testing.T) {
	t.Parallel()

	service := newTestService(t, "ffmpeg")

	_, err := service.Convert(context.Background(), t.TempDir(), 2)
	if err == nil {
		t.Fatal("expected an error when the path is a directory")
	}
}

func TestConvert_RejectsNegativeTrack(t *testing.T) {
	t.Parallel()

	service := newTestService(t, "ffmpeg")
	media := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(media, []byte("not really a video"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	if _, err := service.Convert(context.Background(), media, -1); err == nil {
		t.Fatal("expected an error for a negative track index")
	}
}

func TestConvert_FfmpegFailureIsReported(t *testing.T) {
	t.Parallel()

	service := newTestService(t, "ffmpeg")
	media := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(media, []byte("not really a video"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	if _, err := service.Convert(context.Background(), media, 2); err == nil {
		t.Fatal("expected ffmpeg to fail on a file that is not media")
	}
}

func TestConvert_MissingFfmpegBinary(t *testing.T) {
	t.Parallel()

	service := newTestService(t, "definitely-not-ffmpeg")
	media := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(media, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	if _, err := service.Convert(context.Background(), media, 2); err == nil {
		t.Fatal("expected an error when the ffmpeg binary is missing")
	}
}

func TestCacheKey_ChangesWithTheSource(t *testing.T) {
	t.Parallel()

	service := newTestService(t, "ffmpeg")
	media := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(media, []byte("aaaa"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	before, err := os.Stat(media)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	keyBefore := service.cacheKey(media, before, 2)

	// A different track must not share a cache entry.
	if same := service.cacheKey(media, before, 3); same == keyBefore {
		t.Error("different tracks produced the same cache key")
	}

	// A changed file must not reuse the old conversion.
	if err := os.WriteFile(media, []byte("bbbbbbbb"), 0o644); err != nil {
		t.Fatalf("rewriting media file: %v", err)
	}
	after, err := os.Stat(media)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if same := service.cacheKey(media, after, 2); same == keyBefore {
		t.Error("a modified file reused the previous cache key")
	}
}

func TestCacheKey_IsStable(t *testing.T) {
	t.Parallel()

	service := newTestService(t, "ffmpeg")
	media := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(media, []byte("aaaa"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}
	info, err := os.Stat(media)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if service.cacheKey(media, info, 2) != service.cacheKey(media, info, 2) {
		t.Error("the cache key is not stable for the same input")
	}
}

func TestConvert_UsesTheCacheOnASecondCall(t *testing.T) {
	t.Parallel()

	// A fake ffmpeg that writes a valid WebVTT file, so the cache path can be
	// exercised without depending on real media.
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"for a in \"$@\"; do out=\"$a\"; done\n" +
		"printf 'WEBVTT\\n\\n00:00:01.000 --> 00:00:02.000\\nHello\\n' > \"$out\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake ffmpeg: %v", err)
	}

	service := newTestService(t, fake)
	media := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(media, []byte("pretend media"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	first, err := service.Convert(context.Background(), media, 2)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	body, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("reading converted subtitles: %v", err)
	}
	if !strings.HasPrefix(string(body), "WEBVTT") {
		t.Errorf("converted file does not look like WebVTT: %q", string(body))
	}

	// Remove the fake binary: a second call must be served from the cache and
	// therefore still succeed.
	if err := os.Remove(fake); err != nil {
		t.Fatalf("removing fake ffmpeg: %v", err)
	}
	second, err := service.Convert(context.Background(), media, 2)
	if err != nil {
		t.Fatalf("Convert from cache: %v", err)
	}
	if second != first {
		t.Errorf("cached path = %q, want %q", second, first)
	}
}

func TestConvert_HeaderOnlyOutputIsNoCues(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\n" +
		"out=\"\"\n" +
		"for a in \"$@\"; do out=\"$a\"; done\n" +
		"printf 'WEBVTT\\n' > \"$out\"\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake ffmpeg: %v", err)
	}

	service := newTestService(t, fake)
	media := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(media, []byte("pretend media"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	_, err := service.Convert(context.Background(), media, 2)
	if !errors.Is(err, ErrNoCues) {
		t.Fatalf("error = %v, want ErrNoCues", err)
	}
}
