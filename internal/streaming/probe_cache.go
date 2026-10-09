package streaming

import (
	"context"
	"os"
	"sync"
	"time"
)

// CachingProber memoises probe results. Probing spawns a process, and the same
// file is probed on every playback negotiation, so caching removes a
// per-request process spawn from the critical path.
//
// The cache is keyed on the file's identity as well as its path. Keying on the
// path alone meant a file replaced in place - which is how a Sonarr or Radarr
// upgrade works, and how any re-encode that keeps its name works - kept the old
// stream list for the life of the process. The result was not a slow answer but a
// wrong one: wrong direct play, a wrong burn index, or `-map 0:<old>?` where the
// `?` makes the missing stream silent (S-8 of the 2026-10-09 review).
type CachingProber struct {
	inner Prober

	mu    sync.RWMutex
	cache map[string]probeEntry
	// now is the clock, so a test can expire an entry without sleeping.
	now func() time.Time
	// maxEntries bounds the map. A library is walked and probed file by file, so
	// without a bound the cache grows with the library.
	maxEntries int
	// ttl is the backstop for what identity cannot see: a file edited without its
	// size or modification time changing, and a filesystem that reports both
	// coarsely. It is long enough that a library in normal use never re-probes.
	ttl time.Duration
}

// probeEntry is one cached result together with what the file looked like when it
// was taken.
type probeEntry struct {
	info  *MediaInfo
	size  int64
	mtime time.Time
	// at is when the entry was stored, used for the TTL backstop and eviction.
	at time.Time
}

// defaultProbeTTL is how long a cached probe is trusted when the file's size and
// modification time are unchanged. Long, because the identity check is what
// normally decides, and this only catches a change stat cannot see.
const defaultProbeTTL = 24 * time.Hour

// defaultProbeCacheEntries bounds the map. Sized for a large household library,
// since an entry is small and re-probing is a process spawn.
const defaultProbeCacheEntries = 4096

// NewCachingProber wraps a Prober.
func NewCachingProber(inner Prober) *CachingProber {
	return &CachingProber{
		inner:      inner,
		cache:      make(map[string]probeEntry),
		now:        time.Now,
		maxEntries: defaultProbeCacheEntries,
		ttl:        defaultProbeTTL,
	}
}

// SetLimits changes the cache's bound and TTL. Zero leaves a value unchanged.
func (p *CachingProber) SetLimits(maxEntries int, ttl time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if maxEntries > 0 {
		p.maxEntries = maxEntries
	}
	if ttl > 0 {
		p.ttl = ttl
	}
}

// Probe returns the cached result when the file is the one that was probed,
// otherwise probes and stores it.
func (p *CachingProber) Probe(ctx context.Context, path string) (*MediaInfo, error) {
	// The file's identity is read first, because it is what decides whether the
	// cached answer is about this file at all.
	stat, statErr := os.Stat(path)
	statted := statErr == nil

	var (
		size  int64
		mtime time.Time
	)
	if statted {
		size = stat.Size()
		mtime = stat.ModTime()
	}

	p.mu.RLock()
	entry, ok := p.cache[path]
	p.mu.RUnlock()

	if ok && p.entryStillDescribes(entry, statted, size, mtime) {
		return entry.info, nil
	}

	info, err := p.inner.Probe(ctx, path)
	if err != nil {
		// A file that cannot be probed is dropped rather than kept: the next
		// request should look again rather than be answered from a cache about a
		// file that has gone.
		p.Invalidate(path)
		return nil, err
	}

	p.store(path, probeEntry{info: info, size: size, mtime: mtime, at: p.now()})
	return info, nil
}

// entryStillDescribes reports whether a cached entry is about the file as it is
// now.
//
// `statted` says whether the file could be looked at at all, and it is checked
// first for clarity rather than because the comparisons would miss the case: an
// absent file stats as size 0 with a zero time, and a cached entry that recorded
// a real size fails the size comparison anyway. Saying it outright is what stops
// the next reader concluding that a zero size means "nothing to compare".
//
// Where the stat succeeds, a different size or a different modification time means
// a different file. Both are compared, because a re-encode at the same settings
// produces a different file of the same length.
func (p *CachingProber) entryStillDescribes(entry probeEntry, statted bool, size int64, mtime time.Time) bool {
	if !statted {
		return false
	}
	if p.now().Sub(entry.at) > p.ttl {
		return false
	}
	if size != entry.size {
		return false
	}
	return mtime.Equal(entry.mtime)
}

// store writes an entry, making room first when the map is at its bound.
func (p *CachingProber) store(path string, entry probeEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.cache) >= p.maxEntries {
		p.evictLocked()
	}
	p.cache[path] = entry
}

// evictLocked makes room. Expired entries go first, because they were going to be
// dropped on their next lookup anyway; if that is not enough the oldest goes,
// since a probe is a process spawn and any entry is worth more than none.
func (p *CachingProber) evictLocked() {
	now := p.now()

	for key, entry := range p.cache {
		if now.Sub(entry.at) > p.ttl {
			delete(p.cache, key)
		}
	}
	if len(p.cache) < p.maxEntries {
		return
	}

	// Still full: drop the oldest entry, one at a time, until there is room. A
	// scan rather than a heap, because the map is small and this is the rare path.
	for len(p.cache) >= p.maxEntries {
		var (
			oldestKey string
			oldest    time.Time
			found     bool
		)
		for key, entry := range p.cache {
			if !found || entry.at.Before(oldest) {
				oldestKey, oldest, found = key, entry.at, true
			}
		}
		if !found {
			return
		}
		delete(p.cache, oldestKey)
	}
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
