package companion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCredentialConcurrencyFailsOverAndReleases(t *testing.T) {
	entered := make(chan string, 3)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", 400)
			return
		}
		entered <- body.Model
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	plan := routingPlan{ID: "plan", AccountIDs: []string{"first", "second"}, Concurrency: map[string]int{"first": 1, "second": 1}}
	a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAAPIKey: "proxy", AccountCacheTTL: time.Hour}, db)
	a.accountCache.snapshot = accountSnapshot{accounts: []credential{
		{ID: "first", Prefix: "first", Models: map[string]bool{"first/test-model": true}},
		{ID: "second", Prefix: "second", Models: map[string]bool{"second/test-model": true}},
	}}
	a.accountCache.fetched = time.Now()
	call := func() int {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model"}`))
		w := httptest.NewRecorder()
		a.forwardPlanRequest(w, r, "user", plan, &usageLog{})
		return w.Code
	}
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- call() }()
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case model := <-entered:
			seen[model] = true
		case <-time.After(5 * time.Second):
			close(release)
			t.Fatal("two credentials did not receive concurrent requests")
		}
	}
	if !seen["first/test-model"] || !seen["second/test-model"] {
		close(release)
		t.Fatalf("requests were not spread across credentials: %v", seen)
	}
	if status := call(); status != http.StatusTooManyRequests {
		close(release)
		t.Fatalf("all credentials busy: got HTTP %d, want 429", status)
	}
	close(release)
	for i := 0; i < 2; i++ {
		if status := <-results; status != http.StatusOK {
			t.Fatalf("upstream request returned HTTP %d", status)
		}
	}
	if status := call(); status != http.StatusOK {
		t.Fatalf("released credential returned HTTP %d", status)
	}
}

func TestCredentialConcurrencyLimitPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "limited", AccountIDs: []string{"one"}, Concurrency: map[string]int{"one": 2}}); err != nil {
		t.Fatal(err)
	}
	db, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := db.plans()[0].ConcurrencyLimit("one"); got != 2 {
		t.Fatalf("persisted concurrency limit = %d, want 2", got)
	}
	if err := db.savePlan(routingPlan{Name: "invalid", AccountIDs: []string{"one"}, Concurrency: map[string]int{"one": -1}}); err == nil {
		t.Fatal("negative concurrency limit was accepted")
	}
}

func TestCredentialConcurrencyTracksUnlimitedRequestsAcrossPlanEdit(t *testing.T) {
	var limiter planConcurrencyLimiter
	if !limiter.tryAcquire("credential", 0) {
		t.Fatal("unlimited credential was rejected")
	}
	if limiter.tryAcquire("credential", 1) {
		t.Fatal("new limit ignored an existing unlimited request")
	}
	limiter.release("credential")
	if !limiter.tryAcquire("credential", 1) {
		t.Fatal("released credential remained blocked")
	}
	limiter.release("credential")
}
