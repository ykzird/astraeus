package access

import (
	"os"
	"strings"
	"testing"
)

// parse is the test shorthand: a policy from a string, or a failed test.
func parse(t *testing.T, text string) *Policy {
	t.Helper()

	policy, err := ParsePolicy(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parsing policy: %v\n---\n%s", err, text)
	}
	return policy
}

// A nil policy is the unconfigured install: everything visible, everyone an
// admin. This is what keeps the feature invisible until somebody writes a file.
func TestPolicy_NilAllowsEverything(t *testing.T) {
	t.Parallel()

	var policy *Policy
	if !policy.AllowsLibrary("anyone@example.com", "lib-1", "Movies") {
		t.Error("a nil policy denied a library; it must permit everything")
	}
	if !policy.IsAdmin("anyone@example.com") {
		t.Error("a nil policy denied admin; it must permit everything")
	}
	if policy.Source() != "" || policy.Viewers() != 0 || policy.Admins() != 0 {
		t.Errorf("a nil policy reported configuration: source=%q viewers=%d admins=%d",
			policy.Source(), policy.Viewers(), policy.Admins())
	}
}

func TestPolicy_GrantsLibrariesByNameAndID(t *testing.T) {
	t.Parallel()

	policy := parse(t, `
# alice sees one library by name and one by id; bob sees everything.
alice@example.com: Movies, 7f3a-2b1c
bob@example.com: *
`)

	tests := []struct {
		name    string
		viewer  string
		id      string
		library string
		want    bool
	}{
		{name: "by name", viewer: "alice@example.com", id: "lib-1", library: "Movies", want: true},
		{name: "by name, different case", viewer: "alice@example.com", id: "lib-1", library: "movies", want: true},
		{name: "by name, upper case grant", viewer: "ALICE@example.com", id: "lib-1", library: "MOVIES", want: true},
		{name: "by id", viewer: "alice@example.com", id: "7f3a-2b1c", library: "Anything", want: true},
		{name: "another library by neither", viewer: "alice@example.com", id: "lib-2", library: "Documentaries", want: false},
		{name: "wildcard sees any", viewer: "bob@example.com", id: "lib-9", library: "Later", want: true},
		{name: "an id is matched exactly", viewer: "alice@example.com", id: "7F3A-2B1C", library: "Anything", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := policy.AllowsLibrary(tt.viewer, tt.id, tt.library); got != tt.want {
				t.Errorf("AllowsLibrary(%q, %q, %q) = %v, want %v", tt.viewer, tt.id, tt.library, got, tt.want)
			}
		})
	}
}

// A policy in use denies by default, so an identity the file forgets sees
// nothing rather than everything.
func TestPolicy_UnlistedViewerIsDeniedByDefault(t *testing.T) {
	t.Parallel()

	policy := parse(t, "alice@example.com: Movies\n")
	if policy.DefaultAll() {
		t.Error("DefaultAll() = true, want false when the file does not say otherwise")
	}
	if policy.AllowsLibrary("stranger@example.com", "lib-1", "Movies") {
		t.Error("an unlisted viewer was granted a library")
	}
	if policy.IsAdmin("stranger@example.com") {
		t.Error("an unlisted viewer was made an admin")
	}
}

func TestPolicy_DefaultAll(t *testing.T) {
	t.Parallel()

	policy := parse(t, "default: all\nalice@example.com: Movies\n")
	if !policy.DefaultAll() {
		t.Error("DefaultAll() = false, want true")
	}
	if !policy.AllowsLibrary("stranger@example.com", "lib-1", "Documentaries") {
		t.Error("an unlisted viewer was denied with default: all")
	}
	// Seeing everything is not the same as being able to change everything.
	if policy.IsAdmin("stranger@example.com") {
		t.Error("default: all also granted admin")
	}
	if policy.IsAdmin("alice@example.com") {
		t.Error("a granted library made alice an admin")
	}
}

// Being an admin is about changing the library, not about seeing it. The two
// are separate grants so that neither is given away by accident.
func TestPolicy_AdminDoesNotImplyVisibility(t *testing.T) {
	t.Parallel()

	policy := parse(t, "admin: jok@example.com\njok@example.com: Kids\nother@example.com: Movies\n")

	if !policy.IsAdmin("jok@example.com") {
		t.Error("jok@example.com is not an admin")
	}
	if policy.IsAdmin("other@example.com") {
		t.Error("someone not listed as an admin was one")
	}
	if policy.AllowsLibrary("jok@example.com", "lib-2", "Movies") {
		t.Error("an admin was granted a library it was not given")
	}
	if !policy.AllowsLibrary("jok@example.com", "lib-1", "Kids") {
		t.Error("the admin's own grant was not honoured")
	}
}

// A library named before it exists is not an error: it matches nothing until
// the library is added, which is what makes a policy safe to write ahead of a
// scan.
func TestPolicy_UnknownLibraryNameIsNotAnError(t *testing.T) {
	t.Parallel()

	policy := parse(t, "alice@example.com: Not Yet A Library\n")
	if policy.AllowsLibrary("alice@example.com", "lib-1", "Movies") {
		t.Error("an unknown name matched a real library")
	}
	if policy.Viewers() != 1 {
		t.Errorf("Viewers() = %d, want 1", policy.Viewers())
	}
}

// A viewer listed with no libraries is denied everything but stays known,
// which is how access is revoked without forgetting who somebody is.
func TestPolicy_ListedWithNoLibrariesSeesNothing(t *testing.T) {
	t.Parallel()

	policy := parse(t, "default: all\nalice@example.com:\n")
	if policy.AllowsLibrary("alice@example.com", "lib-1", "Movies") {
		t.Error("a viewer listed with no libraries was granted one")
	}
}

// An identity may be granted twice; the grants combine rather than replace.
func TestPolicy_RepeatedIdentityCombinesGrants(t *testing.T) {
	t.Parallel()

	policy := parse(t, "alice@example.com: Movies\nalice@example.com: Documentaries\n")
	if !policy.AllowsLibrary("alice@example.com", "a", "Movies") {
		t.Error("the first grant was lost")
	}
	if !policy.AllowsLibrary("alice@example.com", "b", "Documentaries") {
		t.Error("the second grant was lost")
	}
}

func TestPolicy_ParseErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "no colon", text: "alice@example.com Movies\n", want: "neither a directive nor an identity grant"},
		{name: "empty key", text: ": Movies\n", want: "no identity or directive"},
		{name: "bad default", text: "default: everyone\n", want: "default must be none or all"},
		{name: "default twice", text: "default: none\ndefault: all\n", want: "default is set more than once"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParsePolicy(strings.NewReader(tt.text))
			if err == nil {
				t.Fatalf("ParsePolicy(%q) succeeded, want an error", tt.text)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), "line 1") && tt.name != "default twice" {
				t.Errorf("error = %q, want it to name the line", err)
			}
		})
	}
}

// The source is carried so the startup line can name the file that took
// effect, which is how an operator confirms a typo in a path did not silently
// leave the install open.
func TestLoadPolicy_ReportsSourceAndRejectsMissingFile(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/policy.conf"
	if err := os.WriteFile(path, []byte("alice@example.com: Movies\n"), 0o600); err != nil {
		t.Fatalf("writing policy: %v", err)
	}

	policy, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("loading policy: %v", err)
	}
	if policy.Source() != path {
		t.Errorf("Source() = %q, want %q", policy.Source(), path)
	}

	if _, err := LoadPolicy(path + ".missing"); err == nil {
		t.Error("loading a missing policy succeeded, want an error")
	}
}
