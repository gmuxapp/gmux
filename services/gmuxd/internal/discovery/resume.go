package discovery

import (
	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/store"
)

// ResolveResumeCommand derives the resume command for a dead session from its
// authoritative conversation file (store.Session.ConversationFile, reported by the agent
// hook). Returns nil if the session has no recorded file or isn't resumable.
func ResolveResumeCommand(sess *store.Session) []string {
	if sess.ConversationFile == "" {
		return nil
	}
	a := adapters.FindByAdapter(sess.Adapter)
	if a == nil {
		return nil
	}
	filer, ok := a.(adapter.ConversationFiler)
	if !ok {
		return nil
	}
	resumer, ok := a.(adapter.Resumer)
	if !ok {
		return nil
	}
	info, err := filer.ParseConversationFile(sess.ConversationFile)
	if err != nil {
		return nil
	}
	return resumer.ResumeCommand(info)
}
