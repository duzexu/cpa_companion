package companion

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetReservationChecksPendingAndSettlesDurably(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "metered", AccountIDs: []string{"account"}, BudgetPicoUSD: 300_000_000}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "alice", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	first, err := db.reserveUsage("alice", plan.ID, 200_000_000, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.reserveUsage("alice", plan.ID, 150_000_000, time.Now()); !errors.Is(err, errPlanBudgetUnavailable) {
		t.Fatalf("pending reservation did not reduce remaining budget: %v", err)
	}
	reopened, err := openStore(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := reopened.getPlan(plan.ID); stored.UsedPicoUSD != 200_000_000 {
		t.Fatalf("reservation was not durable: %d", stored.UsedPicoUSD)
	}
	if err := first.refund(); err != nil {
		t.Fatal(err)
	}
	second, err := db.reserveUsage("alice", plan.ID, 150_000_000, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := second.settle(100_000_000, ""); err != nil {
		t.Fatal(err)
	}
	if err := second.refund(); err != nil {
		t.Fatal(err)
	}
	reopened, err = openStore(db.path)
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := reopened.getPlan(plan.ID); stored.UsedPicoUSD != 100_000_000 {
		t.Fatalf("settlement/refund was not durable or idempotent: %d", stored.UsedPicoUSD)
	}
}

func TestEstimateRequestChargeUsesOutputLimitAndCacheRates(t *testing.T) {
	price := modelPrice{Expression: `tier("standard", p * 1 + cr * 0.1 + cc * 3 + c * 2)`}
	withLimit, err := estimateRequestCharge("/v1/chat/completions", []byte(`{"model":"x","max_tokens":10}`), price)
	if err != nil {
		t.Fatal(err)
	}
	withoutLimit, err := estimateRequestCharge("/v1/chat/completions", []byte(`{"model":"x"}`), price)
	if err != nil || withoutLimit <= withLimit {
		t.Fatalf("missing output limit did not use fallback: limited=%d fallback=%d err=%v", withLimit, withoutLimit, err)
	}
	input := int64((len(`{"model":"x","max_tokens":10}`) + 2) / 3)
	want := input*3_000_000 + 10*2_000_000
	if withLimit != want {
		t.Fatalf("cache-write estimate=%d, want %d", withLimit, want)
	}
}

func TestUSDParsingAndCost(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  int64
	}{
		{"", 0}, {"0.000001", 1}, {"2.50", 2_500_000}, {"100", 100_000_000},
	} {
		got, err := parseUSD(tc.input)
		if err != nil || got != tc.want {
			t.Fatalf("parseUSD(%q) = %d, %v", tc.input, got, err)
		}
	}
	for _, invalid := range []string{"-1", "1.0000001", "NaN", "1e3", "" + strings.Repeat("9", 20)} {
		if _, err := parseUSD(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
	price := modelPrice{Expression: `tier("standard", p * 2 + cr * 0.5 + cc * 2.5 + c * 8)`}
	charge, err := price.charge(tokenUsage{Input: 1000, Output: 250, CacheRead: 400, CacheWrite: 100})
	if err != nil || charge != 3_450_000_000 {
		t.Fatalf("charge = %d, %v", charge, err)
	}
	if got := formatPicoUSD(charge); got != "0.00345" {
		t.Fatalf("format charge = %q", got)
	}
}

func TestBudgetedProxyRequiresPriceAndStopsAfterUsage(t *testing.T) {
	var upstreamCalls atomic.Int32
	var omitUsage atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			_, _ = w.Write([]byte(`{"files":[{"id":"one-id","name":"one.json","source":"file","provider":"codex"}]}`))
		case "/v8/management/credentials/models":
			_, _ = w.Write([]byte(`{"models":[{"id":"test-model"},{"id":"one/test-model"}]}`))
		case "/v8/management/config/api-keys":
			_, _ = w.Write([]byte(`{}`))
		case "/v0/management/oauth-model-alias":
			_, _ = w.Write([]byte(`{"oauth-model-alias":{}}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"test-model"},{"id":"one/test-model"}]}`))
		case "/v1/chat/completions":
			upstreamCalls.Add(1)
			time.Sleep(20 * time.Millisecond)
			var body struct {
				Model string `json:"model"`
			}
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "one/test-model" {
				http.Error(w, "wrong upstream model", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if omitUsage.Load() {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
			} else {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":100}}`))
			}
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
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "metered", AccountIDs: []string{"one-id"}, BudgetPicoUSD: 200_000_000}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "alice", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("alice", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAAPIKey: "proxy", CPAManagementKey: "management"}, db)
	call := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-model","max_tokens":100}`))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if response := call(); response.Code != 422 || upstreamCalls.Load() != 0 {
		t.Fatalf("unpriced model reached upstream: %d, %d", response.Code, upstreamCalls.Load())
	}
	if err := db.savePrice("test-model", modelPrice{Expression: `tier("standard", p * 1 + c * 1)`}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() { <-start; results <- call().Code }()
	}
	close(start)
	first, second := <-results, <-results
	if !((first == 200 && second == 403) || (first == 403 && second == 200)) || upstreamCalls.Load() != 1 {
		t.Fatalf("concurrent calls exceeded budget: %d, %d, upstream=%d", first, second, upstreamCalls.Load())
	}
	plan, _ = db.getPlan(plan.ID)
	if plan.UsedPicoUSD != 200_000_000 {
		t.Fatalf("wrong usage charge: %d", plan.UsedPicoUSD)
	}
	plan.BudgetPicoUSD = 1_000_000_000_000
	if err := db.savePlan(plan); err != nil {
		t.Fatal(err)
	}
	omitUsage.Store(true)
	if response := call(); response.Code != 200 {
		t.Fatalf("first unmetered upstream response: %d", response.Code)
	}
	if response := call(); response.Code != 403 || upstreamCalls.Load() != 2 {
		t.Fatalf("plan did not stop after missing usage: %d, %d", response.Code, upstreamCalls.Load())
	}
}

func TestResponseUsageParsing(t *testing.T) {
	for _, tc := range []struct {
		route, body string
		want        tokenUsage
	}{
		{"/v1/chat/completions", `{"usage":{"prompt_tokens":120,"completion_tokens":30,"prompt_tokens_details":{"cached_tokens":40}}}`, tokenUsage{Input: 120, Output: 30, CacheRead: 40}},
		{"/v1/responses", `{"type":"response.completed","response":{"usage":{"input_tokens":20,"output_tokens":10,"input_tokens_details":{"cached_tokens":5}}}}`, tokenUsage{Input: 20, Output: 10, CacheRead: 5}},
		{"/v1/messages", `{"usage":{"input_tokens":80,"output_tokens":20,"cache_read_input_tokens":10,"cache_creation_input_tokens":5}}`, tokenUsage{Input: 95, Output: 20, CacheRead: 10, CacheWrite: 5}},
	} {
		got, ok := parseResponseUsage(tc.route, []byte(tc.body))
		if !ok || got != tc.want {
			t.Fatalf("usage %s = %+v, %v", tc.route, got, ok)
		}
	}
}

func TestStreamingUsageCaptureAcrossChunks(t *testing.T) {
	output := httptest.NewRecorder()
	meter := &usageCapture{target: output, route: "/v1/responses"}
	meter.Header().Set("Content-Type", "text/event-stream")
	meter.WriteHeader(http.StatusOK)
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"
	for i := 0; i < len(stream); i += 7 {
		end := i + 7
		if end > len(stream) {
			end = len(stream)
		}
		_, _ = meter.Write([]byte(stream[i:end]))
	}
	usage, err := meter.result()
	if err != nil || usage.Input != 10 || usage.Output != 5 || output.Body.String() != stream {
		t.Fatalf("stream usage=%+v err=%v body=%q", usage, err, output.Body.String())
	}
	missing := &usageCapture{target: httptest.NewRecorder(), route: "/v1/responses"}
	missing.Header().Set("Content-Type", "text/event-stream")
	_, _ = missing.Write([]byte("data: [DONE]\n\n"))
	if _, err := missing.result(); err == nil {
		t.Fatal("stream without usage was accepted")
	}
}

func TestPlanBudgetPersistsAcrossEditsAndBlocksAtLimit(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "priced", AccountIDs: []string{"account"}, BudgetPicoUSD: 3_000_000_000}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "alice", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.recordUsage(plan.ID, 2_000_000_000, ""); err != nil {
		t.Fatal(err)
	}
	plan.Name = "edited"
	if err := db.savePlan(plan); err != nil {
		t.Fatal(err)
	}
	stored, _ := db.getPlan(plan.ID)
	if stored.UsedPicoUSD != 2_000_000_000 {
		t.Fatal("editing reset usage")
	}
	if err := db.recordUsage(plan.ID, 1_000_000_000, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.activePlanForUser("alice", time.Now()); ok {
		t.Fatal("exhausted plan allowed traffic")
	}
	reopened, err := openStore(db.path)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ = reopened.getPlan(plan.ID)
	if stored.UsedPicoUSD != stored.BudgetPicoUSD {
		t.Fatalf("usage was not durable: %+v", stored)
	}
}
