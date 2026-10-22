package companion

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAssignedKeysPageShowsPlanEditAfterRefresh(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v8/management/credentials":
			_, _ = w.Write([]byte(`{"files":[{"id":"first-id","name":"first.json","provider":"codex","source":"file"},{"id":"second-id","name":"second.json","provider":"codex","source":"file"}]}`))
		case "/v8/management/config/api-keys":
			_, _ = w.Write([]byte(`{}`))
		case "/v0/management/oauth-model-alias":
			_, _ = w.Write([]byte(`{"oauth-model-alias":{}}`))
		case "/v8/management/credentials/models":
			prefix := strings.TrimSuffix(r.URL.Query().Get("name"), ".json")
			_, _ = w.Write([]byte(`{"models":[{"id":"` + prefix + `/test-model"},{"id":"test-model"}]}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"first/test-model"},{"id":"second/test-model"},{"id":"test-model"}]}`))
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
	for _, id := range []string{"admin", "member"} {
		if err := db.upsertUser(user{ID: id, Username: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.savePlan(routingPlan{Name: "assigned", AccountIDs: []string{"first-id"}}); err != nil {
		t.Fatal(err)
	}
	planID := db.plans()[0].ID
	if err := assignPlanForTest(db, "member", planID, false); err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: baseURL, CPABaseURL: baseURL, CookieKey: testVaultKey,
		CPAAPIKey: "proxy-key", CPAManagementKey: "management-key", AdminSubjects: map[string]bool{"admin": true}}, db)
	request := func(userID, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: userID, CSRF: "valid", Expires: time.Now().Add(time.Hour).Unix()})})
		if method == http.MethodPost {
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			r.Header.Set("Accept", "application/json")
		}
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	page := request("member", http.MethodGet, "/keys", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-account-name="codex · first"`) {
		t.Fatal("first assigned account missing from keys page")
	}
	form := url.Values{"csrf": {"valid"}, "id": {planID}, "name": {"assigned"}, "account_id": {"second-id"}, "rpm": {"0"}, "reset_second-id": {"0"}}
	saved := request("admin", http.MethodPost, "/admin/plans", form.Encode())
	if saved.Code != http.StatusOK {
		t.Fatalf("plan edit failed: %d %s", saved.Code, saved.Body.String())
	}
	page = request("member", http.MethodGet, "/keys", "")
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), `data-account-name="codex · second"`) ||
		strings.Contains(page.Body.String(), `data-account-name="codex · first"`) {
		t.Fatal("keys page still shows the old assigned account after plan edit")
	}
}
