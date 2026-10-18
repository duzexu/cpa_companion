package companion

import (
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

func TestRouteSessionExtraction(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
		body    string
		want    routeSession
	}{
		{"codex", http.Header{"Session-Id": {"root"}, "Thread-Id": {"child"}}, `{}`, routeSession{"codex:child", "codex:root"}},
		{"codex turn subagent", http.Header{"Session-Id": {"root"}, "X-Codex-Turn-Metadata": {`{"agent_name":"/root/research","parent_thread_id":"root"}`}}, `{}`, routeSession{"codex:root:agent:research", "codex:root"}},
		{"claude", http.Header{"X-Claude-Code-Session-Id": {"root"}, "X-Claude-Code-Agent-Id": {"worker"}}, `{}`, routeSession{"claude:root:agent:worker", "claude:root"}},
		{"generic", http.Header{"X-Session-Id": {"child"}, "X-Parent-Session-Id": {"root"}}, `{}`, routeSession{"header:child", "header:root"}},
		{"body", nil, `{"session_id":"child","parent_session_id":"root"}`, routeSession{"session:child", "session:root"}},
		{"none", nil, `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`, routeSession{}},
		{"invalid", http.Header{"X-Session-Id": {"bad\nID"}}, `{}`, routeSession{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := extractRouteSession(tc.headers, []byte(tc.body))
			if got != tc.want {
				t.Fatalf("session = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestRouteAffinityRotationInheritanceAndInvalidation(t *testing.T) {
	cache := newRouteAffinity(time.Hour)
	priority := 5
	accounts := []credential{{ID: "one", Prefix: "one", Priority: &priority}, {ID: "two", Prefix: "two", Priority: &priority}}
	plan := routingPlan{ID: "plan"}
	ns := affinityNamespace("user", plan, accounts, "model")
	first := cache.order(ns, routeSession{ID: "codex:root"}, accounts)[0].ID
	if got := cache.order(ns, routeSession{ID: "codex:root"}, accounts)[0].ID; got != first {
		t.Fatalf("same session moved from %s to %s", first, got)
	}
	if got := cache.order(ns, routeSession{ID: "codex:child", Parent: "codex:root"}, accounts)[0].ID; got != first {
		t.Fatalf("child did not inherit %s: %s", first, got)
	}
	other := cache.order(ns, routeSession{ID: "codex:other"}, accounts)[0].ID
	if other == first {
		t.Fatalf("new session did not rotate from %s", first)
	}
	cache.rebind(ns, "codex:root", first, other)
	if got := cache.order(ns, routeSession{ID: "codex:root"}, accounts)[0].ID; got != other {
		t.Fatalf("quota failover did not rebind: %s", got)
	}
	if got := cache.order(ns, routeSession{ID: "codex:child", Parent: "codex:root"}, accounts)[0].ID; got != first {
		t.Fatalf("existing child unexpectedly changed: %s", got)
	}
	if got := cache.order(ns, routeSession{ID: "codex:new-child", Parent: "codex:root"}, accounts)[0].ID; got != other {
		t.Fatalf("new child did not inherit rebound parent: %s", got)
	}
	changed := []credential{{ID: "one", Prefix: "one", Priority: &priority}}
	if got := cache.order(affinityNamespace("user", plan, changed, "model"), routeSession{ID: "codex:root"}, changed)[0].ID; got != "one" {
		t.Fatalf("changed plan retained stale account: %s", got)
	}
	if got := cache.order(affinityNamespace("another-user", plan, accounts, "model"), routeSession{ID: "codex:root"}, accounts)[0].ID; got != "one" {
		t.Fatalf("binding leaked between users: %s", got)
	}
	if got := cache.order(ns, routeSession{}, accounts)[0].ID; got == cache.order(ns, routeSession{}, accounts)[0].ID {
		t.Fatal("requests without session did not rotate")
	}
}

func TestRouteAffinityExpiryAndConcurrency(t *testing.T) {
	cache := newRouteAffinity(5 * time.Millisecond)
	accounts := []credential{{ID: "one"}, {ID: "two"}}
	first := cache.order("scope", routeSession{ID: "session"}, accounts)[0].ID
	time.Sleep(10 * time.Millisecond)
	if got := cache.order("scope", routeSession{ID: "session"}, accounts)[0].ID; got == first {
		t.Fatalf("expired binding remained %s", got)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cache.order("scope", routeSession{ID: "session", Parent: "parent"}, accounts)
			cache.rebind("scope", "session", "one", "two")
		}()
	}
	wg.Wait()
}

func TestPlanForwardSessionAffinityAndQuotaFailover(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	limitFirst := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{{"id": "first", "name": "first.json", "source": "file", "provider": "codex", "priority": 5}, {"id": "second", "name": "second.json", "source": "file", "provider": "codex", "priority": 5}}})
		case "/v8/management/credentials/models":
			prefix := strings.TrimSuffix(r.URL.Query().Get("name"), ".json")
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"id": prefix + "/test-model"}, {"id": "test-model"}}})
		case "/v8/management/config/api-keys":
			json.NewEncoder(w).Encode(map[string]any{})
		case "/v0/management/oauth-model-alias":
			json.NewEncoder(w).Encode(map[string]any{"oauth-model-alias": map[string]any{}})
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "first/test-model"}, {"id": "second/test-model"}}})
		case "/v1/chat/completions":
			var req struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			calls = append(calls, req.Model)
			limited := limitFirst && req.Model == "first/test-model"
			mu.Unlock()
			if limited {
				http.Error(w, "rate limit", 429)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"model": req.Model})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "plan", AccountIDs: []string{"first", "second"}}); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "user", db.plans()[0].ID, false); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("user", "key", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: &url.URL{Scheme: "http", Host: "localhost:8080"}, CPABaseURL: u, CPAAPIKey: "api", CPAManagementKey: "management"}, db)
	call := func(session, parent string) string {
		t.Helper()
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Session-Id", session)
		if parent != "" {
			r.Header.Set("X-Codex-Parent-Thread-Id", parent)
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("session %q status %d: %s", session, w.Code, w.Body.String())
		}
		var result map[string]string
		json.Unmarshal(w.Body.Bytes(), &result)
		return result["model"]
	}
	if got := call("root", ""); got != "first/test-model" {
		t.Fatalf("first route %s", got)
	}
	if got := call("root", ""); got != "first/test-model" {
		t.Fatalf("sticky route %s", got)
	}
	if got := call("child", "root"); got != "first/test-model" {
		t.Fatalf("subagent route %s", got)
	}
	if got := call("other", ""); got != "second/test-model" {
		t.Fatalf("rotation route %s", got)
	}
	mu.Lock()
	limitFirst = true
	mu.Unlock()
	if got := call("root", ""); got != "second/test-model" {
		t.Fatalf("failover route %s", got)
	}
	mu.Lock()
	before := len(calls)
	mu.Unlock()
	if got := call("root", ""); got != "second/test-model" {
		t.Fatalf("rebound route %s", got)
	}
	mu.Lock()
	after := len(calls)
	mu.Unlock()
	if after != before+1 {
		t.Fatalf("rebound request retried stale account: calls %d -> %d", before, after)
	}
}
