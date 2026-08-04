package adapter

// Drive modes (ADR 0033). A session's identity is its harness (adapter) plus
// its gmux session id; the drive mode is how gmux hosts and drives that
// harness. Capabilities attach to the (harness, mode) pair, never to the
// adapter name alone.
const (
	// DriveModeTerminal is the existing PTY session: attachable,
	// raw-sendable, tailable. Semantic control requires the ADR 0027
	// contract to be source-asserted, which only pi's in-repo extension
	// does.
	DriveModeTerminal = "terminal"
	// DriveModeACP is a runner speaking the Agent Client Protocol to a
	// terminal-less agent process. No PTY exists; there is no screen to
	// tail and nothing to attach to.
	DriveModeACP = "acp"
)

// ValidDriveMode reports whether s names a known drive mode. The empty
// string is not valid: callers that accept older wire payloads normalize
// absence to DriveModeTerminal themselves, at the boundary where the
// payload's age is known.
func ValidDriveMode(s string) bool {
	return s == DriveModeTerminal || s == DriveModeACP
}

// ACPDrivable marks adapters whose harness has an ACP drive mode
// (ADR 0033 §1) — today claude and codex, whose first-party ACP adapters
// the contract spike verified.
//
// Slice-0 consumer: refusal wording. A semantic verb against a
// terminal-mode session of an ACP-drivable harness names the mode boundary
// ("interactive-only; semantic control requires ACP mode") instead of the
// generic "no semantic agent actions", because for these harnesses a
// correct mode exists. The full "how to drive this harness via ACP" seam
// (adapter process, initialize expectations, native-storage ref mapping)
// lands with the ACP runner and will extend this interface.
type ACPDrivable interface {
	// SupportsACPDrive reports that the harness can be driven in ACP mode.
	SupportsACPDrive() bool
}

// SupportsACPDrive reports whether a's harness has an ACP drive mode.
func SupportsACPDrive(a Adapter) bool {
	d, ok := a.(ACPDrivable)
	return ok && d.SupportsACPDrive()
}
