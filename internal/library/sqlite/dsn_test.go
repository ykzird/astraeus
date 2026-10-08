package sqlite

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

func TestDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		path     string
		contains []string
		equals   string
	}{
		{
			name:     "a plain path becomes a file URI with pragmas",
			path:     "/var/lib/astraeus/astraeus.db",
			contains: []string{"file:/var/lib/astraeus/astraeus.db?", "_pragma=journal_mode(WAL)", "_pragma=busy_timeout(5000)", "_pragma=foreign_keys(1)"},
		},
		{
			name:     "a relative path is preserved",
			path:     "demo.db",
			contains: []string{"file:demo.db?", "_pragma=journal_mode(WAL)"},
		},
		{
			name:   "in-memory databases are passed through",
			path:   ":memory:",
			equals: ":memory:",
		},
		{
			name:     "a query in the path does not break the pragmas",
			path:     "/tmp/odd?name.db",
			contains: []string{"%3f", "_pragma=journal_mode(WAL)"},
		},
		{
			name:     "an existing file URI gets the pragmas appended",
			path:     "file:existing.db",
			contains: []string{"file:existing.db?", "_pragma=journal_mode(WAL)"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := DSN(tt.path)
			if tt.equals != "" {
				if got != tt.equals {
					t.Errorf("DSN(%q) = %q, want %q", tt.path, got, tt.equals)
				}
				return
			}
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("DSN(%q) = %q, missing %q", tt.path, got, want)
				}
			}
		})
	}
}

// TestDSN_AppliesPragmasToEveryConnection is the regression test for the
// bug this fixes: the pragmas must cover pooled connections, not just the first
// one, which is why they ride in the DSN rather than being run once.
func TestDSN_AppliesPragmasToEveryConnection(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "pragmas.db")
	db, err := sqlx.Connect("sqlite", DSN(path))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	defer db.Close()

	// Force more than one connection so the pool cannot hide a missing pragma.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(0)

	if _, err := db.Exec(`CREATE TABLE t (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("creating table: %v", err)
	}

	// Each of these may land on a different pooled connection.
	for i := 0; i < 8; i++ {
		var journal string
		if err := db.Get(&journal, "PRAGMA journal_mode"); err != nil {
			t.Fatalf("reading journal_mode: %v", err)
		}
		if strings.ToLower(journal) != "wal" {
			t.Fatalf("connection %d reports journal_mode=%q, want wal", i, journal)
		}

		var busy int
		if err := db.Get(&busy, "PRAGMA busy_timeout"); err != nil {
			t.Fatalf("reading busy_timeout: %v", err)
		}
		if busy != 5000 {
			t.Fatalf("connection %d reports busy_timeout=%d, want 5000", i, busy)
		}

		var foreignKeys int
		if err := db.Get(&foreignKeys, "PRAGMA foreign_keys"); err != nil {
			t.Fatalf("reading foreign_keys: %v", err)
		}
		if foreignKeys != 1 {
			t.Fatalf("connection %d reports foreign_keys=%d, want 1", i, foreignKeys)
		}
	}
}
