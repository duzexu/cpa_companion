package companion

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestResponsesLiteContextForwarding(t *testing.T) {
	tests := []struct {
		name      string
		header    string
		body      string
		want      string
		unchanged bool
	}{
		{name: "ordinary response", body: `{"model":"gpt-6.1-sol","input":"OK"}`, unchanged: true},
		{name: "header with missing reasoning", header: "true", body: `{"model":"gpt-6.1-sol","input":"OK"}`, want: "all_turns"},
		{name: "header with incompatible context", header: " TRUE ", body: `{"model":"gpt-6.1-sol","input":"OK","reasoning":{"context":"current_turn","effort":"low"}}`, want: "all_turns"},
		{name: "metadata mirror boolean", body: `{"model":"gpt-6.1-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":true}}`, want: "all_turns"},
		{name: "metadata mirror string", body: `{"model":"gpt-6.1-sol","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`, want: "all_turns"},
		{name: "already compatible", header: "true", body: `{"model":"gpt-6.1-sol","reasoning":{"context":"all_turns"}}`, want: "all_turns", unchanged: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.body)
			payload, _, err := requestModel(original)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			if tc.header != "" {
				r.Header.Set(responsesLiteHeader, tc.header)
			}
			forwarded, err := ensureResponsesLiteContext(r.Header, payload, original)
			if err != nil {
				t.Fatal(err)
			}
			if tc.unchanged && string(forwarded) != tc.body {
				t.Fatalf("unnecessary body rewrite: %s", forwarded)
			}
			r.Body = io.NopCloser(strings.NewReader(string(forwarded)))
			if err := prepareForwardRequest(r, "assigned"); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Model     string `json:"model"`
				Reasoning struct {
					Context string `json:"context"`
					Effort  string `json:"effort"`
				} `json:"reasoning"`
			}
			if err := json.NewDecoder(r.Body).Decode(&result); err != nil {
				t.Fatal(err)
			}
			if result.Model != "assigned/gpt-6.1-sol" || result.Reasoning.Context != tc.want {
				t.Fatalf("forwarded model=%q context=%q, want context %q", result.Model, result.Reasoning.Context, tc.want)
			}
			if tc.name == "header with incompatible context" && result.Reasoning.Effort != "low" {
				t.Fatal("existing reasoning effort was lost")
			}
		})
	}
}

func TestResponsesLiteRequestPassesStrictCPA(t *testing.T) {
	var gotModel, gotContext string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			_, _ = io.WriteString(w, `{"files":[{"id":"codex-id","name":"codex.json","provider":"codex","source":"file","prefix":"codex"}]}`)
		case "/v8/management/config/api-keys":
			_, _ = io.WriteString(w, `{}`)
		case "/v0/management/oauth-model-alias":
			_, _ = io.WriteString(w, `{"oauth-model-alias":{}}`)
		case "/v8/management/credentials/models":
			_, _ = io.WriteString(w, `{"models":[{"id":"codex/gpt-6.1-sol"},{"id":"gpt-6.1-sol"}]}`)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"codex/gpt-6.1-sol"},{"id":"gpt-6.1-sol"}]}`)
		case "/v1/responses":
			var payload struct {
				Model     string `json:"model"`
				Reasoning struct {
					Context string `json:"context"`
				} `json:"reasoning"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}
			gotModel, gotContext = payload.Model, payload.Reasoning.Context
			if strings.EqualFold(r.Header.Get(responsesLiteHeader), "true") && gotContext != "all_turns" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"unsupported_value","message":"Responses Lite requires all_turns"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"resp_test","usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	baseURL, _ := url.Parse(upstream.URL)
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "test", AccountIDs: []string{"codex-id"}}); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "user", db.plans()[0].ID, false); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("user", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: baseURL, CPABaseURL: baseURL, CPAAPIKey: "proxy-key", CPAManagementKey: "management-key"}, db)
	for _, tc := range []struct {
		name, lite, body, wantContext string
	}{
		{name: "lite", lite: "true", body: `{"model":"gpt-6.1-sol","input":"OK"}`, wantContext: "all_turns"},
		{name: "ordinary", body: `{"model":"gpt-6.1-sol","input":"OK"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.body))
			r.Header.Set("Authorization", "Bearer "+key)
			if tc.lite != "" {
				r.Header.Set(responsesLiteHeader, tc.lite)
			}
			w := httptest.NewRecorder()
			a.routes().ServeHTTP(w, r)
			if w.Code != http.StatusOK || gotModel != "codex/gpt-6.1-sol" || gotContext != tc.wantContext {
				t.Fatalf("status=%d model=%q context=%q body=%s", w.Code, gotModel, gotContext, w.Body.String())
			}
		})
	}
}
