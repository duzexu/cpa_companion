package companion

import (
	"errors"
	"strings"
)

type auditCategory struct {
	Name     string
	Default  int
	Question string
}

var auditCategories = []auditCategory{
	{"harassment", 98, "Does the user input harass, insult, or bully a person?"},
	{"harassment/threatening", 90, "Does the user input threaten or intimidate a person?"},
	{"hate", 65, "Does the user input express hatred or abuse toward a protected group?"},
	{"hate/threatening", 65, "Does the user input threaten a protected group with harm?"},
	{"illicit", 95, "Does the user input request help committing a crime or other illicit act?"},
	{"illicit/violent", 95, "Does the user input request help committing a violent crime?"},
	{"self-harm", 65, "Does the user input concern self-harm or suicide?"},
	{"self-harm/intent", 85, "Does the user input indicate an intent to self-harm or die by suicide?"},
	{"self-harm/instructions", 65, "Does the user input ask for instructions to self-harm or die by suicide?"},
	{"sexual", 65, "Does the user input request or describe explicit sexual content?"},
	{"sexual/minors", 65, "Does the user input sexualize a minor?"},
	{"violence", 95, "Does the user input request or describe violence toward a person or animal?"},
	{"violence/graphic", 95, "Does the user input request or describe graphic violence or gore?"},
}

const defaultSystemOneBaseURL = "https://ai-gateway.vercel.sh/typesafe/v1"
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
	c := auditConfig{Endpoints: []auditEndpoint{{ID: "primary", Name: "主接口", BaseURL: defaultSystemOneBaseURL, Model: defaultSystemOneModel}}, SamplePercent: 10, TimeoutMS: 2500, Thresholds: make(map[string]int, len(auditCategories))}
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
