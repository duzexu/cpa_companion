package companion

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPlanRPMRollingWindow(t *testing.T) {
	var limiter planRateLimiter
	now := time.Unix(1_000_000, 0)
	for i := 0; i < 2; i++ {
		if ok, _ := limiter.allow("a", 2, now.Add(time.Duration(i)*time.Second)); !ok {
			t.Fatal("request within RPM limit rejected")
		}
	}
	if ok, retry := limiter.allow("a", 2, now.Add(2*time.Second)); ok || retry != 58 {
		t.Fatalf("expected 429 with 58-second retry, got allowed=%t retry=%d", ok, retry)
	}
	if ok, _ := limiter.allow("b", 2, now.Add(2*time.Second)); !ok {
		t.Fatal("another plan shared the RPM limit")
	}
	if ok, _ := limiter.allow("a", 2, now.Add(time.Minute)); !ok {
		t.Fatal("oldest request did not expire after 60 seconds")
	}
	if ok, _ := limiter.allow("a", 0, now.Add(time.Minute)); !ok {
		t.Fatal("zero RPM should be unlimited")
	}
}

func TestPlanRPMConcurrentRequests(t *testing.T) {
	var limiter planRateLimiter
	var admitted atomic.Int32
	var wg sync.WaitGroup
	now := time.Now()
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, _ := limiter.allow("plan", 5, now); ok {
				admitted.Add(1)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 5 {
		t.Fatalf("admitted %d requests with RPM 5", admitted.Load())
	}
}

func TestPlanRPMPersistedAndEnforced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "limited", AccountIDs: []string{"account"}, RPM: 1}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "user", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("user", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	db, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stored, ok := db.getPlan(plan.ID)
	if !ok || stored.RPM != 1 {
		t.Fatalf("RPM did not survive reload: %+v", stored)
	}
	a := &app{db: db}
	a.planRPM.allow(plan.ID, 1, time.Now())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	a.proxyRequest(w, req)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("expected 429 and Retry-After, got %d, %q", w.Code, w.Header().Get("Retry-After"))
	}
}
