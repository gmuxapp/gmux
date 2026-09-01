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
	"sync/atomic"
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
	// resumeByRef caches the adapter-derived resume command while the
	// conversation source is indexing the ref. Rendering the session list is
	// a hot read path and must not re-read every dead conversation transcript.
	resumeByRef map[string][]string
	// removeGen counts Remove/RemoveByRef events per (adapter, ref). Scan
	// snapshots it before its unlocked DescribeConversation and commits only
	// if it is unchanged, so a watcher-observed deletion always beats an
	// in-flight scan of the same ref — otherwise the scan's late Upsert would
	// resurrect the deleted conversation until restart (zombie). The map only
	// grows on removal events (manual rm, rotation), which are rare.
	removeGen map[string]uint64
	// snapshotDone flips to true once the initial full snapshot of every
	// adapter ConversationSource has finished (Snapshot or StartSnapshot).
	// Until then, a lookup miss means "not indexed yet", not "absent".
	snapshotDone atomic.Bool
}

// New creates an empty index.
func New() *Index {
	return &Index{
		byKey:            make(map[string]Info),
		byConversationID: make(map[string]string),
		resumeByRef:      make(map[string][]string),
		removeGen:        make(map[string]uint64),
	}
}

func indexKey(adapterName, slug string) string {
	return adapterName + "/" + slug
}

func convKey(adapterName, conversationID string) string {
	return adapterName + "/" + conversationID
}

func refKey(adapterName, ref string) string {
	return adapterName + "/" + ref
}

// LookupResumeCommand returns the command derived when the conversation ref
// was last observed by its source. The startup snapshot populates this cache
// progressively (it may still be running while the daemon serves — see
// SnapshotComplete), and source upserts keep it current. The returned slice
// is a copy so callers cannot mutate index state.
func (idx *Index) LookupResumeCommand(adapterName, ref string) []string {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return append([]string(nil), idx.resumeByRef[refKey(adapterName, ref)]...)
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
	return idx.upsertLocked(info)
}

// upsertLocked is Upsert's body; must be called with idx.mu held.
func (idx *Index) upsertLocked(info Info) string {
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
		if info.Slug != "" {
			// Dedup FIRST (with the old key still in place, so a refresh
			// that resolves to the same suffixed key is a no-op), then
			// re-key only on genuine change. Display slug = final key,
			// always: a collision-suffixed key must be the URL the row
			// advertises, or the unsuffixed URL resolves a DIFFERENT
			// conversation.
			final := idx.uniqueSlugLocked(info.Adapter, info.Slug, info.ConversationID)
			info.Key = final
			info.Slug = final
			if final != existing {
				delete(idx.byKey, indexKey(info.Adapter, existing))
				idx.byConversationID[tk] = final
			}
			idx.byKey[indexKey(info.Adapter, final)] = info
			return final
		}
		ik := indexKey(info.Adapter, existing)
		previous := idx.byKey[ik]
		info.Key = existing
		info.Slug = previous.Slug
		if info.Title == "" {
			info.Title = previous.Title
		}
		idx.byKey[ik] = info
		return existing
	}

	// Assign a unique internal key; a titled conversation's displayed slug
	// is the final deduped key, so URLs and display never diverge.
	info.Key = idx.uniqueSlugLocked(info.Adapter, info.Key, info.ConversationID)
	if info.Slug != "" {
		info.Slug = info.Key
	}
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

// SnapshotComplete reports whether the initial full snapshot across all
// adapter ConversationSources has finished. While false, a lookup miss is
// ambiguous: the conversation may simply not have been scanned yet, so
// callers should surface "index still loading" instead of a hard not-found.
func (idx *Index) SnapshotComplete() bool {
	return idx.snapshotDone.Load()
}

func (idx *Index) markSnapshotComplete() {
	idx.snapshotDone.Store(true)
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

	// Snapshot the ref's removal generation before the unlocked describe:
	// a Remove event that lands while the file is being read/parsed must
	// win over this scan's results (commitScanLocked re-checks it).
	gen := idx.refGeneration(a.Name(), ref)

	convInfo, err := desc.DescribeConversation(ref)
	if err != nil {
		// Keep stale-good state on transient descriptor failures, matching the
		// main metadata index. A source Remove event is the authoritative
		// signal that clears both entries.
		return ""
	}

	var cmd []string
	if resumer, ok := a.(adapter.Resumer); ok {
		cmd = resumer.ResumeCommand(convInfo)
	}

	addToIndex := convInfo.Cwd != ""
	if _, ok := a.(adapter.Resumer); ok && len(cmd) == 0 {
		// An empty command means the adapter considers this conversation
		// non-resumable (empty/corrupted), so it stays out of the index.
		addToIndex = false
	}

	displaySlug := convInfo.Slug
	key := displaySlug
	if key == "" {
		// Untitled conversations still need an internal unique lookup key
		// for UUID deep links, but that fallback must not surface as a URL slug.
		key = adapter.Slugify(convInfo.ID)
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

	idx.mu.Lock()
	defer idx.mu.Unlock()
	if idx.removeGen[refKey(a.Name(), ref)] != gen {
		// The ref was removed while this scan was describing it; the removal
		// is authoritative. Committing anyway would resurrect a deleted
		// conversation with no future event to clear it.
		return ""
	}
	// Cache the resume command before the index eligibility checks above
	// decided addToIndex. The wire command contract depends only on
	// ConversationDescriber + Resumer, while the URL index additionally
	// requires cwd/title metadata. Both writes happen under one lock hold so
	// a Remove can never land between them.
	idx.resumeByRef[refKey(a.Name(), ref)] = append([]string(nil), cmd...)
	if !addToIndex {
		return ""
	}
	return idx.upsertLocked(info)
}

// refGeneration returns the current removal generation for (adapter, ref).
func (idx *Index) refGeneration(adapterName, ref string) uint64 {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	return idx.removeGen[refKey(adapterName, ref)]
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
	if info, exists := idx.byKey[indexKey(adapterName, key)]; exists {
		delete(idx.resumeByRef, refKey(adapterName, info.Ref))
		idx.removeGen[refKey(adapterName, info.Ref)]++
	}
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
// non-resumable, empty cwd) still needs retiring when it disappears, so
// the source-level sink (sources.go) owns that signal instead.
func (idx *Index) RemoveByRef(adapterName, ref string) bool {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	// Bump the removal generation even when nothing is indexed yet: the
	// entry may be mid-scan (describe in flight), and that scan's commit
	// must observe this removal and drop its result.
	idx.removeGen[refKey(adapterName, ref)]++
	delete(idx.resumeByRef, refKey(adapterName, ref))
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
