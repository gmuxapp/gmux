package statetool

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"path/filepath"

	"github.com/gmuxapp/gmux/services/gmuxd/internal/centralstore"
)

// Handler serves the /v1/state/* admin routes. Store is optional: a daemon
// that has no central store open (pre-cutover production) registers the
// routes with a nil store and every call answers 503
// "central store not active" (design S3: routes registered but inert).
//
// These routes are Unix-socket-only. The enforcement is netauth.Middleware
// (which wraps both the TCP and tailscale listeners) blocking the
// /v1/state/ prefix, like /v1/shutdown; Register only ever runs against the
// shared mux behind that gate.
type Handler struct {
	Store *centralstore.Store
}

// Register installs the state routes onto mux. Production wiring
// (cmd/gmuxd) happens at the cutover switch (S5).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/state/check", h.check)
	mux.HandleFunc("POST /v1/state/backup", h.backup)
	mux.HandleFunc("GET /v1/state/export", h.export)
}

func (h *Handler) store(w http.ResponseWriter) (*centralstore.Store, bool) {
	if h.Store == nil {
		writeStateError(w, http.StatusServiceUnavailable, "central_store_not_active",
			"the central store is not active on this daemon")
		return nil, false
	}
	return h.Store, true
}

func (h *Handler) check(w http.ResponseWriter, r *http.Request) {
	store, ok := h.store(w)
	if !ok {
		return
	}
	findings, err := store.CheckState(r.Context())
	if err != nil {
		log.Printf("statetool: check: %v", err)
		writeStateError(w, http.StatusInternalServerError, "check_failed", err.Error())
		return
	}
	writeStateJSON(w, ReportFor(findings))
}

// BackupResult is the wire shape of a completed backup.
type BackupResult struct {
	Path string `json:"path"`
	// Note reminds the caller that the artifact is a secret (peer tokens).
	Note string `json:"note"`
}

// BackupNote is the secret warning attached to every backup result.
const BackupNote = "backup contains peer tokens; keep the file private"

func (h *Handler) backup(w http.ResponseWriter, r *http.Request) {
	store, ok := h.store(w)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeStateError(w, http.StatusBadRequest, "invalid_request", "body must be {\"path\": \"<target file>\"}")
		return
	}
	if !filepath.IsAbs(req.Path) {
		// The path is interpreted by the daemon process; a relative path
		// would silently resolve against the daemon's cwd, not the CLI's.
		writeStateError(w, http.StatusBadRequest, "invalid_request", "backup path must be absolute")
		return
	}
	if err := store.BackupInto(r.Context(), req.Path); err != nil {
		if errors.Is(err, centralstore.ErrBackupTargetExists) {
			writeStateError(w, http.StatusConflict, "target_exists", err.Error())
			return
		}
		log.Printf("statetool: backup: %v", err)
		writeStateError(w, http.StatusInternalServerError, "backup_failed", err.Error())
		return
	}
	writeStateJSON(w, BackupResult{Path: req.Path, Note: BackupNote})
}

func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	store, ok := h.store(w)
	if !ok {
		return
	}
	doc, err := Export(r.Context(), store)
	if err != nil {
		log.Printf("statetool: export: %v", err)
		writeStateError(w, http.StatusInternalServerError, "export_failed", err.Error())
		return
	}
	writeStateJSON(w, doc)
}

func writeStateJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}

func writeStateError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":    false,
		"error": map[string]any{"code": code, "message": message},
	})
}
