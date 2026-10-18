package companion

import (
	"bufio"
	"context"
	"io"
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

// meteredCPA serves one account "one-id" with prefix "one" and two models.
// The chat handler is supplied by each test.
func meteredCPA(t *testing.T, managementCalls *atomic.Int32, chat http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v8/") || strings.HasPrefix(r.URL.Path, "/v0/") || r.URL.Path == "/v1/models" {
			managementCalls.Add(1)
		}
		switch r.URL.Path {
		case "/v8/management/credentials":
			_, _ = w.Write([]byte(`{"files":[{"id":"one-id","name":"one.json","source":"file","provider":"codex"}]}`))
		case "/v8/management/credentials/models":
			_, _ = w.Write([]byte(`{"models":[{"id":"cheap"},{"id":"one/cheap"},{"id":"pricey"},{"id":"one/pricey"}]}`))
		case "/v8/management/config/api-keys":
			_, _ = w.Write([]byte(`{}`))
		case "/v0/management/oauth-model-alias":
			_, _ = w.Write([]byte(`{"oauth-model-alias":{}}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"one/cheap"},{"id":"one/pricey"}]}`))
		case "/v1/chat/completions", "/v1/responses", "/v1/images/generations", "/v1/alpha/search":
			chat(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func meteredApp(t *testing.T, upstream *httptest.Server, budget int64) (*app, string, string) {
	t.Helper()
	base, _ := url.Parse(upstream.URL)
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "metered", AccountIDs: []string{"one-id"}, BudgetPicoUSD: budget}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "alice", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	for model, rate := range map[string]string{"cheap": "0", "pricey": "1000"} {
		if err := db.savePrice(model, modelPrice{Expression: `tier("standard", p * ` + rate + ` + c * ` + rate + `)`}); err != nil {
			t.Fatal(err)
		}
	}
	key, err := db.createKeySecured("alice", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAAPIKey: "proxy", CPAManagementKey: "management", AccountCacheTTL: time.Minute, CookieKey: strings.Repeat("k", 64)}, db)
	return a, key, plan.ID
}

func TestModelCaseVariantCannotSplitPricingFromRouting(t *testing.T) {
	var forwarded atomic.Int32
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		forwarded.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1000,"completion_tokens":1000}}`))
	})
	a, key, _ := meteredApp(t, upstream, 1_000*picoUSD)
	for _, body := range []string{
		`{"model":"pricey","MODEL":"cheap"}`,
		`{"model":"pricey","Model":"cheap"}`,
		`{"MODEL":"cheap"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", body, w.Code)
		}
	}
	if forwarded.Load() != 0 {
		t.Fatalf("ambiguous model bodies reached CPA %d times", forwarded.Load())
	}
}

func TestAccountCacheCoalescesConcurrentLookups(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	a, _, _ := meteredApp(t, upstream, 0)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.forwardAccounts(t.Context()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// One snapshot: credentials, models, aliases, config, one per-file catalog.
	if got := management.Load(); got != 5 {
		t.Fatalf("20 concurrent lookups issued %d management calls, want 5", got)
	}
	if _, err := a.freshAccounts(t.Context()); err != nil || management.Load() != 10 {
		t.Fatalf("fresh lookup did not bypass the cache: calls=%d err=%v", management.Load(), err)
	}
	accounts, _ := a.forwardAccounts(t.Context())
	accounts[0].ResetAvailable = 99
	again, _ := a.forwardAccounts(t.Context())
	if again[0].ResetAvailable == 99 || management.Load() != 10 {
		t.Fatalf("cached snapshot was mutated or refetched: calls=%d", management.Load())
	}
}

func TestMissingUsageIsChargedAndSuspendsPlan(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"` + strings.Repeat("x", 4000) + `"}}]}`))
	})
	a, key, planID := meteredApp(t, upstream, 1_000*picoUSD)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"pricey"}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	plan, _ := a.db.getPlan(planID)
	if plan.UsedPicoUSD <= 0 || plan.UsageError == "" {
		t.Fatalf("unmetered response was free or did not suspend: used=%d error=%q", plan.UsedPicoUSD, plan.UsageError)
	}
}

func TestBudgetedStreamDoesNotBlockAnotherRequest(t *testing.T) {
	var management atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"request_id":"slow"`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"start\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-release
			_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":10}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	})
	a, key, _ := meteredApp(t, upstream, 1000*picoUSD)
	companion := httptest.NewServer(a.routes())
	defer companion.Close()
	request := func(ctx context.Context, body string) *http.Request {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, companion.URL+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+key)
		return r
	}
	first, err := http.DefaultClient.Do(request(context.Background(), `{"model":"pricey","stream":true,"max_tokens":100,"request_id":"slow"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Body.Close()
	if _, err := bufio.NewReader(first.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	second, err := http.DefaultClient.Do(request(ctx, `{"model":"pricey","max_tokens":100,"request_id":"fast"}`))
	if err != nil {
		t.Fatalf("second request waited for the open stream: %v", err)
	}
	io.Copy(io.Discard, second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("second request returned %d", second.StatusCode)
	}
	unblock()
	if _, err := io.Copy(io.Discard, first.Body); err != nil {
		t.Fatal(err)
	}
}

func TestFailedUpstreamRequestRefundsBudgetReservation(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	})
	a, key, planID := meteredApp(t, upstream, 1000*picoUSD)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"pricey","max_tokens":100}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("upstream status was changed: %d", w.Code)
	}
	plan, _ := a.db.getPlan(planID)
	if plan.UsedPicoUSD != 0 {
		t.Fatalf("failed request kept %d of reserved usage", plan.UsedPicoUSD)
	}
	reopened, err := openStore(a.db.path)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = reopened.getPlan(planID)
	if plan.UsedPicoUSD != 0 {
		t.Fatalf("refund was not durable: %d", plan.UsedPicoUSD)
	}
}

func TestAbortedStreamIsCharged(t *testing.T) {
	var management atomic.Int32
	release := make(chan struct{})
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 50; i++ {
			_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+strings.Repeat("y", 200)+`"}}]}`+"\n\n")
		}
		w.(http.Flusher).Flush()
		<-release // never sends the final usage frame
	})
	defer close(release)
	a, key, planID := meteredApp(t, upstream, 1_000*picoUSD)
	companion := httptest.NewServer(a.routes())
	defer companion.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, companion.URL+"/v1/chat/completions", strings.NewReader(`{"model":"pricey","stream":true}`))
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		plan, _ := a.db.getPlan(planID)
		if plan.UsedPicoUSD > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("aborted stream was not charged")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAbortedUnpricedResponsesStreamRecordsAccountAndUsage(t *testing.T) {
	var management atomic.Int32
	release := make(chan struct{})
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+strings.Repeat("x", 400)+`"}`+"\n\n")
		w.(http.Flusher).Flush()
		<-release // The client disconnects before a final usage frame.
	})
	defer close(release)
	a, key, _ := meteredApp(t, upstream, 0)
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a.logs = logs
	companion := httptest.NewServer(a.routes())
	defer companion.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, companion.URL+"/v1/responses", strings.NewReader(`{"model":"cheap","stream":true,"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _, err := logs.list(context.Background(), usageFilter{UserID: "alice", Since: time.Now().Add(-time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			entry := rows[0]
			if entry.Status != http.StatusOK || entry.AccountID != "one-id" || entry.AccountPrefix != "one" || entry.InputTokens == 0 || entry.OutputTokens == 0 || !entry.Estimated {
				t.Fatalf("aborted stream log lost routing or usage: %+v", entry)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("aborted stream was not logged")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAbortedResponsesAfterCompletedUsageKeepsExactCounts(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {})
	a, _, planID := meteredApp(t, upstream, 1000*picoUSD)
	stream := []byte("event: response.completed\n" +
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}` + "\n\n")
	for _, priced := range []bool{false, true} {
		meter := &usageCapture{target: httptest.NewRecorder(), route: "/v1/responses"}
		meter.Header().Set("Content-Type", "text/event-stream")
		_, _ = meter.Write(stream)
		logged := &usageLog{}
		if priced {
			price, _ := a.db.priceFor("pricey")
			reservation, err := a.db.reserveUsage("alice", planID, 100*picoUSD, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			usage, charge, estimated := a.settleUsage(reservation, price, meter, 5000, true)
			setAuditUsage(logged, usage, charge, estimated)
		} else {
			a.recordUnpricedUsage(logged, meter, "cheap", 5000, true)
		}
		if logged.InputTokens != 10 || logged.OutputTokens != 5 || logged.Estimated {
			t.Fatalf("priced=%v: completed usage was replaced by estimate: %+v", priced, logged)
		}
	}
}

func TestResetFormUsesOpaqueAccountReference(t *testing.T) {
	a := newApp(settings{PublicURL: &url.URL{Scheme: "http", Host: "localhost"}, CookieKey: strings.Repeat("k", 64)}, nil)
	plan := routingPlan{ID: "plan", AccountIDs: []string{"codex-person@example.com.json", "other.json"}}
	ref := a.accountRef("alice", plan.ID, plan.AccountIDs[0])
	if strings.Contains(ref, "person") || len(ref) != 24 {
		t.Fatalf("reference leaks the account ID: %q", ref)
	}
	if got := a.accountForRef("alice", plan, ref); got != plan.AccountIDs[0] {
		t.Fatalf("reference resolved to %q", got)
	}
	for _, bad := range []string{"", plan.AccountIDs[0], a.accountRef("bob", plan.ID, plan.AccountIDs[0])} {
		if got := a.accountForRef("alice", plan, bad); got != "" {
			t.Fatalf("reference %q resolved to %q", bad, got)
		}
	}
	view := dashboardView{User: user{ID: "alice"}, Plan: plan, HasPlan: true, PlanActive: true, CSRF: "csrf",
		Accounts: []quotaView{{Account: credential{ID: plan.AccountIDs[0], Prefix: "p", Provider: "codex"}, Ref: ref, Active: true, ResetLimit: 1, ResetAvailable: 1}}}
	w := httptest.NewRecorder()
	render(w, dashboardPlanTemplate, view)
	if strings.Contains(w.Body.String(), "person@example.com") || !strings.Contains(w.Body.String(), ref) {
		t.Fatal("dashboard exposes the upstream account ID or omits the reference")
	}
}

func TestLogoutRevokesCopiedSessionCookies(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: &url.URL{Scheme: "http", Host: "localhost"}, CookieKey: strings.Repeat("k", 64)}, db)
	cookie := a.sign(signedPayload{UserID: "alice", CSRF: "csrf", Expires: time.Now().Add(time.Hour).Unix()})
	dashboard := func() int {
		r := httptest.NewRequest(http.MethodGet, "/login", nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: cookie})
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w.Code
	}
	if dashboard() != http.StatusSeeOther {
		t.Fatal("valid session was not recognised")
	}
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", strings.NewReader("csrf=csrf"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: cookie})
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout status %d", w.Code)
	}
	if dashboard() != http.StatusOK {
		t.Fatal("a copy of the session cookie still works after logout")
	}
	reopened, _ := openStore(db.path)
	if u, _ := reopened.getUser("alice"); u.SessionVersion != 1 {
		t.Fatal("session revocation was not persisted")
	}
	if err := db.upsertUser(user{ID: "alice", Username: "renamed"}); err != nil {
		t.Fatal(err)
	}
	if u, _ := db.getUser("alice"); u.SessionVersion != 1 {
		t.Fatal("login overwrote the session version")
	}
}

func TestAccountCacheRefreshesAheadOfExpiry(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {})
	a, _, _ := meteredApp(t, upstream, 0)
	a.accountCache.ttl = 200 * time.Millisecond
	if _, err := a.forwardAccounts(t.Context()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(170 * time.Millisecond)
	start := time.Now()
	if _, err := a.forwardAccounts(t.Context()); err != nil || time.Since(start) > 50*time.Millisecond {
		t.Fatalf("near-expiry lookup waited on CPA: %v %v", time.Since(start), err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for management.Load() != 10 {
		if time.Now().After(deadline) {
			t.Fatalf("background refresh did not run: calls=%d", management.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // past the original expiry
	if _, err := a.forwardAccounts(t.Context()); err != nil || management.Load() != 10 {
		t.Fatalf("refreshed snapshot was not reused: calls=%d err=%v", management.Load(), err)
	}
}

func TestStaleRouteRefreshesSnapshotWithCooldown(t *testing.T) {
	var management atomic.Int32
	var chats atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		chats.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if strings.Contains(r.Header.Get("X-Test"), "other") {
			_, _ = w.Write([]byte(`{"error":{"message":"max_tokens too large","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"message":"unknown provider for model one/cheap","code":"model_not_found"}}`))
	})
	a, key, _ := meteredApp(t, upstream, 0)
	send := func(header string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cheap"}`))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("X-Test", header)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	waitCalls := func(want int32) {
		deadline := time.Now().Add(2 * time.Second)
		for management.Load() != want {
			if time.Now().After(deadline) {
				t.Fatalf("management calls=%d, want %d", management.Load(), want)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if w := send("other"); w.Code != 400 || !strings.Contains(w.Body.String(), "max_tokens") {
		t.Fatalf("ordinary 400 was altered: %d %s", w.Code, w.Body.String())
	}
	waitCalls(5)
	for i := 0; i < 3; i++ {
		if w := send("stale"); w.Code != 400 || !strings.Contains(w.Body.String(), "model_not_found") {
			t.Fatalf("stale route response was altered: %d %s", w.Code, w.Body.String())
		}
	}
	waitCalls(10) // one refresh despite three stale responses
	time.Sleep(50 * time.Millisecond)
	if management.Load() != 10 || chats.Load() != 4 {
		t.Fatalf("cooldown not applied: management=%d chats=%d", management.Load(), chats.Load())
	}
}

func TestOlderFetchCannotReplaceNewerSnapshot(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			if calls.Add(1) == 1 {
				<-release // the first, older fetch finishes last
				_, _ = w.Write([]byte(`{"files":[{"id":"old-id","name":"one.json","source":"file"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"files":[{"id":"new-id","name":"one.json","source":"file"}]}`))
		case "/v8/management/credentials/models":
			_, _ = w.Write([]byte(`{"models":[{"id":"m"},{"id":"one/m"}]}`))
		case "/v8/management/config/api-keys":
			_, _ = w.Write([]byte(`{}`))
		case "/v0/management/oauth-model-alias":
			_, _ = w.Write([]byte(`{"oauth-model-alias":{}}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"one/m"}]}`))
		}
	}))
	defer upstream.Close()
	a, _, _ := meteredApp(t, upstream, 0)
	older := make(chan error, 1)
	go func() { _, err := a.forwardAccounts(context.Background()); older <- err }()
	for calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if accounts, err := a.freshAccounts(t.Context()); err != nil || accounts[0].ID != "new-id" {
		t.Fatalf("fresh fetch: %v %v", accounts, err)
	}
	close(release)
	if err := <-older; err != nil {
		t.Fatal(err)
	}
	if accounts, _ := a.forwardAccounts(t.Context()); accounts[0].ID != "new-id" {
		t.Fatalf("older fetch replaced the newer snapshot with %q", accounts[0].ID)
	}
}

func TestWebPagesForbidFramingAndInlineHandlers(t *testing.T) {
	a := newApp(settings{PublicURL: &url.URL{Scheme: "http", Host: "localhost"}, CookieKey: strings.Repeat("k", 64)}, nil)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/login", nil))
	csp := w.Header().Get("Content-Security-Policy")
	if w.Header().Get("X-Frame-Options") != "DENY" || !strings.Contains(csp, "frame-ancestors 'none'") || !strings.Contains(csp, "script-src 'nonce-") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("missing framing protection: %v", w.Header())
	}
	for _, page := range []string{loginTemplate, dashboardPlanTemplate, adminPlanTemplate, priceTemplate} {
		if strings.Contains(page, "onsubmit=") || strings.Contains(page, "onclick=") || strings.Contains(page, "<script ") {
			t.Fatal("template relies on markup the CSP blocks")
		}
	}
	view := dashboardView{User: user{ID: "alice"}, CSRF: "c"}
	w = httptest.NewRecorder()
	render(w, dashboardPlanTemplate, view)
	csp = w.Header().Get("Content-Security-Policy")
	nonce := strings.TrimSuffix(strings.SplitN(strings.SplitN(csp, "'nonce-", 2)[1], "'", 2)[0], "'")
	if strings.Count(w.Body.String(), `<script nonce="`+nonce+`">`) != strings.Count(w.Body.String(), "<script") || nonce == "" {
		t.Fatal("rendered scripts do not carry the response nonce")
	}
	api := httptest.NewRecorder()
	a.routes().ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if api.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("API responses should keep upstream headers")
	}
}

func TestQueryStringAPIKeyIsRejected(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {})
	a, key, _ := meteredApp(t, upstream, 0)
	r := httptest.NewRequest(http.MethodGet, "/v1/models?key="+url.QueryEscape(key), nil)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("query-string API key accepted: %d", w.Code)
	}
}

func TestSlowRequestBodyIsCutOffButStreamsAreNot(t *testing.T) {
	previous := requestBodyTimeout
	requestBodyTimeout = 200 * time.Millisecond
	defer func() { requestBodyTimeout = previous }()
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 4; i++ {
			_, _ = io.WriteString(w, "data: {}\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond) // total longer than the body deadline
		}
	})
	a, key, _ := meteredApp(t, upstream, 0)
	companion := httptest.NewServer(a.routes())
	defer companion.Close()

	slow, writer := io.Pipe()
	go func() {
		_, _ = io.WriteString(writer, `{"model":`)
		time.Sleep(time.Second)
		_ = writer.Close()
	}()
	request, _ := http.NewRequest(http.MethodPost, companion.URL+"/v1/chat/completions", slow)
	request.Header.Set("Authorization", "Bearer "+key)
	start := time.Now()
	if response, err := http.DefaultClient.Do(request); err == nil {
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("slow body status %d", response.StatusCode)
		}
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("slow body held the handler for %v", elapsed)
	}

	request, _ = http.NewRequest(http.MethodPost, companion.URL+"/v1/chat/completions", strings.NewReader(`{"model":"cheap","stream":true}`))
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || strings.Count(string(body), "data: {}") != 4 {
		t.Fatalf("stream was cut off after the body deadline: %q %v", body, err)
	}
}
