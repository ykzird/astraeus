package access

import (
	"os"
	"path/filepath"
	"testing"
)

// writePolicy writes a policy file and returns its path.
func writePolicy(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "access-policy.conf")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing the policy: %v", err)
	}
	return path
}

// TestLoadPolicy_AcceptsTheDocumentedExample is the regression test for D-1.
//
// This is the example in docs/configuration.md, verbatim, comments and all. It
// did not work. The parser recognised a comment only at the start of a line, so
// every trailing note became part of the value: startup failed outright on
// "default must be none or all, not \"none            # an unlisted viewer sees
// nothing\"", and without the default line the file loaded but granted the wrong
// things - three admins from one admin line, and a library literally named
// "Kids    # a name".
func TestLoadPolicy_AcceptsTheDocumentedExample(t *testing.T) {
	t.Parallel()

	documented := `# /etc/astraeus/access-policy.conf
default: none            # an unlisted viewer sees nothing (the default)
admin: jok@example.com   # may scan, enrich, add and remove libraries

jok@example.com: *       # "*" is every library, including ones added later
alice@example.com: Movies, Documentaries
bob@example.com: Kids    # a name, matched case-insensitively
`
	policy, err := LoadPolicy(writePolicy(t, documented))
	if err != nil {
		t.Fatalf("the documented policy example does not load, so nobody can follow the "+
			"documentation:\n%v", err)
	}

	// Exactly one admin. The old parser split the admin line's comment at its
	// commas and created junk identities, so it reported three.
	if admins := policy.Admins(); admins != 1 {
		t.Errorf("admins = %d, want 1: the comment on the admin line is not a list of "+
			"admins", admins)
	}
	if !policy.IsAdmin("jok@example.com") {
		t.Error("jok@example.com should be an admin")
	}

	// The default is none, not a parse failure and not "all".
	if policy.DefaultAll() {
		t.Error("default: none means an unlisted viewer sees nothing")
	}

	// Bob is granted "Kids" and not "Kids    # a name". Asked with the name the
	// comment would have produced, a correct parser grants nothing.
	if !policy.AllowsLibrary("bob@example.com", "lib-kids", "Kids") {
		t.Error("bob was not granted Kids")
	}
	if policy.AllowsLibrary("bob@example.com", "lib-kids", "Kids    # a name") {
		t.Error("the trailing comment was taken as part of the library's name")
	}
	if policy.AllowsLibrary("bob@example.com", "lib-movies", "Movies") {
		t.Error("bob was granted a library the file does not give him")
	}

	// Alice's comma-separated list is two libraries, not one.
	if !policy.AllowsLibrary("alice@example.com", "lib-movies", "Movies") ||
		!policy.AllowsLibrary("alice@example.com", "lib-docs", "Documentaries") {
		t.Error("alice's comma-separated grant was not split into two libraries")
	}

	// And everyone who is not listed stays unlisted under default: none.
	if policy.AllowsLibrary("stranger@example.com", "lib-kids", "Kids") {
		t.Error("an unlisted viewer was granted something under default: none")
	}
}

// TestLoadPolicy_AcceptsTheDocumentedGrammarExample is the regression test for
// the policy-file grammar gap in 05-docs.md's missing-documentation list.
//
// This is the extended example in docs/configuration.md's "Which libraries a
// viewer may see", verbatim, including the escaped subject. A grammar nobody can
// copy is not documentation, and this one had to grow an escape before the
// identity deploy/tls/ produces could be listed at all - so the example is the
// thing to hold the parser to.
func TestLoadPolicy_AcceptsTheDocumentedGrammarExample(t *testing.T) {
	t.Parallel()

	documented := `# /etc/astraeus/access-policy.conf
default: none            # an unlisted viewer sees nothing (the default)
admin: jok@example.com   # may scan, enrich, add and remove libraries

jok@example.com: *       # "*" is every library, including ones added later
alice@example.com: Movies, Documentaries
bob@example.com: Kids    # a name, matched case-insensitively

# A client-certificate proxy forwards the whole subject, which contains commas.
# Backslash escapes the next character, so the subject stays one identity:
admin: CN=alice\,O=Acme
CN=alice\,O=Acme: Movies
`
	policy, err := LoadPolicy(writePolicy(t, documented))
	if err != nil {
		t.Fatalf("the documented policy example does not load:\n%v", err)
	}

	// One comment line and one real line for the subject under each directive,
	// so two admins and the viewer the proxy names.
	if got := policy.Admins(); got != 2 {
		t.Errorf("admins = %d, want 2 (jok and the certificate subject)", got)
	}
	if !policy.IsAdmin("CN=alice,O=Acme") {
		t.Error("the escaped subject is not an admin")
	}
	if policy.IsAdmin("CN=alice") || policy.IsAdmin("O=Acme") {
		t.Error("the subject was split at its comma, so its halves became identities")
	}
	if !policy.AllowsLibrary("CN=alice,O=Acme", "lib-movies", "Movies") {
		t.Error("the escaped subject was not granted Movies")
	}
	// And the example's other grants still work.
	if !policy.AllowsLibrary("alice@example.com", "lib-docs", "Documentaries") {
		t.Error("alice was not granted Documentaries")
	}
	if policy.AllowsLibrary("bob@example.com", "lib-docs", "Documentaries") {
		t.Error("bob was granted a library the example does not give him")
	}
}

// TestStripPolicyComment pins the rule, including the case it must not break.
func TestStripPolicyComment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "a whole-line comment", line: "# nothing here", want: ""},
		{name: "a note after a directive", line: "default: none  # the default", want: "default: none"},
		{name: "a note after a grant", line: "bob: Kids    # a name", want: "bob: Kids"},
		{name: "a tab before the hash", line: "default: all\t# everything", want: "default: all"},
		{name: "no comment at all", line: "alice: Movies, Documentaries", want: "alice: Movies, Documentaries"},
		{
			// The case the rule has to protect: a hash inside a value with no
			// whitespace before it is part of the value, not a comment.
			name: "a hash inside a value",
			line: "alice: Movies#2",
			want: "alice: Movies#2",
		},
		{name: "an empty line stays empty", line: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := stripPolicyComment(tt.line); got != tt.want {
				t.Errorf("stripPolicyComment(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

// TestLoadPolicy_CommentOnlyAndInlineCommentsTogether covers a file that mixes
// the shapes, which is what a real one looks like after somebody edits it.
func TestLoadPolicy_CommentOnlyAndInlineCommentsTogether(t *testing.T) {
	t.Parallel()

	policy, err := LoadPolicy(writePolicy(t, `
# The operators.
admin: ops@example.com

# The family.
default: all             # everyone else gets everything
alice@example.com: Movies
`))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if !policy.IsAdmin("ops@example.com") {
		t.Error("ops@example.com should be an admin")
	}
	if !policy.DefaultAll() {
		t.Error("default: all was not read")
	}
	if !policy.AllowsLibrary("alice@example.com", "lib-movies", "Movies") {
		t.Error("alice should still be granted Movies")
	}
	if policy.AllowsLibrary("alice@example.com", "lib-kids", "Kids") {
		t.Error("alice was granted a library the file does not give her")
	}
}

// TestLoadPolicy_EscapesACommaInAnIdentity is the regression test for the
// grammar gap in 05-docs.md's missing-documentation list.
//
// Values are comma-separated, so an identity containing a comma could not be
// listed at all. That is exactly the identity a client-certificate proxy
// forwards: Caddy's `{http.request.tls.client.subject}` is the whole subject,
// "CN=alice,O=Acme", and deploy/tls/README.md tells an operator to use it. The
// line was split into "CN=alice" and "O=Acme", neither of which matched the
// viewer the proxy actually names - so the install had an admin who was not one.
func TestLoadPolicy_EscapesACommaInAnIdentity(t *testing.T) {
	t.Parallel()

	policy, err := LoadPolicy(writePolicy(t, `
default: none
admin: CN=alice\,O=Acme
CN=alice\,O=Acme: *
`))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}

	// One admin, named by the whole subject.
	if admins := policy.Admins(); admins != 1 {
		t.Errorf("admins = %d, want 1: the escaped subject is one identity, not two", admins)
	}
	if !policy.IsAdmin("CN=alice,O=Acme") {
		t.Error("the escaped subject is not an admin, so the escape was not applied")
	}
	// And the fragments the split used to produce are not admins.
	if policy.IsAdmin("CN=alice") || policy.IsAdmin("O=Acme") {
		t.Error("the subject was split at its comma, so its halves became identities")
	}

	// The grant on the same subject works too.
	if !policy.AllowsLibrary("CN=alice,O=Acme", "lib-1", "Movies") {
		t.Error("the escaped subject was not granted a library")
	}
}

// TestSplitList pins the grammar, including what must keep working.
func TestSplitList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "one item", value: "Movies", want: []string{"Movies"}},
		{name: "two items", value: "Movies, Documentaries", want: []string{"Movies", "Documentaries"}},
		{name: "empty entries dropped", value: "Movies,,", want: []string{"Movies"}},
		{name: "whitespace trimmed", value: "  Movies  ,  Kids  ", want: []string{"Movies", "Kids"}},
		{
			name:  "an escaped comma is one item",
			value: `CN=alice\,O=Acme`,
			want:  []string{"CN=alice,O=Acme"},
		},
		{
			name:  "escaped and unescaped beside each other",
			value: `CN=alice\,O=Acme, bob@example.com`,
			want:  []string{"CN=alice,O=Acme", "bob@example.com"},
		},
		{
			name:  "an escaped backslash is literal",
			value: `a\\b, c`,
			want:  []string{`a\b`, "c"},
		},
		{name: "a trailing backslash is kept", value: `Movies\`, want: []string{`Movies\`}},
		{name: "empty", value: "", want: []string{}},
		{name: "only separators", value: " , , ", want: []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := splitList(tt.value)
			if len(got) != len(tt.want) {
				t.Fatalf("splitList(%q) = %v, want %v", tt.value, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitList(%q)[%d] = %q, want %q", tt.value, i, got[i], tt.want[i])
				}
			}
		})
	}
}
