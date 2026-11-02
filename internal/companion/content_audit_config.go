package companion

import (
	"errors"
	"strings"
)

type auditCategory struct {
	Name          string
	Default       int
	Question      string
	CriteriaTrue  string
	CriteriaFalse string
}

// Apply the category's definition to user-authored content, using both sides
// of the conversation to resolve meaning without attributing assistant text
// or quoted material to the user.
const auditUserIntentInstruction = "Use the conversation to interpret the user's actual intent. Judge user messages (用户：), using assistant messages (助手：) only as context. Quoting or discussing an act does not by itself establish a request to perform it. Answer true only when the evidence satisfies the following condition; otherwise answer false. "

func auditQuestionInstruction(question string) string {
	return auditUserIntentInstruction + question
}

func auditQuestionCriteria(category auditCategory) map[string]string {
	return map[string]string{
		"true":  "The user's actual request or assertion, interpreted in context, provides clear evidence for this condition: " + category.CriteriaTrue,
		"false": "The user's actual request or assertion does not establish the condition, or the evidence is absent or ambiguous. " + category.CriteriaFalse,
	}
}

var auditCategories = []auditCategory{
	{"harassment", 98, "Does the user's own message insult, humiliate, or bully an identifiable person?", "The user's own message targets a person with insults, humiliation, or bullying.", "The user's own message does not target a person with insults, humiliation, or bullying."},
	{"harassment/threatening", 90, "Does the user's own message threaten or intimidate an identifiable person?", "The user's own message threatens or intimidates a person with harm or coercion.", "The user's own message contains no threat or intimidation toward a person."},
	{"hate", 65, "Does the user's own message express hatred, dehumanization, or abuse toward a protected group?", "The user's own message attacks or dehumanizes people because of a protected characteristic.", "The user's own message does not express hatred or abuse toward a protected group."},
	{"hate/threatening", 65, "Does the user's own message threaten a protected group with harm?", "The user's own message threatens harm against people because of a protected characteristic.", "The user's own message contains no threat of harm toward a protected group."},
	{"illicit", 95, "Does the user request actionable assistance to carry out or conceal an illicit act? Both the illicit objective and the request for assistance must be established by the conversation.", "The user requests actionable assistance to commit or conceal an illicit act, with evidence of both the illicit objective and the requested assistance.", "The user's own message does not request actionable help with a crime or other illicit act."},
	{"illicit/violent", 95, "Does the user's own message request help planning or committing a violent crime?", "The user's own message asks for actionable help to plan, carry out, or conceal violence or a violent crime.", "The user's own message does not request actionable help for a violent crime."},
	{"self-harm", 65, "Does the user's own message concern self-harm or suicide, including support-seeking or discussion of the topic?", "The user's own message concerns self-harm or suicide, even if it does not express intent or request instructions.", "The user's own message does not concern self-harm or suicide."},
	{"self-harm/intent", 85, "Does the user's own message express personal intent, desire, or a plan to self-harm or die by suicide?", "The user's own message expresses personal intent, desire, or a plan to self-harm or die by suicide.", "The user's own message does not express personal intent, desire, or a plan to self-harm or die by suicide."},
	{"self-harm/instructions", 65, "Does the user request actionable methods or assistance for self-injury or suicide? Both the self-harm objective and the request for implementation guidance must be established by the conversation.", "The user requests actionable methods or implementation assistance for self-injury or suicide, with evidence of both the self-harm objective and the requested guidance.", "The user's own message does not request instructions or methods for self-harm or suicide."},
	{"sexual", 65, "Does the user's own message request or describe explicit sexual content?", "The user's own message requests or describes explicit sexual acts or sexual content.", "The user's own message does not request or describe explicit sexual content."},
	{"sexual/minors", 65, "Does the user's own message sexualize, exploit, or request sexual content involving a minor?", "The user's own message sexualizes, exploits, or requests sexual content involving a minor.", "The user's own message does not sexualize, exploit, or request sexual content involving a minor."},
	{"violence", 95, "Does the user's own message request or describe violence toward a person or animal?", "The user's own message requests, encourages, or describes violence toward a person or animal.", "The user's own message does not request, encourage, or describe violence toward a person or animal."},
	{"violence/graphic", 95, "Does the user's own message request or describe graphic violence or gore?", "The user's own message requests or describes graphic violence, gore, mutilation, or extreme injury.", "The user's own message does not request or describe graphic violence or gore."},
}

const defaultSystemOneURL = "https://ai-gateway.vercel.sh/typesafe/v1/systemone"
const defaultSystemOneModel = "jev-latest"

type auditConfig struct {
	Enabled       bool            `json:"enabled"`
	Endpoints     []auditEndpoint `json:"endpoints"`
	SamplePercent int             `json:"sample_percent"`
	TimeoutMS     int             `json:"timeout_ms"`
	Thresholds    map[string]int  `json:"thresholds"`
	Keywords      []string        `json:"keywords,omitempty"`
}

type auditEndpoint struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BaseURL      string `json:"base_url"`
	Model        string `json:"model"`
	APIKeySealed string `json:"api_key_sealed"`
}

func defaultAuditConfig() auditConfig {
	c := auditConfig{Endpoints: []auditEndpoint{{ID: "primary", Name: "主接口", BaseURL: defaultSystemOneURL, Model: defaultSystemOneModel}}, SamplePercent: 10, TimeoutMS: 2500, Thresholds: make(map[string]int, len(auditCategories))}
	for _, category := range auditCategories {
		c.Thresholds[category.Name] = category.Default
	}
	return c
}

func (c auditConfig) clone() auditConfig {
	c.Endpoints = append([]auditEndpoint(nil), c.Endpoints...)
	c.Keywords = append([]string(nil), c.Keywords...)
	thresholds := make(map[string]int, len(c.Thresholds))
	for name, value := range c.Thresholds {
		thresholds[name] = value
	}
	c.Thresholds = thresholds
	return c
}

func (s *store) auditSettings() auditConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db.Audit == nil {
		return defaultAuditConfig()
	}
	return s.db.Audit.clone()
}

func (s *store) saveAuditSettings(c auditConfig) error {
	if len(c.Endpoints) == 0 || len(c.Endpoints) > 5 {
		return errors.New("one to five audit endpoints are required")
	}
	seenIDs := map[string]bool{}
	for _, endpoint := range c.Endpoints {
		if endpoint.ID == "" || seenIDs[endpoint.ID] || endpoint.Name == "" || len(endpoint.Name) > 64 ||
			endpoint.BaseURL == "" || endpoint.Model == "" || len(endpoint.Model) > 128 || strings.ContainsAny(endpoint.Model, " \t\r\n") {
			return errors.New("invalid audit endpoint")
		}
		seenIDs[endpoint.ID] = true
		if c.Enabled && endpoint.APIKeySealed == "" {
			return errors.New("audit endpoint API keys are required")
		}
	}
	if c.SamplePercent < 0 || c.SamplePercent > 100 || c.TimeoutMS < 500 || c.TimeoutMS > 10000 {
		return errors.New("invalid audit sampling or timeout")
	}
	if c.Enabled && len(c.Keywords) == 0 {
		return errors.New("fallback keywords are required when audit is enabled")
	}
	if len(c.Thresholds) != len(auditCategories) || len(c.Keywords) > 100 {
		return errors.New("invalid audit thresholds or keywords")
	}
	for _, category := range auditCategories {
		v, ok := c.Thresholds[category.Name]
		if !ok || v < 0 || v > 100 {
			return errors.New("invalid audit threshold")
		}
	}
	for _, keyword := range c.Keywords {
		if strings.TrimSpace(keyword) == "" || len(keyword) > 80 {
			return errors.New("invalid audit keyword")
		}
	}
	copy := c.clone()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.db.Audit = &copy
	return s.persistLocked()
}
