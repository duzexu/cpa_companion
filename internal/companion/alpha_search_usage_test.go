package companion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAlphaSearchChargesUsageAndHidesUnmeteredRoutes(t *testing.T) {
	const requestBody = `{"model":"pricey","query":"golang channels"}`
	const estimatedResponse = `{"results":[{"url":"https://example.com"}]}`
	const meteredResponse = `{"results":[],"usage":{"input_tokens":120,"output_tokens":30}}`
	var calls, management atomic.Int32
	var returnUsage atomic.Bool
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Model != "one/pricey" {
			http.Error(w, "wrong upstream model", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if returnUsage.Load() {
			_, _ = w.Write([]byte(meteredResponse))
		} else {
			_, _ = w.Write([]byte(estimatedResponse))
		}
	})
	a, key, planID := meteredApp(t, upstream, 1000*picoUSD)
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a.logs = logs
	call := func(route string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, route, strings.NewReader(requestBody))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}

	if !meterableRoute("/v1/alpha/search") {
		t.Fatal("alpha search is not metered")
	}
	if response := call("/v1/alpha/search"); response.Code != http.StatusOK || response.Body.String() != estimatedResponse {
		t.Fatalf("search response: %d %q", response.Code, response.Body.String())
	}
	price, _ := a.db.priceFor("pricey")
	estimatedUsage := tokenUsage{Input: int64(len(requestBody) / estimatedRequestBytesPerToken), Output: int64(len(estimatedResponse) / estimatedResponseBytesPerToken)}
	estimatedCharge, err := price.charge(estimatedUsage)
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := a.db.getPlan(planID)
	if plan.UsedPicoUSD != estimatedCharge || plan.UsageError != "" {
		t.Fatalf("estimated search charge=%d error=%q, want %d", plan.UsedPicoUSD, plan.UsageError, estimatedCharge)
	}
	rows, _, err := logs.list(context.Background(), usageFilter{UserID: "alice", Route: "/v1/alpha/search", Since: time.Now().Add(-time.Minute)})
	if err != nil || len(rows) != 1 || !rows[0].Estimated || rows[0].ChargePicoUSD != estimatedCharge {
		t.Fatalf("estimated search log: %+v, %v", rows, err)
	}

	returnUsage.Store(true)
	if response := call("/v1/alpha/search"); response.Code != http.StatusOK || response.Body.String() != meteredResponse {
		t.Fatalf("metered search response: %d %q", response.Code, response.Body.String())
	}
	actualCharge, err := price.charge(tokenUsage{Input: 120, Output: 30})
	if err != nil {
		t.Fatal(err)
	}
	plan, _ = a.db.getPlan(planID)
	if plan.UsedPicoUSD != estimatedCharge+actualCharge || plan.UsageError != "" {
		t.Fatalf("search charges=%d error=%q, want %d", plan.UsedPicoUSD, plan.UsageError, estimatedCharge+actualCharge)
	}
	rows, _, err = logs.list(context.Background(), usageFilter{UserID: "alice", Route: "/v1/alpha/search", Since: time.Now().Add(-time.Minute)})
	if err != nil || len(rows) != 2 || rows[0].Estimated || rows[0].ChargePicoUSD != actualCharge {
		t.Fatalf("metered search log: %+v, %v", rows, err)
	}

	for _, route := range []string{"/v1/messages/count_tokens", "/v1/audio/speech", "/v1/tts"} {
		if forwardJSONRoute(route) || meterableRoute(route) {
			t.Fatalf("unexposed route %s is forwarded or metered", route)
		}
		if response := call(route); response.Code != http.StatusBadRequest {
			t.Fatalf("unexposed route %s returned %d", route, response.Code)
		}
	}
	if calls.Load() != 2 {
		t.Fatalf("unexpected upstream calls: %d", calls.Load())
	}
	plan, _ = a.db.getPlan(planID)
	if plan.UsedPicoUSD != estimatedCharge+actualCharge {
		t.Fatalf("hidden routes changed usage: %d", plan.UsedPicoUSD)
	}

	limited, limitedKey, _ := meteredApp(t, upstream, 1)
	request := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer "+limitedKey)
	response := httptest.NewRecorder()
	limited.routes().ServeHTTP(response, request)
	if response.Code != http.StatusForbidden || calls.Load() != 2 {
		t.Fatalf("search bypassed the pre-request budget check: status=%d upstream=%d", response.Code, calls.Load())
	}
}
