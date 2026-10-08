package ratelimit

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jok/astraeus-media/internal/observability"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func byHeader(r *http.Request) string { return r.Header.Get("X-Client") }

// newLimiter builds a limiter whose clock the test controls.
func newLimiter(t *testing.T, cfg Config, now *time.Time) *Limiter {
	t.Helper()

	cfg.Key = byHeader
	if cfg.Logger == nil {
		cfg.Logger = discardLogger()
	}
	cfg.Now = func() time.Time { return *now }
	limiter, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return limiter
}

// serve runs one request through the limiter and returns the recorder.
func serve(limiter *Limiter, client string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "/api/entities", nil)
	if client != "" {
		request.Header.Set("X-Client", client)
	}
	recorder := httptest.NewRecorder()
	limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(recorder, request)
	return recorder
}

// TestLimiter_AllowsTheBurstThenRefuses is the contract: a client may spend its
// bucket at once, and the next request is a 429 with a Retry-After rather than a
// dropped connection or a silent slowdown.
func TestLimiter_AllowsTheBurstThenRefuses(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	metrics := observability.New()
	limiter := newLimiter(t, Config{Rate: 1, Burst: 3, Metrics: metrics}, &now)

	for i := 0; i < 3; i++ {
		if code := serve(limiter, "a").Code; code != http.StatusOK {
			t.Fatalf("request %d = %d, want 200 within the burst", i+1, code)
		}
	}

	recorder := serve(limiter, "a")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("fourth request = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "rate_limited") {
		t.Errorf("the refusal should carry the API's error shape, got %s", body)
	}
	if rendered := metrics.Render(); !strings.Contains(rendered, observability.MetricRateLimited) {
		t.Errorf("a refusal was not counted in %s", observability.MetricRateLimited)
	}
}

// TestLimiter_RefillsOverTime covers the other half of a token bucket: a client
// that waits gets its allowance back, so a limit is a rate and not a quota.
func TestLimiter_RefillsOverTime(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	limiter := newLimiter(t, Config{Rate: 2, Burst: 1}, &now)

	if code := serve(limiter, "a").Code; code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	if code := serve(limiter, "a").Code; code != http.StatusTooManyRequests {
		t.Fatalf("second request = %d, want 429 once the burst is spent", code)
	}

	// Two tokens a second, so half a second buys exactly one.
	now = now.Add(500 * time.Millisecond)
	if code := serve(limiter, "a").Code; code != http.StatusOK {
		t.Errorf("after the refill = %d, want 200", code)
	}
}

// TestLimiter_KeysAreIndependent pins that one noisy client cannot spend another
// client's allowance.
func TestLimiter_KeysAreIndependent(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	limiter := newLimiter(t, Config{Rate: 1, Burst: 1}, &now)

	if code := serve(limiter, "a").Code; code != http.StatusOK {
		t.Fatalf("client a first = %d, want 200", code)
	}
	if code := serve(limiter, "a").Code; code != http.StatusTooManyRequests {
		t.Fatalf("client a second = %d, want 429", code)
	}
	if code := serve(limiter, "b").Code; code != http.StatusOK {
		t.Errorf("client b = %d, want 200: an unrelated client must not be charged", code)
	}
}

// TestLimiter_ExemptBypasses covers the liveness probe: a limit that can refuse
// a health check turns a busy server into an unhealthy one.
func TestLimiter_ExemptBypasses(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	limiter := newLimiter(t, Config{
		Rate: 1, Burst: 1,
		Exempt: func(r *http.Request) bool { return r.URL.Path == "/api/health" },
	}, &now)

	for i := 0; i < 5; i++ {
		request := httptest.NewRequest(http.MethodGet, "/api/health", nil)
		request.Header.Set("X-Client", "a")
		recorder := httptest.NewRecorder()
		limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("exempt request %d = %d, want 200", i+1, recorder.Code)
		}
	}
}

// TestLimiter_UnattributableRequestIsAllowed pins the fail-open choice: a
// request the server cannot name is allowed rather than charged to a bucket it
// shares with everyone else, which would turn one client's flood into an outage.
func TestLimiter_UnattributableRequestIsAllowed(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	limiter := newLimiter(t, Config{Rate: 1, Burst: 1}, &now)

	for i := 0; i < 5; i++ {
		if code := serve(limiter, "").Code; code != http.StatusOK {
			t.Fatalf("unattributable request %d = %d, want 200", i+1, code)
		}
	}
}

// TestLimiter_SweepsIdleBuckets is the memory bound: a limiter keyed by address
// must not grow forever on a network that keeps presenting new ones.
func TestLimiter_SweepsIdleBuckets(t *testing.T) {
	t.Parallel()

	now := time.Unix(0, 0)
	limiter := newLimiter(t, Config{
		Rate: 100, Burst: 1,
		IdleTTL:        time.Millisecond,
		SweepThreshold: 2,
	}, &now)

	serve(limiter, "old")
	now = now.Add(10 * time.Millisecond)
	serve(limiter, "new") // reaching the threshold sweeps the idle key

	limiter.mu.Lock()
	defer limiter.mu.Unlock()
	if _, stale := limiter.buckets["old"]; stale {
		t.Error("an idle bucket survived the sweep")
	}
	if _, kept := limiter.buckets["new"]; !kept {
		t.Error("the bucket in use was swept")
	}
	if len(limiter.buckets) != 1 {
		t.Errorf("buckets = %d, want 1", len(limiter.buckets))
	}
}

// TestNew_RejectsBadConfig pins fail-closed configuration: a limiter with no way
// to tell clients apart would throttle everyone as one.
func TestNew_RejectsBadConfig(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{Rate: 0, Key: byHeader}); err == nil {
		t.Error("a zero rate should be an error, not a limiter that never allows")
	}
	if _, err := New(Config{Rate: 1}); err == nil {
		t.Error("a limiter without a key function should be refused")
	}
}
