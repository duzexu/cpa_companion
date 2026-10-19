package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxForwardBody = 16 << 20

var safePrefix = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// API routing reuses an account snapshot for CPA_ACCOUNT_CACHE_TTL; 0s
// disables reuse while still coalescing concurrent lookups.
const (
	defaultAccountCacheTTL = 15 * time.Second
	maxAccountCacheTTL     = 30 * time.Minute
)

// accountSnapshot is one consistent read of CPA routing metadata.
type accountSnapshot struct {
	accounts []credential
	models   []map[string]any
	aliases  map[string]bool // OAuth model IDs kept internal to CPA
}

// staleRefreshCooldown bounds refreshes triggered by CPA routing errors, so
// error responses cannot be used to amplify management API traffic.
const staleRefreshCooldown = 30 * time.Second

type accountFetch struct {
	done     chan struct{}
	seq      uint64
	started  time.Time
	snapshot accountSnapshot
	err      error
}

// accountCache keeps API traffic from issuing N+4 management requests per call.
// Concurrent misses share one fetch; errors are never cached, so a management
// outage still fails closed once the last good snapshot expires. Fetches are
// sequenced so a slow older fetch never replaces a newer snapshot.
type accountCache struct {
	mu           sync.Mutex
	ttl          time.Duration
	snapshot     accountSnapshot
	fetched      time.Time // start time of the fetch that produced snapshot
	seq          uint64    // last fetch started
	stored       uint64    // fetch that produced snapshot, or the invalidation floor
	inflight     *accountFetch
	staleRefresh time.Time
}

// invalidate drops the snapshot and discards results of fetches already running.
func (c *accountCache) invalidate() {
	c.mu.Lock()
	c.fetched, c.stored, c.inflight = time.Time{}, c.seq, nil
	c.mu.Unlock()
}

// A stale snapshot may be shown on read-only pages when CPA is unavailable.
// Routing never calls this method and still fails closed after the TTL.
func (c *accountCache) lastSnapshot() accountSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snapshot.clone()
}

// forwardAccounts returns cached accounts, refreshing them after the TTL.
func (a *app) forwardAccounts(ctx context.Context) ([]credential, error) {
	snapshot, err := a.accountSnapshot(ctx, false)
	return snapshot.accounts, err
}

// freshAccounts bypasses the cache for administrator views and mutations.
func (a *app) freshAccounts(ctx context.Context) ([]credential, error) {
	snapshot, err := a.accountSnapshot(ctx, true)
	return snapshot.accounts, err
}

func (a *app) accountSnapshot(ctx context.Context, fresh bool) (accountSnapshot, error) {
	c := &a.accountCache
	c.mu.Lock()
	if !fresh && !c.fetched.IsZero() {
		if age := time.Since(c.fetched); age < c.ttl {
			snapshot := c.snapshot.clone()
			// Refresh ahead in the last fifth of the TTL so busy traffic rarely
			// waits on CPA; an expired snapshot is still never served.
			if age >= c.ttl-c.ttl/5 && c.inflight == nil {
				a.startAccountFetchLocked(true)
			}
			c.mu.Unlock()
			return snapshot, nil
		}
	}
	fetch := c.inflight
	if fetch == nil || fresh {
		fetch = a.startAccountFetchLocked(!fresh)
	}
	c.mu.Unlock()
	select {
	case <-fetch.done:
		return fetch.snapshot.clone(), fetch.err
	case <-ctx.Done():
		return accountSnapshot{}, ctx.Err()
	}
}

// refreshStaleAccounts starts a shared background fetch after CPA reports that
// a routed account or model no longer exists. It is rate limited globally.
func (a *app) refreshStaleAccounts() bool {
	c := &a.accountCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight != nil || !c.staleRefresh.IsZero() && time.Since(c.staleRefresh) < staleRefreshCooldown {
		return false
	}
	c.staleRefresh = time.Now()
	a.startAccountFetchLocked(true)
	return true
}

// startAccountFetchLocked requires c.mu. A shared fetch is joined by later
// cache misses; a private one serves a single fresh read.
func (a *app) startAccountFetchLocked(shared bool) *accountFetch {
	c := &a.accountCache
	c.seq++
	fetch := &accountFetch{done: make(chan struct{}), seq: c.seq, started: time.Now()}
	if shared {
		c.inflight = fetch
	}
	go a.runAccountFetch(fetch)
	return fetch
}

// runAccountFetch is detached from any one caller so a cancelled request does
// not fail the other requests waiting on the same fetch.
func (a *app) runAccountFetch(fetch *accountFetch) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	fetch.snapshot, fetch.err = a.fetchAccounts(ctx)
	c := &a.accountCache
	c.mu.Lock()
	if fetch.err == nil && fetch.seq > c.stored {
		c.snapshot, c.fetched, c.stored = fetch.snapshot, fetch.started, fetch.seq
	}
	if c.inflight == fetch {
		c.inflight = nil
	}
	c.mu.Unlock()
	close(fetch.done)
}

// clone copies the slices so callers may edit per-request fields such as
// ResetAvailable. Model maps and model items are shared and read-only.
func (s accountSnapshot) clone() accountSnapshot {
	return accountSnapshot{accounts: append([]credential(nil), s.accounts...), models: append([]map[string]any(nil), s.models...), aliases: s.aliases}
}

// fetchAccounts identifies file and configured provider prefixes without reading credential contents.
func (a *app) fetchAccounts(ctx context.Context) (accountSnapshot, error) {
	client := managementClient(20 * time.Second)
	raw, err := remoteGet(ctx, client, a.cfg, "/v8/management/credentials", remoteFileLimit)
	if err != nil {
		return accountSnapshot{}, err
	}
	var result struct {
		Files []credential `json:"files"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Files == nil {
		return accountSnapshot{}, errors.New("invalid CPA credential list")
	}
	globalIDs, modelItems, err := a.globalModels(ctx, client)
	if err != nil {
		return accountSnapshot{}, err
	}
	aliases, err := a.oauthModelAliases(ctx, client)
	if err != nil {
		return accountSnapshot{}, err
	}
	for id := range globalIDs {
		prefix, suffix, hasSlash := strings.Cut(id, "/")
		if hasSlash && suffix != "" && a.cfg.ModelAliasPrefixes[strings.ToLower(prefix)] {
			aliases[id] = true
		}
	}
	aliasPrefixes := make(map[string]bool)
	for alias := range aliases {
		prefix, suffix, hasSlash := strings.Cut(alias, "/")
		if hasSlash && suffix != "" && safePrefix.MatchString(prefix) {
			aliasPrefixes[strings.ToLower(prefix)] = true
		}
	}
	for prefix := range a.cfg.ModelAliasPrefixes {
		aliasPrefixes[prefix] = true
	}
	configAccounts, err := a.configAccounts(ctx, client, globalIDs, aliasPrefixes)
	if err != nil {
		return accountSnapshot{}, err
	}
	accounts := make([]credential, 0, len(result.Files))
	seenID := make(map[string]bool)
	seenName := make(map[string]bool)
	for _, entry := range result.Files {
		if entry.Source != "file" {
			continue
		}
		if entry.Disabled || strings.EqualFold(strings.TrimSpace(entry.Status), "disabled") {
			continue
		}
		if entry.ID == "" || entry.Name == "" || path.Base(entry.Name) != entry.Name || strings.ContainsAny(entry.Name, `/\`) || !strings.HasSuffix(strings.ToLower(entry.Name), ".json") {
			return accountSnapshot{}, errors.New("invalid CPA credential entry")
		}
		if seenID[entry.ID] || seenName[entry.Name] {
			return accountSnapshot{}, errors.New("duplicate CPA credential identity")
		}
		seenID[entry.ID] = true
		seenName[entry.Name] = true
		file, err := remoteGet(ctx, client, a.cfg, "/v8/management/credentials/models?name="+url.QueryEscape(entry.Name), remoteFileLimit)
		if err != nil {
			return accountSnapshot{}, fmt.Errorf("read CPA models for %q: %w", entry.Name, err)
		}
		var catalog struct {
			Models []struct {
				ID string `json:"id"`
			} `json:"models"`
		}
		if err := json.Unmarshal(file, &catalog); err != nil || catalog.Models == nil {
			return accountSnapshot{}, fmt.Errorf("invalid CPA model catalog for %q", entry.Name)
		}
		entry.FileBacked = true
		modelIDs := make(map[string]bool)
		for _, model := range catalog.Models {
			modelIDs[model.ID] = true
		}
		entry.Models = modelIDs
		candidates := matchingAccountPrefixes(modelIDs, aliasPrefixes)
		for prefix := range candidates {
			routable := false
			for id := range modelIDs {
				if strings.HasPrefix(id, prefix+"/") && globalIDs[id] {
					routable = true
					break
				}
			}
			if !routable {
				delete(candidates, prefix)
			}
		}
		if len(candidates) == 1 {
			for prefix := range candidates {
				entry.Prefix = prefix
			}
		}
		if len(candidates) == 0 {
			entry.Disabled = true
			entry.Status = "模型中未找到有效前缀"
		} else if len(candidates) > 1 {
			entry.Disabled = true
			entry.Status = "模型前缀无法唯一确定"
		}
		accounts = append(accounts, entry)
	}
	accounts = append(accounts, configAccounts...)
	count := make(map[string]int)
	for _, account := range accounts {
		if account.Prefix != "" {
			count[strings.ToLower(account.Prefix)]++
		}
	}
	for i := range accounts {
		if count[strings.ToLower(accounts[i].Prefix)] > 1 {
			accounts[i].Disabled = true
			accounts[i].Status = "前缀重复"
		}
	}
	sort.SliceStable(accounts, func(i, j int) bool {
		if accounts[i].Priority == nil {
			return false
		}
		if accounts[j].Priority == nil {
			return true
		}
		return *accounts[i].Priority > *accounts[j].Priority
	})
	return accountSnapshot{accounts: accounts, models: modelItems, aliases: aliases}, nil
}

func (a *app) oauthModelAliases(ctx context.Context, client *http.Client) (map[string]bool, error) {
	raw, err := remoteGet(ctx, client, a.cfg, "/v0/management/oauth-model-alias", remoteFileLimit)
	if err != nil {
		return nil, fmt.Errorf("read CPA OAuth model aliases: %w", err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, errors.New("invalid CPA OAuth model alias list")
	}
	entry, ok := response["oauth-model-alias"]
	if !ok {
		return nil, errors.New("CPA OAuth model alias list is missing")
	}
	var aliases map[string][]struct {
		Name  string `json:"name"`
		Alias string `json:"alias"`
	}
	if err := json.Unmarshal(entry, &aliases); err != nil {
		return nil, errors.New("invalid CPA OAuth model alias list")
	}
	models := make(map[string]bool)
	for _, list := range aliases {
		for _, alias := range list {
			name, id := strings.TrimSpace(alias.Name), strings.TrimSpace(alias.Alias)
			if id != "" && id != name {
				models[id] = true
			}
		}
	}
	return models, nil
}

// CPA registers both the original model ID and prefix/model for an account.
// A slash in an original ID is not by itself evidence of an account prefix.
// Accept a prefix only when its model IDs exactly mirror the unprefixed IDs.
func matchingAccountPrefixes(modelIDs map[string]bool, excluded map[string]bool) map[string]bool {
	possible := make(map[string]bool)
	for id := range modelIDs {
		prefix, suffix, ok := strings.Cut(id, "/")
		if ok && suffix != "" && safePrefix.MatchString(prefix) && !excluded[strings.ToLower(prefix)] {
			possible[prefix] = true
		}
	}
	matches := make(map[string]bool)
	for prefix := range possible {
		baseCount := 0
		valid := true
		for id := range modelIDs {
			if strings.HasPrefix(id, prefix+"/") {
				if !modelIDs[strings.TrimPrefix(id, prefix+"/")] {
					valid = false
					break
				}
			} else {
				baseCount++
				if !modelIDs[prefix+"/"+id] {
					valid = false
					break
				}
			}
		}
		if valid && baseCount > 0 {
			matches[prefix] = true
		}
	}
	return matches
}

func (a *app) globalModels(ctx context.Context, client *http.Client) (map[string]bool, []map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.CPABaseURL.String()+"/v1/models", nil)
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Authorization", "Bearer "+a.cfg.CPAAPIKey)
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("CPA model lookup: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("CPA model lookup returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, remoteFileLimit+1))
	if err != nil || len(data) > remoteFileLimit {
		return nil, nil, errors.New("CPA model list unavailable or too large")
	}
	var catalog struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil || catalog.Data == nil {
		return nil, nil, errors.New("invalid CPA model list")
	}
	ids := make(map[string]bool, len(catalog.Data))
	for _, model := range catalog.Data {
		if id, ok := model["id"].(string); ok {
			ids[id] = true
		}
	}
	return ids, catalog.Data, nil
}

// forwardPlanRequest keeps all traffic on the original CPA instance. Plan
// accounts are addressed by their model prefixes in CPA priority order.
func (a *app) forwardPlanRequest(w http.ResponseWriter, r *http.Request, userID string, plan routingPlan, audit *usageLog) {
	if len(plan.AccountIDs) == 0 {
		http.Error(w, "plan accounts required", http.StatusForbidden)
		return
	}
	snapshot, err := a.accountSnapshot(r.Context(), false)
	if err != nil {
		http.Error(w, "CPA account verification unavailable", 502)
		return
	}
	byID := make(map[string]credential, len(snapshot.accounts))
	for _, account := range snapshot.accounts {
		byID[account.ID] = account
	}
	selected := make([]credential, 0, len(plan.AccountIDs))
	now := time.Now()
	for _, id := range plan.AccountIDs {
		if !plan.accountActiveAt(id, now) {
			continue
		}
		account, ok := byID[id]
		if !ok || account.Disabled || !safePrefix.MatchString(account.Prefix) {
			continue
		}
		selected = append(selected, account)
	}
	if len(selected) == 0 {
		http.Error(w, "plan accounts unavailable", 503)
		return
	}
	sort.SliceStable(selected, func(i, j int) bool {
		if selected[i].Priority == nil {
			return false
		}
		if selected[j].Priority == nil {
			return true
		}
		return *selected[i].Priority > *selected[j].Priority
	})
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		a.forwardPlanModels(w, snapshot, selected)
		return
	}
	if r.Method != http.MethodPost || !forwardJSONRoute(r.URL.Path) {
		http.Error(w, "unsupported route", 400)
		return
	}
	priced := plan.BudgetPicoUSD > 0
	if priced && !meterableRoute(r.URL.Path) {
		http.Error(w, "usage metering is unavailable for this route", 422)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxForwardBody+1))
	if err != nil || len(body) > maxForwardBody {
		http.Error(w, "invalid request body", 400)
		return
	}
	// Validation, pricing and the forwarded body must all read the same field.
	_, model, err := requestModel(body)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	audit.Model = model
	if _, isAlias := snapshot.aliases[model]; isAlias {
		http.Error(w, "model unavailable in plan accounts", 404)
		return
	}
	eligible := selected[:0]
	for _, account := range selected {
		if account.Models[account.Prefix+"/"+model] {
			eligible = append(eligible, account)
		}
	}
	if len(eligible) == 0 {
		http.Error(w, "model unavailable in plan accounts", 404)
		return
	}
	if err := a.inspectContent(r.Context(), r.Header, body, audit); err != nil {
		log.Printf("content audit failed: %v", err)
	}
	var price modelPrice
	var reservation *usageReservation
	if priced {
		var ok bool
		price, ok = a.db.priceFor(model)
		if !ok {
			http.Error(w, "model price is not configured", 422)
			return
		}
		estimate, err := estimateRequestCharge(r.URL.Path, body, price)
		if err != nil {
			http.Error(w, "request cost could not be estimated", 422)
			return
		}
		reservation, err = a.db.reserveUsage(userID, plan.ID, estimate, time.Now())
		if err != nil {
			if errors.Is(err, errPlanBudgetUnavailable) {
				http.Error(w, "routing plan usage budget is unavailable or exhausted", 403)
			} else {
				log.Printf("reserve plan usage: %v", err)
				http.Error(w, "routing plan usage reservation failed", 500)
			}
			return
		}
		defer func() {
			if err := reservation.refund(); err != nil {
				log.Printf("refund plan usage reservation: %v", err)
			}
		}()
		if !a.db.reservationStillAllowed(userID, plan.ID, time.Now()) {
			http.Error(w, "routing plan usage budget is unavailable or exhausted", 403)
			return
		}
	}
	if meterableRoute(r.URL.Path) {
		body = requestUsageInStream(r.URL.Path, body)
	}
	session := extractRouteSession(r.Header, body)
	namespace := affinityNamespace(userID, plan, eligible, model)
	{
		now := time.Now()
		cooled := map[string]time.Time{}
		if a.logs != nil {
			var err error
			cooled, err = a.logs.activeModelCooldowns(r.Context(), model, now)
			if err != nil {
				log.Printf("read model cooldowns: %v", err)
				http.Error(w, "model cooldown state unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		available := eligible[:0]
		var soonest time.Time
		for _, account := range eligible {
			resetAt := cooled[account.ID]
			if credentialReset := a.credentialBlockedUntil(account, now); credentialReset.After(resetAt) {
				resetAt = credentialReset
			}
			if resetAt.After(now) {
				if soonest.IsZero() || resetAt.Before(soonest) {
					soonest = resetAt
				}
				continue
			}
			available = append(available, account)
		}
		eligible = available
		if len(eligible) == 0 {
			writeModelCooldown(w, model, soonest, time.Now())
			return
		}
	}
	eligible = a.affinity.order(namespace, session, eligible)
	var meter *usageCapture
	settled := false
	var activeAccount credential
	if meterableRoute(r.URL.Path) {
		meter = &usageCapture{target: w, route: r.URL.Path}
	}
	// ReverseProxy panics with http.ErrAbortHandler when a streamed response is
	// cut off. Preserve the selected account and delivered usage before the
	// request-log defer runs, including plans without a USD limit.
	defer func() {
		if recovered := recover(); recovered != nil {
			if meter != nil && meter.status >= 200 && meter.status < 300 {
				audit.AccountID, audit.AccountPrefix = activeAccount.ID, activeAccount.Prefix
				if priced && !settled {
					usage, charge, estimated := a.settleUsage(reservation, price, meter, len(body), true)
					setAuditUsage(audit, usage, charge, estimated)
				} else if !priced {
					a.recordUnpricedUsage(audit, meter, model, len(body), true)
				}
			}
			panic(recovered)
		}
	}()
	var finalCyberResponse *retryResponseWriter
	var finalCyberAccount credential
	defer func() {
		if finalCyberResponse == nil || !finalCyberResponse.committed || !finalCyberResponse.cyberPolicyDetected() {
			return
		}
		audit.AccountID, audit.AccountPrefix = finalCyberAccount.ID, finalCyberAccount.Prefix
		if err := a.recordUpstreamCyberPolicy(body, audit, finalCyberResponse.cyber.code); err != nil {
			log.Printf("record upstream cyber policy audit: %v", err)
		}
	}()
	for i, account := range eligible {
		finalCyberResponse = nil
		attempt := r.Clone(r.Context())
		attempt.Body = io.NopCloser(bytes.NewReader(body))
		attempt.ContentLength = int64(len(body))
		if err := prepareForwardRequest(attempt, account.Prefix); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		// Keep CPA's JSON and SSE response readable so structured upstream
		// policy errors can be recognized without altering the forwarded body.
		attempt.Header.Del("Accept-Encoding")
		target := w
		if meter != nil {
			target = meter
		}
		buffer := &retryResponseWriter{target: target, header: make(http.Header), watchCyber: a.logs != nil}
		finalCyberResponse, finalCyberAccount = buffer, account
		activeAccount = account
		a.proxy.ServeHTTP(buffer, attempt)
		if buffer.status == http.StatusTooManyRequests {
			now := time.Now()
			resetAt, modelCooled := modelCooldownReset(buffer, now)
			if !modelCooled {
				resetAt = upstream429Reset(buffer.header, now)
			}
			credentialReset := time.Time{}
			if modelCooled {
				var err error
				credentialReset, err = a.cpaCredentialCooldown(r.Context(), account.ID, now)
				if err != nil {
					log.Printf("query CPA credential cooldown for %s: %v", account.ID, err)
				}
				if credentialReset.After(now) {
					a.observedCooldowns.recordBlocked(account.ID, credentialReset, now)
				}
			}
			if !credentialReset.After(now) {
				a.observedCooldowns.record(account.ID, resetAt, now)
			}
			a.refreshStaleAccounts()
			if modelCooled && !credentialReset.After(now) && a.logs != nil {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				if err := a.logs.recordModelCooldown(ctx, account.ID, model, resetAt); err != nil {
					log.Printf("record model cooldown for %s: %v", account.ID, err)
				} else {
					seconds := int((time.Until(resetAt) + time.Second - 1) / time.Second)
					if seconds < 1 {
						seconds = 1
					}
					buffer.header.Set("Retry-After", strconv.Itoa(seconds))
				}
				cancel()
			}
		}
		retry := buffer.retryable()
		// CPA rejected a prefix our snapshot considered routable: the account
		// was removed, disabled or lost the model. Refresh in the background and
		// fail over; local validation already rejects models absent from the
		// snapshot, so clients cannot trigger this with arbitrary model names.
		if buffer.staleRoute() {
			a.refreshStaleAccounts()
			retry = true
		}
		if retry && i < len(eligible)-1 {
			a.affinity.rebind(namespace, session.ID, account.ID, eligible[i+1].ID)
			finalCyberResponse = nil
			continue
		}
		if retry {
			a.affinity.rebind(namespace, session.ID, account.ID, "")
		}
		buffer.commit()
		audit.AccountID, audit.AccountPrefix = account.ID, account.Prefix
		if priced && meter.status >= 200 && meter.status < 300 {
			settled = true
			usage, charge, estimated := a.settleUsage(reservation, price, meter, len(body), false)
			setAuditUsage(audit, usage, charge, estimated)
		} else if meter != nil && meter.status >= 200 && meter.status < 300 {
			a.recordUnpricedUsage(audit, meter, model, len(body), false)
		}
		return
	}
}

func (a *app) recordUnpricedUsage(audit *usageLog, meter *usageCapture, model string, requestBytes int, aborted bool) {
	usage, err := meter.result()
	estimated := false
	incomplete := aborted && !(meter.route == "/v1/responses" && meter.finalUsage && err == nil)
	if meter.route != "/v1/images/generations" && (incomplete || err != nil && meter.route == "/v1/alpha/search") {
		usage, estimated, err = meter.estimate(requestBytes), true, nil
	}
	if err != nil {
		return
	}
	charge := int64(0)
	if configured, ok := a.db.priceFor(model); ok {
		if calculated, err := configured.charge(usage); err == nil {
			charge = calculated
		}
	}
	setAuditUsage(audit, usage, charge, estimated)
}

// settleUsage charges every delivered response. A response without a final
// usage report is charged a conservative estimate so it is never free; a
// completed response that lacked usage also suspends the plan for review.
func setAuditUsage(audit *usageLog, usage tokenUsage, charge int64, estimated bool) {
	audit.InputTokens, audit.OutputTokens = usage.Input, usage.Output
	audit.CacheReadTokens, audit.CacheWriteTokens = usage.CacheRead, usage.CacheWrite
	audit.ChargePicoUSD, audit.Estimated = charge, estimated
}

func (a *app) settleUsage(reservation *usageReservation, price modelPrice, meter *usageCapture, requestBytes int, aborted bool) (tokenUsage, int64, bool) {
	usage, err := meter.result()
	meterError := ""
	incomplete := aborted && !(meter.route == "/v1/responses" && meter.finalUsage && err == nil)
	estimated := incomplete || err != nil
	if meter.route == "/v1/images/generations" && (incomplete || err != nil) {
		// A base64 image's response byte count has no useful relationship to
		// billed tokens. Keep the reservation as the charge and pause the plan
		// until an administrator can inspect missing upstream usage.
		meterError = "图片响应未返回可用 Token 用量，已按预估额度扣费"
		if saveErr := reservation.settle(reservation.amount, meterError); saveErr != nil {
			log.Printf("record plan image usage: %v", saveErr)
		}
		return tokenUsage{}, reservation.amount, true
	}
	if meter.route == "/v1/alpha/search" && (incomplete || err != nil) {
		// CPA forwards Codex search responses as-is, and those responses
		// usually omit token usage. Price the request and delivered response
		// bytes using the same conservative ratios as other fallback paths.
		usage = meter.estimate(requestBytes)
	} else if incomplete || err != nil {
		usage = meter.estimate(requestBytes)
		if !incomplete {
			meterError = "上游响应未返回可用 Token 用量，已按估算计费"
		}
	}
	charge, err := price.charge(usage)
	if err != nil {
		charge, meterError = reservation.amount, "Token 用量无法计价，已按预估额度扣费"
	}
	if saveErr := reservation.settle(charge, meterError); saveErr != nil {
		log.Printf("record plan usage: %v", saveErr)
	}
	return usage, charge, estimated
}

type retryResponseWriter struct {
	target     http.ResponseWriter
	header     http.Header
	status     int
	body       bytes.Buffer
	committed  bool
	watchCyber bool
	cyber      cyberPolicyDetector
}

func (w *retryResponseWriter) Header() http.Header { return w.header }
func (w *retryResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	if !heldStatus(status) {
		w.commit()
	}
}

// heldStatus reports responses buffered until retryable or staleRoute decides
// whether the next account should be tried.
func heldStatus(status int) bool {
	switch status {
	case 400, 402, 403, 429, 500, 503:
		return true
	}
	return false
}
func (w *retryResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	if w.committed {
		if w.watchCyber {
			w.cyber.write(p)
		}
		return w.target.Write(p)
	}
	if w.body.Len()+len(p) > 64<<10 {
		w.commit()
		if w.watchCyber {
			w.cyber.write(p)
		}
		return w.target.Write(p)
	}
	return w.body.Write(p)
}
func (w *retryResponseWriter) Flush() {
	if w.committed {
		if f, ok := w.target.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (w *retryResponseWriter) retryable() bool {
	if w.committed {
		return false
	}
	if w.status == 429 {
		return true
	}
	if w.status != 402 && w.status != 403 {
		return false
	}
	message := strings.ToLower(w.body.String())
	return strings.Contains(message, "quota") || strings.Contains(message, "rate_limit") || strings.Contains(message, "rate limit") || strings.Contains(message, "insufficient") || strings.Contains(message, "exhausted") || strings.Contains(message, "额度") || strings.Contains(message, "限流")
}

// staleRoute recognises CPA's errors for a prefixed model that has no provider
// or no usable credential behind it.
func (w *retryResponseWriter) staleRoute() bool {
	if w.committed || (w.status != 400 && w.status != 500 && w.status != 503) {
		return false
	}
	message := strings.ToLower(w.body.String())
	return strings.Contains(message, "model_not_found") || strings.Contains(message, "unknown provider for model") ||
		strings.Contains(message, "auth_not_found") || strings.Contains(message, "auth_unavailable")
}

func (w *retryResponseWriter) commit() {
	if w.committed {
		return
	}
	w.committed = true
	if w.watchCyber {
		w.cyber.stream = strings.Contains(strings.ToLower(w.header.Get("Content-Type")), "text/event-stream")
		w.cyber.write(w.body.Bytes())
	}
	for k, values := range w.header {
		// CPA's trace ID embeds the selected upstream credential index.
		// Keep that internal routing identifier out of downstream responses.
		if strings.EqualFold(k, "X-CPA-TRACE-ID") {
			continue
		}
		for _, value := range values {
			w.target.Header().Add(k, value)
		}
	}
	status := w.status
	if status == 0 {
		status = 200
	}
	w.target.WriteHeader(status)
	if w.body.Len() > 0 {
		_, _ = io.Copy(w.target, &w.body)
	}
}

func (w *retryResponseWriter) cyberPolicyDetected() bool {
	return w.watchCyber && w.cyber.detected()
}

// forwardPlanModels uses the model list from the same snapshot that selected
// the accounts, so the listing and routing never disagree.
func (a *app) forwardPlanModels(w http.ResponseWriter, snapshot accountSnapshot, accounts []credential) {
	visible := make([]map[string]any, 0)
	seen := make(map[string]bool)
	for _, item := range snapshot.models {
		id, _ := item["id"].(string)
		for _, account := range accounts {
			if strings.HasPrefix(id, account.Prefix+"/") {
				base := strings.TrimPrefix(id, account.Prefix+"/")
				_, isAlias := snapshot.aliases[base]
				if !isAlias && !seen[base] {
					// Expose only public model fields. CPA may add internal
					// routing metadata to its own catalog in the future.
					copyItem := map[string]any{"id": base}
					for _, field := range []string{"object", "created", "owned_by"} {
						if value, ok := item[field]; ok {
							copyItem[field] = value
						}
					}
					visible = append(visible, copyItem)
					seen[base] = true
				}
				break
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": visible})
}

func prepareForwardRequest(r *http.Request, prefix string) error {
	if r.Method != http.MethodPost {
		return errors.New("forwarding requires a model-bearing POST request")
	}
	if !forwardJSONRoute(r.URL.Path) {
		return errors.New("API route is not supported in prefix forwarding mode")
	}
	if r.Header.Get("Content-Encoding") != "" {
		return errors.New("compressed request bodies are not supported")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxForwardBody+1))
	if err != nil {
		return errors.New("could not read request body")
	}
	if len(body) > maxForwardBody {
		return errors.New("request body too large")
	}
	payload, model, err := requestModel(body)
	if err != nil {
		return err
	}
	if strings.HasPrefix(model, prefix+"/") {
		return errors.New("send the model name without an account prefix")
	}
	forwardModel := model
	if prefix != "" {
		forwardModel = prefix + "/" + model
	}
	encoded, _ := json.Marshal(forwardModel)
	payload["model"] = encoded
	forwardBody, err := json.Marshal(payload)
	if err != nil {
		return errors.New("could not encode request body")
	}
	r.Body = io.NopCloser(bytes.NewReader(forwardBody))
	r.ContentLength = int64(len(forwardBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Content-Encoding")
	query := r.URL.Query()
	query.Del("model")
	r.URL.RawQuery = query.Encode()
	return nil
}

// requestModel reads the exact "model" key. Go struct decoding matches keys
// case-insensitively, so case variants such as "MODEL" are rejected outright
// rather than letting one variant be priced and another be forwarded.
func requestModel(body []byte) (map[string]json.RawMessage, string, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, "", errors.New("request body must be a JSON object")
	}
	for key := range payload {
		if key != "model" && strings.EqualFold(key, "model") {
			return nil, "", errors.New("request must contain a single lowercase model field")
		}
	}
	var model string
	if err := json.Unmarshal(payload["model"], &model); err != nil || model == "" || len(model) > 256 || strings.TrimSpace(model) != model {
		return nil, "", errors.New("request must contain a valid model")
	}
	return payload, model, nil
}
