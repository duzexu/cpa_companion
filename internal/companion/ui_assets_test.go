package companion

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSharedStylesheetLoadsUnderPageCSP(t *testing.T) {
	page := httptest.NewRecorder()
	render(page, loginTemplate, nil)
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), stylesheetLink) {
		t.Fatalf("login page stylesheet: status=%d", page.Code)
	}
	if !strings.Contains(page.Body.String(), `rel="icon" type="image/png"`) {
		t.Fatal("login page has no browser icon")
	}
	if !strings.Contains(page.Header().Get("Content-Security-Policy"), "style-src 'self'") {
		t.Fatal("page CSP does not allow the shared stylesheet")
	}

	style := httptest.NewRecorder()
	serveAppCSS(style, httptest.NewRequest(http.MethodGet, "/assets/app.css", nil))
	if style.Code != http.StatusOK || !strings.HasPrefix(style.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("stylesheet response: status=%d content-type=%q", style.Code, style.Header().Get("Content-Type"))
	}
	for _, selector := range []string{".sidebar", ".plan-dialog", ".dash-chart", ".audit-dialog", ".price-dialog", ".log-dialog"} {
		if !strings.Contains(style.Body.String(), selector) {
			t.Errorf("stylesheet missing %s", selector)
		}
	}
}

func TestLogClearPagesLoadSubmitConfirmation(t *testing.T) {
	for _, page := range []struct {
		name, source, action string
	}{
		{"audit", auditPageTemplate, `/admin/audit/clear`},
		{"usage", usageLogsTemplate, `/admin/logs/clear`},
	} {
		if !strings.Contains(page.source, `action="`+page.action+`" data-confirm=`) ||
			!strings.Contains(page.source, `window.__companionRegister('confirm'`) {
			t.Errorf("%s clear form is missing its confirmation handler", page.name)
		}
	}
}
