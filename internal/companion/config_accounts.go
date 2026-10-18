package companion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// configAccountGroups contains only routing metadata; upstream API keys are discarded.
type configAccountGroups map[string][]struct {
	Name      string `json:"name"`
	Prefix    string `json:"prefix"`
	Priority  *int   `json:"priority"`
	AuthIndex string `json:"auth_index"`
	Disabled  bool   `json:"disabled"`
	Keys      []struct {
		Prefix    *string `json:"prefix"`
		Priority  *int    `json:"priority"`
		AuthIndex string  `json:"auth_index"`
	} `json:"keys"`
}

func (a *app) configAccounts(ctx context.Context, client *http.Client, globalIDs, aliasPrefixes map[string]bool) ([]credential, error) {
	raw, err := remoteGet(ctx, client, a.cfg, "/v8/management/config/api-keys", remoteFileLimit)
	if err != nil {
		return nil, fmt.Errorf("read CPA provider accounts: %w", err)
	}
	var groups configAccountGroups
	if err := json.Unmarshal(raw, &groups); err != nil || groups == nil {
		return nil, errors.New("invalid CPA provider account list")
	}
	providers := make([]string, 0, len(groups))
	for provider := range groups {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	accounts := make([]credential, 0)
	for _, provider := range providers {
		for groupNumber, group := range groups[provider] {
			if group.Disabled {
				continue
			}
			add := func(keyNumber int, authIndex, prefix string, priority *int) {
				label := strings.TrimSpace(group.Name)
				if label == "" {
					label = fmt.Sprintf("配置组 %d", groupNumber+1)
				}
				if keyNumber >= 0 && len(group.Keys) > 1 {
					label = fmt.Sprintf("%s · 账号 %d", label, keyNumber+1)
				}
				account := credential{
					ID:        "config:" + provider + ":" + strings.TrimSpace(prefix),
					AuthIndex: authIndex,
					Name:      label,
					Label:     label,
					Provider:  provider,
					Prefix:    strings.TrimSpace(prefix),
					Priority:  priority,
					Source:    "config",
					Models:    globalIDs,
				}
				if account.Prefix == "" {
					account.ID = fmt.Sprintf("config:%s:group-%d:key-%d", provider, groupNumber, keyNumber)
				}
				if !safePrefix.MatchString(account.Prefix) {
					account.Disabled = true
					account.Status = "未设置有效前缀"
				} else if aliasPrefixes[strings.ToLower(account.Prefix)] {
					account.Disabled = true
					account.Status = "前缀与模型别名冲突"
				} else {
					routable := false
					for model := range globalIDs {
						if strings.HasPrefix(model, account.Prefix+"/") {
							routable = true
							break
						}
					}
					if !routable {
						account.Disabled = true
						account.Status = "模型列表中未找到该前缀"
					}
				}
				accounts = append(accounts, account)
			}
			if len(group.Keys) == 0 {
				add(-1, group.AuthIndex, group.Prefix, group.Priority)
				if group.AuthIndex == "" {
					accounts[len(accounts)-1].Disabled = true
					accounts[len(accounts)-1].Status = "无法确认无密钥配置账号"
				}
				continue
			}
			for keyNumber, key := range group.Keys {
				prefix := group.Prefix
				if key.Prefix != nil {
					prefix = *key.Prefix
				}
				priority := group.Priority
				if key.Priority != nil {
					priority = key.Priority
				}
				add(keyNumber, key.AuthIndex, prefix, priority)
			}
		}
	}
	return accounts, nil
}
