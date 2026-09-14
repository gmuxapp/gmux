package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
)

// autoDismissInterval is how often the retention sweep re-runs after the
// startup pass. Rows only age in one direction, so a coarse cadence is
// enough; each pass is one ListSessions plus (usually) nothing.
const autoDismissInterval = 6 * time.Hour

// autoDismissPolicy maps host.toml into the coordinator policy. A zero
// auto_dismiss_days disables the sweep entirely. GMUXD_AUTO_DISMISS_DRY_RUN=1
// logs what a sweep would hide without committing — the first thing to run
// on a real corpus.
func autoDismissPolicy(cfg config.SessionsConfig) sessioncoord.AutoDismissPolicy {
	return sessioncoord.AutoDismissPolicy{
		MaxIdle:       time.Duration(cfg.AutoDismissDays) * 24 * time.Hour,
		IncludeUnread: cfg.AutoDismissUnread,
		DryRun:        os.Getenv("GMUXD_AUTO_DISMISS_DRY_RUN") == "1",
	}
}

// runAutoDismiss runs one sweep now (the caller starts it after the startup
// convergence window closed, so liveness is known) and then periodically
// until ctx ends. Errors are logged and the loop keeps going: retention is
// housekeeping and must never take the daemon down.
func runAutoDismiss(ctx context.Context, coord *sessioncoord.Coordinator, policy sessioncoord.AutoDismissPolicy) {
	if policy.MaxIdle <= 0 {
		return
	}
	sweep := func() {
		report, err := coord.AutoDismiss(ctx, policy)
		if err != nil {
			log.Printf("auto-dismiss: sweep failed: %v", err)
			return
		}
		mode := "committed"
		if policy.DryRun {
			mode = "dry-run"
		}
		log.Printf("auto-dismiss (%s): max_idle=%s include_unread=%t visible=%d candidates=%d dismissed=%d kept_unread=%d kept_ancestors=%d",
			mode, policy.MaxIdle, policy.IncludeUnread, report.Visible, len(report.Candidates), len(report.Dismissed), report.KeptUnread, report.KeptAncestors)
		if policy.DryRun && len(report.Candidates) > 0 {
			log.Printf("auto-dismiss (dry-run): would dismiss %v", report.Candidates)
		}
	}
	sweep()
	t := time.NewTicker(autoDismissInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			sweep()
		}
	}
}
