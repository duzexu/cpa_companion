package companion

import (
	"strings"
	"testing"
)

func TestPartialNavigationIsLimitedToMarkedGetControls(t *testing.T) {
	for _, want := range []string{
		`a[data-partial-nav][href]`,
		`form[data-partial-nav]`,
		`method !== 'get'`,
		`new URLSearchParams(new FormData(form))`,
	} {
		if !strings.Contains(navigationScript, want) {
			t.Fatalf("navigation runtime missing %q", want)
		}
	}
	if strings.Contains(navigationScript, `form:not(`) {
		t.Fatal("navigation runtime must not intercept unmarked forms")
	}
	for name, page := range map[string]string{
		"audit": auditPageTemplate,
		"logs":  usageLogsTemplate,
	} {
		if !strings.Contains(page, `method="get" data-partial-nav`) {
			t.Fatalf("%s filter form is not marked for partial navigation", name)
		}
		if !strings.Contains(page, `data-partial-nav>重置</a>`) {
			t.Fatalf("%s reset link is not marked for partial navigation", name)
		}
	}
	if strings.Contains(usageLogsTemplate, `action="/admin/logs/clear" data-partial-nav`) {
		t.Fatal("log deletion must remain a native POST")
	}
}
