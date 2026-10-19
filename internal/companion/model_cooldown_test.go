package companion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestModelCooldownResetParsing(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		body string
		want time.Duration
		ok   bool
	}{
		{`{"error":{"code":"model_cooldown","reset_seconds":928,"reset_time":"15m27s"}}`, 928 * time.Second, true},
		{`{"error":{"code":"model_cooldown","reset_time":"15m27s"}}`, 15*time.Minute + 27*time.Second, true},
		{`{"error":{"code":"model_cooldown","reset_time":"` + now.Add(2*time.Minute).Format(time.RFC3339) + `"}}`, 2 * time.Minute, true},
		{`{"error":{"code":"rate_limit","reset_seconds":928}}`, 0, false},
		{`{"error":{"code":"model_cooldown"}}`, 30 * time.Second, true},
	} {
		w := &retryResponseWriter{target: httptest.NewRecorder(), header: make(http.Header)}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(tc.body))
		resetAt, ok := modelCooldownReset(w, now)
		if ok != tc.ok || ok && resetAt.Sub(now) != tc.want {
			t.Fatalf("body %s: reset=%v ok=%v", tc.body, resetAt.Sub(now), ok)
		}
	}
}

func TestModelCooldownRefreshesStoredDeadline(t *testing.T) {
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	now := time.Now().UTC().Truncate(time.Second)
	for _, resetAt := range []time.Time{now.Add(time.Minute), now.Add(2 * time.Minute)} {
		if err := logs.recordModelCooldown(context.Background(), "account", "test-model", resetAt); err != nil {
			t.Fatal(err)
		}
	}
	active, err := logs.activeModelCooldowns(context.Background(), "test-model", now)
	if err != nil || !active["account"].Equal(now.Add(2*time.Minute)) {
		t.Fatalf("refreshed deadline: %v, %v", active, err)
	}
}

func TestModelCooldownSkipsAccountsAndSurvivesRestart(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	coolFirst, coolSecond := true, false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			_, _ = w.Write([]byte(`{"files":[{"id":"first-id","name":"first.json","source":"file","provider":"codex","priority":10},{"id":"second-id","name":"second.json","source":"file","provider":"codex","priority":5}]}`))
		case "/v8/management/credentials/models":
			prefix := strings.TrimSuffix(r.URL.Query().Get("name"), ".json")
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"id": "test-model"}, {"id": prefix + "/test-model"}}})
		case "/v8/management/config/api-keys":
			_, _ = w.Write([]byte(`{}`))
		case "/v0/management/oauth-model-alias":
			_, _ = w.Write([]byte(`{"oauth-model-alias":{}}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"first/test-model"},{"id":"second/test-model"}]}`))
		case "/v1/chat/completions":
			var body struct {
				Model string `json:"model"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			mu.Lock()
			calls = append(calls, body.Model)
			cool := body.Model == "first/test-model" && coolFirst || body.Model == "second/test-model" && coolSecond
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if cool {
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"code":"model_cooldown","reset_seconds":600,"reset_time":"10m","message":"all channels cooled"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"model": body.Model})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	dir := t.TempDir()
	db, err := openStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "plan", AccountIDs: []string{"first-id", "second-id"}}); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "alice", db.plans()[0].ID, false); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("alice", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	logs, err := openUsageLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	newTestApp := func(logs *usageLogStore) *app {
		a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAAPIKey: "proxy", CPAManagementKey: "management"}, db)
		a.logs = logs
		return a
	}
	a := newTestApp(logs)
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if got := call(); got.Code != http.StatusOK {
		t.Fatalf("first request: %d %s", got.Code, got.Body.String())
	}
	mu.Lock()
	if len(calls) != 2 || calls[0] != "first/test-model" || calls[1] != "second/test-model" {
		t.Fatalf("failover calls: %v", calls)
	}
	coolSecond = true
	mu.Unlock()
	if got := call(); got.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: %d %s", got.Code, got.Body.String())
	}
	mu.Lock()
	if len(calls) != 3 || calls[2] != "second/test-model" {
		t.Fatalf("cooled first account was retried: %v", calls)
	}
	mu.Unlock()
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	logs, err = openUsageLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a = newTestApp(logs)
	blocked := call()
	if blocked.Code != http.StatusTooManyRequests || blocked.Header().Get("Retry-After") == "" || !strings.Contains(blocked.Body.String(), `"code":"model_cooldown"`) {
		t.Fatalf("local cooldown after restart: %d %s", blocked.Code, blocked.Body.String())
	}
	rows, _, err := logs.list(context.Background(), usageFilter{UserID: "alice", Since: time.Now().Add(-time.Minute), Limit: 10})
	if err != nil || len(rows) != 3 || rows[0].AccountID != "" || rows[1].AccountID != "second-id" {
		t.Fatalf("upstream/local 429 origin not recorded: rows=%+v err=%v", rows, err)
	}
	buckets, _, err := logs.traffic(context.Background(), "alice", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var success, failed int64
	for _, bucket := range buckets {
		success += bucket.Success
		failed += bucket.Failed
	}
	if success != 1 || failed != 1 {
		t.Fatalf("local cooldown affected dashboard success rate: success=%d failed=%d", success, failed)
	}
	mu.Lock()
	if len(calls) != 3 {
		t.Fatalf("local cooldown reached upstream: %v", calls)
	}
	coolFirst, coolSecond = false, false
	mu.Unlock()
	if _, err := logs.db.ExecContext(context.Background(), `UPDATE model_cooldowns SET reset_at_ms=?`, time.Now().Add(-time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if got := call(); got.Code != http.StatusOK {
		t.Fatalf("request after reset time: %d %s", got.Code, got.Body.String())
	}
	mu.Lock()
	if len(calls) != 4 {
		t.Fatalf("expired cooldown did not reach upstream: %v", calls)
	}
	mu.Unlock()
}
