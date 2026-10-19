package companion

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAuditExtractsConversationWithoutToolsOrThinking(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"private system text"},{"role":"user","content":[{"type":"text","text":"old user"},{"type":"image_url","image_url":{"url":"data:image/png;base64,secret"}}]},{"role":"assistant","content":[{"type":"thinking","thinking":"private chain"},{"type":"text","text":"<think>hidden thought</think>old assistant"}],"tool_calls":[{"function":{"arguments":"private tool call"}}]},{"role":"tool","content":"private tool output"},{"role":"user","content":[{"type":"tool_result","content":"private tool result"},{"type":"text","text":"current user"}]}]}`)
	got := auditInputText(body)
	if got != "用户：\nold user\n\n助手：\nold assistant\n\n用户：\ncurrent user" {
		t.Fatalf("unexpected extracted input: %q", got)
	}
	for _, forbidden := range []string{"private", "base64", "hidden thought"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("extracted tool, thinking or image data: %q", got)
		}
	}
	for _, test := range []struct{ body, want string }{
		{`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"hidden"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply"}]},{"type":"function_call_output","output":"secret"},{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`, "用户：\nfirst\n\n助手：\nreply\n\n用户：\nnext"},
		{`{"contents":[{"role":"user","parts":[{"text":"first"}]},{"role":"model","parts":[{"thought":true,"text":"hidden"},{"text":"reply"},{"functionCall":{"name":"lookup"}}]},{"role":"user","parts":[{"functionResponse":{"response":"secret"}},{"text":"next"}]}]}`, "用户：\nfirst\n\n助手：\nreply\n\n用户：\nnext"},
		{`{"prompt":"standalone prompt"}`, "用户：\nstandalone prompt"},
	} {
		if got := auditInputText([]byte(test.body)); got != test.want {
			t.Fatalf("conversation extraction = %q, want %q", got, test.want)
		}
	}
	longMessage := strings.Repeat("x", 13_000)
	if got := auditInputText([]byte(`{"messages":[{"role":"user","content":"` + longMessage + `"}]}`)); !strings.HasSuffix(got, longMessage) {
		t.Fatal("conversation was truncated at the old single-message audit limit")
	}
}

func TestAuditSessionFirstAndSampling(t *testing.T) {
	var sessions auditSessions
	now := time.Now()
	if inspect, first := sessions.shouldInspect("user", routeSession{ID: "conversation"}, 0, now); !inspect || !first {
		t.Fatal("first request was not inspected")
	}
	if inspect, first := sessions.shouldInspect("user", routeSession{ID: "conversation"}, 0, now.Add(time.Second)); inspect || first {
		t.Fatal("repeated request ignored zero sampling rate")
	}
	if inspect, first := sessions.shouldInspect("user", routeSession{}, 0, now); !inspect || !first {
		t.Fatal("unidentified session was not inspected")
	}
	if inspect, first := sessions.shouldInspect("other-user", routeSession{ID: "conversation"}, 0, now); !inspect || !first {
		t.Fatal("session tracking leaked across users")
	}
}

func TestAuditAnonymousConversationHistory(t *testing.T) {
	first := []byte(`{"messages":[{"role":"system","content":"rules"},{"role":"user","content":"hello"}]}`)
	continued := []byte(`{"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":"hi"},{"role":"user","content":"again"}]}`)
	if anonymousConversationHasHistory(first) || !anonymousConversationHasHistory(continued) {
		t.Fatal("anonymous chat history did not distinguish first and later user turns")
	}
}

func TestAuditDecisionAndKeywordFallback(t *testing.T) {
	thresholds := defaultAuditConfig().Thresholds
	decision, err := decideAuditScores(auditTestResponse("hate", 0.70), thresholds)
	if err != nil || !decision.Hit || decision.Category != "hate" || decision.Score != 70 {
		t.Fatalf("unexpected decision: %+v, %v", decision, err)
	}
	if _, err := decideAuditScores([]byte(`{"answers":{"hate":{"type":"noul","noul":0.7}}}`), thresholds); err == nil {
		t.Fatal("missing scores did not trigger fallback")
	}
	if _, err := decideAuditScores([]byte(`{"results":[{"category_scores":{"hate":0.7}}]}`), thresholds); err == nil {
		t.Fatal("non-TypeSafe response was accepted")
	}
	if fallback := keywordAudit("This contains WATCHWORD", []string{"watchword"}); !fallback.Hit || !fallback.Fallback {
		t.Fatalf("keyword fallback missed: %+v", fallback)
	}
}

func auditTestResponse(hitCategory string, hitScore float64) []byte {
	answers := map[string]any{}
	for _, category := range auditCategories {
		score := 0.01
		if category.Name == hitCategory {
			score = hitScore
		}
		answers[auditQuestionID(category.Name)] = map[string]any{"type": "noul", "noul": score}
	}
	data, _ := json.Marshal(map[string]any{"model": "typesafe-ai/jev", "answers": answers})
	return data
}

func TestAuditDecisionsEndpointUsesFullURLAndObjectState(t *testing.T) {
	const conversation = "用户：\n第一条消息\n\n助手：\n上一轮回复\n\n用户：\n当前消息"
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/decisions/" || r.URL.RawQuery != "tenant=qa" {
			t.Errorf("unexpected decisions endpoint: %s %s", r.Method, r.URL.String())
		}
		if r.Header.Get("Authorization") != "Bearer decisions-test-key" {
			t.Error("missing decisions API key")
		}
		var body struct {
			Model string `json:"model"`
			State struct {
				Conversation string `json:"conversation"`
			} `json:"state"`
			Questions map[string]struct {
				Type string `json:"type"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "typesafe/jev-latest" ||
			body.State.Conversation != conversation || len(body.Questions) != len(auditCategories) {
			t.Errorf("invalid decisions request: %+v, %v", body, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(auditTestResponse("hate", 0.7))
	}))
	defer provider.Close()
	sealed, err := sealAPIKey(strings.Repeat("a", 64), "systemone", "audit", "decisions-test-key")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: settings{CookieKey: strings.Repeat("a", 64)}}
	decision, err := a.systemOneDecision(context.Background(), auditEndpoint{
		BaseURL: provider.URL + "/v1/decisions/?tenant=qa", Model: "typesafe/jev-latest", APIKeySealed: sealed,
	}, defaultAuditConfig().Thresholds, conversation)
	if err != nil || !decision.Hit || decision.Category != "hate" || decision.Score != 70 {
		t.Fatalf("unexpected decisions result: %+v, %v", decision, err)
	}
}

func TestAuditPersistsOnlyHitInputAndFallsBack(t *testing.T) {
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
	var fail atomic.Bool
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/typesafe/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("unexpected decision request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer gateway-test-key" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected decision request headers")
		}
		var body struct {
			Model     string `json:"model"`
			State     string `json:"state"`
			Questions map[string]struct {
				Type         string `json:"type"`
				Instructions string `json:"instructions"`
			} `json:"questions"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "custom-jev" || body.State == "" || len(body.Questions) != len(auditCategories) {
			t.Errorf("invalid TypeSafe request: %+v, %v", body, err)
		}
		for _, category := range auditCategories {
			question := body.Questions[auditQuestionID(category.Name)]
			if question.Type != "noul" || question.Instructions != category.Question {
				t.Errorf("invalid %s question: %+v", category.Name, question)
			}
		}
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(auditTestResponse("hate", 0.7))
	}))
	defer provider.Close()
	cfg := defaultAuditConfig()
	cfg.Enabled = true
	cfg.Endpoints[0].Model = "custom-jev"
	cfg.Endpoints[0].BaseURL = provider.URL + "/typesafe/v1/systemone"
	cfg.SamplePercent = 0
	cfg.Keywords = []string{"watchword"}
	cookieKey := strings.Repeat("a", 64)
	cfg.Endpoints[0].APIKeySealed, err = sealAPIKey(cookieKey, "systemone", "audit", "gateway-test-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.saveAuditSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, logs: logs, cfg: settings{CookieKey: cookieKey}}
	usage := &usageLog{UserID: "user", Username: "AliceName", KeyID: "key", PlanID: "plan", Model: "model", Route: "/v1/chat/completions"}
	request := func(session, text string) {
		t.Helper()
		body := []byte(`{"model":"model","messages":[{"role":"user","content":"` + text + `"}]}`)
		h := make(http.Header)
		h.Set("X-Session-ID", session)
		if err := a.inspectContent(context.Background(), h, body, usage); err != nil {
			t.Fatal(err)
		}
	}
	request("first", "first input")
	request("first", "not sampled")
	fail.Store(true)
	request("second", "watchword input")
	request("third", "ordinary input")
	rows, _, err := logs.auditRecords(context.Background(), "user", "", 1)
	if err != nil || len(rows) != 3 {
		t.Fatalf("expected 3 inspected records, got %d: %v", len(rows), err)
	}
	for _, row := range rows {
		if row.Username != "AliceName" || row.UserID != "user" {
			t.Fatalf("audit identity was not persisted: %+v", row)
		}
	}
	if rows[0].InputText != "" || !rows[0].Fallback || rows[0].Hit {
		t.Fatalf("non-hit stored content or missed fallback: %+v", rows[0])
	}
	if !rows[1].Hit || !rows[1].Fallback || rows[1].InputText != "用户：\nwatchword input" {
		t.Fatalf("keyword hit missing: %+v", rows[1])
	}
	if !rows[2].Hit || rows[2].Fallback || rows[2].InputText != "用户：\nfirst input" {
		t.Fatalf("SystemOne hit missing: %+v", rows[2])
	}
	if err := db.upsertUser(user{ID: "user", Username: "RenamedLater"}); err != nil {
		t.Fatal(err)
	}
	a.cfg.AdminSubjects = map[string]bool{"user": true}
	r := httptest.NewRequest(http.MethodGet, "/audit", nil)
	r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: "user", CSRF: "csrf", Expires: time.Now().Add(time.Hour).Unix()})})
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `<strong class="audit-user-name">AliceName</strong>`) ||
		!strings.Contains(w.Body.String(), `ID user`) || !strings.Contains(w.Body.String(), `Key key`) {
		t.Fatalf("audit page omitted the saved username and user ID: status %d", w.Code)
	}
}

func TestAuditConfigPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultAuditConfig()
	cfg.Enabled = true
	cfg.Endpoints[0].Model = "provider/jev-v2"
	cfg.SamplePercent = 25
	cfg.Keywords = []string{"example"}
	cfg.Endpoints[0].APIKeySealed, err = sealAPIKey(strings.Repeat("a", 64), "systemone", "audit", "gateway-test-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.saveAuditSettings(cfg); err != nil {
		t.Fatal(err)
	}
	db, err = openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got := db.auditSettings()
	if !got.Enabled || len(got.Endpoints) != 1 || got.Endpoints[0].Model != "provider/jev-v2" || got.SamplePercent != 25 || got.Thresholds["hate"] != 65 || len(got.Keywords) != 1 {
		t.Fatalf("audit settings did not survive reload: %+v", got)
	}
}

func TestAuditEndpointFailoverAndUsageLogLink(t *testing.T) {
	dir := t.TempDir()
	db, err := openStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logs, err := openUsageLogs(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	var primaryCalls, backupCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "limited", http.StatusTooManyRequests)
	}))
	defer primary.Close()
	var backupFails atomic.Bool
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupCalls.Add(1)
		if backupFails.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		var request struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.State != "用户：\nprevious user\n\n助手：\nprevious reply\n\n用户：\ncurrent user" {
			t.Errorf("backup endpoint received the wrong conversation: %q, %v", request.State, err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(auditTestResponse("hate", 0.7))
	}))
	defer backup.Close()
	cookieKey := strings.Repeat("a", 64)
	sealed, err := sealAPIKey(cookieKey, "systemone", "audit", "gateway-test-key")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultAuditConfig()
	cfg.Enabled = true
	cfg.SamplePercent = 0
	cfg.Keywords = []string{"watchword"}
	cfg.Endpoints = []auditEndpoint{
		{ID: "primary", Name: "主接口", BaseURL: primary.URL + "/typesafe/v1/systemone", Model: "jev-latest", APIKeySealed: sealed},
		{ID: "backup", Name: "备用接口", BaseURL: backup.URL + "/typesafe/v1/systemone", Model: "jev-latest", APIKeySealed: sealed},
	}
	if err := db.saveAuditSettings(cfg); err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, logs: logs, cfg: settings{CookieKey: cookieKey}}
	usage := usageLog{At: time.Now(), UserID: "user", Username: "Alice", KeyID: "key", PlanID: "plan", Model: "model", Route: "/v1/responses", Status: 200}
	body := []byte(`{"model":"model","input":[{"role":"user","content":"previous user"},{"role":"assistant","content":"previous reply"},{"role":"user","content":"current user"}]}`)
	if err := a.inspectContent(context.Background(), nil, body, &usage); err != nil {
		t.Fatal(err)
	}
	backupFails.Store(true)
	body = []byte(`{"model":"model","input":[{"role":"user","content":"watchword"},{"role":"assistant","content":"previous reply"},{"role":"user","content":"ordinary follow-up"}]}`)
	if err := a.inspectContent(context.Background(), nil, body, &usage); err != nil {
		t.Fatal(err)
	}
	if primaryCalls.Load() != 2 || backupCalls.Load() != 2 || len(usage.AuditIDs) != 2 {
		t.Fatalf("failover calls or IDs: primary=%d backup=%d IDs=%v", primaryCalls.Load(), backupCalls.Load(), usage.AuditIDs)
	}
	if err := logs.insert(context.Background(), usage); err != nil {
		t.Fatal(err)
	}
	rows, _, err := logs.auditRecords(context.Background(), "user", "", 1)
	if err != nil || len(rows) != 2 {
		t.Fatalf("audit records: %d %v", len(rows), err)
	}
	if rows[1].EndpointName != "备用接口" || rows[1].AttemptCount != 2 || !rows[1].Hit || rows[1].Fallback || rows[1].InputText != "用户：\nprevious user\n\n助手：\nprevious reply\n\n用户：\ncurrent user" {
		t.Fatalf("backup result: %+v", rows[1])
	}
	if !rows[0].Fallback || rows[0].EndpointName != "" || rows[0].AttemptCount != 2 || !rows[0].Hit || !strings.Contains(rows[0].InputText, "ordinary follow-up") {
		t.Fatalf("keyword fallback: %+v", rows[0])
	}
	if rows[0].UsageLogID == 0 || rows[0].UsageLogID != rows[1].UsageLogID || !strings.Contains(rows[0].LogURL(), "log_id=") {
		t.Fatalf("audit links: %+v %+v", rows[0], rows[1])
	}
	listed, _, err := logs.list(context.Background(), usageFilter{ID: rows[0].UsageLogID, UserID: "user", Since: time.Unix(0, 0)})
	if err != nil || len(listed) != 1 || listed[0].UserID != "user" {
		t.Fatalf("linked usage log: %+v %v", listed, err)
	}
	if err := db.upsertUser(user{ID: "user", Username: "Alice"}); err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "other", Username: "Other"}); err != nil {
		t.Fatal(err)
	}
	a.cfg.AdminSubjects = map[string]bool{"user": true}
	auditPageRequest := httptest.NewRequest(http.MethodGet, "/audit", nil)
	auditPageRequest.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: "user", CSRF: "csrf", Expires: time.Now().Add(time.Hour).Unix()})})
	auditPageResponse := httptest.NewRecorder()
	a.routes().ServeHTTP(auditPageResponse, auditPageRequest)
	logDialogID := "log-detail-" + strconv.FormatInt(rows[0].UsageLogID, 10)
	linked := strings.Contains(auditPageResponse.Body.String(), `data-audit-open="`+logDialogID+`"`) &&
		strings.Contains(auditPageResponse.Body.String(), `class="log-dialog" id="`+logDialogID+`"`) &&
		strings.Contains(auditPageResponse.Body.String(), "Alice · user") &&
		!strings.Contains(auditPageResponse.Body.String(), "log_id="+strconv.FormatInt(rows[0].UsageLogID, 10))
	endpointShown := strings.Contains(auditPageResponse.Body.String(), "SystemOne · 备用接口")
	if auditPageResponse.Code != http.StatusOK || !linked || !endpointShown {
		t.Fatalf("audit page status=%d linked=%t endpoint=%t", auditPageResponse.Code, linked, endpointShown)
	}
	for _, subject := range []struct {
		id      string
		visible bool
	}{{"user", true}, {"other", false}} {
		r := httptest.NewRequest(http.MethodGet, rows[0].LogURL(), nil)
		r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: subject.id, CSRF: "csrf", Expires: time.Now().Add(time.Hour).Unix()})})
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "正在查看审计关联的请求日志") != true {
			t.Fatalf("linked log page status/content for %s: %d", subject.id, w.Code)
		}
		if got := strings.Contains(w.Body.String(), "共 1 条"); got != subject.visible {
			t.Fatalf("linked log visibility for %s = %t", subject.id, got)
		}
	}
}

func TestAuditSettingsFormKeepsKeysWithReorderedEndpoints(t *testing.T) {
	dir := t.TempDir()
	db, err := openStore(filepath.Join(dir, "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := strings.Repeat("a", 64)
	first, err := sealAPIKey(key, "systemone", "audit", "first-secret")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sealAPIKey(key, "systemone", "audit", "second-secret")
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultAuditConfig()
	cfg.Enabled = true
	cfg.Keywords = []string{"watchword"}
	cfg.Endpoints = []auditEndpoint{
		{ID: "first", Name: "First", BaseURL: "https://first.example/typesafe/v1/systemone", Model: "jev-latest", APIKeySealed: first},
		{ID: "second", Name: "Second", BaseURL: "https://second.example/typesafe/v1/systemone", Model: "jev-latest", APIKeySealed: second},
	}
	if err := db.saveAuditSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "admin", Username: "Admin"}); err != nil {
		t.Fatal(err)
	}
	a := &app{db: db, cfg: settings{CookieKey: key, AdminSubjects: map[string]bool{"admin": true}}}
	form := url.Values{"csrf": {"csrf-token"}, "enabled": {"on"}, "sample_percent": {"10"}, "timeout_ms": {"2500"}, "endpoint_count": {"3"}, "keywords": {"watchword"}}
	for _, category := range auditCategories {
		form.Set("threshold_"+category.Name, strconv.Itoa(category.Default))
	}
	for index, endpoint := range []auditEndpoint{cfg.Endpoints[1], cfg.Endpoints[0]} {
		prefix := "endpoint_" + strconv.Itoa(index) + "_"
		form.Set(prefix+"id", endpoint.ID)
		form.Set(prefix+"name", endpoint.Name)
		form.Set(prefix+"model", endpoint.Model)
		form.Set(prefix+"base_url", endpoint.BaseURL)
	}
	form.Set("endpoint_2_name", "Third")
	form.Set("endpoint_2_model", "jev-latest")
	form.Set("endpoint_2_base_url", "https://third.example/typesafe/v1/systemone")
	form.Set("endpoint_2_api_key", "third-secret")
	post := func(values url.Values) int {
		r := httptest.NewRequest(http.MethodPost, "/admin/audit", strings.NewReader(values.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: "admin", CSRF: "csrf-token", Expires: time.Now().Add(time.Hour).Unix()})})
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w.Code
	}
	bad := url.Values{}
	for k, v := range form {
		bad[k] = append([]string(nil), v...)
	}
	bad.Set("csrf", "wrong")
	if got := post(bad); got != http.StatusForbidden {
		t.Fatalf("CSRF status = %d", got)
	}
	bad = url.Values{}
	for k, v := range form {
		bad[k] = append([]string(nil), v...)
	}
	bad.Set("endpoint_2_base_url", "https://third.example")
	if got := post(bad); got != http.StatusBadRequest {
		t.Fatalf("URL validation status = %d", got)
	}
	if got := post(form); got != http.StatusSeeOther {
		t.Fatalf("save status = %d", got)
	}
	got := db.auditSettings().Endpoints
	if len(got) != 3 || got[0].ID != "second" || got[1].ID != "first" || got[2].ID == "" || got[2].ID == "first" || got[2].ID == "second" {
		t.Fatalf("endpoint order or IDs: %+v", got)
	}
	for i, want := range []string{"second-secret", "first-secret", "third-secret"} {
		plain, err := openAPIKey(key, "systemone", "audit", got[i].APIKeySealed)
		if err != nil || plain != want {
			t.Fatalf("endpoint %d key mismatch: %v", i, err)
		}
	}
}
