//go:build !(linux || darwin)

package tsdiag

import (
	"context"
	"errors"
)

// errProbeUnsupported: the shell-out is compiled only where it is audited.
// Probe() never reaches it (supportedGOOS gates the platform first), so this
// exists to keep the package compiling everywhere.
var errProbeUnsupported = errors.New("tailscale probing is not supported on this platform")

func execTailscaleStatus(context.Context) ([]byte, error) {
	return nil, errProbeUnsupported
}
