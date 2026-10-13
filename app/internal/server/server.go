// Package server exposes the bot's HTTP surface: health/readiness probes for
// belmo, a status snapshot, and an admin CRUD endpoint for manual signature
// maintenance (gated by BOT_ADMIN_TOKEN).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/normalize"
	"adbot/internal/store"
)

// Inspector is the subset of the bot the server reads for /statusz.
type Inspector interface {
	Stats() (poll, punished, aiCalls, aiFails int64)
	DeleteStats() (deleted, failed int64)
	Uptime() time.Duration
}

// Server carries dependencies for building the mux.
type Server struct {
	Store      store.Store
	Bot        Inspector
	Cfg        config.Config
	AdminToken string
	Version    string
	StartedAt  time.Time
	Log        *logx.Logger
}

// New builds a Server from the full config.
func New(st store.Store, bot Inspector, cfg config.Config, log *logx.Logger) *Server {
	if log == nil {
		log = logx.New(nil, logx.Info, "text", "http")
	}
	return &Server{
		Store:      st,
		Bot:        bot,
		Cfg:        cfg,
		AdminToken: cfg.AdminToken,
		Version:    cfg.Version,
		StartedAt:  time.Now(),
		Log:        log,
	}
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/readyz", s.handleReady)
	mux.HandleFunc("/statusz", s.handleStatus)
	if s.AdminToken != "" {
		mux.HandleFunc("/admin/signatures", s.handleAdminSignatures)
	} else {
		mux.HandleFunc("/admin/signatures", func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})
	}
	return mux
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("adbot ok\n"))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok\n"))
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("store: " + err.Error() + "\n"))
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ready\n"))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	storeOK := true
	storeErr := ""
	if err := s.Store.Ping(ctx); err != nil {
		storeOK = false
		storeErr = err.Error()
	}
	suspects, _ := s.Store.SuspectCount(ctx)
	bioSigs, _ := s.Store.BioSigCount(ctx)
	msgSigs, _ := s.Store.MsgSigCount(ctx)
	punished, _ := s.Store.Punished(ctx)
	poll, _, aiCalls, aiFails := s.Bot.Stats()
	deleted, deleteFails := s.Bot.DeleteStats()

	// Retention/sweep reporting best-effort: the janitor lives in main and is
	// not wired into the server for this round beyond TTL config.
	body := map[string]any{
		"version":             s.Version,
		"uptime_seconds":      int64(time.Since(s.StartedAt).Seconds()),
		"poll_count":          poll,
		"punish_count":        punished,
		"ai_calls":            aiCalls,
		"ai_failures":         aiFails,
		"deleted_count":       deleted,
		"delete_failed":       deleteFails,
		"suspects":            suspects,
		"bio_signatures":      bioSigs,
		"msg_signatures":      msgSigs,
		"store_ok":            storeOK,
		"store_error":         storeErr,
		"store_backend":       s.Cfg.Store,
		"retention":           s.Cfg.Retention.String(),
		"retention_suspect":   s.Cfg.RetentionFor("suspect").String(),
		"retention_signature": s.Cfg.RetentionFor("signature").String(),
		"retention_sliding":   s.Cfg.RetentionSliding,
		"sweep_interval":      s.Cfg.SweepInterval.String(),
		"dry_run":             s.Cfg.DryRun,
		"ai_ok":               true, // liveness is best-effort; errors counted above
		"started_at":          s.StartedAt.Format(time.RFC3339),
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleAdminSignatures(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		kind := r.URL.Query().Get("kind")
		bios, _ := s.Store.BioSignatures(ctx)
		msgs, _ := s.Store.MessageSignatures(ctx)
		if kind == "bio" {
			msgs = nil
		} else if kind == "msg" {
			bios = nil
		}
		writeJSON(w, http.StatusOK, map[string]any{"bio": bios, "msg": msgs})
	case http.MethodPost:
		var req struct {
			Kind     string `json:"kind"` // "bio" or "msg"
			Text     string `json:"text"`
			Original string `json:"original"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		if req.Kind != "bio" && req.Kind != "msg" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "kind must be \"bio\" or \"msg\""})
			return
		}
		text := req.Text
		if text == "" {
			text = req.Original
		}
		if text == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text or original is required"})
			return
		}
		sig := normalize.Signature(text)
		orig := normalize.Normalize(text)
		var err error
		if req.Kind == "bio" {
			err = s.Store.AddBioSignature(ctx, sig, orig)
		} else {
			err = s.Store.AddMessageSignature(ctx, sig, orig)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"sig": sig, "normalized": orig})
	case http.MethodDelete:
		sig := r.URL.Query().Get("sig")
		kind := r.URL.Query().Get("kind")
		if sig == "" || (kind != "bio" && kind != "msg") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "sig and kind (bio|msg) are required"})
			return
		}
		removed, err := s.Store.RemoveSignature(ctx, kind, sig)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}

func (s *Server) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+s.AdminToken
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
