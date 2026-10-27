package companion

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A just-observed CPA 429 is shown immediately while the management snapshot
// catches up. It never participates in request routing.
type observedCredentialCooldowns struct {
	mu       sync.Mutex
	accounts map[string]time.Time
	blocked  map[string]time.Time
}

func (c *observedCredentialCooldowns) record(accountID string, resetAt time.Time, now time.Time) {
	if accountID == "" || !resetAt.After(now) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accounts == nil {
		c.accounts = make(map[string]time.Time)
	}
	for id, deadline := range c.accounts {
		if !deadline.After(now) {
			delete(c.accounts, id)
		}
	}
	if resetAt.After(c.accounts[accountID]) {
		c.accounts[accountID] = resetAt
	}
}

func (c *observedCredentialCooldowns) active(now time.Time) map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := make(map[string]time.Time, len(c.accounts))
	for id, deadline := range c.accounts {
		if deadline.After(now) {
			active[id] = deadline
		} else {
			delete(c.accounts, id)
		}
	}
	return active
}

func (c *observedCredentialCooldowns) clear(accountID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.accounts[accountID]; !ok {
		return false
	}
	delete(c.accounts, accountID)
	return true
}

func (c *observedCredentialCooldowns) clearForRefresh(accountID string) {
	c.mu.Lock()
	delete(c.accounts, accountID)
	delete(c.blocked, accountID)
	c.mu.Unlock()
}

func (c *observedCredentialCooldowns) recordBlocked(accountID string, resetAt, now time.Time) {
	if accountID == "" || !resetAt.After(now) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.blocked == nil {
		c.blocked = make(map[string]time.Time)
	}
	c.blocked[accountID] = resetAt
}

func (c *observedCredentialCooldowns) blockedUntil(accountID string, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	resetAt := c.blocked[accountID]
	if !resetAt.After(now) {
		delete(c.blocked, accountID)
		return time.Time{}
	}
	return resetAt
}

func (a *app) cpaCredential(ctx context.Context, accountID string) (credential, error) {
	raw, err := remoteGet(ctx, managementClient(8*time.Second), a.cfg, "/v8/management/credentials", remoteFileLimit)
	if err != nil {
		return credential{}, err
	}
	var result struct {
		Files []credential `json:"files"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.Files == nil {
		return credential{}, errors.New("invalid CPA credential cooldown list")
	}
	for _, account := range result.Files {
		if account.ID == accountID {
			return account, nil
		}
	}
	return credential{}, errors.New("CPA credential missing from cooldown list")
}

// A model_cooldown response describes the requested model, not the underlying
// credential's scope. Ask CPA's management API before applying a local gate.
func (a *app) cpaCredentialCooldown(ctx context.Context, accountID string, now time.Time) (time.Time, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	account, err := a.cpaCredential(ctx, accountID)
	if err != nil {
		return time.Time{}, err
	}
	var resetAt time.Time
	for _, cooldown := range account.Cooldowns {
		if cooldown.Scope == "credential" && quotaCooldown(cooldown) && cooldown.RetryAt.After(now) && cooldown.RetryAt.After(resetAt) {
			resetAt = cooldown.RetryAt
		}
	}
	return resetAt, nil
}

func (a *app) credentialBlockedUntil(account credential, now time.Time) time.Time {
	resetAt := a.observedCooldowns.blockedUntil(account.ID, now)
	for _, cooldown := range account.Cooldowns {
		if cooldown.Scope == "credential" && quotaCooldown(cooldown) && cooldown.RetryAt.After(resetAt) && cooldown.RetryAt.After(now) {
			resetAt = cooldown.RetryAt
		}
	}
	return resetAt
}

func upstream429Reset(header http.Header, now time.Time) time.Time {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if seconds, err := strconv.ParseInt(raw, 10, 64); err == nil && seconds > 0 && seconds <= int64(maxModelCooldown/time.Second) {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if absolute, err := http.ParseTime(raw); err == nil && absolute.After(now) && absolute.Sub(now) <= maxModelCooldown {
		return absolute
	}
	return now.Add(30 * time.Second)
}

// CPA's credential listing exposes active retry timers without raw upstream
// errors. The companion's model_cooldown table covers a freshly observed 429
// even before CPA's cached credential snapshot reflects it.
func (a *app) annotateCredentialCooldowns(ctx context.Context, accounts []credential) {
	now := time.Now()
	observed := a.observedCooldowns.active(now)
	for i := range accounts {
		for _, cooldown := range accounts[i].Cooldowns {
			if !cooldown.RetryAt.After(now) || !quotaCooldown(cooldown) {
				continue
			}
			if cooldown.RetryAt.After(accounts[i].CoolingUntil) {
				accounts[i].CoolingUntil = cooldown.RetryAt
			}
			if cooldown.Scope == "credential" && cooldown.RetryAt.After(accounts[i].BlockedUntil) {
				accounts[i].BlockedUntil = cooldown.RetryAt
			}
		}
		if resetAt := observed[accounts[i].ID]; resetAt.After(accounts[i].CoolingUntil) {
			accounts[i].CoolingUntil = resetAt
		}
		if resetAt := a.observedCooldowns.blockedUntil(accounts[i].ID, now); resetAt.After(accounts[i].BlockedUntil) {
			accounts[i].BlockedUntil = resetAt
			if resetAt.After(accounts[i].CoolingUntil) {
				accounts[i].CoolingUntil = resetAt
			}
		}
	}
	if a.logs == nil {
		return
	}
	local, err := a.logs.activeAccountCooldowns(ctx, now)
	if err != nil {
		log.Printf("read credential cooldowns: %v", err)
		return
	}
	for i := range accounts {
		if resetAt := local[accounts[i].ID]; resetAt.After(accounts[i].CoolingUntil) {
			accounts[i].CoolingUntil = resetAt
		}
	}
}

func quotaCooldown(c credentialCooldown) bool {
	return c.HTTPStatus == 429 || c.Reason == "quota" || c.Reason == "credential_quota"
}

func (s *usageLogStore) activeAccountCooldowns(ctx context.Context, now time.Time) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,MAX(reset_at_ms) FROM model_cooldowns WHERE reset_at_ms > ? GROUP BY account_id`, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	active := make(map[string]time.Time)
	for rows.Next() {
		var id string
		var resetAt int64
		if err := rows.Scan(&id, &resetAt); err != nil {
			return nil, err
		}
		active[id] = time.UnixMilli(resetAt)
	}
	return active, rows.Err()
}
