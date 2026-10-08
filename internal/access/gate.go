// Package access puts an identity-aware gate in front of the server.
//
// The specification calls for integration with Tailscale or Cloudflare Access
// rather than a built-in user database (§7.1). Those products authenticate the
// user at the edge and then forward the request with an identity header, so
// this package's job is to verify that the assertion came from a trusted proxy
// and to reject everything else.
//
// The important security property: an identity header is only believed when the
// request arrives from a configured trusted address. Headers are trivially
// forgeable by anyone who can reach the port directly, so trusting them without
// that check would be worse than having no gate at all.
package access

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/jok/astraeus-media/internal/observability"
)

// Mode selects how the gate establishes identity.
type Mode string

const (
	// ModeNone disables the gate. It is the default so that a local install
	// works out of the box, and it must never be used on an untrusted network.
	ModeNone Mode = "none"
	// ModeProxy believes an identity header, but only from a trusted address.
	ModeProxy Mode = "proxy"
	// ModeToken requires a bearer token. Intended for API clients and scripts;
	// a browser cannot send one on a plain navigation, so browser access
	// belongs behind ModeProxy.
	ModeToken Mode = "token"
)

// ParseMode validates a user-supplied mode.
func ParseMode(value string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(value))) {
	case ModeNone:
		return ModeNone, nil
	case ModeProxy:
		return ModeProxy, nil
	case ModeToken:
		return ModeToken, nil
	default:
		return "", fmt.Errorf("invalid auth mode %q: want %q, %q or %q", value, ModeNone, ModeProxy, ModeToken)
	}
}

// Config configures a Gate.
type Config struct {
	Mode Mode

	// IdentityHeaders are checked in order; the first non-empty value wins.
	// Tailscale's `tailscale serve` sets Tailscale-User-Login, and Cloudflare
	// Access sets Cf-Access-Authenticated-User-Email.
	IdentityHeaders []string

	// TrustedProxies are the CIDRs whose identity headers are believed.
	TrustedProxies []netip.Prefix

	// Token is the expected bearer token in ModeToken.
	Token string

	// ExemptPaths bypass the gate entirely, for liveness probes. Exact matches
	// only, so a prefix cannot accidentally open a subtree.
	ExemptPaths []string

	Metrics *observability.Metrics
	Logger  *slog.Logger
}

// DefaultIdentityHeaders are the headers the supported proxies set.
var DefaultIdentityHeaders = []string{
	"Tailscale-User-Login",
	"Cf-Access-Authenticated-User-Email",
}

// Gate enforces an access policy around a handler.
type Gate struct {
	mode            Mode
	identityHeaders []string
	trustedProxies  []netip.Prefix
	token           string
	exempt          map[string]bool
	metrics         *observability.Metrics
	logger          *slog.Logger
}

// New validates the configuration and builds a Gate. It fails closed: a proxy
// mode without trusted addresses, or a token mode without a token, is an error
// rather than a silently open server.
func New(cfg Config) (*Gate, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeNone
	}

	gate := &Gate{
		mode:    cfg.Mode,
		token:   cfg.Token,
		metrics: cfg.Metrics,
		logger:  cfg.Logger,
		exempt:  make(map[string]bool, len(cfg.ExemptPaths)),
	}
	for _, path := range cfg.ExemptPaths {
		if path = strings.TrimSpace(path); path != "" {
			gate.exempt[path] = true
		}
	}

	switch cfg.Mode {
	case ModeNone:
		gate.logger.Warn("the API is not gated; do not expose it beyond a trusted network")
		return gate, nil

	case ModeProxy:
		gate.identityHeaders = cfg.IdentityHeaders
		if len(gate.identityHeaders) == 0 {
			gate.identityHeaders = DefaultIdentityHeaders
		}
		if len(cfg.TrustedProxies) == 0 {
			return nil, errors.New("access: proxy mode requires at least one trusted proxy address, " +
				"otherwise any client could forge an identity header")
		}
		gate.trustedProxies = cfg.TrustedProxies
		return gate, nil

	case ModeToken:
		if strings.TrimSpace(cfg.Token) == "" {
			return nil, errors.New("access: token mode requires a token")
		}
		return gate, nil

	default:
		return nil, fmt.Errorf("access: unknown mode %q", cfg.Mode)
	}
}

// Mode reports the configured mode.
func (g *Gate) Mode() Mode { return g.mode }

// Middleware wraps a handler with the access policy.
func (g *Gate) Middleware(next http.Handler) http.Handler {
	if g.mode == ModeNone {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.exempt[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}

		var (
			identity string
			err      error
		)
		switch g.mode {
		case ModeProxy:
			identity, err = g.authoriseProxy(r)
		case ModeToken:
			identity, err = g.authoriseToken(r)
		}
		if err != nil {
			g.deny(w, r, err)
			return
		}

		g.metrics.IncCounter(observability.MetricAuthGranted,
			"Requests admitted by the access gate.", map[string]string{"mode": string(g.mode)})

		ctx := contextWithIdentity(r.Context(), identity)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authoriseProxy believes an identity header only from a trusted address.
func (g *Gate) authoriseProxy(r *http.Request) (string, error) {
	addr, ok := clientAddr(r)
	if !ok {
		return "", &Denied{Status: http.StatusForbidden, Code: "untrusted_source",
			Message: "the client address could not be determined"}
	}

	if !g.isTrusted(addr) {
		// The header may well be present and valid-looking; it is still not
		// believable from an address the administrator did not nominate.
		return "", &Denied{Status: http.StatusForbidden, Code: "untrusted_source",
			Message: "requests must arrive through the configured access proxy"}
	}

	for _, header := range g.identityHeaders {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value, nil
		}
	}

	return "", &Denied{Status: http.StatusUnauthorized, Code: "unauthenticated",
		Message: "the access proxy did not supply an identity for this request"}
}

// authoriseToken checks a bearer token in constant time.
func (g *Gate) authoriseToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", &Denied{Status: http.StatusUnauthorized, Code: "unauthenticated",
			Message: "expected an Authorization: Bearer token"}
	}

	presented := strings.TrimSpace(header[len(prefix):])
	// ConstantTimeCompare returns 0 for differing lengths, so this does not
	// leak the token length through timing.
	if subtle.ConstantTimeCompare([]byte(presented), []byte(g.token)) != 1 {
		return "", &Denied{Status: http.StatusUnauthorized, Code: "invalid_token",
			Message: "the bearer token is not valid"}
	}
	return "token", nil
}

func (g *Gate) isTrusted(addr netip.Addr) bool {
	for _, prefix := range g.trustedProxies {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

func (g *Gate) deny(w http.ResponseWriter, r *http.Request, err error) {
	var denied *Denied
	status := http.StatusUnauthorized
	code := "unauthenticated"
	message := "access denied"
	if errors.As(err, &denied) {
		status, code, message = denied.Status, denied.Code, denied.Message
	}

	// Only the client address is logged; the offending header value is not,
	// because it is attacker-controlled text.
	g.logger.WarnContext(r.Context(), "access denied",
		"code", code, "path", r.URL.Path, "remote", r.RemoteAddr)
	g.metrics.IncCounter(observability.MetricAuthDenied,
		"Requests refused by the access gate.", map[string]string{"reason": code})

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("WWW-Authenticate", `Bearer realm="astraeus"`)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message})
}

// Denied describes a refusal, so callers can map it onto a status code.
type Denied struct {
	Status  int
	Code    string
	Message string
}

func (d *Denied) Error() string { return d.Code + ": " + d.Message }

// clientAddr extracts the peer address.
//
// X-Forwarded-For is deliberately ignored: a client can set it, and honouring
// it here would let anyone on the network claim to be a trusted proxy. The
// address the connection actually came from is the only trustworthy one.
func clientAddr(r *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}, false
	}
	return addr.Unmap(), true
}

// ClientAddress exposes the peer address the gate is willing to trust, so other
// middleware can key on the same address rather than deriving a second, weaker
// opinion about X-Forwarded-For. It returns false when the address cannot be
// parsed, which a caller must treat as "unattributable" rather than "trusted".
func ClientAddress(r *http.Request) (netip.Addr, bool) {
	return clientAddr(r)
}

// ParseTrustedProxies parses comma-separated CIDRs or bare addresses.
func ParseTrustedProxies(values []string) ([]netip.Prefix, error) {
	var prefixes []netip.Prefix
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if prefix, err := netip.ParsePrefix(part); err == nil {
				prefixes = append(prefixes, prefix.Masked())
				continue
			}
			addr, err := netip.ParseAddr(part)
			if err != nil {
				return nil, fmt.Errorf("access: %q is neither a CIDR nor an address", part)
			}
			addr = addr.Unmap()
			prefixes = append(prefixes, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	return prefixes, nil
}

/* ---- identity in the request context ---- */

type identityKey struct{}

func contextWithIdentity(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, identityKey{}, identity)
}

// IdentityFromContext returns the authenticated identity, if the gate set one.
func IdentityFromContext(ctx context.Context) string {
	identity, _ := ctx.Value(identityKey{}).(string)
	return identity
}
