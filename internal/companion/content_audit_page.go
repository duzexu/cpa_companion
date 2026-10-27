package companion

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type auditCategoryView struct {
	Name, Default string
	Value         int
}

type auditEndpointView struct {
	Index                    int
	ID, Name, BaseURL, Model string
	KeyConfigured            bool
}

type auditPageData struct {
	User           user
	CSRF           string
	Config         auditConfig
	Categories     []auditCategoryView
	Rows           []auditRecord
	LinkedLogs     []usageLog
	LinkedLogIDs   map[int64]bool
	Summary        auditSummary
	Endpoints      []auditEndpointView
	KeywordsText   string
	UserFilter     string
	ResultFilter   string
	Page           int
	PreviousURL    string
	NextURL        string
	HasNext        bool
	Saved, Cleared bool
}

func (a *app) auditPage(w http.ResponseWriter, r *http.Request) {
	u, session, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if a.logs == nil {
		http.Error(w, "audit database unavailable", http.StatusServiceUnavailable)
		return
	}
	query := r.URL.Query()
	page, _ := strconv.Atoi(query.Get("page"))
	if page < 1 {
		page = 1
	}
	userFilter := strings.TrimSpace(query.Get("user"))
	resultFilter := query.Get("result")
	rows, hasNext, err := a.logs.auditRecords(r.Context(), userFilter, resultFilter, page)
	if err != nil {
		http.Error(w, "audit records unavailable", http.StatusInternalServerError)
		return
	}
	logIDs := make([]int64, 0, len(rows))
	seenLogIDs := make(map[int64]bool, len(rows))
	for _, row := range rows {
		if row.UsageLogID > 0 && !seenLogIDs[row.UsageLogID] {
			seenLogIDs[row.UsageLogID] = true
			logIDs = append(logIDs, row.UsageLogID)
		}
	}
	linkedLogs, err := a.logs.logsByIDs(r.Context(), logIDs)
	if err != nil {
		http.Error(w, "linked request logs unavailable", http.StatusInternalServerError)
		return
	}
	linkedLogIDs := make(map[int64]bool, len(linkedLogs))
	for _, entry := range linkedLogs {
		linkedLogIDs[entry.ID] = true
	}
	summary, err := a.logs.auditStats(r.Context())
	if err != nil {
		http.Error(w, "audit summary unavailable", http.StatusInternalServerError)
		return
	}
	cfg := a.db.auditSettings()
	endpoints := make([]auditEndpointView, 0, len(cfg.Endpoints))
	for i, endpoint := range cfg.Endpoints {
		endpoints = append(endpoints, auditEndpointView{i, endpoint.ID, endpoint.Name, endpoint.BaseURL, endpoint.Model, endpoint.APIKeySealed != ""})
	}
	categories := make([]auditCategoryView, 0, len(auditCategories))
	for _, item := range auditCategories {
		categories = append(categories, auditCategoryView{item.Name, strconv.Itoa(item.Default), cfg.Thresholds[item.Name]})
	}
	pageURL := func(number int) string {
		q := url.Values{}
		if userFilter != "" {
			q.Set("user", userFilter)
		}
		if resultFilter != "" {
			q.Set("result", resultFilter)
		}
		q.Set("page", strconv.Itoa(number))
		return "/audit?" + q.Encode()
	}
	render(w, auditPageTemplate, auditPageData{User: u, CSRF: session.CSRF, Config: cfg,
		Categories: categories, Rows: rows, LinkedLogs: linkedLogs, LinkedLogIDs: linkedLogIDs, Summary: summary, Endpoints: endpoints,
		KeywordsText: strings.Join(cfg.Keywords, "\n"), UserFilter: userFilter, ResultFilter: resultFilter,
		Page: page, PreviousURL: pageURL(page - 1), NextURL: pageURL(page + 1), HasNext: hasNext,
		Saved: query.Get("saved") == "1", Cleared: query.Get("cleared") == "1"}, a.cfg.GitHubURL)
}

func (a *app) saveAuditSettings(w http.ResponseWriter, r *http.Request) {
	u, session, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if !checkCSRF(r, session) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	current := a.db.auditSettings()
	cfg := current.clone()
	cfg.Enabled = r.PostForm.Get("enabled") == "on"
	cfg.SamplePercent, _ = strconv.Atoi(r.PostForm.Get("sample_percent"))
	cfg.TimeoutMS, _ = strconv.Atoi(r.PostForm.Get("timeout_ms"))
	count, err := strconv.Atoi(r.PostForm.Get("endpoint_count"))
	if err != nil || count < 1 || count > 5 {
		adminMutationError(w, r, "请配置 1 到 5 个审计接口", 400)
		return
	}
	currentByID := make(map[string]auditEndpoint, len(current.Endpoints))
	for _, endpoint := range current.Endpoints {
		currentByID[endpoint.ID] = endpoint
	}
	cfg.Endpoints = make([]auditEndpoint, 0, count)
	usedIDs := map[string]bool{}
	for i := 0; i < count; i++ {
		prefix := "endpoint_" + strconv.Itoa(i) + "_"
		id := r.PostForm.Get(prefix + "id")
		old, exists := currentByID[id]
		if !exists || usedIDs[id] {
			id, err = randomHex(8)
			if err != nil {
				adminMutationError(w, r, "无法创建审计接口 ID", 500)
				return
			}
			old = auditEndpoint{ID: id}
		}
		usedIDs[id] = true
		endpoint := old
		endpoint.Name = strings.TrimSpace(r.PostForm.Get(prefix + "name"))
		endpoint.Model = strings.TrimSpace(r.PostForm.Get(prefix + "model"))
		baseURL := strings.TrimSpace(r.PostForm.Get(prefix + "base_url"))
		parsed, parseErr := url.Parse(strings.TrimRight(baseURL, "/"))
		if parseErr != nil || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" || parsed.Path == "" || parsed.Path == "/" ||
			parsed.Scheme != "https" && !(parsed.Scheme == "http" && allowedCPAHTTPHost(parsed.Hostname(), a.cfg.PublicURL != nil && a.cfg.PublicURL.Hostname() == "localhost")) ||
			parsed.Opaque != "" {
			adminMutationError(w, r, "请填写包含完整路径的 HTTPS 或内网 HTTP 审计接口地址", 400)
			return
		}
		endpoint.BaseURL = baseURL
		if r.PostForm.Get(prefix+"clear_key") == "on" {
			endpoint.APIKeySealed = ""
		}
		if key := strings.TrimSpace(r.PostForm.Get(prefix + "api_key")); key != "" {
			endpoint.APIKeySealed, err = sealAPIKey(a.cfg.CookieKey, "systemone", "audit", key)
			if err != nil {
				adminMutationError(w, r, "无法加密审计 API Key", 500)
				return
			}
		}
		cfg.Endpoints = append(cfg.Endpoints, endpoint)
	}
	cfg.Thresholds = make(map[string]int, len(auditCategories))
	for _, category := range auditCategories {
		value, err := strconv.Atoi(r.PostForm.Get("threshold_" + category.Name))
		if err != nil || value < 0 || value > 100 {
			adminMutationError(w, r, "风险阈值须为 0 到 100 的整数百分比", 400)
			return
		}
		cfg.Thresholds[category.Name] = value
	}
	seen := map[string]bool{}
	cfg.Keywords = nil
	for _, line := range strings.Split(r.PostForm.Get("keywords"), "\n") {
		word := strings.TrimSpace(line)
		if word == "" || seen[strings.ToLower(word)] {
			continue
		}
		seen[strings.ToLower(word)] = true
		cfg.Keywords = append(cfg.Keywords, word)
	}
	if err := a.db.saveAuditSettings(cfg); err != nil {
		adminMutationError(w, r, "审计配置无效：请检查接口名称、模型、密钥、回退关键词及抽查设置", 400)
		return
	}
	if wantsAdminJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"message":"审计设置已保存"}`))
		return
	}
	http.Redirect(w, r, "/audit?saved=1", http.StatusSeeOther)
}
