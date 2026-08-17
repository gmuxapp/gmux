package sessioncoord

import (
	"context"
	"fmt"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// SetSessionParent mutates the organizational parent edge while holding the
// lifecycle mutex. Dismiss holds the same mutex from its subtree/liveness
// check through DismissSessionTree, so a reparent cannot change the checked
// subtree before the dismissal transaction commits.
//
// The central store remains the sole owner of transaction, cycle-validation,
// version, and sibling-order semantics.
func (c *Coordinator) SetSessionParent(ctx context.Context, id centralstore.SessionID, parent *centralstore.SessionID) (centralstore.MutationResult, error) {
	if c.beforeReparentLock != nil {
		c.beforeReparentLock()
	}
	c.mu.Lock()
	reparenter, ok := c.durable.(interface {
		SetSessionParent(context.Context, centralstore.SessionID, *centralstore.SessionID) (centralstore.MutationResult, error)
	})
	if !ok {
		c.mu.Unlock()
		return centralstore.MutationResult{}, fmt.Errorf("sessioncoord: durable store does not support session reparenting")
	}
	result, err := reparenter.SetSessionParent(ctx, id, parent)
	if err == nil && c.activeSubagents != nil {
		c.activeSubagents.setParent(id, parent)
	}
	c.mu.Unlock()
	if err == nil {
		c.publish(ctx, result)
	}
	return result, err
}
