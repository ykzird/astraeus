package sqlite

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

// sqliteTxLock starts every transaction as BEGIN IMMEDIATE rather than BEGIN
// DEFERRED, and it is the fix for the failure the other pragmas cannot reach.
//
// A deferred transaction takes a read lock first and upgrades to a write lock
// when it first writes. If another connection committed in between, SQLite
// cannot give the upgrade - the transaction's snapshot is stale - so it returns
// SQLITE_BUSY_SNAPSHOT immediately, and the busy handler is never consulted.
// That is why busy_timeout does not help and why a scan running beside the
// metadata worker failed with "database is locked (517)" after zero to two files
// (L-4 of the 2026-10-09 review). file:..._txlock=immediate takes the write lock
// up front, where the busy handler *does* apply, so the two wait for each other
// instead of one of them failing.
//
// The cost is that immediate transactions serialise writers. That is the right
// trade here: this is a single-process server whose writers are a scan, a
// metadata pass and the API, all of which want to finish rather than to
// interleave. Readers are unaffected, because WAL lets them proceed against a
// consistent snapshot.
const sqliteTxLock = "_txlock=immediate"

// DSN builds the connection string for a SQLite database at path.
func DSN(path string) string {
	if path == "" {
		return ""
	}

	// An already-formed URI is left alone apart from the pragmas, and the two
	// in-memory spellings are passed through untouched: neither is a file, so
	// neither can carry a query string usefully.
	if strings.HasPrefix(path, "file:") {
		return appendPragmas(path)
	}
	// Note for the in-memory spellings: they skip the pragmas entirely, so they
	// also skip the transaction lock. That is a test-only path, and the locking
	// question is about files anyway.
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
	return dsn + separator + strings.Join(append([]string{sqliteTxLock}, sqlitePragmas...), "&")
}
