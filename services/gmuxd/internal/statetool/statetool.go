// Package statetool implements the admin state tooling behind
// `gmux daemon state check|backup|export` (cutover design §5): the daemon's
// /v1/state/* HTTP handlers, the offline-ownership gate for check/backup
// against a database no daemon is serving, and the deterministic redacted
// export document.
//
// All schema-aware SQL lives in internal/centralstore's admin surface; this
// package owns transport, gating policy, and output shaping. The routes are
// Unix-socket-only: internal/netauth blocks the /v1/state/ prefix on every
// network listener (TCP and tailscale both wrap netauth.Middleware), like
// /v1/shutdown.
package statetool

import (
	"net/url"
	"regexp"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// CheckReport is the wire shape of a state check: OK is true iff no
// invariant finding was produced. Operational failures (the check could not
// run) are errors, not reports.
type CheckReport struct {
	OK       bool                        `json:"ok"`
	Findings []centralstore.CheckFinding `json:"findings"`
}

// ReportFor constructs the canonical check report (including a non-nil
// empty findings array for stable JSON).
func ReportFor(findings []centralstore.CheckFinding) CheckReport {
	if findings == nil {
		findings = []centralstore.CheckFinding{}
	}
	return CheckReport{OK: len(findings) == 0, Findings: findings}
}

var schemelessCredentials = regexp.MustCompile(`^[^/@\s:]+:[^/@\s]+@`)

// RedactURLUserinfo strips embedded credentials (userinfo) from a URL
// string, replacing them with "REDACTED@" so their presence stays visible.
// It also conservatively handles schemeless credential-shaped git remotes
// such as "user:password@host/path": net/url parses those as an opaque URL
// whose User field is nil. Other unparseable/userinfo-free values pass
// through verbatim (design checklist 5: every exported URL field is
// scrubbed).
//
// Query-string credentials remain outside the URL-userinfo contract. This
// intentionally does not guess at arbitrary parameter names or destructively
// rewrite non-credential query data.
func RedactURLUserinfo(raw string) string {
	if raw == "" {
		return raw
	}
	if schemelessCredentials.MatchString(raw) {
		return schemelessCredentials.ReplaceAllString(raw, "REDACTED@")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User == nil {
		return raw
	}
	parsed.User = url.User("REDACTED")
	return parsed.String()
}
