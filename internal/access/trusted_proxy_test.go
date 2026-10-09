package access

import (
	"net/http/httptest"
	"testing"
)

// TestTrustedProxy_AddressForms is the regression test for the IPv6 trap the
// troubleshooting page documents.
//
// A single-prefix configuration does not cover both loopback forms. A connection
// from the IPv6 loopback arrives as ::1, which 127.0.0.1/32 does not contain, and
// the failure is an `untrusted_source` 403 rather than anything that names the
// address - so an operator who wrote the IPv4 form and a proxy that connected
// over IPv6 are left with a refusal that looks like a misconfigured header.
//
// The IPv4-mapped form is the one case that does not need both: it is unmapped
// before any comparison, so ::ffff:127.0.0.1 is treated as 127.0.0.1.
func TestTrustedProxy_AddressForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		remote  string
		trusted string
		want    bool
	}{
		{"IPv4 loopback, IPv4 prefix", "127.0.0.1:5000", "127.0.0.1/32", true},
		{"IPv4 loopback, IPv6 prefix only", "127.0.0.1:5000", "::1/128", false},
		{"IPv6 loopback, IPv4 prefix only", "[::1]:5000", "127.0.0.1/32", false},
		{"IPv6 loopback, IPv6 prefix", "[::1]:5000", "::1/128", true},
		{"both prefixes cover IPv4", "127.0.0.1:5000", "127.0.0.1/32,::1/128", true},
		{"both prefixes cover IPv6", "[::1]:5000", "127.0.0.1/32,::1/128", true},
		{
			// The mapped form is unmapped, so the IPv4 prefix matches it. This is
			// the case that made the trap look intermittent: the same proxy can
			// produce either form depending on how it dials.
			"IPv4-mapped loopback is treated as IPv4",
			"[::ffff:127.0.0.1]:5000", "127.0.0.1/32", true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			prefixes, err := ParseTrustedProxies([]string{tt.trusted})
			if err != nil {
				t.Fatalf("parsing %q: %v", tt.trusted, err)
			}
			gate := &Gate{trustedProxies: prefixes}

			request := httptest.NewRequest("GET", "/api/libraries", nil)
			request.RemoteAddr = tt.remote

			addr, ok := clientAddr(request)
			if !ok {
				t.Fatalf("the address in %q could not be parsed", tt.remote)
			}
			if got := gate.isTrusted(addr); got != tt.want {
				t.Errorf("trusted=%q remote=%q: isTrusted = %v, want %v",
					tt.trusted, tt.remote, got, tt.want)
			}
		})
	}
}
