package companion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestOAuthTrustLevelGate(t *testing.T) {
	profile := `{"id":123,"username":"alice","active":true,"trust_level":2}`
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(profile))
	}))
	defer provider.Close()
	a := &app{cfg: settings{OAuthUserInfo: provider.URL, MinTrustLevel: 2}}
	token := &oauth2.Token{AccessToken: "test"}
	for _, tc := range []struct {
		profile string
		allowed bool
	}{
		{`{"id":123,"trust_level":1}`, false},
		{`{"id":123}`, false},
		{`{"id":123,"trust_level":"2"}`, false},
		{`{"id":123,"trust_level":2,"active":false}`, false},
		{`{"id":123,"trust_level":2,"active":true}`, true},
	} {
		profile = tc.profile
		_, err := a.fetchUser(context.Background(), token)
		if (err == nil) != tc.allowed {
			t.Errorf("profile %s: allowed=%t, error=%v", tc.profile, tc.allowed, err)
		}
	}
}

func TestLinuxDOAvatarURLAllowsKnownHostsOnly(t *testing.T) {
	for raw, want := range map[string]string{
		"/user_avatar/linux.do/alice/{size}/1_2.png": "https://linux.do/user_avatar/linux.do/alice/64/1_2.png",
		"https://cdn.ldstatic.com/avatar.png":        "https://cdn.ldstatic.com/avatar.png",
		"https://evil.example/avatar.png":            "",
		"javascript:alert(1)":                        "",
		"//evil.example/avatar.png":                  "",
	} {
		if got := linuxDOAvatarURL(raw); got != want {
			t.Errorf("avatar %q: got %q, want %q", raw, got, want)
		}
	}
}

func TestKeyRevocation(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "1"}); err != nil {
		t.Fatal(err)
	}
	key, err := db.createKeySecured("1", "test", testVaultKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := db.lookupKey(key); !ok {
		t.Fatal("new key rejected")
	}
	keys := db.keysForUser("1")
	if len(keys) != 1 || keys[0].Hash != "" {
		t.Fatalf("key list leaks hash: %#v", keys)
	}
	if err := db.revokeKey("1", keys[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.lookupKey(key); ok {
		t.Fatal("revoked key accepted")
	}
}

func TestOAuthLoginCreatesUserSession(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.FormValue("code") != "valid-code" || r.FormValue("code_verifier") == "" {
				http.Error(w, "invalid code", 400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"access_token":"test-access","token_type":"Bearer"}`))
		case "/user":
			if r.Header.Get("Authorization") != "Bearer test-access" {
				http.Error(w, "invalid token", 401)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":123,"username":"alice","name":"Alice","avatar_template":"/user_avatar/linux.do/alice/{size}/7_2.png","trust_level":2}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(settings{PublicURL: &url.URL{Scheme: "http", Host: "localhost:8080"}, ClientID: "client", ClientSecret: "secret",
		OAuthAuthorize: provider.URL + "/authorize", OAuthToken: provider.URL + "/token", OAuthUserInfo: provider.URL + "/user",
		CookieKey: strings.Repeat("a", 64)}, db)
	login := httptest.NewRecorder()
	a.routes().ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("login status %d", login.Code)
	}
	location, err := url.Parse(login.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if location.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("PKCE challenge missing")
	}
	state := location.Query().Get("state")
	if state == "" {
		t.Fatal("OAuth state missing")
	}
	callback := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+url.QueryEscape(state)+"&code=valid-code", nil)
	for _, cookie := range login.Result().Cookies() {
		callback.AddCookie(cookie)
	}
	result := httptest.NewRecorder()
	a.routes().ServeHTTP(result, callback)
	if result.Code != http.StatusSeeOther || result.Header().Get("Location") != "/keys" {
		t.Fatalf("callback status %d, location %q: %s", result.Code, result.Header().Get("Location"), result.Body.String())
	}
	user, ok := db.getUser("123")
	if !ok || user.Username != "alice" || user.AvatarURL != "https://linux.do/user_avatar/linux.do/alice/64/7_2.png" {
		t.Fatalf("OAuth user not saved: %#v", user)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range result.Result().Cookies() {
		if cookie.Name == "session" {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatal("session cookie missing")
	}
	dashboard := httptest.NewRequest(http.MethodGet, "/keys", nil)
	dashboard.AddCookie(sessionCookie)
	page := httptest.NewRecorder()
	a.routes().ServeHTTP(page, dashboard)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "alice") {
		t.Fatalf("dashboard status %d body %s", page.Code, page.Body.String())
	}
	root := httptest.NewRecorder()
	a.routes().ServeHTTP(root, httptest.NewRequest(http.MethodGet, "/", nil))
	if root.Code != http.StatusSeeOther || root.Header().Get("Location") != "/keys" {
		t.Fatalf("root should redirect to /keys, got %d %q", root.Code, root.Header().Get("Location"))
	}
}
