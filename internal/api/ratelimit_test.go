package api

import (
	"net/http"
	"testing"
)

// TestRateLimitedResponseCarriesSecurityHeaders pins the wrapper order: the
// limiter sits inside the security headers, so a refusal is a response like any
// other and cannot be the one reply that lacks the policy.
func TestRateLimitedResponseCarriesSecurityHeaders(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, func(deps *Deps) {
		deps.RateLimit = func(http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusTooManyRequests)
			})
		}
	})

	recorder := env.do(t, http.MethodGet, "/api/entities", "")
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 from the configured limiter", recorder.Code)
	}
	// The policy itself is deliberately absent on API paths; the headers that
	// apply everywhere are what show the refusal went through the same wrapper.
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff = %q, want it on every response", got)
	}
	if got := recorder.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Errorf("X-Frame-Options = %q, want it on every response", got)
	}

	// A UI path keeps its policy even when the limiter refuses it, which is the
	// order the wrapper promises: headers outside, limiter inside.
	uiRecorder := env.do(t, http.MethodGet, "/", "")
	if uiRecorder.Code != http.StatusTooManyRequests {
		t.Fatalf("UI status = %d, want 429 from the same limiter", uiRecorder.Code)
	}
	if policy := uiRecorder.Header().Get("Content-Security-Policy"); policy == "" {
		t.Error("a refused document should still carry the content security policy")
	}
}

// TestRateLimiterIsNotAppliedWhenUnset keeps the default honest: with no
// limiter configured the API answers normally, because the flag is opt-in.
func TestRateLimiterIsNotAppliedWhenUnset(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	if code := env.do(t, http.MethodGet, "/api/entities", "").Code; code != http.StatusOK {
		t.Errorf("status = %d, want 200 with no limiter configured", code)
	}
}
