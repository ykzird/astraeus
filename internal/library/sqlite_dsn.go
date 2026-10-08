package library

import "strings"

// sqlitePragmas are the settings every connection needs.
//
// These are applied through the DSN rather than as one-off PRAGMA statements
// because database/sql pools connections: a PRAGMA run once applies only to
// whichever connection happened to run it, and the next connection would open
// with the library defaults again.
//
// The defaults modernc.org/sqlite ships are wrong for a server that writes:
//
//   - busy_timeout defaults to 0, so any write contention fails immediately
//     with SQLITE_BUSY instead of waiting. Two scans overlapping, or a scan
//     during a metadata write, produce "database is locked" rather than a short
//     delay.
//   - The default rollback journal makes a writer block readers, and makes bulk
//     work dramatically slower: a 2000-file scan measured 45s against 2.1s in
//     WAL mode.
//   - synchronous=NORMAL is the standard pairing with WAL. It is durable across
//     an application crash; only a power loss can lose the last few
//     transactions, and a media library is rebuildable by rescanning.
//   - foreign_keys defaults to off, which makes every constraint in the schema
//     decorative.
var sqlitePragmas = []string{
	"_pragma=busy_timeout(5000)",
	"_pragma=journal_mode(WAL)",
	"_pragma=synchronous(NORMAL)",
	"_pragma=foreign_keys(1)",
}

// SQLiteDSN builds the connection string for a SQLite database at path.
func SQLiteDSN(path string) string {
	if path == "" {
		return ""
	}

	// An already-formed URI is left alone apart from the pragmas, and the two
	// in-memory spellings are passed through untouched: neither is a file, so
	// neither can carry a query string usefully.
	if strings.HasPrefix(path, "file:") {
		return appendPragmas(path)
	}
	if strings.HasPrefix(path, ":") {
		return path
	}

	// Only the characters that would otherwise end the path and start a query
	// need escaping; sqlite tolerates spaces and the rest of a normal filename.
	escaped := strings.NewReplacer("?", "%3f", "#", "%23").Replace(path)
	return appendPragmas("file:" + escaped)
}

// appendPragmas adds the pragma parameters to an existing DSN.
func appendPragmas(dsn string) string {
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	return dsn + separator + strings.Join(sqlitePragmas, "&")
}
