//go:build !linux && !darwin

package socklease

import (
	"os"
	"syscall"
)

// statSocket on a platform this package has not been taught: the socket type is
// still checked, but no creation stamp is reported, so every identity is
// unknown.
//
// That is the conservative outcome by construction rather than by policy: an
// unknown identity suppresses no probe and authorises no unlink, so the lease
// still excludes concurrent runners and nothing destructive happens on evidence
// this platform cannot supply. Sockets accumulate instead, which is a leak, not
// a hazard.
func statSocket(path string) (Ident, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&fileModeSocket == 0 {
		return Ident{}, false
	}
	return Ident{}, true
}

func pinFile(string) (int, error) { return -1, syscall.ENOSYS }

func pinnedLinkCount(int) (uint64, error) { return 0, syscall.ENOSYS }

func closeFile(int) error { return nil }
