package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/security"
)

// ConsoleUser can sign in to the admin console with an email and password.
type ConsoleUser struct {
	Email    string
	Name     string
	Role     string
	Password string
}

const (
	sessionCookie = "atf_session"
	// HeaderConsole must accompany state-changing requests authenticated by the session cookie,
	// so a cross-site form post can never act on an operator's behalf (on top of SameSite=Strict).
	HeaderConsole = "X-ATF-Console"
)

type session struct {
	Email string `json:"e"`
	Name  string `json:"n"`
	Role  string `json:"r"`
	Exp   int64  `json:"x"`
}

func (s *Server) signSession(p []byte) string {
	m := hmac.New(sha256.New, s.cfg.SessionKey)
	m.Write(p)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *Server) encodeSession(ss session) string {
	p, _ := json.Marshal(ss)
	return base64.RawURLEncoding.EncodeToString(p) + "." + s.signSession(p)
}

// sessionFrom returns the signed, unexpired console session on r, if any.
func (s *Server) sessionFrom(r *http.Request) (session, bool) {
	var ss session
	c, err := r.Cookie(sessionCookie)
	if err != nil || len(s.cfg.SessionKey) == 0 {
		return ss, false
	}
	body, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return ss, false
	}
	p, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil || !hmac.Equal([]byte(sig), []byte(s.signSession(p))) {
		return ss, false
	}
	if json.Unmarshal(p, &ss) != nil || time.Now().Unix() > ss.Exp || roleRank[ss.Role] == 0 {
		return ss, false
	}
	return ss, true
}

func (s *Server) secureCookie(r *http.Request) bool {
	return r.TLS != nil || (s.cfg.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: value, Path: "/admin", MaxAge: maxAge,
		HttpOnly: true, Secure: s.secureCookie(r), SameSite: http.SameSiteStrictMode})
}

// lookupUser matches console users by email, or admin tokens by name (the token is the password).
func (s *Server) lookupUser(login, password string) (ConsoleUser, bool) {
	login = strings.ToLower(strings.TrimSpace(login))
	var found ConsoleUser
	ok := false
	for _, u := range s.cfg.ConsoleUsers {
		if strings.ToLower(u.Email) == login && security.EqualHex(hexOf(u.Password), hexOf(password)) {
			found, ok = u, true
		}
	}
	for _, a := range s.cfg.AdminTokens {
		if strings.ToLower(a.Name) == login && security.EqualHex(hexOf(a.Token), hexOf(password)) {
			found, ok = ConsoleUser{Email: a.Name, Name: a.Name, Role: a.Role}, true
		}
	}
	return found, ok
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, nil, &req); err != nil {
		s.writeErr(w, r, err)
		return
	}
	if ok, _ := s.limiter.Allow(r.Context(), "console-login:"+s.clientIP(r).String(), 3); !ok {
		fail(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in attempts; wait a moment")
		return
	}
	u, ok := s.lookupUser(req.Email, req.Password)
	if !ok || len(s.cfg.SessionKey) == 0 {
		login := strings.ToLower(strings.TrimSpace(req.Email))
		if len(login) > 80 {
			login = login[:80]
		}
		s.store.Audit(r.Context(), "console:"+login, "console.login_failed", "console", "", map[string]any{"ip": s.clientIP(r).String()})
		fail(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	ttl := s.cfg.SessionTTL
	ss := session{Email: u.Email, Name: u.Name, Role: u.Role, Exp: time.Now().Add(ttl).Unix()}
	s.setSessionCookie(w, r, s.encodeSession(ss), int(ttl.Seconds()))
	s.store.Audit(r.Context(), "console:"+u.Email, "console.login", "console", u.Email, map[string]any{"ip": s.clientIP(r).String()})
	writeJSON(w, http.StatusOK, map[string]string{"email": u.Email, "name": u.Name, "role": u.Role})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	s.setSessionCookie(w, r, "", -1)
	w.WriteHeader(http.StatusNoContent)
}

// me describes who is signed in (session or bearer token).
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	u, _ := r.Context().Value(ctxUser).(ConsoleUser)
	writeJSON(w, http.StatusOK, map[string]string{"email": u.Email, "name": u.Name, "role": u.Role})
}

func withUser(ctx context.Context, u ConsoleUser, actor string) context.Context {
	return context.WithValue(context.WithValue(ctx, ctxUser, u), ctxActor, actor)
}
