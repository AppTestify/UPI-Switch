package api

import (
	_ "embed"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

// console serves the single-page admin console; every action it takes goes through the RBAC-protected admin API.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Write(consoleHTML)
}
