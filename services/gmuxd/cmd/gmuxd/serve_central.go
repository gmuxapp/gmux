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

func serveCentral(stderr io.Writer) int {
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
	sessionDirs := sessionmeta.New(sessionmeta.DefaultDir(), sessionmeta.WithRetention(sessionmeta.DefaultRetention()))
	convIndex := conversations.New()
	convIndex.Snapshot()
	log.Printf("conversations: indexed %d conversations", convIndex.Count())

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

	takeover := func(context.Context) error {
		sock := paths.SocketPath()
		if _, ok := unixipc.HealthIdentity(sock); !ok {
			return nil
		}
		if !unixipc.Shutdown(sock) {
			return fmt.Errorf("existing daemon at %s did not shut down", sock)
		}
		return nil
	}
	storeHandle, storeLock, err := bootstrapOwnership(context.Background(), stateDir, takeover)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "gmuxd: %v\n", err)
		return 1
	}
	defer storeHandle.Close()
	defer storeLock.Close()

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

	converter := &wire.Converter{Titlers: make(map[string]func([]string) string), ResumeCommand: func(adapterName, ref string) []string {
		legacy := &compatSession{Adapter: adapterName, ConversationRef: ref}
		return discovery.ResolveResumeCommandFor(legacy.Adapter, legacy.ConversationRef)
	}, IsLocalPeer: func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) }}
	for _, a := range adapters.All {
		if titler, ok := a.(adapter.CommandTitler); ok {
			converter.Titlers[a.Name()] = titler.CommandTitle
		}
	}
	if fallback := adapters.DefaultFallback(); fallback != nil {
		if titler, ok := fallback.(adapter.CommandTitler); ok {
			converter.Titlers[fallback.Name()] = titler.CommandTitle
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
			frames := fanout.Current()
			health := peerAdapter.health()
			if frames.World != nil && frames.World.Health != nil {
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
		mux.HandleFunc("GET /v1/projects", func(w http.ResponseWriter, r *http.Request) {
			frames := fanout.Current()
			state := projectStateFromWorld(frames.World)
			infos := buildSessionInfosWire(frames.Sessions, func(name string) bool { return peerManager != nil && peerManager.IsLocalPeer(name) })
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"configured": state.Items, "discovered": state.Discovered(infos), "unmatched_active_count": state.UnmatchedActiveCount(infos)}})
		})
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
			frames := fanout.Current()
			state := projectStateFromWorld(frames.World)
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
			frames := fanout.Current()
			if frames.Sessions == nil {
				frames = boot.Cache.Current()
			}
			// The CLI (`gmux ls`/`kill`/`attach`/... via fetchSessions) and the
			// legacy daemon contract expect `data` to be a flat JSON array of
			// sessions, not the SSE snapshot's {"sessions":[...]} envelope. Unwrap
			// so `data` is the array. See tools/dev-container regression.
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
		if !ok {
			if _, found, err := boot.Store.Session(r.Context(), sid); err != nil || !found {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
		}
		socketPath := sess.SocketPath
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
		scrollbackBrokerHandlerCentral(w, r, sessionID, sess, ok, sessionDirs.SessionDir)
	case "conversation":
		conversationHandlerCentral(w, r, sessionID, boot.Store)
	case "clipboard":
		if !ok {
			if _, found, err := boot.Store.Session(r.Context(), sid); err != nil || !found {
				writeError(w, http.StatusNotFound, "not_found", "session not found")
				return
			}
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

func conversationHandlerCentral(w http.ResponseWriter, r *http.Request, sessionID string, sessions *centralstore.Store) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "bad_request", "method not allowed")
		return
	}
	sess, ok, err := sessions.Session(r.Context(), centralstore.SessionID(sessionID))
	if err != nil || !ok {
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
	if sess.ConversationRef == "" {
		writeError(w, http.StatusNotFound, "no_conversation", "session has no conversation")
		return
	}
	a := adapters.FindByAdapter(sess.Adapter)
	renderer, ok := a.(adapter.ConversationRenderer)
	if !ok {
		writeError(w, http.StatusNotFound, "no_conversation", "adapter does not render conversations")
		return
	}
	msgs, err := renderer.RenderConversation(sess.ConversationRef)
	if err != nil {
		writeError(w, http.StatusNotFound, "no_conversation", "conversation is gone")
		return
	}
	if len(msgs) == 0 {
		writeError(w, http.StatusNotFound, "no_conversation", "conversation has no messages yet")
		return
	}
	if tailN > 0 && len(msgs) > tailN {
		msgs = msgs[len(msgs)-tailN:]
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(formatConversationMarkdown(msgs))
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
	if _, ok := visibleSession(fanout.Current().Sessions, sessionID); !ok {
		writeError(w, http.StatusNotFound, "not_found", "session not found")
		return
	}
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
	if cur, ok := visibleSession(fanout.Current().Sessions, sessionID); ok {
		legacy := legacySessionFromWire(cur)
		seenAlive = seenAlive || cur.Alive
		if reason, done := terminalReason(legacy, seenAlive); done {
			writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
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
			writeError(w, http.StatusRequestTimeout, "timeout", "session did not become idle within timeout")
			return
		case outcome, ok := <-outcomes:
			if !ok || string(outcome.ID) != sessionID {
				continue
			}
			if outcome.Type == sessioncoord.OutcomeRemoved {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "died"}})
				return
			}
			if outcome.Type != sessioncoord.OutcomeUpserted || outcome.Session == nil {
				continue
			}
			legacy := legacySessionFromOutcome(*outcome.Session, outcome.Alive)
			seenAlive = seenAlive || outcome.Alive
			if reason, done := terminalReason(legacy, seenAlive); done {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
				return
			}
		case <-ticker.C:
			cur, ok := visibleSession(fanout.Current().Sessions, sessionID)
			if !ok {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": "died"}})
				return
			}
			legacy := legacySessionFromWire(cur)
			seenAlive = seenAlive || cur.Alive
			if reason, done := terminalReason(legacy, seenAlive); done {
				writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
				return
			}
		}
	}
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
	reason, timedOut := awaitTurnCentral(r.Context(), fanout, outcomes, sessionID, deadline)
	switch {
	case timedOut:
		writeError(w, http.StatusRequestTimeout, "timeout", "session did not become idle within timeout")
	case reason != "":
		writeJSON(w, map[string]any{"ok": true, "data": map[string]any{"reason": reason}})
	}
}

func awaitTurnCentral(ctx context.Context, fanout *sseFanout, outcomes <-chan sessioncoord.Outcome, sessionID string, deadline <-chan time.Time) (string, bool) {
	seenWorking := false
	check := func(s compatSession) (string, bool) {
		if !s.Alive {
			if seenWorking && s.Status != nil && !s.Status.Working {
				return "idle", true
			}
			return "died", true
		}
		if s.Status != nil && s.Status.Working {
			seenWorking = true
			return "", false
		}
		if seenWorking && s.Status != nil && !s.Status.Working {
			return "idle", true
		}
		return "", false
	}
	if cur, ok := visibleSession(fanout.Current().Sessions, sessionID); !ok {
		return "died", false
	} else if reason, done := check(legacySessionFromWire(cur)); done {
		return reason, false
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", false
		case <-deadline:
			return "", true
		case outcome, ok := <-outcomes:
			if !ok || string(outcome.ID) != sessionID {
				continue
			}
			if outcome.Type == sessioncoord.OutcomeRemoved {
				return "died", false
			}
			if outcome.Type != sessioncoord.OutcomeUpserted || outcome.Session == nil {
				continue
			}
			if reason, done := check(legacySessionFromOutcome(*outcome.Session, outcome.Alive)); done {
				return reason, false
			}
		case <-ticker.C:
			cur, ok := visibleSession(fanout.Current().Sessions, sessionID)
			if !ok {
				return "died", false
			}
			if reason, done := check(legacySessionFromWire(cur)); done {
				return reason, false
			}
		}
	}
}
