//go:build !linux

package main

import "syscall"

// dupOnto atomically makes dst refer to the same open file as src, and marks
// dst close-on-exec.
//
// Darwin has no dup3, so the close-on-exec flag needs a second call and the
// window this platform cannot avoid is real: a fork landing between the two
// would hand a child the daemon's log. It is not a correctness hole for the log
// specifically -- every child launch in this package wires its stdio explicitly
// -- and the flag is defence against code that does not.
//
// As on Linux, that dupOnto leaves the destination close-on-exec is asserted
// directly; the size of the window is not testable.
func dupOnto(src, dst int) error {
	if src == dst {
		return nil
	}
	if err := syscall.Dup2(src, dst); err != nil {
		return err
	}
	return setCloseOnExec(dst)
}
