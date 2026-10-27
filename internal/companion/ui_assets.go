package companion

import (
	_ "embed"
	"net/http"
)

// All page styles are loaded once. Navigation replaces page content without
// removing and reinserting styles, so controls keep their appearance in place.
const stylesheetLink = `<link rel="stylesheet" href="/assets/app.css"><link rel="icon" type="image/png" sizes="256x256" href="/assets/logo.png"><link rel="apple-touch-icon" href="/assets/logo.png">`

//go:embed assets/app.css
var appCSS []byte

func serveAppCSS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(appCSS)
}
