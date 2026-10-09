package access

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/ykzird/astraeus/internal/observability"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// okHandler reports the identity the gate established.
func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		identity := IdentityFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "identity="+identity)
	})
}

func mustPrefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()

	prefixes, err := ParseTrustedProxies(values)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%v): %v", values, err)
	}
	return prefixes
}

func request(method, path, remote string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remote
	return req
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	for _, want := range []Mode{ModeNone, ModeProxy, ModeToken} {
		if got, err := ParseMode(string(want)); err != nil || got != want {
			t.Errorf("ParseMode(%q) = (%q, %v), want %q", want, got, err, want)
		}
	}
	if _, err := ParseMode("basic"); err == nil {
		t.Error("expected an error for an unknown mode")
	}
	if got, err := ParseMode("  PROXY "); err != nil || got != ModeProxy {
		t.Errorf("ParseMode should trim and fold case, got (%q, %v)", got, err)
	}
}

func TestNew_FailsClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "proxy mode without an identity header",
			cfg: Config{
				Mode:           ModeProxy,
				TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
				Logger:         testLogger(),
			},
		},
		{
			name: "proxy mode with both known identity headers",
			cfg: Config{
				Mode:           ModeProxy,
				IdentityHeader: HeaderTailscaleLogin + "," + HeaderCloudflareEmail,
				TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
				Logger:         testLogger(),
			},
		},
		{
			name: "proxy mode with an invalid header name",
			cfg: Config{
				Mode:           ModeProxy,
				IdentityHeader: "Tailscale User Login",
				TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
				Logger:         testLogger(),
			},
		},
		{
			name: "proxy mode without trusted proxies",
			cfg: Config{
				Mode:           ModeProxy,
				IdentityHeader: HeaderTailscaleLogin,
				Logger:         testLogger(),
			},
		},
		{
			name: "token mode without a token",
			cfg:  Config{Mode: ModeToken, Logger: testLogger()},
		},
		{
			name: "token mode with a blank token",
			cfg:  Config{Mode: ModeToken, Token: "   ", Logger: testLogger()},
		},
		{
			name: "unknown mode",
			cfg:  Config{Mode: Mode("magic"), Logger: testLogger()},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := New(tt.cfg); err == nil {
				t.Error("expected New to reject this configuration rather than serve openly")
			}
		})
	}
}

func TestModeNone_AllowsEverything(t *testing.T) {
	t.Parallel()

	gate, err := New(Config{Mode: ModeNone, Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recorder := httptest.NewRecorder()
	gate.Middleware(okHandler()).ServeHTTP(recorder, request(http.MethodGet, "/api/libraries", "203.0.113.9:1234"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
}

func TestProxyMode(t *testing.T) {
	t.Parallel()

	gate, err := New(Config{
		Mode:           ModeProxy,
		IdentityHeader: HeaderTailscaleLogin,
		TrustedProxies: mustPrefixes(t, "127.0.0.1/32", "100.64.0.0/10", "::1/128"),
		Metrics:        observability.New(),
		Logger:         testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := gate.Middleware(okHandler())

	tests := []struct {
		name       string
		remote     string
		headers    map[string]string
		wantStatus int
		wantCodes  string
	}{
		{
			name:       "trusted proxy with a tailscale identity",
			remote:     "127.0.0.1:5000",
			headers:    map[string]string{"Tailscale-User-Login": "viewer@example.com"},
			wantStatus: http.StatusOK,
			wantCodes:  "identity=viewer@example.com",
		},
		{
			name:       "trusted proxy with no identity",
			remote:     "127.0.0.1:5000",
			headers:    nil,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "trusted proxy with a blank identity",
			remote:     "127.0.0.1:5000",
			headers:    map[string]string{"Tailscale-User-Login": "   "},
			wantStatus: http.StatusUnauthorized,
		},
		{
			// The whole point of the trusted-address check: a forged header
			// from anywhere else must not be believed.
			name:       "untrusted source forging an identity header",
			remote:     "203.0.113.9:5000",
			headers:    map[string]string{"Tailscale-User-Login": "attacker@example.com"},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "untrusted source without a header",
			remote:     "203.0.113.9:5000",
			headers:    nil,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "ipv6 loopback is trusted when configured",
			remote:     "[::1]:5000",
			headers:    map[string]string{"Tailscale-User-Login": "viewer@example.com"},
			wantStatus: http.StatusOK,
			wantCodes:  "identity=viewer@example.com",
		},
		{
			name:       "unparseable remote address is refused",
			remote:     "not-an-address",
			headers:    map[string]string{"Tailscale-User-Login": "viewer@example.com"},
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := request(http.MethodGet, "/api/libraries", tt.remote)
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantCodes != "" && !strings.Contains(recorder.Body.String(), tt.wantCodes) {
				t.Errorf("body = %q, want it to contain %q", recorder.Body.String(), tt.wantCodes)
			}
		})
	}
}

// A deployment behind Cloudflare Access names that header instead, and then the
// Tailscale header is not believed at all.
func TestProxyMode_CloudflareHeader(t *testing.T) {
	t.Parallel()

	gate, err := New(Config{
		Mode:           ModeProxy,
		IdentityHeader: HeaderCloudflareEmail,
		TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
		Metrics:        observability.New(),
		Logger:         testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := request(http.MethodGet, "/api/libraries", "127.0.0.1:5000")
	req.Header.Set(HeaderCloudflareEmail, "viewer@example.com")
	recorder := httptest.NewRecorder()
	gate.Middleware(okHandler()).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "identity=viewer@example.com") {
		t.Errorf("body = %q, want the Cloudflare identity", recorder.Body.String())
	}
}

// TestProxyMode_RefusesACompetingIdentityHeader is the regression test for A-1.
//
// Before this, the gate believed whichever of the two known proxy headers was
// non-empty, first match winning. Each real proxy overwrites only its own
// header, so a user of the proxy that was actually deployed could send the
// other one and be believed as anyone — an administrator included. The
// deployment now names exactly one header, and a value in any other known
// identity header is a refusal rather than a silent second chance.
func TestProxyMode_RefusesACompetingIdentityHeader(t *testing.T) {
	t.Parallel()

	gate, err := New(Config{
		Mode:           ModeProxy,
		IdentityHeader: HeaderTailscaleLogin,
		TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
		Metrics:        observability.New(),
		Logger:         testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := gate.Middleware(okHandler())

	tests := []struct {
		name       string
		headers    map[string]string
		wantStatus int
		wantCode   string
		wantTwice  bool
	}{
		{
			name: "the other proxy's header alongside the configured one",
			headers: map[string]string{
				HeaderTailscaleLogin:  "viewer@example.com",
				HeaderCloudflareEmail: "admin@example.com",
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "competing_identity_header",
		},
		{
			name: "the other proxy's header with no configured one",
			headers: map[string]string{
				HeaderCloudflareEmail: "admin@example.com",
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "competing_identity_header",
		},
		{
			name: "the configured header twice",
			headers: map[string]string{
				HeaderTailscaleLogin: "viewer@example.com",
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "ambiguous_identity",
			wantTwice:  true,
		},
		{
			name: "the configured header as a comma-separated list",
			headers: map[string]string{
				HeaderTailscaleLogin: "viewer@example.com,admin@example.com",
			},
			wantStatus: http.StatusForbidden,
			wantCode:   "ambiguous_identity",
		},
		{
			// The honest single-header request still works.
			name: "only the configured header",
			headers: map[string]string{
				HeaderTailscaleLogin: "viewer@example.com",
			},
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := request(http.MethodGet, "/api/libraries", "127.0.0.1:5000")
			for name, value := range tt.headers {
				if tt.wantTwice {
					req.Header.Add(name, value)
					req.Header.Add(name, "second@example.com")
					continue
				}
				req.Header.Set(name, value)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantCode != "" && !strings.Contains(recorder.Body.String(), `"code":"`+tt.wantCode+`"`) {
				t.Errorf("body = %q, want the %s code", recorder.Body.String(), tt.wantCode)
			}
		})
	}
}

func TestProxyMode_IgnoresForwardedFor(t *testing.T) {
	t.Parallel()

	// Honouring X-Forwarded-For would let any client claim to be the proxy.
	gate, err := New(Config{
		Mode:           ModeProxy,
		IdentityHeader: HeaderTailscaleLogin,
		TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
		Logger:         testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := request(http.MethodGet, "/api/libraries", "203.0.113.9:5000")
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "127.0.0.1")
	req.Header.Set("Tailscale-User-Login", "attacker@example.com")

	recorder := httptest.NewRecorder()
	gate.Middleware(okHandler()).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403: forwarded headers must not establish trust", recorder.Code)
	}
}

func TestTokenMode(t *testing.T) {
	t.Parallel()

	const token = "s3cret-token-value"
	gate, err := New(Config{Mode: ModeToken, Token: token, Metrics: observability.New(), Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := gate.Middleware(okHandler())

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{name: "correct token", header: "Bearer " + token, wantStatus: http.StatusOK},
		{name: "case insensitive scheme", header: "bearer " + token, wantStatus: http.StatusOK},
		{name: "wrong token", header: "Bearer nope", wantStatus: http.StatusUnauthorized},
		{name: "wrong token of the same length", header: "Bearer " + strings.Repeat("x", len(token)), wantStatus: http.StatusUnauthorized},
		{name: "token prefix only", header: "Bearer " + token[:4], wantStatus: http.StatusUnauthorized},
		{name: "missing header", header: "", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", header: "Basic " + token, wantStatus: http.StatusUnauthorized},
		{name: "bare token", header: token, wantStatus: http.StatusUnauthorized},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := request(http.MethodGet, "/api/libraries", "203.0.113.9:5000")
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestExemptPaths(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  Config
	}{
		{
			name: "proxy mode",
			cfg: Config{
				Mode:           ModeProxy,
				IdentityHeader: HeaderTailscaleLogin,
				TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
				ExemptPaths:    []string{"/api/health"},
				Logger:         testLogger(),
			},
		},
		{
			name: "token mode",
			cfg: Config{
				Mode: ModeToken,
				// Long enough for the minimum; the value is irrelevant to this
				// test, which is about which paths bypass the gate.
				Token:       "0123456789abcdef0123456789abcdef",
				ExemptPaths: []string{"/api/health"},
				Logger:      testLogger(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gate, err := New(tt.cfg)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			handler := gate.Middleware(okHandler())

			// Unauthenticated liveness probe succeeds.
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request(http.MethodGet, "/api/health", "203.0.113.9:1"))
			if recorder.Code != http.StatusOK {
				t.Errorf("exempt path status = %d, want 200", recorder.Code)
			}

			// Anything else does not.
			recorder = httptest.NewRecorder()
			handler.ServeHTTP(recorder, request(http.MethodGet, "/api/libraries", "203.0.113.9:1"))
			if recorder.Code == http.StatusOK {
				t.Error("a non-exempt path was allowed without credentials")
			}

			// Exemption is an exact match, not a prefix.
			recorder = httptest.NewRecorder()
			handler.ServeHTTP(recorder, request(http.MethodGet, "/api/health/../../etc", "203.0.113.9:1"))
			if recorder.Code == http.StatusOK {
				t.Error("exemption leaked to a descendant path")
			}
		})
	}
}

func TestDenialsAreJSONAndCounted(t *testing.T) {
	t.Parallel()

	metrics := observability.New()
	gate, err := New(Config{
		Mode:           ModeProxy,
		IdentityHeader: HeaderTailscaleLogin,
		TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
		Metrics:        metrics,
		Logger:         testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	handler := gate.Middleware(okHandler())

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request(http.MethodGet, "/api/libraries", "203.0.113.9:1"))

	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("content type = %q, want JSON", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"code":"untrusted_source"`) {
		t.Errorf("body = %q, want the untrusted_source code", body)
	}

	rendered := metrics.Render()
	if !strings.Contains(rendered, `astraeus_auth_denied_total{reason="untrusted_source"} 1`) {
		t.Errorf("the denial was not counted:\n%s", rendered)
	}
}

func TestSuccessIsCounted(t *testing.T) {
	t.Parallel()

	metrics := observability.New()
	gate, err := New(Config{
		Mode:           ModeProxy,
		IdentityHeader: HeaderTailscaleLogin,
		TrustedProxies: mustPrefixes(t, "127.0.0.1/32"),
		Metrics:        metrics,
		Logger:         testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	req := request(http.MethodGet, "/api/libraries", "127.0.0.1:1")
	req.Header.Set("Tailscale-User-Login", "viewer@example.com")
	recorder := httptest.NewRecorder()
	gate.Middleware(okHandler()).ServeHTTP(recorder, req)

	if rendered := metrics.Render(); !strings.Contains(rendered, `astraeus_auth_granted_total{mode="proxy"} 1`) {
		t.Errorf("the grant was not counted:\n%s", rendered)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	t.Parallel()

	prefixes, err := ParseTrustedProxies([]string{"127.0.0.1/32, 100.64.0.0/10", "::1"})
	if err != nil {
		t.Fatalf("ParseTrustedProxies: %v", err)
	}
	if len(prefixes) != 3 {
		t.Fatalf("got %d prefixes, want 3: %v", len(prefixes), prefixes)
	}

	for _, tc := range []struct {
		addr string
		want bool
	}{
		{"127.0.0.1", true},
		{"127.0.0.2", false},
		{"100.101.0.1", true},
		{"100.128.0.1", false},
		{"203.0.113.1", false},
		{"::1", true},
	} {
		addr := netip.MustParseAddr(tc.addr)
		found := false
		for _, prefix := range prefixes {
			if prefix.Contains(addr) {
				found = true
			}
		}
		if found != tc.want {
			t.Errorf("Contains(%s) = %v, want %v", tc.addr, found, tc.want)
		}
	}

	if _, err := ParseTrustedProxies([]string{"not-a-cidr"}); err == nil {
		t.Error("expected an error for an unparseable entry")
	}
	if prefixes, err := ParseTrustedProxies([]string{"", "  "}); err != nil || len(prefixes) != 0 {
		t.Errorf("blank entries should be skipped, got %v (%v)", prefixes, err)
	}
}

func TestIdentityFromContext_EmptyWhenAbsent(t *testing.T) {
	t.Parallel()

	if got := IdentityFromContext(request(http.MethodGet, "/", "127.0.0.1:1").Context()); got != "" {
		t.Errorf("identity = %q, want empty", got)
	}
}

// TestNew_RefusesAShortToken is the regression test for the token half of A-6.
//
// Any non-blank token was accepted. The comparison is exact and constant-time, so
// the token's length is the whole of the attacker's problem, and a three-character
// token is a three-character search - the sort of thing a server set up to
// "survive a reboot" quietly has.
func TestNew_RefusesAShortToken(t *testing.T) {
	t.Parallel()

	for _, token := range []string{
		"t",
		"hunter2",
		"0123456789abcde", // one short of the minimum
	} {
		if _, err := New(Config{Mode: ModeToken, Token: token, Logger: testLogger()}); err == nil {
			t.Errorf("New accepted a %d-character token %q", len(token), token)
		}
	}

	// The minimum itself, and the documented generator's output, are accepted.
	for _, token := range []string{
		"0123456789abcdef", // exactly the minimum
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // openssl rand -hex 32
	} {
		if _, err := New(Config{Mode: ModeToken, Token: token, Logger: testLogger()}); err != nil {
			t.Errorf("New refused a %d-character token: %v", len(token), err)
		}
	}

	// A blank token is still the other refusal, with its own message.
	if _, err := New(Config{Mode: ModeToken, Token: "   ", Logger: testLogger()}); err == nil {
		t.Error("New accepted a blank token")
	}
}

// TestDeny_ChallengesOnlyWhereItHelps is the last of A-10's smaller items.
//
// Every denial carried `WWW-Authenticate: Bearer`, including proxy-mode 403s and
// the 403s that are not about credentials at all. The header is an instruction:
// "authenticate with a bearer token and try again". In proxy mode that is advice
// the client cannot take - the gate does not read a token - and on an
// already-authenticated refusal it is wrong about what the failure was.
func TestDeny_ChallengesOnlyWhereItHelps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		mode          Mode
		status        int
		code          string
		wantChallenge bool
	}{
		{
			name:          "a token-mode 401 is a challenge",
			mode:          ModeToken,
			status:        http.StatusUnauthorized,
			code:          "unauthenticated",
			wantChallenge: true,
		},
		{
			name:          "a token-mode 403 is not",
			mode:          ModeToken,
			status:        http.StatusForbidden,
			code:          "untrusted_source",
			wantChallenge: false,
		},
		{
			// The case the finding named: a proxy-mode refusal told the client to
			// present a token.
			name:          "a proxy-mode 403 is not",
			mode:          ModeProxy,
			status:        http.StatusForbidden,
			code:          "untrusted_source",
			wantChallenge: false,
		},
		{
			name:          "a 401 in a mode that takes no token is not",
			mode:          ModeNone,
			status:        http.StatusUnauthorized,
			code:          "ambiguous_identity",
			wantChallenge: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gate := &Gate{mode: tt.mode, logger: testLogger(), metrics: observability.New()}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/libraries", nil)

			gate.deny(recorder, request, &Denied{Status: tt.status, Code: tt.code, Message: "no"})

			if recorder.Code != tt.status {
				t.Errorf("status = %d, want %d", recorder.Code, tt.status)
			}
			got := recorder.Header().Get("WWW-Authenticate")
			if tt.wantChallenge && got == "" {
				t.Error("a token-mode 401 carried no challenge, so a client is not told how " +
					"to authenticate")
			}
			if !tt.wantChallenge && got != "" {
				t.Errorf("the denial carries %q, which tells the client to retry with a "+
					"bearer token that this mode does not read", got)
			}
		})
	}
}
