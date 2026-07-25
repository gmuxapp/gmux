//go:build linux

package main

import "syscall"

// dupOnto atomically makes dst refer to the same open file as src, and marks
// dst close-on-exec in the same operation.
//
// Linux/arm64 has no dup2 syscall at all, so Dup3 is both the portable choice
// within Linux and the atomic one: a writer racing this call sees either the
// old file or the new one, never a closed descriptor. Passing O_CLOEXEC here
// rather than setting it afterwards leaves no window in which a fork could
// hand a child the daemon's log.
//
// What is pinned by tests and what is not: that dupOnto leaves the destination
// close-on-exec is asserted directly (TestDupOntoMarksTheDestinationCloseOnExec),
// and the invariant it serves -- no descendant holds a log descriptor -- is
// asserted after install and after two rotations. The *absence of a window*
// between the dup and the flag is not observable from user space at all: with
// the flag folded into the syscall there is no second step to race, and a
// version that set it afterwards would differ only in a few instructions of
// exposure. That half rests on this argument and on review, not on a test.
func dupOnto(src, dst int) error {
	if src == dst {
		return nil
	}
	return syscall.Dup3(src, dst, syscall.O_CLOEXEC)
}
