package companion

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type settings struct {
	PublicURL          *url.URL
	GitHubURL          string
	Listen             string
	DataDir            string
	CPABaseURL         *url.URL
	CPAManagementKey   string
	CPAAPIKey          string
	ModelAliasPrefixes map[string]bool
	AccountCacheTTL    time.Duration
	ClientID           string
	ClientSecret       string
	AdminSubjects      map[string]bool
	CookieKey          string
	CookieSecure       bool
	OAuthAuthorize     string
	OAuthToken         string
	OAuthUserInfo      string
	MinTrustLevel      int
}

func envRequired(name string) (string, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func loadSettings() (settings, error) {
	var c settings
	var err error
	for _, item := range []struct {
		name string
		dest *string
	}{
		{"LINUXDO_CLIENT_ID", &c.ClientID}, {"LINUXDO_CLIENT_SECRET", &c.ClientSecret},
		{"CPA_API_KEY", &c.CPAAPIKey}, {"CPA_MANAGEMENT_KEY", &c.CPAManagementKey},
	} {
		*item.dest, err = envRequired(item.name)
		if err != nil {
			return c, err
		}
	}
	rawPublic, err := envRequired("PUBLIC_URL")
	if err != nil {
		return c, err
	}
	c.PublicURL, err = url.Parse(rawPublic)
	if err != nil || c.PublicURL.Host == "" || c.PublicURL.User != nil || (c.PublicURL.Scheme != "https" && c.PublicURL.Scheme != "http") ||
		(c.PublicURL.Path != "" && c.PublicURL.Path != "/") || c.PublicURL.RawQuery != "" || c.PublicURL.Fragment != "" {
		return c, errors.New("PUBLIC_URL must be an absolute http(s) root URL")
	}
	c.PublicURL.Path = ""
	if c.PublicURL.Scheme == "http" && c.PublicURL.Hostname() != "localhost" && c.PublicURL.Hostname() != "127.0.0.1" {
		return c, errors.New("PUBLIC_URL must use HTTPS outside localhost")
	}
	c.CookieSecure = c.PublicURL.Scheme == "https"
	if raw := strings.TrimSpace(os.Getenv("GITHUB_URL")); raw != "" {
		github, parseErr := url.Parse(raw)
		if parseErr != nil || github.Scheme != "https" || github.Hostname() == "" || github.User != nil {
			return c, errors.New("GITHUB_URL must be an absolute HTTPS URL")
		}
		c.GitHubURL = github.String()
	}
	c.Listen = os.Getenv("LISTEN_ADDR")
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return c, errors.New("invalid LISTEN_ADDR")
	}
	c.DataDir = os.Getenv("DATA_DIR")
	if c.DataDir == "" {
		c.DataDir = "./data"
	}
	c.DataDir, err = filepath.Abs(c.DataDir)
	if err != nil {
		return c, err
	}
	localMock := os.Getenv("LOCAL_TEST_MODE") == "1" && (c.PublicURL.Hostname() == "localhost" || c.PublicURL.Hostname() == "127.0.0.1")
	rawBase, err := envRequired("CPA_BASE_URL")
	if err != nil {
		return c, err
	}
	c.CPABaseURL, err = parseCPAURL(rawBase, localMock)
	if err != nil {
		return c, fmt.Errorf("CPA_BASE_URL: %w", err)
	}
	c.ModelAliasPrefixes = make(map[string]bool)
	for _, raw := range strings.Split(os.Getenv("CPA_MODEL_ALIAS_PREFIXES"), ",") {
		alias := strings.TrimSpace(raw)
		if alias == "" {
			continue
		}
		if !safePrefix.MatchString(alias) {
			return c, fmt.Errorf("CPA_MODEL_ALIAS_PREFIXES contains invalid prefix %q", alias)
		}
		c.ModelAliasPrefixes[strings.ToLower(alias)] = true
	}
	c.AccountCacheTTL = defaultAccountCacheTTL
	if raw := strings.TrimSpace(os.Getenv("CPA_ACCOUNT_CACHE_TTL")); raw != "" {
		c.AccountCacheTTL, err = time.ParseDuration(raw)
		if err != nil || c.AccountCacheTTL < 0 || c.AccountCacheTTL > maxAccountCacheTTL {
			return c, fmt.Errorf("CPA_ACCOUNT_CACHE_TTL must be a duration between 0s and %s", maxAccountCacheTTL)
		}
	}
	c.AdminSubjects = map[string]bool{}
	for _, id := range strings.Split(os.Getenv("ADMIN_SUBJECTS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			c.AdminSubjects[id] = true
		}
	}
	c.OAuthAuthorize = "https://connect.linux.do/oauth2/authorize"
	c.OAuthToken = "https://connect.linux.do/oauth2/token"
	c.OAuthUserInfo = "https://connect.linux.do/api/user"
	c.MinTrustLevel = 2
	if raw := strings.TrimSpace(os.Getenv("LINUXDO_MIN_TRUST_LEVEL")); raw != "" {
		if len(raw) != 1 || raw[0] < '1' || raw[0] > '4' {
			return c, errors.New("LINUXDO_MIN_TRUST_LEVEL must be an integer from 1 to 4")
		}
		c.MinTrustLevel = int(raw[0] - '0')
	}
	if os.Getenv("LOCAL_TEST_MODE") == "1" {
		if !localMock {
			return c, errors.New("LOCAL_TEST_MODE requires a loopback PUBLIC_URL")
		}
		for _, item := range []struct {
			name string
			dest *string
		}{
			{"LOCAL_OAUTH_AUTHORIZE_URL", &c.OAuthAuthorize},
			{"LOCAL_OAUTH_TOKEN_URL", &c.OAuthToken},
			{"LOCAL_OAUTH_USERINFO_URL", &c.OAuthUserInfo},
		} {
			*item.dest, err = envRequired(item.name)
			if err != nil {
				return c, err
			}
			u, parseErr := url.Parse(*item.dest)
			if parseErr != nil || u.Host == "" || u.Scheme != "http" {
				return c, fmt.Errorf("%s must be an absolute local http URL", item.name)
			}
		}
	}
	return c, nil
}

func parseCPAURL(raw string, localMock bool) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if err != nil || u == nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("must be a root URL without credentials, path, query or fragment")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && allowedCPAHTTPHost(u.Hostname(), localMock)) {
		return nil, errors.New("must use HTTPS or private-network HTTP")
	}
	return u, nil
}

func allowedCPAHTTPHost(host string, localMock bool) bool {
	if host == "localhost" || localMock && host == "mock" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func secretFile(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(strings.TrimSpace(string(b))) >= 64 {
			return strings.TrimSpace(string(b)), nil
		}
		return "", fmt.Errorf("secret file %s is invalid", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	s, err := randomHex(32)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		return "", err
	}
	return s, nil
}

func (c *settings) initializeSecrets() error {
	if err := os.MkdirAll(c.DataDir, 0700); err != nil {
		return err
	}
	var err error
	c.CookieKey, err = secretFile(filepath.Join(c.DataDir, "cookie.key"))
	return err
}
