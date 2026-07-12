package adapters

import (
	"os"
	"time"
)

// fileLastActivity returns the conversation file's mtime — the file-backed
// adapters' implementation detail behind ConversationInfo.LastActivity
// (the tools append to the transcript on every message, so mtime tracks
// real conversation activity). Returns the zero time when the file can't
// be statted; consumers treat zero as "unknown".
func fileLastActivity(path string) time.Time {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return fi.ModTime()
}
