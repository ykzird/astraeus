package streaming

import (
	"context"
	"sync"
)

// CachingProber memoises probe results. Probing spawns a process, and the same
// file is probed on every playback negotiation, so caching removes a
// per-request process spawn from the critical path.
type CachingProber struct {
	inner Prober

	mu    sync.RWMutex
	cache map[string]*MediaInfo
}

// NewCachingProber wraps a Prober.
func NewCachingProber(inner Prober) *CachingProber {
	return &CachingProber{inner: inner, cache: make(map[string]*MediaInfo)}
}

// Probe returns the cached result when present, otherwise probes and stores it.
func (p *CachingProber) Probe(ctx context.Context, path string) (*MediaInfo, error) {
	p.mu.RLock()
	cached, ok := p.cache[path]
	p.mu.RUnlock()
	if ok {
		return cached, nil
	}

	info, err := p.inner.Probe(ctx, path)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.cache[path] = info
	p.mu.Unlock()
	return info, nil
}

// Invalidate drops the cached result for a path, for use when a file changes.
func (p *CachingProber) Invalidate(path string) {
	p.mu.Lock()
	delete(p.cache, path)
	p.mu.Unlock()
}

// Len reports how many results are cached.
func (p *CachingProber) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.cache)
}
