package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func okResponse() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "served")
	})
}

func TestHostAllowlist(t *testing.T) {
	t.Parallel()

	handler := HostAllowlist([]string{"localhost", "127.0.0.1", "::1", "media.example.com"}, okResponse())

	tests := []struct {
		name       string
		host       string
		wantStatus int
	}{
		{name: "a listed name", host: "media.example.com", wantStatus: http.StatusOK},
		{name: "a listed name with a port", host: "media.example.com:8642", wantStatus: http.StatusOK},
		{name: "a listed name in another case", host: "Media.Example.COM", wantStatus: http.StatusOK},
		{name: "a listed name with a trailing dot", host: "media.example.com.", wantStatus: http.StatusOK},
		{name: "loopback", host: "127.0.0.1:8642", wantStatus: http.StatusOK},
		{name: "ipv6 loopback", host: "[::1]:8642", wantStatus: http.StatusOK},
		{
			// DNS rebinding: the browser is told this name resolves to the
			// server, so the attacker's page is same-origin with it. The name
			// is not one the operator listed, so the request stops here.
			name:       "an unlisted name",
			host:       "attacker.example",
			wantStatus: http.StatusMisdirectedRequest,
		},
		{name: "an unlisted name with a port", host: "attacker.example:1234", wantStatus: http.StatusMisdirectedRequest},
		{name: "a missing host", host: "", wantStatus: http.StatusMisdirectedRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/api/libraries", nil)
			req.Host = tt.host
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)

			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if tt.wantStatus == http.StatusMisdirectedRequest &&
				!strings.Contains(recorder.Body.String(), `"code":"host_not_allowed"`) {
				t.Errorf("body = %q, want the host_not_allowed code", recorder.Body.String())
			}
		})
	}
}

func TestDefaultAllowedHosts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		addr      string
		want      []string
		unwanted  string
		wantEmpty bool
	}{
		{addr: "127.0.0.1:8642", want: nil},
		{addr: ":8642", unwanted: "0.0.0.0"},
		{addr: "0.0.0.0:8642", unwanted: "0.0.0.0"},
		{addr: "192.168.1.10:8642", want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			t.Parallel()

			got := DefaultAllowedHosts(tt.addr)
			for _, host := range []string{"localhost", "127.0.0.1", "::1"} {
				if !contains(got, host) {
					t.Errorf("DefaultAllowedHosts(%q) = %v, want it to include %q", tt.addr, got, host)
				}
			}
			if tt.unwanted != "" && contains(got, tt.unwanted) {
				t.Errorf("DefaultAllowedHosts(%q) = %v, want no wildcard address", tt.addr, got)
			}
			switch tt.addr {
			case "192.168.1.10:8642":
				if !contains(got, "192.168.1.10") {
					t.Errorf("DefaultAllowedHosts(%q) = %v, want the bound address included", tt.addr, got)
				}
			}
		})
	}
}

func TestParseAllowedHosts(t *testing.T) {
	t.Parallel()

	hosts, err := ParseAllowedHosts([]string{"media.example.com, TV.LAN:8642", "127.0.0.1"})
	if err != nil {
		t.Fatalf("ParseAllowedHosts: %v", err)
	}
	for _, want := range []string{"media.example.com", "tv.lan", "127.0.0.1"} {
		if !contains(hosts, want) {
			t.Errorf("ParseAllowedHosts = %v, want it to include %q", hosts, want)
		}
	}

	for _, bad := range []string{"https://media.example.com", "http://x"} {
		if _, err := ParseAllowedHosts([]string{bad}); err == nil {
			t.Errorf("ParseAllowedHosts(%q) should reject a URL", bad)
		}
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestCrossOriginProtection covers the state-changing request shapes a page on
// another origin can produce. The server enables this protection by default,
// which is what closes the "add a library at / and scan it" path in A-2.
func TestCrossOriginProtection(t *testing.T) {
	t.Parallel()

	protection := http.NewCrossOriginProtection()
	handler := protection.Handler(okResponse())

	tests := []struct {
		name       string
		method     string
		host       string
		secFetch   string
		origin     string
		wantStatus int
	}{
		{
			name:       "a same-origin post is allowed",
			method:     http.MethodPost,
			host:       "media.example.com",
			secFetch:   "same-origin",
			wantStatus: http.StatusOK,
		},
		{
			name:       "a cross-site post is refused",
			method:     http.MethodPost,
			host:       "media.example.com",
			secFetch:   "cross-site",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a cross-site delete is refused",
			method:     http.MethodDelete,
			host:       "media.example.com",
			secFetch:   "cross-site",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a cross-site get is still served",
			method:     http.MethodGet,
			host:       "media.example.com",
			secFetch:   "cross-site",
			wantStatus: http.StatusOK,
		},
		{
			name:       "an older browser with a foreign origin is refused",
			method:     http.MethodPost,
			host:       "media.example.com",
			origin:     "https://evil.example",
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "an older browser with the same origin is allowed",
			method:     http.MethodPost,
			host:       "media.example.com",
			origin:     "http://media.example.com",
			wantStatus: http.StatusOK,
		},
		{
			name:       "a client that sends neither header is a non-browser client",
			method:     http.MethodPost,
			host:       "media.example.com",
			wantStatus: http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(tt.method, "/api/libraries", nil)
			req.Host = tt.host
			if tt.secFetch != "" {
				req.Header.Set("Sec-Fetch-Site", tt.secFetch)
			}
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, req)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestServerEnablesCrossOriginProtection(t *testing.T) {
	t.Parallel()

	server := NewServer(Deps{CrossOrigin: true})
	if server.crossOrigin == nil {
		t.Fatal("NewServer with CrossOrigin should install cross-origin protection")
	}
	if server := NewServer(Deps{}); server.crossOrigin != nil {
		t.Fatal("NewServer without CrossOrigin should not install it")
	}
}
