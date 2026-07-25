package socklease

import (
	"fmt"
	"os"
)

// StampKind names which of the inode's timestamps an Ident carries. Two
// identities derived from different clocks are never comparable, so this is
// part of the identity rather than a note about it.
type StampKind uint8

const (
	// StampNone means the platform gave us no creation-ish timestamp. An Ident
	// without one is unknown: it cannot be told apart from a later inode that
	// happens to reuse the same number.
	StampNone StampKind = iota
	// StampChange is the inode's change time (st_ctim). For a file that is
	// created and never chmod'ed or renamed -- which is every socket in this
	// protocol -- it is the creation time.
	StampChange
	// StampBirth is the inode's birth time, where the platform records one.
	StampBirth
)

func (k StampKind) String() string {
	switch k {
	case StampChange:
		return "ctime"
	case StampBirth:
		return "btime"
	}
	return "no-stamp"
}

// Ident is the physical identity ("incarnation") of a socket pathname.
//
// Device and inode number alone are not an identity. Inode numbers are
// recycled, and on some filesystems recycled *immediately*: a socket unlinked
// and rebound in the same breath can come back with the same number, which is
// how a live replacement runner came to look exactly like the dead leftover it
// replaced -- and got unlinked for it. So an Ident also carries the inode's
// creation timestamp, which separates two incarnations that a number cannot.
//
// It is deliberately a comparable value: the daemon keys installed generations
// on it. It is also deliberately runtime-only. Nothing persists an Ident --
// inode numbers and timestamps describe this boot of this filesystem, and a
// stored one would be a fact about a world that no longer exists.
//
// # What this does and does not settle
//
// The stamp comes from the platform's file-timestamp clock, which is coarse:
// Linux derives it from jiffies, so two creations a few microseconds apart
// share a timestamp (measured on ext4 and tmpfs: ~99% of back-to-back socket
// creations share both ctime and statx btime). An Ident therefore separates
// incarnations that are more than a clock tick apart, and cannot separate a
// number recycled inside one tick.
//
// That is why an Ident never authorises an unlink on its own. The destructive
// paths hold a Pin, which answers "is this still the file I decided about?"
// exactly, without reasoning about numbers or clocks at all.
type Ident struct {
	Dev uint64
	Ino uint64
	// StampSec and StampNsec are the inode's creation-ish timestamp; Stamp
	// records which one it is.
	StampSec  int64
	StampNsec int64
	Stamp     StampKind
}

// Known reports whether the identity was actually observed, in full. An
// identity without a timestamp is not partially useful: it is exactly the
// number-only identity that inode reuse defeats, so it counts as unknown and
// every consumer treats it conservatively.
func (i Ident) Known() bool {
	return (i.Dev != 0 || i.Ino != 0) && i.Stamp != StampNone
}

// Same reports whether two identities are known and identical. Unknown
// identities never compare equal, not even to themselves, and identities
// derived from different clocks never compare equal to each other.
func (i Ident) Same(o Ident) bool { return i.Known() && i == o }

func (i Ident) String() string {
	if !i.Known() {
		return "unknown-identity"
	}
	return fmt.Sprintf("%d/%d@%d.%09d(%s)", i.Dev, i.Ino, i.StampSec, i.StampNsec, i.Stamp)
}

// StatSocket returns the identity of the socket file at path. It reports
// isSocket=false when the path does not exist, is not a socket, or cannot be
// stat'ed. It never follows symlinks: a symlinked pathname is not a socket and
// is therefore never treated as one.
func StatSocket(path string) (id Ident, isSocket bool) { return statSocket(path) }

// Pin is a handle on one specific file, held so that a decision made about
// that file can later be checked against the file itself rather than against
// its name.
//
// This is the primitive the destructive paths rest on, because it is the only
// one that is exact. A caller that probed a leftover socket and found nothing
// listening wants to unlink *that file* -- but between the probe and the unlink
// the pathname can be handed to a live runner, and no amount of stat'ing can
// reliably tell the difference once the kernel has recycled the inode number.
// A pin can: the file it holds is either still the one the pathname names, or
// it is not.
type Pin struct {
	path  string
	ident Ident
	// fd is a handle on the pinned file where the platform can open one
	// (Linux's O_PATH). It is -1 otherwise, and StillNamesIt falls back to
	// comparing identities, which is as good as the platform's timestamps.
	fd int
}

// PinSocket pins the socket the lease owns.
func (l *Lease) PinSocket() (*Pin, error) { return PinSocket(l.sockPath) }

// PinSocket pins the socket file at path. The caller must Close the pin.
func PinSocket(path string) (*Pin, error) {
	id, isSocket := StatSocket(path)
	if !isSocket {
		return nil, fmt.Errorf("socklease: %s is not a socket", path)
	}
	fd, err := pinFile(path)
	if err != nil {
		// No handle available: the pin degrades to its identity, which is what
		// every pre-Pin version of this code had.
		fd = -1
	}
	return &Pin{path: path, ident: id, fd: fd}, nil
}

// Ident returns the identity of the pinned file.
func (p *Pin) Ident() Ident { return p.ident }

// Path returns the pathname this pin was taken on.
func (p *Pin) Path() string {
	if p == nil {
		return ""
	}
	return p.path
}

// Exact reports whether this pin can answer StillNamesIt exactly, or is
// falling back to comparing identities.
func (p *Pin) Exact() bool { return p != nil && p.fd >= 0 }

// StillNamesIt reports whether the pathname still names the pinned file.
//
// With a handle this is exact and needs no assumptions about inode numbering:
// the pinned file must still be linked exactly once (so it has not been
// unlinked, and has not been hardlinked elsewhere), and the pathname must still
// resolve to that inode. Two live inodes on one device cannot share an inode
// number, so those two facts together mean the pathname resolves to the pinned
// file and nothing else.
//
// Without a handle it compares identities, which separates incarnations by the
// platform's timestamp granularity -- see Ident.
func (p *Pin) StillNamesIt() bool {
	if p == nil {
		return false
	}
	current, isSocket := StatSocket(p.path)
	if !isSocket || current.Dev != p.ident.Dev || current.Ino != p.ident.Ino {
		return false
	}
	if p.fd < 0 {
		return current.Same(p.ident)
	}
	links, err := pinnedLinkCount(p.fd)
	if err != nil {
		return false
	}
	return links == 1
}

// Close releases the pin.
func (p *Pin) Close() error {
	if p == nil || p.fd < 0 {
		return nil
	}
	fd := p.fd
	p.fd = -1
	return closeFile(fd)
}

// statFileMode is the mode bits a socket must have; kept here so the
// platform files agree on it.
const fileModeSocket = os.ModeSocket
