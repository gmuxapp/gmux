package main

import (
	"testing"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/ntfy"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
)

type recordingExternalNotifier struct {
	messages chan ntfy.Message
}

func (n *recordingExternalNotifier) Notify(message ntfy.Message) bool {
	n.messages <- message
	return true
}

func TestCentralNotifierSendsExternalWithoutBrowserTarget(t *testing.T) {
	external := &recordingExternalNotifier{messages: make(chan ntfy.Message, 1)}
	router := newCentralNotifyRouter(presence.New(presence.Callbacks{}), notifyConfig{GracePeriod: time.Hour, IdleThreshold: time.Minute})
	router.external = external
	router.scheduleNotification("session-1", "finished", "private prompt title", "private browser body", "pi")

	router.firePending("session-1")

	select {
	case got := <-external.messages:
		if got.Kind != ntfy.KindFinished || got.SessionID != "session-1" || got.Adapter != "pi" {
			t.Fatalf("external message = %+v", got)
		}
	default:
		t.Fatal("external notification was coupled to browser target")
	}
}

func TestCentralNotifierSendsCoalescedExternalWithoutBrowserTarget(t *testing.T) {
	external := &recordingExternalNotifier{messages: make(chan ntfy.Message, 1)}
	router := newCentralNotifyRouter(presence.New(presence.Callbacks{}), notifyConfig{GracePeriod: time.Hour, IdleThreshold: time.Minute})
	router.external = external
	for _, id := range []string{"one", "two", "three"} {
		router.scheduleNotification(id, "finished", "private", "private", "pi")
	}

	router.firePending("one")

	select {
	case got := <-external.messages:
		if got.Kind != ntfy.KindCoalesced || got.Count != 3 || got.SessionID != "" || got.Adapter != "" {
			t.Fatalf("external message = %+v", got)
		}
	default:
		t.Fatal("coalesced external notification was not sent")
	}
}
