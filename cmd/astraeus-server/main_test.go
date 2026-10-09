package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// These are the first tests for cmd/astraeus-server, which the 2026-10-09
// review recorded at 0% coverage. They cover the flag handling that carries
// secrets and the identity-header resolution, because those are the two places
// where a mistake is a security problem rather than a bug.

func TestSplitList(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  []string
	}{
		{name: "empty", value: "", want: nil},
		{name: "one", value: "a", want: []string{"a"}},
		{name: "many", value: "a, b ,c", want: []string{"a", "b", "c"}},
		{name: "only separators", value: " , , ", want: nil},
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
					t.Fatalf("splitList(%q) = %v, want %v", tt.value, got, tt.want)
				}
			}
		})
	}
}

func TestResolveIdentityHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		header   string
		provider string
		want     string
		wantErr  bool
	}{
		{name: "no choice is an empty header, which the gate rejects", provider: "", header: ""},
		{name: "a known provider", provider: "tailscale", want: "Tailscale-User-Login"},
		{name: "a known provider in another case", provider: "CloudFlare", want: "Cf-Access-Authenticated-User-Email"},
		{name: "a custom header", header: "X-Astraeus-User", want: "X-Astraeus-User"},
		{name: "an unknown provider", provider: "nginx", wantErr: true},
		{name: "both spellings at once", header: "X-Astraeus-User", provider: "tailscale", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := resolveIdentityHeader(tt.header, tt.provider)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("resolveIdentityHeader(%q, %q) should fail", tt.header, tt.provider)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveIdentityHeader(%q, %q): %v", tt.header, tt.provider, err)
			}
			if got != tt.want {
				t.Errorf("resolveIdentityHeader(%q, %q) = %q, want %q", tt.header, tt.provider, got, tt.want)
			}
		})
	}
}

func TestApplyEnv_FlagWins(t *testing.T) {
	t.Setenv("TMDB_API_KEY", "from-the-environment")

	explicit := &config{tmdbKey: "explicit"}
	explicit.applyEnv()
	if explicit.tmdbKey != "explicit" {
		t.Errorf("an explicit --tmdb-key should win, got %q", explicit.tmdbKey)
	}

	inherited := &config{}
	inherited.applyEnv()
	if inherited.tmdbKey != "from-the-environment" {
		t.Errorf("the environment should fill an empty --tmdb-key, got %q", inherited.tmdbKey)
	}
}

// TestConfigRegister_DoesNotPrintSecrets is the regression test for A-7.
//
// The flag defaults used to come from os.Getenv, so `serve -h` and - worse -
// the usage block flag.ExitOnError prints on any parse error both contained the
// bearer token and the TMDB key. A typo in a systemd ExecStart therefore wrote
// both secrets into the journal.
func TestConfigRegister_DoesNotPrintSecrets(t *testing.T) {
	const token, key = "supersecret-token", "supersecret-key"
	t.Setenv("ASTRAEUS_AUTH_TOKEN", token)
	t.Setenv("TMDB_API_KEY", key)

	// The values are present in the environment, so a test that passes without
	// reading them is not proving anything.
	if os.Getenv("ASTRAEUS_AUTH_TOKEN") != token || os.Getenv("TMDB_API_KEY") != key {
		t.Fatal("the environment did not take the test values")
	}

	var cfg config
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg.register(fs)

	// The secret-bearing flags every serve flag would add. Only the
	// registered defaults matter here, so the rest are not needed.
	out := &strings.Builder{}
	fs.SetOutput(out)
	fs.PrintDefaults()

	if strings.Contains(out.String(), token) {
		t.Errorf("the help output contains the bearer token:\n%s", out.String())
	}
	if strings.Contains(out.String(), key) {
		t.Errorf("the help output contains the TMDB key:\n%s", out.String())
	}
}

// TestBinaryHelpDoesNotLeakSecrets runs the real binary, because the leak was
// in flag.ExitOnError's own path (a parse error) rather than in anything the
// code prints itself.
func TestBinaryHelpDoesNotLeakSecrets(t *testing.T) {
	t.Parallel()

	const token, key = "supersecret-token", "supersecret-key"
	binary := buildBinary(t)

	for _, args := range [][]string{
		{"serve", "-h"},
		{"serve", "--not-a-flag"},
	} {
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "ASTRAEUS_AUTH_TOKEN="+token, "TMDB_API_KEY="+key)
		output, _ := cmd.CombinedOutput()

		if strings.Contains(string(output), token) {
			t.Errorf("%v printed the bearer token:\n%s", args, output)
		}
		if strings.Contains(string(output), key) {
			t.Errorf("%v printed the TMDB key:\n%s", args, output)
		}
	}
}

// TestDefaultAddrIsLoopback is the regression test for A-3, at the flag layer.
func TestDefaultAddrIsLoopback(t *testing.T) {
	t.Parallel()

	binary := buildBinary(t)
	cmd := exec.Command(binary, "serve", "-h")
	output, err := cmd.CombinedOutput()
	if err != nil {
		// -h exits non-zero under flag.ExitOnError; the output is what matters.
		t.Logf("serve -h exited with %v", err)
	}
	if !strings.Contains(string(output), `default "127.0.0.1:8642"`) {
		t.Errorf("serve -h does not show a loopback default address:\n%s", output)
	}
}

var (
	buildOnce   sync.Once
	builtBinary string
	buildErr    error
)

// buildBinary compiles the server once per test run and returns its path. The
// tests that use it run in parallel, so the build has to happen under a
// sync.Once rather than behind a plain "already built?" check.
func buildBinary(t *testing.T) string {
	t.Helper()

	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "astraeus-server-test")
		if err != nil {
			buildErr = fmt.Errorf("creating a build directory: %w", err)
			return
		}
		binary := filepath.Join(dir, "astraeus-server")
		cmd := exec.Command("go", "build", "-o", binary, ".")
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			buildErr = fmt.Errorf("building the server binary: %w", err)
			return
		}
		builtBinary = binary
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return builtBinary
}

// TestServeFlagsAreDocumented is the regression test for D-4.
//
// The flag table in docs/configuration.md was maintained by hand beside the code
// that declares the flags, and it drifted: two intervals were shown as having no
// default when both are 6h, three flags were missing entirely, and two documents
// claimed the table covered "every flag". The review's recommendation was to
// generate it from the binary; checking it against the binary is the part that
// catches drift, and it does not need a generator to write Markdown nobody reads.
//
// This test is deliberately one-directional: every flag the binary offers must
// appear in the table. A table entry for something that is not a flag would be a
// different mistake, and the command's own -h output is the authority on what
// exists.
func TestServeFlagsAreDocumented(t *testing.T) {
	t.Parallel()

	binary := buildBinary(t)

	// The flags come from the binary, not from a list here, because a list here
	// would be the same hand-maintained duplicate the test exists to catch.
	help := exec.Command(binary, "serve", "-h")
	output, _ := help.CombinedOutput()

	flagLine := regexp.MustCompile(`(?m)^\s+-([a-z][a-z0-9-]*)`)
	offered := map[string]bool{}
	for _, match := range flagLine.FindAllStringSubmatch(string(output), -1) {
		offered[match[1]] = true
	}
	if len(offered) < 20 {
		t.Fatalf("only %d flags were parsed from the help output, so the parsing is wrong "+
			"rather than the documentation:\n%s", len(offered), output)
	}

	// Any backticked --flag counts as documented, because the reference groups
	// several flags into one row ("`--auth-mode`, `--auth-header`, ...") and a
	// pattern that only matched the first flag of a row reported the rest as
	// missing. The file is named rather than located by line, so the test does
	// not depend on where the table sits.
	docs, err := os.ReadFile(filepath.Join("..", "..", "docs", "configuration.md"))
	if err != nil {
		t.Fatalf("reading docs/configuration.md: %v", err)
	}
	anywhere := regexp.MustCompile("`--([a-z][a-z0-9-]*)`")
	documented := map[string]bool{}
	for _, match := range anywhere.FindAllStringSubmatch(string(docs), -1) {
		documented[match[1]] = true
	}

	var missing []string
	for flag := range offered {
		if !documented[flag] {
			missing = append(missing, "--"+flag)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these flags exist but are not in the table in docs/configuration.md, so "+
			"the reference does not cover the binary:\n  %s", strings.Join(missing, "\n  "))
	}
}

// TestShippedUnitDoesNotUseTokenMode is the regression test for DD-13.
//
// The systemd unit shipped with `--auth-mode token`, and deploy/README.md
// presented that install as the finished one. No browser can satisfy token mode:
// the gate wants an `Authorization: Bearer` header, a navigation cannot send one,
// and web/app.js has no way to set one. So the documented install served a UI
// whose every request was refused with 401 - the runbook described a working
// server and delivered a broken page.
//
// The mode is a string in a unit file, which is exactly the kind of thing that
// gets edited back. This reads the unit that ships and the runbook that installs
// it, and refuses token mode in the unit and a token step in the runbook.
func TestShippedUnitDoesNotUseTokenMode(t *testing.T) {
	t.Parallel()

	unit, err := os.ReadFile(filepath.Join("..", "..", "deploy", "astraeus.service"))
	if err != nil {
		t.Fatalf("reading the shipped unit: %v", err)
	}

	// A commented mention is fine - the unit explains why it does not use token
	// mode. What must not appear is the flag in the ExecStart command.
	var execStart strings.Builder
	for _, line := range strings.Split(string(unit), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		execStart.WriteString(trimmed)
		execStart.WriteString(" ")
	}
	if strings.Contains(execStart.String(), "--auth-mode token") {
		t.Error("the shipped unit runs with --auth-mode token, which leaves the web UI " +
			"unusable: a browser cannot send a bearer token, so every request is refused " +
			"with 401. Use proxy mode behind a TLS terminator, or leave the default")
	}

	// And the runbook must not tell an operator to create the token the unit
	// would read, because that is the step that made the install look finished.
	runbook, err := os.ReadFile(filepath.Join("..", "..", "deploy", "README.md"))
	if err != nil {
		t.Fatalf("reading the deployment runbook: %v", err)
	}
	for _, line := range strings.Split(string(runbook), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ">") {
			continue
		}
		// Any step that writes the token into the environment file, however it is
		// spelled - the runbook used a printf into tee, and could use install or
		// an editor next.
		writesToken := strings.Contains(trimmed, "ASTRAEUS_AUTH_TOKEN") &&
			(strings.Contains(trimmed, "astraeus.env") || strings.Contains(trimmed, "tee"))
		if writesToken {
			t.Error("the deployment runbook tells an operator to write ASTRAEUS_AUTH_TOKEN, " +
				"which is the token step that paired with the unit's token mode. The unit " +
				"does not read it, and creating it does not secure anything on its own")
		}
	}
}

// TestBackupDocumentationNamesRealPaths ties the backup section of the deployment
// runbook to the paths the shipped unit actually uses.
//
// The backup section is the one page where a wrong path is worse than missing
// documentation: it tells an operator what to copy, and a path that does not
// match the unit produces a backup of something else, or of nothing, without
// reporting an error. The unit is the authority - it is what runs - so the paths
// it names are the ones the documentation has to name.
func TestBackupDocumentationNamesRealPaths(t *testing.T) {
	t.Parallel()

	unit, err := os.ReadFile(filepath.Join("..", "..", "deploy", "astraeus.service"))
	if err != nil {
		t.Fatalf("reading the shipped unit: %v", err)
	}
	runbook, err := os.ReadFile(filepath.Join("..", "..", "deploy", "README.md"))
	if err != nil {
		t.Fatalf("reading the deployment runbook: %v", err)
	}

	// The database path the unit passes to --db.
	unitText := string(unit)
	match := regexp.MustCompile(`--db\s+(\S+)`).FindStringSubmatch(unitText)
	if match == nil {
		t.Fatal("the unit does not pass --db, so this test cannot check the path")
	}
	database := match[1]

	// The check is scoped to the backup section, not the whole runbook: the
	// database path appears in several places, so a whole-file search passed even
	// with the backup table pointing at a directory that does not exist. What
	// matters is that the section telling an operator what to copy names the real
	// path.
	const heading = "## Backups and upgrades"
	start := strings.Index(string(runbook), heading)
	if start < 0 {
		t.Fatalf("the runbook has no %q section", heading)
	}
	// To the next top-level heading, which is where the section ends.
	section := string(runbook[start:])
	if next := strings.Index(section[len(heading):], "\n## "); next >= 0 {
		section = section[:len(heading)+next]
	}

	// The state table is the part that says *what* to copy, so its database row
	// is what has to agree with the unit. Checking the section as a whole passed
	// with a corrupted table, because the real path also appears in the restore
	// example below it.
	stateRow := regexp.MustCompile("(?m)^\\|\\s*The database\\s*\\|\\s*`([^`]+)`")
	row := stateRow.FindStringSubmatch(section)
	if row == nil {
		t.Fatal(`the backup section has no "The database" row, so this test cannot ` +
			"check what it tells an operator to copy")
	}
	if row[1] != database {
		t.Errorf("the backup section says the database is %s and the unit uses %s, so "+
			"an operator following it copies the wrong file", row[1], database)
	}

	// The two files the runbook calls state have to be the ones the unit reads.
	for _, path := range []string{"/etc/astraeus/access-policy.conf", "/etc/astraeus/astraeus.env"} {
		if !strings.Contains(section, path) {
			t.Errorf("the backup section does not mention %s, which is state", path)
		}
	}
	// The unit reads the environment file, so the runbook's claim that it is state
	// is checkable rather than decorative.
	if !strings.Contains(unitText, "astraeus.env") {
		t.Error("the unit does not read astraeus.env, so the runbook should not call it state")
	}
	// And it must say sqlite3 is required, because it is not a project dependency.
	if !strings.Contains(section, "sqlite3") {
		t.Error("the backup section does not mention sqlite3, which its recipe needs")
	}
}
