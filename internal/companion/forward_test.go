package companion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestForwardModeUsesOnlyAssignedPrefixThroughOriginalCPA(t *testing.T) {
	prefixes := map[string]string{"first.json": "first", "second.json": "second"}
	var gotAuth, gotModel, gotCallerKey string
	proxyCalls := 0
	downloadCalls := 0
	modelsAvailable := true
	aliasesAvailable := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			if r.Header.Get("Authorization") != "Bearer management-secret" {
				http.Error(w, "bad management key", 401)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"id": "first-id", "name": "first.json", "source": "file", "provider": "codex"},
				{"id": "second-id", "name": "second.json", "source": "file", "provider": "codex"},
			}})
		case "/v8/management/credentials/download":
			downloadCalls++
			http.Error(w, "credential download forbidden", http.StatusForbidden)
		case "/v8/management/config/api-keys":
			json.NewEncoder(w).Encode(map[string]any{})
		case "/v0/management/oauth-model-alias":
			if r.Header.Get("Authorization") != "Bearer management-secret" {
				http.Error(w, "bad management key", 401)
				return
			}
			if !aliasesAvailable {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"oauth-model-alias": map[string]any{}})
		case "/v8/management/credentials/models":
			if r.Header.Get("Authorization") != "Bearer management-secret" {
				http.Error(w, "bad management key", 401)
				return
			}
			if !modelsAvailable {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
				return
			}
			prefix := prefixes[r.URL.Query().Get("name")]
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{
				{"id": prefix + "/test-model"}, {"id": "test-model"},
				{"id": prefix + "/openai/gpt-test"}, {"id": "openai/gpt-test"},
			}})
		case "/v1/chat/completions":
			proxyCalls++
			w.Header().Set("X-CPA-TRACE-ID", "20261019000000-private-index-request")
			gotAuth = r.Header.Get("Authorization")
			gotCallerKey = r.URL.Query().Get("key") + r.Header.Get("X-API-Key") + r.Header.Get("X-Companion-Auth-ID")
			var body struct {
				Model string `json:"model"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			gotModel = body.Model
			json.NewEncoder(w).Encode(map[string]string{"model": body.Model})
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
				{"id": "first/test-model", "auth_index": "private-index"}, {"id": "second/test-model"}, {"id": "test-model"},
				{"id": "openai/gpt-test"}, {"id": "first/openai/gpt-test"}, {"id": "second/openai/gpt-test"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "first", AccountIDs: []string{"first-id"}}); err != nil {
		t.Fatal(err)
	}
	firstPlan := db.plans()[0].ID
	if err := assignPlanForTest(db, "user", firstPlan, false); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("user", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: &url.URL{Scheme: "http", Host: "localhost:8080"},
		CPABaseURL: u, CPAAPIKey: "proxy-secret", CPAManagementKey: "management-secret",
		ModelAliasPrefixes: map[string]bool{"openai": true}}, db)
	call := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("X-API-Key", "caller-secret")
		r.Header.Set("X-Companion-Auth-ID", "second-id")
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	first := call("/v1/chat/completions?key=caller-secret", `{"model":"test-model"}`)
	if first.Code != 200 || gotModel != "first/test-model" || gotAuth != "Bearer proxy-secret" || gotCallerKey != "" {
		t.Fatalf("first route status=%d model=%q auth=%q leaked=%q", first.Code, gotModel, gotAuth, gotCallerKey)
	}
	if first.Header().Get("X-CPA-TRACE-ID") != "" {
		t.Fatal("upstream credential index leaked in CPA trace response header")
	}
	modelRequest := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelRequest.Header.Set("Authorization", "Bearer "+key)
	models := httptest.NewRecorder()
	a.routes().ServeHTTP(models, modelRequest)
	if models.Code != http.StatusOK || !strings.Contains(models.Body.String(), `"id":"test-model"`) || strings.Contains(models.Body.String(), `first/test-model`) || strings.Contains(models.Body.String(), `second/test-model`) || strings.Contains(models.Body.String(), `openai/gpt-test`) || strings.Contains(models.Body.String(), `private-index`) {
		t.Fatalf("models were not filtered to the assigned account: status=%d body=%s", models.Code, models.Body.String())
	}
	alias := call("/v1/chat/completions", `{"model":"openai/gpt-test"}`)
	if alias.Code != http.StatusNotFound || proxyCalls != 1 {
		t.Fatalf("OAuth alias was exposed to clients: status=%d proxy_calls=%d", alias.Code, proxyCalls)
	}
	if err := db.savePlan(routingPlan{Name: "second", AccountIDs: []string{"second-id"}}); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "user", db.plans()[1].ID, false); err != nil {
		t.Fatal(err)
	}
	a.accountCache.invalidate()
	second := call("/v1/chat/completions", `{"model":"test-model"}`)
	if second.Code != 200 || gotModel != "second/test-model" {
		t.Fatalf("second route status=%d model=%q", second.Code, gotModel)
	}
	prefixes["first.json"] = "second"
	a.accountCache.invalidate()
	duplicate := call("/v1/chat/completions", `{"model":"test-model"}`)
	if duplicate.Code != http.StatusServiceUnavailable || proxyCalls != 2 {
		t.Fatalf("duplicate prefix did not fail closed: status=%d proxy_calls=%d", duplicate.Code, proxyCalls)
	}
	models = httptest.NewRecorder()
	a.routes().ServeHTTP(models, modelRequest)
	if models.Code != http.StatusServiceUnavailable {
		t.Fatalf("models available with duplicate prefixes: %d", models.Code)
	}
	prefixes["first.json"] = "first"
	a.cfg.CPAManagementKey = "wrong-key"
	a.accountCache.invalidate()
	unavailable := call("/v1/chat/completions", `{"model":"test-model"}`)
	if unavailable.Code != http.StatusBadGateway || proxyCalls != 2 {
		t.Fatalf("management outage did not fail closed: status=%d proxy_calls=%d", unavailable.Code, proxyCalls)
	}
	a.cfg.CPAManagementKey = "management-secret"
	modelsAvailable = false
	a.accountCache.invalidate()
	unavailable = call("/v1/chat/completions", `{"model":"test-model"}`)
	if unavailable.Code != http.StatusBadGateway || proxyCalls != 2 {
		t.Fatalf("per-file model outage did not fail closed: status=%d proxy_calls=%d", unavailable.Code, proxyCalls)
	}
	modelsAvailable = true
	aliasesAvailable = false
	a.accountCache.invalidate()
	unavailable = call("/v1/chat/completions", `{"model":"test-model"}`)
	if unavailable.Code != http.StatusBadGateway || proxyCalls != 2 {
		t.Fatalf("OAuth alias lookup outage did not fail closed: status=%d proxy_calls=%d", unavailable.Code, proxyCalls)
	}
	if downloadCalls != 0 {
		t.Fatalf("credential download was requested %d times", downloadCalls)
	}
}

func TestForwardModeRejectsUnsupportedOrMissingModel(t *testing.T) {
	for _, tc := range []struct{ route, body string }{
		{"/v1/chat/completions", `{}`},
		{"/v1/images/edits", `{"model":"image"}`},
		{"/v1/chat/completions", `{"model":"first/test"}`},
	} {
		r := httptest.NewRequest(http.MethodPost, tc.route, strings.NewReader(tc.body))
		if err := prepareForwardRequest(r, "first"); err == nil {
			t.Fatalf("accepted %s %s", tc.route, tc.body)
		}
	}
}

func TestForwardAccountsRejectsMissingAndAmbiguousPrefixes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fileModels []string
		global     []string
	}{
		{"missing", []string{"plain-model"}, []string{"plain-model"}},
		{"not-global", []string{"account/plain-model"}, []string{"plain-model"}},
		{"ambiguous", []string{"account/plain-model", "other/plain-model"}, []string{"account/plain-model", "other/plain-model"}},
		{"incomplete", []string{"account/plain-model", "plain-model", "other-model"}, []string{"account/plain-model", "plain-model", "other-model"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v8/management/credentials":
					json.NewEncoder(w).Encode(map[string]any{"files": []map[string]string{{"id": "account", "name": "account.json", "source": "file"}}})
				case "/v8/management/config/api-keys":
					json.NewEncoder(w).Encode(map[string]any{})
				case "/v8/management/credentials/models":
					models := make([]map[string]string, 0, len(tc.fileModels))
					for _, id := range tc.fileModels {
						models = append(models, map[string]string{"id": id})
					}
					json.NewEncoder(w).Encode(map[string]any{"models": models})
				case "/v0/management/oauth-model-alias":
					json.NewEncoder(w).Encode(map[string]any{"oauth-model-alias": map[string]any{}})
				case "/v1/models":
					models := make([]map[string]string, 0, len(tc.global))
					for _, id := range tc.global {
						models = append(models, map[string]string{"id": id})
					}
					json.NewEncoder(w).Encode(map[string]any{"data": models})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			u, _ := url.Parse(server.URL)
			a := &app{cfg: settings{CPABaseURL: u, CPAAPIKey: "api", CPAManagementKey: "management"}}
			accounts, err := a.forwardAccounts(t.Context())
			if err != nil || len(accounts) != 1 || !accounts[0].Disabled || accounts[0].Prefix != "" {
				t.Fatalf("expected disabled account with no prefix, got accounts=%+v err=%v", accounts, err)
			}
		})
	}
}

func TestMatchingAccountPrefixesHandlesSlashInNativeModel(t *testing.T) {
	models := map[string]bool{
		"gpt-test": true, "openai/gpt-test": true,
		"account/gpt-test": true, "account/openai/gpt-test": true,
	}
	got := matchingAccountPrefixes(models, map[string]bool{"openai": true})
	if len(got) != 1 || !got["account"] {
		t.Fatalf("expected only account prefix, got %#v", got)
	}
}

func TestMatchingAccountPrefixesExcludesConfiguredAlias(t *testing.T) {
	models := map[string]bool{"model": true, "openai/model": true}
	if got := matchingAccountPrefixes(models, map[string]bool{"openai": true}); len(got) != 0 {
		t.Fatalf("OAuth alias treated as account prefix: %#v", got)
	}
	if got := matchingAccountPrefixes(models, nil); len(got) != 1 || !got["openai"] {
		t.Fatalf("test fixture no longer exercises alias exclusion: %#v", got)
	}
}

func TestForwardAccountsIncludesProviderKeysAndPriorities(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"id": "oauth", "name": "oauth.json", "source": "file", "provider": "codex", "priority": 5},
			}})
		case "/v8/management/credentials/models":
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"id": "oauth/model"}, {"id": "model"}}})
		case "/v8/management/config/api-keys":
			json.NewEncoder(w).Encode(map[string]any{"claude": []map[string]any{{
				"name": "Claude group", "prefix": "claude-a", "priority": 7,
				"keys": []map[string]any{
					{"api-key": "must-never-be-shown", "auth_index": "claude-a-index"},
					{"api-key": "also-secret", "prefix": "claude-b", "priority": 0},
				},
			}}})
		case "/v0/management/oauth-model-alias":
			json.NewEncoder(w).Encode(map[string]any{"oauth-model-alias": map[string]any{}})
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{
				{"id": "oauth/model"}, {"id": "claude-a/model"}, {"id": "claude-b/model"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	a := &app{cfg: settings{CPABaseURL: u, CPAAPIKey: "api", CPAManagementKey: "management"}}
	accounts, err := a.forwardAccounts(t.Context())
	if err != nil || len(accounts) != 3 {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	if accounts[0].Provider != "claude" || accounts[0].AuthIndex != "claude-a-index" || accounts[0].PriorityText() != "7" || accounts[0].Disabled {
		t.Fatalf("first account=%+v", accounts[0])
	}
	if accounts[1].Provider != "codex" || accounts[1].PriorityText() != "5" {
		t.Fatalf("second account=%+v", accounts[1])
	}
	if accounts[2].ID != "config:claude:claude-b" || accounts[2].Prefix != "claude-b" || accounts[2].PriorityText() != "0" || accounts[2].Disabled {
		t.Fatalf("third account=%+v", accounts[2])
	}
	for _, account := range accounts {
		if strings.Contains(account.Name+account.Label+account.ID, "secret") {
			t.Fatal("provider API key leaked into account metadata")
		}
	}
}

func TestForwardAccountsExcludesUpstreamDisabledAccounts(t *testing.T) {
	modelLookups := map[string]int{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
				{"id": "active", "name": "active.json", "source": "file", "provider": "codex"},
				{"id": "disabled-flag", "name": "disabled-flag.json", "source": "file", "provider": "codex", "disabled": true},
				{"id": "disabled-status", "name": "disabled-status.json", "source": "file", "provider": "codex", "status": "disabled"},
			}})
		case "/v8/management/credentials/models":
			name := r.URL.Query().Get("name")
			modelLookups[name]++
			json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"id": "active/model"}, {"id": "model"}}})
		case "/v8/management/config/api-keys":
			json.NewEncoder(w).Encode(map[string]any{"openai-compatibility": []map[string]any{{
				"name": "disabled provider", "prefix": "disabled-provider", "disabled": true,
				"keys": []map[string]any{{"auth_index": "disabled-provider-index"}},
			}}})
		case "/v0/management/oauth-model-alias":
			json.NewEncoder(w).Encode(map[string]any{"oauth-model-alias": map[string]any{}})
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "active/model"}, {"id": "disabled-provider/model"}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	a := &app{cfg: settings{CPABaseURL: u, CPAAPIKey: "api", CPAManagementKey: "management"}}
	accounts, err := a.forwardAccounts(t.Context())
	if err != nil || len(accounts) != 1 || accounts[0].ID != "active" || accounts[0].Disabled {
		t.Fatalf("accounts=%+v err=%v", accounts, err)
	}
	if len(modelLookups) != 1 || modelLookups["active.json"] != 1 {
		t.Fatalf("disabled credentials triggered model lookups: %+v", modelLookups)
	}
}
