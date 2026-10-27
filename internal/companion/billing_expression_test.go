package companion

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBillingExpressionTierAndUnsupportedFields(t *testing.T) {
	expr := `len <= 272000 ? tier("0_272k", p * 2 + cr * 0.1 + cc * 2.5 + c * 10) : tier("272k_plus", p * 4 + cr * 0.2 + cc * 5 + c * 15)`
	if err := validatePriceExpression(expr); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		usage tokenUsage
		want  int64
	}{
		{tokenUsage{Input: 272000, Output: 100000}, 1_544_000_000_000},
		{tokenUsage{Input: 272001, Output: 100000}, 2_588_004_000_000},
		{tokenUsage{Input: 100000, Output: 100000, CacheRead: 20000, CacheWrite: 10000}, 1_167_000_000_000},
	} {
		got, err := (modelPrice{Expression: expr}).charge(tc.usage)
		if err != nil || got != tc.want {
			t.Fatalf("charge(%+v) = %d, %v; want %d", tc.usage, got, err, tc.want)
		}
	}
	for _, invalid := range []string{`tier("x", ai * 1)`, `tier("x", param("enable_thinking"))`, `tier("x", p * -1)`, `tier("x", p / 0)`, `exec("anything")`} {
		if err := validatePriceExpression(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestIgnoreUnsupportedPriceFieldsKeepsMeasuredTerms(t *testing.T) {
	expr := `len <= 272000 ? tier("0_272k", p * 2 + cr * 0.1 + cc * 2.5 + cc12h * 10 + c * 10) : tier("272k_plus", p * 4 + cr * 0.2 + cc * 5 + cc12h * 20 + c * 15)`
	adjusted, changed := ignoreUnsupportedPriceFields(expr)
	if !changed || strings.Contains(adjusted, "cc12h") || !strings.Contains(adjusted, "p * 2") || !strings.Contains(adjusted, "c * 15") {
		t.Fatalf("unsupported field was not isolated: changed=%t expression=%q", changed, adjusted)
	}
	if err := validatePriceExpression(adjusted); err != nil {
		t.Fatalf("adjusted expression is not billable: %v", err)
	}
}

func TestPriceListSummarizesTieredRates(t *testing.T) {
	view := modelPriceView{Model: "tiered-model", Configured: true, Price: modelPrice{
		Expression: `len <= 272000 ? tier("0_272k", p * 2 + cr * 0.1 + cc * 2.5 + c * 10) : tier("272k_plus", p * 4 + cr * 0.2 + cc * 5 + c * 15)`,
		Source:     "basellm",
	}}
	row := displayPrice(view)
	if row.Prompt != "$2.0000–$4.0000/1M" || row.Completion != "$10.0000–$15.0000/1M" ||
		row.CacheRead != "$0.1000–$0.2000/1M" || row.CacheWrite != "$2.5000–$5.0000/1M" ||
		row.Rule != "分段 · 2 档" || row.Source != "BaseLLM" {
		t.Fatalf("tiered price row: %+v", row)
	}
}

func TestPricePageReadOnlyAndSync(t *testing.T) {
	var cpaUnavailable atomic.Bool
	cpa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cpaUnavailable.Load() {
			http.Error(w, "CPA unavailable", http.StatusServiceUnavailable)
			return
		}
		switch r.URL.Path {
		case "/v8/management/credentials":
			w.Write([]byte(`{"files":[{"id":"one","name":"one.json","source":"file","provider":"codex"}]}`))
		case "/v8/management/credentials/models":
			w.Write([]byte(`{"models":[{"id":"gpt-5"},{"id":"one/gpt-5"},{"id":"openai/gpt-5"},{"id":"one/openai/gpt-5"}]}`))
		case "/v8/management/config/api-keys":
			w.Write([]byte(`{}`))
		case "/v0/management/oauth-model-alias":
			w.Write([]byte(`{"oauth-model-alias":{"codex":[{"name":"gpt-5","alias":"openai/gpt-5"}]}}`))
		case "/v1/models":
			w.Write([]byte(`{"data":[{"id":"gpt-5"},{"id":"one/gpt-5"},{"id":"openai/gpt-5"},{"id":"one/openai/gpt-5"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer cpa.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"billing_expr":{"gpt-5":"tier(\"standard\", p * 2 + c * 10)","openai/gpt-5":"tier(\"standard\", p * 5)","unrelated":"tier(\"standard\", p * 1)"}}}`))
	}))
	defer source.Close()
	oldSource := priceFetchURL
	priceFetchURL = source.URL
	defer func() { priceFetchURL = oldSource }()
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"admin", "member"} {
		if err := db.upsertUser(user{ID: id, Username: id}); err != nil {
			t.Fatal(err)
		}
	}
	base, _ := url.Parse(cpa.URL)
	a := newApp(settings{PublicURL: base, CPABaseURL: base, CPAAPIKey: "api", CPAManagementKey: "secret", CookieKey: strings.Repeat("a", 64), AdminSubjects: map[string]bool{"admin": true}}, db)
	request := func(id, method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: id, CSRF: "token", Expires: time.Now().Add(time.Hour).Unix()})})
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Accept", "application/json")
		}
		return r
	}
	memberPage := httptest.NewRecorder()
	a.routes().ServeHTTP(memberPage, request("member", http.MethodGet, "/prices", ""))
	if memberPage.Code != 200 || !strings.Contains(memberPage.Body.String(), "gpt-5") ||
		strings.Contains(memberPage.Body.String(), "openai/gpt-5") ||
		!strings.Contains(memberPage.Body.String(), "data-view-price") || !strings.Contains(memberPage.Body.String(), "readonly") ||
		strings.Contains(memberPage.Body.String(), "一键同步价格") || strings.Contains(memberPage.Body.String(), "保存价格") {
		t.Fatalf("member page: %d %s", memberPage.Code, memberPage.Body.String())
	}
	denied := httptest.NewRecorder()
	a.routes().ServeHTTP(denied, request("member", http.MethodPost, "/admin/prices/sync", "csrf=token"))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("member sync status %d", denied.Code)
	}
	synced := httptest.NewRecorder()
	a.routes().ServeHTTP(synced, request("admin", http.MethodPost, "/admin/prices/sync", "csrf=token"))
	if synced.Code != 200 || !strings.Contains(synced.Body.String(), `"synced":1`) {
		t.Fatalf("sync: %d %s", synced.Code, synced.Body.String())
	}
	price, ok := db.priceFor("gpt-5")
	if !ok || price.Expression != `tier("standard", p * 2 + c * 10)` || price.Source != "basellm" {
		t.Fatalf("saved price: %+v, %v", price, ok)
	}
	if _, ok := db.priceFor("openai/gpt-5"); ok {
		t.Fatal("OAuth alias was saved as a separate price")
	}
	adminPage := httptest.NewRecorder()
	a.routes().ServeHTTP(adminPage, request("admin", http.MethodGet, "/prices", ""))
	if adminPage.Code != 200 || !strings.Contains(adminPage.Body.String(), "一键同步价格") || !strings.Contains(adminPage.Body.String(), "保存价格") {
		t.Fatalf("admin page: %d", adminPage.Code)
	}
	cpaUnavailable.Store(true)
	a.accountCache.invalidate()
	memberFallback := httptest.NewRecorder()
	a.routes().ServeHTTP(memberFallback, request("member", http.MethodGet, "/prices", ""))
	if memberFallback.Code != http.StatusOK || !strings.Contains(memberFallback.Body.String(), "gpt-5") ||
		!strings.Contains(memberFallback.Body.String(), "暂时无法刷新 CPA 模型列表") ||
		strings.Contains(memberFallback.Body.String(), "一键同步价格") {
		t.Fatalf("member stale-price fallback: %d %s", memberFallback.Code, memberFallback.Body.String())
	}
	cold := newApp(a.cfg, db)
	memberSaved := httptest.NewRecorder()
	cold.routes().ServeHTTP(memberSaved, request("member", http.MethodGet, "/prices", ""))
	if memberSaved.Code != http.StatusOK || !strings.Contains(memberSaved.Body.String(), "gpt-5") ||
		!strings.Contains(memberSaved.Body.String(), "暂时无法刷新 CPA 模型列表") {
		t.Fatalf("member saved-price fallback: %d %s", memberSaved.Code, memberSaved.Body.String())
	}
}
