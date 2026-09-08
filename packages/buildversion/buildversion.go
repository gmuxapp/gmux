// Package buildversion answers one question about the version string that
// -ldflags stamps into gmux and gmuxd: did this binary come from a working
// tree, or from a release?
//
// It lives in a shared package because the answer drives policy in three
// separate places — the CLI's daemon autostart, gmuxd's incumbent/takeover
// check, and the update checker — and three copies of the predicate would
// drift. A drifting copy is not cosmetic: gmuxd decides whether to shut a
// healthy daemon down by comparing versions.
package buildversion

import "strings"

// Dev is the version a build carries when nothing stamped it.
const Dev = "dev"

// IsDev reports whether a version denotes a development build.
//
// scripts/build.sh stamps source builds "dev+<short hash>" (optionally
// "-dirty") so a local install can say which tree it came from. Those hashes
// move on every rebuild, so anything that treats "dev" as "not a release" —
// and in particular anything that compares two versions for equality to decide
// whether to replace a running daemon — must treat every dev+ stamp as the
// same kind of build.
func IsDev(v string) bool {
	return v == Dev || strings.HasPrefix(v, Dev+"+")
}

// SameBuildClass reports whether two version strings should be treated as the
// same daemon version for replacement decisions: either they are identical, or
// both are development builds (whose hashes differ per rebuild).
func SameBuildClass(a, b string) bool {
	return a == b || (IsDev(a) && IsDev(b))
}
