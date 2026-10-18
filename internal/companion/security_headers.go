package companion

import (
	"crypto/rand"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"
)

// requestBodyTimeout bounds how long a client may take to upload a request
// body (at most maxForwardBody). It is cleared once the body is read, so it
// never cuts off a long streaming response.
var requestBodyTimeout = 60 * time.Second

// confirmScript replaces inline onsubmit handlers, which the CSP blocks.
const confirmScript = `<script>
if (!window.__companionConfirmInstalled) {
window.__companionConfirmInstalled = true;
document.addEventListener('submit', event => {
  const message = event.target.dataset.confirm;
  if (message && !window.confirm(message)) {
    event.preventDefault();
    event.stopImmediatePropagation();
  }
}, true);
}
</script>`

// withSecurity applies browser hardening to the web UI and a body read
// deadline to every request. API responses keep CPA's headers untouched.
func withSecurity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !apiPath(r.URL.Path) {
			h := w.Header()
			h.Set("X-Frame-Options", "DENY")
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "same-origin")
			h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		}
		if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
			controller := http.NewResponseController(w)
			// Unsupported writers (tests) simply run without a deadline.
			if controller.SetReadDeadline(time.Now().Add(requestBodyTimeout)) == nil {
				r.Body = &deadlineBody{ReadCloser: r.Body, controller: controller}
			}
		}
		next.ServeHTTP(w, r)
	})
}

func apiPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1beta/") || strings.HasPrefix(path, "/v1alpha/")
}

// deadlineBody lifts the read deadline when the body is exhausted. The server
// keeps reading the connection in the background to detect client disconnects,
// and an expired deadline there would cancel the request context mid-stream.
type deadlineBody struct {
	io.ReadCloser
	controller *http.ResponseController
	cleared    bool
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && !b.cleared {
		b.cleared = true
		_ = b.controller.SetReadDeadline(time.Time{})
	}
	return n, err
}

// pageCSP allows only this response's inline scripts. Inline style attributes
// carry server-computed widths, so styles stay inline-capable; the page loads
// no external resources.
func pageCSP(nonce string) string {
	return "default-src 'none'; script-src 'nonce-" + nonce + "'; style-src 'unsafe-inline'; img-src 'self' data: https://linux.do https://*.linux.do https://cdn.ldstatic.com https://avatars.discourse-cdn.com; " +
		"connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
}

func scriptNonce() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}
