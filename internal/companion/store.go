package companion

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type user struct {
	ID             string                    `json:"id"`
	Username       string                    `json:"username"`
	Name           string                    `json:"name"`
	AvatarURL      string                    `json:"avatar_url,omitempty"`
	PlanID         string                    `json:"plan_id,omitempty"`
	ResetUsed      map[string]map[string]int `json:"reset_used,omitempty"`
	Disabled       bool                      `json:"disabled,omitempty"`
	SessionVersion int                       `json:"session_version,omitempty"`
}

type routingPlan struct {
	ID             string                `json:"id"`
	Name           string                `json:"name"`
	StartsAt       *time.Time            `json:"-"`
	EndsAt         *time.Time            `json:"-"`
	AccountIDs     []string              `json:"account_ids,omitempty"`
	AccountWindows map[string]planWindow `json:"account_windows,omitempty"`
	ResetLimits    map[string]int        `json:"reset_limits,omitempty"`
	WeeklyMin      map[string]int        `json:"weekly_min,omitempty"`
	Concurrency    map[string]int        `json:"concurrency_limits,omitempty"`
	ResetCaps      map[string]int        `json:"-"`
	RPM            int                   `json:"rpm,omitempty"`
	BudgetPicoUSD  int64                 `json:"budget_pico_usd,omitempty"`
	UsedPicoUSD    int64                 `json:"used_pico_usd,omitempty"`
	UsageError     string                `json:"usage_error,omitempty"`
	OwnerID        string                `json:"-"`
	OwnerName      string                `json:"-"`
	OwnerConflict  bool                  `json:"-"`
	Conflict       bool                  `json:"-"`
}

type planWindow struct {
	StartsAt *time.Time `json:"starts_at,omitempty"`
	EndsAt   *time.Time `json:"ends_at,omitempty"`
}

func clonePlan(p routingPlan) routingPlan {
	p.AccountIDs = append([]string(nil), p.AccountIDs...)
	if p.AccountWindows != nil {
		windows := make(map[string]planWindow, len(p.AccountWindows))
		for id, window := range p.AccountWindows {
			windows[id] = window
		}
		p.AccountWindows = windows
	}
	if p.ResetLimits != nil {
		limits := make(map[string]int, len(p.ResetLimits))
		for id, limit := range p.ResetLimits {
			limits[id] = limit
		}
		p.ResetLimits = limits
	}
	if p.ResetCaps != nil {
		caps := make(map[string]int, len(p.ResetCaps))
		for id, cap := range p.ResetCaps {
			caps[id] = cap
		}
		p.ResetCaps = caps
	}
	if p.WeeklyMin != nil {
		limits := make(map[string]int, len(p.WeeklyMin))
		for id, limit := range p.WeeklyMin {
			limits[id] = limit
		}
		p.WeeklyMin = limits
	}
	if p.Concurrency != nil {
		limits := make(map[string]int, len(p.Concurrency))
		for id, limit := range p.Concurrency {
			limits[id] = limit
		}
		p.Concurrency = limits
	}
	return p
}

func (w planWindow) activeAt(now time.Time) bool {
	return (w.StartsAt == nil || !now.Before(*w.StartsAt)) && (w.EndsAt == nil || now.Before(*w.EndsAt))
}

func (w planWindow) overlaps(other planWindow) bool {
	return (w.EndsAt == nil || other.StartsAt == nil || w.EndsAt.After(*other.StartsAt)) &&
		(other.EndsAt == nil || w.StartsAt == nil || other.EndsAt.After(*w.StartsAt))
}

func (w planWindow) futureAt(now time.Time) bool { return w.EndsAt == nil || w.EndsAt.After(now) }

func (w planWindow) startInput() string {
	if w.StartsAt == nil {
		return ""
	}
	return w.StartsAt.In(beijing).Format("2006-01-02T15:04")
}
func (w planWindow) endInput() string {
	if w.EndsAt == nil {
		return ""
	}
	return w.EndsAt.In(beijing).Format("2006-01-02T15:04")
}

var errPlanAccountConflict = errors.New("upstream account belongs to an overlapping routing plan")
var errPlanAlreadyAssigned = errors.New("routing plan is already assigned to another user")
var errPlanAccountsRequired = errors.New("routing plan requires an upstream account")
var errPlanResetCapacity = errors.New("upstream account reset credits are already reserved")

// Plan validity is a half-open interval [start, end). Adjacent windows can
// reuse the same account without ever owning it at the same instant.
func planWindowsOverlap(a, b routingPlan) bool {
	for _, id := range a.AccountIDs {
		if b.HasAccount(id) && a.windowFor(id).overlaps(b.windowFor(id)) {
			return true
		}
	}
	return false
}

func plansConflictAt(a, b routingPlan, now time.Time) bool {
	for _, id := range a.AccountIDs {
		if a.accountActiveAt(id, now) && b.accountActiveAt(id, now) {
			return true
		}
	}
	return false
}

func futurePlanWindowsOverlap(a, b routingPlan, now time.Time) bool {
	for _, id := range a.AccountIDs {
		if !b.HasAccount(id) {
			continue
		}
		aw, bw := a.windowFor(id), b.windowFor(id)
		if aw.futureAt(now) && bw.futureAt(now) && aw.overlaps(bw) {
			return true
		}
	}
	return false
}

func (p routingPlan) activeAt(now time.Time) bool {
	for _, id := range p.AccountIDs {
		if p.accountActiveAt(id, now) {
			return true
		}
	}
	return false
}

func (p routingPlan) windowFor(id string) planWindow {
	return p.AccountWindows[id]
}

func (p routingPlan) accountActiveAt(id string, now time.Time) bool {
	return p.HasAccount(id) && p.windowFor(id).activeAt(now)
}

func (p routingPlan) AccountStartInput(id string) string { return p.windowFor(id).startInput() }
func (p routingPlan) AccountEndInput(id string) string   { return p.windowFor(id).endInput() }

func (p routingPlan) Periods() []string {
	spans := make([]planWindow, 0, len(p.AccountIDs))
	for _, id := range p.AccountIDs {
		spans = append(spans, p.windowFor(id))
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].StartsAt == nil {
			return spans[j].StartsAt != nil
		}
		if spans[j].StartsAt == nil {
			return false
		}
		return spans[i].StartsAt.Before(*spans[j].StartsAt)
	})
	merged := make([]planWindow, 0, len(spans))
	for _, span := range spans {
		if len(merged) == 0 {
			merged = append(merged, span)
			continue
		}
		last := &merged[len(merged)-1]
		if last.EndsAt != nil && span.StartsAt != nil && last.EndsAt.Before(*span.StartsAt) {
			merged = append(merged, span)
			continue
		}
		if last.EndsAt == nil || span.EndsAt == nil {
			last.EndsAt = nil
		} else if span.EndsAt.After(*last.EndsAt) {
			last.EndsAt = span.EndsAt
		}
	}
	labels := make([]string, 0, len(merged))
	for _, span := range merged {
		start, end := span.startInput(), span.endInput()
		if start == "" {
			start = "立即"
		}
		if end == "" {
			end = "永久"
		}
		labels = append(labels, start+" ～ "+end)
	}
	return labels
}

func (p *routingPlan) deriveBounds() {
	var first, last *time.Time
	openStart, openEnd := false, false
	for _, id := range p.AccountIDs {
		w := p.windowFor(id)
		if w.StartsAt == nil {
			openStart = true
		} else if first == nil || w.StartsAt.Before(*first) {
			first = w.StartsAt
		}
		if w.EndsAt == nil {
			openEnd = true
		} else if last == nil || w.EndsAt.After(*last) {
			last = w.EndsAt
		}
	}
	if openStart {
		first = nil
	}
	if openEnd {
		last = nil
	}
	p.StartsAt, p.EndsAt = first, last
}

func (p routingPlan) StatusText(now time.Time) string {
	if p.Conflict {
		return "账号冲突"
	}
	if p.StartsAt != nil && now.Before(*p.StartsAt) {
		return "未开始"
	}
	if p.EndsAt != nil && !now.Before(*p.EndsAt) {
		return "已到期"
	}
	if !p.activeAt(now) {
		return "时段未开放"
	}
	if p.UsageError != "" {
		return "计费异常"
	}
	if p.BudgetPicoUSD > 0 && p.UsedPicoUSD >= p.BudgetPicoUSD {
		return "额度已用尽"
	}
	return "生效中"
}

func (p routingPlan) StatusClass(now time.Time) string {
	if p.Conflict {
		return "disabled"
	}
	if !p.activeAt(now) {
		return "waiting"
	}
	if !p.usageAllowed() {
		return "disabled"
	}
	return ""
}

func (p routingPlan) HasAccount(id string) bool {
	for _, selected := range p.AccountIDs {
		if selected == id {
			return true
		}
	}
	return false
}
func (p routingPlan) ResetLimit(id string) int       { return p.ResetLimits[id] }
func (p routingPlan) WeeklyMinimum(id string) int    { return p.WeeklyMin[id] }
func (p routingPlan) ConcurrencyLimit(id string) int { return p.Concurrency[id] }
func (p routingPlan) ResetCap(id string) int         { return p.ResetCaps[id] }
func (p routingPlan) StartInput() string {
	if p.StartsAt == nil {
		return ""
	}
	return p.StartsAt.In(beijing).Format("2006-01-02T15:04")
}
func (p routingPlan) EndInput() string {
	if p.EndsAt == nil {
		return ""
	}
	return p.EndsAt.In(beijing).Format("2006-01-02T15:04")
}

type apiKey struct {
	ID        string    `json:"id"`
	UserID    string    `json:"user_id"`
	Name      string    `json:"name"`
	Hash      string    `json:"hash"`
	Sealed    string    `json:"sealed,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type database struct {
	Users                  map[string]user        `json:"-"`
	Keys                   []apiKey               `json:"keys"`
	Plans                  map[string]routingPlan `json:"plans,omitempty"`
	Prices                 map[string]modelPrice  `json:"prices,omitempty"`
	Audit                  *auditConfig           `json:"audit,omitempty"`
	UsageJournalOffset     int64                  `json:"usage_journal_offset,omitempty"`
	UsageJournalGeneration int64                  `json:"usage_journal_generation,omitempty"`
}

type store struct {
	mu      sync.RWMutex
	usageMu sync.Mutex
	path    string
	usersDB *sql.DB
	db      database
}

func openStore(path string) (*store, error) {
	s := &store{path: path, db: database{Users: map[string]user{}, Plans: map[string]routingPlan{}, Prices: map[string]modelPrice{}}}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if decodeErr := json.Unmarshal(b, &s.db); decodeErr != nil {
			return nil, fmt.Errorf("decode store: %w", decodeErr)
		}
	}
	for _, key := range s.db.Keys {
		if key.Sealed == "" {
			return nil, fmt.Errorf("API key %q has no encrypted value", key.ID)
		}
	}
	for id, plan := range s.db.Plans {
		plan.deriveBounds()
		s.db.Plans[id] = plan
	}
	if s.usersDB, err = openUsersDB(filepath.Join(filepath.Dir(path), "users.sqlite")); err != nil {
		return nil, err
	}
	ready := false
	defer func() {
		if !ready {
			s.usersDB.Close()
		}
	}()
	if err := s.loadUsers(); err != nil {
		return nil, err
	}
	if s.db.Plans == nil {
		s.db.Plans = map[string]routingPlan{}
	}
	if s.db.Prices == nil {
		s.db.Prices = map[string]modelPrice{}
	}
	if err := s.replayUsageJournal(); err != nil {
		return nil, fmt.Errorf("replay usage journal: %w", err)
	}
	if err := s.removeUncommittedJournal(); err != nil {
		return nil, fmt.Errorf("clean usage journal: %w", err)
	}
	ready = true
	return s, nil
}

func (s *store) persistLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.db, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".store-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}

func (s *store) upsertUser(u user) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.db.Users[u.ID]; ok {
		u.PlanID, u.ResetUsed, u.Disabled, u.SessionVersion = old.PlanID, old.ResetUsed, old.Disabled, old.SessionVersion
		if old.Username == u.Username && old.Name == u.Name && old.AvatarURL == u.AvatarURL {
			return nil
		}
	}
	return s.saveUserLocked(u)
}

func (s *store) getUser(id string) (user, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.db.Users[id]
	return u, ok
}

func (s *store) users() []user {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]user, 0, len(s.db.Users))
	for _, u := range s.db.Users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out
}

func (s *store) assignPlanKeepingStatus(id, planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.db.Users[id]
	if !ok {
		return os.ErrNotExist
	}
	if planID != "" {
		if _, ok := s.db.Plans[planID]; !ok {
			return os.ErrNotExist
		}
		for otherID, other := range s.db.Users {
			if otherID != id && other.PlanID == planID {
				return errPlanAlreadyAssigned
			}
		}
	}
	u.PlanID = planID
	return s.saveUserLocked(u)
}

func (s *store) activePlanForUser(id string, now time.Time) (routingPlan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.db.Users[id]
	if !ok || u.Disabled || u.PlanID == "" {
		return routingPlan{}, false
	}
	p, ok := s.db.Plans[u.PlanID]
	if !ok || len(p.AccountIDs) == 0 || !p.activeAt(now) || !p.usageAllowed() {
		return routingPlan{}, false
	}
	// Reject conflicting assignments and account windows if stored data was edited outside the application.
	for otherID, other := range s.db.Users {
		if otherID != id && other.PlanID == u.PlanID {
			return routingPlan{}, false
		}
	}
	for otherID, other := range s.db.Plans {
		if otherID != p.ID && plansConflictAt(p, other, now) {
			return routingPlan{}, false
		}
	}
	return clonePlan(p), true
}

func (s *store) plans() []routingPlan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]routingPlan, 0, len(s.db.Plans))
	for _, plan := range s.db.Plans {
		out = append(out, clonePlan(plan))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *store) getPlan(id string) (routingPlan, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	plan, ok := s.db.Plans[id]
	return clonePlan(plan), ok
}

func (s *store) planResetUsedLocked(planID, accountID string) int {
	used := 0
	for _, member := range s.db.Users {
		used += member.ResetUsed[planID][accountID]
	}
	return used
}

// The provider count already excludes credits consumed upstream. Only unused
// allocations are reserved; subtracting a spent allocation again would double-count it.
func (s *store) resetAllocationMaxLocked(accountID, ownPlanID string, providerAvailable int, now time.Time) int {
	reserved := 0
	for _, plan := range s.db.Plans {
		if !plan.HasAccount(accountID) || !plan.windowFor(accountID).futureAt(now) {
			continue
		}
		remaining := plan.ResetLimits[accountID] - s.planResetUsedLocked(plan.ID, accountID)
		if remaining > 0 {
			reserved += remaining
		}
	}
	used := s.planResetUsedLocked(ownPlanID, accountID)
	currentLimit := 0
	ownReserved := 0
	if own, ok := s.db.Plans[ownPlanID]; ok && own.HasAccount(accountID) && own.windowFor(accountID).futureAt(now) {
		currentLimit = own.ResetLimits[accountID]
		if currentLimit > used {
			ownReserved = currentLimit - used
		}
	}
	free := providerAvailable - reserved
	if free < 0 {
		free = 0
	}
	max := used + ownReserved + free
	if currentLimit > max {
		max = currentLimit
	}
	return max
}

func (s *store) resetAllocationMax(accountID, ownPlanID string, providerAvailable int, now time.Time) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resetAllocationMaxLocked(accountID, ownPlanID, providerAvailable, now)
}

func (s *store) savePlan(plan routingPlan, resetAvailability ...map[string]int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(plan.AccountIDs) == 0 {
		return errPlanAccountsRequired
	}
	if plan.BudgetPicoUSD < 0 {
		return errors.New("plan budget cannot be negative")
	}
	if plan.RPM < 0 || plan.RPM > 1_000_000 {
		return errors.New("plan RPM must be between 0 and 1000000")
	}
	for id, limit := range plan.Concurrency {
		if !plan.HasAccount(id) || limit < 0 || limit > 1_000_000 {
			return errors.New("invalid plan account concurrency limit")
		}
	}
	if plan.ID != "" {
		old, ok := s.db.Plans[plan.ID]
		if !ok {
			return os.ErrNotExist
		}
		plan.UsedPicoUSD = old.UsedPicoUSD
	}
	if plan.AccountWindows == nil {
		plan.AccountWindows = map[string]planWindow{}
	}
	for _, id := range plan.AccountIDs {
		w, ok := plan.AccountWindows[id]
		if !ok {
			w = planWindow{}
			plan.AccountWindows[id] = w
		}
		if w.StartsAt != nil && w.EndsAt != nil && !w.EndsAt.After(*w.StartsAt) {
			return errors.New("account window end must be later than start")
		}
	}
	plan.deriveBounds()
	now := time.Now()
	if len(resetAvailability) > 0 {
		for _, id := range plan.AccountIDs {
			limit := plan.ResetLimits[id]
			if limit > s.resetAllocationMaxLocked(id, plan.ID, resetAvailability[0][id], now) {
				return errPlanResetCapacity
			}
		}
	}
	for id, other := range s.db.Plans {
		if id != plan.ID && futurePlanWindowsOverlap(plan, other, now) {
			return fmt.Errorf("%w: %s", errPlanAccountConflict, other.Name)
		}
	}
	if plan.ID == "" {
		id, err := randomHex(8)
		if err != nil {
			return err
		}
		plan.ID = id
	}
	s.db.Plans[plan.ID] = plan
	return s.persistLocked()
}

func (s *store) resetUsed(userID, planID, accountID string) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.planResetUsedLocked(planID, accountID)
}

func (s *store) countReset(userID, planID, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.db.Users[userID]
	if !ok || u.PlanID != planID || u.Disabled {
		return os.ErrNotExist
	}
	plan, ok := s.db.Plans[planID]
	if !ok || !plan.accountActiveAt(accountID, time.Now()) || s.planResetUsedLocked(planID, accountID) >= plan.ResetLimits[accountID] {
		return errors.New("reset allowance exhausted")
	}
	if u.ResetUsed == nil {
		u.ResetUsed = map[string]map[string]int{}
	}
	if u.ResetUsed[planID] == nil {
		u.ResetUsed[planID] = map[string]int{}
	}
	u.ResetUsed[planID][accountID]++
	return s.saveUserLocked(u)
}

func (s *store) refundReset(userID, planID, accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.db.Users[userID]
	if !ok || u.ResetUsed[planID][accountID] <= 0 {
		return os.ErrNotExist
	}
	u.ResetUsed[planID][accountID]--
	return s.saveUserLocked(u)
}

func (s *store) deletePlan(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Plans[id]; !ok {
		return os.ErrNotExist
	}
	delete(s.db.Plans, id)
	var changed []user
	for _, u := range s.db.Users {
		if u.PlanID == id {
			u.PlanID = ""
			changed = append(changed, u)
		}
	}
	if err := s.saveUsersLocked(changed); err != nil {
		return err
	}
	return s.persistLocked()
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func keyHash(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

func (s *store) createKeySecured(userID, name, vaultKey string) (string, error) {
	secret, err := randomHex(32)
	if err != nil {
		return "", err
	}
	id, err := randomHex(8)
	if err != nil {
		return "", err
	}
	plain := "cpa_" + id + "_" + secret
	sealed, err := sealAPIKey(vaultKey, userID, id, plain)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.db.Users[userID]; !ok {
		return "", os.ErrNotExist
	}
	s.db.Keys = append(s.db.Keys, apiKey{ID: id, UserID: userID, Name: name, Hash: keyHash(plain), Sealed: sealed, CreatedAt: time.Now().UTC()})
	if err := s.persistLocked(); err != nil {
		s.db.Keys = s.db.Keys[:len(s.db.Keys)-1]
		return "", err
	}
	return plain, nil
}

func (s *store) lookupKey(plain string) (user, bool) {
	u, _, ok := s.lookupKeyInfo(plain)
	return u, ok
}

func (s *store) lookupKeyInfo(plain string) (user, apiKey, bool) {
	if len(plain) < 20 || len(plain) > 200 {
		return user{}, apiKey{}, false
	}
	h := keyHash(plain)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.db.Keys {
		if subtle.ConstantTimeCompare([]byte(k.Hash), []byte(h)) == 1 {
			u, ok := s.db.Users[k.UserID]
			return u, k, ok && !u.Disabled
		}
	}
	return user{}, apiKey{}, false
}

func (s *store) keysForUser(id string) []apiKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []apiKey{}
	for _, k := range s.db.Keys {
		if k.UserID == id {
			k.Hash = ""
			k.Sealed = ""
			out = append(out, k)
		}
	}
	return out
}

func (s *store) sealedKeyForUser(userID, keyID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, k := range s.db.Keys {
		if k.ID == keyID && k.UserID == userID {
			return k.Sealed, nil
		}
	}
	return "", os.ErrNotExist
}

func (s *store) rotateKey(userID, keyID, vaultKey string) (string, error) {
	secret, err := randomHex(32)
	if err != nil {
		return "", err
	}
	plain := "cpa_" + keyID + "_" + secret
	sealed, err := sealAPIKey(vaultKey, userID, keyID, plain)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.db.Keys {
		if k.ID == keyID && k.UserID == userID {
			s.db.Keys[i].Hash = keyHash(plain)
			s.db.Keys[i].Sealed = sealed
			if err := s.persistLocked(); err != nil {
				s.db.Keys[i] = k
				return "", err
			}
			return plain, nil
		}
	}
	return "", os.ErrNotExist
}

func (s *store) revokeKey(userID, keyID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, k := range s.db.Keys {
		if k.ID == keyID && k.UserID == userID {
			s.db.Keys = append(s.db.Keys[:i], s.db.Keys[i+1:]...)
			return s.persistLocked()
		}
	}
	return os.ErrNotExist
}

// revokeSessions invalidates every session cookie issued to the user so far.
func (s *store) revokeSessions(userID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.db.Users[userID]
	if !ok {
		return os.ErrNotExist
	}
	u.SessionVersion++
	return s.saveUserLocked(u)
}
