package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

const (
	clientID     = "local-test-client"
	clientSecret = "local-test-secret"
	accessToken  = "local-test-access-token"
)

type authorization struct {
	challenge string
	issued    time.Time
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		client := &http.Client{Timeout: 2 * time.Second}
		response, err := client.Get("http://127.0.0.1:8081/healthz")
		if err != nil || response.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		response.Body.Close()
		return
	}
	codes := struct {
		sync.Mutex
		values map[string]authorization
	}{values: make(map[string]authorization)}
	mux := http.NewServeMux()
	callbackHost := os.Getenv("LOCAL_CALLBACK_HOST")
	if callbackHost == "" {
		callbackHost = "localhost:18080"
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /oauth2/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		redirect, err := url.Parse(q.Get("redirect_uri"))
		if err != nil || redirect.Scheme != "http" || redirect.Host != callbackHost || redirect.Path != "/auth/callback" ||
			q.Get("client_id") != clientID || q.Get("response_type") != "code" || q.Get("state") == "" ||
			q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
			http.Error(w, "invalid local authorization request", http.StatusBadRequest)
			return
		}
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			http.Error(w, "authorization unavailable", http.StatusInternalServerError)
			return
		}
		code := hex.EncodeToString(random[:])
		codes.Lock()
		codes.values[code] = authorization{challenge: q.Get("code_challenge"), issued: time.Now()}
		codes.Unlock()
		callback := *redirect
		values := callback.Query()
		values.Set("code", code)
		values.Set("state", q.Get("state"))
		callback.RawQuery = values.Encode()
		http.Redirect(w, r, callback.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		id, secret, hasBasic := r.BasicAuth()
		if !hasBasic {
			id, secret = r.Form.Get("client_id"), r.Form.Get("client_secret")
		}
		code := r.Form.Get("code")
		codes.Lock()
		entry, found := codes.values[code]
		delete(codes.values, code)
		codes.Unlock()
		verifier := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(verifier[:])
		if id != clientID || secret != clientSecret || r.Form.Get("grant_type") != "authorization_code" ||
			!found || time.Since(entry.issued) > 10*time.Minute || challenge != entry.challenge {
			http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"` + accessToken + `","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("GET /api/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+accessToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sub":"local-admin","username":"local-admin","name":"Local Admin","avatar_template":"/user_avatar/linux.do/system/{size}/1_2.png","active":true,"trust_level":2}`))
	})
	mux.HandleFunc("GET /v8/management/credentials", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-management-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"files":[{"id":"local-first.json","auth_index":"local-first","name":"local-first.json","provider":"codex","source":"file","status":"active","priority":10},{"id":"local-second.json","auth_index":"local-second","name":"local-second.json","provider":"codex","source":"file","status":"active","priority":0}]}`))
	})
	mux.HandleFunc("GET /v8/management/config/api-keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-management-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"claude":[{"name":"Local AI provider","prefix":"local-ai","priority":5,"keys":[{"api-key":"mock-upstream-secret"}]}]}`))
	})
	mux.HandleFunc("GET /v8/management/credentials/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-management-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Query().Get("name") {
		case "local-first.json":
			_, _ = w.Write([]byte(`{"models":[{"id":"local-first/local-model"},{"id":"local-model"},{"id":"local-first/local-throttle-model"},{"id":"local-throttle-model"}]}`))
		case "local-second.json":
			_, _ = w.Write([]byte(`{"models":[{"id":"local-second/local-model"},{"id":"local-model"},{"id":"local-second/local-throttle-model"},{"id":"local-throttle-model"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("GET /v0/management/oauth-model-alias", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-management-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"oauth-model-alias":{"codex":[{"name":"local-model","alias":"openai/local-model"}]}}`))
	})
	mux.HandleFunc("POST /v8/management/credentials/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-management-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("POST /v0/management/api-call", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-management-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		var request struct {
			AuthIndex string `json:"auth_index"`
			Method    string `json:"method"`
			URL       string `json:"url"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if request.AuthIndex != "local-first" && request.AuthIndex != "local-second" {
			http.Error(w, "unknown account", 400)
			return
		}
		result := map[string]any{"status_code": 200}
		switch request.URL {
		case "https://chatgpt.com/backend-api/wham/usage":
			result["body"] = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":77,"reset_at":%q},"secondary_window":{"used_percent":12,"reset_at":%q}}}`,
				time.Now().Add(22*time.Minute).UTC().Format(time.RFC3339), time.Now().Add(6*24*time.Hour).UTC().Format(time.RFC3339))
		case "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits":
			result["body"] = fmt.Sprintf(`{"available_count":2,"applicable_available_count":2,"credits":[{"status":"available","reset_type":"codex_rate_limits","expires_at":%q},{"status":"available","reset_type":"codex_rate_limits","expires_at":%q}]}`,
				time.Now().Add(4*24*time.Hour).UTC().Format(time.RFC3339), time.Now().Add(11*24*time.Hour).UTC().Format(time.RFC3339))
		case "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume":
			result["body"] = `{"code":"reset"}`
		default:
			result["status_code"] = 404
			result["body"] = `{"error":"not found"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("POST /v8/management/routing/cooldown/reset", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-management-key" {
			http.Error(w, "unauthorized", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-proxy-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"local-first/local-model","object":"model"},{"id":"local-second/local-model","object":"model"},{"id":"local-ai/local-model","object":"model"},{"id":"local-first/local-throttle-model","object":"model"},{"id":"local-second/local-throttle-model","object":"model"}]}`))
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-cpa-proxy-key" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		label := ""
		switch body.Model {
		case "local-model":
			label = "first-account"
		case "local-first/local-model":
			label = "first-account"
		case "local-second/local-model":
			label = "second-account"
		case "local-first/local-throttle-model":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":{"code":"rate_limit_exceeded","message":"quota exhausted"}}`))
			return
		case "local-second/local-throttle-model":
			label = "second-account"
		case "local-ai/local-model":
			label = "ai-provider-account"
		default:
			http.Error(w, "unknown prefixed model", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if body.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", label)
			_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50}}\n\ndata: [DONE]\n\n"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "local-test", "object": "chat.completion", "created": 1, "model": body.Model,
			"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": label}, "finish_reason": "stop"}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 50, "total_tokens": 150},
		})
	})
	for _, account := range []string{"first", "second"} {
		account := account
		mux.HandleFunc("POST /"+account+"/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "local-test", "object": "chat.completion", "created": 1, "model": "local-model",
				"choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": account + "-account"}, "finish_reason": "stop"}},
			})
		})
		mux.HandleFunc("GET /"+account+"/v1/models", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"local-model","object":"model"}]}`)
		})
	}
	server := &http.Server{Addr: ":8081", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Print("local OAuth and upstream mock listening on :8081")
	log.Fatal(server.ListenAndServe())
}
