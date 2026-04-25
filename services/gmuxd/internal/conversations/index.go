// Package conversations maintains an index of conversation files
// discovered on disk. It maps (kind, slug) to file metadata, enabling
// URL resolution for dead conversations and (future) fulltext search.
//
// The index is populated on startup by scanning adapter session
// directories and updated as filemon detects new or changed files.
// It never writes to the session store.
package conversations

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
)

// Info holds metadata for a single conversation file.
type Info struct {
	ToolID        string    // full UUID from the session file header
	Slug          string    // human-readable URL identifier, unique within (kind)
	Kind          string    // adapter name (claude, codex, pi, shell)
	Title         string    // display title
	Cwd           string    // working directory
	FilePath      string    // absolute path to the session file
	ResumeCommand []string  // command to resume this conversation
	Created       time.Time // when the conversation started
}

// Index is a concurrency-safe lookup table for conversation files.
// It is the authority on slug uniqueness: when two conversations
// produce the same slug within the same kind, the index assigns
// -2, -3 suffixes.
type Index struct {
	mu sync.RWMutex
	// byKey maps "kind/slug" → Info.
	byKey map[string]Info
	// byToolID maps "kind/toolID" → slug for reverse lookup
	// (e.g., finding a conversation's slug from its tool UUID).
	byToolID map[string]string
	// fileStat caches (mtime, size) per file path so Scan() can skip
	// re-parsing session files that haven't changed since the last
	// scan. Pi/Claude/Codex session JSONL files can grow to tens of
	// MB; without this cache, periodic rescans read and re-parse the
	// entire session corpus every interval.
	fileStat map[string]fileStat
}

// fileStat snapshots a file's mtime and size at index time. Two
// snapshots compare equal iff the file is byte-identical to the last
// indexed version (modulo mtime/size collisions, which are negligible
// for append-only JSONL files).
type fileStat struct {
	modTimeUnixNano int64
	size            int64
}

// New creates an empty index.
func New() *Index {
	return &Index{
		byKey:    make(map[string]Info),
		byToolID: make(map[string]string),
		fileStat: make(map[string]fileStat),
	}
}

func indexKey(kind, slug string) string {
	return kind + "/" + slug
}

func toolKey(kind, toolID string) string {
	return kind + "/" + toolID
}

// Lookup returns the conversation info for a (kind, slug) pair.
// Returns ok=false if no matching conversation exists.
func (idx *Index) Lookup(kind, slug string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	info, ok := idx.byKey[indexKey(kind, slug)]
	return info, ok
}

// LookupByToolID returns the slug for a conversation identified by
// its adapter-level tool ID. Returns empty string if unknown.
func (idx *Index) LookupByToolID(kind, toolID string) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.byToolID[toolKey(kind, toolID)]
}

// Upsert adds or updates a conversation in the index. If the slug
// collides with an existing entry of the same kind (but different
// tool ID), a -2, -3, ... suffix is appended. Returns the final
// (possibly suffixed) slug.
func (idx *Index) Upsert(info Info) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// If this tool ID already has a slug, update in place.
	tk := toolKey(info.Kind, info.ToolID)
	if existing, ok := idx.byToolID[tk]; ok {
		ik := indexKey(info.Kind, existing)
		info.Slug = existing
		idx.byKey[ik] = info
		return existing
	}

	// Assign a unique slug.
	info.Slug = idx.uniqueSlugLocked(info.Kind, info.Slug, info.ToolID)
	ik := indexKey(info.Kind, info.Slug)
	idx.byKey[ik] = info
	idx.byToolID[tk] = info.Slug
	return info.Slug
}

// uniqueSlugLocked returns a slug that doesn't collide within the
// given kind. Appends -2, -3, ... on collision. Must be called with
// idx.mu held.
func (idx *Index) uniqueSlugLocked(kind, slug, toolID string) string {
	base := slug
	for i := 2; ; i++ {
		ik := indexKey(kind, slug)
		existing, occupied := idx.byKey[ik]
		if !occupied || existing.ToolID == toolID {
			return slug
		}
		slug = base + "-" + strconv.Itoa(i)
	}
}

// Count returns the number of indexed conversations.
func (idx *Index) Count() int {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return len(idx.byKey)
}

// All returns a snapshot of all indexed conversations.
func (idx *Index) All() []Info {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	out := make([]Info, 0, len(idx.byKey))
	for _, info := range idx.byKey {
		out = append(out, info)
	}
	return out
}

// Scan discovers all conversation files from all SessionFiler adapters
// and populates the index. Safe to call multiple times; existing entries
// are updated, new entries are added.
func (idx *Index) Scan() {
	for _, a := range adapters.AllAdapters() {
		sf, ok := a.(adapter.SessionFiler)
		if !ok {
			continue
		}
		resumer, hasResume := a.(adapter.Resumer)

		root := sf.SessionRootDir()
		if root == "" {
			continue
		}

		var allFiles []string
		if lister, ok := a.(adapter.SessionFileLister); ok {
			allFiles = lister.ListSessionFiles()
		} else {
			subdirs, err := os.ReadDir(root)
			if err != nil {
				if !os.IsNotExist(err) {
					log.Printf("conversations: read root %s: %v", root, err)
				}
				continue
			}
			for _, d := range subdirs {
				if !d.IsDir() {
					continue
				}
				dir := filepath.Join(root, d.Name())
				allFiles = append(allFiles, adapters.ListSessionFiles(dir)...)
			}
		}

		for _, path := range allFiles {
			stat, err := os.Stat(path)
			if err != nil {
				continue
			}
			snap := fileStat{
				modTimeUnixNano: stat.ModTime().UnixNano(),
				size:            stat.Size(),
			}
			if idx.unchanged(path, snap) {
				continue
			}
			// Record the snapshot before the parse-and-filter chain so
			// that even files we skip (parse errors, missing cwd, not
			// resumable) don't get re-read on every scan. Any future
			// change bumps mtime or size, which invalidates the cache.
			idx.recordStat(path, snap)

			fileInfo, err := sf.ParseSessionFile(path)
			if err != nil {
				continue
			}

			if fileInfo.Cwd == "" {
				continue
			}

			if hasResume && !resumer.CanResume(path) {
				continue
			}

			slug := fileInfo.Slug
			if slug == "" {
				slug = adapter.Slugify(fileInfo.ID)
			}

			var cmd []string
			if hasResume {
				cmd = resumer.ResumeCommand(fileInfo)
			}

			info := Info{
				ToolID:        fileInfo.ID,
				Slug:          slug,
				Kind:          a.Name(),
				Title:         fileInfo.Title,
				Cwd:           fileInfo.Cwd,
				FilePath:      path,
				ResumeCommand: cmd,
				Created:       fileInfo.Created,
			}
			idx.Upsert(info)
		}
	}
}

// unchanged reports whether path was indexed previously with the same
// mtime/size. Callers use this to skip re-parsing session files that
// haven't been touched since the last scan.
func (idx *Index) unchanged(path string, snap fileStat) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	cached, ok := idx.fileStat[path]
	return ok && cached == snap
}

// recordStat memoizes the file's mtime/size so subsequent scans can
// short-circuit. Stored regardless of whether parsing or filtering
// succeeded: any future change to the file bumps mtime or size and
// invalidates the cache.
func (idx *Index) recordStat(path string, snap fileStat) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	idx.fileStat[path] = snap
}

// ScanFile indexes a single conversation file. Called by filemon when
// a file is created or modified. Returns the assigned slug.
func (idx *Index) ScanFile(a adapter.Adapter, path string) string {
	sf, ok := a.(adapter.SessionFiler)
	if !ok {
		return ""
	}

	fileInfo, err := sf.ParseSessionFile(path)
	if err != nil {
		return ""
	}

	if fileInfo.Cwd == "" {
		return ""
	}

	slug := fileInfo.Slug
	if slug == "" {
		slug = adapter.Slugify(fileInfo.ID)
	}

	var cmd []string
	if resumer, ok := a.(adapter.Resumer); ok {
		if !resumer.CanResume(path) {
			return ""
		}
		cmd = resumer.ResumeCommand(fileInfo)
	}

	info := Info{
		ToolID:        fileInfo.ID,
		Slug:          slug,
		Kind:          a.Name(),
		Title:         fileInfo.Title,
		Cwd:           fileInfo.Cwd,
		FilePath:      path,
		ResumeCommand: cmd,
		Created:       fileInfo.Created,
	}
	slug = idx.Upsert(info)
	if stat, err := os.Stat(path); err == nil {
		idx.recordStat(path, fileStat{
			modTimeUnixNano: stat.ModTime().UnixNano(),
			size:            stat.Size(),
		})
	}
	return slug
}

// Remove deletes a conversation from the index by tool ID.
// Returns true if it was present.
func (idx *Index) Remove(kind, toolID string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	tk := toolKey(kind, toolID)
	slug, ok := idx.byToolID[tk]
	if !ok {
		return false
	}
	ik := indexKey(kind, slug)
	if info, ok := idx.byKey[ik]; ok && info.FilePath != "" {
		// Drop the cached stat so a recreated file at the same path
		// is re-parsed on the next scan.
		delete(idx.fileStat, info.FilePath)
	}
	delete(idx.byToolID, tk)
	delete(idx.byKey, ik)
	return true
}

// SlugExists reports whether a slug is taken within a kind.
func (idx *Index) SlugExists(kind, slug string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.byKey[indexKey(kind, slug)]
	return ok
}

// LookupBySlug searches for a conversation by slug across all kinds.
// Returns the first match. Used when the caller doesn't know the kind
// (e.g., project session arrays that store bare slugs).
func (idx *Index) LookupBySlug(slug string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for key, info := range idx.byKey {
		// key is "kind/slug"; check if the slug suffix matches.
		if i := len(key) - len(slug); i > 0 && key[i-1] == '/' && key[i:] == slug {
			return info, true
		}
	}
	return Info{}, false
}

// FindByPrefix returns conversations whose slug starts with the given
// prefix, within a kind. Used for URL resolution when the frontend
// provides a partial slug (e.g. from session.id.slice(0, 8)).
func (idx *Index) FindByPrefix(kind, prefix string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	keyPrefix := kind + "/" + prefix
	for key, info := range idx.byKey {
		if strings.HasPrefix(key, keyPrefix) {
			return info, true
		}
	}
	return Info{}, false
}
