package companion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var auditThinkingBlock = regexp.MustCompile(`(?is)<(?:think|analysis)(?:\s[^>]*)?>.*?</(?:think|analysis)\s*>`)
var auditOpenThinkingBlock = regexp.MustCompile(`(?is)<(?:think|analysis)(?:\s[^>]*)?>.*$`)

type auditSessions struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// First request of an explicit session is always audited. A request without a
// usable session ID is treated as a first request so it cannot skip sampling.
func (s *auditSessions) shouldInspect(userID string, session routeSession, percent int, now time.Time) (bool, bool) {
	if session.ID == "" {
		return true, true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = make(map[string]time.Time)
	}
	if len(s.seen) > 10_000 {
		for key, expires := range s.seen {
			if !now.Before(expires) {
				delete(s.seen, key)
			}
		}
	}
	key := userID + "\x00" + session.ID
	first := !now.Before(s.seen[key])
	s.seen[key] = now.Add(time.Hour)
	if first {
		return true, true
	}
	return rand.IntN(100) < percent, false
}

// Chat requests without a session ID still reveal whether this is the first
// user turn through their message history. Other formats cannot prove it, so
// they are treated as first requests to preserve the first-message guarantee.
func anonymousConversationHasHistory(body []byte) bool {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return false
	}
	for _, key := range []string{"messages", "contents"} {
		var messages []map[string]json.RawMessage
		if json.Unmarshal(root[key], &messages) != nil {
			continue
		}
		userTurns := 0
		for _, message := range messages {
			var role string
			_ = json.Unmarshal(message["role"], &role)
			if role == "user" {
				userTurns++
			}
		}
		return userTurns > 1
	}
	return false
}

func auditInputText(body []byte) string {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	for _, key := range []string{"messages", "input", "prompt", "contents"} {
		raw := root[key]
		if len(raw) == 0 {
			continue
		}
		var turns []string
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil {
			for _, item := range list {
				appendAuditTurn(&turns, item)
			}
		} else {
			appendAuditTurn(&turns, raw)
		}
		if len(turns) > 0 {
			return strings.Join(turns, "\n\n")
		}
	}
	return ""
}

func appendAuditTurn(turns *[]string, raw json.RawMessage) {
	var message map[string]json.RawMessage
	if json.Unmarshal(raw, &message) != nil {
		var parts []string
		appendAuditText(&parts, raw, 0)
		if text := strings.TrimSpace(strings.Join(parts, "\n")); text != "" {
			*turns = append(*turns, "用户：\n"+text)
		}
		return
	}
	var role, itemType string
	_ = json.Unmarshal(message["role"], &role)
	_ = json.Unmarshal(message["type"], &itemType)
	if itemType != "" && itemType != "message" {
		return // Responses tool calls and reasoning items are not conversation text.
	}
	label := ""
	switch role {
	case "user":
		label = "用户"
	case "assistant", "model":
		label = "助手"
	case "":
		if itemType != "" {
			return
		}
		label = "用户"
	default:
		return // System, developer and tool messages are excluded.
	}
	var parts []string
	for _, field := range []string{"content", "parts", "text"} {
		if child := message[field]; len(child) > 0 {
			appendAuditText(&parts, child, 0)
			if len(parts) > 0 {
				break
			}
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if label == "助手" {
		text = strings.TrimSpace(auditOpenThinkingBlock.ReplaceAllString(auditThinkingBlock.ReplaceAllString(text, ""), ""))
	}
	if text != "" {
		*turns = append(*turns, label+"：\n"+text)
	}
}

func appendAuditText(parts *[]string, raw json.RawMessage, depth int) {
	if depth > 16 {
		return
	}
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if value = strings.TrimSpace(value); value != "" {
			*parts = append(*parts, value)
		}
		return
	}
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		for _, item := range list {
			appendAuditText(parts, item, depth+1)
		}
		return
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return
	}
	var itemType string
	_ = json.Unmarshal(obj["type"], &itemType)
	switch itemType {
	case "", "text", "input_text", "output_text", "refusal", "message":
	default:
		return // Image, audio, thinking and tool blocks are excluded.
	}
	var thought bool
	_ = json.Unmarshal(obj["thought"], &thought)
	if thought {
		return
	}
	for _, key := range []string{"text", "input_text", "output_text", "refusal", "content", "parts"} {
		if child := obj[key]; len(child) > 0 {
			appendAuditText(parts, child, depth+1)
			return
		}
	}
}

type auditDecision struct {
	Category  string
	Score     float64
	Threshold int
	Hit       bool
	Fallback  bool
}

func decideAuditScores(body []byte, thresholds map[string]int) (auditDecision, error) {
	var response struct {
		Answers map[string]struct {
			Type string   `json:"type"`
			Noul *float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return auditDecision{}, err
	}
	best := auditDecision{}
	seen := false
	for _, category := range auditCategories {
		answer, ok := response.Answers[auditQuestionID(category.Name)]
		if !ok || answer.Type != "noul" || answer.Noul == nil || *answer.Noul < 0 || *answer.Noul > 1 {
			return auditDecision{}, fmt.Errorf("SystemOne response has invalid answer for %s", category.Name)
		}
		score := *answer.Noul * 100
		threshold := thresholds[category.Name]
		hit := score >= float64(threshold)
		if !seen || hit && !best.Hit || hit == best.Hit && score > best.Score {
			best = auditDecision{Category: category.Name, Score: score, Threshold: threshold, Hit: hit}
			seen = true
		}
	}
	return best, nil
}

func auditQuestionID(name string) string {
	return strings.NewReplacer("/", "_", "-", "_").Replace(name)
}

func keywordAudit(text string, keywords []string) auditDecision {
	lower := strings.ToLower(text)
	for _, keyword := range keywords {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			return auditDecision{Category: "keyword", Score: 100, Hit: true, Fallback: true}
		}
	}
	return auditDecision{Category: "keyword", Fallback: true}
}

func (a *app) systemOneDecision(ctx context.Context, endpoint auditEndpoint, thresholds map[string]int, input string) (auditDecision, error) {
	endpointURL := endpoint.BaseURL
	if endpointURL == "" {
		return auditDecision{}, errors.New("audit endpoint URL is required")
	}
	model := endpoint.Model
	if model == "" {
		model = defaultSystemOneModel
	}
	key, err := openAPIKey(a.cfg.CookieKey, "systemone", "audit", endpoint.APIKeySealed)
	if err != nil || key == "" {
		return auditDecision{}, errors.New("AI Gateway API key unavailable")
	}
	type question struct {
		Type         string            `json:"type"`
		Instructions string            `json:"instructions"`
		Criteria     map[string]string `json:"criteria,omitempty"`
	}
	questions := make(map[string]question, len(auditCategories))
	for _, category := range auditCategories {
		questions[auditQuestionID(category.Name)] = question{Type: "noul", Instructions: auditQuestionInstruction(category.Question)}
	}
	parsedURL, err := url.Parse(endpointURL)
	if err != nil {
		return auditDecision{}, err
	}
	decisionsAPI := strings.HasSuffix(strings.TrimRight(parsedURL.Path, "/"), "/v1/decisions")
	var state any = input
	if decisionsAPI {
		state = map[string]string{"task": "内容审计", "description": input}
		for _, category := range auditCategories {
			q := questions[auditQuestionID(category.Name)]
			q.Criteria = map[string]string{
				"true":  category.CriteriaTrue,
				"false": category.CriteriaFalse,
			}
			questions[auditQuestionID(category.Name)] = q
		}
	}
	payload, err := json.Marshal(struct {
		Model     string              `json:"model"`
		State     any                 `json:"state"`
		Questions map[string]question `json:"questions"`
	}{Model: model, State: state, Questions: questions})
	if err != nil {
		return auditDecision{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpointURL, bytes.NewReader(payload))
	if err != nil {
		return auditDecision{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return auditDecision{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return auditDecision{}, fmt.Errorf("SystemOne returned HTTP %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(data) > 1<<20 {
		return auditDecision{}, errors.New("SystemOne response unavailable or too large")
	}
	return decideAuditScores(data, thresholds)
}

func (a *app) inspectContent(ctx context.Context, header http.Header, body []byte, usage *usageLog) error {
	cfg := a.db.auditSettings()
	if !cfg.Enabled || a.logs == nil {
		return nil
	}
	input := auditInputText(body)
	if input == "" {
		return nil
	}
	session := extractRouteSession(header, body)
	inspect, first := a.auditSessions.shouldInspect(usage.UserID, session, cfg.SamplePercent, time.Now())
	if session.ID == "" && anonymousConversationHasHistory(body) {
		inspect, first = rand.IntN(100) < cfg.SamplePercent, false
	}
	if !inspect {
		return nil
	}
	started := time.Now()
	var decision auditDecision
	var selected string
	attempts := 0
	for _, endpoint := range cfg.Endpoints {
		if ctx.Err() != nil {
			break
		}
		attempts++
		decisionCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMS)*time.Millisecond)
		result, err := a.systemOneDecision(decisionCtx, endpoint, cfg.Thresholds, input)
		cancel()
		if err == nil {
			decision, selected = result, endpoint.Name
			break
		}
		log.Printf("SystemOne audit endpoint %q failed: %v", endpoint.Name, err)
	}
	if selected == "" {
		decision = keywordAudit(input, cfg.Keywords)
	}
	record := auditRecord{At: started.UTC(), UserID: usage.UserID, Username: usage.Username, KeyID: usage.KeyID, PlanID: usage.PlanID,
		Model: usage.Model, Route: usage.Route, FirstInSession: first, Hit: decision.Hit,
		Fallback: decision.Fallback, Category: decision.Category, ScorePercent: decision.Score, EndpointName: selected, AttemptCount: attempts,
		ThresholdPercent: decision.Threshold, DurationMS: int(time.Since(started).Milliseconds())}
	if decision.Hit {
		record.InputText = input
	}
	writeCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
	defer done()
	id, err := a.logs.insertAudit(writeCtx, record)
	if err != nil {
		return fmt.Errorf("save content audit record: %w", err)
	}
	usage.AuditIDs = append(usage.AuditIDs, id)
	return nil
}

func (a *app) recordUpstreamCyberPolicy(body []byte, usage *usageLog, code string) error {
	if a.logs == nil {
		return nil
	}
	input := auditInputText(body)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := a.logs.insertAudit(ctx, auditRecord{
		At: time.Now().UTC(), UserID: usage.UserID, Username: usage.Username, KeyID: usage.KeyID,
		PlanID: usage.PlanID, Model: usage.Model, Route: usage.Route,
		Hit: true, Category: code, InputText: input,
	})
	if err == nil {
		usage.AuditIDs = append(usage.AuditIDs, id)
	}
	return err
}
