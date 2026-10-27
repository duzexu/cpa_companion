package companion

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const picoUSD = int64(1_000_000_000_000)

type modelPrice struct {
	Expression string `json:"expression"`
	Source     string `json:"source,omitempty"`
}

type modelPriceView struct {
	Model      string
	Price      modelPrice
	Configured bool
}

func (p routingPlan) BudgetInput() string {
	if p.BudgetPicoUSD == 0 {
		return ""
	}
	return formatPicoUSD(p.BudgetPicoUSD)
}

func (p routingPlan) usageAllowed() bool {
	return p.BudgetPicoUSD == 0 || (p.UsageError == "" && p.UsedPicoUSD < p.BudgetPicoUSD)
}

func (p routingPlan) UsedUSD() string   { return formatPicoUSD(p.UsedPicoUSD) }
func (p routingPlan) BudgetUSD() string { return formatPicoUSD(p.BudgetPicoUSD) }
func (p routingPlan) BudgetPercent() int {
	if p.BudgetPicoUSD <= 0 {
		return 0
	}
	return int(math.Min(100, 100*float64(p.UsedPicoUSD)/float64(p.BudgetPicoUSD)))
}

func formatPicoUSD(value int64) string {
	whole, fraction := value/picoUSD, value%picoUSD
	if fraction == 0 {
		return strconv.FormatInt(whole, 10) + ".00"
	}
	decimal := strings.TrimRight(fmt.Sprintf("%012d", fraction), "0")
	if len(decimal) < 2 {
		decimal += strings.Repeat("0", 2-len(decimal))
	}
	return strconv.FormatInt(whole, 10) + "." + decimal
}

// parseUSD accepts up to six decimal places, sufficient for a budget or a
// per-million-token rate. It deliberately avoids floating-point rounding.
func parseUSD(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || len(parts[0]) == 0 || len(parts[0]) > 7 {
		return 0, errors.New("invalid USD amount")
	}
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || whole < 0 {
		return 0, errors.New("invalid USD amount")
	}
	fraction := int64(0)
	if len(parts) == 2 {
		if len(parts[1]) == 0 || len(parts[1]) > 6 {
			return 0, errors.New("invalid USD amount")
		}
		for _, digit := range parts[1] {
			if digit < '0' || digit > '9' {
				return 0, errors.New("invalid USD amount")
			}
			fraction = fraction*10 + int64(digit-'0')
		}
		for i := len(parts[1]); i < 6; i++ {
			fraction *= 10
		}
	}
	return whole*1_000_000 + fraction, nil
}

func planModels(accounts []credential) []string {
	seen := map[string]bool{}
	for _, account := range accounts {
		if account.Disabled || account.Prefix == "" {
			continue
		}
		prefix := account.Prefix + "/"
		for id := range account.Models {
			if strings.HasPrefix(id, prefix) {
				seen[strings.TrimPrefix(id, prefix)] = true
			}
		}
	}
	models := make([]string, 0, len(seen))
	for model := range seen {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

// The price catalog contains only original models. CPA's OAuth aliases are
// internal to the upstream and are not offered by this service.
func (s accountSnapshot) priceModels() []string {
	models := planModels(s.accounts)
	filtered := models[:0]
	for _, model := range models {
		if _, isAlias := s.aliases[model]; !isAlias {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func (s *store) modelPrices(models []string) []modelPriceView {
	s.mu.RLock()
	defer s.mu.RUnlock()
	views := make([]modelPriceView, 0, len(models))
	for _, model := range models {
		price, ok := s.db.Prices[model]
		views = append(views, modelPriceView{Model: model, Price: price, Configured: ok})
	}
	return views
}

func (s *store) configuredPriceModels() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	models := make([]string, 0, len(s.db.Prices))
	for model := range s.db.Prices {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

func (s *store) priceFor(model string) (modelPrice, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	price, ok := s.db.Prices[model]
	return price, ok
}

func (s *store) savePrice(model string, price modelPrice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(model) == "" || strings.TrimSpace(price.Expression) == "" {
		return errors.New("invalid model price")
	}
	if err := validatePriceExpression(price.Expression); err != nil {
		return fmt.Errorf("invalid billing expression: %w", err)
	}
	if s.db.Prices == nil {
		s.db.Prices = map[string]modelPrice{}
	}
	s.db.Prices[model] = price
	return s.persistLocked()
}

func (s *store) savePrices(prices map[string]modelPrice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Prices == nil {
		s.db.Prices = map[string]modelPrice{}
	}
	for model, price := range prices {
		s.db.Prices[model] = price
	}
	return s.persistLocked()
}

func (s *store) recordUsage(planID string, charge int64, meterError string) error {
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.mu.RLock()
	plan, ok := s.db.Plans[planID]
	if !ok {
		s.mu.RUnlock()
		return errors.New("plan no longer exists")
	}
	if charge < 0 || plan.UsedPicoUSD > math.MaxInt64-charge {
		s.mu.RUnlock()
		return errors.New("usage amount overflow")
	}
	s.mu.RUnlock()
	// Make the small, append-only charge durable before exposing it in memory.
	// JSON snapshots may run concurrently; their journal offset only advances
	// after the charge has been applied under s.mu.
	end, err := s.appendUsageJournal(usageJournalEntry{PlanID: planID, Charge: charge, MeterError: meterError})
	if err != nil {
		return err
	}
	s.mu.Lock()
	plan, ok = s.db.Plans[planID]
	if !ok {
		// An administrator may have deleted the plan while the journal synced.
		// Replay also ignores charges for plans absent from the snapshot.
		s.db.UsageJournalOffset = end
		s.mu.Unlock()
		return nil
	}
	plan.UsedPicoUSD += charge
	if meterError != "" {
		plan.UsageError = meterError
	}
	s.db.Plans[planID] = plan
	s.db.UsageJournalOffset = end
	s.mu.Unlock()
	if err := s.rotateUsageJournal(end); err != nil {
		log.Printf("rotate usage journal: %v", err)
	}
	return nil
}

var errPlanBudgetUnavailable = errors.New("routing plan usage budget is unavailable or exhausted")

// A reservation is durable before the upstream request starts. UsedPicoUSD
// includes in-flight reservations; settlement adjusts it to the actual cost.
// The short usageMu critical sections serialize ledger writes, never streams.
type usageReservation struct {
	store  *store
	planID string
	amount int64
	active bool
}

func (s *store) reserveUsage(userID, planID string, estimate int64, now time.Time) (*usageReservation, error) {
	if estimate < 0 {
		return nil, errors.New("invalid estimated charge")
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	plan, ok := s.activePlanForUser(userID, now)
	if !ok || plan.ID != planID || plan.BudgetPicoUSD <= 0 ||
		estimate > plan.BudgetPicoUSD-plan.UsedPicoUSD {
		return nil, errPlanBudgetUnavailable
	}
	reservation := &usageReservation{store: s, planID: planID, amount: estimate, active: true}
	if estimate == 0 {
		return reservation, nil
	}
	end, err := s.appendUsageJournal(usageJournalEntry{PlanID: planID, Charge: estimate})
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if plan, ok := s.db.Plans[planID]; ok {
		plan.UsedPicoUSD += estimate
		s.db.Plans[planID] = plan
	}
	s.db.UsageJournalOffset = end
	s.mu.Unlock()
	if err := s.rotateUsageJournal(end); err != nil {
		log.Printf("rotate usage journal: %v", err)
	}
	return reservation, nil
}

// A reservation can consume the last available unit, so usageAllowed cannot
// be used to recheck it. This catches administrator edits made during fsync.
func (s *store) reservationStillAllowed(userID, planID string, now time.Time) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.db.Users[userID]
	if !ok || u.Disabled || u.PlanID != planID {
		return false
	}
	plan, ok := s.db.Plans[planID]
	if !ok || len(plan.AccountIDs) == 0 || !plan.activeAt(now) || plan.UsageError != "" ||
		plan.BudgetPicoUSD <= 0 || plan.UsedPicoUSD > plan.BudgetPicoUSD {
		return false
	}
	for otherID, other := range s.db.Users {
		if otherID != userID && other.PlanID == planID {
			return false
		}
	}
	for otherID, other := range s.db.Plans {
		if otherID != planID && plansConflictAt(plan, other, now) {
			return false
		}
	}
	return true
}

func (r *usageReservation) settle(actual int64, meterError string) error {
	if !r.active {
		return nil
	}
	r.active = false
	if actual < 0 {
		return errors.New("invalid settled charge")
	}
	return r.store.adjustReservedUsage(r.planID, actual-r.amount, meterError)
}

// estimateRequestCharge follows new-api's pre-consume/settle pattern. Prompt
// text is counted with the same tokenkit model-family estimator, while
// explicit output-token limits give a useful reservation for streams. The
// fallback is an estimate; final upstream usage remains authoritative and can
// exceed it.
func estimateRequestCharge(route string, body []byte, price modelPrice) (int64, error) {
	var request struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return 0, err
	}
	input := estimateRequestTokens(route, body, request.Model)
	if input < 1 {
		input = 1
	}
	output := int64(0)
	if route == "/v1/images/generations" {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			return 0, err
		}
		// Image output is billed in tokens, but its exact count is only known
		// when CPA returns usage. Reserve a substantial per-image estimate and
		// settle to the actual count after the response has been forwarded.
		var size string
		_ = json.Unmarshal(payload["size"], &size)
		output = 30_000
		if size == "1024x1024" {
			output = 20_000
		}
		var quality string
		_ = json.Unmarshal(payload["quality"], &quality)
		if quality == "high" {
			output *= 2
		}
		var count int64
		if json.Unmarshal(payload["n"], &count) == nil && count > 1 {
			if count > math.MaxInt64/output {
				return 0, errors.New("estimated image token count overflow")
			}
			output *= count
		}
	} else {
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			return 0, err
		}
		for _, field := range []string{"max_completion_tokens", "max_output_tokens", "max_tokens"} {
			var limit int64
			if json.Unmarshal(payload[field], &limit) == nil && limit > output {
				output = limit
			}
		}
		if output == 0 {
			output = 256
		}
		var count int64
		if json.Unmarshal(payload["n"], &count) == nil && count > 1 {
			if count > math.MaxInt64/output {
				return 0, errors.New("estimated output token count overflow")
			}
			output *= count
		}
		if route == "/v1/alpha/search" && output < 4096 {
			// Search responses do not normally report usage. Reserve enough
			// for a typical result set, then settle from the delivered bytes.
			output = 4096
		}
	}
	var estimate int64
	for _, usage := range []tokenUsage{
		{Input: input, Output: output},
		{Input: input, Output: output, CacheRead: input},
		{Input: input, Output: output, CacheWrite: input},
	} {
		charge, err := price.charge(usage)
		if err != nil {
			return 0, err
		}
		if charge > estimate {
			estimate = charge
		}
	}
	return estimate, nil
}

func (r *usageReservation) refund() error {
	if !r.active {
		return nil
	}
	r.active = false
	return r.store.adjustReservedUsage(r.planID, -r.amount, "")
}

func (s *store) adjustReservedUsage(planID string, delta int64, meterError string) error {
	if delta == 0 && meterError == "" {
		return nil
	}
	s.usageMu.Lock()
	defer s.usageMu.Unlock()
	s.mu.RLock()
	plan, ok := s.db.Plans[planID]
	if !ok {
		s.mu.RUnlock()
		return errors.New("plan no longer exists")
	}
	if delta > 0 && plan.UsedPicoUSD > math.MaxInt64-delta ||
		delta < 0 && (delta == math.MinInt64 || plan.UsedPicoUSD < -delta) {
		s.mu.RUnlock()
		return errors.New("usage amount overflow")
	}
	s.mu.RUnlock()
	end, err := s.appendUsageJournal(usageJournalEntry{PlanID: planID, Charge: delta, MeterError: meterError})
	if err != nil {
		s.mu.Lock()
		if plan, ok := s.db.Plans[planID]; ok {
			plan.UsageError = "计费记录写入失败，方案已暂停"
			s.db.Plans[planID] = plan
		}
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	if plan, ok := s.db.Plans[planID]; ok {
		plan.UsedPicoUSD += delta
		if meterError != "" {
			plan.UsageError = meterError
		}
		s.db.Plans[planID] = plan
	}
	s.db.UsageJournalOffset = end
	s.mu.Unlock()
	if err := s.rotateUsageJournal(end); err != nil {
		log.Printf("rotate usage journal: %v", err)
	}
	return nil
}

type tokenUsage struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

func (u tokenUsage) valid() bool {
	return u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheWrite >= 0 &&
		u.CacheRead <= u.Input && u.CacheWrite <= u.Input-u.CacheRead
}

func (p modelPrice) charge(u tokenUsage) (int64, error) {
	if !u.valid() {
		return 0, errors.New("invalid token usage")
	}
	return chargeExpression(p.Expression, u)
}
