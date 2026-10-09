package streaming

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type countingProber struct {
	mu    sync.Mutex
	calls int
	info  *MediaInfo
	err   error
}

func (p *countingProber) Probe(context.Context, string) (*MediaInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.info, p.err
}

func (p *countingProber) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestCachingProber_MemoisesResults(t *testing.T) {
	t.Parallel()

	// A file that exists. The cache keys on the file's identity, so a path with
	// nothing behind it is not cacheable - which is the point of S-8's fix, and
	// is what this test was originally written without.
	path := writeProbeFixture(t, "movie.mkv")

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264", Container: "mp4"}}
	prober := NewCachingProber(inner)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		info, err := prober.Probe(ctx, path)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if info.VideoCodec != "h264" {
			t.Errorf("video codec = %q, want h264", info.VideoCodec)
		}
	}

	if got := inner.callCount(); got != 1 {
		t.Errorf("underlying prober called %d times, want 1", got)
	}
	if got := prober.Len(); got != 1 {
		t.Errorf("cache size = %d, want 1", got)
	}
}

func TestCachingProber_DistinguishesPaths(t *testing.T) {
	t.Parallel()

	first := writeProbeFixture(t, "a.mkv")
	second := writeProbeFixture(t, "b.mkv")

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264"}}
	prober := NewCachingProber(inner)
	ctx := context.Background()

	if _, err := prober.Probe(ctx, first); err != nil {
		t.Fatalf("Probe a: %v", err)
	}
	if _, err := prober.Probe(ctx, second); err != nil {
		t.Fatalf("Probe b: %v", err)
	}

	if got := inner.callCount(); got != 2 {
		t.Errorf("underlying prober called %d times, want 2", got)
	}
}

func TestCachingProber_DoesNotCacheErrors(t *testing.T) {
	t.Parallel()

	inner := &countingProber{err: errors.New("ffprobe exploded")}
	prober := NewCachingProber(inner)
	ctx := context.Background()

	bothered := writeProbeFixture(t, "broken.mkv")
	for i := 0; i < 2; i++ {
		if _, err := prober.Probe(ctx, bothered); err == nil {
			t.Fatal("expected an error")
		}
	}

	if got := inner.callCount(); got != 2 {
		t.Errorf("underlying prober called %d times, want 2 (errors must not be cached)", got)
	}
}

func TestCachingProber_Invalidate(t *testing.T) {
	t.Parallel()

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264"}}
	prober := NewCachingProber(inner)
	ctx := context.Background()

	path := writeProbeFixture(t, "movie.mkv")
	if _, err := prober.Probe(ctx, path); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	prober.Invalidate(path)
	if _, err := prober.Probe(ctx, path); err != nil {
		t.Fatalf("Probe after invalidate: %v", err)
	}

	if got := inner.callCount(); got != 2 {
		t.Errorf("underlying prober called %d times, want 2", got)
	}
}

// TestCachingProber_ReprobesAReplacedFile is the regression test for S-8.
//
// The cache was keyed on the path alone, so a file replaced in place - how a
// Sonarr or Radarr upgrade works, and how any re-encode that keeps its name works
// - kept the old stream list for the life of the process. The wrong answer is not
// slow, it is wrong: the wrong direct-play decision, the wrong burn index, or
// `-map 0:<old>?` where the `?` makes the missing audio stream silent.
func TestCachingProber_ReprobesAReplacedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(path, []byte("the original file"), 0o644); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264", AudioCodec: "aac"}}
	cache := NewCachingProber(inner)

	first, err := cache.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if inner.callCount() != 1 {
		t.Fatalf("the first probe called the inner prober %d times, want 1", inner.callCount())
	}

	// The same file asked for again is answered from the cache.
	if _, err := cache.Probe(context.Background(), path); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if inner.callCount() != 1 {
		t.Errorf("asking twice for an unchanged file probed %d times, want 1: the cache is "+
			"there so negotiation does not spawn a process", inner.callCount())
	}

	// The upgrade: a different file at the same path. Its size differs, which is
	// what stat can see.
	if err := os.WriteFile(path, []byte("a completely different and longer file"), 0o644); err != nil {
		t.Fatalf("replacing the file: %v", err)
	}
	inner.mu.Lock()
	inner.info = &MediaInfo{VideoCodec: "hevc", AudioCodec: "ac3"}
	inner.mu.Unlock()

	second, err := cache.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe after the replacement: %v", err)
	}
	if inner.callCount() != 2 {
		t.Errorf("a replaced file was answered from the cache (%d probes): the old stream "+
			"list is then used for direct play, for the burn index and for -map",
			inner.callCount())
	}
	if first == second {
		t.Error("the replaced file returned the identical MediaInfo value")
	}
	if second.VideoCodec != "hevc" {
		t.Errorf("video codec after the replacement = %q, want the new file's hevc",
			second.VideoCodec)
	}
}

// TestCachingProber_ReprobesWhenOnlyTheModTimeChanges covers the other half of the
// identity check: a replacement that happens to be the same size, which is
// ordinary for a re-encode at the same settings.
func TestCachingProber_ReprobesWhenOnlyTheModTimeChanges(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "film.mkv")
	body := []byte("exactly this many bytes")
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264"}}
	cache := NewCachingProber(inner)
	if _, err := cache.Probe(context.Background(), path); err != nil {
		t.Fatalf("first probe: %v", err)
	}

	// Same length, new content, and a modification time the filesystem will
	// record differently. The sleep is a filesystem granularity, not a race: some
	// filesystems record mtime to the second.
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("replacing the file: %v", err)
	}

	if _, err := cache.Probe(context.Background(), path); err != nil {
		t.Fatalf("probe after the replacement: %v", err)
	}
	if inner.callCount() != 2 {
		t.Errorf("a same-size replacement was answered from the cache (%d probes), so only "+
			"the size is being compared", inner.callCount())
	}
}

// TestCachingProber_DropsAFailedFile covers the case where the file has gone: the
// next request should look again rather than be answered from a cache about a
// file that is no longer there.
func TestCachingProber_DropsAFailedFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264"}}
	cache := NewCachingProber(inner)
	if _, err := cache.Probe(context.Background(), path); err != nil {
		t.Fatalf("first probe: %v", err)
	}

	// The file disappears and probing starts failing.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the file: %v", err)
	}
	inner.mu.Lock()
	inner.err = errors.New("no such file")
	inner.mu.Unlock()

	if _, err := cache.Probe(context.Background(), path); err == nil {
		t.Fatal("probing a removed file reported success")
	}
	if cache.Len() != 0 {
		t.Errorf("the cache holds %d entries after a failed probe, want 0", cache.Len())
	}
}

// TestCachingProber_ExpiresOnItsTTL is the backstop for what identity cannot see:
// a file edited without its size or modification time changing.
func TestCachingProber_ExpiresOnItsTTL(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "film.mkv")
	if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264"}}
	cache := NewCachingProber(inner)

	// A clock the test controls, so the TTL can pass without waiting for it.
	now := time.Now()
	cache.now = func() time.Time { return now }
	cache.SetLimits(0, time.Minute)

	if _, err := cache.Probe(context.Background(), path); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	if _, err := cache.Probe(context.Background(), path); err != nil {
		t.Fatalf("second probe: %v", err)
	}
	if inner.callCount() != 1 {
		t.Fatalf("an unchanged file within its TTL probed %d times, want 1", inner.callCount())
	}

	now = now.Add(2 * time.Minute)
	if _, err := cache.Probe(context.Background(), path); err != nil {
		t.Fatalf("probe after the TTL: %v", err)
	}
	if inner.callCount() != 2 {
		t.Errorf("an entry past its TTL was still served (%d probes)", inner.callCount())
	}
}

// TestCachingProber_EvictsWhenFull bounds the map. A library is walked file by
// file, so without a bound the cache grows with the library and the entries are
// never freed.
func TestCachingProber_EvictsWhenFull(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264"}}
	cache := NewCachingProber(inner)
	cache.SetLimits(4, time.Hour)

	for i := 0; i < 20; i++ {
		path := filepath.Join(dir, "film"+strconv.Itoa(i)+".mkv")
		if err := os.WriteFile(path, []byte("bytes"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
		if _, err := cache.Probe(context.Background(), path); err != nil {
			t.Fatalf("probing %s: %v", path, err)
		}
	}

	if got := cache.Len(); got > 4 {
		t.Errorf("the cache holds %d entries after 20 distinct files with a bound of 4", got)
	}
}

// writeProbeFixture writes a file for the cache to have an identity for.
func writeProbeFixture(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("probe fixture bytes"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}
