// Package sessionstream defines protocol 3's bounded, semantic session-list
// framing. A sender emits begin, one or more batches of complete rows, then
// ready. Receivers stage rows and replace their visible projection only at
// ready.
package sessionstream

import (
	"encoding/json"
	"errors"
	"fmt"
)

const (
	ProtocolVersion = 3

	EventBegin = "snapshot.sessions.begin"
	EventBatch = "snapshot.sessions.batch"
	EventReady = "snapshot.sessions.ready"
	EventError = "snapshot.sessions.error"

	// MaxEventPayload is deliberately below bufio.Scanner's 64 KiB default.
	// The 48 KiB budget leaves 16 KiB for the SSE "data: " prefix, event line,
	// proxies which add metadata, and modest future envelope growth. Rows are
	// never split: a row which cannot fit in one batch is rejected.
	MaxEventPayload = 48 * 1024

	// MaxStagedRows and MaxStagedBytes bound a receiver's unpublished staging
	// memory even when the sender is faulty or hostile.
	MaxStagedRows  = 100_000
	MaxStagedBytes = 64 * 1024 * 1024
)

var ErrRowTooLarge = errors.New("session stream row too large")

type Event struct {
	Type string
	Data []byte
}

type Begin struct {
	Version int    `json:"version"`
	Epoch   uint64 `json:"epoch"`
}

type Batch[T any] struct {
	Epoch    uint64 `json:"epoch"`
	Sessions []T    `json:"sessions"`
}

type Ready struct {
	Epoch uint64 `json:"epoch"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Encode builds a complete replacement transaction. Every batch contains
// whole semantic rows and every event payload is at most MaxEventPayload.
func Encode[T any](epoch uint64, rows []T, rowID func(T) string) ([]Event, error) {
	begin, err := marshalBounded(EventBegin, Begin{Version: ProtocolVersion, Epoch: epoch})
	if err != nil {
		return nil, err
	}
	ready, err := marshalBounded(EventReady, Ready{Epoch: epoch})
	if err != nil {
		return nil, err
	}

	events := []Event{begin}
	prefix := []byte(fmt.Sprintf(`{"epoch":%d,"sessions":[`, epoch))
	suffix := []byte("]}")
	newBatch := func() []byte {
		batch := make([]byte, len(prefix), MaxEventPayload)
		copy(batch, prefix)
		return batch
	}
	batch := newBatch()
	batchRows := 0
	flush := func() {
		data := append(batch, suffix...)
		events = append(events, Event{Type: EventBatch, Data: data})
		batch = newBatch()
		batchRows = 0
	}
	for _, row := range rows {
		encoded, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			return nil, fmt.Errorf("session stream: encode row %q: %w", rowID(row), marshalErr)
		}
		separator := 0
		if batchRows > 0 {
			separator = 1
		}
		if len(batch)+separator+len(encoded)+len(suffix) > MaxEventPayload {
			if batchRows == 0 {
				return nil, fmt.Errorf("%w: session %q cannot fit the %d-byte event payload limit (large command, cwd, remotes, title, subtitle, socket_path, or conversation_file fields can cause this)", ErrRowTooLarge, rowID(row), MaxEventPayload)
			}
			flush()
		}
		if batchRows > 0 {
			batch = append(batch, ',')
		}
		batch = append(batch, encoded...)
		batchRows++
	}
	if batchRows > 0 {
		flush()
	}
	return append(events, ready), nil
}

func ErrorEvent(err error) Event {
	message := err.Error()
	// A malformed/unbounded ID can be part of the diagnostic. Keep the error
	// frame bounded independently of the rejected row.
	const maxMessageBytes = 4 * 1024
	if len(message) > maxMessageBytes {
		message = message[:maxMessageBytes] + "…"
	}
	data, _ := json.Marshal(Error{Code: "session_row_too_large", Message: message})
	return Event{Type: EventError, Data: data}
}

func marshalBounded(eventType string, value any) (Event, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Event{}, err
	}
	if len(data) > MaxEventPayload {
		return Event{}, fmt.Errorf("session stream: %s payload is %d bytes (limit %d)", eventType, len(data), MaxEventPayload)
	}
	return Event{Type: eventType, Data: data}, nil
}
