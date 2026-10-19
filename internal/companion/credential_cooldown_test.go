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
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialCooldownStatusFromCPAAndLocal429(t *testing.T) {
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	now := time.Now().UTC().Truncate(time.Second)
	if err := logs.recordModelCooldown(context.Background(), "local", "test-model", now.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var accounts []credential
	fixture := `[
      {"id":"model","provider":"codex","prefix":"model","cooldowns":[{"scope":"model","reason":"quota","http_status":429,"retry_at":"` + now.Add(3*time.Minute).Format(time.RFC3339) + `"}]},
      {"id":"credential","provider":"codex","prefix":"credential","cooldowns":[{"scope":"credential","reason":"credential_quota","retry_at":"` + now.Add(2*time.Minute).Format(time.RFC3339) + `"}]},
      {"id":"transient","cooldowns":[{"scope":"model","reason":"transient_error","http_status":503,"retry_at":"` + now.Add(5*time.Minute).Format(time.RFC3339) + `"}]},
      {"id":"expired","cooldowns":[{"scope":"model","reason":"quota","http_status":429,"retry_at":"` + now.Add(-time.Minute).Format(time.RFC3339) + `"}]},
      {"id":"local","provider":"codex","prefix":"local"}
    ]`
	if err := json.Unmarshal([]byte(fixture), &accounts); err != nil {
		t.Fatal(err)
	}
	a := &app{logs: logs}
	a.annotateCredentialCooldowns(context.Background(), accounts)
	if !accounts[0].CoolingAt(now) || accounts[0].BlockedAt(now) || !strings.Contains(accounts[0].CoolingTextAt(now), "部分模型冷却中") {
		t.Fatalf("model-level 429 status: %+v", accounts[0])
	}
	if !accounts[1].BlockedAt(now) || !strings.Contains(accounts[1].CoolingTextAt(now), "凭证冷却中") {
		t.Fatalf("credential-level cooldown status: %+v", accounts[1])
	}
	if accounts[2].CoolingAt(now) || accounts[3].CoolingAt(now) {
		t.Fatal("non-429 or expired CPA cooldown was marked active")
	}
	if !accounts[4].CoolingAt(now) || accounts[4].BlockedAt(now) {
		t.Fatal("locally observed model_cooldown was not shown")
	}
	plan := routingPlan{AccountIDs: []string{"model", "credential"}}
	rows := dashboardPlanAccounts(plan, map[string]credential{"model": accounts[0], "credential": accounts[1]}, true, now)
	if len(rows) != 2 || !rows[0].Cooling || !rows[0].Active || !rows[1].Cooling || rows[1].Active {
		t.Fatalf("dashboard statuses: %+v", rows)
	}
	view := dashboardView{HasPlan: true, Plan: routingPlan{AccountIDs: []string{"model"}}, Accounts: []quotaView{{Account: accounts[0], Active: true}}}
	w := httptest.NewRecorder()
	render(w, dashboardPlanTemplate, view)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "部分模型冷却中") {
		t.Fatalf("key page does not show cooldown: %d", w.Code)
	}
	admin := httptest.NewRecorder()
	render(admin, adminPlanTemplate, struct {
		User       user
		Users      []user
		Accounts   []credential
		Plans      []routingPlan
		CSRF       string
		CanRefresh bool
		Now        time.Time
	}{Accounts: accounts, Now: now})
	if admin.Code != 200 || !strings.Contains(admin.Body.String(), "凭证冷却中") {
		t.Fatalf("admin account list does not show cooldown: %d", admin.Code)
	}
}

func TestUpstream429ImmediatelyMarksCredentialWithoutBlockingRouting(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" {
			calls.Add(1)
		}
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit"}}`))
	}))
	defer upstream.Close()
	base, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAAPIKey: "proxy", AccountCacheTTL: time.Hour}, db)
	account := credential{ID: "account", Prefix: "account", Models: map[string]bool{"account/test-model": true}}
	a.accountCache.snapshot = accountSnapshot{accounts: []credential{account}}
	a.accountCache.fetched = time.Now()
	plan := routingPlan{AccountIDs: []string{account.ID}}
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
		w := httptest.NewRecorder()
		a.forwardPlanRequest(w, r, "user", plan, &usageLog{})
		return w
	}
	if got := call(); got.Code != http.StatusTooManyRequests || !strings.Contains(got.Body.String(), `"rate_limit"`) {
		t.Fatalf("first upstream response: %d %s", got.Code, got.Body.String())
	}
	accounts := []credential{account}
	a.annotateCredentialCooldowns(context.Background(), accounts)
	if !accounts[0].Cooling() || accounts[0].BlockedAt(time.Now()) {
		t.Fatalf("generic 429 should be a display-only model cooldown: %+v", accounts[0])
	}
	remaining := time.Until(accounts[0].CoolingUntil)
	if remaining < 110*time.Second || remaining > 121*time.Second {
		t.Fatalf("Retry-After was not used: %v", remaining)
	}
	if got := call(); got.Code != http.StatusTooManyRequests || calls.Load() != 2 {
		t.Fatalf("generic 429 incorrectly blocked a subsequent upstream attempt: status=%d calls=%d", got.Code, calls.Load())
	}
}

func TestUpstream429ResetParsesRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, tc := range []struct {
		value string
		want  time.Duration
	}{
		{"75", 75 * time.Second},
		{now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute},
		{"bad", 30 * time.Second},
	} {
		header := make(http.Header)
		header.Set("Retry-After", tc.value)
		if got := upstream429Reset(header, now).Sub(now); got != tc.want {
			t.Fatalf("Retry-After %q: got %v, want %v", tc.value, got, tc.want)
		}
	}
}

func TestModelCooldownQueriesCredentialScope(t *testing.T) {
	for _, scope := range []string{"credential", "model"} {
		t.Run(scope, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			resetAt := time.Now().Add(10 * time.Minute).UTC()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v8/management/credentials":
					_ = json.NewEncoder(w).Encode(map[string]any{"files": []any{
						map[string]any{"id": "first", "cooldowns": []any{map[string]any{
							"scope": scope, "reason": "quota", "http_status": 429, "retry_at": resetAt,
						}}},
						map[string]any{"id": "second", "cooldowns": []any{}},
					}})
				case "/v1/chat/completions":
					var body struct {
						Model string `json:"model"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						http.Error(w, "bad JSON", 400)
						return
					}
					mu.Lock()
					calls = append(calls, body.Model)
					mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					if body.Model == "first/model-a" {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = w.Write([]byte(`{"error":{"code":"model_cooldown","reset_seconds":600}}`))
						return
					}
					_, _ = w.Write([]byte(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			logs, err := openUsageLogs(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer logs.Close()
			a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAManagementKey: "management", CPAAPIKey: "proxy", AccountCacheTTL: time.Hour}, db)
			a.logs = logs
			priorityFirst, prioritySecond := 10, 5
			accounts := []credential{
				{ID: "first", Prefix: "first", Priority: &priorityFirst, Models: map[string]bool{"first/model-a": true, "first/model-b": true}},
				{ID: "second", Prefix: "second", Priority: &prioritySecond, Models: map[string]bool{"second/model-a": true, "second/model-b": true}},
			}
			a.accountCache.snapshot = accountSnapshot{accounts: accounts}
			a.accountCache.fetched = time.Now()
			plan := routingPlan{AccountIDs: []string{"first", "second"}}
			call := func(model string) {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`))
				w := httptest.NewRecorder()
				a.forwardPlanRequest(w, r, "user", plan, &usageLog{})
				if w.Code != http.StatusOK {
					t.Fatalf("%s request: %d %s", model, w.Code, w.Body.String())
				}
			}
			call("model-a")
			view := []credential{accounts[0]}
			a.annotateCredentialCooldowns(context.Background(), view)
			if got := view[0].BlockedAt(time.Now()); got != (scope == "credential") {
				t.Fatalf("scope %s shown as credential blocked=%t", scope, got)
			}
			call("model-b")
			mu.Lock()
			gotCalls := append([]string(nil), calls...)
			mu.Unlock()
			wantLast := "first/model-b"
			if scope == "credential" {
				wantLast = "second/model-b"
			}
			if len(gotCalls) != 3 || gotCalls[0] != "first/model-a" || gotCalls[1] != "second/model-a" || gotCalls[2] != wantLast {
				t.Fatalf("scope %s routed calls=%v, want last %s", scope, gotCalls, wantLast)
			}
			if scope == "credential" {
				r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"model-b"}`))
				w := httptest.NewRecorder()
				a.forwardPlanRequest(w, r, "user", routingPlan{AccountIDs: []string{"first"}}, &usageLog{})
				if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
					t.Fatalf("sole cooled credential should be limited locally: %d %s", w.Code, w.Body.String())
				}
				mu.Lock()
				count := len(calls)
				mu.Unlock()
				if count != 3 {
					t.Fatalf("credential-wide cooldown reached CPA: %d calls", count)
				}
			}
		})
	}
}
