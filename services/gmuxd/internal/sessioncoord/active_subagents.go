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
	Depth         int
	Active, Limit int
}

func (e *SubagentLimitError) Error() string {
	return fmt.Sprintf("%v at depth %d for root %s: %d of %d autonomous subagents", ErrSubagentLimitReached, e.Depth, e.Root, e.Active, e.Limit)
}
func (e *SubagentLimitError) Unwrap() error { return ErrSubagentLimitReached }

// ActiveSubagentReservation is an admission receipt. Root is empty for a
// top-level/orphan launch, which starts a root and therefore consumes no
// descendant slot.
type ActiveSubagentReservation struct {
	Token         string
	Root          centralstore.SessionID
	Depth         int
	Active, Limit int
	ExpiresAt     time.Time
}

type activeSubagentNode struct {
	parent    centralstore.SessionID
	hasParent bool
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
type activeSubagentPlacement struct {
	root  centralstore.SessionID
	depth int
}

type activeSubagentCountKey struct {
	root  centralstore.SessionID
	depth int
}

type activeSubagentBudget struct {
	limits        []int
	disabled      bool
	semantic      func(string) bool
	nodes         map[centralstore.SessionID]activeSubagentNode
	children      map[centralstore.SessionID]map[centralstore.SessionID]struct{}
	placements    map[centralstore.SessionID]activeSubagentPlacement
	activeByDepth map[activeSubagentCountKey]int
	launches      map[string]activeSubagentLaunch
	now           func() time.Time
}

func newActiveSubagentBudget(limits []int, disabled bool, semantic func(string) bool, rows []centralstore.Session) *activeSubagentBudget {
	b := &activeSubagentBudget{
		limits: append([]int(nil), limits...), disabled: disabled, semantic: semantic,
		nodes:         make(map[centralstore.SessionID]activeSubagentNode, len(rows)),
		children:      make(map[centralstore.SessionID]map[centralstore.SessionID]struct{}),
		placements:    make(map[centralstore.SessionID]activeSubagentPlacement, len(rows)),
		activeByDepth: make(map[activeSubagentCountKey]int),
		launches:      make(map[string]activeSubagentLaunch), now: time.Now,
	}
	if b.semantic == nil {
		b.semantic = func(string) bool { return false }
	}
	for _, row := range rows {
		n := activeSubagentNode{semantic: b.semantic(row.Adapter)}
		if row.ParentSessionID != nil {
			n.parent, n.hasParent = *row.ParentSessionID, true
			b.addChild(n.parent, row.ID)
		}
		b.nodes[row.ID] = n
	}
	for id := range b.nodes {
		b.placements[id] = b.resolvePlacement(id)
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
		if !n.hasParent {
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

func (b *activeSubagentBudget) resolvePlacement(start centralstore.SessionID) activeSubagentPlacement {
	root := b.resolveRoot(start)
	if root == "" {
		return activeSubagentPlacement{}
	}
	depth := 0
	cur := start
	seen := make(map[centralstore.SessionID]bool)
	for cur != root {
		if seen[cur] {
			return activeSubagentPlacement{root: root}
		}
		seen[cur] = true
		n, ok := b.nodes[cur]
		if !ok || !n.hasParent {
			return activeSubagentPlacement{root: root}
		}
		cur = n.parent
		depth++
	}
	return activeSubagentPlacement{root: root, depth: depth}
}

func (b *activeSubagentBudget) cap(depth int) int {
	if depth < 1 || depth > len(b.limits) {
		return 0
	}
	return b.limits[depth-1]
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
		p := b.placements[id]
		if n.live && n.semantic && p.root != "" && p.depth >= 1 {
			key := activeSubagentCountKey{root: p.root, depth: p.depth}
			b.activeByDepth[key]--
			if b.activeByDepth[key] == 0 {
				delete(b.activeByDepth, key)
			}
		}
	}
}
func (b *activeSubagentBudget) add(ids []centralstore.SessionID) {
	for _, id := range ids {
		p := b.resolvePlacement(id)
		b.placements[id] = p
		n := b.nodes[id]
		if n.live && n.semantic && p.root != "" && p.depth >= 1 {
			b.activeByDepth[activeSubagentCountKey{root: p.root, depth: p.depth}]++
		}
	}
}

func (b *activeSubagentBudget) upsert(row centralstore.Session, live bool) {
	affected := b.subtree(row.ID)
	b.subtract(affected)
	if old, ok := b.nodes[row.ID]; ok && old.hasParent {
		b.removeChild(old.parent, row.ID)
	}
	n := activeSubagentNode{semantic: b.semantic(row.Adapter), live: live}
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
	p := b.placements[id]
	key := activeSubagentCountKey{root: p.root, depth: p.depth}
	if n.live && n.semantic && p.root != "" && p.depth >= 1 {
		b.activeByDepth[key]--
		if b.activeByDepth[key] == 0 {
			delete(b.activeByDepth, key)
		}
	}
	n.live = live
	b.nodes[id] = n
	if n.live && n.semantic && p.root != "" && p.depth >= 1 {
		b.activeByDepth[key]++
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
	delete(b.placements, id)
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
func (b *activeSubagentBudget) launchPlacement(launch activeSubagentLaunch) activeSubagentPlacement {
	if !launch.hasParent {
		return activeSubagentPlacement{}
	}
	p, ok := b.placements[launch.parent]
	if !ok {
		return activeSubagentPlacement{}
	}
	return activeSubagentPlacement{root: p.root, depth: p.depth + 1}
}
func (b *activeSubagentBudget) reservedAt(want activeSubagentPlacement) int {
	n := 0
	for _, launch := range b.launches {
		if b.launchPlacement(launch) == want {
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
	placement := b.launchPlacement(launch)
	active, limit := 0, 0
	if placement.root != "" {
		limit = b.cap(placement.depth)
		active = b.activeByDepth[activeSubagentCountKey{root: placement.root, depth: placement.depth}] + b.reservedAt(placement)
		if b.disabled {
			limit = -1
		}
		if limit != -1 && active >= limit {
			return ActiveSubagentReservation{}, &SubagentLimitError{Root: placement.root, Depth: placement.depth, Active: active, Limit: limit}
		}
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ActiveSubagentReservation{}, err
	}
	token := hex.EncodeToString(raw[:])
	b.launches[token] = launch
	return ActiveSubagentReservation{Token: token, Root: placement.root, Depth: placement.depth, Active: active, Limit: limit, ExpiresAt: launch.expires}, nil
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
	if b.disabled {
		return nil
	}
	placement := b.launchPlacement(launch)
	if placement.root == "" {
		return nil
	}
	limit := b.cap(placement.depth)
	if limit == -1 {
		return nil
	}
	active := b.activeByDepth[activeSubagentCountKey{root: placement.root, depth: placement.depth}] + b.reservedAt(placement)
	if active > limit {
		return &SubagentLimitError{Root: placement.root, Depth: placement.depth, Active: active - 1, Limit: limit}
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
