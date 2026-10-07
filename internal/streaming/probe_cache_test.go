package streaming

import (
	"context"
	"errors"
	"sync"
	"testing"
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

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264", Container: "mp4"}}
	prober := NewCachingProber(inner)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		info, err := prober.Probe(ctx, "/media/movie.mkv")
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

	inner := &countingProber{info: &MediaInfo{VideoCodec: "h264"}}
	prober := NewCachingProber(inner)
	ctx := context.Background()

	if _, err := prober.Probe(ctx, "/media/a.mkv"); err != nil {
		t.Fatalf("Probe a: %v", err)
	}
	if _, err := prober.Probe(ctx, "/media/b.mkv"); err != nil {
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

	for i := 0; i < 2; i++ {
		if _, err := prober.Probe(ctx, "/media/broken.mkv"); err == nil {
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

	if _, err := prober.Probe(ctx, "/media/movie.mkv"); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	prober.Invalidate("/media/movie.mkv")
	if _, err := prober.Probe(ctx, "/media/movie.mkv"); err != nil {
		t.Fatalf("Probe after invalidate: %v", err)
	}

	if got := inner.callCount(); got != 2 {
		t.Errorf("underlying prober called %d times, want 2", got)
	}
}
