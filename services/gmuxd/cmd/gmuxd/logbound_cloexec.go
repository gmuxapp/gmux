package main

import "syscall"

// setCloseOnExec marks fd close-on-exec, so no exec'd child inherits it.
//
// It is the enforcement half of the daemon log's inheritance invariant: a
// child that inherited a log descriptor would go on writing to whatever inode
// that descriptor names, which after two rotations is a file with no name at
// all. Rotation can move this process's descriptors; nothing can move a
// child's.
func setCloseOnExec(fd int) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), syscall.F_SETFD, syscall.FD_CLOEXEC); errno != 0 {
		return errno
	}
	return nil
}
