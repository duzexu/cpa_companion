package companion

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type quotaWindow struct {
	Name      string
	Remaining int
	ResetAt   *time.Time
}

func (w quotaWindow) Low() bool { return w.Remaining < 30 }
func (w quotaWindow) ResetAtText() string {
	if w.ResetAt == nil {
		return ""
	}
	return w.ResetAt.In(beijing).Format("01/02 15:04")
}
func (w quotaWindow) ResetRelativeText() string {
	if w.ResetAt == nil {
		return ""
	}
	return quotaRelativeTime(*w.ResetAt, time.Now())
}
func (w quotaWindow) ResetSoon() bool {
	return w.ResetAt != nil && time.Until(*w.ResetAt) < time.Hour
}

type resetExpiry struct {
	Order     int
	ExpiresAt time.Time
}

func (e resetExpiry) AtText() string { return e.ExpiresAt.In(beijing).Format("01/02 15:04") }
func (e resetExpiry) RelativeText() string {
	return quotaRelativeTime(e.ExpiresAt, time.Now())
}

func quotaRelativeTime(target, now time.Time) string {
	remaining := target.Sub(now)
	if remaining <= 0 {
		return "已到期"
	}
	if remaining < time.Hour {
		return fmt.Sprintf("%d 分钟后", int((remaining+time.Minute-1)/time.Minute))
	}
	if remaining < 24*time.Hour {
		return fmt.Sprintf("%d 小时后", int((remaining+time.Hour-1)/time.Hour))
	}
	return fmt.Sprintf("%d 天后", int((remaining+24*time.Hour-1)/(24*time.Hour)))
}

type quotaView struct {
	Account        credential
	Ref            string
	Active         bool
	StartInput     string
	EndInput       string
	Windows        []quotaWindow
	Error          string
	ResetLimit     int
	ResetUsed      int
	ResetAvailable int
	ResetExpiries  []resetExpiry
}

type resetCreditStatus struct {
	Count    int
	Expiries []time.Time
}

type upstreamCall struct {
	StatusCode int    `json:"status_code"`
	Body       string `json:"body"`
}

var errResetNotSpent = errors.New("upstream reset was not spent")

// accountRef is the only account identifier shown to plan users. CPA file IDs
// are credential file names and often contain the upstream account email.
func (a *app) accountRef(userID, planID, accountID string) string {
	mac := hmac.New(sha256.New, []byte("cpa-companion-account-ref-v1:"+a.cfg.CookieKey))
	mac.Write([]byte(userID + "\x00" + planID + "\x00" + accountID))
	return hex.EncodeToString(mac.Sum(nil)[:12])
}

func (a *app) accountForRef(userID string, plan routingPlan, ref string) string {
	if ref == "" {
		return ""
	}
	for _, id := range plan.AccountIDs {
		if subtle.ConstantTimeCompare([]byte(a.accountRef(userID, plan.ID, id)), []byte(ref)) == 1 {
			return id
		}
	}
	return ""
}

func (a *app) accountCall(ctx context.Context, account credential, method, endpoint, data string, headers map[string]string) (upstreamCall, error) {
	if account.AuthIndex == "" {
		return upstreamCall{}, errors.New("account has no auth index")
	}
	if strings.EqualFold(account.Provider, "codex") && account.ChatGPTAccountID != "" {
		clone := make(map[string]string, len(headers)+1)
		for key, value := range headers {
			clone[key] = value
		}
		clone["Chatgpt-Account-Id"] = account.ChatGPTAccountID
		headers = clone
	}
	payload, _ := json.Marshal(map[string]any{"auth_index": account.AuthIndex, "method": method, "url": endpoint, "header": headers, "data": data})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.CPABaseURL.String()+"/v0/management/api-call", bytes.NewReader(payload))
	if err != nil {
		return upstreamCall{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.CPAManagementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := managementClient(30 * time.Second).Do(req)
	if err != nil {
		return upstreamCall{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return upstreamCall{}, fmt.Errorf("CPA management returned HTTP %d", resp.StatusCode)
	}
	var result upstreamCall
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return upstreamCall{}, err
	}
	if result.StatusCode == 0 || len(result.Body) > 1<<20 {
		return upstreamCall{}, errors.New("invalid upstream response")
	}
	return result, nil
}

func accountHeaders(provider string) map[string]string {
	h := map[string]string{"Authorization": "Bearer $TOKEN$", "Content-Type": "application/json", "Accept": "application/json"}
	if provider == "codex" {
		h["OpenAI-Beta"] = "codex-1"
		h["Originator"] = "Codex Desktop"
		h["User-Agent"] = "codex-tui/0.149.1 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.149.1)"
	}
	if provider == "claude" || provider == "anthropic" {
		h["anthropic-beta"] = "oauth-2025-04-20"
		h["User-Agent"] = "claude-cli/2.1.280 (external, cli)"
	}
	return h
}

func decodeCall(result upstreamCall) (map[string]any, error) {
	if result.StatusCode < 200 || result.StatusCode >= 300 {
		return nil, fmt.Errorf("upstream returned HTTP %d", result.StatusCode)
	}
	var payload map[string]any
	if json.Unmarshal([]byte(result.Body), &payload) != nil || payload == nil {
		return nil, errors.New("invalid quota response")
	}
	return payload, nil
}

func record(v any) map[string]any { m, _ := v.(map[string]any); return m }
func number(v any) int {
	n, _ := v.(float64)
	if n < 0 {
		return 0
	}
	return int(n)
}
func stringField(v any) string { s, _ := v.(string); return s }

func (a *app) availableResetCount(ctx context.Context, account credential) (int, error) {
	status, err := a.resetCreditStatus(ctx, account)
	return status.Count, err
}

func parseQuotaTime(value any) *time.Time {
	var t time.Time
	switch v := value.(type) {
	case string:
		if v == "" {
			return nil
		}
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			t = parsed
		} else if numeric, err := strconv.ParseFloat(v, 64); err == nil {
			value = numeric
		} else {
			return nil
		}
	}
	if numeric, ok := value.(float64); ok {
		if numeric <= 0 || numeric > 253402300799000 {
			return nil
		}
		if numeric > 1e12 {
			numeric /= 1000
		}
		t = time.Unix(int64(numeric), 0)
	}
	if t.IsZero() {
		return nil
	}
	return &t
}

func (a *app) resetCreditStatus(ctx context.Context, account credential) (resetCreditStatus, error) {
	if account.AuthIndex == "" {
		return resetCreditStatus{}, nil
	}
	switch strings.ToLower(account.Provider) {
	case "codex":
		result, err := a.accountCall(ctx, account, "GET", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits", "", accountHeaders("codex"))
		if err != nil {
			return resetCreditStatus{}, err
		}
		data, err := decodeCall(result)
		if err != nil {
			return resetCreditStatus{}, err
		}
		status := resetCreditStatus{}
		credits, _ := data["credits"].([]any)
		for _, item := range credits {
			c := record(item)
			expiry := parseQuotaTime(c["expires_at"])
			if c["status"] == "available" && c["reset_type"] == "codex_rate_limits" &&
				(expiry == nil || time.Now().Before(*expiry)) {
				status.Count++
				if expiry != nil {
					status.Expiries = append(status.Expiries, *expiry)
				}
			}
		}
		if n, ok := data["applicable_available_count"].(float64); ok {
			status.Count = number(n)
		} else if n, ok := data["available_count"].(float64); ok {
			status.Count = number(n)
		}
		sort.Slice(status.Expiries, func(i, j int) bool { return status.Expiries[i].Before(status.Expiries[j]) })
		return status, nil
	case "claude", "anthropic":
		result, err := a.accountCall(ctx, account, "GET", "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1", "", accountHeaders("claude"))
		if err != nil {
			return resetCreditStatus{}, err
		}
		data, err := decodeCall(result)
		if err != nil {
			return resetCreditStatus{}, err
		}
		grantData := record(data["cedar_ember"])
		if grantData["eligible"] != true {
			return resetCreditStatus{}, nil
		}
		grants, _ := grantData["grants"].([]any)
		status := resetCreditStatus{}
		for _, raw := range grants {
			grant := record(raw)
			if grant["paused"] == true || !inGrantWindow(stringField(grant["starts_at"]), stringField(grant["ends_at"])) {
				continue
			}
			left := number(grant["resets_left"])
			status.Count += left
			if expiry := parseQuotaTime(grant["ends_at"]); expiry != nil {
				for i := 0; i < left && len(status.Expiries) < 100; i++ {
					status.Expiries = append(status.Expiries, *expiry)
				}
			}
		}
		sort.Slice(status.Expiries, func(i, j int) bool { return status.Expiries[i].Before(status.Expiries[j]) })
		return status, nil
	default:
		return resetCreditStatus{}, nil
	}
}

func inGrantWindow(start, end string) bool {
	now := time.Now()
	if start != "" {
		if t, err := time.Parse(time.RFC3339, start); err != nil || now.Before(t) {
			return false
		}
	}
	if end != "" {
		if t, err := time.Parse(time.RFC3339, end); err != nil || !now.Before(t) {
			return false
		}
	}
	return true
}

func (a *app) quotaViews(ctx context.Context, u user, plan routingPlan, accounts []credential) []quotaView {
	byID := make(map[string]credential, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
	}
	views := make([]quotaView, 0, len(plan.AccountIDs))
	for _, id := range plan.AccountIDs {
		account, ok := byID[id]
		if !ok {
			continue
		}
		view := quotaView{Account: account, Ref: a.accountRef(u.ID, plan.ID, id), Active: plan.accountActiveAt(id, time.Now()), StartInput: plan.AccountStartInput(id), EndInput: plan.AccountEndInput(id), ResetLimit: plan.ResetLimits[id], ResetUsed: a.db.resetUsed(u.ID, plan.ID, id)}
		views = append(views, view)
	}
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 4)
	for i := range views {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			callCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			view := &views[i]
			account := view.Account
			credits, _ := a.resetCreditStatus(callCtx, account)
			view.ResetAvailable = credits.Count
			remaining := view.ResetLimit - view.ResetUsed
			if remaining > credits.Count {
				remaining = credits.Count
			}
			for j, expiry := range credits.Expiries {
				if j >= remaining {
					break
				}
				view.ResetExpiries = append(view.ResetExpiries, resetExpiry{Order: view.ResetUsed + j + 1, ExpiresAt: expiry})
			}
			endpoint := ""
			switch strings.ToLower(account.Provider) {
			case "codex":
				endpoint = "https://chatgpt.com/backend-api/wham/usage"
			case "claude", "anthropic":
				endpoint = "https://api.anthropic.com/api/oauth/usage"
			default:
				view.Windows, _ = a.pluginQuotaWindows(callCtx, account)
				if len(view.Windows) == 0 {
					view.Error = "该账号暂无可读取的额度接口"
				}
			}
			if endpoint != "" {
				result, err := a.accountCall(callCtx, account, "GET", endpoint, "", accountHeaders(account.Provider))
				if err == nil {
					var data map[string]any
					data, err = decodeCall(result)
					if err == nil {
						view.Windows = parseQuotaWindows(account.Provider, data)
					}
				}
				if err != nil {
					view.Error = "上游额度暂不可用"
				}
				if err == nil && len(view.Windows) == 0 {
					view.Error = "上游未返回可识别的额度窗口"
				}
			}
		}(i)
	}
	wg.Wait()
	return views
}

func (a *app) pluginQuotaWindows(ctx context.Context, account credential) ([]quotaWindow, error) {
	if account.AuthIndex == "" {
		return nil, errors.New("account has no auth index")
	}
	body, _ := json.Marshal(map[string]string{"auth_index": account.AuthIndex})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.CPABaseURL.String()+"/v0/management/quota/fetch", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.CPAManagementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := managementClient(12 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("quota fetch returned HTTP %d", resp.StatusCode)
	}
	var result struct {
		Groups []struct {
			DisplayName string `json:"displayName"`
			Buckets     []struct {
				Window            string  `json:"window"`
				Description       string  `json:"description"`
				RemainingFraction float64 `json:"remainingFraction"`
				ResetTime         string  `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, err
	}
	windows := make([]quotaWindow, 0)
	for _, group := range result.Groups {
		for _, bucket := range group.Buckets {
			remaining := bucket.RemainingFraction
			if remaining < 0 {
				remaining = 0
			}
			if remaining > 1 {
				remaining = 1
			}
			name := strings.TrimSpace(bucket.Description)
			if name == "" {
				name = strings.TrimSpace(bucket.Window)
			}
			if name == "" {
				name = group.DisplayName
			}
			if name == "" {
				name = "额度"
			}
			windows = append(windows, quotaWindow{Name: name, Remaining: int(remaining * 100), ResetAt: parseQuotaTime(bucket.ResetTime)})
		}
	}
	return windows, nil
}

func parseQuotaWindows(provider string, data map[string]any) []quotaWindow {
	var windows []quotaWindow
	add := func(name string, value any) {
		item := record(value)
		used, ok := item["used_percent"].(float64)
		if !ok {
			used, ok = item["utilization"].(float64)
		}
		if !ok {
			return
		}
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		reset := parseQuotaTime(item["resets_at"])
		if reset == nil {
			reset = parseQuotaTime(item["reset_at"])
		}
		windows = append(windows, quotaWindow{Name: name, Remaining: 100 - int(used), ResetAt: reset})
	}
	if strings.ToLower(provider) == "codex" {
		limit := record(data["rate_limit"])
		add("5 小时限额", limit["primary_window"])
		add("周限额", limit["secondary_window"])
		return windows
	}
	for _, entry := range []struct{ key, label string }{{"five_hour", "5 小时限额"}, {"seven_day", "周限额"}, {"seven_day_sonnet", "Sonnet 周限额"}, {"seven_day_oauth_apps", "OAuth 周限额"}} {
		add(entry.label, data[entry.key])
	}
	return windows
}

// A reservation is persisted before contacting the provider. Unknown outcomes
// retain the reservation so a retry cannot spend another user's allowance.
func (a *app) resetQuota(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", 403)
		return
	}
	a.resetGate.Lock()
	defer a.resetGate.Unlock()
	plan, ok := a.db.activePlanForUser(u.ID, time.Now())
	if !ok {
		http.Error(w, "routing plan is not active", 403)
		return
	}
	id := a.accountForRef(u.ID, plan, r.PostForm.Get("account"))
	if id == "" || !plan.accountActiveAt(id, time.Now()) || plan.ResetLimits[id] <= 0 {
		http.Error(w, "reset not allowed", 403)
		return
	}
	accounts, err := a.credentials(r.Context())
	if err != nil {
		http.Error(w, "account lookup failed", 502)
		return
	}
	var account credential
	for _, current := range accounts {
		if current.ID == id {
			account = current
			break
		}
	}
	if account.ID == "" || account.Disabled || account.AuthIndex == "" {
		http.Error(w, "account unavailable", 503)
		return
	}
	available, err := a.availableResetCount(r.Context(), account)
	if err != nil || available <= 0 {
		http.Error(w, "no upstream reset credit available", 409)
		return
	}
	if err := a.db.countReset(u.ID, plan.ID, id); err != nil {
		http.Error(w, "reset allowance exhausted", 403)
		return
	}
	if err := a.performReset(r.Context(), account); err != nil {
		if errors.Is(err, errResetNotSpent) {
			if refundErr := a.db.refundReset(u.ID, plan.ID, id); refundErr != nil {
				log.Printf("quota reset refund failed: %v", refundErr)
			}
			http.Error(w, "upstream reset is currently unavailable", 409)
			return
		}
		log.Printf("quota reset for account %s: %v", account.ID, err)
		http.Error(w, "upstream reset could not be confirmed; allowance reserved", 502)
		return
	}
	http.Redirect(w, r, "/keys", http.StatusSeeOther)
}

func (a *app) performReset(ctx context.Context, account credential) error {
	if strings.EqualFold(account.Provider, "claude") || strings.EqualFold(account.Provider, "anthropic") {
		return a.performClaudeReset(ctx, account)
	}
	if !strings.EqualFold(account.Provider, "codex") {
		return fmt.Errorf("%w: provider reset is not supported", errResetNotSpent)
	}
	id, err := randomHex(16)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"redeem_request_id": id})
	result, err := a.accountCall(ctx, account, "POST", "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits/consume", string(data), accountHeaders("codex"))
	if err != nil {
		return err
	}
	if result.StatusCode == 400 || result.StatusCode == 401 || result.StatusCode == 403 || result.StatusCode == 429 {
		return fmt.Errorf("%w: upstream HTTP %d", errResetNotSpent, result.StatusCode)
	}
	response, err := decodeCall(result)
	if err != nil {
		return err
	}
	if response["code"] != "reset" && response["code"] != "already_redeemed" {
		return fmt.Errorf("%w: reset was refused", errResetNotSpent)
	}
	clear, _ := json.Marshal(map[string]string{"auth_index": account.AuthIndex})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.CPABaseURL.String()+"/v8/management/routing/cooldown/reset", bytes.NewReader(clear))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.CPAManagementKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := managementClient(10 * time.Second).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("cooldown reset returned HTTP %d", resp.StatusCode)
	}
	return nil
}

var resetGrantID = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
var organizationID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (a *app) performClaudeReset(ctx context.Context, account credential) error {
	headers := accountHeaders("claude")
	result, err := a.accountCall(ctx, account, "GET", "https://api.anthropic.com/api/oauth/usage?cedar_ember=1&skip_spend=1", "", headers)
	if err != nil {
		return fmt.Errorf("%w: %v", errResetNotSpent, err)
	}
	usage, err := decodeCall(result)
	if err != nil {
		return fmt.Errorf("%w: %v", errResetNotSpent, err)
	}
	status := record(usage["cedar_ember"])
	if status["eligible"] != true {
		return fmt.Errorf("%w: reset grant is ineligible", errResetNotSpent)
	}
	grantID := stringField(status["next_grant_id"])
	if !resetGrantID.MatchString(grantID) {
		return fmt.Errorf("%w: no usable reset grant", errResetNotSpent)
	}
	grants, _ := status["grants"].([]any)
	usable := false
	for _, raw := range grants {
		grant := record(raw)
		if grant["id"] == grantID && number(grant["resets_left"]) > 0 && grant["paused"] != true && grant["usable_now"] == true && (grant["use_requires_limit"] != true || status["at_limit"] == true) {
			usable = true
			break
		}
	}
	if !usable {
		return fmt.Errorf("%w: reset grant is not usable", errResetNotSpent)
	}
	profileResult, err := a.accountCall(ctx, account, "GET", "https://api.anthropic.com/api/oauth/profile", "", headers)
	if err != nil {
		return fmt.Errorf("%w: %v", errResetNotSpent, err)
	}
	profile, err := decodeCall(profileResult)
	if err != nil {
		return fmt.Errorf("%w: %v", errResetNotSpent, err)
	}
	org := stringField(record(profile["organization"])["uuid"])
	if !organizationID.MatchString(org) {
		return fmt.Errorf("%w: invalid organization", errResetNotSpent)
	}
	requestID, err := randomHex(16)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]string{"program": "cedar_ember", "grant_id": grantID, "request_id": requestID})
	claim, err := a.accountCall(ctx, account, "POST", "https://api.anthropic.com/api/organizations/"+org+"/reset_rate_limits", string(data), headers)
	if err != nil {
		return err
	}
	if claim.StatusCode == 401 || claim.StatusCode == 403 || claim.StatusCode == 429 {
		return fmt.Errorf("%w: upstream HTTP %d", errResetNotSpent, claim.StatusCode)
	}
	response, err := decodeCall(claim)
	if err != nil {
		return err
	}
	if response["result"] != "reset" {
		for _, outcome := range []string{"already_used", "not_limited", "cooldown", "ineligible", "unavailable"} {
			if response["result"] == outcome {
				return fmt.Errorf("%w: %s", errResetNotSpent, outcome)
			}
		}
		return errors.New("reset was not confirmed")
	}
	return nil
}
