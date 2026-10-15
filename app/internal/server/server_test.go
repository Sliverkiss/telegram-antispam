package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"adbot/internal/config"
	"adbot/internal/logx"
	"adbot/internal/store"
)

// stubBot implements the full Inspector contract (amend3: ExtraStats,
// PendingQueueLen, AIStatsSnapshot on top of the round-2 counters).
type stubBot struct {
	poll, punished, aiCalls, aiFails int64
	localFallback, simhash           int64
	pending                          int
}

func (s *stubBot) Stats() (int64, int64, int64, int64) {
	return s.poll, s.punished, s.aiCalls, s.aiFails
}

func (s *stubBot) DeleteStats() (int64, int64) { return 0, 0 }
func (s *stubBot) Uptime() time.Duration       { return time.Minute }
func (s *stubBot) ExtraStats() (int64, int64) {
	return s.localFallback, s.simhash
}
func (s *stubBot) PendingQueueLen() int { return s.pending }
func (s *stubBot) AIStatsSnapshot() AISnapshot {
	return AISnapshot{
		ModelCalls:    map[string]int64{"a-model": 4, "b-model": 2},
		ModelOKs:      map[string]int64{"a-model": 3, "b-model": 1},
		Fallbacks:     2,
		VerifyRounds:  1,
		VerifyFlipped: 1,
		LocalFallback: 1,
	}
}
func (s *stubBot) NotifySnapshot() NotifyStatsView {
	return NotifyStatsView{Enabled: true, TargetOK: true, TargetChecked: true, Sent: 7}
}
func (s *stubBot) JudgePoolView() JudgePoolView {
	return JudgePoolView{Async: true, Concurrency: 8, Queued: 2, Inflight: 1,
		QueueCap: 500, EnqueuedTotal: 42, QueueFullTotal: 0, UpdateWorkers: 16}
}
func (s *stubBot) AnnounceView() AnnounceView {
	return AnnounceView{Enabled: true, SentTotal: 5, FallbackTotal: 2, ErrorTotal: 0}
}
func (s *stubBot) MsgTTLView() MsgTTLView {
	return MsgTTLView{TTL: "1m0s", ScheduledTotal: 3, DeletedTotal: 2, ErrorTotal: 1}
}
func (s *stubBot) JoinRuleView() JoinRuleView {
	return JoinRuleView{Enabled: true, Hits: 4}
}
func (s *stubBot) JoinVerifyView() JoinVerifyView {
	return JoinVerifyView{Enabled: true, Pending: 2, PassedTotal: 9, TimeoutTotal: 3, SkippedTotal: 7}
}
func (s *stubBot) CommentBioView() CommentBioView {
	return CommentBioView{Enabled: true, TriggeredTotal: 11, FlashHitsTotal: 4, FlashCachedTotal: 2, FlashTTL: "30m0s"}
}
func (s *stubBot) Amend13View() Amend13View {
	return Amend13View{MinPunishScore: 85, BelowScoreReleasesTotal: 6}
}

// fakeSweep reports a fixed (already-run) sweep for /statusz rendering.
type fakeSweep struct{}

func (fakeSweep) SweepSnapshot() (time.Time, time.Time, int64, int64, int64, bool) {
	at := time.Now().Add(-time.Minute)
	next := time.Now().Add(9 * time.Minute)
	return at, next, 2, 3, 42, true
}

// neverSweep reports that the janitor has not run yet.
type neverSweep struct{}

func (neverSweep) SweepSnapshot() (time.Time, time.Time, int64, int64, int64, bool) {
	return time.Time{}, time.Time{}, 0, 0, 0, false
}

func newTestServer(t *testing.T, adminToken string) (*Server, *httptest.Server) {
	t.Helper()
	return newTestServerWithSweep(t, adminToken, nil)
}

func newTestServerWithSweep(t *testing.T, adminToken string, sweep SweepReporter) (*Server, *httptest.Server) {
	t.Helper()
	st := store.NewMemory()
	for i := 0; i < 7; i++ {
		st.RecordPunishment(context.Background(), store.Punishment{UID: int64(i), Action: "ban"})
	}
	cfg := config.Config{Store: "memory", AdminToken: adminToken, Version: "test", Retention: 0}
	// local_fallback_count comes from ExtraStats; the AI-side snapshot's
	// LocalFallback is the ai-package view of the same counter.
	s := New(st, &stubBot{punished: 7, aiCalls: 3, localFallback: 1}, sweep, cfg, logx.New(nil, logx.Error, "text", "http"))
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func get(t *testing.T, url string) *http.Response {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestHealthRootReady(t *testing.T) {
	_, ts := newTestServer(t, "")
	// GET / and /healthz → 200.
	for _, path := range []string{"/", "/healthz", "/readyz"} {
		resp := get(t, ts.URL+path)
		if resp.StatusCode != 200 {
			t.Errorf("%s → %d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

func TestStatusz(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["punish_count"] != float64(7) {
		t.Errorf("punish_count = %v", body["punish_count"])
	}
	if body["ai_calls"] != float64(3) {
		t.Errorf("ai_calls = %v", body["ai_calls"])
	}
	if _, ok := body["version"]; !ok {
		t.Error("statusz missing version")
	}
	if _, ok := body["store_backend"]; !ok {
		t.Error("statusz missing store_backend")
	}
	// Without a sweeper the sweep block stays absent.
	if _, ok := body["last_sweep"]; ok {
		t.Error("last_sweep must be absent without a sweeper")
	}
}

// TestStatuszAmend3Fields pins the round-3 /statusz contract: model chain
// stats, vote counters, local-fallback/simhash counters, pending queue,
// prompt_version, and the janitor sweep block.
func TestStatuszAmend3Fields(t *testing.T) {
	_, ts := newTestServerWithSweep(t, "", fakeSweep{})
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["prompt_version"] != "judge_v2" {
		t.Errorf("prompt_version = %v", body["prompt_version"])
	}
	if body["ai_fallbacks"] != float64(2) {
		t.Errorf("ai_fallbacks = %v", body["ai_fallbacks"])
	}
	if body["ai_verify_rounds"] != float64(1) || body["ai_verify_flipped"] != float64(1) {
		t.Errorf("vote counters = %v/%v", body["ai_verify_rounds"], body["ai_verify_flipped"])
	}
	// amend5 B: /report is gone — report_count must not be rendered at all.
	if _, ok := body["report_count"]; ok {
		t.Error("report_count must be absent after the /report removal")
	}
	if body["local_fallback_count"] != float64(1) {
		t.Errorf("local_fallback_count = %v", body["local_fallback_count"])
	}
	if body["simhash_hits"] != float64(0) {
		t.Errorf("simhash_hits = %v", body["simhash_hits"])
	}
	if body["pending_queue"] != float64(0) {
		t.Errorf("pending_queue = %v", body["pending_queue"])
	}
	models, ok := body["ai_model_stats"].(map[string]any)
	if !ok {
		t.Fatalf("ai_model_stats missing: %v", body["ai_model_stats"])
	}
	calls, ok := models["calls"].(map[string]any)
	if !ok || calls["a-model"] != float64(4) {
		t.Errorf("ai_model_stats.calls = %v", models["calls"])
	}
	// The sweep block renders once the janitor has run.
	last, ok := body["last_sweep"].(map[string]any)
	if !ok {
		t.Fatalf("last_sweep missing: %v", body["last_sweep"])
	}
	if last["dropped_suspects"] != float64(2) || last["dropped_signatures"] != float64(3) {
		t.Errorf("last_sweep = %v", last)
	}
	if last["remaining_total"] != float64(42) {
		t.Errorf("remaining_total = %v", last["remaining_total"])
	}
	if nextIn, ok := body["next_sweep_in"].(string); !ok || nextIn == "" {
		t.Errorf("next_sweep_in = %v", body["next_sweep_in"])
	}
}

// A configured-but-never-run janitor must not render the sweep block.
func TestStatuszSweepNotYetRun(t *testing.T) {
	_, ts := newTestServerWithSweep(t, "", neverSweep{})
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if _, ok := body["last_sweep"]; ok {
		t.Error("last_sweep must be absent before the first sweep")
	}
	if _, ok := body["next_sweep_in"]; ok {
		t.Error("next_sweep_in must be absent before the first sweep")
	}
}

// TestStatuszNotifyBlock pins the amend4 /statusz notify block: it is always
// present, carries the counters, and surfaces notify.target_ok once checked.
func TestStatuszNotifyBlock(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	nb, ok := body["notify"].(map[string]any)
	if !ok {
		t.Fatalf("notify block missing: %v", body["notify"])
	}
	if nb["enabled"] != true || nb["target_ok"] != true || nb["target_checked"] != true {
		t.Errorf("notify.target = %v/%v/%v", nb["enabled"], nb["target_ok"], nb["target_checked"])
	}
	if nb["sent"] != float64(7) {
		t.Errorf("notify.sent = %v", nb["sent"])
	}
	if nb["queue_cap"] == nil || nb["queue_len"] == nil {
		t.Errorf("notify queue = %v/%v", nb["queue_len"], nb["queue_cap"])
	}
	if body["notify.target_ok"] != true {
		t.Error("top-level notify.target_ok should be set once checked")
	}
}

// TestStatuszAnnounceBlock pins the amend7 /statusz announce gauges.
func TestStatuszAnnounceBlock(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["announce_enabled"] != true {
		t.Errorf("announce_enabled = %v", body["announce_enabled"])
	}
	if body["announce_sent_total"] != float64(5) {
		t.Errorf("announce_sent_total = %v", body["announce_sent_total"])
	}
	if body["announce_fallback_total"] != float64(2) {
		t.Errorf("announce_fallback_total = %v", body["announce_fallback_total"])
	}
	if body["announce_error_total"] != float64(0) {
		t.Errorf("announce_error_total = %v", body["announce_error_total"])
	}
}

// TestStatuszMsgTTLBlock pins the amend8 /statusz msg_ttl gauges.
func TestStatuszMsgTTLBlock(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["msg_ttl"] != "1m0s" {
		t.Errorf("msg_ttl = %v", body["msg_ttl"])
	}
	if body["msg_ttl_scheduled_total"] != float64(3) {
		t.Errorf("msg_ttl_scheduled_total = %v", body["msg_ttl_scheduled_total"])
	}
	if body["msg_ttl_deleted_total"] != float64(2) {
		t.Errorf("msg_ttl_deleted_total = %v", body["msg_ttl_deleted_total"])
	}
	if body["msg_ttl_error_total"] != float64(1) {
		t.Errorf("msg_ttl_error_total = %v", body["msg_ttl_error_total"])
	}
}

// TestStatuszJoinRuleBlock pins the amend9 /statusz join-rule gauges.
func TestStatuszJoinRuleBlock(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["join_rule_enabled"] != true {
		t.Errorf("join_rule_enabled = %v", body["join_rule_enabled"])
	}
	if body["join_rule_hits_total"] != float64(4) {
		t.Errorf("join_rule_hits_total = %v", body["join_rule_hits_total"])
	}
}

// The admin token must never leak into any status payload.
func TestStatuszNoSecrets(t *testing.T) {
	_, ts := newTestServerWithSweep(t, "s3cret", fakeSweep{})
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Error("statusz leaked the admin token")
	}
}

func TestAdminGatedWhenNoToken(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/admin/signatures")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("admin route should be closed without token: %d", resp.StatusCode)
	}
}

func TestAdminAuth(t *testing.T) {
	_, ts := newTestServer(t, "s3cret")
	// No token → 401.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/admin/signatures", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("unauth = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Wrong token → 401.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/signatures", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Errorf("wrong token = %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAdminCRUD(t *testing.T) {
	_, ts := newTestServer(t, "s3cret")
	client := ts.Client()

	// POST a bio signature.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/signatures",
		strings.NewReader(`{"kind":"bio","text":"办证 加V"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := client.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("POST = %d", resp.StatusCode)
	}
	var posted map[string]string
	json.NewDecoder(resp.Body).Decode(&posted)
	resp.Body.Close()
	sig := posted["sig"]
	if sig == "" {
		t.Fatal("POST did not return sig")
	}

	// GET lists it.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/signatures?kind=bio", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, _ = client.Do(req)
	var got map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if _, ok := got["bio"][sig]; !ok {
		t.Fatalf("GET missing sig: %v", got)
	}

	// DELETE removes it.
	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/admin/signatures?kind=bio&sig="+sig, nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, _ = client.Do(req)
	if resp.StatusCode != 200 {
		t.Fatalf("DELETE = %d", resp.StatusCode)
	}
	resp.Body.Close()

	// GET again: empty.
	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/admin/signatures?kind=bio", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp, _ = client.Do(req)
	var after map[string]map[string]string
	json.NewDecoder(resp.Body).Decode(&after)
	resp.Body.Close()
	if len(after["bio"]) != 0 {
		t.Fatalf("after delete: %v", after)
	}
}

func TestAdminBadInput(t *testing.T) {
	_, ts := newTestServer(t, "s3cret")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/signatures",
		strings.NewReader(`{"kind":"nope"}`))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Content-Type", "application/json")
	resp, _ := ts.Client().Do(req)
	if resp.StatusCode != 400 {
		t.Errorf("bad kind = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// amend10/11: the /statusz join-verification block.
func TestStatuszJoinVerifyBlock(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["join_verify_enabled"] != true {
		t.Errorf("join_verify_enabled = %v", body["join_verify_enabled"])
	}
	if body["join_verify_pending"] != float64(2) {
		t.Errorf("join_verify_pending = %v", body["join_verify_pending"])
	}
	if body["join_verify_passed_total"] != float64(9) {
		t.Errorf("join_verify_passed_total = %v", body["join_verify_passed_total"])
	}
	if body["join_verify_timeout_total"] != float64(3) {
		t.Errorf("join_verify_timeout_total = %v", body["join_verify_timeout_total"])
	}
	if body["join_verify_skipped_total"] != float64(7) {
		t.Errorf("join_verify_skipped_total = %v", body["join_verify_skipped_total"])
	}
}

// TestStatuszAmend13Block pins the amend13 disposal-floor gauges
// (ai_min_punish_score / ai_below_score_releases_total).
func TestStatuszAmend13Block(t *testing.T) {
	_, ts := newTestServer(t, "")
	resp := get(t, ts.URL+"/statusz")
	defer resp.Body.Close()
	var body map[string]any
	json.NewDecoder(resp.Body).Decode(&body)
	if body["ai_min_punish_score"] != float64(85) {
		t.Errorf("ai_min_punish_score = %v", body["ai_min_punish_score"])
	}
	if body["ai_below_score_releases_total"] != float64(6) {
		t.Errorf("ai_below_score_releases_total = %v", body["ai_below_score_releases_total"])
	}
}
