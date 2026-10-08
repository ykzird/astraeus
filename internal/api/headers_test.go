package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestSecurityHeaders covers the headers a browser has to be told about, and the
// one place they are deliberately absent.
func TestSecurityHeaders(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	// A document gets the whole set, including the policy.
	page := env.do(t, http.MethodGet, "/", "")
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
		// Same-origin keeps the media and the artwork reachable by this UI and
		// by nothing else that happens to be in a browser tab.
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := page.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if got := page.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("the UI document was served without a content security policy")
	}
	if got := page.Header().Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS = %q; this server speaks plain HTTP, where it means nothing", got)
	}

	// JSON gets the transport-level headers but not a policy it could never act
	// on.
	api := env.do(t, http.MethodGet, "/api/health", "")
	if got := api.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("API nosniff = %q, want it set", got)
	}
	if got := api.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("an API response carries a content security policy (%q); a JSON body cannot use one", got)
	}
	metrics := env.do(t, http.MethodGet, "/metrics", "")
	if got := metrics.Header().Get("Content-Security-Policy"); got != "" {
		t.Errorf("/metrics carries a content security policy (%q); it is text for a scraper", got)
	}
}

// TestContentSecurityPolicyCoversWhatThePlayerNeeds pins the directives that
// exist for a reason a reader cannot guess from the string. Dropping one is
// silent until playback breaks in a browser, which no unit test would notice.
func TestContentSecurityPolicyCoversWhatThePlayerNeeds(t *testing.T) {
	t.Parallel()

	for _, want := range []string{
		"media-src 'self' blob:",  // MSE plays a blob URL
		"worker-src 'self' blob:", // hls.js demuxes in a worker built from one
		"img-src 'self'",          // artwork is proxied, never fetched from a provider
		"frame-ancestors 'none'",
		"object-src 'none'",
		"base-uri 'none'",
	} {
		if !strings.Contains(contentSecurityPolicy, want) {
			t.Errorf("the policy is missing %q:\n%s", want, contentSecurityPolicy)
		}
	}

	// The two escape hatches this UI does not need, and whose absence is the
	// point: every script and style it loads is a file it serves itself.
	for _, unwanted := range []string{"unsafe-inline", "unsafe-eval"} {
		if strings.Contains(contentSecurityPolicy, unwanted) {
			t.Errorf("the policy contains %q, which this UI does not need:\n%s", unwanted, contentSecurityPolicy)
		}
	}
}
