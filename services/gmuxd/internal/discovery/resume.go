package discovery

import (
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// ResolveResumeCommand derives the resume command for a dead session from its
// authoritative conversation ref (store.Session.ConversationRef, reported by
// the agent hook). Returns nil if the session has no recorded conversation
// or isn't resumable.
func ResolveResumeCommand(sess *store.Session) []string {
	if sess.ConversationRef == "" {
		return nil
	}
	a := adapters.FindByAdapter(sess.Adapter)
	if a == nil {
		return nil
	}
	desc, ok := a.(adapter.ConversationDescriber)
	if !ok {
		return nil
	}
	resumer, ok := a.(adapter.Resumer)
	if !ok {
		return nil
	}
	info, err := desc.DescribeConversation(sess.ConversationRef)
	if err != nil {
		return nil
	}
	return resumer.ResumeCommand(info)
}
