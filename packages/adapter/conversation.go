package adapter

import (
	"errors"
	"io/fs"
	"os"
)

// ConversationGoneAtRoot is the standard ConversationProber rule for
// adapters whose storage root directory's presence indicates the tool's
// conversation storage is reachable. It distinguishes a deleted file
// from unreachable storage:
//
//   - path present                          → (false, true)  not gone
//   - path absent, root is a readable dir    → (true,  true)  deleted
//   - path absent, root absent/unreadable    → (false, false) undeterminable
//   - any non-NotExist stat error on path    → (false, false) undeterminable
//
// root is the adapter's layout anchor (e.g. ConversationRootDir). It lives
// under the same storage as path, so an unmounted home or a tool that
// was never installed makes root absent too — yielding ok=false rather
// than a false "deleted". Empty path or root is undeterminable.
func ConversationGoneAtRoot(path, root string) (gone bool, ok bool) {
	if path == "" || root == "" {
		return false, false
	}
	if _, err := os.Stat(path); err == nil {
		return false, true // present
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, false // can't tell (permission/IO error)
	}
	// path is absent: is the storage anchor present and a directory?
	if fi, err := os.Stat(root); err == nil && fi.IsDir() {
		return true, true // storage reachable, file gone → deleted
	}
	return false, false // storage unavailable → undeterminable
}
