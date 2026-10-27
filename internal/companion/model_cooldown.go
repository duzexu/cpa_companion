package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CPA reports model_cooldown only after all credentials for a prefixed model
// have cooled down. Keep that result per account and public model, so another
// account can still serve the same model while this one is unavailable.
const maxModelCooldown = 30 * 24 * time.Hour

func (s *usageLogStore) activeModelCooldowns(ctx context.Context, model string, now time.Time) (map[string]time.Time, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account_id,reset_at_ms FROM model_cooldowns WHERE model = ? AND reset_at_ms > ?`, model, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	active := make(map[string]time.Time)
	for rows.Next() {
		var accountID string
		var resetAt int64
		if err := rows.Scan(&accountID, &resetAt); err != nil {
			return nil, err
		}
		active[accountID] = time.UnixMilli(resetAt)
	}
	return active, rows.Err()
}

func (s *usageLogStore) recordModelCooldown(ctx context.Context, accountID, model string, resetAt time.Time) error {
	if accountID == "" || model == "" || resetAt.IsZero() {
		return errors.New("invalid model cooldown")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_cooldowns(account_id,model,reset_at_ms) VALUES(?,?,?)
    ON CONFLICT(account_id,model) DO UPDATE SET reset_at_ms=excluded.reset_at_ms`, accountID, model, resetAt.UnixMilli())
	return err
}

func (s *usageLogStore) clearModelCooldown(ctx context.Context, accountID, model string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_cooldowns WHERE account_id = ? AND model = ?`, accountID, model)
	return err
}

func (s *usageLogStore) clearAccountModelCooldowns(ctx context.Context, accountID string) error {
	if accountID == "" {
		return errors.New("account ID required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM model_cooldowns WHERE account_id = ?`, accountID)
	return err
}

// The absolute deadline is stored rather than CPA's relative duration. An
// expired entry no longer limits requests even if cleanup has not run yet.
func modelCooldownReset(w *retryResponseWriter, now time.Time) (time.Time, bool) {
	if w.committed || w.status != http.StatusTooManyRequests {
		return time.Time{}, false
	}
	var payload struct {
		Error struct {
			Code         string          `json:"code"`
			ResetSeconds json.RawMessage `json:"reset_seconds"`
			ResetTime    string          `json:"reset_time"`
		} `json:"error"`
	}
	if json.Unmarshal(w.body.Bytes(), &payload) != nil || payload.Error.Code != "model_cooldown" {
		return time.Time{}, false
	}
	var seconds float64
	if len(payload.Error.ResetSeconds) > 0 {
		if json.Unmarshal(payload.Error.ResetSeconds, &seconds) != nil {
			var raw string
			if json.Unmarshal(payload.Error.ResetSeconds, &raw) == nil {
				seconds, _ = strconv.ParseFloat(raw, 64)
			}
		}
	}
	var duration time.Duration
	if seconds > 0 && seconds <= maxModelCooldown.Seconds() {
		duration = time.Duration(seconds * float64(time.Second))
	} else if parsed, err := time.ParseDuration(strings.TrimSpace(payload.Error.ResetTime)); err == nil && parsed > 0 && parsed <= maxModelCooldown {
		duration = parsed
	} else if absolute, err := time.Parse(time.RFC3339, strings.TrimSpace(payload.Error.ResetTime)); err == nil && absolute.After(now) && absolute.Sub(now) <= maxModelCooldown {
		return absolute, true
	}
	if duration <= 0 {
		// Keep a short local backoff when CPA omits or corrupts its reset
		// interval; otherwise every request would hit the same cooled channel.
		duration = 30 * time.Second
	}
	return now.Add(duration), true
}

func writeModelCooldown(w http.ResponseWriter, model string, resetAt, now time.Time) {
	seconds := int((resetAt.Sub(now) + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": "model_cooldown", "model": model,
		"message":       fmt.Sprintf("All assigned upstream accounts for model %s are cooling down", model),
		"reset_seconds": seconds, "reset_time": (time.Duration(seconds) * time.Second).String(),
	}})
}
