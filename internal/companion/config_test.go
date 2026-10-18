package companion

import (
	"testing"
	"time"
)

func TestStandaloneSettingsRequireCPAConnections(t *testing.T) {
	t.Setenv("GITHUB_URL", "")
	t.Setenv("PUBLIC_URL", "https://companion.example.com")
	t.Setenv("LINUXDO_CLIENT_ID", "client")
	t.Setenv("LINUXDO_CLIENT_SECRET", "secret")
	t.Setenv("CPA_BASE_URL", "https://cpa.example.com")
	t.Setenv("CPA_API_KEY", "proxy-key")
	t.Setenv("CPA_MANAGEMENT_KEY", "management-key")
	t.Setenv("CPA_MODEL_ALIAS_PREFIXES", "openai, Models")
	t.Setenv("DATA_DIR", t.TempDir())
	cfg, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CPABaseURL.String() != "https://cpa.example.com" {
		t.Fatalf("unexpected CPA base URL: %+v", cfg)
	}
	if cfg.MinTrustLevel != 2 {
		t.Fatalf("unexpected default trust level: %d", cfg.MinTrustLevel)
	}
	t.Setenv("GITHUB_URL", "https://github.com/example/project")
	if cfg, err := loadSettings(); err != nil || cfg.GitHubURL != "https://github.com/example/project" {
		t.Fatalf("configured GitHub URL: %q %v", cfg.GitHubURL, err)
	}
	for _, bad := range []string{"http://github.com/example", "javascript:alert(1)", "https://user:pass@github.com/example"} {
		t.Setenv("GITHUB_URL", bad)
		if _, err := loadSettings(); err == nil {
			t.Fatalf("invalid GitHub URL %q accepted", bad)
		}
	}
	t.Setenv("GITHUB_URL", "")
	t.Setenv("LINUXDO_MIN_TRUST_LEVEL", "2")
	if cfg, err := loadSettings(); err != nil || cfg.MinTrustLevel != 2 {
		t.Fatalf("configured trust level: %d %v", cfg.MinTrustLevel, err)
	}
	for _, bad := range []string{"0", "5", "2.0", "none"} {
		t.Setenv("LINUXDO_MIN_TRUST_LEVEL", bad)
		if _, err := loadSettings(); err == nil {
			t.Fatalf("invalid trust level %q accepted", bad)
		}
	}
	t.Setenv("LINUXDO_MIN_TRUST_LEVEL", "")
	if !cfg.ModelAliasPrefixes["openai"] || !cfg.ModelAliasPrefixes["models"] {
		t.Fatalf("OAuth model alias prefixes not normalized: %+v", cfg.ModelAliasPrefixes)
	}
	t.Setenv("CPA_BASE_URL", "http://cpa.example.com")
	if _, err := loadSettings(); err == nil {
		t.Fatal("public HTTP CPA URL accepted")
	}
	t.Setenv("CPA_BASE_URL", "https://cpa.example.com")
	t.Setenv("CPA_MODEL_ALIAS_PREFIXES", "bad/prefix")
	if _, err := loadSettings(); err == nil {
		t.Fatal("invalid OAuth model alias prefix accepted")
	}
	t.Setenv("CPA_MODEL_ALIAS_PREFIXES", "openai")
	if cfg, err := loadSettings(); err != nil || cfg.AccountCacheTTL != defaultAccountCacheTTL {
		t.Fatalf("default account cache TTL: %v %v", cfg.AccountCacheTTL, err)
	}
	t.Setenv("CPA_ACCOUNT_CACHE_TTL", "45s")
	if cfg, err := loadSettings(); err != nil || cfg.AccountCacheTTL != 45*time.Second {
		t.Fatalf("account cache TTL not applied: %v %v", cfg.AccountCacheTTL, err)
	}
	for _, bad := range []string{"15", "-1s", "31m", "soon"} {
		t.Setenv("CPA_ACCOUNT_CACHE_TTL", bad)
		if _, err := loadSettings(); err == nil {
			t.Fatalf("invalid account cache TTL %q accepted", bad)
		}
	}
	t.Setenv("CPA_ACCOUNT_CACHE_TTL", "30m")
	if cfg, err := loadSettings(); err != nil || cfg.AccountCacheTTL != 30*time.Minute {
		t.Fatalf("maximum account cache TTL rejected: %v %v", cfg.AccountCacheTTL, err)
	}
	t.Setenv("CPA_ACCOUNT_CACHE_TTL", "0s")
	t.Setenv("CPA_API_KEY", "")
	if _, err := loadSettings(); err == nil {
		t.Fatal("missing proxy API key accepted")
	}
}

func TestCPAHTTPHostsArePrivate(t *testing.T) {
	for _, tc := range []struct {
		host string
		want bool
	}{
		{"localhost", true}, {"127.0.0.1", true}, {"10.1.2.3", true},
		{"192.168.1.5", true}, {"fd00::1", true},
		{"8.8.8.8", false}, {"cpa.example.com", false}, {"mock", false},
	} {
		if got := allowedCPAHTTPHost(tc.host, false); got != tc.want {
			t.Errorf("host %q allowed=%t, want %t", tc.host, got, tc.want)
		}
	}
}
