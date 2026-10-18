package companion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeyCopyAndRotation(t *testing.T) {
	db, err := openStore(filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		if err := db.upsertUser(user{ID: id, Username: id}); err != nil {
			t.Fatal(err)
		}
	}
	vaultKey := strings.Repeat("a", 64)
	old, err := db.createKeySecured("alice", "client", vaultKey)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(db.path)
	if err != nil || strings.Contains(string(stored), old) {
		t.Fatalf("plaintext key written to store: %v", err)
	}
	keys := db.keysForUser("alice")
	if len(keys) != 1 || keys[0].Hash != "" || keys[0].Sealed != "" {
		t.Fatalf("key list should omit secret material: %#v", keys)
	}
	keyID := keys[0].ID
	a := newApp(settings{PublicURL: &url.URL{Scheme: "http", Host: "localhost:8080"}, CookieKey: vaultKey}, db)
	request := func(userID, path, csrf string) *httptest.ResponseRecorder {
		body := url.Values{"id": {keyID}, "csrf": {csrf}}.Encode()
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: userID, CSRF: "valid", Expires: time.Now().Add(time.Hour).Unix()})})
		w := httptest.NewRecorder()
		a.routes().ServeHTTP(w, r)
		return w
	}
	if got := request("alice", "/keys/copy", "wrong").Code; got != http.StatusForbidden {
		t.Fatalf("wrong CSRF status: %d", got)
	}
	if got := request("bob", "/keys/copy", "valid").Code; got != http.StatusNotFound {
		t.Fatalf("other user's key copy status: %d", got)
	}
	copyResult := request("alice", "/keys/copy", "valid")
	if copyResult.Code != http.StatusOK || copyResult.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("copy response: %d, cache: %q", copyResult.Code, copyResult.Header().Get("Cache-Control"))
	}
	var result struct{ Key string }
	if err := json.Unmarshal(copyResult.Body.Bytes(), &result); err != nil || result.Key != old {
		t.Fatalf("copied key mismatch: %v", err)
	}
	if got := request("bob", "/keys/rotate", "valid").Code; got != http.StatusNotFound {
		t.Fatalf("other user's key rotation status: %d", got)
	}
	rotated := request("alice", "/keys/rotate", "valid")
	if rotated.Code != http.StatusSeeOther || rotated.Header().Get("Location") != "/keys" {
		t.Fatalf("rotation status: %d", rotated.Code)
	}
	var noticeCookie *http.Cookie
	for _, cookie := range rotated.Result().Cookies() {
		if cookie.Name == "key_notice" {
			noticeCookie = cookie
		}
	}
	if noticeCookie == nil || noticeCookie.Value != "rotated" || !noticeCookie.HttpOnly {
		t.Fatal("rotation notice cookie missing")
	}
	pageRequest := httptest.NewRequest(http.MethodGet, "/keys", nil)
	pageRequest.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: "alice", CSRF: "valid", Expires: time.Now().Add(time.Hour).Unix()})})
	pageRequest.AddCookie(noticeCookie)
	page := httptest.NewRecorder()
	a.routes().ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "密钥轮换成功") || strings.Contains(page.Body.String(), old) {
		t.Fatal("rotation should show a success toast without exposing the old key")
	}
	if !strings.Contains(page.Header().Get("Set-Cookie"), "key_notice=") {
		t.Fatal("rotation notice was not consumed")
	}
	pageRequest = httptest.NewRequest(http.MethodGet, "/keys", nil)
	pageRequest.AddCookie(&http.Cookie{Name: "session", Value: a.sign(signedPayload{UserID: "alice", CSRF: "valid", Expires: time.Now().Add(time.Hour).Unix()})})
	pageRequest.AddCookie(&http.Cookie{Name: "key_notice", Value: "created"})
	page = httptest.NewRecorder()
	a.routes().ServeHTTP(page, pageRequest)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "API Key 创建成功") || strings.Contains(page.Body.String(), "new-api-key") {
		t.Fatal("creation should show a toast instead of the old key banner")
	}
	if _, ok := db.lookupKey(old); ok {
		t.Fatal("old key remains valid after rotation")
	}
	copyResult = request("alice", "/keys/copy", "valid")
	if err := json.Unmarshal(copyResult.Body.Bytes(), &result); err != nil || result.Key == old || result.Key == "" {
		t.Fatalf("rotated key copy failed: %v", err)
	}
	if _, ok := db.lookupKey(result.Key); !ok {
		t.Fatal("rotated key is not valid")
	}
	reopened, err := openStore(db.path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := reopened.sealedKeyForUser("alice", keyID)
	if err != nil {
		t.Fatal(err)
	}
	if restored, err := openAPIKey(vaultKey, "alice", keyID, sealed); err != nil || restored != result.Key {
		t.Fatalf("persisted rotated key cannot be opened: %v", err)
	}
	if _, err := openAPIKey(vaultKey, "bob", keyID, sealed); err == nil {
		t.Fatal("ciphertext opened for another user")
	}
}

func TestStoreRequiresEncryptedKeyValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.json")
	db, err := openStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.createKeySecured("alice", "client", strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	db.db.Keys[0].Sealed = ""
	if err := db.persistLocked(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := openStore(path); err == nil || !strings.Contains(err.Error(), "has no encrypted value") {
		t.Fatalf("store accepted a key without an encrypted value: %v", err)
	}
}
