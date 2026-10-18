package companion

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestImageUsageCaptureWithLargeJSONAndSSE(t *testing.T) {
	const usage = `"usage":{"input_tokens":120,"output_tokens":4000,"input_tokens_details":{"cached_tokens":20}}`
	for _, tc := range []struct {
		name, contentType, body string
	}{
		{"json", "application/json", `{"data":[{"b64_json":"` + strings.Repeat("A", maxUsageCapture+1) + `"}],` + usage + `}`},
		{"sse", "text/event-stream", "event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"" + strings.Repeat("A", 2<<20) + "\"," + usage + "}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := httptest.NewRecorder()
			meter := &usageCapture{target: out, route: "/v1/images/generations"}
			meter.Header().Set("Content-Type", tc.contentType)
			for start := 0; start < len(tc.body); start += 4093 {
				end := start + 4093
				if end > len(tc.body) {
					end = len(tc.body)
				}
				_, _ = meter.Write([]byte(tc.body[start:end]))
			}
			got, err := meter.result()
			if err != nil || got != (tokenUsage{Input: 120, Output: 4000, CacheRead: 20}) {
				t.Fatalf("image usage = %+v, %v", got, err)
			}
			if out.Body.String() != tc.body {
				t.Fatal("response changed while metering")
			}
		})
	}
}

func TestImageUsageCaptureRejectsMissingOrZeroUsage(t *testing.T) {
	for _, body := range []string{
		`{"data":[{"b64_json":"AA=="}]}`,
		`{"data":[{"b64_json":"AA=="}],"usage":{"total_tokens":0}}`,
		`{"data":[{"b64_json":"AA=="}],"usage":{"input_tokens":0,"output_tokens":0}}`,
		`{"metadata":{"usage":{"input_tokens":10,"output_tokens":20}},"data":[{"b64_json":"AA=="}]}`,
		`{"usage":{"input_tokens":10,"output_tokens":20},"data":[{"b64_json":"unfinished`,
	} {
		meter := &usageCapture{target: httptest.NewRecorder(), route: "/v1/images/generations"}
		meter.Header().Set("Content-Type", "application/json")
		_, _ = meter.Write([]byte(body))
		if got, err := meter.result(); err == nil {
			t.Fatalf("unusable image usage accepted: %+v", got)
		}
	}
}

func TestBudgetedImageGenerationChecksReservationBeforeForwarding(t *testing.T) {
	var calls atomic.Int32
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	a, key, _ := meteredApp(t, upstream, picoUSD)
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"pricey","prompt":"draw a tree"}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || calls.Load() != 0 {
		t.Fatalf("insufficient image reservation status=%d upstream=%d", w.Code, calls.Load())
	}
}

func TestImageMeterableRoute(t *testing.T) {
	if !meterableRoute("/v1/images/generations") || meterableRoute("/v1/images/edits") {
		t.Fatal("unexpected image metering routes")
	}
	meter := &usageCapture{target: httptest.NewRecorder(), route: "/v1/images/generations"}
	meter.WriteHeader(http.StatusBadGateway)
	if _, err := meter.result(); err == nil {
		t.Fatal("failed image response returned usage")
	}
}

func TestImageReservationScalesWithSizeQualityAndCount(t *testing.T) {
	price := modelPrice{Expression: `tier("standard", c * 30)`}
	base, err := estimateRequestCharge("/v1/images/generations", []byte(`{"model":"gpt-image-2","size":"1024x1024"}`), price)
	if err != nil {
		t.Fatal(err)
	}
	high, err := estimateRequestCharge("/v1/images/generations", []byte(`{"model":"gpt-image-2","size":"1536x1024","quality":"high","n":2}`), price)
	if err != nil || high != base*3*2 {
		t.Fatalf("image reservation base=%d high=%d err=%v", base, high, err)
	}
}

func TestBudgetedImageGenerationSettlesActualUsage(t *testing.T) {
	var calls atomic.Int32
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"AA=="}],"usage":{"input_tokens":120,"output_tokens":4000}}`))
	})
	a, key, planID := meteredApp(t, upstream, 1000*picoUSD)
	body := `{"model":"pricey","prompt":"draw a tree","size":"1024x1024"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("image request status=%d upstream=%d body=%q", w.Code, calls.Load(), w.Body.String())
	}
	plan, ok := a.db.getPlan(planID)
	if !ok || plan.UsedPicoUSD != 4_120_000_000_000 || plan.UsageError != "" {
		t.Fatalf("image charge was not settled from CPA usage: %+v", plan)
	}
}

func TestBudgetedImageGenerationMissingUsagePausesPlan(t *testing.T) {
	var calls atomic.Int32
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"AA=="}],"usage":{"total_tokens":0}}`))
	})
	a, key, planID := meteredApp(t, upstream, 1000*picoUSD)
	body := `{"model":"pricey","prompt":"draw a tree","size":"1024x1024"}`
	estimate, err := estimateRequestCharge("/v1/images/generations", []byte(body), modelPrice{Expression: `tier("standard", p * 1000 + c * 1000)`})
	if err != nil {
		t.Fatal(err)
	}
	request := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if w := request(); w.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("first image request status=%d upstream=%d", w.Code, calls.Load())
	}
	plan, _ := a.db.getPlan(planID)
	if plan.UsedPicoUSD != estimate || plan.UsageError == "" {
		t.Fatalf("missing usage did not charge estimate and pause plan: %+v, estimate=%d", plan, estimate)
	}
	if w := request(); w.Code != http.StatusForbidden || calls.Load() != 1 {
		t.Fatalf("paused plan request status=%d upstream=%d", w.Code, calls.Load())
	}
}
