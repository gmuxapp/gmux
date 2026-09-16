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

// PROTO (3.0, round 2): scoped payloads ride INSIDE the delta event. One
// epoch is one SSE event is one atomic apply on the client: a root's
// descendant_counts (in upsert) and its open children window (in scopes)
// change in the same store transaction, never one frame apart. Scopes are
// keyed by the client-facing scope name, emitted in sorted order.
//
// A scope entry is either a payload (rows whose current value changed inside
// the viewport + ids that left it, and the listing total) or {reset:true}: the
// daemon could not honor that scope's chain (evicted, registered after the
// connection's last delta but before its baseline, or it did not fit next to
// the world delta) and the client must re-fetch the pages it holds. A world
// delta that does not fit by itself still falls back to a full transaction.
type ScopePayload[T any] struct {
	FromEpoch uint64   `json:"from_epoch,omitempty"`
	Total     int      `json:"total,omitempty"`
	Upsert    []T      `json:"upsert,omitempty"`
	Remove    []string `json:"remove,omitempty"`
	Reset     bool     `json:"reset,omitempty"`
}

type deltaWithScopes[T any] struct {
	Delta[T]
	Scopes map[string]ScopePayload[T] `json:"scopes,omitempty"`
}

// EncodeDeltaWithScopes marshals one delta carrying scope payloads. When the
// whole event exceeds MaxEventPayload, scope payloads are downgraded to
// {reset:true} largest-first until it fits; the returned resets names the
// scopes that were downgraded. ok is false only when the world part alone
// does not fit (the caller then sends a full transaction).
func EncodeDeltaWithScopes[T any](bootID string, fromEpoch, epoch uint64, upsert []T, remove []string, scopes map[string]ScopePayload[T]) (event Event, ok bool, resets []string, err error) {
	if upsert == nil {
		upsert = []T{}
	}
	if remove == nil {
		remove = []string{}
	}
	body := deltaWithScopes[T]{Delta: Delta[T]{Version: ProtocolVersion, BootID: bootID, Epoch: epoch, FromEpoch: fromEpoch, Upsert: upsert, Remove: remove}}
	if len(scopes) > 0 {
		body.Scopes = make(map[string]ScopePayload[T], len(scopes))
		for k, v := range scopes {
			body.Scopes[k] = v
		}
	}
	data, err := json.Marshal(body)
	if err != nil {
		return Event{}, false, nil, err
	}
	for len(data) > MaxEventPayload {
		// Downgrade the largest non-reset scope; if none is left, the world
		// part itself is too big.
		largest, size := "", 0
		for k, v := range body.Scopes {
			if v.Reset {
				continue
			}
			b, _ := json.Marshal(v)
			if len(b) > size {
				largest, size = k, len(b)
			}
		}
		if largest == "" {
			return Event{}, false, nil, nil
		}
		body.Scopes[largest] = ScopePayload[T]{Reset: true}
		resets = append(resets, largest)
		if data, err = json.Marshal(body); err != nil {
			return Event{}, false, nil, err
		}
	}
	return Event{Type: EventDelta, Data: data}, true, resets, nil
}
