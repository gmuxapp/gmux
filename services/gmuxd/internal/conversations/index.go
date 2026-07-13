// Package conversations maintains an index of the conversations each
// adapter's tool has stored. It maps (adapter, slug) to conversation
// metadata, enabling URL resolution for dead conversations and (future)
// fulltext search.
//
// The index is populated and kept current by adapter ConversationSources
// (snapshot at startup, incremental thereafter), which emit opaque
// conversation refs; the index resolves each ref to metadata via the
// owning adapter's DescribeConversation and never interprets the ref
// itself (for file-backed adapters it happens to be a path — that is the
// adapter's detail). It never writes to the session store.
package conversations

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
)

// Info holds metadata for a single stored conversation.
type Info struct {
	ConversationID string    // adapter-native conversation ID (typically a UUID)
	Key            string    // internal lookup key, unique within (adapter)
	Slug           string    // human-readable URL identifier; empty until titled
	Adapter        string    // adapter name (claude, codex, pi, shell)
	Title          string    // display title
	Cwd            string    // working directory
	Ref            string    // opaque adapter-scoped conversation ref (a file path for file-backed adapters)
	ResumeCommand  []string  // command to resume this conversation
	Created        time.Time // when the conversation started
	LastActivity   time.Time // adapter-reported most recent activity (zero when unknown)
}

// Index is a concurrency-safe lookup table for stored conversations.
// It is the authority on internal-key uniqueness: when two conversations
// produce the same key within the same adapter, the index assigns
// -2, -3 suffixes.
type Index struct {
	mu sync.RWMutex
	// byKey maps "adapter/key" → Info.
	byKey map[string]Info
	// byConversationID maps "adapter/conversationID" → internal key for reverse lookup.
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

// Lookup returns the conversation info for an (adapter, key) pair.
// Returns ok=false if no matching conversation exists.
func (idx *Index) Lookup(adapterName, key string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	if info, ok := idx.byKey[indexKey(adapterName, key)]; ok {
		return info, true
	}
	// A conversation ID (or its old untitled fallback key) keeps resolving
	// after the key upgraded to a titled slug — deep links must not break.
	if k, ok := idx.byConversationID[convKey(adapterName, key)]; ok {
		info, ok := idx.byKey[indexKey(adapterName, k)]
		return info, ok
	}
	return Info{}, false
}

// LookupByConversationID returns the internal key for a conversation identified
// by its agent-native conversation ID. Returns empty string if unknown.
func (idx *Index) LookupByConversationID(adapterName, conversationID string) string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.byConversationID[convKey(adapterName, conversationID)]
}

// Upsert adds or updates a conversation in the index. If the internal key
// collides with an existing entry of the same adapter (but different
// conversation ID), a -2, -3, ... suffix is appended. Returns the final
// (possibly suffixed) key.
func (idx *Index) Upsert(info Info) string {
	idx.mu.Lock()
	defer idx.mu.Unlock()

	// Callers that construct Info directly historically supplied only Slug.
	// Keep that shorthand for titled conversations.
	if info.Key == "" {
		info.Key = info.Slug
	}

	// If this conversation ID already has a key, update in place — but a
	// TITLED conversation must stay reachable through its displayed slug
	// (keys and display slugs are otherwise separate, ADR 0024 §5). So the
	// key follows the slug: it upgrades from the untitled UUID fallback
	// when a title first arrives, and re-keys again on rename — the same
	// slug-follows-rename semantics session URLs have (#348). Conversation-
	// ID deep links keep resolving via the fallbacks in Lookup/FindByPrefix.
	// A transiently empty slug (a parse hiccup) keeps the existing key.
	tk := convKey(info.Adapter, info.ConversationID)
	if existing, ok := idx.byConversationID[tk]; ok {
		if info.Slug != "" && existing != info.Slug {
			delete(idx.byKey, indexKey(info.Adapter, existing))
			info.Key = idx.uniqueSlugLocked(info.Adapter, info.Slug, info.ConversationID)
			idx.byKey[indexKey(info.Adapter, info.Key)] = info
			idx.byConversationID[tk] = info.Key
			return info.Key
		}
		ik := indexKey(info.Adapter, existing)
		info.Key = existing
		idx.byKey[ik] = info
		return existing
	}

	// Assign a unique internal key.
	info.Key = idx.uniqueSlugLocked(info.Adapter, info.Key, info.ConversationID)
	ik := indexKey(info.Adapter, info.Key)
	idx.byKey[ik] = info
	idx.byConversationID[tk] = info.Key
	return info.Key
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

// Scan indexes a single conversation ref (snapshot or live update from an
// adapter ConversationSource), resolving it to metadata via the owning
// adapter's DescribeConversation. Returns the assigned slug.
func (idx *Index) Scan(a adapter.Adapter, ref string) string {
	desc, ok := a.(adapter.ConversationDescriber)
	if !ok {
		return ""
	}

	convInfo, err := desc.DescribeConversation(ref)
	if err != nil {
		return ""
	}

	if convInfo.Cwd == "" {
		return ""
	}

	displaySlug := convInfo.Slug
	key := displaySlug
	if key == "" {
		// Untitled conversations still need an internal unique lookup key
		// for UUID deep links, but that fallback must not surface as a URL slug.
		key = adapter.Slugify(convInfo.ID)
	}

	var cmd []string
	if resumer, ok := a.(adapter.Resumer); ok {
		if !resumer.CanResume(ref) {
			return ""
		}
		cmd = resumer.ResumeCommand(convInfo)
	}

	info := Info{
		ConversationID: convInfo.ID,
		Key:            key,
		Slug:           displaySlug,
		Adapter:        a.Name(),
		Title:          convInfo.Title,
		Cwd:            convInfo.Cwd,
		Ref:            ref,
		ResumeCommand:  cmd,
		Created:        convInfo.Created,
		LastActivity:   convInfo.LastActivity,
	}
	return idx.Upsert(info)
}

// Remove deletes a conversation from the index by conversation ID.
// Returns true if it was present.
func (idx *Index) Remove(adapterName, conversationID string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	tk := convKey(adapterName, conversationID)
	key, ok := idx.byConversationID[tk]
	if !ok {
		return false
	}
	delete(idx.byConversationID, tk)
	delete(idx.byKey, indexKey(adapterName, key))
	return true
}

// RemoveByRef deletes the conversation whose (Adapter, Ref) matches.
// Used when a ConversationSource observes a removal event and we don't have
// the (adapter, conversationID) handy. Refs are only unique within an
// adapter (ADR 0022: opaque, adapter-scoped), so the match is scoped to the
// reporting adapter — two adapters may legitimately use the same ref string.
// Linear walk over the index; that's fine because Remove events are rare
// (manual `rm`, file rotation) and the index size stays in the
// hundreds-to-low-thousands range. Returns true if an entry was removed.
//
// Session retirement on conversation-gone deliberately does NOT hang off
// this method: an unindexed conversation (describe failure,
// CanResume=false, empty cwd) still needs retiring when it disappears, so
// the source-level sink (sources.go) owns that signal instead.
func (idx *Index) RemoveByRef(adapterName, ref string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for key, info := range idx.byKey {
		if info.Adapter != adapterName || info.Ref != ref {
			continue
		}
		delete(idx.byKey, key)
		delete(idx.byConversationID, convKey(info.Adapter, info.ConversationID))
		return true
	}
	return false
}

// SlugExists reports whether an internal key is taken within an adapter.
func (idx *Index) SlugExists(adapterName, key string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	_, ok := idx.byKey[indexKey(adapterName, key)]
	return ok
}

// LookupBySlug searches for a conversation by internal key across all kinds.
// Returns the first match. Used when the caller doesn't know the adapter
// (e.g., project session arrays that store bare legacy slugs or UUID keys).
func (idx *Index) LookupBySlug(lookupKey string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for indexedKey, info := range idx.byKey {
		// indexedKey is "adapter/internal-key"; check its suffix.
		if i := len(indexedKey) - len(lookupKey); i > 0 && indexedKey[i-1] == '/' && indexedKey[i:] == lookupKey {
			return info, true
		}
	}
	return Info{}, false
}

// FindByPrefix returns conversations whose internal key starts with the given
// prefix, within an adapter. Used for URL resolution when the frontend
// provides a partial slug (e.g. an abbreviated or legacy session-id
// prefix); an exact/full id is just the degenerate prefix case.
func (idx *Index) FindByPrefix(adapterName, prefix string) (Info, bool) {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	keyPrefix := adapterName + "/" + prefix
	for key, info := range idx.byKey {
		if strings.HasPrefix(key, keyPrefix) {
			return info, true
		}
	}
	// Conversation-ID prefixes keep resolving after a titled-key upgrade.
	for ck, key := range idx.byConversationID {
		if strings.HasPrefix(ck, keyPrefix) {
			if info, ok := idx.byKey[indexKey(adapterName, key)]; ok {
				return info, true
			}
		}
	}
	return Info{}, false
}
