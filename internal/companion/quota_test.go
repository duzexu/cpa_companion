package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestQuotaWindowNamesAndResetTimes(t *testing.T) {
	reset := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	windows := parseQuotaWindows("codex", map[string]any{"rate_limit": map[string]any{
		"primary_window":   map[string]any{"used_percent": float64(77), "reset_at": float64(reset.Unix())},
		"secondary_window": map[string]any{"used_percent": float64(12), "reset_at": reset.Format(time.RFC3339)},
	}})
	if len(windows) != 2 || windows[0].Name != "5 小时限额" || windows[1].Name != "周限额" ||
		windows[0].Remaining != 23 || windows[1].Remaining != 88 {
		t.Fatalf("unexpected quota windows: %+v", windows)
	}
	if windows[0].ResetAt == nil || !windows[0].ResetAt.Equal(reset) || windows[0].ResetAtText() == "" || windows[0].ResetRelativeText() == "" {
		t.Fatalf("quota reset time missing: %+v", windows[0])
	}
	if !windows[0].Low() || windows[1].Low() {
		t.Fatal("low quota color threshold is incorrect")
	}
}

func TestWeeklyQuotaAvailabilityUsesMostRestrictiveWindow(t *testing.T) {
	windows := []quotaWindow{
		{Name: "5 小时限额", Remaining: 99},
		{Name: "周限额", Remaining: 82},
		{Name: "Sonnet 周限额", Remaining: 76},
	}
	remaining, found := weeklyQuotaAvailable(windows)
	if !found || remaining != 76 {
		t.Fatalf("unexpected weekly quota: remaining=%d found=%v", remaining, found)
	}
	if got, found := weeklyQuotaAvailable([]quotaWindow{{Name: "5 小时限额", Remaining: 1}}); found || got != 100 {
		t.Fatalf("non-weekly window should not satisfy threshold: remaining=%d found=%v", got, found)
	}
}

func TestWeeklyQuotaCacheTTLTracksDistanceToFloor(t *testing.T) {
	near := weeklyQuotaCacheTTL([]quotaWindow{{Name: "周限额", Remaining: 81}}, nil, 80, 0)
	far := weeklyQuotaCacheTTL([]quotaWindow{{Name: "周限额", Remaining: 99}}, nil, 80, 0)
	busy := weeklyQuotaCacheTTL([]quotaWindow{{Name: "周限额", Remaining: 99}}, nil, 80, 120)
	if near >= far || near < 5*time.Second || far > 2*time.Hour {
		t.Fatalf("cache TTL did not follow quota margin: near=%v far=%v", near, far)
	}
	if busy >= far || busy > time.Minute {
		t.Fatalf("cache TTL did not follow recent request volume: busy=%v far=%v", busy, far)
	}
	if got := weeklyQuotaCacheTTL(nil, errors.New("unavailable"), 80, 0); got != 5*time.Second {
		t.Fatalf("failed quota should use shortest TTL, got %v", got)
	}
}

func TestFilterWeeklyQuotaAccounts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var call struct {
			AuthIndex string `json:"auth_index"`
		}
		if r.URL.Path != "/v0/management/quota/fetch" || json.NewDecoder(r.Body).Decode(&call) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		remaining := 0.79
		if call.AuthIndex == "high" {
			remaining = 0.80
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"groups": []map[string]any{{
			"displayName": "weekly",
			"buckets":     []map[string]any{{"description": "周限额", "remainingFraction": remaining}},
		}}})
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	a := &app{cfg: settings{CPABaseURL: base, CPAManagementKey: "test-key"}}
	accounts := []credential{{ID: "low", AuthIndex: "low", Provider: "other"}, {ID: "high", AuthIndex: "high", Provider: "other"}}
	plan := routingPlan{WeeklyMin: map[string]int{"low": 80, "high": 80}}
	filtered := a.filterWeeklyQuotaAccounts(context.Background(), plan, accounts)
	if len(filtered) != 1 || filtered[0].ID != "high" {
		t.Fatalf("weekly threshold filtering failed: %+v", filtered)
	}
	filtered = a.filterWeeklyQuotaAccounts(context.Background(), plan, accounts)
	if len(filtered) != 1 || filtered[0].ID != "high" || calls.Load() != 2 {
		t.Fatalf("weekly quota cache was not reused: accounts=%+v calls=%d", filtered, calls.Load())
	}
}

func TestResetCreditExpiriesAreSortedAndExcludeExpired(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	earlier, later := now.Add(4*24*time.Hour), now.Add(11*24*time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			URL string `json:"url"`
		}
		if r.URL.Path != "/v0/management/api-call" || json.NewDecoder(r.Body).Decode(&call) != nil ||
			call.URL != "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		body := fmt.Sprintf(`{"applicable_available_count":2,"credits":[{"status":"available","reset_type":"codex_rate_limits","expires_at":%q},{"status":"available","reset_type":"codex_rate_limits","expires_at":%q},{"status":"available","reset_type":"codex_rate_limits","expires_at":%q}]}`,
			later.Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339), earlier.Format(time.RFC3339))
		_ = json.NewEncoder(w).Encode(upstreamCall{StatusCode: 200, Body: body})
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	a := &app{cfg: settings{CPABaseURL: base, CPAManagementKey: "test-key"}}
	status, err := a.resetCreditStatus(context.Background(), credential{Provider: "codex", AuthIndex: "account"})
	if err != nil || status.Count != 2 || len(status.Expiries) != 2 ||
		!status.Expiries[0].Equal(earlier) || !status.Expiries[1].Equal(later) {
		t.Fatalf("unexpected reset credits: %+v, %v", status, err)
	}
}

func TestResetExpiryPreviewTracksNearestAvailableCredit(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	firstExpiry, secondExpiry := now.Add(4*24*time.Hour), now.Add(11*24*time.Hour)
	var firstSpent atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct {
			URL string `json:"url"`
		}
		if r.URL.Path != "/v0/management/api-call" || json.NewDecoder(r.Body).Decode(&call) != nil {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		body := `{"rate_limit":{"primary_window":{"used_percent":0}}}`
		if call.URL == "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits" {
			if firstSpent.Load() {
				body = fmt.Sprintf(`{"applicable_available_count":1,"credits":[{"status":"available","reset_type":"codex_rate_limits","expires_at":%q}]}`, secondExpiry.Format(time.RFC3339))
			} else {
				body = fmt.Sprintf(`{"applicable_available_count":2,"credits":[{"status":"available","reset_type":"codex_rate_limits","expires_at":%q},{"status":"available","reset_type":"codex_rate_limits","expires_at":%q}]}`, firstExpiry.Format(time.RFC3339), secondExpiry.Format(time.RFC3339))
			}
		}
		_ = json.NewEncoder(w).Encode(upstreamCall{StatusCode: 200, Body: body})
	}))
	defer server.Close()
	base, _ := url.Parse(server.URL)
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: settings{CPABaseURL: base, CPAManagementKey: "test-key", CookieKey: "test-cookie-key"}, db: db}
	account := credential{ID: "shared-account", AuthIndex: "shared-account", Provider: "codex", Prefix: "shared"}
	preview := func(userID, planID string) quotaView {
		views := a.quotaViews(context.Background(), user{ID: userID}, routingPlan{ID: planID, AccountIDs: []string{account.ID}, ResetLimits: map[string]int{account.ID: 1}}, []credential{account})
		if len(views) != 1 {
			t.Fatalf("unexpected views: %+v", views)
		}
		return views[0]
	}
	for _, view := range []quotaView{preview("first-user", "first-plan"), preview("second-user", "second-plan")} {
		if view.ResetAvailable != 2 || len(view.ResetExpiries) != 1 ||
			view.ResetExpiries[0].Order != 1 || !view.ResetExpiries[0].ExpiresAt.Equal(firstExpiry) {
			t.Fatalf("one remaining reset should preview the nearest current expiry: %+v", view)
		}
	}
	firstSpent.Store(true)
	a.invalidateQuotaCache(account.ID)
	view := preview("second-user", "second-plan")
	if view.ResetAvailable != 1 || len(view.ResetExpiries) != 1 || !view.ResetExpiries[0].ExpiresAt.Equal(secondExpiry) {
		t.Fatalf("preview should follow the upstream pool after a reset, not retain a fixed expiry: %+v", view)
	}
}
