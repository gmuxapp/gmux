//go:build darwin

package socklease

import (
	"os"
	"syscall"
)

// statSocket reads a socket's identity on Darwin, where the birth time is in
// the ordinary stat result and is therefore free to use.
//
// Never the modification time: mtime is caller-visible and would make an
// identity something another process can forge by touching the file.
func statSocket(path string) (Ident, bool) {
	fi, err := os.Lstat(path)
	if err != nil || fi.Mode()&fileModeSocket == 0 {
		return Ident{}, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return Ident{}, false
	}
	return Ident{
		Dev:       uint64(st.Dev),
		Ino:       uint64(st.Ino),
		StampSec:  int64(st.Birthtimespec.Sec),
		StampNsec: int64(st.Birthtimespec.Nsec),
		Stamp:     StampBirth,
	}, true
}

// Darwin has no O_PATH, and open(2) on a socket fails, so there is no handle to
// pin with: a pin degrades to its identity, which here means device, inode and
// APFS birth time.
//
// The reincarnation this guards against is also much harder to reach on Darwin:
// APFS assigns object identifiers from a counter rather than a reusable bitmap,
// so an inode number freed now is not handed out again next. Where the number
// cannot repeat, the number is already an identity.
func pinFile(string) (int, error) { return -1, syscall.ENOTSUP }

func pinnedLinkCount(int) (uint64, error) { return 0, syscall.ENOTSUP }

func closeFile(int) error { return nil }
