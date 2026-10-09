package access

// Access policy: which libraries a viewer may see, and who may change the
// library at all.
//
// This is deliberately a file rather than a table or an API. Authorization that
// a client can grant itself is not authorization, so the policy is something an
// operator writes on the host and the server reads at startup — the same shape
// as the gate's own configuration. The model is small enough that a table could
// back it later; nothing outside this file assumes where the grants come from.
//
// A nil Policy allows everything, which is what an install has always done and
// is what keeps this change invisible until somebody configures it.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Policy is the operator's answer to "what may this viewer see, and who may
// change things".
//
// The zero value is not usable; load one with LoadPolicy, or leave the pointer
// nil to permit everything.
type Policy struct {
	// viewers maps a lower-cased identity to its grants. Identities come from
	// the access gate, and are compared case-insensitively because they are
	// usually e-mail addresses and nobody means Alice@ and alice@ to differ.
	viewers map[string]grant
	// unlisted is what a viewer the file does not mention may see.
	unlisted grant
	// admins are the identities allowed to change the library: scan, enrich,
	// add and remove libraries. Being an admin says nothing about visibility,
	// which is a separate grant — see the file format below.
	admins map[string]bool
	// source is where the policy was read from, for the startup log line.
	source string
}

// grant is what one viewer (or the default) may see.
type grant struct {
	// all is a literal "*": every library, including ones added later.
	all bool
	// libraries are names or ids as written in the file. A name that does not
	// exist yet is not an error and simply matches nothing, so a policy can be
	// written before the library it refers to is added.
	libraries []string
}

// LoadPolicy reads a policy file.
//
// It returns an error rather than a partial policy: a file that cannot be
// understood is a misconfiguration, and starting with a policy that is not the
// one written is how an operator ends up explaining an outage.
func LoadPolicy(path string) (*Policy, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("access policy: %w", err)
	}
	defer file.Close()

	policy, err := ParsePolicy(file)
	if err != nil {
		return nil, fmt.Errorf("access policy %s: %w", path, err)
	}
	policy.source = path
	return policy, nil
}

// ParsePolicy reads a policy from a reader. The format is one directive or
// grant per line:
//
//	# comments start with a hash
//	default: none            # or "all"; what an unlisted viewer may see
//	admin: alice@example.com # comma separated; may scan and change libraries
//	alice@example.com: *     # "*" is every library, including future ones
//	bob@example.com: Movies, Documentaries
//
// A grant names a library by its name (case-insensitive) or by its id (exact).
// A viewer that appears twice has its grants combined.
func ParsePolicy(r io.Reader) (*Policy, error) {
	policy := &Policy{
		viewers: make(map[string]grant),
		admins:  make(map[string]bool),
	}
	// A policy in use denies by default: an operator who writes a file is
	// choosing who may see what, and "everyone, unless listed" would make the
	// file a list of exceptions rather than of grants. "default: all" asks for
	// the other arrangement explicitly.
	policy.unlisted = grant{}
	sawDefault := false

	scanner := bufio.NewScanner(r)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, found := strings.Cut(line, ":")
		if !found {
			return nil, fmt.Errorf("line %d: %q is neither a directive nor an identity grant", lineNumber, line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			return nil, fmt.Errorf("line %d: no identity or directive before the colon", lineNumber)
		}

		switch strings.ToLower(key) {
		case "default":
			if sawDefault {
				return nil, fmt.Errorf("line %d: default is set more than once", lineNumber)
			}
			sawDefault = true
			switch strings.ToLower(value) {
			case "none":
				policy.unlisted = grant{}
			case "all", "*":
				policy.unlisted = grant{all: true}
			default:
				return nil, fmt.Errorf("line %d: default must be none or all, not %q", lineNumber, value)
			}
		case "admin":
			for _, identity := range splitList(value) {
				policy.admins[strings.ToLower(identity)] = true
			}
		default:
			existing := policy.viewers[strings.ToLower(key)]
			for _, library := range splitList(value) {
				if library == "*" {
					existing.all = true
					continue
				}
				existing.libraries = append(existing.libraries, library)
			}
			policy.viewers[strings.ToLower(key)] = existing
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading policy: %w", err)
	}

	return policy, nil
}

// splitList splits a comma-separated value, dropping empty entries so that
// "Movies," and "Movies, Documentaries" mean the same thing.
func splitList(value string) []string {
	out := make([]string, 0, 2)
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// AllowsLibrary reports whether viewer may see the library with this id and
// name. A nil policy allows everything.
func (p *Policy) AllowsLibrary(viewer, libraryID, libraryName string) bool {
	if p == nil {
		return true
	}

	granted, listed := p.viewers[strings.ToLower(viewer)]
	if !listed {
		granted = p.unlisted
	}
	if granted.all {
		return true
	}

	for _, allowed := range granted.libraries {
		// An id is matched exactly because it is opaque and machine-written; a
		// name is matched case-insensitively because a person typed it.
		if allowed == libraryID || strings.EqualFold(allowed, libraryName) {
			return true
		}
	}
	return false
}

// IsAdmin reports whether viewer may change the library. A nil policy makes
// every viewer an admin, which is the behaviour an install has had until now.
func (p *Policy) IsAdmin(viewer string) bool {
	if p == nil {
		return true
	}
	return p.admins[strings.ToLower(viewer)]
}

// Source is where the policy was read from, or "" when there is none.
func (p *Policy) Source() string {
	if p == nil {
		return ""
	}
	return p.source
}

// DefaultAll reports whether an unlisted viewer may see every library.
func (p *Policy) DefaultAll() bool {
	if p == nil {
		return true
	}
	return p.unlisted.all
}

// Viewers is how many identities the policy names, which is what the startup
// line reports so an operator can see the file took effect.
func (p *Policy) Viewers() int {
	if p == nil {
		return 0
	}
	return len(p.viewers)
}

// Admins is how many identities may change the library.
func (p *Policy) Admins() int {
	if p == nil {
		return 0
	}
	return len(p.admins)
}
