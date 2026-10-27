package companion

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialRefreshClearsOnlyItsLocalCooldownsAfterSuccess(t *testing.T) {
	var refreshStatus atomic.Int32
	var cpaCooling atomic.Bool
	refreshStatus.Store(http.StatusBadGateway)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			cooldowns := "[]"
			if cpaCooling.Load() {
				cooldowns = fmt.Sprintf(`[{"scope":"model","reason":"quota","http_status":429,"retry_at":%q}]`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
			}
			_, _ = w.Write([]byte(`{"files":[{"id":"one-id","name":"one.json","auth_index":"one-auth","source":"file","provider":"codex","status":"active","cooldowns":` + cooldowns + `}]}`))
		case "/v8/management/credentials/models":
			_, _ = w.Write([]byte(`{"models":[{"id":"test-model"},{"id":"one/test-model"}]}`))
		case "/v8/management/config/api-keys":
			_, _ = w.Write([]byte(`{}`))
		case "/v0/management/oauth-model-alias":
			_, _ = w.Write([]byte(`{"oauth-model-alias":{}}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"one/test-model"}]}`))
		case "/v8/management/credentials/refresh":
			w.WriteHeader(int(refreshStatus.Load()))
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
	if err := db.upsertUser(user{ID: "admin"}); err != nil {
		t.Fatal(err)
	}
	logs, err := openUsageLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAManagementKey: "management", CookieKey: strings.Repeat("k", 64), AdminSubjects: map[string]bool{"admin": true}}, db)
	a.logs = logs
	resetAt := time.Now().Add(time.Hour)
	for _, item := range []struct{ account, model string }{
		{"one-id", "test-model"}, {"one-id", "other-model"}, {"other-id", "test-model"},
	} {
		if err := logs.recordModelCooldown(context.Background(), item.account, item.model, resetAt); err != nil {
			t.Fatal(err)
		}
		a.observedCooldowns.record(item.account, resetAt, time.Now())
	}
	a.observedCooldowns.recordBlocked("one-id", resetAt, time.Now())
	refresh := func() *httptest.ResponseRecorder {
		body := url.Values{"auth_id": {"one-id"}, "csrf": {"token"}}.Encode()
		r := httptest.NewRequest(http.MethodPost, "/admin/refresh", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: "admin", CSRF: "token", Expires: time.Now().Add(time.Hour).Unix()})})
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if got := refresh(); got.Code != http.StatusBadGateway {
		t.Fatalf("failed upstream refresh returned %d", got.Code)
	}
	if active, err := logs.activeModelCooldowns(context.Background(), "test-model", time.Now()); err != nil || len(active) != 2 {
		t.Fatalf("failed refresh changed cooldowns: %v, %v", active, err)
	}
	refreshStatus.Store(http.StatusOK)
	if got := refresh(); got.Code != http.StatusSeeOther {
		t.Fatalf("successful refresh returned %d: %s", got.Code, got.Body.String())
	}
	for _, model := range []string{"test-model", "other-model"} {
		active, err := logs.activeModelCooldowns(context.Background(), model, time.Now())
		if err != nil || !active["one-id"].IsZero() {
			t.Fatalf("refreshed account still cooled for %s: %v, %v", model, active, err)
		}
		if model == "test-model" && !active["other-id"].After(time.Now()) {
			t.Fatalf("another account was cleared: %v", active)
		}
	}
	if a.observedCooldowns.active(time.Now())["one-id"].After(time.Now()) || a.observedCooldowns.blockedUntil("one-id", time.Now()).After(time.Now()) {
		t.Fatal("refreshed account retained in-memory cooldown")
	}
	if !a.observedCooldowns.active(time.Now())["other-id"].After(time.Now()) {
		t.Fatal("another account lost in-memory cooldown")
	}
	accounts, err := a.credentials(context.Background())
	if err != nil || len(accounts) != 1 || accounts[0].Cooling() {
		t.Fatalf("credential view still shows local cooldown: %+v, %v", accounts, err)
	}
	if err := logs.recordModelCooldown(context.Background(), "one-id", "test-model", resetAt); err != nil {
		t.Fatal(err)
	}
	cpaCooling.Store(true)
	if got := refresh(); got.Code != http.StatusSeeOther {
		t.Fatalf("refresh with CPA cooldown returned %d: %s", got.Code, got.Body.String())
	}
	active, err := logs.activeModelCooldowns(context.Background(), "test-model", time.Now())
	if err != nil || !active["one-id"].After(time.Now()) {
		t.Fatalf("CPA cooldown was cleared locally: %v, %v", active, err)
	}
	accounts, err = a.credentials(context.Background())
	if err != nil || len(accounts) != 1 || !accounts[0].Cooling() {
		t.Fatalf("CPA cooldown disappeared from credential view: %+v, %v", accounts, err)
	}
}
