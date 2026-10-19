package companion

import (
	"bytes"
	_ "embed"
	"net/http"
	"time"
)

//go:embed assets/logo.png
var brandLogoPNG []byte

func serveBrandLogo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeContent(w, r, "logo.png", time.Time{}, bytes.NewReader(brandLogoPNG))
}
