// Package server exposes the bot's HTTP surface: health/readiness probes for
// belmo, a status snapshot, and an admin CRUD endpoint for manual signature
// maintenance (gated by BOT_ADMIN_TOKEN).
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"adbot/internal/ai"
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
	// Amend3 additions (all optional-capable via the narrow interface).
	ExtraStats() (localFallback, simhashHits int64)
	PendingQueueLen() int
	AIStatsSnapshot() AISnapshot
	// Amend4: notification subsystem counters (notify.target_ok etc.).
	NotifySnapshot() NotifyStatsView
	// Amend6 A: async judgement-pool gauges (nil-safe zero view when the
	// escape hatch is on).
	JudgePoolView() JudgePoolView
}

// JudgePoolView is the judge-pool counters the server renders (plain data so
// server does not import bot).
type JudgePoolView struct {
	Async          bool
	Concurrency    int64
	Queued         int64
	Inflight       int64
	QueueCap       int64
	EnqueuedTotal  int64
	QueueFullTotal int64
	UpdateWorkers  int
}

// NotifyStatsView is the notify counters the server renders (plain data so
// server does not import notify).
type NotifyStatsView struct {
	Enabled       bool
	TargetOK      bool
	TargetChecked bool
	Sent          int64
	Dropped       int64
	Suppressed    int64
	QueueLen      int
	QueueCap      int
}

// AISnapshot is the AI-side counters the server renders (kept as plain data
// so server does not import ai).
type AISnapshot struct {
	ModelCalls    map[string]int64
	ModelOKs      map[string]int64
	Fallbacks     int64
	VerifyRounds  int64
	VerifyFlipped int64
	LocalFallback int64
}

// SweepReporter feeds the janitor's last/next sweep into /statusz.
type SweepReporter interface {
	SweepSnapshot() (at time.Time, next time.Time, droppedSuspects, droppedSigs, remaining int64, ran bool)
}

// Server carries dependencies for building the mux.
type Server struct {
	Store      store.Store
	Bot        Inspector
	Sweeper    SweepReporter // nil = no janitor wired
	Cfg        config.Config
	AdminToken string
	Version    string
	StartedAt  time.Time
	Log        *logx.Logger
}

// New builds a Server from the full config.
func New(st store.Store, bot Inspector, sweep SweepReporter, cfg config.Config, log *logx.Logger) *Server {
	if log == nil {
		log = logx.New(nil, logx.Info, "text", "http")
	}
	return &Server{
		Store:      st,
		Bot:        bot,
		Sweeper:    sweep,
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
	localFallback, simhashHits := s.Bot.ExtraStats()
	pending := s.Bot.PendingQueueLen()
	aiSnap := s.Bot.AIStatsSnapshot()

	// Retention/sweep reporting: the janitor pushes each sweep result into the
	// shared SweepState (main wires it in via the Sweeper field).
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
		// amend3 additions ---------------------------------------------------
		"ai_models":              s.Cfg.AIEffectiveModels(),
		"ai_base_url_host":       s.Cfg.AIBaseURLHost(),
		"ai_base_url_configured": s.Cfg.AIBaseURLConfigured(),
		"ai_model_stats": map[string]any{
			"calls": aiSnap.ModelCalls,
			"oks":   aiSnap.ModelOKs,
		},
		"ai_fallbacks":         aiSnap.Fallbacks,
		"ai_verify_rounds":     aiSnap.VerifyRounds,
		"ai_verify_flipped":    aiSnap.VerifyFlipped,
		"local_fallback_count": localFallback,
		"simhash_hits":         simhashHits,
		"pending_queue":        pending,
		"prompt_version":       ai.PromptVersion,
	}

	// amend4: notify subsystem block (always present; enabled=false when the
	// target chat is not configured).
	ns := s.Bot.NotifySnapshot()
	nb := map[string]any{
		"enabled":        ns.Enabled,
		"target_ok":      ns.TargetOK,
		"target_checked": ns.TargetChecked,
		"sent":           ns.Sent,
		"dropped":        ns.Dropped,
		"suppressed":     ns.Suppressed,
		"queue_len":      ns.QueueLen,
		"queue_cap":      ns.QueueCap,
	}
	if ns.TargetChecked {
		body["notify.target_ok"] = ns.TargetOK
	}
	body["notify"] = nb

	// amend6 A: judgement-pool gauges (update_workers mirrors
	// BOT_UPDATE_WORKERS from the config).
	jv := s.Bot.JudgePoolView()
	body["judge_async"] = jv.Async
	body["judge_concurrency"] = jv.Concurrency
	body["judge_queued"] = jv.Queued
	body["judge_inflight"] = jv.Inflight
	body["judge_queue_cap"] = jv.QueueCap
	body["judge_enqueued_total"] = jv.EnqueuedTotal
	body["judge_queue_full_total"] = jv.QueueFullTotal
	body["update_workers"] = jv.UpdateWorkers
	// The sweeper block only appears when the janitor has run at least once.
	if s.Sweeper != nil {
		at, next, droppedSus, droppedSigs, remaining, ran := s.Sweeper.SweepSnapshot()
		if ran {
			body["last_sweep"] = map[string]any{
				"at":                 at.Format(time.RFC3339),
				"dropped_suspects":   droppedSus,
				"dropped_signatures": droppedSigs,
				"remaining_total":    remaining,
			}
			if next.After(time.Now()) {
				body["next_sweep_in"] = next.Sub(time.Now()).String()
			}
		}
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
