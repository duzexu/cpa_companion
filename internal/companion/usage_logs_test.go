package companion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLogResponseWriterRecordsFirstBodyByte(t *testing.T) {
	tracked := &logResponseWriter{ResponseWriter: httptest.NewRecorder(), started: time.Now().Add(-50 * time.Millisecond)}
	tracked.WriteHeader(http.StatusOK)
	if tracked.firstByteMS != nil {
		t.Fatal("headers were counted as the first body byte")
	}
	if _, err := tracked.Write(nil); err != nil || tracked.firstByteMS != nil {
		t.Fatalf("empty write recorded first byte: %v", err)
	}
	if _, err := tracked.Write([]byte("first")); err != nil || tracked.firstByteMS == nil {
		t.Fatalf("first body write was not recorded: %v", err)
	}
	first := *tracked.firstByteMS
	if _, err := tracked.Write([]byte("second")); err != nil || *tracked.firstByteMS != first {
		t.Fatalf("later body write changed first-byte time: %v", err)
	}
}

func TestUsageLogCapturesFailureDetailsAndCacheRate(t *testing.T) {
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	tracked := &logResponseWriter{ResponseWriter: httptest.NewRecorder(), started: time.Now()}
	tracked.Header().Set("Content-Type", "application/json")
	tracked.WriteHeader(http.StatusTooManyRequests)
	_, _ = tracked.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"quota exhausted"}}`))
	code, message := tracked.errorDetails()
	if code != "rate_limit_exceeded" || message != "quota exhausted" {
		t.Fatalf("error details: %q %q", code, message)
	}
	if err := logs.insert(context.Background(), usageLog{At: time.Now(), UserID: "alice", Route: "/v1/responses", Status: tracked.status,
		ErrorCode: code, ErrorMessage: message, InputTokens: 100, CacheReadTokens: 25}); err != nil {
		t.Fatal(err)
	}
	rows, _, err := logs.list(context.Background(), usageFilter{UserID: "alice", Since: time.Now().Add(-time.Minute)})
	if err != nil || len(rows) != 1 || rows[0].ErrorCode != code || rows[0].ErrorMessage != message || rows[0].CacheHitRateText() != "25.0%" {
		t.Fatalf("saved log details: %+v %v", rows, err)
	}
	if rows[0].ErrorCodeText() != code || (usageLog{}).CacheHitRateText() != "—" {
		t.Fatal("error code or unknown cache rate rendered incorrectly")
	}
}

func TestForwardedUpstreamErrorIsSaved(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"code":"upstream_unavailable","message":"provider temporarily unavailable"}}`))
	})
	a, key, _ := meteredApp(t, upstream, 0)
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	a.logs = logs
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"cheap","messages":[{"role":"user","content":"test"}]}`))
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("upstream status changed: %d", w.Code)
	}
	rows, _, err := logs.list(context.Background(), usageFilter{UserID: "alice", Since: time.Now().Add(-time.Minute)})
	if err != nil || len(rows) != 1 || rows[0].ErrorCode != "upstream_unavailable" || rows[0].ErrorMessage != "provider temporarily unavailable" {
		t.Fatalf("forwarded error log: %+v %v", rows, err)
	}
}

func TestUsageLogsPersistAndRespectUserScope(t *testing.T) {
	var management atomic.Int32
	upstream := meteredCPA(t, &management, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":100,"completion_tokens":20}}`))
	})
	a, key, _ := meteredApp(t, upstream, 0)
	dir := t.TempDir()
	logs, err := openUsageLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.logs = logs
	a.cfg.AdminSubjects = map[string]bool{"alice": true}
	if err := a.db.upsertUser(user{ID: "alice", Username: "AliceDisplay"}); err != nil {
		t.Fatal(err)
	}
	call := func(key, body string) int {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w.Code
	}
	if got := call(key, `{"model":"pricey"}`); got != 200 {
		t.Fatalf("request status %d", got)
	}
	if got := call(key, `{"model":"missing"}`); got != 404 {
		t.Fatalf("invalid model status %d", got)
	}
	if err := a.db.upsertUser(user{ID: "bob", Username: "BobDisplay"}); err != nil {
		t.Fatal(err)
	}
	bobKey, err := a.db.createKeySecured("bob", "bob", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	if got := call(bobKey, `{"model":"pricey"}`); got != 403 {
		t.Fatalf("unassigned status %d", got)
	}
	if err := a.db.upsertUser(user{ID: "alice", Username: "AliceRenamed"}); err != nil {
		t.Fatal(err)
	}
	alice, more, err := logs.list(context.Background(), usageFilter{UserID: "alice", Since: time.Now().Add(-time.Hour), Limit: 50})
	if err != nil || more || len(alice) != 2 {
		t.Fatalf("alice logs %d, %v", len(alice), err)
	}
	if alice[1].Model != "pricey" || alice[1].Username != "AliceDisplay" || alice[1].AccountPrefix != "one" || alice[1].InputTokens != 100 || alice[1].OutputTokens != 20 || alice[1].ChargePicoUSD != 120_000_000_000 {
		t.Fatalf("priced request log: %+v", alice[1])
	}
	if alice[1].FirstByteMS == nil || *alice[1].FirstByteMS > alice[1].DurationMS {
		t.Fatalf("first-byte timing missing or exceeds total duration: %+v", alice[1])
	}
	if alice[0].Status != 404 || alice[0].Model != "missing" {
		t.Fatalf("failure request log: %+v", alice[0])
	}
	summary, err := logs.summary(context.Background(), usageFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil || summary.Requests != 3 || summary.Successful != 1 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	if err := logs.insert(context.Background(), usageLog{At: time.Now().Add(-7 * time.Hour), UserID: "alice", Model: "old-model", Route: "/v1/chat/completions", Status: 200}); err != nil {
		t.Fatal(err)
	}
	sessionPage := func(userID, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: userID, CSRF: "csrf", Expires: time.Now().Add(time.Hour).Unix()})})
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/dashboard", "/logs"} {
		bob := sessionPage("bob", path+"?scope=all&user=alice")
		if bob.Code != 200 || !strings.Contains(bob.Body.String(), "使用日志") && path == "/logs" {
			t.Fatalf("bob page %s: %d %s", path, bob.Code, bob.Body.String())
		}
		if strings.Contains(bob.Body.String(), "pricey") {
			t.Fatalf("bob saw alice log at %s", path)
		}
		admin := sessionPage("alice", path+"?scope=all")
		visible := strings.Contains(admin.Body.String(), "pricey")
		if path == "/dashboard" {
			visible = strings.Contains(admin.Body.String(), `<strong class="dash-figure">3</strong>`) && strings.Contains(admin.Body.String(), `<strong class="dash-stat-value">33%</strong>`)
			if !strings.Contains(admin.Body.String(), `已分配方案情况`) || !strings.Contains(admin.Body.String(), `class="dash-plan-name">metered</strong>`) ||
				!strings.Contains(bob.Body.String(), `已分配方案情况`) || !strings.Contains(bob.Body.String(), `class="dash-plan-name">尚未分配</strong>`) {
				t.Fatal("dashboard plan section leaked or hid the signed-in user's assignment")
			}
			for _, body := range []string{admin.Body.String(), bob.Body.String()} {
				if !strings.Contains(body, `所有用户的方案分配`) || !strings.Contains(body, `AliceRenamed`) ||
					!strings.Contains(body, `BobDisplay`) || !strings.Contains(body, `已分配 1 / 2 位用户`) ||
					!strings.Contains(body, `codex · one`) || !strings.Contains(body, `当前可用`) {
					t.Fatal("dashboard did not show the shared assignment list")
				}
				if !(strings.Index(body, `上游账号状态</h2>`) < strings.Index(body, `id="dash-plan-title"`) &&
					strings.Index(body, `id="dash-plan-title"`) < strings.Index(body, `id="dash-assignments-title"`) &&
					strings.Index(body, `id="dash-assignments-title"`) < strings.Index(body, `接下来去哪里</h2>`)) {
					t.Fatal("dashboard assignment sections are not directly above next steps")
				}
			}
			if strings.Contains(admin.Body.String(), "近 7 天") || strings.Contains(admin.Body.String(), "近 30 天") || !strings.Contains(admin.Body.String(), "最近 6 小时") {
				t.Fatal("dashboard still shows the old date ranges")
			}
			if strings.Contains(bob.Body.String(), `<strong class="dash-figure">3</strong>`) {
				t.Fatal("bob saw the all-user dashboard total")
			}
		}
		if path == "/logs" && (!strings.Contains(admin.Body.String(), `class="logs-table"`) || !strings.Contains(admin.Body.String(), `data-log-detail="log-detail-`) || !strings.Contains(admin.Body.String(), `class="log-dialog"`)) {
			t.Fatal("log rows or detail dialog missing")
		}
		if path == "/logs" {
			body := admin.Body.String()
			if !strings.Contains(body, `<td data-col="user"><span class="log-username">AliceDisplay</span></td>`) ||
				!strings.Contains(body, `<td data-col="user"><span class="log-username">BobDisplay</span></td>`) ||
				!strings.Contains(body, `<dt>用户名 / ID</dt><dd>AliceDisplay · alice</dd>`) {
				t.Fatal("admin log page did not resolve usernames")
			}
			if strings.Contains(bob.Body.String(), "AliceDisplay") {
				t.Fatal("bob saw alice's username")
			}
		}
		if admin.Code != 200 || !visible {
			t.Fatalf("admin page %s: %d", path, admin.Code)
		}
		if strings.Contains(admin.Body.String(), "ZgotmplZ") {
			t.Fatalf("template escaped valid page styles at %s", path)
		}
	}
	if err := logs.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openUsageLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	persisted, _, err := reopened.list(context.Background(), usageFilter{Since: time.Now().Add(-time.Hour)})
	if err != nil || len(persisted) != 3 {
		t.Fatalf("persisted logs %d, %v", len(persisted), err)
	}
}

func TestUsageLogTimeRangeAndAdvancedFilters(t *testing.T) {
	now := time.Date(2026, 10, 16, 8, 5, 0, 0, time.UTC)
	start, end, err := logTimeRange(url.Values{"start": {"2026-10-16T15:00"}, "end": {"2026-10-16T15:00"}}, now)
	if err != nil || !start.Equal(time.Date(2026, 10, 16, 7, 0, 0, 0, time.UTC)) || !end.Equal(start.Add(time.Minute)) {
		t.Fatalf("time range %v..%v: %v", start, end, err)
	}
	if _, _, err := logTimeRange(url.Values{"start": {"2026-10-17T15:00"}, "end": {"2026-10-16T15:00"}}, now); err == nil {
		t.Fatal("backwards time range accepted")
	}
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	for _, row := range []usageLog{
		{At: start.Add(30 * time.Second), UserID: "alice", KeyID: "key-1", AccountID: "upstream-1", Model: "model-a", Route: "/v1/responses", Status: 200},
		{At: start.Add(30 * time.Second), UserID: "alice", KeyID: "key-2", AccountID: "upstream-1", Model: "model-a", Route: "/v1/responses", Status: 429},
		{At: start.Add(30 * time.Second), UserID: "bob", KeyID: "key-1", AccountID: "upstream-1", Model: "model-a", Route: "/v1/responses", Status: 200},
		{At: end, UserID: "alice", KeyID: "key-1", AccountID: "upstream-1", Model: "model-a", Route: "/v1/responses", Status: 200},
	} {
		if err := logs.insert(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	filter := usageFilter{UserID: "alice", KeyID: "key-1", AccountID: "upstream-1", Route: "/v1/responses", Status: "success", Since: start, Until: end}
	rows, _, err := logs.list(context.Background(), filter)
	if err != nil || len(rows) != 1 || rows[0].UserID != "alice" {
		t.Fatalf("filtered rows=%+v err=%v", rows, err)
	}
}

func TestDashboardTrafficBucketsAndProviderAggregation(t *testing.T) {
	logs, err := openUsageLogs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	now := time.Date(2026, 10, 16, 10, 21, 0, 0, time.UTC)
	for _, row := range []usageLog{
		{At: now.Add(-17 * time.Minute), UserID: "alice", AccountID: "a", AccountPrefix: "a", Status: 200, Route: "/v1/chat/completions"},
		{At: now.Add(-15 * time.Minute), UserID: "alice", AccountID: "b", AccountPrefix: "b", Status: 429, Route: "/v1/chat/completions"},
		{At: now.Add(-12 * time.Minute), UserID: "alice", Status: 429, ErrorCode: "model_cooldown", Route: "/v1/chat/completions"},
		{At: now.Add(-11 * time.Minute), UserID: "alice", Status: 429, ErrorCode: "HTTP 429", Route: "/v1/chat/completions"},
		{At: now.Add(-7 * time.Minute), UserID: "alice", AccountID: "b", AccountPrefix: "b", Status: 200, Route: "/v1/chat/completions"},
		{At: now.Add(-5 * time.Minute), UserID: "bob", AccountID: "b", AccountPrefix: "b", Status: 200, Route: "/v1/chat/completions"},
	} {
		if err := logs.insert(context.Background(), row); err != nil {
			t.Fatal(err)
		}
	}
	buckets, accounts, err := logs.traffic(context.Background(), "alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 37 || len(accounts) != 2 {
		t.Fatalf("buckets=%d accounts=%d", len(buckets), len(accounts))
	}
	var successes, failures int64
	for _, b := range buckets {
		successes += b.Success
		failures += b.Failed
	}
	if successes != 2 || failures != 1 {
		t.Fatalf("success=%d failed=%d", successes, failures)
	}
	allLogs, _, err := logs.list(context.Background(), usageFilter{UserID: "alice", Since: now.Add(-time.Hour), Limit: 50})
	if err != nil || len(allLogs) != 5 {
		t.Fatalf("local 429s disappeared from request logs: rows=%d err=%v", len(allLogs), err)
	}
	providers := dashboardProviders(routingPlan{AccountIDs: []string{"a", "b"}}, true, false, accounts, map[string]string{"a": "codex", "b": "codex"})
	if len(providers) != 1 || providers[0].Requests != 3 || providers[0].Success != 2 || providers[0].Accounts != 2 || providers[0].Points == "" {
		t.Fatalf("provider aggregation: %+v", providers)
	}
}
