package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gmuxapp/gmux/packages/adapter"
	"github.com/gmuxapp/gmux/packages/adapter/adapters"
	"github.com/gmuxapp/gmux/packages/paths"
	"github.com/gmuxapp/gmux/packages/scrollback"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/authtoken"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/binhash"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/clipfile"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/config"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/conversations"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/devcontainers"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/discovery"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/identity"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/netauth"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/nodeid"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/peering"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/presence"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/projects"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessioncoord"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sessionmeta"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/sleep"
	central "github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/central"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/snapshot/wire"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/statetool"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/tsauth"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/unixipc"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/update"
	"github.com/gmuxapp/gmux/services/gmuxd/internal/wsproxy"
	"nhooyr.io/websocket"
)

// errIncumbentHealthy reports that serve declined to start because a healthy
// daemon of the same version already owns the socket and no explicit
// replacement was requested.
var errIncumbentHealthy = errors.New("gmuxd: already running")

// registerGetProjectsRoute installs the production GET /v1/projects handler.
// Keeping registration and ownership projection together lets contract tests
// exercise the same mux path peers consume.
func registerGetProjectsRoute(mux *http.ServeMux, render func(*http.Request) (wire.Frames, error), isLocalPeer func(string) bool) {
	mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
		frames, err := render(r)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", "snapshot unavailable")
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": projectDiscoveryData(frames, isLocalPeer)})
	})
}

func serveCentral(stderr io.Writer, replace bool) int {
	gmuxBin := resolveGmux()
	if gmuxBin != "" {
		log.Printf("gmux: %s", gmuxBin)
		h := binhash.File(gmuxBin)
		if h != "" {
			discovery.ExpectedRunnerHash = h
			log.Printf("gmux hash: %s…", h[:12])
		}
	}
	launchConfig := discoverLaunchers()
	updateChecker := update.New(version)
	peerTransport := tsauth.NewRoutedTransport()
	stateDir := paths.StateDir()
	convIndex := conversations.New()

	nodeID, err := nodeid.LoadOrCreate(stateDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	retention := sessionmeta.RetentionPolicy{
		MaxAge:               time.Duration(cfg.Sessions.RetentionDays) * 24 * time.Hour,
		MaxCount:             cfg.Sessions.RetentionMax,
		ScrollbackCacheBytes: int64(cfg.Sessions.ScrollbackCacheMB) << 20,
	}
	sessionDirs := sessionmeta.New(sessionmeta.DefaultDir(), sessionmeta.WithRetention(retention))
	tcpAddr, err := cfg.ListenAddr()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	authToken, err := authtoken.LoadOrCreate(stateDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}

	// A healthy incumbent of the SAME version is never shut down implicitly.
	// This path is reached by `gmux`'s daemon autostart whenever a health
	// probe times out (500ms budget) — on a loaded host that spawned daemons
	// which killed the healthy incumbent, re-bootstrapped for seconds, and
	// were killed in turn by the next autostart: a rolling production outage
	// (registrations timing out, /v1/health dead) that looked like a store
	// wedge. Replacement must be explicit (`gmuxd start`/`restart` pass
	// --replace) or an actual version upgrade (release CLI replacing an
	// outdated daemon).
	//
	// The yield check runs as bootstrapOwnership's precheck — BEFORE Verify
	// opens the database — so a pointless autostart bounces off the incumbent
	// without touching SQLite at all.
	precheck := func(context.Context) error {
		if replace {
			return nil
		}
		sock := paths.SocketPath()
		ident, ok := unixipc.HealthIdentity(sock)
		if !ok {
			return nil
		}
		if ident.Version == version {
			return fmt.Errorf("%w: healthy daemon %s (pid %d) owns %s", errIncumbentHealthy, ident.Version, ident.PID, sock)
		}
		return nil
	}
	takeover := func(context.Context) error {
		sock := paths.SocketPath()
		ident, ok := unixipc.HealthIdentity(sock)
		if !ok {
			return nil
		}
		if !replace && ident.Version == version {
			// Re-check: an incumbent may have become healthy between the
			// precheck and here (e.g. it was mid-bootstrap during precheck).
			return fmt.Errorf("%w: healthy daemon %s (pid %d) owns %s", errIncumbentHealthy, ident.Version, ident.PID, sock)
		}
		if !unixipc.Shutdown(sock) {
			return fmt.Errorf("existing daemon at %s did not shut down", sock)
		}
		return nil
	}
	storeHandle, storeLock, err := bootstrapOwnership(context.Background(), stateDir, precheck, takeover)
	if err != nil {
		if errors.Is(err, errIncumbentHealthy) {
			// Idempotent success: the daemon this invocation wanted is
			// already there. Exit code 0 so autostart callers and service
			// managers treat it as "nothing to do", not a crash loop.
			_, _ = fmt.Fprintf(stderr, "%v\n", err)
			return 0
		}
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	defer storeHandle.Close()
	defer storeLock.Close()

	// Bound the log only now: ownership is settled, so this process is the
	// daemon and no incumbent is writing to the same file. An invocation that
	// yielded above returned without touching it.
	if bounded := installBoundedLog(stderr, paths.DaemonLogPath(), defaultLogLimit); bounded != nil {
		defer bounded.Stop()
	}

	// Conversation discovery reads and parses the complete on-disk history.
	// Do it only after ownership is settled: autostart contenders that find a
	// healthy incumbent must yield without paying this multi-second cost.
	convIndex.Snapshot()
	log.Printf("conversations: indexed %d conversations", convIndex.Count())

	var peerManager *peering.Manager
	var tsListener *tsauth.Listener
	var notifier *centralNotifyRouter
	fanout := newSSEFanout()
	presenceTable := presence.New(presence.Callbacks{
		OnClientConnected: func(string) {
			if peerManager != nil {
				peerManager.ReconnectAll()
			}
		},
		OnClientFocused: func(string) {
			if notifier != nil {
				notifier.CancelAllPending()
			}
		},
		OnSessionSelected: func(_ string, sessionID string) {
			if notifier != nil {
				notifier.CancelForSession(sessionID)
			}
		},
	})

	var boot *Bootstrap
	peerAdapter := &centralPeerAdapter{store: storeHandle, dirty: func(sd, wd bool) {
		if boot != nil && boot.Composer != nil {
			boot.Composer.MarkDirty(sd, wd)
		}
	}, activity: func(id centralstore.SessionID) {
		if boot != nil && boot.Coordinator != nil {
			boot.Coordinator.PublishActivity(id)
		}
	}, now: func() centralstore.UnixMillis { return centralstore.UnixMillis(time.Now().UnixMilli()) }}
	peerLaunchers := make([]peering.LauncherDef, 0, len(launchConfig.Launchers))
	for _, launcher := range launchConfig.Launchers {
		peerLaunchers = append(peerLaunchers, peering.LauncherDef{ID: launcher.ID, Label: launcher.Label, Command: append([]string(nil), launcher.Command...), Description: launcher.Description, Available: launcher.Available})
	}
	peerAdapter.health = func() central.HealthInfo {
		osHost, _ := os.Hostname()
		tsFQDN := ""
		if tsListener != nil {
			tsFQDN = tsListener.FQDN()
		}
		h := central.HealthInfo{Service: "gmuxd", Version: version, NodeID: nodeID, Status: "ready", Hostname: identity.Resolve(tsFQDN, osHost), Listen: tcpAddr, RunnerHash: discovery.ExpectedRunnerHash, DefaultLauncher: launchConfig.DefaultLauncher, Launchers: append([]peering.LauncherDef(nil), peerLaunchers...), Peers: currentPeers(peerManager)}
		if tsListener != nil {
			d := tsListener.Diag()
			h.Tailscale = &d
			if d.Connected && d.HTTPS && d.FQDN != "" {
				h.TailscaleURL = "https://" + d.FQDN
			}
		}
		if v := updateChecker.Available(); v != "" {
			h.UpdateAvailable = v
		}
		return h
	}

	converter := &wire.Converter{Titlers: make(map[string]func([]string) string), SemanticAgents: make(map[string]bool), ResumeCommand: convIndex.LookupResumeCommand, IsLocalPeer: func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) }}
	for _, a := range adapters.All {
		if titler, ok := a.(adapter.CommandTitler); ok {
			converter.Titlers[a.Name()] = titler.CommandTitle
		}
		if semanticAgentAdapter(a) {
			converter.SemanticAgents[a.Name()] = true
		}
	}
	if fallback := adapters.DefaultFallback(); fallback != nil {
		if titler, ok := fallback.(adapter.CommandTitler); ok {
			converter.Titlers[fallback.Name()] = titler.CommandTitle
		}
		if semanticAgentAdapter(fallback) {
			converter.SemanticAgents[fallback.Name()] = true
		}
	}

	spawner := &productionRunnerSpawner{GmuxBin: gmuxBin, ResolveDir: func(row centralstore.Session) (string, error) {
		dir, _, err := resolveResumeDirCentral(context.Background(), storeHandle, row)
		return dir, err
	}, ResolveCommand: func(row centralstore.Session) []string {
		legacy := centralSessionToLegacy(row)
		return discovery.ResolveResumeCommandFor(legacy.Adapter, legacy.ConversationRef)
	}}

	boot, err = newBootstrap(BootstrapConfig{Store: storeHandle, Runners: productionRunnerClient{}, Control: productionRunnerControl{}, Spawner: spawner, Resolver: productionConversationResolver{}, Reconciler: productionAdapterReconciler{}, LocalPeers: peerAdapter.LocalPeerMatchInputs, Peers: peerAdapter, PeerSessions: peerAdapter, Converter: converter, Endpoints: productionEndpointSource{}, Errors: sessioncoord.ErrorSinkFunc(func(_ context.Context, err error) { log.Printf("gmuxd: %v", err) }), Frames: func(_ context.Context, frames wire.Frames) {
		// The converter builds world.health.launchers but not the top-level
		// world.launchers/default_launcher that the web UI's "+" menu reads
		// (parity with the legacy composeWorld). Inject the static launch
		// config onto a shallow World copy so every broadcast carries it.
		if frames.World != nil {
			w := *frames.World
			w.Launchers = peerLaunchers
			w.DefaultLauncher = launchConfig.DefaultLauncher
			frames.World = &w
		}
		fanout.BroadcastFrames(frames)
	}})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	defer boot.Close()

	hostname, _ := os.Hostname()
	peerManager = peering.NewProjectionManager(nil, hostname, nil, peerAdapter.hooks(), peering.WithTransport(peerTransport))
	peerAdapter.manager = peerManager
	if err := reconcileManualPeers(context.Background(), storeHandle, peerManager); err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	peerManager.Start()
	defer peerManager.Stop()

	endpoints, err := boot.Converge(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	if err := boot.StartPostConvergence(context.Background(), endpoints); err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}

	seed, events, cancelNotify, err := boot.SubscribeOutcomes(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	defer cancelNotify()
	notifier = newCentralNotifyRouter(presenceTable, defaultNotifyConfig())
	daemonCtx, daemonCancel := context.WithCancel(context.Background())
	defer daemonCancel()
	go notifier.Run(daemonCtx, seed, events)

	commonMux := http.NewServeMux()
	unixMux := http.NewServeMux()
	registerCommon := func(mux *http.ServeMux, unixOnly bool) {
		mux.HandleFunc("/v1/health", func(w http.ResponseWriter, r *http.Request) {
			// Store-direct read (ADR 0026 §2a): render from SQLite so
			// session counts are always fresh. Subsumes the
			// freshHealthCounts point-fix from 41de2a28.
			frames, renderErr := renderStoreDirect(r.Context(), boot, converter, peerAdapter)
			health := peerAdapter.health()
			if renderErr == nil && frames.World != nil && frames.World.Health != nil {
				h := *frames.World.Health
				health.Sessions = h.Sessions
				if len(h.Peers) > 0 {
					health.Peers = append([]peering.PeerInfo(nil), h.Peers...)
				}
			}
			peers := health.Peers
			if peers == nil {
				peers = []peering.PeerInfo{}
			}
			launchers := health.Launchers
			if launchers == nil {
				launchers = []peering.LauncherDef{}
			}
			data := map[string]any{"service": health.Service, "version": health.Version, "pid": os.Getpid(), "node_id": health.NodeID, "status": health.Status, "hostname": health.Hostname, "listen": health.Listen, "peers": peers, "sessions": health.Sessions, "runner_hash": health.RunnerHash, "default_launcher": health.DefaultLauncher, "launchers": launchers}
			if health.TailscaleURL != "" {
				data["tailscale_url"] = health.TailscaleURL
			}
			if health.Tailscale != nil {
				data["tailscale"] = health.Tailscale
			}
			if health.UpdateAvailable != "" {
				data["update_available"] = health.UpdateAvailable
			}
			if unixOnly {
				data["auth_token"] = authToken
			}
			writeJSON(w, map[string]any{"ok": true, "data": data})
		})
		mux.HandleFunc("/v1/capabilities", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"adapters": []string{"pi", "shell"}, "transport": map[string]any{"kind": "websocket", "replay": true}}})
		})
		mux.HandleFunc("GET /v1/frontend-config", func(w http.ResponseWriter, r *http.Request) {
			theme, themeErr := config.LoadTheme()
			settings, settingsErr := config.LoadSettings()
			if themeErr != nil {
				log.Printf("frontend-config: theme: %v", themeErr)
			}
			if settingsErr != nil {
				log.Printf("frontend-config: settings: %v", settingsErr)
			}
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"theme": theme, "settings": settings}})
		})
		registerGetProjectsRoute(mux, func(r *http.Request) (wire.Frames, error) {
			// Store-direct read (ADR 0026 §2a).
			return renderStoreDirect(r.Context(), boot, converter, peerAdapter)
		}, func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) })
		mux.HandleFunc("PUT /v1/projects", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read error")
				return
			}
			incoming, err := decodeProjectState(body)
			if err != nil {
				if errors.Is(err, errInvalidProjectJSON) {
					writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
					return
				}
				writeError(w, http.StatusBadRequest, "validation_error", err.Error())
				return
			}
			log.Printf("gmuxd: projects-replace-pending")
			if _, err := boot.Coordinator.ReplaceCatalog(r.Context(), projectSpecsFromState(incoming)); err != nil {
				log.Printf("projects replace: %v", err)
				writeError(w, http.StatusInternalServerError, "internal", "failed to save projects")
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		})
		mux.HandleFunc("POST /v1/projects/add", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read error")
				return
			}
			var req struct {
				Remote string   `json:"remote"`
				Paths  []string `json:"paths"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
				return
			}
			if len(req.Paths) == 0 {
				writeError(w, http.StatusBadRequest, "bad_request", "paths required")
				return
			}
			var rules []projects.MatchRule
			if req.Remote != "" {
				rules = append(rules, projects.MatchRule{Remote: projects.NormalizeRemote(req.Remote)})
			}
			for _, p := range req.Paths {
				rules = append(rules, projects.MatchRule{Path: paths.CanonicalizePath(p)})
			}
			slug := projects.SlugFromPath(req.Paths[0])
			if req.Remote != "" {
				slug = projects.SlugFromRemote(req.Remote)
			}
			// Store-direct project read for slug uniqueness check.
			snap, snapErr := boot.Store.ReadSnapshot(r.Context(), centralstore.SnapshotQuery{IncludeProjects: true})
			if snapErr != nil {
				writeError(w, http.StatusInternalServerError, "internal", "failed to load projects")
				return
			}
			state := *ownedProjectStateFromCatalog(snap.Projects)
			item := projects.Item{Slug: projects.UniqueSlug(slug, state.Items), Match: rules}
			state.Items = append(state.Items, item)
			if err := state.Validate(); err != nil {
				writeError(w, http.StatusConflict, "validation_error", err.Error())
				return
			}
			if _, err := boot.Coordinator.ReplaceCatalog(r.Context(), projectSpecsFromState(state)); err != nil {
				log.Printf("projects add: %v", err)
				writeError(w, http.StatusInternalServerError, "internal", "failed to save projects")
				return
			}
			writeJSON(w, map[string]any{"ok": true, "data": item})
		})
		mux.HandleFunc("PATCH /v1/projects/{slug}/sessions", func(w http.ResponseWriter, r *http.Request) {
			slug := r.PathValue("slug")
			body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read error")
				return
			}
			var req struct {
				Sessions []string `json:"sessions"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
				return
			}
			local, world, err := reorderPayloads(r.Context(), storeHandle)
			if err != nil {
				log.Printf("projects reorder payloads: %v", err)
				writeError(w, http.StatusInternalServerError, "internal", "failed to load projects")
				return
			}
			orders, ok := converter.DecomposeReorder(slug, req.Sessions, local, world)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "project not found")
				return
			}
			scopes := make([]centralstore.SiblingReorder, 0, len(orders))
			for _, order := range orders {
				scopes = append(scopes, centralstore.SiblingReorder{Project: order.Project, Parent: order.Parent, Order: order.Order})
			}
			if _, err := boot.Coordinator.ReorderSiblingScopes(r.Context(), scopes); err != nil {
				log.Printf("projects reorder: %v", err)
				writeError(w, http.StatusInternalServerError, "internal", "failed to save projects")
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		})
		mux.HandleFunc("/v1/peers/", func(w http.ResponseWriter, r *http.Request) {
			rest := strings.TrimPrefix(r.URL.Path, "/v1/peers/")
			name, sub, ok := strings.Cut(rest, "/")
			if !ok || name == "" || sub == "" {
				writeError(w, http.StatusNotFound, "not_found", "peer path required")
				return
			}
			if !isAllowedPeerProxyPath(r.Method, sub) {
				writeError(w, http.StatusForbidden, "forbidden", "peer proxy: method+path not allowed")
				return
			}
			if peerManager == nil {
				writeError(w, http.StatusBadGateway, "unknown_peer", "no peers configured")
				return
			}
			peer := peerManager.GetPeer(name)
			if peer == nil {
				writeError(w, http.StatusBadGateway, "unknown_peer", fmt.Sprintf("peer %q not configured", name))
				return
			}
			peer.ForwardPath(w, r, "/"+sub)
		})
		mux.HandleFunc("POST /v1/peers", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 8192))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read error")
				return
			}
			var req struct{ URL, Token string }
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
				return
			}
			req.URL = strings.TrimRight(strings.TrimSpace(req.URL), "/")
			nodeID, name, err := probePeerHealth(r.Context(), peerTransport, req.URL, req.Token)
			if err != nil {
				writeError(w, http.StatusBadGateway, "unreachable", err.Error())
				return
			}
			log.Printf("gmuxd: peer-upsert-pending")
			rec, outcome, result, err := storeHandle.UpsertManualPeer(r.Context(), centralstore.ManualPeerSpec{Name: name, URL: req.URL, Token: req.Token, NodeID: nodeID}, centralstore.UnixMillis(time.Now().UnixMilli()))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", err.Error())
				return
			}
			if result.Changed {
				boot.Composer.Invalidate(result)
			}
			if outcome == centralstore.PeerUnchanged {
				writeJSON(w, manualPeerResponse(rec, outcome))
				return
			}
			if err := reconcileManualPeers(r.Context(), storeHandle, peerManager); err != nil {
				writeError(w, http.StatusBadGateway, "reconcile_failed", err.Error())
				return
			}
			writeJSON(w, manualPeerResponse(rec, outcome))
		})
		mux.HandleFunc("DELETE /v1/peers/{name}", func(w http.ResponseWriter, r *http.Request) {
			result, err := storeHandle.RemoveManualPeer(r.Context(), r.PathValue("name"))
			if err != nil {
				if errors.Is(err, centralstore.ErrPeerNotFound) {
					writeError(w, http.StatusNotFound, "not_found", err.Error())
					return
				}
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			boot.Composer.Invalidate(result)
			if err := reconcileManualPeers(r.Context(), storeHandle, peerManager); err != nil {
				writeError(w, http.StatusBadGateway, "reconcile_failed", err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		})
		mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
			// Store-direct read (ADR 0026 §2a): render from SQLite at
			// request time so REST clients get read-your-writes.
			frames, err := renderStoreDirect(r.Context(), boot, converter, peerAdapter)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", "snapshot unavailable")
				return
			}
			// The CLI (`gmux ls`/`kill`/`attach`/... via fetchSessions) and the
			// legacy daemon contract expect `data` to be a flat JSON array of
			// sessions, not the SSE snapshot's {"sessions":[...]} envelope.
			sessions := []wire.Session{}
			if frames.Sessions != nil {
				sessions = frames.Sessions.Sessions
			}
			writeJSON(w, map[string]any{"ok": true, "data": sessions})
		})
		mux.HandleFunc("GET /v1/conversations/{adapter}/{slug}", func(w http.ResponseWriter, r *http.Request) {
			adapterName := r.PathValue("adapter")
			slug := r.PathValue("slug")
			info, ok := convIndex.Lookup(adapterName, slug)
			if !ok {
				writeError(w, http.StatusNotFound, "not_found", "conversation not found")
				return
			}
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"slug": info.Slug, "adapter": info.Adapter, "title": info.Title, "cwd": info.Cwd, "resume_command": info.ResumeCommand, "created": info.Created}})
		})
		// Read-only, non-reserving preflight for client-minted IDs. Absence is
		// advisory; POST /v1/register repeats the check under the coordinator's
		// lifecycle fence to close the race.
		mux.HandleFunc("GET /v1/session-ids/{id}", func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if !paths.IsValidSessionID(id) {
				writeError(w, http.StatusBadRequest, "invalid_session_id", "invalid session id")
				return
			}
			_, exists, err := boot.Store.Session(r.Context(), centralstore.SessionID(id))
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error())
				return
			}
			if exists {
				writeError(w, http.StatusConflict, "session_id_exists", "session id already exists")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
		mux.HandleFunc("POST /v1/register", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read error")
				return
			}
			var req struct {
				SessionID  string `json:"session_id"`
				SocketPath string `json:"socket_path"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
				return
			}
			if req.SessionID == "" || req.SocketPath == "" {
				writeError(w, http.StatusBadRequest, "bad_request", "session_id and socket_path required")
				return
			}
			if _, err := boot.Coordinator.Register(r.Context(), sessioncoord.RegisterRequest{Endpoint: req.SocketPath, AssertedID: centralstore.SessionID(req.SessionID)}); err != nil {
				if errors.Is(err, sessioncoord.ErrSessionIDExists) {
					writeError(w, http.StatusConflict, "session_id_exists", err.Error())
					return
				}
				if errors.Is(err, sessioncoord.ErrInvalidSessionID) || errors.Is(err, sessioncoord.ErrAssertedIdentityMismatch) {
					writeError(w, http.StatusBadRequest, "invalid_session_id", err.Error())
					return
				}
				writeError(w, http.StatusBadGateway, "runner_unreachable", err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true})
		})
		mux.HandleFunc("POST /v1/deregister", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read error")
				return
			}
			var req struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
				return
			}
			if req.SessionID == "" {
				writeJSON(w, map[string]any{"ok": true})
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			seed, outcomes, unsubscribe, err := boot.SubscribeOutcomes(ctx)
			if err == nil {
				defer unsubscribe()
				for _, outcome := range seed {
					if string(outcome.ID) == req.SessionID && outcome.Session != nil && outcome.Session.Adapter == "editor" && !outcome.Alive && outcome.Session.Version > 0 {
						_ = boot.Coordinator.Remove(ctx, outcome.ID, outcome.Session.Version)
						break
					}
				}
				for {
					select {
					case <-ctx.Done():
						writeJSON(w, map[string]any{"ok": true})
						return
					case outcome, ok := <-outcomes:
						if !ok {
							writeJSON(w, map[string]any{"ok": true})
							return
						}
						if string(outcome.ID) != req.SessionID || outcome.Type != sessioncoord.OutcomeUpserted || outcome.Session == nil || outcome.Session.Adapter != "editor" || outcome.Alive {
							continue
						}
						_ = boot.Coordinator.Remove(ctx, outcome.ID, outcome.Session.Version)
						writeJSON(w, map[string]any{"ok": true})
						return
					}
				}
			}
			writeJSON(w, map[string]any{"ok": true})
		})
		mux.HandleFunc("POST /v1/launch", func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "read error")
				return
			}
			var req struct {
				Cwd        string   `json:"cwd"`
				Command    []string `json:"command"`
				LauncherID string   `json:"launcher_id"`
				Peer       string   `json:"peer"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON")
				return
			}
			if req.Peer != "" {
				if peerManager == nil {
					writeError(w, http.StatusBadRequest, "unknown_peer", "no peers configured")
					return
				}
				if peer := peerManager.GetPeer(req.Peer); peer != nil {
					r.Body = io.NopCloser(bytes.NewReader(body))
					peer.ForwardLaunch(w, r)
					return
				}
				writeError(w, http.StatusBadRequest, "unknown_peer", fmt.Sprintf("peer %q not configured", req.Peer))
				return
			}
			if len(req.Command) == 0 && req.LauncherID != "" {
				found := false
				for _, l := range launchConfig.Launchers {
					if l.ID == req.LauncherID {
						req.Command = l.Command
						found = true
						break
					}
				}
				if !found {
					writeError(w, http.StatusBadRequest, "launcher_unavailable", fmt.Sprintf("launcher %q is not available on this system", req.LauncherID))
					return
				}
			}
			if len(req.Command) == 0 {
				shell := os.Getenv("SHELL")
				if shell == "" {
					shell = "/bin/sh"
				}
				req.Command = []string{shell}
			}
			cwd := projects.NormalizePath(req.Cwd)
			if cwd == "" {
				cwd = os.Getenv("HOME")
			}
			if !projects.IsDir(cwd) {
				writeError(w, http.StatusUnprocessableEntity, "cwd_missing", fmt.Sprintf("working directory %q does not exist", cwd))
				return
			}
			if gmuxBin == "" {
				writeError(w, http.StatusInternalServerError, "gmux_not_found", "gmux not found (install gmux alongside gmuxd)")
				return
			}
			pid, err := launchGmux(gmuxBin, req.Command, cwd, "", 0, 0)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "launch_failed", err.Error())
				return
			}
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"pid": pid}})
		})
		mux.HandleFunc("/v1/sessions/", func(w http.ResponseWriter, r *http.Request) {
			handleCentralSessionAction(w, r, boot, fanout, converter, peerManager, sessionDirs, gmuxBin)
		})
		mux.HandleFunc("/ws/{sessionID}", func(w http.ResponseWriter, r *http.Request) {
			sessionID := r.PathValue("sessionID")
			if peerManager != nil {
				if peer, originalID := peerManager.FindPeer(sessionID); peer != nil {
					peer.ProxyWS(w, r, originalID)
					return
				}
			}
			proxy := wsproxy.New(func(sessionID string) (string, error) {
				if e, ok := registryRuntime(boot.Registry, centralstore.SessionID(sessionID)); ok {
					return e.Endpoint, nil
				}
				if _, ok := visibleSession(fanout.Current().Sessions, sessionID); ok {
					return "", fmt.Errorf("session %s has no socket", sessionID)
				}
				return "", fmt.Errorf("session %s not found", sessionID)
			}, centralSizer{fanout: fanout})
			proxy.Handler()(w, r)
		})
		mux.HandleFunc("/v1/presence", func(w http.ResponseWriter, r *http.Request) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			clientID := fmt.Sprintf("client-%d", time.Now().UnixNano())
			client := &presence.Client{ID: clientID, Conn: conn, ConnectedAt: time.Now()}
			ctx := r.Context()
			_, data, err := conn.Read(ctx)
			if err != nil {
				conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			var hello struct {
				Type                   string `json:"type"`
				DeviceType             string `json:"device_type"`
				NotificationPermission string `json:"notification_permission"`
			}
			if err := json.Unmarshal(data, &hello); err == nil && hello.Type == "client-hello" {
				client.DeviceType = hello.DeviceType
				client.NotificationPermission = hello.NotificationPermission
			}
			presenceTable.Add(client)
			defer func() {
				presenceTable.Remove(clientID)
				_ = conn.Close(websocket.StatusNormalClosure, "")
			}()
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					return
				}
				var msg struct {
					Type              string  `json:"type"`
					Visibility        string  `json:"visibility"`
					Focused           bool    `json:"focused"`
					SelectedSessionID string  `json:"selected_session_id"`
					LastInteraction   float64 `json:"last_interaction"`
					Permission        string  `json:"permission"`
				}
				if err := json.Unmarshal(data, &msg); err != nil {
					continue
				}
				switch msg.Type {
				case "client-state":
					presenceTable.Update(clientID, presence.ClientState{Visibility: msg.Visibility, Focused: msg.Focused, SelectedSessionID: msg.SelectedSessionID, LastInteraction: msg.LastInteraction})
					if msg.SelectedSessionID != "" {
						_ = boot.Coordinator.AcknowledgeDead(context.Background(), centralstore.SessionID(msg.SelectedSessionID))
					}
				case "notif-permission":
					presenceTable.SetPermission(clientID, msg.Permission)
				}
			}
		})
		mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			rc := http.NewResponseController(w)
			asPeer := r.URL.Query().Get("as") == "peer"
			initial, ch, cancel := fanout.Subscribe()
			defer cancel()
			isLocalPeer := func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) }
			sendSessions := func(payload *wire.SessionsPayload) error {
				if payload == nil {
					return nil
				}
				if asPeer {
					filtered := payload.FilterOwned(isLocalPeer)
					return sendSSEFrame(rc, w, "snapshot.sessions", filtered)
				}
				return sendSSEFrame(rc, w, "snapshot.sessions", payload)
			}
			if err := sendSessions(initial.Sessions); err != nil {
				return
			}
			if !asPeer && initial.World != nil {
				// Same staleness as /v1/health: the cached world frame's
				// health counts predate any liveness-only batches. Legacy
				// composed the initial snapshot at subscribe time; refresh
				// the counts (on the fanout's private copy) to match.
				if counts, ok := freshHealthCounts(initial); ok && initial.World.Health != nil {
					h := *initial.World.Health
					h.Sessions = counts
					initial.World.Health = &h
				}
				if err := sendSSEFrame(rc, w, "snapshot.world", initial.World); err != nil {
					return
				}
			}
			heartbeat := time.NewTicker(30 * time.Second)
			defer heartbeat.Stop()
			for {
				select {
				case <-r.Context().Done():
					return
				case <-heartbeat.C:
					if err := sendSSEComment(rc, w); err != nil {
						return
					}
				case msg, ok := <-ch:
					if !ok {
						return
					}
					if msg.ActivityID != "" {
						if !shouldForwardActivity(asPeer, msg.ActivityID, isLocalPeer) {
							continue
						}
						if err := sendSSEFrame(rc, w, "session-activity", map[string]any{"type": "session-activity", "id": msg.ActivityID}); err != nil {
							return
						}
						continue
					}
					if err := sendSessions(msg.Frames.Sessions); err != nil {
						return
					}
					if asPeer {
						if msg.ProjectsUpdate {
							if err := sendSSEFrame(rc, w, "projects-update", map[string]any{"type": "projects-update"}); err != nil {
								return
							}
						}
						continue
					}
					if msg.Frames.World != nil {
						if err := sendSSEFrame(rc, w, "snapshot.world", msg.Frames.World); err != nil {
							return
						}
					}
				}
			}
		})
		mux.Handle("/", spaHandler())
	}
	registerCommon(commonMux, false)
	registerCommon(unixMux, true)
	(&statetool.Handler{Store: storeHandle}).Register(unixMux)
	unixMux.HandleFunc("POST /v1/shutdown", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true})
		go daemonCancel()
	})

	boot.StartOwnedTriggers(TriggerConfig{Tick: productionEndpointSchedule(daemonCtx, 30*time.Second), ConversationDeleted: productionConversationDeletionSource(daemonCtx, convIndex), PeerSessionsChanged: nil, PeerWorldChanged: nil, Activity: func(o sessioncoord.Outcome) { fanout.BroadcastActivity(string(o.ID)) }})

	authedHandler := netauth.Middleware(authToken, commonMux)
	sock := paths.SocketPath()
	sockLn, err := unixipc.Listen(sock)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		daemonCancel()
		return 1
	}
	defer unixipc.Cleanup(sock)
	sockSrv := &http.Server{Handler: unixMux}
	go func() {
		if err := sockSrv.Serve(sockLn); err != nil && err != http.ErrServerClosed {
			log.Printf("unix socket listener: %v", err)
		}
	}()
	tcpLn, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: tcp listener on %s: %v\n", tcpAddr, err)
		daemonCancel()
		return 1
	}
	tcpSrv := &http.Server{Addr: tcpAddr, Handler: authedHandler}
	go func() {
		if err := tcpSrv.Serve(tcpLn); err != nil && err != http.ErrServerClosed {
			log.Printf("tcp listener: %v", err)
		}
	}()

	sleepWatcher := sleep.NewWatcher()
	defer sleepWatcher.Stop()
	go func() {
		for range sleepWatcher.C() {
			peerManager.OnSleep()
		}
	}()
	var dcWatcher *devcontainers.Watcher
	if cfg.Discovery.Devcontainers {
		dcWatcher = devcontainers.NewWatcher(peerManager)
		if dcWatcher != nil {
			dcWatcher.Start()
			defer dcWatcher.Stop()
		}
	}
	if cfg.Tailscale.Enabled {
		tsSeed := strings.TrimSpace(os.Getenv("GMUXD_TS_HOSTNAME"))
		if tsSeed == "" {
			tsSeed = tsauth.SeedFromHostname(hostname)
		}
		tsListener = tsauth.Start(tsauth.Config{Hostname: tsSeed, Allow: cfg.Tailscale.Allow}, stateDir, authedHandler)
		defer tsListener.Shutdown()
		go func(l *tsauth.Listener) {
			<-l.Ready()
			if suffix := l.MagicDNSSuffix(); suffix != "" {
				peerTransport.SetTailnet(suffix, l.Transport())
			}
		}(tsListener)
	}
	shutdownCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-daemonCtx.Done():
	case <-shutdownCh:
	case <-sigCh:
	}
	daemonCancel()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	_ = sockSrv.Shutdown(shutdownCtx)
	_ = tcpSrv.Shutdown(shutdownCtx)
	return 0
}

type centralSizer struct{ fanout *sseFanout }

func (s centralSizer) SetTerminalSize(string, uint16, uint16) bool { return true }
func (s centralSizer) GetTerminalSize(sessionID string) (uint16, uint16, bool) {
	frames := s.fanout.Current()
	sess, ok := visibleSession(frames.Sessions, sessionID)
	if !ok {
		return 0, 0, false
	}
	return sess.TerminalCols, sess.TerminalRows, true
}

func handleCentralSessionAction(w http.ResponseWriter, r *http.Request, boot *Bootstrap, fanout *sseFanout, converter *wire.Converter, peerManager *peering.Manager, sessionDirs *sessionmeta.Store, gmuxBin string) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	sessionID := parts[2]
	action := ""
	if len(parts) == 4 {
		action = parts[3]
	}
	if peerManager != nil && action != "" {
		if peer, originalID := peerManager.FindPeer(sessionID); peer != nil {
			// Semantic agent actions are local-only in this slice (ADR 0027).
			// Refusing is not a limitation to paper over: forwarding would run
			// an agent on another host under a contract (readiness, admission,
			// transparent resume) that has not been validated across the peer
			// boundary, and silently doing it is worse than saying no.
			if agentAction(action) {
				writeError(w, http.StatusBadRequest, codeLocalOnly, fmt.Sprintf(
					"%s is only available for sessions owned by this host; run gmux on the owning host", action))
				return
			}
			if action == "attach" {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"transport": "websocket", "ws_path": "/ws/" + sessionID}})
				return
			}
			peer.Forward(w, r, originalID, action)
			return
		}
	}
	frames := fanout.Current()
	sess, ok := visibleSession(frames.Sessions, sessionID)
	sid := centralstore.SessionID(sessionID)
	switch action {
	case "attach":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}
		// Store-direct existence check (ADR 0026 §2a).
		if _, found, err := boot.Store.Session(r.Context(), sid); err != nil || !found {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		socketPath := ""
		if e, live := registryRuntime(boot.Registry, sid); live {
			socketPath = e.Endpoint
		}
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"transport": "websocket", "ws_path": "/ws/" + sessionID, "socket_path": socketPath}})
	case "resume":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}
		row, found, err := boot.Store.Session(r.Context(), sid)
		if err != nil || !found {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		if row.ExitedAt == nil || len(row.Command) == 0 {
			writeError(w, http.StatusBadRequest, "not_resumable", "session is not resumable")
			return
		}
		if gmuxBin == "" {
			writeError(w, http.StatusInternalServerError, "gmux_not_found", "gmux not found")
			return
		}
		resumeCwd, fellBack, err := resolveResumeDirCentral(r.Context(), boot.Store, row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if resumeCwd == "" {
			writeError(w, http.StatusUnprocessableEntity, "cwd_missing", "the session's working directory no longer exists and no fallback directory is available")
			return
		}
		runtime, err := boot.Coordinator.Resume(r.Context(), sid)
		if err != nil {
			writeCentralLifecycleError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": relaunchData(sessionID, runtime.PID, projects.NormalizePath(row.CWD), resumeCwd, fellBack)})
	case "restart":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}
		row, found, err := boot.Store.Session(r.Context(), sid)
		if err != nil || !found {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		if gmuxBin == "" {
			writeError(w, http.StatusInternalServerError, "gmux_not_found", "gmux not found")
			return
		}
		restartCwd, fellBack, err := resolveResumeDirCentral(r.Context(), boot.Store, row)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if restartCwd == "" {
			writeError(w, http.StatusUnprocessableEntity, "cwd_missing", "the session's working directory no longer exists and no fallback directory is available")
			return
		}
		runtime, err := boot.Coordinator.Restart(r.Context(), sid)
		if err != nil {
			writeCentralLifecycleError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": relaunchData(sessionID, runtime.PID, projects.NormalizePath(row.CWD), restartCwd, fellBack)})
	case "kill":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}
		if err := boot.Coordinator.Stop(r.Context(), sid); err != nil {
			writeCentralLifecycleError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})
	case "read":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}
		if err := boot.Coordinator.AcknowledgeDead(r.Context(), sid); err != nil && !errors.Is(err, centralstore.ErrSessionNotFound) {
			writeError(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})
	case "input":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}
		runtime, live := registryRuntime(boot.Registry, sid)
		if !live {
			writeError(w, http.StatusConflict, "not_running", "session is not running")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, maxInputBytes+1))
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error())
			return
		}
		if int64(len(body)) > maxInputBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("input exceeds %d bytes", maxInputBytes))
			return
		}
		send := func() error { return discovery.SendInput(r.Context(), runtime.Endpoint, bytes.NewReader(body)) }
		if r.URL.Query().Get("wait") != "" {
			handleInputWaitCentral(w, r, boot, fanout, sessionID, body, send)
			return
		}
		if err := send(); err != nil {
			writeError(w, http.StatusBadGateway, "runner_unreachable", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case "scrollback":
		// Store-direct session lookup (ADR 0026 §2a).
		if !ok {
			row, found, storeErr := boot.Store.Session(r.Context(), sid)
			if storeErr == nil && found {
				ok = true
				sess = wireSessionFromStore(row, boot.Registry)
			}
		}
		scrollbackBrokerHandlerCentral(w, r, sessionID, sess, ok, sessionDirs.SessionDir)
	case "conversation":
		conversationHandlerCentral(w, r, sessionID, boot)
	case "clipboard":
		// Store-direct existence check (ADR 0026 §2a).
		if _, found, err := boot.Store.Session(r.Context(), sid); err != nil || !found {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		clipboardHandler(clipfile.NewLocalWriter(os.TempDir())).ServeHTTP(w, r)
	case "dismiss":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
			return
		}
		rows, err := sessionTreeRows(r.Context(), boot.Store, sid)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		for _, row := range rows {
			if _, live := registryRuntime(boot.Registry, row.ID); !live {
				continue
			}
			if err := boot.Coordinator.Stop(ctx, row.ID); err != nil {
				writeCentralLifecycleError(w, err)
				return
			}
		}
		if _, err := boot.Coordinator.Dismiss(r.Context(), sid); err != nil {
			writeCentralLifecycleError(w, err)
			return
		}
		go sessionDirs.MaybePruneScrollback(currentAliveSessionIDs(boot.Registry), 12*time.Hour)
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{}})
	case "wait":
		handleWaitCentral(w, r, boot, fanout, sessionID, sessionDirs.SessionDir)
	case "prompt":
		handleAgentPromptCentral(w, r, productionAgentDeps(boot, gmuxBin), sessionID)
	case "cancel":
		handleAgentCancelCentral(w, r, productionAgentDeps(boot, gmuxBin), sessionID)
	default:
		http.NotFound(w, r)
	}
	_ = converter
}

func currentAliveSessionIDs(reg *sessioncoord.Registry) map[string]bool {
	out := map[string]bool{}
	for _, runtime := range reg.Snapshot() {
		out[string(runtime.SessionID)] = true
	}
	return out
}

func writeCentralLifecycleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, centralstore.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "not_found", "session not found")
	case errors.Is(err, sessioncoord.ErrSessionAlive), errors.Is(err, sessioncoord.ErrSessionNotAlive):
		writeError(w, http.StatusBadRequest, "not_resumable", err.Error())
	case errors.Is(err, sessioncoord.ErrConvergencePending):
		writeError(w, http.StatusServiceUnavailable, "convergence_pending", err.Error())
	case errors.Is(err, sessioncoord.ErrLifecycleOpInFlight), errors.Is(err, sessioncoord.ErrSubtreeBusy), errors.Is(err, sessioncoord.ErrStopSuperseded):
		writeError(w, http.StatusConflict, "busy", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "kill_timeout", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
	}
}

func conversationHandlerCentral(w http.ResponseWriter, r *http.Request, sessionID string, boot *Bootstrap) {
	sessions := boot.Store
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	for key := range r.URL.Query() {
		if key != "tail" {
			writeError(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("unknown conversation parameter %q; only tail is supported", key))
			return
		}
	}
	n := 1
	if raw := r.URL.Query().Get("tail"); raw != "" {
		var err error
		n, err = strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "tail must be a positive number of exchanges")
			return
		}
	}
	sess, ok, err := sessions.Session(r.Context(), centralstore.SessionID(sessionID))
	if err != nil || !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	renderer, ok := adapters.FindByAdapter(sess.Adapter).(adapter.ConversationExchangeRenderer)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, codeUnsupportedAdapter,
			fmt.Sprintf("adapter %q does not render conversation exchanges", sess.Adapter))
		return
	}
	if sess.ConversationRef == "" {
		writeError(w, http.StatusNotFound, "no_conversation", "session has no resolvable conversation")
		return
	}
	exchanges, err := renderer.RenderConversationExchanges(sess.ConversationRef)
	if errors.Is(err, os.ErrNotExist) {
		// A known ref can precede pi's first append. It is a resolvable empty
		// timeline, not a failure to locate the conversation source.
		exchanges = nil
	} else if err != nil {
		writeError(w, http.StatusNotFound, "no_conversation", "conversation cannot be read")
		return
	}
	outcome := adapter.ExchangeSnapshot
	if sess.Active {
		// Native persistence trails the runner edge. Reconcile by user boundaries
		// before tailing so a pre-persistence read appends the live exchange rather
		// than rewriting the previous completed exchange as active. Once pi has
		// persisted a boundary, the overlap is merged instead of duplicated.
		if frame := retainedTurnFrame(boot, sessionID); frame != nil && frame.Current != nil && len(frame.Current.Exchanges) > 0 {
			exchanges = reconcileActiveExchanges(exchanges, frame.Current)
			outcome = adapter.ExchangeActive
		}
	}
	previous := 0
	if len(exchanges) > n {
		previous = len(exchanges) - n
		exchanges = exchanges[previous:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set(conversationScopeHeader, "exchanges")
	report := adapter.ExchangeReport{Exchanges: exchanges, Previous: previous, PreviousKnown: true, Outcome: outcome}
	if outcome == adapter.ExchangeActive {
		if frame := retainedTurnFrame(boot, sessionID); frame != nil && frame.Current != nil {
			report.OmittedExchanges = frame.Current.OmittedExchanges
			report.OmittedBytes = frame.Current.OmittedBytes
		}
	}
	_, _ = w.Write(adapter.RenderExchangeReport(report))
}

// reconcileActiveExchanges uses the adapter-asserted user-boundary position,
// never text equality. Identical consecutive prompts therefore remain distinct.
func reconcileActiveExchanges(native []adapter.Exchange, current *sessioncoord.TurnCurrent) []adapter.Exchange {
	if current == nil || len(current.Exchanges) == 0 {
		return append([]adapter.Exchange(nil), native...)
	}
	if current.PreviousExchanges == nil {
		// A version-skewed frame cannot be reconciled positionally. Anonymous
		// hook-driven adapters (currently Codex) also report an empty user because
		// raw PTY input has no semantic source bytes. Never append that empty frame
		// as a duplicate exchange once native storage has the real boundary.
		out := append([]adapter.Exchange(nil), native...)
		for _, ex := range current.Exchanges {
			if ex.User == "" {
				continue
			}
			out = append(out, adapter.Exchange{Ordinal: ex.Ordinal, User: ex.User, Iterations: ex.Iterations})
		}
		if len(out) > 0 {
			out[len(out)-1].Terminal = ""
		}
		return out
	}
	prior := max(0, *current.PreviousExchanges)
	// The live slice starts after every exchange evicted from its front. Native
	// history does not evict, so positional overlay begins at prior+omitted.
	start := prior + max(0, current.OmittedExchanges)
	out := append([]adapter.Exchange(nil), native...)
	for i, live := range current.Exchanges {
		idx := start + i
		if idx < len(out) {
			out[idx].Ordinal, out[idx].Iterations, out[idx].Terminal = live.Ordinal, live.Iterations, ""
			continue
		}
		out = append(out, adapter.Exchange{Ordinal: live.Ordinal, User: live.User, Iterations: live.Iterations})
	}
	// Native can advance before the frame event is drained. Keep that tail, but
	// never render persisted assistant prose as a completed terminal while the
	// source still reports active.
	if len(out) > 0 {
		out[len(out)-1].Terminal = ""
	}
	return out
}

func scrollbackBrokerHandlerCentral(w http.ResponseWriter, r *http.Request, sessionID string, sess wire.Session, ok bool, dirFor func(string) string) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	tailN := 0
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "bad_request", "tail must be a positive integer")
			return
		}
		tailN = n
	}
	rc, err := scrollback.OpenReader(dirFor(sessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusInternalServerError, "internal", "scrollback unavailable")
		return
	}
	if tailN > 0 {
		renderTail(w, rc, legacySessionFromWire(sess), sessionID, tailN)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	if rc == nil {
		return
	}
	defer rc.Close()
	_, _ = io.Copy(w, rc)
}

func handleWaitCentral(w http.ResponseWriter, r *http.Request, boot *Bootstrap, fanout *sseFanout, sessionID string, dirFor func(string) string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	// Validate parameters before the existence check so cheap rejections
	// don't need a store round-trip.
	forText := r.URL.Query().Get("for_text")
	forRegex := r.URL.Query().Get("for_regex")
	if forText != "" && forRegex != "" {
		writeError(w, http.StatusBadRequest, "bad_request", "for_text and for_regex are mutually exclusive")
		return
	}
	deadline, err := timeoutChan(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if forRegex != "" {
		if _, err := regexp.Compile(forRegex); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid regex: "+err.Error())
			return
		}
	}
	// Store-direct existence check (ADR 0026 §2a): a just-registered
	// session is visible immediately without waiting for a compose pass.
	stored, found, storeErr := boot.Store.Session(r.Context(), centralstore.SessionID(sessionID))
	if storeErr != nil || !found {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
	if forText == "" && forRegex == "" && !stored.Active {
		if _, capable := adapters.FindByAdapter(stored.Adapter).(adapter.ConversationExchangeRenderer); capable {
			writeLateExchangeWait(w, stored, retainedTurnFrame(boot, sessionID))
			return
		}
	}
	if forText != "" || forRegex != "" {
		var match func(string) bool
		if forText != "" {
			match = func(line string) bool { return strings.Contains(line, forText) }
		} else {
			re, err := regexp.Compile(forRegex)
			if err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid regex: "+err.Error())
				return
			}
			match = re.MatchString
		}
		waitForOutputCentral(w, r, boot, fanout, sessionID, dirFor(sessionID), match, deadline)
		return
	}
	_, outcomes, cancel, err := boot.SubscribeOutcomes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer cancel()
	seenAlive := false
	// The turn this wait may report the result of is identified by the ADAPTER,
	// through the runner's turn frame: the wait records the open turn's turn_seq
	// when it first sees a turn (the very first look if one is already running, or
	// a fresh inactive→active edge), and at the close it accepts the frame's
	// settled record only when that record names the same turn.
	//
	// A wait that never identifies a turn — one that finds the turn already
	// closed, or a session whose adapter asserts no turn identity — resolves
	// result-free. It still reports the conclusion; it just does not claim an
	// answer it cannot attribute.
	var observedSeq uint64
	var anchorOrdinal uint64
	active := false
	// frameFor prefers a frame stamped on the resolving outcome (retained at apply
	// time for the generation that published it) and falls back to the frame
	// retained for the live generation — which is what makes the initial-fanout
	// and 500ms-ticker paths, neither of which carries an event at all, exactly as
	// result-bearing as the outcome path.
	frameFor := func(o *sessioncoord.Outcome) *sessioncoord.TurnFrame {
		if o != nil && o.Frame != nil {
			return o.Frame
		}
		return retainedTurnFrame(boot, sessionID)
	}
	observe := func(s compatSession, o *sessioncoord.Outcome) {
		if s.Status == nil {
			return
		}
		if s.Status.Active && !active {
			frame := frameFor(o)
			if anchorOrdinal == 0 && frame != nil && frame.Current != nil && len(frame.Current.Exchanges) > 0 {
				anchorOrdinal = frame.Current.Exchanges[len(frame.Current.Exchanges)-1].Ordinal
			}
			if seq := frame.CurrentTurnSeq(); seq != 0 {
				observedSeq = seq
			}
		}
		active = s.Status.Active
	}
	closedTurn := func(o *sessioncoord.Outcome) *sessioncoord.TurnClose {
		return frameFor(o).ClosedTurn(observedSeq)
	}
	if cur, ok := visibleSession(fanout.Current().Sessions, sessionID); ok {
		legacy := legacySessionFromWire(cur)
		seenAlive = seenAlive || cur.Alive
		observe(legacy, nil)
		if verdict, done := terminalReason(legacy, seenAlive); done {
			writeWaitConclusion(w, r, boot, sessionID, verdict, closedTurn(nil), anchorOrdinal)
			return
		}
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			data := map[string]any{"reason": "timeout", "outcome": "timeout", "cause": "timeout"}
			if anchorOrdinal != 0 {
				data["anchor_ordinal"] = anchorOrdinal
			}
			if frame := retainedTurnFrame(boot, sessionID); frame != nil && frame.Current != nil {
				data["exchanges"] = frame.Current.Exchanges
				data["omitted_exchanges"] = frame.Current.OmittedExchanges
				data["omitted_bytes"] = frame.Current.OmittedBytes
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestTimeout)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
			return
		case outcome, ok := <-outcomes:
			if !ok || string(outcome.ID) != sessionID {
				continue
			}
			if outcome.Type == sessioncoord.OutcomeRemoved {
				writeWaitConclusion(w, r, boot, sessionID, diedConclusion(), runnerLossClose(retainedTurnFrame(boot, sessionID)), anchorOrdinal)
				return
			}
			if outcome.Type != sessioncoord.OutcomeUpserted || outcome.Session == nil {
				continue
			}
			legacy := legacySessionFromOutcome(*outcome.Session, outcome.Alive)
			seenAlive = seenAlive || outcome.Alive
			observe(legacy, &outcome)
			if verdict, done := terminalReason(legacy, seenAlive); done {
				writeWaitConclusion(w, r, boot, sessionID, verdict, closedTurn(&outcome), anchorOrdinal)
				return
			}
		case <-ticker.C:
			cur, ok := visibleSession(fanout.Current().Sessions, sessionID)
			if !ok {
				writeWaitConclusion(w, r, boot, sessionID, diedConclusion(), runnerLossClose(retainedTurnFrame(boot, sessionID)), anchorOrdinal)
				return
			}
			legacy := legacySessionFromWire(cur)
			seenAlive = seenAlive || cur.Alive
			observe(legacy, nil)
			if verdict, done := terminalReason(legacy, seenAlive); done {
				writeWaitConclusion(w, r, boot, sessionID, verdict, closedTurn(nil), anchorOrdinal)
				return
			}
		}
	}
}

// writeWaitConclusion answers a resolved turn wait, attaching the turn's
// asserted answer when — and only when — the turn completed normally AND the
// adapter's settled record names the turn this wait observed.
//
// close is that record, or nil for every result-free resolution: a death, a
// session whose adapter asserts no turn identity (a shell, Claude, Codex), a raw
// `PUT /status` close, a version-skewed runner that never sent a frame, or two
// back-to-back turns between looks. Result-free is a normal, honest completion —
// not an error and never a hang — because those sessions are legitimately
// waitable.
//
// Nothing is re-read from the conversation here: the bounded source frame is
// the observed activity, including partial terminal prose and diagnostics.
// retainedTurnFrame reads the turn frame gmuxd retains for a session's live
// generation. It is indirected through a variable because the guarantee under
// test is that the frame-less resolution paths (the initial fanout look and the
// 500 ms ticker, neither of which carries an event) serve results from the
// RETAINED frame — and a test cannot install a frame through a registry it does
// not own a runner for.
//
// Tests reassign it (restoring it with t.Cleanup), so it is parallel-unsafe: no
// test touching it may call t.Parallel. Production never writes it.
var retainedTurnFrame = func(boot *Bootstrap, sessionID string) *sessioncoord.TurnFrame {
	if boot == nil || boot.Registry == nil {
		return nil
	}
	return boot.Registry.Frame(centralstore.SessionID(sessionID))
}

func writeWaitConclusion(w http.ResponseWriter, _ *http.Request, _ *Bootstrap, _ string, verdict waitConclusion, close *sessioncoord.TurnClose, anchorOrdinal uint64) {
	data := map[string]any{"reason": verdict.Reason}
	if anchorOrdinal != 0 {
		data["anchor_ordinal"] = anchorOrdinal
	}
	if verdict.Outcome != "" {
		data["outcome"] = verdict.Outcome
	}
	if verdict.Cause != "" {
		data["cause"] = verdict.Cause
	}
	if close != nil {
		if len(close.Exchanges) == 0 && close.Trigger != "" {
			data["trigger"] = close.Trigger
		}
		if close.PreviousExchanges != nil {
			data["previous_exchanges"] = *close.PreviousExchanges
		}
		data["exchanges"] = close.Exchanges
		data["omitted_exchanges"] = close.OmittedExchanges
		data["omitted_bytes"] = close.OmittedBytes
		if close.Diagnostic != "" {
			data["diagnostic"] = close.Diagnostic
		}
		if close.Output != "" {
			data["output"] = close.Output
			if close.Truncated {
				data["truncated"] = true
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true, "data": data})
}

func runnerLossClose(frame *sessioncoord.TurnFrame) *sessioncoord.TurnClose {
	if frame == nil || frame.Current == nil {
		return nil
	}
	cur := frame.Current
	// Runner loss has no durable message identity with which to attribute native
	// prose to this activity. Text equality is insufficient (a repeated prompt
	// can select an old answer), so the honest degraded report carries no partial.
	return &sessioncoord.TurnClose{TurnSeq: cur.TurnSeq, Outcome: outcomeError, PreviousExchanges: cur.PreviousExchanges,
		Exchanges: append([]sessioncoord.TurnExchange(nil), cur.Exchanges...), OmittedExchanges: cur.OmittedExchanges,
		OmittedBytes: cur.OmittedBytes, Diagnostic: "agent activity was lost"}
}

func writeLateExchangeWait(w http.ResponseWriter, sess centralstore.Session, frame *sessioncoord.TurnFrame) {
	renderer, ok := adapters.FindByAdapter(sess.Adapter).(adapter.ConversationExchangeRenderer)
	if !ok || sess.ConversationRef == "" {
		writeError(w, http.StatusNotFound, "no_conversation", "session has no resolvable conversation")
		return
	}
	exchanges, err := renderer.RenderConversationExchanges(sess.ConversationRef)
	if errors.Is(err, os.ErrNotExist) {
		exchanges = nil
	} else if err != nil {
		writeError(w, http.StatusNotFound, "no_conversation", "conversation cannot be read")
		return
	}
	previous := 0
	if len(exchanges) > 1 {
		previous = len(exchanges) - 1
		exchanges = exchanges[len(exchanges)-1:]
	}
	data := map[string]any{"reason": "idle", "outcome": string(adapter.ExchangeSnapshot), "exchanges": exchanges, "previous_exchanges": previous}
	if sess.StatusReported {
		outcome := classifyTurnClose(sess.Error, sess.Interrupted)
		data["outcome"] = outcome
		if outcome != outcomeCompleted {
			if len(exchanges) > 0 && exchanges[len(exchanges)-1].Terminal != "" {
				data["terminal_partial"] = true
			}
			if frame != nil && frame.Last != nil && frame.Last.Diagnostic != "" {
				data["diagnostic"] = frame.Last.Diagnostic
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true, "data": data})
}

func waitForOutputCentral(w http.ResponseWriter, r *http.Request, boot *Bootstrap, fanout *sseFanout, sessionID, dir string, match func(string) bool, deadline <-chan time.Time) {
	_, outcomes, cancel, err := boot.SubscribeOutcomes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer cancel()
	var lastSig scrollbackSig
	rendered := false
	seenAlive := false
	check := func(cur wire.Session) bool {
		sig := statScrollback(dir)
		if rendered && sig == lastSig {
			return false
		}
		lastSig, rendered = sig, true
		return outputMatchesCentral(dir, cur, match)
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		cur, ok := visibleSession(fanout.Current().Sessions, sessionID)
		if ok && check(cur) {
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "matched"}})
			return
		}
		if ok {
			legacy := legacySessionFromWire(cur)
			seenAlive = seenAlive || cur.Alive
			if !cur.Alive && hasRunEvidence(legacy, seenAlive) {
				if check(cur) {
					writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "matched"}})
					return
				}
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "died"}})
				return
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-deadline:
			writeError(w, http.StatusRequestTimeout, "timeout", "session output did not match within timeout")
			return
		case outcome, ok := <-outcomes:
			if !ok || string(outcome.ID) != sessionID {
				continue
			}
			if outcome.Type == sessioncoord.OutcomeRemoved {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "died"}})
				return
			}
		case <-ticker.C:
		}
	}
}

func outputMatchesCentral(dir string, sess wire.Session, match func(string) bool) bool {
	rc, err := scrollback.OpenReader(dir)
	if err != nil || rc == nil {
		return false
	}
	defer rc.Close()
	cols, rows := int(sess.TerminalCols), int(sess.TerminalRows)
	if rows <= 0 {
		rows = 24
	}
	lines, err := scrollback.RenderTail(rc, cols, rows, scrollback.RenderScrollbackSize+rows)
	if err != nil {
		return false
	}
	for _, line := range lines {
		if match(line) {
			return true
		}
	}
	return false
}

func handleInputWaitCentral(w http.ResponseWriter, r *http.Request, boot *Bootstrap, fanout *sseFanout, sessionID string, body []byte, send func() error) {
	if mode := r.URL.Query().Get("wait"); mode != "idle" {
		writeError(w, http.StatusBadRequest, "bad_request", "unsupported wait mode "+strconv.Quote(mode)+`; expected "idle"`)
		return
	}
	if !inputSubmits(body) {
		writeError(w, http.StatusUnprocessableEntity, "input_no_submit", "input does not submit (no carriage return \\r or Enter key sequence; a bare newline \\n is treated as literal text, not a submit); add a trailing Enter key or drop --wait")
		return
	}
	deadline, err := timeoutChan(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	_, outcomes, cancel, err := boot.SubscribeOutcomes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	defer cancel()
	if err := send(); err != nil {
		writeError(w, http.StatusBadGateway, "runner_unreachable", err.Error())
		return
	}
	verdict, timedOut := awaitTurnCentral(r.Context(), fanout, outcomes, sessionID, deadline)
	switch {
	case timedOut:
		writeError(w, http.StatusRequestTimeout, "timeout", "session did not become idle within timeout")
	case verdict.Reason != "":
		// Raw send --wait reports the turn's CONCLUSION but never a result: the
		// bytes it delivered are keystrokes, and it makes no claim about which
		// agent turn (if any) they belong to. The conclusion is still needed —
		// without it an intentionally interrupted turn would exit 0 through the
		// composition the docs call preferred, while `gmux wait` exits 2. Passing
		// no close record is what keeps it result-free.
		writeWaitConclusion(w, r, boot, sessionID, verdict, nil, 0)
	}
}

func awaitTurnCentral(ctx context.Context, fanout *sseFanout, outcomes <-chan sessioncoord.Outcome, sessionID string, deadline <-chan time.Time) (waitConclusion, bool) {
	seenActive := false
	// Turn-close classification is the shared one (classifyTurnClose), so a
	// fused send --wait and a plain `gmux wait` cannot disagree about whether a
	// turn completed, failed or was intentionally stopped.
	closed := func(s compatSession) waitConclusion {
		return waitConclusion{Reason: "idle", Outcome: classifyTurnClose(s.Status.Error, s.Status.Interrupted)}
	}
	check := func(s compatSession) (waitConclusion, bool) {
		if !s.Alive {
			if seenActive && s.Status != nil && !s.Status.Active {
				return closed(s), true
			}
			return diedConclusion(), true
		}
		if s.Status != nil && s.Status.Active {
			seenActive = true
			return waitConclusion{}, false
		}
		if seenActive && s.Status != nil && !s.Status.Active {
			return closed(s), true
		}
		return waitConclusion{}, false
	}
	if cur, ok := visibleSession(fanout.Current().Sessions, sessionID); !ok {
		return diedConclusion(), false
	} else if verdict, done := check(legacySessionFromWire(cur)); done {
		return verdict, false
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return waitConclusion{}, false
		case <-deadline:
			return waitConclusion{}, true
		case outcome, ok := <-outcomes:
			if !ok || string(outcome.ID) != sessionID {
				continue
			}
			if outcome.Type == sessioncoord.OutcomeRemoved {
				return diedConclusion(), false
			}
			if outcome.Type != sessioncoord.OutcomeUpserted || outcome.Session == nil {
				continue
			}
			if verdict, done := check(legacySessionFromOutcome(*outcome.Session, outcome.Alive)); done {
				return verdict, false
			}
		case <-ticker.C:
			cur, ok := visibleSession(fanout.Current().Sessions, sessionID)
			if !ok {
				return diedConclusion(), false
			}
			if verdict, done := check(legacySessionFromWire(cur)); done {
				return verdict, false
			}
		}
	}
}
