package sessioncoord

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// ErrSubagentLimitReached is the stable admission verdict for a launch whose
// current behavioral root has no free active-subagent slot.
var ErrSubagentLimitReached = errors.New("sessioncoord: active subagent limit reached")

var (
	// ErrActiveSubagentReservationInvalid marks an absent, expired, reused, or
	// parent-mismatched launch receipt. A runner presenting one must not be
	// registered as an unbudgeted fallback.
	ErrActiveSubagentReservationInvalid = errors.New("sessioncoord: invalid active-subagent launch reservation")
	errLaunchReservationNotFound        = fmt.Errorf("%w: not found, expired, or already claimed", ErrActiveSubagentReservationInvalid)
	errLaunchReservationMismatch        = fmt.Errorf("%w: parent does not match admission", ErrActiveSubagentReservationInvalid)
)

const activeSubagentReservationTTL = 2 * time.Minute

// SubagentLimitError carries the machine-readable facts behind
// ErrSubagentLimitReached.
type SubagentLimitError struct {
	Root          centralstore.SessionID
	Active, Limit int
}

func (e *SubagentLimitError) Error() string {
	return fmt.Sprintf("%v for root %s: %d of %d active subagents", ErrSubagentLimitReached, e.Root, e.Active, e.Limit)
}
func (e *SubagentLimitError) Unwrap() error { return ErrSubagentLimitReached }

// ActiveSubagentReservation is an admission receipt. Root is empty for a
// top-level/orphan launch, which starts a root and therefore consumes no
// descendant slot.
type ActiveSubagentReservation struct {
	Token         string
	Root          centralstore.SessionID
	Active, Limit int
	ExpiresAt     time.Time
}

type activeSubagentNode struct {
	parent    centralstore.SessionID
	hasParent bool
	promoted  bool
	semantic  bool
	live      bool
}

type activeSubagentLaunch struct {
	parent    centralstore.SessionID
	hasParent bool
	expires   time.Time
	claimedBy centralstore.SessionID
}

// activeSubagentBudget is guarded by Coordinator.mu. It is an incrementally
// maintained projection of durable mutable ownership plus runtime liveness.
// Durable rows provide parent/promotion/adapter facts; only installed local
// registry generations set live. Remote projections never enter this index.
type activeSubagentBudget struct {
	limit        int
	semantic     func(string) bool
	nodes        map[centralstore.SessionID]activeSubagentNode
	children     map[centralstore.SessionID]map[centralstore.SessionID]struct{}
	roots        map[centralstore.SessionID]centralstore.SessionID
	activeByRoot map[centralstore.SessionID]int
	launches     map[string]activeSubagentLaunch
	now          func() time.Time
}

func newActiveSubagentBudget(limit int, semantic func(string) bool, rows []centralstore.Session) *activeSubagentBudget {
	b := &activeSubagentBudget{
		limit: limit, semantic: semantic,
		nodes:        make(map[centralstore.SessionID]activeSubagentNode, len(rows)),
		children:     make(map[centralstore.SessionID]map[centralstore.SessionID]struct{}),
		roots:        make(map[centralstore.SessionID]centralstore.SessionID, len(rows)),
		activeByRoot: make(map[centralstore.SessionID]int),
		launches:     make(map[string]activeSubagentLaunch), now: time.Now,
	}
	if b.semantic == nil {
		b.semantic = func(string) bool { return false }
	}
	for _, row := range rows {
		n := activeSubagentNode{promoted: row.PromotedToRoot, semantic: b.semantic(row.Adapter)}
		if row.ParentSessionID != nil {
			n.parent, n.hasParent = *row.ParentSessionID, true
			b.addChild(n.parent, row.ID)
		}
		b.nodes[row.ID] = n
	}
	for id := range b.nodes {
		b.roots[id] = b.resolveRoot(id)
	}
	return b
}

func (b *activeSubagentBudget) addChild(parent, child centralstore.SessionID) {
	if b.children[parent] == nil {
		b.children[parent] = make(map[centralstore.SessionID]struct{})
	}
	b.children[parent][child] = struct{}{}
}
func (b *activeSubagentBudget) removeChild(parent, child centralstore.SessionID) {
	delete(b.children[parent], child)
	if len(b.children[parent]) == 0 {
		delete(b.children, parent)
	}
}

// resolveRoot follows current parent/promotion facts. Missing parents make the
// last present node a root, matching family presentation. Corrupt cycles are
// collapsed onto their lexicographically smallest member so resolution is
// bounded and deterministic instead of hanging or splitting one cycle across
// unrelated budgets.
func (b *activeSubagentBudget) resolveRoot(start centralstore.SessionID) centralstore.SessionID {
	path := make([]centralstore.SessionID, 0, 8)
	seen := make(map[centralstore.SessionID]int)
	cur := start
	for {
		n, ok := b.nodes[cur]
		if !ok {
			if len(path) == 0 {
				return ""
			}
			return path[len(path)-1]
		}
		if n.promoted || !n.hasParent {
			return cur
		}
		seen[cur] = len(path)
		path = append(path, cur)
		if at, cycle := seen[n.parent]; cycle {
			root := n.parent
			for _, id := range path[at:] {
				if id < root {
					root = id
				}
			}
			return root
		}
		if _, exists := b.nodes[n.parent]; !exists {
			return cur
		}
		cur = n.parent
	}
}

func (b *activeSubagentBudget) subtree(start centralstore.SessionID) []centralstore.SessionID {
	out := make([]centralstore.SessionID, 0, 1)
	seen := map[centralstore.SessionID]bool{}
	queue := []centralstore.SessionID{start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := b.nodes[id]; ok {
			out = append(out, id)
		}
		for child := range b.children[id] {
			queue = append(queue, child)
		}
	}
	return out
}

func (b *activeSubagentBudget) subtract(ids []centralstore.SessionID) {
	for _, id := range ids {
		n := b.nodes[id]
		root := b.roots[id]
		if n.live && n.semantic && root != "" && root != id {
			b.activeByRoot[root]--
			if b.activeByRoot[root] == 0 {
				delete(b.activeByRoot, root)
			}
		}
	}
}
func (b *activeSubagentBudget) add(ids []centralstore.SessionID) {
	for _, id := range ids {
		root := b.resolveRoot(id)
		b.roots[id] = root
		n := b.nodes[id]
		if n.live && n.semantic && root != "" && root != id {
			b.activeByRoot[root]++
		}
	}
}

func (b *activeSubagentBudget) upsert(row centralstore.Session, live bool) {
	affected := b.subtree(row.ID)
	b.subtract(affected)
	if old, ok := b.nodes[row.ID]; ok && old.hasParent {
		b.removeChild(old.parent, row.ID)
	}
	n := activeSubagentNode{promoted: row.PromotedToRoot, semantic: b.semantic(row.Adapter), live: live}
	if row.ParentSessionID != nil {
		n.parent, n.hasParent = *row.ParentSessionID, true
		b.addChild(n.parent, row.ID)
	}
	b.nodes[row.ID] = n
	if len(affected) == 0 {
		affected = []centralstore.SessionID{row.ID}
	}
	// A formerly missing parent may already have orphan children in the index.
	seen := make(map[centralstore.SessionID]bool, len(affected))
	for _, id := range affected {
		seen[id] = true
	}
	for _, id := range b.subtree(row.ID) {
		if !seen[id] {
			affected = append(affected, id)
			seen[id] = true
		}
	}
	b.add(affected)
}

func (b *activeSubagentBudget) setLive(id centralstore.SessionID, live bool) {
	n, ok := b.nodes[id]
	if !ok || n.live == live {
		return
	}
	root := b.roots[id]
	if n.live && n.semantic && root != "" && root != id {
		b.activeByRoot[root]--
		if b.activeByRoot[root] == 0 {
			delete(b.activeByRoot, root)
		}
	}
	n.live = live
	b.nodes[id] = n
	if n.live && n.semantic && root != "" && root != id {
		b.activeByRoot[root]++
	}
}

func (b *activeSubagentBudget) setParent(id centralstore.SessionID, parent *centralstore.SessionID) {
	n, ok := b.nodes[id]
	if !ok {
		return
	}
	affected := b.subtree(id)
	b.subtract(affected)
	if n.hasParent {
		b.removeChild(n.parent, id)
	}
	n.hasParent = parent != nil
	if parent != nil {
		n.parent = *parent
		b.addChild(*parent, id)
	} else {
		n.parent = ""
	}
	b.nodes[id] = n
	b.add(affected)
}

func (b *activeSubagentBudget) setPromotion(id centralstore.SessionID, promoted bool) {
	n, ok := b.nodes[id]
	if !ok {
		return
	}
	affected := b.subtree(id)
	b.subtract(affected)
	n.promoted = promoted
	b.nodes[id] = n
	b.add(affected)
}

func (b *activeSubagentBudget) remove(id centralstore.SessionID) {
	n, ok := b.nodes[id]
	if !ok {
		return
	}
	affected := b.subtree(id)
	b.subtract(affected)
	if n.hasParent {
		b.removeChild(n.parent, id)
	}
	for child := range b.children[id] {
		cn := b.nodes[child]
		cn.hasParent, cn.parent = false, ""
		b.nodes[child] = cn
	}
	delete(b.children, id)
	delete(b.nodes, id)
	delete(b.roots, id)
	var survivors []centralstore.SessionID
	for _, member := range affected {
		if member != id {
			survivors = append(survivors, member)
		}
	}
	b.add(survivors)
}

func (b *activeSubagentBudget) cleanupExpired(now time.Time) {
	for token, launch := range b.launches {
		if launch.claimedBy == "" && !now.Before(launch.expires) {
			delete(b.launches, token)
		}
	}
}
func (b *activeSubagentBudget) launchRoot(launch activeSubagentLaunch) centralstore.SessionID {
	if !launch.hasParent {
		return ""
	}
	if _, ok := b.nodes[launch.parent]; !ok {
		return ""
	}
	return b.roots[launch.parent]
}
func (b *activeSubagentBudget) reservedAt(root centralstore.SessionID) int {
	n := 0
	for _, launch := range b.launches {
		if b.launchRoot(launch) == root {
			n++
		}
	}
	return n
}

func (b *activeSubagentBudget) reserve(parent *centralstore.SessionID) (ActiveSubagentReservation, error) {
	now := b.now()
	b.cleanupExpired(now)
	launch := activeSubagentLaunch{expires: now.Add(activeSubagentReservationTTL)}
	if parent != nil {
		launch.parent, launch.hasParent = *parent, true
	}
	root := b.launchRoot(launch)
	active := 0
	if root != "" {
		active = b.activeByRoot[root] + b.reservedAt(root)
		if active >= b.limit {
			return ActiveSubagentReservation{}, &SubagentLimitError{Root: root, Active: active, Limit: b.limit}
		}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ActiveSubagentReservation{}, err
	}
	token := hex.EncodeToString(raw[:])
	b.launches[token] = launch
	return ActiveSubagentReservation{Token: token, Root: root, Active: active, Limit: b.limit, ExpiresAt: launch.expires}, nil
}

func (b *activeSubagentBudget) claim(token string, id centralstore.SessionID) (activeSubagentLaunch, error) {
	now := b.now()
	b.cleanupExpired(now)
	launch, ok := b.launches[token]
	if !ok || launch.claimedBy != "" || id == "" {
		return activeSubagentLaunch{}, errLaunchReservationNotFound
	}
	launch.claimedBy = id
	b.launches[token] = launch
	return launch, nil
}
func (b *activeSubagentBudget) validateParent(launch activeSubagentLaunch, parent *centralstore.SessionID) error {
	if launch.hasParent != (parent != nil) {
		return errLaunchReservationMismatch
	}
	if parent != nil && launch.parent != *parent {
		return errLaunchReservationMismatch
	}
	return nil
}

// validateClaimedBudget re-checks a claimed receipt against current mutable
// ownership immediately before the registration commit. reservedAt includes
// this receipt, so equality with limit is valid; anything greater means its
// parent moved into a root that was already full after admission.
func (b *activeSubagentBudget) validateClaimedBudget(launch activeSubagentLaunch) error {
	root := b.launchRoot(launch)
	if root == "" {
		return nil
	}
	active := b.activeByRoot[root] + b.reservedAt(root)
	if active > b.limit {
		return &SubagentLimitError{Root: root, Active: active - 1, Limit: b.limit}
	}
	return nil
}
func (b *activeSubagentBudget) release(token string, claimed bool) {
	launch, ok := b.launches[token]
	if !ok {
		return
	}
	if claimed && launch.claimedBy == "" {
		return
	}
	if !claimed && launch.claimedBy != "" {
		return
	}
	delete(b.launches, token)
}

// unclaim restores a receipt after a pre-commit registration failure so the
// runner's existing retry loop can present the same receipt again. The
// original expiry remains authoritative and the receipt keeps counting.
func (b *activeSubagentBudget) unclaim(token string) {
	launch, ok := b.launches[token]
	if !ok || launch.claimedBy == "" {
		return
	}
	launch.claimedBy = ""
	b.launches[token] = launch
}
func (b *activeSubagentBudget) hasLaunchFrom(members map[centralstore.SessionID]bool) bool {
	b.cleanupExpired(b.now())
	for _, launch := range b.launches {
		if launch.hasParent && members[launch.parent] {
			return true
		}
	}
	return false
}

// ReserveActiveSubagent atomically checks and reserves one gmux-mediated new
// semantic-agent launch against current mutable ownership.
func (c *Coordinator) ReserveActiveSubagent(_ context.Context, parent *centralstore.SessionID) (ActiveSubagentReservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return ActiveSubagentReservation{}, errors.New("sessioncoord: coordinator closed")
	}
	if c.activeSubagents == nil {
		return ActiveSubagentReservation{}, errors.New("sessioncoord: active-subagent budget is not configured")
	}
	return c.activeSubagents.reserve(parent)
}

// ReleaseActiveSubagentReservation cancels an unclaimed pre-launch receipt.
// It is idempotent; registration consumes claimed receipts itself.
func (c *Coordinator) ReleaseActiveSubagentReservation(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.activeSubagents != nil {
		c.activeSubagents.release(token, false)
	}
}
