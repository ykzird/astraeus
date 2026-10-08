// Package ratelimit bounds how often one client may call the API.
//
// The server's expensive work is reachable over HTTP: a playback negotiation
// starts an ffmpeg process, a scan walks a library, and the artwork proxy makes
// an upstream request. A client that retries in a loop, or a script that walks
// every entity, can therefore cost far more than it looks like. This is the
// cheap bound in front of that.
//
// The limiter is a token bucket per client. A bucket is generous up to its burst
// so an ordinary page load is never refused, and refills at a steady rate so a
// runaway client settles at that rate instead of being cut off mid-flight.
package ratelimit

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"github.com/ykzird/astraeus/internal/observability"
)

// Config configures a Limiter.
type Config struct {
	// Rate is the sustained requests per second a client may make.
	Rate float64
	// Burst is how many requests a client may make at once. Zero means the rate
	// rounded up, which is the smallest bucket that still allows a normal page
	// load to fire its requests together.
	Burst int
	// Key identifies the client a request belongs to. An empty key means the
	// request cannot be attributed, and it is allowed rather than charged to a
	// shared bucket.
	Key func(*http.Request) string
	// Exempt, when set, marks requests that bypass the limit entirely.
	Exempt func(*http.Request) bool
	// IdleTTL is how long an unused bucket is kept before it is swept. Zero
	// means ten minutes.
	IdleTTL time.Duration
	// SweepThreshold is the number of keys at which a sweep is attempted on
	// insert. Zero means 1024.
	SweepThreshold int
	// Now is a test seam for the clock. Zero means time.Now.
	Now func() time.Time

	Metrics *observability.Metrics
	Logger  *slog.Logger
}

// Limiter refuses requests over a per-client rate.
type Limiter struct {
	rate           float64
	burst          float64
	key            func(*http.Request) string
	exempt         func(*http.Request) bool
	idleTTL        time.Duration
	sweepThreshold int
	now            func() time.Time

	metrics *observability.Metrics
	logger  *slog.Logger

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// bucket is one client's allowance.
type bucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

// New validates the configuration and builds a Limiter.
func New(cfg Config) (*Limiter, error) {
	if cfg.Rate <= 0 {
		return nil, fmt.Errorf("ratelimit: rate must be positive, got %v", cfg.Rate)
	}
	if cfg.Key == nil {
		return nil, errors.New("ratelimit: Key is required, otherwise every request shares one bucket")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Burst <= 0 {
		cfg.Burst = int(math.Ceil(cfg.Rate))
	}
	if cfg.IdleTTL <= 0 {
		cfg.IdleTTL = 10 * time.Minute
	}
	if cfg.SweepThreshold <= 0 {
		cfg.SweepThreshold = 1024
	}

	return &Limiter{
		rate:           cfg.Rate,
		burst:          float64(cfg.Burst),
		key:            cfg.Key,
		exempt:         cfg.Exempt,
		idleTTL:        cfg.IdleTTL,
		sweepThreshold: cfg.SweepThreshold,
		now:            cfg.Now,
		metrics:        cfg.Metrics,
		logger:         cfg.Logger,
		buckets:        make(map[string]*bucket),
	}, nil
}

// Burst reports the bucket size actually in use, which is what the rate rounds
// up to when the configuration did not name one.
func (l *Limiter) Burst() int { return int(l.burst) }

// Middleware wraps a handler with the limit.
func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if l.exempt != nil && l.exempt(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := l.key(r)
		if key == "" {
			// Nothing to attribute the request to. Refusing it would punish a
			// client for the server's own inability to tell who it is.
			next.ServeHTTP(w, r)
			return
		}
		if !l.allow(key, l.now()) {
			l.deny(w, r, key)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// allow charges one request to a key, reporting whether it may proceed.
func (l *Limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, ok := l.buckets[key]
	if !ok {
		// seen is stamped at creation, before the sweep below: a bucket with a
		// zero stamp looks idle to the sweep, which would evict the very entry
		// this request is about to use and hand a heavy client a fresh bucket.
		entry = &bucket{tokens: l.burst, last: now, seen: now}
		l.buckets[key] = entry
		if len(l.buckets) >= l.sweepThreshold {
			l.sweepLocked(now)
		}
	} else {
		// Refill for the time that passed, never above the burst. Doing it on
		// read rather than on a timer means an idle server holds no goroutine
		// and an idle client's bucket costs nothing to leave alone.
		elapsed := now.Sub(entry.last).Seconds()
		if elapsed > 0 {
			entry.tokens = math.Min(l.burst, entry.tokens+elapsed*l.rate)
			entry.last = now
		}
		entry.seen = now
	}

	if entry.tokens < 1 {
		return false
	}
	entry.tokens--
	return true
}

// sweepLocked drops buckets that have not been used for IdleTTL, which is what
// keeps a limiter keyed by address from growing without bound on a busy network.
func (l *Limiter) sweepLocked(now time.Time) {
	// The sweep is itself work, so it is rate-limited rather than run on every
	// insert once the map is large.
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for key, entry := range l.buckets {
		if now.Sub(entry.seen) > l.idleTTL {
			delete(l.buckets, key)
		}
	}
}

// deny answers a request the limiter refused, in the same shape as every other
// API error, so a client parses one error format and not two.
func (l *Limiter) deny(w http.ResponseWriter, r *http.Request, key string) {
	l.metrics.IncCounter(observability.MetricRateLimited,
		"API requests refused by the rate limiter.", nil)

	// The client address is logged, not the identity: the key may be either, and
	// the identity is already on the request log line for this same request.
	l.logger.WarnContext(r.Context(), "rate limited",
		"path", r.URL.Path, "method", r.Method, "client", key)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{
		Code:    "rate_limited",
		Message: "too many requests; this client is over its rate limit",
	})
}
