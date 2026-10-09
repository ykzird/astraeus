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
