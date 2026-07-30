package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinHasPendingData reports whether f contains launch-time input which the
// headless session path will not forward. It is deliberately non-consuming
// and best-effort: any inspection error means no notice.
func stdinHasPendingData(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}

	switch {
	case info.Mode().IsRegular():
		offset, err := f.Seek(0, 1)
		return err == nil && info.Size() > offset
	case info.Mode()&os.ModeNamedPipe != 0:
		fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 0)
		return err == nil && n > 0 && fds[0].Revents&unix.POLLIN != 0
	default:
		// Character devices (including /dev/null), sockets, and unknown file
		// types are not launcher input, even when poll calls them readable.
		return false
	}
}
