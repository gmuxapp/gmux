package centralstore

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore/internal/db"
)

// ManualPeer is one durable manually-added peer (ADR 0026 §1/§9). Token is a
// SECRET: it must never appear in logs or error messages, and any export or
// diagnostic path must go through Redacted. Raw database backups containing
// this table are secrets.
type ManualPeer struct {
	ID                   int64
	Version              RowVersion
	Name, URL            string
	Token, NodeID        string
	CreatedAt, UpdatedAt UnixMillis
}

// RedactedManualPeer is the export/diagnostics projection of a ManualPeer:
// the token value is elided and only its presence is reported. This is the
// seam `gmux daemon state export` (a later slice) must consume.
type RedactedManualPeer struct {
	Name, URL, NodeID    string
	TokenPresent         bool
	CreatedAt, UpdatedAt UnixMillis
}

// Redacted returns the secret-free projection of the peer.
func (p ManualPeer) Redacted() RedactedManualPeer {
	return RedactedManualPeer{
		Name: p.Name, URL: p.URL, NodeID: p.NodeID,
		TokenPresent: p.Token != "",
		CreatedAt:    p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
}

// ManualPeerSpec is the caller-supplied upsert input. Name is a display name
// (slugified and de-collided for genuinely new hosts; an existing match
// keeps its stored name). Empty Token/NodeID mean "unknown", never "clear":
// production parity with peerstore.AddOrGet, where a token-less re-add must
// not wipe a stored credential.
type ManualPeerSpec struct {
	Name, URL, Token, NodeID string
}

// PeerUpsertOutcome reports what UpsertManualPeer did.
type PeerUpsertOutcome int

const (
	PeerAdded PeerUpsertOutcome = iota
	PeerUpdated
	PeerUnchanged
)

// ErrPeerNotFound marks a mutation targeting a manual peer that does not
// exist.
var ErrPeerNotFound = errors.New("centralstore: manual peer not found")

var peerNonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugifyPeerName mirrors peerstore.Slugify: lowercase, non-slug runs become
// hyphens, trimmed. Returns "" when nothing usable remains.
func slugifyPeerName(s string) string {
	return strings.Trim(peerNonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// validatePeerURL mirrors peerstore.ValidateURL.
func validatePeerURL(u string) error {
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("centralstore: invalid peer url %q: %w", u, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("centralstore: peer url %q must use http or https", u)
	}
	if parsed.Host == "" {
		return fmt.Errorf("centralstore: peer url %q has no host", u)
	}
	return nil
}

// samePeerURL mirrors peerstore.sameURL: trailing-slash and case
// insensitive equality.
func samePeerURL(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}

func manualPeerFromDB(v db.ManualPeer) (ManualPeer, error) {
	if v.ID <= 0 || v.RowVersion < 1 || v.Name == "" || v.Url == "" || v.CreatedAtMs < 0 || v.UpdatedAtMs < 0 {
		return ManualPeer{}, errors.New("centralstore: corrupt manual peer value")
	}
	return ManualPeer{
		ID: v.ID, Version: RowVersion(v.RowVersion),
		Name: v.Name, URL: v.Url, Token: v.Token.String, NodeID: v.NodeID.String,
		CreatedAt: UnixMillis(v.CreatedAtMs), UpdatedAt: UnixMillis(v.UpdatedAtMs),
	}, nil
}

// ListManualPeers returns every durable manual peer, including tokens (the
// runtime peering reconciliation needs credentials). Callers must not log
// the result; use Redacted for anything user- or file-visible.
func (s *Store) ListManualPeers(ctx context.Context) ([]ManualPeer, error) {
	rows, err := s.queries.ListManualPeers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ManualPeer, 0, len(rows))
	for _, r := range rows {
		v, e := manualPeerFromDB(r)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}

// UpsertManualPeer is the durable half of "Connect to host" (production
// parity: peerstore.AddOrGet). One transaction matches an existing row by
// node identity first, then by URL; on a match it refreshes the URL always
// and token/node_id only when non-empty, keeping the stored display name.
// Otherwise the display name is slugified, de-collided against existing
// names, and a new row inserted. Peers ride the world payload, so any change
// reports WorldDirty.
func (s *Store) UpsertManualPeer(ctx context.Context, spec ManualPeerSpec, at UnixMillis) (ManualPeer, PeerUpsertOutcome, MutationResult, error) {
	if at < 0 {
		return ManualPeer{}, PeerAdded, MutationResult{}, errors.New("centralstore: peer timestamp must be non-negative")
	}
	if err := validatePeerURL(spec.URL); err != nil {
		return ManualPeer{}, PeerAdded, MutationResult{}, err
	}
	name := slugifyPeerName(spec.Name)
	if name == "" {
		return ManualPeer{}, PeerAdded, MutationResult{}, fmt.Errorf("centralstore: host name %q has no usable slug characters", spec.Name)
	}

	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return ManualPeer{}, PeerAdded, MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)

	rows, err := q.ListManualPeers(ctx)
	if err != nil {
		return ManualPeer{}, PeerAdded, MutationResult{}, err
	}
	peers := make([]ManualPeer, 0, len(rows))
	for _, r := range rows {
		v, e := manualPeerFromDB(r)
		if e != nil {
			return ManualPeer{}, PeerAdded, MutationResult{}, e
		}
		peers = append(peers, v)
	}

	// Match by durable node identity first, then by URL, under the same
	// transaction as the write (no check-then-act seam).
	matchIdx := -1
	if spec.NodeID != "" {
		for i, p := range peers {
			if p.NodeID == spec.NodeID {
				matchIdx = i
				break
			}
		}
	}
	if matchIdx == -1 {
		for i, p := range peers {
			if samePeerURL(p.URL, spec.URL) {
				matchIdx = i
				break
			}
		}
	}

	if matchIdx >= 0 {
		cur := peers[matchIdx]
		next := cur
		next.URL = spec.URL
		if spec.Token != "" {
			next.Token = spec.Token
		}
		if spec.NodeID != "" {
			next.NodeID = spec.NodeID
		}
		if next == cur {
			if err = tx.Commit(); err != nil {
				return ManualPeer{}, PeerAdded, MutationResult{}, err
			}
			return cur, PeerUnchanged, MutationResult{}, nil
		}
		n, updateErr := q.UpdateManualPeer(ctx, db.UpdateManualPeerParams{
			Url: next.URL, Token: nullString(next.Token), NodeID: nullString(next.NodeID),
			UpdatedAtMs: int64(at), ID: cur.ID, RowVersion: int64(cur.Version),
		})
		if updateErr != nil {
			// SQLite errors carry constraint names, never bound values; the
			// token cannot leak through this wrap.
			return ManualPeer{}, PeerAdded, MutationResult{}, fmt.Errorf("centralstore: update manual peer %q: %w", cur.Name, updateErr)
		}
		if n != 1 {
			return ManualPeer{}, PeerAdded, MutationResult{}, ErrStaleVersion
		}
		if err = tx.Commit(); err != nil {
			return ManualPeer{}, PeerAdded, MutationResult{}, err
		}
		next.Version = cur.Version + 1
		next.UpdatedAt = at
		return next, PeerUpdated, MutationResult{Changed: true, WorldDirty: true}, nil
	}

	taken := make(map[string]bool, len(peers))
	for _, p := range peers {
		taken[p.Name] = true
	}
	final := name
	for i := 2; taken[final]; i++ {
		final = fmt.Sprintf("%s-%d", name, i)
	}
	row, err := q.InsertManualPeer(ctx, db.InsertManualPeerParams{
		Name: final, Url: spec.URL, Token: nullString(spec.Token), NodeID: nullString(spec.NodeID),
		CreatedAtMs: int64(at), UpdatedAtMs: int64(at),
	})
	if err != nil {
		return ManualPeer{}, PeerAdded, MutationResult{}, fmt.Errorf("centralstore: insert manual peer %q: %w", final, err)
	}
	out, err := manualPeerFromDB(row)
	if err != nil {
		return ManualPeer{}, PeerAdded, MutationResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return ManualPeer{}, PeerAdded, MutationResult{}, err
	}
	return out, PeerAdded, MutationResult{Changed: true, WorldDirty: true}, nil
}

// RemoveManualPeer deletes a manual peer by name. It runs inside an
// explicit transaction like every other mutation in this package (sql
// review M-01): a single DELETE is atomic anyway, but the uniform shape
// keeps a future expansion (e.g. pruning peer-related state alongside)
// from silently losing atomicity.
func (s *Store) RemoveManualPeer(ctx context.Context, name string) (MutationResult, error) {
	if name == "" {
		return MutationResult{}, errors.New("centralstore: peer name required")
	}
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return MutationResult{}, err
	}
	defer tx.Rollback()
	q := s.queries.WithTx(tx)
	n, err := q.DeleteManualPeerByName(ctx, name)
	if err != nil {
		return MutationResult{}, err
	}
	if n == 0 {
		return MutationResult{}, ErrPeerNotFound
	}
	if err = tx.Commit(); err != nil {
		return MutationResult{}, err
	}
	return MutationResult{Changed: true, WorldDirty: true}, nil
}

