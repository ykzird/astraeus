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
	"sort"
	"strings"

	"github.com/ykzird/astraeus/internal/observability"
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

// minimumTokenLength is the shortest bearer token the gate will accept.
//
// The token is compared exactly and in constant time, so an attacker has to guess
// the whole string; the length is what makes guessing hopeless rather than slow.
// Sixteen characters is 64 bits if the token is random hex, and the documented
// generator produces 32 bytes.
const minimumTokenLength = 16

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

	// IdentityHeader is the single header the access proxy sets. Proxy mode
	// requires exactly one: believing two headers at once would let a user of
	// whichever proxy is actually in front present the other proxy's header,
	// which neither proxy overwrites, and so claim any identity it names.
	// Tailscale's `tailscale serve` sets HeaderTailscaleLogin; Cloudflare
	// Access sets HeaderCloudflareEmail.
	IdentityHeader string

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

// The identity headers the supported identity-aware proxies set. A deployment
// behind one of them names exactly one of these.
const (
	// HeaderTailscaleLogin is set by `tailscale serve`.
	HeaderTailscaleLogin = "Tailscale-User-Login"
	// HeaderCloudflareEmail is set by Cloudflare Access.
	HeaderCloudflareEmail = "Cf-Access-Authenticated-User-Email"
)

// identityProviders maps a convenience name to the header that proxy sets, so
// an operator can write `--auth-provider tailscale` instead of remembering the
// exact header spelling. Naming no provider is not an option: the gate has no
// safe default, because believing both headers at once is the vulnerability
// this mapping exists to prevent.
var identityProviders = map[string]string{
	"tailscale":  HeaderTailscaleLogin,
	"cloudflare": HeaderCloudflareEmail,
}

// IdentityProviderHeader resolves a provider name to its identity header.
func IdentityProviderHeader(provider string) (string, bool) {
	header, ok := identityProviders[strings.ToLower(strings.TrimSpace(provider))]
	return header, ok
}

// IdentityProviders lists the accepted --auth-provider values, sorted, for help
// text and error messages.
func IdentityProviders() []string {
	names := make([]string, 0, len(identityProviders))
	for name := range identityProviders {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Gate enforces an access policy around a handler.
type Gate struct {
	mode           Mode
	identityHeader string
	trustedProxies []netip.Prefix
	token          string
	exempt         map[string]bool
	metrics        *observability.Metrics
	logger         *slog.Logger
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
		header := strings.TrimSpace(cfg.IdentityHeader)
		if header == "" {
			return nil, fmt.Errorf("access: proxy mode requires exactly one identity header: "+
				"name the proxy in front with a provider (%s) or give its header with "+
				"a custom header name; there is no safe default, because a header the real "+
				"proxy does not overwrite is an identity anyone can claim",
				strings.Join(IdentityProviders(), ", "))
		}
		if !validHeaderName(header) {
			return nil, fmt.Errorf("access: %q is not a valid HTTP header name", cfg.IdentityHeader)
		}
		if len(cfg.TrustedProxies) == 0 {
			return nil, errors.New("access: proxy mode requires at least one trusted proxy address, " +
				"otherwise any client could forge an identity header")
		}
		gate.identityHeader = header
		gate.trustedProxies = cfg.TrustedProxies
		return gate, nil

	case ModeToken:
		if strings.TrimSpace(cfg.Token) == "" {
			return nil, errors.New("access: token mode requires a token")
		}
		// A minimum length, because any non-blank token was accepted and the
		// comparison is exact rather than derived: a three-character token is a
		// three-character search. The comparison itself is constant-time, so
		// length is the whole of the attacker's problem, and this makes brute
		// force infeasible rather than merely slow (A-6 of the 2026-10-09
		// review). Sixteen hex characters is 64 bits; the documented way to
		// generate one is 32 bytes.
		if len(cfg.Token) < minimumTokenLength {
			return nil, fmt.Errorf(
				"access: the token is %d characters and must be at least %d; generate one with `openssl rand -hex 32`",
				len(cfg.Token), minimumTokenLength)
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

	present, clean := headerValues(r, g.identityHeader)
	if !clean {
		// More than one value for one header, or a comma-separated list: the
		// gate cannot say which of them the proxy actually asserted, so it
		// believes none.
		return "", &Denied{Status: http.StatusForbidden, Code: "ambiguous_identity",
			Message: "the identity header was supplied more than once"}
	}

	// A second identity header is refused outright rather than ignored. Only
	// the configured proxy overwrites its own header, so a value in any other
	// one is attacker-controlled and is exactly how a user of one proxy
	// impersonates a user of the other. Refusing keeps that attempt visible
	// instead of degrading to "unauthenticated".
	for _, other := range identityHeaders() {
		if strings.EqualFold(other, g.identityHeader) {
			continue
		}
		if value, _ := headerValues(r, other); value != "" {
			return "", &Denied{Status: http.StatusForbidden, Code: "competing_identity_header",
				Message: "the request carried an identity header this deployment does not accept"}
		}
	}

	if present == "" {
		return "", &Denied{Status: http.StatusUnauthorized, Code: "unauthenticated",
			Message: "the access proxy did not supply an identity for this request"}
	}
	return present, nil
}

// identityHeaders lists every header a supported identity proxy sets.
func identityHeaders() []string {
	headers := make([]string, 0, len(identityProviders))
	for _, header := range identityProviders {
		headers = append(headers, header)
	}
	sort.Strings(headers)
	return headers
}

// headerValues returns the single trimmed value of a header and whether the
// request carried exactly one. http.Header.Get returns only the first value, so
// a repeated or comma-joined header would otherwise be silently truncated to
// whichever the attacker put first.
func headerValues(r *http.Request, header string) (string, bool) {
	values := r.Header.Values(header)
	if len(values) == 0 {
		return "", true
	}
	if len(values) > 1 {
		return "", false
	}
	value := strings.TrimSpace(values[0])
	if strings.Contains(value, ",") {
		return "", false
	}
	return value, true
}

// validHeaderName reports whether name is a legal HTTP header field name
// (RFC 9110 token). net/http does not reject a bad name at request time, it
// simply never matches, so a typo would otherwise present as every request
// being unauthenticated with no hint of the cause.
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isTokenChar(name[i]) {
			return false
		}
	}
	return true
}

func isTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
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
	// A challenge belongs on a 401, and only for a scheme this server actually
	// accepts. Sending `WWW-Authenticate: Bearer` on a 403 told a client to retry
	// with a bearer token that would not have helped: proxy mode does not read one,
	// and the other 403s are refusals of a request the client was already
	// authenticated for (A-10 of the 2026-10-09 review).
	if status == http.StatusUnauthorized && g.mode == ModeToken {
		w.Header().Set("WWW-Authenticate", `Bearer realm="astraeus"`)
	}
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
