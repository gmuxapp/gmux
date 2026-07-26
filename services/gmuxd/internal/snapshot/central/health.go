package central

import (
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
)

// HealthInfo is the diagnostic blob shared between GET /v1/health and the
// snapshot.world SSE event. It is the typed successor of the map[string]any
// composeHealth builds in cmd/gmuxd/main.go (cutover checklist 4: PeerWorld
// type tightening); JSON tags reproduce the map keys byte-for-byte, with
// the same presence rules (omitempty where the map conditionally sets the
// key, always-present otherwise).
//
// The auth_token field is deliberately absent: the /v1/health route injects
// it only on local Unix-socket connections, and it must never ride
// snapshot.world.
//
// Sessions is the one field the PeerSource does not supply: per FD-6 the
// counts derive from the durable rows + runtime registry at wire
// conversion, so the wire layer stamps them onto the emitted copy.
type HealthInfo struct {
	Service string `json:"service"`
	Version string `json:"version"`
	NodeID  string `json:"node_id"`
	Status  string `json:"status"`
	// Hostname is the node identity (ADR 0007): the live tailscale name
	// when connected, else the OS hostname.
	Hostname     string             `json:"hostname"`
	TailscaleURL string             `json:"tailscale_url,omitempty"`
	Tailscale    *tsauth.DiagStatus `json:"tailscale,omitempty"`
	Listen       string             `json:"listen"`
	// UpdateAvailable carries the newer released version string, when one
	// is known.
	UpdateAvailable string             `json:"update_available,omitempty"`
	Peers           []peering.PeerInfo `json:"peers,omitempty"`
	Sessions        SessionCounts      `json:"sessions"`
	// RunnerHash is the sha256 of the gmux runner binary on disk.
	RunnerHash      string                `json:"runner_hash,omitempty"`
	DefaultLauncher string                `json:"default_launcher"`
	Launchers       []peering.LauncherDef `json:"launchers"`
}

// SessionCounts is the health session summary. Per FD-6 it is derived from
// registry + durable rows: dismissed rows no longer count as dead
// (diagnostic-only field; the accepted wire diff).
type SessionCounts struct {
	LocalAlive  int `json:"local_alive"`
	RemoteAlive int `json:"remote_alive"`
	Dead        int `json:"dead"`
}
