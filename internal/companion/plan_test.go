package companion

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlanTemplatesRender(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		data         any
	}{
		{"dashboard", dashboardPlanTemplate, dashboardView{User: user{ID: "u", Username: "alice"}, Plan: routingPlan{Name: "Test", AccountIDs: []string{"a"}}, HasPlan: true, PlanActive: true, Accounts: []quotaView{{Account: credential{ID: "a", Prefix: "first", Provider: "codex"}, Windows: []quotaWindow{{Name: "5 小时限额", Remaining: 75}}, ResetLimit: 1, ResetAvailable: 1, ResetExpiries: []resetExpiry{{Order: 1, ExpiresAt: time.Now().Add(24 * time.Hour)}}}}, CSRF: "csrf", BaseURL: "http://localhost/v1"}},
		{"admin", adminPlanTemplate, struct {
			User       user
			Users      []user
			Accounts   []credential
			Plans      []routingPlan
			Prices     []modelPriceView
			CSRF       string
			CanRefresh bool
			Now        time.Time
		}{user{ID: "u", Username: "alice"}, []user{{ID: "u", Username: "alice", PlanID: "p"}, {ID: "v", Username: "bob"}}, []credential{{ID: "a", Prefix: "first", Provider: "codex"}}, []routingPlan{{ID: "p", Name: "Test", AccountIDs: []string{"a"}, OwnerID: "u", OwnerName: "alice", BudgetPicoUSD: picoUSD}}, []modelPriceView{{Model: "test-model", Price: modelPrice{Expression: `tier("standard", p * 2)`}, Configured: true}}, "csrf", true, time.Now()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := parsePageTemplate(tc.source, "")
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			if err := page.Execute(w, tc.data); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(w.Body.String(), "Test") {
				t.Fatal("plan missing")
			}
			if tc.name == "dashboard" && (!strings.Contains(w.Body.String(), "5 小时限额") || !strings.Contains(w.Body.String(), "主动重置过期时间") || !strings.Contains(w.Body.String(), "第 1 次")) {
				t.Fatal("quota or reset expiry preview missing")
			}
			if tc.name == "admin" && !strings.Contains(w.Body.String(), "已分配给 alice") {
				t.Fatal("assignment owner not shown")
			}
		})
	}
}

func TestAccountCannotBelongToOverlappingPlans(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Minute)
	middle := start.Add(time.Hour)
	end := middle.Add(time.Hour)
	first := routingPlan{Name: "first", AccountIDs: []string{"account-a"}, AccountWindows: map[string]planWindow{"account-a": {StartsAt: &start, EndsAt: &middle}}}
	if err := db.savePlan(first); err != nil {
		t.Fatal(err)
	}
	adjacent := routingPlan{Name: "adjacent", AccountIDs: []string{"account-a"}, AccountWindows: map[string]planWindow{"account-a": {StartsAt: &middle, EndsAt: &end}}}
	if err := db.savePlan(adjacent); err != nil {
		t.Fatal("adjacent windows should be allowed:", err)
	}
	if err := db.savePlan(routingPlan{Name: "overlap", AccountIDs: []string{"account-a"}, AccountWindows: map[string]planWindow{"account-a": {StartsAt: &start, EndsAt: &end}}}); !errors.Is(err, errPlanAccountConflict) {
		t.Fatalf("overlap accepted: %v", err)
	}
	if err := db.savePlan(routingPlan{Name: "other account", AccountIDs: []string{"account-b"}, AccountWindows: map[string]planWindow{"account-b": {StartsAt: &start, EndsAt: &end}}}); err != nil {
		t.Fatal("different account should be allowed:", err)
	}
	if err := db.savePlan(routingPlan{Name: "all accounts"}); !errors.Is(err, errPlanAccountsRequired) {
		t.Fatalf("accountless plan accepted: %v", err)
	}
	for _, plan := range db.plans() {
		if plan.Name == "adjacent" {
			plan.AccountWindows["account-a"] = planWindow{StartsAt: &start, EndsAt: &end}
			if err := db.savePlan(plan); !errors.Is(err, errPlanAccountConflict) {
				t.Fatalf("conflicting edit accepted: %v", err)
			}
			stored, _ := db.getPlan(plan.ID)
			if !stored.StartsAt.Equal(middle) {
				t.Fatal("failed edit changed persisted plan")
			}
		}
	}
}

func TestPlanUsesUnionOfAccountWindows(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Minute).Add(time.Hour)
	aEnd, bStart, bEnd := base.Add(time.Hour), base.Add(2*time.Hour), base.Add(3*time.Hour)
	plan := routingPlan{Name: "split", AccountIDs: []string{"a", "b"}, AccountWindows: map[string]planWindow{
		"a": {StartsAt: &base, EndsAt: &aEnd},
		"b": {StartsAt: &bStart, EndsAt: &bEnd},
	}}
	if err := db.savePlan(plan); err != nil {
		t.Fatal(err)
	}
	stored := db.plans()[0]
	if !stored.StartsAt.Equal(base) || !stored.EndsAt.Equal(bEnd) || len(stored.Periods()) != 2 {
		t.Fatalf("incorrect union: %#v %v", stored, stored.Periods())
	}
	raw, err := os.ReadFile(db.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		Plans map[string]map[string]json.RawMessage `json:"plans"`
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if _, ok := saved.Plans[stored.ID]["starts_at"]; ok {
		t.Fatal("derived plan start was persisted")
	}
	if _, ok := saved.Plans[stored.ID]["ends_at"]; ok {
		t.Fatal("derived plan end was persisted")
	}
	if !stored.accountActiveAt("a", base) || stored.accountActiveAt("b", base) || stored.activeAt(aEnd) || !stored.accountActiveAt("b", bStart) {
		t.Fatal("account window routing failed")
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "alice", stored.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.activePlanForUser("alice", aEnd); ok {
		t.Fatal("gap between account windows allowed traffic")
	}
	if _, ok := db.activePlanForUser("alice", bStart); !ok {
		t.Fatal("later account window denied traffic")
	}
	if err := db.savePlan(routingPlan{Name: "reuse a", AccountIDs: []string{"a"}, AccountWindows: map[string]planWindow{"a": {StartsAt: &aEnd, EndsAt: &bEnd}}}); err != nil {
		t.Fatal("adjacent per-account time should be allowed:", err)
	}
	if err := db.savePlan(routingPlan{Name: "overlap b", AccountIDs: []string{"b"}, AccountWindows: map[string]planWindow{"b": {StartsAt: &aEnd, EndsAt: &bEnd}}}); !errors.Is(err, errPlanAccountConflict) {
		t.Fatalf("overlapping account window accepted: %v", err)
	}
	reopened, err := openStore(db.path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, ok := reopened.getPlan(stored.ID)
	if !ok || len(got.Periods()) != 2 || !got.accountActiveAt("b", bStart) || !got.StartsAt.Equal(base) || !got.EndsAt.Equal(bEnd) {
		t.Fatal("account windows did not persist")
	}
}

func TestPlanTimeCannotBeMovedIntoPast(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	past := now.Add(-time.Hour)
	otherPast := past.Add(time.Minute)
	future := now.Add(time.Hour)
	if planTimeAllowed(&past, nil, now) || planTimeAllowed(&otherPast, &past, now) {
		t.Fatal("new or changed past time accepted")
	}
	if !planTimeAllowed(&past, &past, now) || !planTimeAllowed(&future, nil, now) || !planTimeAllowed(nil, nil, now) {
		t.Fatal("unchanged history or future time rejected")
	}
}

func TestExpiredPlanReleasesAccount(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := db.savePlan(routingPlan{Name: "expired", AccountIDs: []string{"account-a"}, AccountWindows: map[string]planWindow{"account-a": {EndsAt: &past}}}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "current", AccountIDs: []string{"account-a"}}); err != nil {
		t.Fatal("expired plan did not release account:", err)
	}
}

func TestPlanCanOnlyBeAssignedToOneUser(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		if err := db.upsertUser(user{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.savePlan(routingPlan{Name: "private", AccountIDs: []string{"account-a"}}); err != nil {
		t.Fatal(err)
	}
	planID := db.plans()[0].ID
	if err := assignPlanForTest(db, "alice", planID, false); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "bob", planID, false); !errors.Is(err, errPlanAlreadyAssigned) {
		t.Fatalf("double assignment accepted: %v", err)
	}
	if err := assignPlanForTest(db, "alice", planID, true); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "bob", planID, false); !errors.Is(err, errPlanAlreadyAssigned) {
		t.Fatalf("disabled user released assignment: %v", err)
	}
	if err := assignPlanForTest(db, "alice", "", true); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "bob", planID, false); err != nil {
		t.Fatal("released plan could not be assigned:", err)
	}
}

func TestAssignPlanKeepingStatus(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "alice", Disabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "private", AccountIDs: []string{"account-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := db.assignPlanKeepingStatus("alice", db.plans()[0].ID); err != nil {
		t.Fatal(err)
	}
	if member, ok := db.getUser("alice"); !ok || !member.Disabled {
		t.Fatal("saving an assignment changed the existing disabled status")
	}
}

func TestConcurrentPlanAssignmentHasOneWinner(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.savePlan(routingPlan{Name: "exclusive", AccountIDs: []string{"account-a"}}); err != nil {
		t.Fatal(err)
	}
	planID := db.plans()[0].ID
	const users = 12
	for i := 0; i < users; i++ {
		if err := db.upsertUser(user{ID: string(rune('a' + i))}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	results := make(chan error, users)
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(id string) { defer wg.Done(); results <- assignPlanForTest(db, id, planID, false) }(string(rune('a' + i)))
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, errPlanAlreadyAssigned) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("%d users received one plan", successes)
	}
}

func TestConcurrentPlanCreationHasOneWinner(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 12
	var wg sync.WaitGroup
	results := make(chan error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- db.savePlan(routingPlan{Name: "exclusive", AccountIDs: []string{"account-a"}})
		}()
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, errPlanAccountConflict) {
			t.Fatal(err)
		}
	}
	if successes != 1 || len(db.plans()) != 1 {
		t.Fatalf("%d plans reserved the same account", successes)
	}
}

func TestPlanRequiresAssignmentAndActiveTime(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.activePlanForUser("alice", time.Now()); ok {
		t.Fatal("unassigned plan allowed")
	}
	future := time.Now().Add(time.Hour)
	if err := db.savePlan(routingPlan{Name: "later", AccountIDs: []string{"account-a"}, AccountWindows: map[string]planWindow{"account-a": {StartsAt: &future}}}); err != nil {
		t.Fatal(err)
	}
	id := db.plans()[0].ID
	if err := assignPlanForTest(db, "alice", id, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.activePlanForUser("alice", time.Now()); ok {
		t.Fatal("future plan allowed")
	}
	if _, ok := db.activePlanForUser("alice", future); !ok {
		t.Fatal("active plan denied")
	}
	if err := assignPlanForTest(db, "alice", id, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := db.activePlanForUser("alice", future); ok {
		t.Fatal("disabled user allowed")
	}
}

func TestResetAllowanceIsSharedAcrossPlanAssignments(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		if err := db.upsertUser(user{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.savePlan(routingPlan{Name: "team", AccountIDs: []string{"first", "second"}, ResetLimits: map[string]int{"first": 1, "second": 2}}); err != nil {
		t.Fatal(err)
	}
	plan := db.plans()[0]
	if err := assignPlanForTest(db, "alice", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.countReset("alice", plan.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if err := db.countReset("alice", plan.ID, "first"); err == nil {
		t.Fatal("allowed extra reset for alice")
	}
	if err := assignPlanForTest(db, "alice", "", false); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "bob", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.countReset("bob", plan.ID, "first"); err == nil {
		t.Fatal("reassigning a plan restored a spent account reset")
	}
	if err := assignPlanForTest(db, "bob", "", false); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "alice", plan.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.countReset("alice", plan.ID, "second"); err != nil {
		t.Fatal("second account should have own allowance:", err)
	}
	if err := db.refundReset("alice", plan.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if db.resetUsed("alice", plan.ID, "first") != 0 || db.resetUsed("bob", plan.ID, "first") != 0 {
		t.Fatal("refunded reset remained spent")
	}
}

func TestResetCreditsReservedAcrossPlans(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	boundary := now.Add(time.Hour)
	end := boundary.Add(time.Hour)
	first := routingPlan{Name: "first", AccountIDs: []string{"shared"}, AccountWindows: map[string]planWindow{"shared": {EndsAt: &boundary}}, ResetLimits: map[string]int{"shared": 1}}
	if err := db.savePlan(first, map[string]int{"shared": 2}); err != nil {
		t.Fatal(err)
	}
	first = db.plans()[0]
	if got := db.resetAllocationMax("shared", "", 2, now); got != 1 {
		t.Fatalf("second plan may reserve %d resets, want 1", got)
	}
	second := routingPlan{Name: "second", AccountIDs: []string{"shared"}, AccountWindows: map[string]planWindow{"shared": {StartsAt: &boundary, EndsAt: &end}}, ResetLimits: map[string]int{"shared": 2}}
	if err := db.savePlan(second, map[string]int{"shared": 2}); !errors.Is(err, errPlanResetCapacity) {
		t.Fatalf("overallocated reset credits: %v", err)
	}
	second.ResetLimits["shared"] = 1
	if err := db.savePlan(second, map[string]int{"shared": 2}); err != nil {
		t.Fatal(err)
	}
	if got := db.resetAllocationMax("shared", first.ID, 2, now); got != 1 {
		t.Fatalf("editing first plan offered %d resets, want its existing 1", got)
	}
	if err := db.upsertUser(user{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := assignPlanForTest(db, "alice", first.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := db.countReset("alice", first.ID, "shared"); err != nil {
		t.Fatal(err)
	}
	if got := db.resetAllocationMax("shared", "", 1, now); got != 0 {
		t.Fatalf("spent credit was not accounted for: new plan may reserve %d", got)
	}
	second = db.plans()[1]
	if got := db.resetAllocationMax("shared", second.ID, 1, now); got != 1 {
		t.Fatalf("spent credit was subtracted twice: second plan may keep %d", got)
	}
	past := now.Add(-time.Hour)
	if err := db.savePlan(routingPlan{Name: "expired", AccountIDs: []string{"retired"}, AccountWindows: map[string]planWindow{"retired": {EndsAt: &past}}, ResetLimits: map[string]int{"retired": 1}}); err != nil {
		t.Fatal(err)
	}
	for _, plan := range db.plans() {
		if plan.Name == "expired" && db.resetAllocationMax("retired", plan.ID, 0, now) != 0 {
			t.Fatal("expired plan retained a reset allocation without upstream credit")
		}
	}
}

func TestConcurrentPlansCannotReserveSameResetCredit(t *testing.T) {
	db, err := openStore(t.TempDir() + "/store.json")
	if err != nil {
		t.Fatal(err)
	}
	boundary := time.Now().Add(time.Hour)
	plans := []routingPlan{
		{Name: "first", AccountIDs: []string{"shared"}, AccountWindows: map[string]planWindow{"shared": {EndsAt: &boundary}}, ResetLimits: map[string]int{"shared": 1}},
		{Name: "second", AccountIDs: []string{"shared"}, AccountWindows: map[string]planWindow{"shared": {StartsAt: &boundary}}, ResetLimits: map[string]int{"shared": 1}},
	}
	results := make(chan error, len(plans))
	for _, plan := range plans {
		go func(plan routingPlan) { results <- db.savePlan(plan, map[string]int{"shared": 1}) }(plan)
	}
	saved, rejected := 0, 0
	for range plans {
		switch err := <-results; {
		case err == nil:
			saved++
		case errors.Is(err, errPlanResetCapacity):
			rejected++
		default:
			t.Fatalf("unexpected save result: %v", err)
		}
	}
	if saved != 1 || rejected != 1 {
		t.Fatalf("saved %d plans and rejected %d, want 1 each", saved, rejected)
	}
}

func TestOnlyQuotaResponsesRetry(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		retry  bool
	}{
		{429, `{"error":"rate limit"}`, true},
		{402, `{"error":"insufficient quota"}`, true},
		{402, `{"error":"payment required"}`, false},
		{403, `{"error":"quota exhausted"}`, true},
		{403, `{"error":"forbidden"}`, false},
		{500, `{"error":"upstream failure"}`, false},
	} {
		w := &retryResponseWriter{target: httptest.NewRecorder(), header: make(map[string][]string)}
		w.WriteHeader(tc.status)
		_, _ = w.Write([]byte(tc.body))
		if w.retryable() != tc.retry {
			t.Fatalf("status %d body %s retry=%v", tc.status, tc.body, w.retryable())
		}
	}
}
