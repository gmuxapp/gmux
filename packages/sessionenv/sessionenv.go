// Package sessionenv filters gmux session-identity environment
// variables out of a process environment before it is handed to a
// child that must not inherit the parent session's identity.
//
// Every gmux session is stamped with a set of GMUX_* variables that
// describe *that specific session* — its socket, id, adapter, runner
// version (see the runner's common-env block in cli/gmux). When a
// process that already lives inside a session spawns the daemon — or
// when the daemon launches/restarts a runner — those identity vars
// must be stripped, or the child would masquerade as the parent
// session: a daemon auto-started from inside a session would otherwise
// carry GMUX_SESSION_ID/GMUX_SOCKET/GMUX_ADAPTER and stamp them onto
// every future session it launches. See the investigation in the
// "stale/leaked session env" work.
package sessionenv

import "strings"

// preserve lists GMUX_-prefixed variables that are *configuration*,
// not session identity, and must survive Strip. GMUX_SOCKET_DIR tells
// both the daemon (discovery scan) and the runner (where to bind its
// socket) which directory session sockets live in; stripping it would
// make an auto-started daemon scan a different directory than the
// runner that triggered the auto-start, so they could never find each
// other.
var preserve = map[string]bool{
	"GMUX_SOCKET_DIR": true,
}

// Strip returns env with gmux session-identity variables removed: the
// bare GMUX marker and every GMUX_* variable except the configuration
// vars in preserve. GMUXD_* daemon config is untouched (it does not
// match the GMUX_ prefix — the character after GMUX is 'D', not '_').
//
// Used by every site that spawns a process which must not inherit the
// caller's session identity: the daemon launching/restarting runners,
// the daemon self-restart, and gmux auto-starting the daemon.
func Strip(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		key := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			key = e[:i]
		}
		if key == "GMUX" {
			continue
		}
		if strings.HasPrefix(key, "GMUX_") && !preserve[key] {
			continue
		}
		out = append(out, e)
	}
	return out
}
