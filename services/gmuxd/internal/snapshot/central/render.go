package central

import (
	"context"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// RenderSessions reads the store and overlays runtime facts to produce a
// composed SessionsPayload. This is the same logic the async composer uses
// per pass, factored out so REST handlers can call it directly for
// store-direct reads (ADR 0026 §2a).
func RenderSessions(
	ctx context.Context,
	reader Reader,
	runtime RuntimeSource,
	verdicts VerdictSource,
) (*SessionsPayload, error) {
	var runtimeFacts map[centralstore.SessionID]RuntimeFacts
	var verdictMap map[centralstore.SessionID]ResumeVerdict
	if runtime != nil {
		runtimeFacts = runtime.RuntimeFacts()
	}
	if verdicts != nil {
		verdictMap = verdicts.ResumeVerdicts()
	}
	snap, err := reader.ReadSnapshot(ctx, centralstore.SnapshotQuery{
		IncludeSessions: true,
	})
	if err != nil {
		return nil, err
	}
	return composeSessions(snap.Sessions, runtimeFacts, verdictMap), nil
}

// RenderAll reads both sessions and projects from the store in a single
// transaction and overlays runtime/peer facts. Returns a Batch with both
// payloads populated.
func RenderAll(
	ctx context.Context,
	reader Reader,
	runtime RuntimeSource,
	verdicts VerdictSource,
	peers PeerSource,
) (Batch, error) {
	var runtimeFacts map[centralstore.SessionID]RuntimeFacts
	var verdictMap map[centralstore.SessionID]ResumeVerdict
	var peerWorld PeerWorld
	if runtime != nil {
		runtimeFacts = runtime.RuntimeFacts()
	}
	if verdicts != nil {
		verdictMap = verdicts.ResumeVerdicts()
	}
	if peers != nil {
		peerWorld = peers.PeerWorld()
	}
	snap, err := reader.ReadSnapshot(ctx, centralstore.SnapshotQuery{
		IncludeSessions: true,
		IncludeProjects: true,
	})
	if err != nil {
		return Batch{}, err
	}
	return Batch{
		Sessions: composeSessions(snap.Sessions, runtimeFacts, verdictMap),
		Projects: composeProjects(snap, peerWorld),
	}, nil
}

// composeSessions is the pure session overlay logic extracted from
// Composer.compose.
func composeSessions(
	views []centralstore.SessionView,
	runtime map[centralstore.SessionID]RuntimeFacts,
	verdicts map[centralstore.SessionID]ResumeVerdict,
) *SessionsPayload {
	rows := make([]SessionRow, 0, len(views))
	for _, v := range views {
		row := SessionRow{SessionView: v}
		if facts, live := runtime[v.ID]; live {
			row.Alive = true
			f := facts
			row.Runtime = &f
		} else {
			everRan := v.StartedAt != nil
			row.Resumable = everRan && len(v.Command) > 0 && verdicts[v.ID] != VerdictGone
			if !everRan {
				row.Unread = false
			}
		}
		rows = append(rows, row)
	}
	return &SessionsPayload{Sessions: rows}
}

// composeProjects is the pure projects overlay logic extracted from
// Composer.compose.
func composeProjects(
	snap centralstore.StoreSnapshot,
	peerWorld PeerWorld,
) *ProjectsPayload {
	joined := make([]LocalPeerPlacementRow, 0, len(snap.LocalPeerPlacements))
	for _, view := range snap.LocalPeerPlacements {
		if _, connected := peerWorld.LocalPeerSessions[LocalPeerSessionKey{PeerKey: view.PeerKey, SessionID: view.SessionID}]; !connected {
			continue
		}
		joined = append(joined, LocalPeerPlacementRow{LocalPeerPlacementView: view})
	}
	return &ProjectsPayload{
		Projects:            snap.Projects,
		LocalPeerPlacements: joined,
		Peers:               peerWorld.Peers,
		Health:              peerWorld.Health,
		Launchers:           peerWorld.Launchers,
		DefaultLauncher:     peerWorld.DefaultLauncher,
		PeerProjects:        peerWorld.PeerProjects,
		PeerDiscovered:      peerWorld.PeerDiscovered,
	}
}
