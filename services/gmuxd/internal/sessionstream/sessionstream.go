// Package sessionstream defines protocol 3's bounded, semantic session-list
// framing. A sender emits begin, bounded batches of complete rows, optional
// bounded diagnostics for quarantined rows, then ready. Receivers expose only
// the rows committed by ready.
package sessionstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	ProtocolVersion = 3

	EventBegin = "snapshot.sessions.begin"
	EventBatch = "snapshot.sessions.batch"
	EventReady = "snapshot.sessions.ready"
	EventError = "snapshot.sessions.error"

	// MaxEventPayload is deliberately below bufio.Scanner's 64 KiB default.
	// The 48 KiB budget leaves 16 KiB for the SSE prefix, event line, proxies,
	// and modest future envelope growth. Rows are never split.
	MaxEventPayload = 48 * 1024

	// MaxStagedRows and MaxStagedBytes are shared sender/receiver transaction
	// bounds. The sender reserves aggregateEnvelopeReserve for batch envelopes,
	// so a correct transaction always fits the receiver's exact encoded-byte
	// accounting.
	MaxStagedRows            = 100_000
	MaxStagedBytes           = 64 * 1024 * 1024
	aggregateEnvelopeReserve = 1024 * 1024
	maxDiagnostics           = 256
	maxDiagnosticIdentity    = 128
)

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

// Error is a non-fatal diagnostic. The named row was omitted, but the epoch
// remains valid and ready commits every other row.
type Error struct {
	Epoch   uint64 `json:"epoch"`
	Code    string `json:"code"`
	ID      string `json:"id,omitempty"`
	Message string `json:"message"`
	Count   int    `json:"count,omitempty"`
}

// Encode builds a complete replacement transaction. Every batch contains
// whole semantic rows and every event payload is at most MaxEventPayload.
// Rows which cannot safely participate are quarantined with bounded diagnostic
// events; they never prevent ready from publishing the remaining projection.
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
	acceptedRows := 0
	acceptedJSONBytes := 0
	diagnostics := 0
	suppressedDiagnostics := 0
	flush := func() {
		data := append(batch, suffix...)
		events = append(events, Event{Type: EventBatch, Data: data})
		batch = newBatch()
		batchRows = 0
	}
	addDiagnostic := func(id, code, message string) {
		if diagnostics >= maxDiagnostics {
			suppressedDiagnostics++
			return
		}
		event, marshalErr := marshalBounded(EventError, Error{Epoch: epoch, Code: code, ID: safeIdentity(id), Message: message, Count: 1})
		if marshalErr == nil { // fixed-size fields make this defensive only
			events = append(events, event)
			diagnostics++
		}
	}

	for _, row := range rows {
		id := rowID(row)
		encoded, marshalErr := json.Marshal(row)
		if marshalErr != nil {
			addDiagnostic(id, "row_encode_failed", "session row could not be encoded and was omitted")
			continue
		}
		if len(prefix)+len(encoded)+len(suffix) > MaxEventPayload {
			addDiagnostic(id, "row_too_large", fmt.Sprintf("session row encoded to %d bytes and exceeds the %d-byte event limit; row omitted", len(encoded), MaxEventPayload))
			continue
		}
		if transactionWouldOverflow(acceptedRows, acceptedJSONBytes, len(encoded)) {
			addDiagnostic(id, "transaction_limit", "session row exceeds the bounded transaction row/byte limit and was omitted")
			continue
		}
		separator := 0
		if batchRows > 0 {
			separator = 1
		}
		if len(batch)+separator+len(encoded)+len(suffix) > MaxEventPayload {
			flush()
		}
		if batchRows > 0 {
			batch = append(batch, ',')
		}
		batch = append(batch, encoded...)
		batchRows++
		acceptedRows++
		acceptedJSONBytes += len(encoded)
	}
	if batchRows > 0 {
		flush()
	}
	if suppressedDiagnostics > 0 {
		event, marshalErr := marshalBounded(EventError, Error{Epoch: epoch, Code: "diagnostics_suppressed", Message: fmt.Sprintf("%d additional omitted session rows were not individually reported", suppressedDiagnostics), Count: suppressedDiagnostics})
		if marshalErr == nil {
			events = append(events, event)
		}
	}
	return append(events, ready), nil
}

func transactionWouldOverflow(rows, encodedBytes, nextRowBytes int) bool {
	return rows >= MaxStagedRows || encodedBytes+nextRowBytes > MaxStagedBytes-aggregateEnvelopeReserve
}

func safeIdentity(id string) string {
	if len(id) <= maxDiagnosticIdentity {
		return id
	}
	digest := sha256.Sum256([]byte(id))
	return "sha256:" + hex.EncodeToString(digest[:])
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
