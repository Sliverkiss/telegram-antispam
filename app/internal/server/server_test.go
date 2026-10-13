package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"adbot/internal/logx"
	"adbot/internal/store"
)

type stubBot struct{ poll, punished, aiCalls, aiFails int64 }

func (s *stubBot) Stats() (int64, int64, int64, int64) {
	return s.poll, s.punished, s.aiCalls, s.aiFails
}

func newTestServer(t *testing.T, adminToken string) (*Server, *httptest.Server) {
	t.Helper()
	st := store.NewMemory()
	for i := 0; i < 7; i++ {
		st.RecordPunishment(context.Background(), store.Punishment{UID: int64(i), Action: "ban"})
	}
	s := New(st, &stubBot{punished: 7, aiCalls: 3}, adminToken, "test", logx.New(nil, logx.Error, "text", "http"))
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
	if _, ok := body["redis_ok"]; !ok {
		t.Error("statusz missing redis_ok")
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
