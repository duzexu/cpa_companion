package companion

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/tokenkit"
)

func TestInterruptedStreamEstimatesGeneratedContent(t *testing.T) {
	for _, tc := range []struct {
		name, route, frame string
	}{
		{"chat", "/v1/chat/completions", `data: {"choices":[{"delta":{"content":"Hello"}}]}` + "\n\n"},
		{"responses", "/v1/responses", `event: response.output_text.delta` + "\n" + `data: {"type":"response.output_text.delta","delta":"Hello"}` + "\n\n"},
		{"messages", "/v1/messages", `event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meter := &usageCapture{target: httptest.NewRecorder(), route: tc.route, model: "gpt-5.6-sol"}
			meter.Header().Set("Content-Type", "text/event-stream")
			meter.WriteHeader(http.StatusOK)
			if _, err := meter.Write([]byte(tc.frame)); err != nil {
				t.Fatal(err)
			}
			request := []byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"hello"}]}`)
			got := meter.estimate(request)
			wantOutput := int64(tokenkit.Estimate("gpt-5.6-sol", "Hello"))
			if got.Input != estimateRequestTokens(tc.route, request, "gpt-5.6-sol") || got.Output != wantOutput {
				t.Fatalf("estimated usage = %+v, want request token count and %d output tokens", got, wantOutput)
			}
		})
	}
}

func TestClientDisconnectRecordsEstimatedStreamCharge(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			_, _ = io.WriteString(w, `{"files":[{"id":"one-id","name":"one.json","source":"file","provider":"codex"}]}`)
		case "/v8/management/credentials/models":
			_, _ = io.WriteString(w, `{"models":[{"id":"test-model"},{"id":"one/test-model"}]}`)
		case "/v8/management/config/api-keys":
			_, _ = io.WriteString(w, `{}`)
		case "/v0/management/oauth-model-alias":
			_, _ = io.WriteString(w, `{"oauth-model-alias":{}}`)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"test-model"},{"id":"one/test-model"}]}`)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
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
	logs, err := openUsageLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	if err := db.upsertUser(user{ID: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "limited", AccountIDs: []string{"one-id"}, BudgetPicoUSD: 1000 * picoUSD}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "user", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.savePrice("test-model", modelPrice{Expression: `p * 1000000 + c * 1000000`}); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("user", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAAPIKey: "proxy", CPAManagementKey: "management"}, db)
	a.logs = logs
	server := httptest.NewUnstartedServer(a.routes())
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.Start()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test-model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	chunk := make([]byte, 256)
	n, readErr := response.Body.Read(chunk)
	if n == 0 {
		t.Fatalf("stream did not start: bytes=%d err=%v", n, readErr)
	}
	cancel()
	response.Body.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, err := logs.list(context.Background(), usageFilter{UserID: "user", Since: time.Now().Add(-time.Minute)})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			row := rows[0]
			if row.Status != http.StatusOK || !row.Estimated || row.InputTokens == 0 || row.OutputTokens != int64(tokenkit.Estimate("test-model", "Hello")) || row.ChargePicoUSD == 0 {
				t.Fatalf("cancelled stream log = %+v", row)
			}
			stored, _ := db.getPlan(plan.ID)
			if stored.UsedPicoUSD != row.ChargePicoUSD {
				t.Fatalf("plan charge %d differs from log %d", stored.UsedPicoUSD, row.ChargePicoUSD)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("cancelled stream did not produce a usage log")
}

func TestInterruptedStreamSettlesReservationAndLogEstimate(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "limited", AccountIDs: []string{"account"}, BudgetPicoUSD: 100 * picoUSD}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "user", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	reservation, err := db.reserveUsage("user", plan.ID, 3*picoUSD, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	meter := &usageCapture{target: httptest.NewRecorder(), route: "/v1/responses", model: "gpt-5.6-sol"}
	meter.Header().Set("Content-Type", "text/event-stream")
	meter.WriteHeader(http.StatusOK)
	_, _ = meter.Write([]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n"))
	a := &app{}
	price := modelPrice{Expression: `p * 1000000 + c * 1000000`}
	request := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	usage, charge, estimated := a.settleUsage(reservation, price, meter, request, true)
	wantOutput := int64(tokenkit.Estimate("gpt-5.6-sol", "Hello"))
	if !estimated || usage.Input != estimateRequestTokens("/v1/responses", request, "gpt-5.6-sol") || usage.Output != wantOutput || charge == 0 {
		t.Fatalf("settled cancelled stream: usage=%+v charge=%d estimated=%v", usage, charge, estimated)
	}
	if got := db.plans()[0]; got.UsedPicoUSD != charge || got.UsageError != "" {
		t.Fatalf("cancelled stream was not charged to plan: %+v", got)
	}
	log := &usageLog{}
	setAuditUsage(log, usage, charge, estimated)
	if !log.Estimated || log.ChargePicoUSD != charge || log.OutputTokens != wantOutput {
		t.Fatalf("cancelled stream log = %+v", log)
	}
}
