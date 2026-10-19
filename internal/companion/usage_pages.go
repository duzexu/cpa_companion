package companion

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const logTimeLayout = "2006-01-02T15:04"

func logTimeRange(q url.Values, now time.Time) (time.Time, time.Time, error) {
	local := now.In(beijing)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, beijing)
	end := local.Add(time.Hour).Truncate(time.Minute)
	var err error
	if raw := q.Get("start"); raw != "" {
		start, err = time.ParseInLocation(logTimeLayout, raw, beijing)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if raw := q.Get("end"); raw != "" {
		end, err = time.ParseInLocation(logTimeLayout, raw, beijing)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
	}
	if end.Before(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("end precedes start")
	}
	return start, end.Add(time.Minute), nil // The displayed end minute is inclusive.
}
func selectedScope(r *http.Request, u user, admin bool) string {
	if admin && r.URL.Query().Get("scope") == "all" {
		return ""
	}
	return u.ID
}
func (a *app) usageDashboard(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if a.logs == nil {
		http.Error(w, "usage database unavailable", 503)
		return
	}
	admin := a.cfg.AdminSubjects[u.ID]
	now := time.Now()
	scope := selectedScope(r, u, admin)
	f := usageFilter{UserID: scope, Since: now.Add(-6 * time.Hour)}
	summary, err := a.logs.summary(r.Context(), f)
	if err != nil {
		http.Error(w, "usage summary unavailable", 500)
		return
	}
	modelCount, err := a.logs.distinctModels(r.Context(), f)
	if err != nil {
		http.Error(w, "model count unavailable", 500)
		return
	}
	traffic, accountTraffic, err := a.logs.traffic(r.Context(), scope, now)
	if err != nil {
		http.Error(w, "traffic unavailable", 500)
		return
	}
	plan, hasPlan := a.db.getPlan(u.PlanID)
	_, planActive := a.db.activePlanForUser(u.ID, now)
	var assigned int
	if hasPlan {
		assigned = len(plan.AccountIDs)
	}
	accountProviders := map[string]string{}
	accountDisabled := map[string]bool{}
	accountDetails := map[string]credential{}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if accounts, err := a.credentials(ctx); err == nil {
		for _, account := range accounts {
			accountProviders[account.ID] = account.Provider
			accountDisabled[account.ID] = account.Disabled
			accountDetails[account.ID] = account
		}
	}
	activeAccounts := 0
	if planActive {
		for _, id := range plan.AccountIDs {
			if plan.accountActiveAt(id, now) && !accountDisabled[id] {
				activeAccounts++
			}
		}
	}
	providers := dashboardProviders(plan, hasPlan, scope == "", accountTraffic, accountProviders)
	var recentRequests, recentSuccess, recentFailure int64
	values := make([]int64, len(traffic))
	for i, b := range traffic {
		recentSuccess += b.Success
		recentFailure += b.Failed
		values[i] = b.Success + b.Failed
	}
	recentRequests = recentSuccess + recentFailure
	verdict := "静候流量"
	if recentRequests > 0 {
		rate := recentSuccess * 100 / recentRequests
		switch {
		case rate >= 95:
			verdict = "运行平稳"
		case rate >= 80:
			verdict = "略有波动"
		default:
			verdict = "需要留意"
		}
	}
	planStatus := "尚未分配分流方案"
	planStatusClass := "waiting"
	if hasPlan {
		planStatus = plan.StatusText(now)
		planStatusClass = plan.StatusClass(now)
		if planStatus == "生效中" && !planActive {
			planStatus = "暂不可用"
			planStatusClass = "disabled"
		}
	}
	planAccounts := make([]dashboardPlanAccount, 0, assigned)
	planPeriods := make([]string, 0)
	if hasPlan {
		for _, period := range plan.Periods() {
			planPeriods = append(planPeriods, strings.ReplaceAll(period, "T", " "))
		}
		planAccounts = dashboardPlanAccounts(plan, accountDetails, planActive, now)
	}
	assignments, assignedUsers := dashboardAssignments(a.db.users(), a.db.plans(), accountDetails, now)
	scopeText := "我的数据"
	if scope == "" {
		scopeText = "全部用户"
	}
	render(w, usageDashboardTemplate, struct {
		User                                                 user
		CSRF                                                 string
		Admin                                                bool
		All                                                  bool
		ScopeText                                            string
		Summary                                              usageSummary
		Traffic                                              []trafficBucket
		Providers                                            []dashboardProvider
		RecentRequests, RecentSuccess, RecentFailure         int64
		SuccessRate                                          int64
		SuccessWidth, FailureWidth                           int64
		Verdict, WirePoints, PlanName, PlanStatus            string
		PlanStatusClass                                      string
		Plan                                                 routingPlan
		PlanPeriods                                          []string
		PlanAccounts                                         []dashboardPlanAccount
		Assignments                                          []dashboardAssignment
		AssignedUsers                                        int
		HasPlan, PlanActive, HasBudget                       bool
		Assigned, ActiveAccounts, InactiveAccounts, KeyCount int
		ActivePercent, InactivePercent                       int64
		ModelCount                                           int64
		Budget, Used                                         string
	}{u, p.CSRF, admin, scope == "", scopeText, summary, traffic, providers,
		recentRequests, recentSuccess, recentFailure, percentOf(recentSuccess, recentRequests), percentOf(recentSuccess, recentRequests), percentOf(recentFailure, recentRequests),
		verdict, sparklinePoints(values), plan.Name, planStatus, planStatusClass, plan, planPeriods, planAccounts, assignments, assignedUsers, hasPlan, planActive, plan.BudgetPicoUSD > 0,
		assigned, activeAccounts, assigned - activeAccounts, len(a.db.keysForUser(u.ID)), percentOf(int64(activeAccounts), int64(assigned)), percentOf(int64(assigned-activeAccounts), int64(assigned)), modelCount,
		formatPicoUSD(plan.BudgetPicoUSD), formatPicoUSD(plan.UsedPicoUSD)}, a.cfg.GitHubURL)
}

type dashboardPlanAccount struct {
	Label, Period, State string
	Active               bool
}

func dashboardPlanAccounts(plan routingPlan, details map[string]credential, planActive bool, now time.Time) []dashboardPlanAccount {
	accounts := make([]dashboardPlanAccount, 0, len(plan.AccountIDs))
	for index, id := range plan.AccountIDs {
		account, known := details[id]
		label := fmt.Sprintf("上游账号 %d", index+1)
		state := "账号信息暂不可用"
		active := false
		if known {
			label = account.Provider + " · " + account.Prefix
			state = "时段未开放"
			if account.Disabled {
				state = "上游已禁用"
			} else if plan.accountActiveAt(id, now) {
				if planActive {
					state, active = "当前可用", true
				} else {
					state = "方案不可用"
				}
			}
		}
		start, end := plan.AccountStartInput(id), plan.AccountEndInput(id)
		if start == "" {
			start = "立即"
		}
		if end == "" {
			end = "永久"
		}
		accounts = append(accounts, dashboardPlanAccount{Label: label, Period: strings.ReplaceAll(start+" ～ "+end, "T", " "), State: state, Active: active})
	}
	return accounts
}

type dashboardAssignment struct {
	Username, UserID, PlanName, Status, StatusClass, Periods string
	AccountCount                                             int
	Accounts                                                 []dashboardPlanAccount
	Assigned                                                 bool
}

func dashboardAssignments(users []user, plans []routingPlan, details map[string]credential, now time.Time) ([]dashboardAssignment, int) {
	byID := make(map[string]routingPlan, len(plans))
	for _, plan := range plans {
		byID[plan.ID] = plan
	}
	rows := make([]dashboardAssignment, 0, len(users))
	assigned := 0
	for _, member := range users {
		name := member.Username
		if name == "" {
			name = member.ID
		}
		row := dashboardAssignment{Username: name, UserID: member.ID, PlanName: "—", Status: "未分配", StatusClass: "waiting"}
		if member.PlanID != "" {
			row.Assigned = true
			assigned++
			if plan, ok := byID[member.PlanID]; ok {
				row.PlanName = plan.Name
				row.Status = plan.StatusText(now)
				row.StatusClass = plan.StatusClass(now)
				row.AccountCount = len(plan.AccountIDs)
				row.Accounts = dashboardPlanAccounts(plan, details, !member.Disabled && row.Status == "生效中", now)
				periods := plan.Periods()
				for i := range periods {
					periods[i] = strings.ReplaceAll(periods[i], "T", " ")
				}
				row.Periods = strings.Join(periods, "；")
			} else {
				row.Status, row.StatusClass = "方案不存在", "disabled"
			}
		}
		if member.Disabled {
			row.Status, row.StatusClass = "用户已禁用", "disabled"
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Assigned != rows[j].Assigned {
			return rows[i].Assigned
		}
		if rows[i].Username != rows[j].Username {
			return rows[i].Username < rows[j].Username
		}
		return rows[i].UserID < rows[j].UserID
	})
	return rows, assigned
}

type dashboardProvider struct {
	Name, ID, Label, Points string
	Rank                    int
	Accounts                int
	Requests, Success       int64
	Buckets                 []int64
}

func (p dashboardProvider) Rate() int64 { return percentOf(p.Success, p.Requests) }
func percentOf(n, total int64) int64 {
	if total == 0 {
		return 0
	}
	return n * 100 / total
}

func dashboardProviders(plan routingPlan, hasPlan, all bool, traffic []accountTraffic, types map[string]string) []dashboardProvider {
	groups := map[string]*dashboardProvider{}
	allowed := map[string]bool{}
	if !all && hasPlan {
		for _, id := range plan.AccountIDs {
			allowed[id] = true
		}
	}
	for id, provider := range types {
		if !all && !allowed[id] {
			continue
		}
		if provider == "" {
			provider = "其他"
		}
		if groups[provider] == nil {
			groups[provider] = &dashboardProvider{Name: provider}
		}
		groups[provider].Accounts++
	}
	for _, entry := range traffic {
		if !all && !allowed[entry.AccountID] {
			continue
		}
		provider := types[entry.AccountID]
		if provider == "" {
			provider = "其他"
		}
		if groups[provider] == nil {
			groups[provider] = &dashboardProvider{Name: provider}
		}
		groups[provider].Requests += entry.Requests
		groups[provider].Success += entry.Success
		if len(groups[provider].Buckets) == 0 {
			groups[provider].Buckets = make([]int64, len(entry.Buckets))
		}
		for i, n := range entry.Buckets {
			groups[provider].Buckets[i] += n
		}
	}
	// When CPA management is unavailable, still show assigned accounts from local plan data.
	if !all && hasPlan && len(types) == 0 {
		groups["已分配上游"] = &dashboardProvider{Name: "已分配上游", Accounts: len(plan.AccountIDs)}
	}
	out := make([]dashboardProvider, 0, len(groups))
	for _, entry := range groups {
		entry.Label = strings.ToUpper(entry.Name)
		if len(entry.Buckets) > 0 {
			entry.Points = sparklinePoints(entry.Buckets)
		}
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests == out[j].Requests {
			return out[i].Name < out[j].Name
		}
		return out[i].Requests > out[j].Requests
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func (a *app) usageLogsPage(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if a.logs == nil {
		http.Error(w, "usage database unavailable", 503)
		return
	}
	admin := a.cfg.AdminSubjects[u.ID]
	q := r.URL.Query()
	var logID int64
	if raw := q.Get("log_id"); raw != "" {
		var parseErr error
		logID, parseErr = strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || logID <= 0 {
			http.Error(w, "invalid log ID", 400)
			return
		}
	}
	now := time.Now()
	since, until, err := logTimeRange(q, now)
	if err != nil {
		http.Error(w, "invalid time range", 400)
		return
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	if page > 10000 {
		page = 10000
	}
	all := admin && q.Get("scope") != "self"
	userInput := strings.TrimSpace(q.Get("user"))
	if !all {
		userInput = ""
	}
	if len(userInput) > 128 {
		http.Error(w, "invalid user filter", 400)
		return
	}
	userFilter := u.ID
	if all {
		userFilter = userInput
	}
	model := strings.TrimSpace(q.Get("model"))
	keyID := strings.TrimSpace(q.Get("key"))
	accountID := strings.TrimSpace(q.Get("account"))
	route := strings.TrimSpace(q.Get("route"))
	if len(model) > 256 || len(keyID) > 128 || len(accountID) > 128 || len(route) > 256 {
		http.Error(w, "invalid log filter", 400)
		return
	}
	status := q.Get("status")
	if status != "success" && status != "error" {
		status = ""
	}
	f := usageFilter{UserID: userFilter, Model: model, KeyID: keyID, AccountID: accountID, Route: route, Status: status, Since: since, Until: until, Page: page, Limit: 50}
	if logID > 0 {
		f = usageFilter{ID: logID, UserID: userFilter, Since: time.Unix(0, 0), Page: 1, Limit: 50}
		page = 1
	}
	rows, more, err := a.logs.list(r.Context(), f)
	if err != nil {
		http.Error(w, "request logs unavailable", 500)
		return
	}
	summary, err := a.logs.summary(r.Context(), f)
	if err != nil {
		http.Error(w, "usage summary unavailable", 500)
		return
	}
	recent := f
	if minute := now.Add(-time.Minute); recent.Since.Before(minute) {
		recent.Since = minute
	}
	minuteSummary, err := a.logs.summary(r.Context(), recent)
	if err != nil {
		http.Error(w, "recent log stats unavailable", 500)
		return
	}
	pageURL := func(number int) string {
		v := url.Values{}
		for key, values := range q {
			for _, value := range values {
				v.Add(key, value)
			}
		}
		v.Set("page", strconv.Itoa(number))
		return "/logs?" + v.Encode()
	}
	scopeURL := func(target string) string {
		v := url.Values{}
		for key, values := range q {
			for _, value := range values {
				v.Add(key, value)
			}
		}
		v.Set("scope", target)
		v.Del("page")
		if target == "self" {
			v.Del("user")
		}
		return "/logs?" + v.Encode()
	}
	resetURL := "/logs"
	if admin && !all {
		resetURL = "/logs?scope=self"
	}
	render(w, usageLogsTemplate, struct {
		User                                               user
		CSRF                                               string
		Admin, All                                         bool
		SelfURL, AllURL, ResetURL                          string
		Start, End                                         string
		Page, TotalPages                                   int
		LogID                                              int64
		UserFilter, Model, KeyID, AccountID, Route, Status string
		Summary, Minute                                    usageSummary
		Rows                                               []usageLog
		HasPrevious, HasNext                               bool
		PreviousURL, NextURL                               string
	}{u, p.CSRF, admin, all, scopeURL("self"), scopeURL("all"), resetURL,
		since.In(beijing).Format(logTimeLayout), until.Add(-time.Minute).In(beijing).Format(logTimeLayout),
		page, int((summary.Requests + 49) / 50), logID, userInput, model, keyID, accountID, route, status, summary, minuteSummary, rows, page > 1, more, pageURL(page - 1), pageURL(page + 1)}, a.cfg.GitHubURL)
}
