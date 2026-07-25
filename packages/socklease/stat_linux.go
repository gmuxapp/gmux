//go:build linux

package socklease

import (
	"os"
	"syscall"
)

// statSocket reads a socket's identity on Linux.
//
// The stamp is the inode change time. For a file that is created and then never
// chmod'ed, chown'ed or renamed -- every socket in this protocol -- ctime is its
// creation time, and it is stored with nanosecond fields.
//
// Not the birth time, despite btime being the more obviously correct field:
// reading it needs statx, which the standard library does not expose, so it
// would mean a hand-written syscall number per architecture and a hand-written
// struct layout on the platform this code matters most on. Measured on this
// project's filesystems, btime buys nothing for it -- both stamps come from the
// same coarse kernel clock, and ~99% of back-to-back socket creations share
// either one. The risk is real and the gain is zero.
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
		//nolint:unconvert // Dev is uint64 on linux and int32 on darwin.
		Dev:       uint64(st.Dev),
		Ino:       uint64(st.Ino),
		StampSec:  int64(st.Ctim.Sec),
		StampNsec: int64(st.Ctim.Nsec),
		Stamp:     StampChange,
	}, true
}

// oPath is O_PATH: open a file to refer to it, without opening it for I/O. It
// is the only way to get a handle on a socket file, which cannot be opened
// normally, and it is what makes an exact pin possible on Linux.
const oPath = 0x200000

func pinFile(path string) (int, error) {
	return syscall.Open(path, syscall.O_RDONLY|oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
}

func pinnedLinkCount(fd int) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		return 0, err
	}
	return uint64(st.Nlink), nil
}

func closeFile(fd int) error { return syscall.Close(fd) }
