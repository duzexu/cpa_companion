package companion

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

type app struct {
	cfg           settings
	db            *store
	logs          *usageLogStore
	oauth         oauth2.Config
	proxy         *httputil.ReverseProxy
	affinity      *routeAffinity
	auditSessions auditSessions
	planRPM       planRateLimiter
	accountCache  accountCache
	resetGate     sync.Mutex
}

type signedPayload struct {
	UserID   string `json:"user_id,omitempty"`
	CSRF     string `json:"csrf,omitempty"`
	State    string `json:"state,omitempty"`
	Verifier string `json:"verifier,omitempty"`
	Session  int    `json:"session,omitempty"`
	Expires  int64  `json:"expires"`
}

var errTrustLevelTooLow = errors.New("Linux DO trust level is below the required minimum or unavailable")

type credential struct {
	ID               string          `json:"id"`
	AuthIndex        string          `json:"auth_index"`
	Name             string          `json:"name"`
	Label            string          `json:"-"`
	Provider         string          `json:"provider"`
	Prefix           string          `json:"prefix"`
	Priority         *int            `json:"priority"`
	Source           string          `json:"source"`
	Email            string          `json:"email"`
	ChatGPTAccountID string          `json:"chatgpt_account_id"`
	Status           string          `json:"status"`
	Disabled         bool            `json:"disabled"`
	FileBacked       bool            `json:"file_backed"`
	Models           map[string]bool `json:"-"`
	ResetAvailable   int             `json:"-"`
}

func (c credential) PriorityText() string {
	if c.Priority == nil {
		return "未设置"
	}
	return fmt.Sprint(*c.Priority)
}

func newApp(cfg settings, db *store) *app {
	a := &app{cfg: cfg, db: db, affinity: newRouteAffinity(time.Hour), accountCache: accountCache{ttl: cfg.AccountCacheTTL}}
	a.oauth = oauth2.Config{ClientID: cfg.ClientID, ClientSecret: cfg.ClientSecret,
		RedirectURL: cfg.PublicURL.String() + "/auth/callback", Scopes: []string{"openid", "profile", "email"},
		Endpoint: oauth2.Endpoint{AuthURL: cfg.OAuthAuthorize, TokenURL: cfg.OAuthToken}}
	a.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(cfg.CPABaseURL)
			pr.Out.Header.Del("X-Companion-Auth-ID")
			pr.Out.Header.Del("X-Companion-Internal-Key")
			pr.Out.Header.Del("X-Management-Key")
			pr.Out.Header.Del("X-CPA-Auth-Index")
			pr.Out.Header.Del("X-Auth-Index")
			pr.Out.Header.Del("X-API-Key")
			pr.Out.Header.Del("X-Goog-API-Key")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("Authorization", "Bearer "+cfg.CPAAPIKey)
			query := pr.Out.URL.Query()
			query.Del("key")
			query.Del("api_key")
			query.Del("auth_index")
			query.Del("auth_id")
			pr.Out.URL.RawQuery = query.Encode()
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("CPA proxy error: %v", err)
			http.Error(w, "upstream proxy unavailable", http.StatusBadGateway)
		},
	}
	return a
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /login", a.loginPage)
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/keys", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /keys", a.dashboard)
	mux.HandleFunc("GET /dashboard", a.usageDashboard)
	mux.HandleFunc("GET /logs", a.usageLogsPage)
	mux.HandleFunc("POST /keys", a.createKey)
	mux.HandleFunc("POST /keys/copy", a.copyKey)
	mux.HandleFunc("POST /keys/rotate", a.rotateKey)
	mux.HandleFunc("POST /keys/revoke", a.revokeKey)
	mux.HandleFunc("GET /admin", a.admin)
	mux.HandleFunc("GET /audit", a.auditPage)
	mux.HandleFunc("GET /prices", a.pricesPage)
	mux.HandleFunc("POST /admin/assign", a.assign)
	mux.HandleFunc("POST /admin/plans", a.savePlan)
	mux.HandleFunc("POST /admin/prices", a.savePrice)
	mux.HandleFunc("POST /admin/audit", a.saveAuditSettings)
	mux.HandleFunc("POST /admin/prices/sync", a.syncPrices)
	mux.HandleFunc("POST /admin/plans/delete", a.deletePlan)
	mux.HandleFunc("POST /quota/reset", a.resetQuota)
	mux.HandleFunc("POST /admin/refresh", a.refreshSource)
	mux.HandleFunc("/v1/", a.proxyRequest)
	mux.HandleFunc("/v1beta/", a.proxyRequest)
	mux.HandleFunc("/v1alpha/", a.proxyRequest)
	return withSecurity(mux)
}

func (a *app) sign(p signedPayload) string {
	b, _ := json.Marshal(p)
	message := base64.RawURLEncoding.EncodeToString(b)
	mac := hmac.New(sha256.New, []byte(a.cfg.CookieKey))
	mac.Write([]byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *app) verify(value string) (signedPayload, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return signedPayload{}, false
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.CookieKey))
	mac.Write([]byte(parts[0]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		return signedPayload{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return signedPayload{}, false
	}
	var p signedPayload
	if json.Unmarshal(b, &p) != nil || p.Expires < time.Now().Unix() {
		return signedPayload{}, false
	}
	return p, true
}

func (a *app) setCookie(w http.ResponseWriter, name string, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: age,
		Secure: a.cfg.CookieSecure, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	state, err := randomHex(24)
	if err != nil {
		http.Error(w, "login unavailable", 500)
		return
	}
	verifier := oauth2.GenerateVerifier()
	a.setCookie(w, "oauth_state", a.sign(signedPayload{State: state, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute).Unix()}), 600)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (a *app) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.session(r); ok {
		http.Redirect(w, r, "/keys", http.StatusSeeOther)
		return
	}
	render(w, loginTemplate, nil)
}

func (a *app) callback(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("oauth_state")
	if err != nil {
		http.Error(w, "login state missing", 400)
		return
	}
	p, ok := a.verify(cookie.Value)
	a.setCookie(w, "oauth_state", "", -1)
	if !ok || p.State == "" || subtle.ConstantTimeCompare([]byte(p.State), []byte(r.URL.Query().Get("state"))) != 1 {
		http.Error(w, "invalid login state", 400)
		return
	}
	if r.URL.Query().Get("error") != "" {
		http.Error(w, "login denied", 400)
		return
	}
	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(p.Verifier))
	if err != nil {
		log.Printf("OAuth exchange failed: %v", err)
		http.Error(w, "login failed", 502)
		return
	}
	userInfo, err := a.fetchUser(r.Context(), token)
	if err != nil {
		if errors.Is(err, errTrustLevelTooLow) {
			http.Error(w, "Linux DO trust level is below the required minimum", http.StatusForbidden)
			return
		}
		log.Printf("OAuth userinfo failed: %v", err)
		http.Error(w, "login failed", 502)
		return
	}
	if err := a.db.upsertUser(userInfo); err != nil {
		http.Error(w, "account storage failed", 500)
		return
	}
	csrf, err := randomHex(24)
	if err != nil {
		http.Error(w, "login failed", 500)
		return
	}
	stored, _ := a.db.getUser(userInfo.ID)
	a.setCookie(w, "session", a.sign(signedPayload{UserID: userInfo.ID, CSRF: csrf, Session: stored.SessionVersion, Expires: time.Now().Add(7 * 24 * time.Hour).Unix()}), 7*24*3600)
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (a *app) fetchUser(ctx context.Context, token *oauth2.Token) (user, error) {
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(token))
	client.Timeout = 10 * time.Second
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.OAuthUserInfo, nil)
	if err != nil {
		return user{}, err
	}
	resp, err := client.Do(request)
	if err != nil {
		return user{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return user{}, fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var data map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&data); err != nil {
		return user{}, err
	}
	if rawActive, ok := data["active"]; ok {
		var active bool
		if json.Unmarshal(rawActive, &active) != nil || !active {
			return user{}, errors.New("Linux DO account is inactive")
		}
	}
	var trustLevel int
	minimum := a.cfg.MinTrustLevel
	if minimum < 1 {
		minimum = 2
	}
	if err := json.Unmarshal(data["trust_level"], &trustLevel); err != nil || trustLevel < minimum || trustLevel > 4 {
		return user{}, errTrustLevelTooLow
	}
	read := func(field string) string {
		var s string
		if json.Unmarshal(data[field], &s) == nil {
			return s
		}
		var n json.Number
		if json.Unmarshal(data[field], &n) == nil {
			return n.String()
		}
		return ""
	}
	id := read("sub")
	if id == "" {
		id = read("id")
	}
	if id == "" {
		return user{}, errors.New("userinfo has no stable subject")
	}
	username := read("username")
	if username == "" {
		username = read("login")
	}
	if username == "" {
		username = id
	}
	avatar := read("avatar_url")
	if avatar == "" {
		avatar = read("avatar_template")
	}
	if avatar == "" {
		avatar = read("picture")
	}
	return user{ID: id, Username: username, Name: read("name"), AvatarURL: linuxDOAvatarURL(avatar)}, nil
}

func linuxDOAvatarURL(raw string) string {
	raw = strings.ReplaceAll(strings.TrimSpace(raw), "{size}", "64")
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, "//") {
		raw = "https://linux.do" + raw
	} else if strings.HasPrefix(raw, "//") {
		raw = "https:" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "linux.do" && !strings.HasSuffix(host, ".linux.do") && host != "cdn.ldstatic.com" && host != "avatars.discourse-cdn.com" {
		return ""
	}
	return u.String()
}

func (a *app) session(r *http.Request) (user, signedPayload, bool) {
	c, err := r.Cookie("session")
	if err != nil {
		return user{}, signedPayload{}, false
	}
	p, ok := a.verify(c.Value)
	if !ok || p.UserID == "" {
		return user{}, signedPayload{}, false
	}
	u, ok := a.db.getUser(p.UserID)
	return u, p, ok && !u.Disabled && p.Session == u.SessionVersion
}

func (a *app) requireSession(w http.ResponseWriter, r *http.Request) (user, signedPayload, bool) {
	u, p, ok := a.session(r)
	if !ok {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return user{}, signedPayload{}, false
	}
	return u, p, true
}

func checkCSRF(r *http.Request, p signedPayload) bool {
	if r.ParseForm() != nil {
		return false
	}
	return p.CSRF != "" && subtle.ConstantTimeCompare([]byte(p.CSRF), []byte(r.PostForm.Get("csrf"))) == 1
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	_, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	// Bumping the version also revokes copies of this cookie held elsewhere.
	if err := a.db.revokeSessions(p.UserID); err != nil {
		http.Error(w, "logout failed", 500)
		return
	}
	a.setCookie(w, "session", "", -1)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	var notice string
	if c, err := r.Cookie("key_notice"); err == nil {
		a.setCookie(w, "key_notice", "", -1)
		switch c.Value {
		case "created":
			notice = "API Key 创建成功，请在下方列表中复制密钥。"
		case "rotated":
			notice = "密钥轮换成功，旧密钥已失效。请复制新密钥并更新客户端。"
		}
	}
	a.renderDashboard(w, r, u, p, notice)
}

type dashboardView struct {
	User       user
	Keys       []apiKey
	CSRF       string
	Admin      bool
	KeyNotice  string
	BaseURL    string
	Endpoints  []apiEndpoint
	Prefix     string
	Plan       routingPlan
	HasPlan    bool
	PlanActive bool
	Accounts   []quotaView
}

func (a *app) renderDashboard(w http.ResponseWriter, r *http.Request, u user, p signedPayload, notice string) {
	w.Header().Set("Cache-Control", "no-store")
	view := dashboardView{
		User: u, Keys: a.db.keysForUser(u.ID), CSRF: p.CSRF,
		Admin: a.cfg.AdminSubjects[u.ID], KeyNotice: notice, BaseURL: a.cfg.PublicURL.String() + "/v1", Endpoints: apiEndpoints,
	}
	if plan, exists := a.db.getPlan(u.PlanID); exists {
		view.Plan, view.HasPlan, view.PlanActive = plan, true, plan.activeAt(time.Now()) && len(plan.AccountIDs) > 0 && plan.usageAllowed()
		if len(plan.AccountIDs) > 0 {
			accounts, err := a.credentials(r.Context())
			if err != nil {
				log.Printf("resolve assigned plan accounts failed: %v", err)
			} else {
				view.Accounts = a.quotaViews(r.Context(), u, plan, accounts)
			}
		}
	}
	render(w, dashboardPlanTemplate, view, a.cfg.GitHubURL)
}

func (a *app) createKey(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	if _, ok := a.db.activePlanForUser(u.ID, time.Now()); !ok {
		http.Error(w, "routing plan is not active", 403)
		return
	}
	if len(a.db.keysForUser(u.ID)) >= 50 {
		http.Error(w, "API key limit reached", http.StatusTooManyRequests)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" || len(name) > 80 {
		http.Error(w, "invalid key name", 400)
		return
	}
	_, err := a.db.createKeySecured(u.ID, name, a.cfg.CookieKey)
	if err != nil {
		http.Error(w, "could not create key", 500)
		return
	}
	a.setCookie(w, "key_notice", "created", 30)
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (a *app) copyKey(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	keyID := r.PostForm.Get("id")
	sealed, err := a.db.sealedKeyForUser(u.ID, keyID)
	if err != nil {
		http.Error(w, "key is unavailable; rotate it first", http.StatusNotFound)
		return
	}
	plain, err := openAPIKey(a.cfg.CookieKey, u.ID, keyID, sealed)
	if err != nil {
		http.Error(w, "could not open key", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Key string `json:"key"`
	}{Key: plain})
}

func (a *app) rotateKey(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	_, err := a.db.rotateKey(u.ID, r.PostForm.Get("id"), a.cfg.CookieKey)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "key not found", http.StatusNotFound)
			return
		}
		http.Error(w, "could not rotate key", http.StatusInternalServerError)
		return
	}
	a.setCookie(w, "key_notice", "rotated", 30)
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (a *app) revokeKey(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	if err := a.db.revokeKey(u.ID, r.PostForm.Get("id")); err != nil {
		http.Error(w, "key not found", 404)
		return
	}
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (a *app) credentials(ctx context.Context) ([]credential, error) {
	return a.forwardAccounts(ctx)
}

func (a *app) admin(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", 403)
		return
	}
	accounts, err := a.freshAccounts(r.Context())
	if err != nil {
		log.Printf("list accounts failed: %v", err)
		http.Error(w, "could not list accounts", 502)
		return
	}
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for i := range accounts {
		if accounts[i].Disabled || accounts[i].AuthIndex == "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			accounts[i].ResetAvailable, _ = a.availableResetCount(ctx, accounts[i])
		}(i)
	}
	wg.Wait()
	users := a.db.users()
	plans := a.db.plans()
	for i := range accounts {
		providerAvailable := accounts[i].ResetAvailable
		accounts[i].ResetAvailable = a.db.resetAllocationMax(accounts[i].ID, "", providerAvailable, time.Now())
		for j := range plans {
			if plans[j].ResetCaps == nil {
				plans[j].ResetCaps = map[string]int{}
			}
			plans[j].ResetCaps[accounts[i].ID] = a.db.resetAllocationMax(accounts[i].ID, plans[j].ID, providerAvailable, time.Now())
		}
	}
	owners := make(map[string][]user, len(users))
	for _, member := range users {
		if member.PlanID != "" {
			owners[member.PlanID] = append(owners[member.PlanID], member)
		}
	}
	for i := range plans {
		if assigned := owners[plans[i].ID]; len(assigned) == 1 {
			plans[i].OwnerID, plans[i].OwnerName = assigned[0].ID, assigned[0].Username
		} else if len(assigned) > 1 {
			plans[i].OwnerConflict = true
			names := make([]string, len(assigned))
			for j, member := range assigned {
				names[j] = member.Username
			}
			plans[i].OwnerName = "冲突：" + strings.Join(names, "、")
		}
	}
	for i := range plans {
		for j := i + 1; j < len(plans); j++ {
			if futurePlanWindowsOverlap(plans[i], plans[j], time.Now()) {
				plans[i].Conflict, plans[j].Conflict = true, true
			}
		}
	}
	render(w, adminPlanTemplate, struct {
		User       user
		Users      []user
		Accounts   []credential
		Plans      []routingPlan
		CSRF       string
		CanRefresh bool
		Now        time.Time
	}{
		u, users, accounts, plans, p.CSRF, a.cfg.CPABaseURL != nil, time.Now()}, a.cfg.GitHubURL)
}

func (a *app) assign(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", 403)
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	if err := a.db.assignPlanKeepingStatus(r.PostForm.Get("user_id"), strings.TrimSpace(r.PostForm.Get("plan_id"))); err != nil {
		if errors.Is(err, errPlanAlreadyAssigned) {
			http.Error(w, "该分流方案已分配给其他用户", http.StatusConflict)
			return
		}
		http.Error(w, "user not found", 404)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *app) refreshSource(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", 403)
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	if a.cfg.CPABaseURL == nil {
		http.Error(w, "source CPA management is not configured", 503)
		return
	}
	accounts, err := a.freshAccounts(r.Context())
	if err != nil {
		http.Error(w, "account lookup failed", 502)
		return
	}
	id := r.PostForm.Get("auth_id")
	var target *credential
	for i := range accounts {
		if accounts[i].ID == id {
			target = &accounts[i]
			break
		}
	}
	if target == nil || !target.FileBacked {
		http.Error(w, "account not found", 404)
		return
	}
	sourceReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.cfg.CPABaseURL.String()+"/v8/management/credentials", nil)
	if err != nil {
		http.Error(w, "refresh failed", 500)
		return
	}
	sourceReq.Header.Set("Authorization", "Bearer "+a.cfg.CPAManagementKey)
	sourceResp, err := managementClient(10 * time.Second).Do(sourceReq)
	if err != nil {
		http.Error(w, "source CPA unavailable", 502)
		return
	}
	defer sourceResp.Body.Close()
	if sourceResp.StatusCode != 200 {
		http.Error(w, "source CPA account lookup failed", 502)
		return
	}
	var sourceList struct {
		Files []credential `json:"files"`
	}
	if err := json.NewDecoder(io.LimitReader(sourceResp.Body, 8<<20)).Decode(&sourceList); err != nil {
		http.Error(w, "source CPA account lookup failed", 502)
		return
	}
	var sourceAccount *credential
	for i := range sourceList.Files {
		if sourceList.Files[i].Name == target.Name && sourceList.Files[i].Source == "file" {
			sourceAccount = &sourceList.Files[i]
			break
		}
	}
	if sourceAccount == nil || sourceAccount.Name == "" {
		http.Error(w, "source CPA account not found", 404)
		return
	}
	body, _ := json.Marshal(map[string]string{"name": sourceAccount.Name, "auth_index": sourceAccount.AuthIndex})
	endpoint := a.cfg.CPABaseURL.String() + "/v8/management/credentials/refresh"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		http.Error(w, "refresh failed", 500)
		return
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.CPAManagementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := managementClient(90 * time.Second).Do(req)
	if err != nil {
		log.Printf("source CPA refresh failed: %v", err)
		http.Error(w, "source CPA refresh failed", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("source CPA refresh status: %d", resp.StatusCode)
		http.Error(w, "source CPA refresh failed", 502)
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func downstreamKey(r *http.Request) string {
	if authorization := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(authorization), "bearer ") {
		return strings.TrimSpace(authorization[7:])
	}
	for _, key := range []string{"X-API-Key", "X-Goog-API-Key"} {
		if value := r.Header.Get(key); value != "" {
			return value
		}
	}
	// Query-string keys are deliberately unsupported: URLs end up in proxy
	// access logs, browser history and Referer headers.
	return ""
}

func (a *app) proxyRequest(w http.ResponseWriter, r *http.Request) {
	u, key, ok := a.db.lookupKeyInfo(downstreamKey(r))
	if !ok {
		http.Error(w, "invalid API key", 401)
		return
	}
	started := time.Now()
	audit := usageLog{At: started.UTC(), UserID: u.ID, Username: u.Username, KeyID: key.ID, PlanID: u.PlanID, Route: r.URL.Path}
	tracked := &logResponseWriter{ResponseWriter: w, started: started}
	w = tracked
	defer func() {
		if a.logs == nil {
			return
		}
		audit.Status = tracked.status
		if audit.Status == 0 {
			audit.Status = http.StatusInternalServerError
		}
		audit.DurationMS = int(time.Since(started).Milliseconds())
		audit.FirstByteMS = tracked.firstByteMS
		audit.ErrorCode, audit.ErrorMessage = tracked.errorDetails()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := a.logs.insert(ctx, audit); err != nil {
			log.Printf("record request log: %v", err)
		}
	}()
	plan, ok := a.db.activePlanForUser(u.ID, time.Now())
	if !ok {
		http.Error(w, "routing plan is not active", 403)
		return
	}
	audit.PlanID = plan.ID
	if allowed, retryAfter := a.planRPM.allow(plan.ID, plan.RPM, time.Now()); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		http.Error(w, "routing plan RPM limit exceeded", http.StatusTooManyRequests)
		return
	}
	a.forwardPlanRequest(w, r, u.ID, plan, &audit)
}

var beijing = time.FixedZone("北京时间", 8*3600)

func parsePlanTime(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", value, beijing)
	if err != nil {
		return nil, err
	}
	t = t.UTC()
	return &t, nil
}

// Existing elapsed boundaries can remain unchanged when an administrator edits
// another plan field. New or changed boundaries must be in a future minute.
func planTimeAllowed(value, previous *time.Time, now time.Time) bool {
	if value == nil {
		return true
	}
	if previous != nil && value.Equal(*previous) {
		return true
	}
	return !value.Before(now)
}

func (a *app) savePlan(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", 403)
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	a.resetGate.Lock()
	defer a.resetGate.Unlock()
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" || len(name) > 80 {
		adminMutationError(w, r, "方案名称不能为空，且最多 80 个字符", 400)
		return
	}
	if len(r.PostForm["account_id"]) == 0 {
		adminMutationError(w, r, "请至少选择一个上游账号", 400)
		return
	}
	planID := strings.TrimSpace(r.PostForm.Get("id"))
	previous, hasPrevious := a.db.getPlan(planID)
	minTime := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	accounts, err := a.freshAccounts(r.Context())
	if err != nil {
		adminMutationError(w, r, "暂时无法获取 CPA 账号，请稍后重试", 502)
		return
	}
	available := make(map[string]credential, len(accounts))
	for _, account := range accounts {
		if !account.Disabled && safePrefix.MatchString(account.Prefix) {
			available[account.ID] = account
		}
	}
	var ids []string
	limits := map[string]int{}
	resetAvailability := map[string]int{}
	windows := map[string]planWindow{}
	seen := map[string]bool{}
	for _, id := range r.PostForm["account_id"] {
		account, ok := available[id]
		if !ok || seen[id] {
			adminMutationError(w, r, "所选账号不可用或重复，请刷新页面后重试", 400)
			return
		}
		seen[id] = true
		ids = append(ids, id)
		start, err := parsePlanTime(r.PostForm.Get("starts_at_" + id))
		if err != nil {
			adminMutationError(w, r, "上游账号开始时间格式无效", 400)
			return
		}
		end, err := parsePlanTime(r.PostForm.Get("ends_at_" + id))
		if err != nil || (start != nil && end != nil && !end.After(*start)) {
			adminMutationError(w, r, "上游账号结束时间必须晚于开始时间", 400)
			return
		}
		var oldWindow planWindow
		if hasPrevious && previous.HasAccount(id) {
			oldWindow = previous.windowFor(id)
		}
		if !planTimeAllowed(start, oldWindow.StartsAt, minTime) || !planTimeAllowed(end, oldWindow.EndsAt, minTime) {
			adminMutationError(w, r, "新设置的开始和结束时间不能早于当前时间", 400)
			return
		}
		windows[id] = planWindow{StartsAt: start, EndsAt: end}
		rawLimit := r.PostForm.Get("reset_" + id)
		if rawLimit == "" {
			rawLimit = "0"
		}
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 0 {
			adminMutationError(w, r, "重置次数必须是非负整数", 400)
			return
		}
		if limit > 0 {
			providerAvailable, err := a.availableResetCount(r.Context(), account)
			if err != nil {
				adminMutationError(w, r, "暂时无法读取上游可用重置次数", 502)
				return
			}
			resetAvailability[id] = providerAvailable
			max := a.db.resetAllocationMax(id, planID, providerAvailable, time.Now())
			if limit > max {
				adminMutationError(w, r, "重置次数超过该上游扣除其他方案预留后的可分配次数", 400)
				return
			}
		}
		limits[id] = limit
	}
	budgetValue := strings.TrimSpace(r.PostForm.Get("budget_usd"))
	budgetMicros, err := parseUSD(budgetValue)
	if err != nil || (budgetValue != "" && budgetMicros == 0) || budgetMicros > 9_000_000_000_000 {
		adminMutationError(w, r, "使用额度须为大于 0 的 USD 金额，最多保留 6 位小数", 400)
		return
	}
	rpmValue := strings.TrimSpace(r.PostForm.Get("rpm"))
	if rpmValue == "" {
		rpmValue = "0"
	}
	rpm, err := strconv.Atoi(rpmValue)
	if err != nil || rpm < 0 || rpm > 1_000_000 {
		adminMutationError(w, r, "每分钟请求数须为 0 到 1000000 的整数，0 表示不限制", 400)
		return
	}
	plan := routingPlan{ID: planID, Name: name, AccountIDs: ids, AccountWindows: windows, ResetLimits: limits, RPM: rpm, BudgetPicoUSD: budgetMicros * 1_000_000}
	if err := a.db.savePlan(plan, resetAvailability); err != nil {
		if errors.Is(err, errPlanAccountConflict) {
			adminMutationError(w, r, "所选上游账号的有效期与已有方案重叠，请调整时间段或账号", http.StatusConflict)
			return
		}
		if errors.Is(err, errPlanResetCapacity) {
			adminMutationError(w, r, "该上游的重置次数已被其他方案预留，请刷新页面后重试", http.StatusConflict)
			return
		}
		adminMutationError(w, r, "无法保存方案，请检查方案内容后重试", 400)
		return
	}
	if rpm == 0 && planID != "" {
		a.planRPM.forget(planID)
	}
	if wantsAdminJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "方案已保存"})
		return
	}
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (a *app) savePrice(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", 403)
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	model := strings.TrimSpace(r.PostForm.Get("model"))
	accounts, err := a.freshAccounts(r.Context())
	if err != nil {
		adminMutationError(w, r, "暂时无法读取 CPA 模型列表", 502)
		return
	}
	valid := false
	for _, id := range planModels(accounts) {
		if id == model {
			valid = true
			break
		}
	}
	if !valid {
		adminMutationError(w, r, "模型已不可用，请刷新页面后重试", 400)
		return
	}
	expression := strings.TrimSpace(r.PostForm.Get("expression"))
	if expression == "" {
		adminMutationError(w, r, "请输入计费表达式", 400)
		return
	}
	if err := a.db.savePrice(model, modelPrice{Expression: expression, Source: "manual"}); err != nil {
		adminMutationError(w, r, "计费表达式无效，请检查语法、变量及金额是否为非负数", 400)
		return
	}
	if wantsAdminJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "模型价格已保存"})
		return
	}
	http.Redirect(w, r, "/prices", http.StatusSeeOther)
}

func wantsAdminJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func adminMutationError(w http.ResponseWriter, r *http.Request, message string, status int) {
	if !wantsAdminJSON(r) {
		http.Error(w, message, status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (a *app) deletePlan(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", 403)
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	if err := a.db.deletePlan(r.PostForm.Get("id")); err != nil {
		http.Error(w, "plan not found", 404)
		return
	}
	a.planRPM.forget(r.PostForm.Get("id"))
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func render(w http.ResponseWriter, source string, data any, githubURL ...string) {
	link := ""
	if len(githubURL) > 0 {
		link = githubURL[0]
	}
	t, err := parsePageTemplate(source, link)
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	nonce, err := scriptNonce()
	if err != nil {
		http.Error(w, "template error", 500)
		return
	}
	var page bytes.Buffer
	if err := t.Execute(&page, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "template error", 500)
		return
	}
	// Script blocks come only from the constant templates: html/template
	// escapes "<" in data, so no user value can produce a nonced tag.
	body := strings.ReplaceAll(page.String(), "<script>", `<script nonce="`+nonce+`">`)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", pageCSP(nonce))
	_, _ = io.WriteString(w, body)
}

func parsePageTemplate(source, githubURL string) (*template.Template, error) {
	return template.New("page").Funcs(template.FuncMap{"githubURL": func() string { return githubURL }}).Parse(source)
}
