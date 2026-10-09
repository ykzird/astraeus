package api

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

// HostAllowlist refuses a request whose Host header names a host this
// deployment does not answer for.
//
// The API is unauthenticated in `none` mode, and its data is readable from any
// page that can reach it. Without a Host check, a public DNS name that resolves
// to the server lets a page on that name read the API with the visitor's
// network position, which is DNS rebinding: the attacker does not need to reach
// the server directly, only to make the victim's browser address it by a name
// the browser treats as same-origin.
//
// allowed holds lowercase hostnames without ports. A request with no Host, or
// with one that is not listed, is answered 421 Misdirected Request rather than
// being served. HTTP/1.0 requests normally carry no Host at all; they are
// refused too, because the front end and every supported client send one.
func HostAllowlist(allowed []string, next http.Handler) http.Handler {
	set := make(map[string]bool, len(allowed))
	for _, host := range allowed {
		if host = strings.ToLower(strings.TrimSpace(host)); host != "" {
			set[host] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := requestHost(r)
		if host == "" || !set[host] {
			writeError(w, http.StatusMisdirectedRequest, "host_not_allowed",
				"this server does not answer for that host name")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requestHost reduces a Host header, or an absolute request URI, to a lowercase
// hostname with no port and no IPv6 brackets. A trailing dot is dropped: FQDN
// spelling is the same name to DNS and must not become a way past the check.
func requestHost(r *http.Request) string {
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}

	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		// An IPv6 literal with no port keeps its brackets until here.
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// DefaultAllowedHosts is what the server answers for when the operator has not
// named any hosts: loopback, under the names a browser or a local client
// actually uses. It is derived from the listen address so that a deliberate
// non-loopback bind is usable without a second flag, and a wildcard bind - the
// one shape that accepts traffic from anywhere - is not silently trusted.
func DefaultAllowedHosts(addr string) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}

	host, _, err := net.SplitHostPort(strings.TrimSpace(addr))
	if err != nil {
		return hosts
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		// A wildcard bind means "every interface", which is every name that
		// resolves here, so deriving names from it would allow them all. The
		// operator names the hosts instead.
		return hosts
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return hosts
	}
	for _, existing := range hosts {
		if strings.EqualFold(existing, host) {
			return hosts
		}
	}
	return append(hosts, strings.ToLower(host))
}

// ParseAllowedHosts validates a comma-separated host list, so a typo is a
// startup error rather than a server that answers nobody.
func ParseAllowedHosts(values []string) ([]string, error) {
	var hosts []string
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			host := part
			if strings.Contains(host, "://") {
				return nil, fmt.Errorf("api: %q looks like a URL; give a host name such as media.example.com", part)
			}
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			host = strings.Trim(strings.TrimSuffix(host, "."), "[]")
			if host == "" {
				return nil, fmt.Errorf("api: %q is not a host name", part)
			}
			hosts = append(hosts, strings.ToLower(host))
		}
	}
	return hosts, nil
}

// LocalHostname is this machine's own name, included so that reaching the
// server by the host's name keeps working when the operator has not listed it.
// It returns "" when the name cannot be determined, which is not an error: the
// loopback names are always allowed anyway.
func LocalHostname() string {
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
}

// WithLocalHostname returns the allowed hosts plus this machine's own name,
// without duplicates.
func WithLocalHostname(hosts []string) []string {
	name := LocalHostname()
	if name == "" {
		return hosts
	}
	for _, existing := range hosts {
		if strings.EqualFold(existing, name) {
			return hosts
		}
	}
	return append(hosts, name)
}
