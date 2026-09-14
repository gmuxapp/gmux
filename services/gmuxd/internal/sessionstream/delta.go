package sessionstream

import "encoding/json"

// SPIKE (R5/R6): epoch-chained deltas, additive to protocol 3.
//
// A delta is one self-contained SSE event: the rows whose *current* value the
// receiver does not have (upsert) plus the ids it must drop (remove), moving
// it from from_epoch to epoch. Because upserts always carry the row as of
// `epoch` (never a historical value), applying one delta is atomic and
// order-free within the event; there is no journal replay and no partial
// commit. Receivers that are not exactly at from_epoch must ignore it — the
// sender only emits a delta to a connection whose last delivered epoch it
// knows.
//
// A delta that would exceed MaxEventPayload is never split: the sender falls
// back to today's begin/batch/ready transaction, which is already bounded and
// already quarantines oversized rows.
const EventDelta = "snapshot.sessions.delta"

type Delta[T any] struct {
	Version   int      `json:"version"`
	BootID    string   `json:"boot_id"`
	Epoch     uint64   `json:"epoch"`
	FromEpoch uint64   `json:"from_epoch"`
	Upsert    []T      `json:"upsert"`
	Remove    []string `json:"remove"`
}

// EncodeDelta marshals one delta event. ok is false when the delta does not
// fit the per-event budget; the caller must then send a full transaction.
func EncodeDelta[T any](bootID string, fromEpoch, epoch uint64, upsert []T, remove []string) (Event, bool, error) {
	if upsert == nil {
		upsert = []T{}
	}
	if remove == nil {
		remove = []string{}
	}
	data, err := json.Marshal(Delta[T]{
		Version:   ProtocolVersion,
		BootID:    bootID,
		Epoch:     epoch,
		FromEpoch: fromEpoch,
		Upsert:    upsert,
		Remove:    remove,
	})
	if err != nil {
		return Event{}, false, err
	}
	if len(data) > MaxEventPayload {
		return Event{}, false, nil
	}
	return Event{Type: EventDelta, Data: data}, true, nil
}
