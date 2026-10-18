package api

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

// consoleCSP allows only the console's own inline script, by hash, so injected markup can't run script.
var consoleCSP = func() string {
	_, rest, _ := bytes.Cut(consoleHTML, []byte("<script>"))
	script, _, _ := bytes.Cut(rest, []byte("</script>"))
	sum := sha256.Sum256(script)
	return "default-src 'self'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'; style-src 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
}()

// console serves the single-page admin console; every action it takes goes through the RBAC-protected admin API.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", consoleCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Write(consoleHTML)
}
