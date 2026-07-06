// Package conversations maintains an index of conversation files
// discovered on disk. It maps (adapter, slug) to file metadata, enabling
// URL resolution for dead conversations and (future) fulltext search.
//
// The index is populated and kept current by adapter ConversationSources
// (snapshot at startup, incremental thereafter). It never writes to the
// session store.
package conversations

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Info holds metadata for a single conversation file.
type Info struct {
	ConversationID string    // full UUID from the session file header
	Slug           string    // human-readable URL identifier, unique within (adapter)
	Adapter        string    // adapter name (claude, codex, pi, shell)
	Title          string    // display title
	Cwd            string    // working directory
	FilePath       string    // absolute path to the session file
	ResumeCommand  []string  // command to resume this conversation
	Created        time.Time // when the conversation started
}

// Index is a concurrency-safe lookup table for conversation files.
// It is the authority on slug uniqueness: when two conversations
// produce the same slug within the same adapter, the index assigns
// -2, -3 suffixes.
type Index struct {
	mu sync.RWMutex
	// byKey maps "adapter/slug" → Info.
	byKey map[string]Info
	// byConversationID maps "adapter/conversationID" → slug for reverse lookup
	// (e.g., finding a conversation's slug from the agent's conversation UUID).
	byConversationID map[string]string
}

// New creates an empty index.
func New() *Index {
	return &Index{
		byKey:            make(map[string]Info),
		byConversationID: make(map[string]string),
	}
}

func indexKey(adapterName, slug string) string {
	return adapterName + "/" + slug
}

func convKey(adapterName, conversationID string) string {
	return adapterName + "/" + conversationID
}

// Lookup returns the conversation info for an (adapter, slug) pair.
// Returns ok=false if no matching conversation exists.
func (idx *Index) Lookup(adapterName, slug string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	info, ok := idx.byKey[indexKey(adapterName, slug)]
	return info, ok
}

// LookupByConversationID returns the slug for a conversation identified by
// its agent-native conversation ID. Returns empty string if unknown.
func (idx *Index) LookupByConversationID(adapterName, conversationID string) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.byConversationID[convKey(adapterName, conversationID)]
}

// Upsert adds or updates a conversation in the index. If the slug
// collides with an existing entry of the same adapter (but different
// conversation ID), a -2, -3, ... suffix is appended. Returns the final
// (possibly suffixed) slug.
func (idx *Index) Upsert(info Info) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// If this conversation ID already has a slug, update in place.
	tk := convKey(info.Adapter, info.ConversationID)
	if existing, ok := idx.byConversationID[tk]; ok {
		ik := indexKey(info.Adapter, existing)
		info.Slug = existing
		idx.byKey[ik] = info
		return existing
	}

	// Assign a unique slug.
	info.Slug = idx.uniqueSlugLocked(info.Adapter, info.Slug, info.ConversationID)
	ik := indexKey(info.Adapter, info.Slug)
	idx.byKey[ik] = info
	idx.byConversationID[tk] = info.Slug
	return info.Slug
}

// uniqueSlugLocked returns a slug that doesn't collide within the
// given adapter. Appends -2, -3, ... on collision. Must be called with
// idx.mu held.
func (idx *Index) uniqueSlugLocked(adapterName, slug, conversationID string) string {
	base := slug
	for i := 2; ; i++ {
		ik := indexKey(adapterName, slug)
		existing, occupied := idx.byKey[ik]
		if !occupied || existing.ConversationID == conversationID {
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

// ScanFile indexes a single conversation file (snapshot or live update
// from an adapter ConversationSource). Returns the assigned slug.
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
		ConversationID: fileInfo.ID,
		Slug:           slug,
		Adapter:        a.Name(),
		Title:          fileInfo.Title,
		Cwd:            fileInfo.Cwd,
		FilePath:       path,
		ResumeCommand:  cmd,
		Created:        fileInfo.Created,
	}
	return idx.Upsert(info)
}

// Remove deletes a conversation from the index by conversation ID.
// Returns true if it was present.
func (idx *Index) Remove(adapterName, conversationID string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	tk := convKey(adapterName, conversationID)
	slug, ok := idx.byConversationID[tk]
	if !ok {
		return false
	}
	delete(idx.byConversationID, tk)
	delete(idx.byKey, indexKey(adapterName, slug))
	return true
}

// RemoveByPath deletes any conversation whose FilePath matches path.
// Used when a ConversationSource observes a deletion event and we don't have the
// (adapter, conversationID) handy. Linear walk over the index; that's fine
// because Remove events are rare (manual `rm`, file rotation) and
// the index size stays in the hundreds-to-low-thousands range.
// Returns true if an entry was removed.
//
// Session retirement on file-gone deliberately does NOT hang off this
// method: an unindexed conversation (parse failure, CanResume=false,
// empty cwd) still needs retiring when its file disappears, so the
// watcher-level sink (sources.go) owns that signal instead.
func (idx *Index) RemoveByPath(path string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for key, info := range idx.byKey {
		if info.FilePath != path {
			continue
		}
		delete(idx.byKey, key)
		delete(idx.byConversationID, convKey(info.Adapter, info.ConversationID))
		return true
	}
	return false
}

// SlugExists reports whether a slug is taken within an adapter.
func (idx *Index) SlugExists(adapterName, slug string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.byKey[indexKey(adapterName, slug)]
	return ok
}

// LookupBySlug searches for a conversation by slug across all kinds.
// Returns the first match. Used when the caller doesn't know the adapter
// (e.g., project session arrays that store bare slugs).
func (idx *Index) LookupBySlug(slug string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for key, info := range idx.byKey {
		// key is "adapter/slug"; check if the slug suffix matches.
		if i := len(key) - len(slug); i > 0 && key[i-1] == '/' && key[i:] == slug {
			return info, true
		}
	}
	return Info{}, false
}

// FindByPrefix returns conversations whose slug starts with the given
// prefix, within an adapter. Used for URL resolution when the frontend
// provides a partial slug (e.g. from session.id.slice(0, 8)).
func (idx *Index) FindByPrefix(adapterName, prefix string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	keyPrefix := adapterName + "/" + prefix
	for key, info := range idx.byKey {
		if strings.HasPrefix(key, keyPrefix) {
			return info, true
		}
	}
	return Info{}, false
}
