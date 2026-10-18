package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
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
	ID    string `json:"i"`
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
	if json.Unmarshal(p, &ss) != nil || time.Now().Unix() > ss.Exp || roleRank[ss.Role] == 0 || ss.ID == "" {
		return ss, false
	}
	// The cookie only proves who signed in; the server-side record decides whether the session is still live
	// (signed out, idle too long), and the user must still exist with the same role.
	if _, err := s.store.TouchSession(r.Context(), ss.ID, s.sessionIdle()); err != nil {
		return ss, false
	}
	if u, ok := s.userByLogin(ss.Email); !ok || u.Role != ss.Role {
		_ = s.store.RevokeSession(r.Context(), ss.ID)
		return ss, false
	}
	return ss, true
}

func (s *Server) sessionIdle() time.Duration {
	if s.cfg.SessionIdle > 0 {
		return s.cfg.SessionIdle
	}
	return 30 * time.Minute
}

// userByLogin finds a configured console user (or admin token holder) without checking a password.
func (s *Server) userByLogin(login string) (ConsoleUser, bool) {
	login = strings.ToLower(login)
	for _, u := range s.cfg.ConsoleUsers {
		if strings.ToLower(u.Email) == login {
			return u, true
		}
	}
	for _, a := range s.cfg.AdminTokens {
		if strings.ToLower(a.Name) == login {
			return ConsoleUser{Email: a.Name, Name: a.Name, Role: a.Role}, true
		}
	}
	return ConsoleUser{}, false
}

// Sign-in lockout: this many failures within lockFor locks the account until lockFor has passed since the last one.
const (
	maxLoginFailures = 5
	lockFor          = 15 * time.Minute
)

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
	login := strings.ToLower(strings.TrimSpace(req.Email))
	if len(login) > 80 {
		login = login[:80]
	}
	if n, last, err := s.store.RecentLoginFailures(r.Context(), "console:"+login, lockFor); err != nil {
		s.writeErr(w, r, err)
		return
	} else if n >= maxLoginFailures && time.Since(last) < lockFor {
		mins := int(math.Ceil((lockFor - time.Since(last)).Minutes()))
		fail(w, http.StatusTooManyRequests, "account_locked", fmt.Sprintf("too many failed sign-ins; this account is locked for %d more minutes", mins))
		return
	}
	u, ok := s.lookupUser(req.Email, req.Password)
	if !ok || len(s.cfg.SessionKey) == 0 {
		s.store.Audit(r.Context(), "console:"+login, "console.login_failed", "console", "", map[string]any{"ip": s.clientIP(r).String()})
		fail(w, http.StatusUnauthorized, "invalid_credentials", "email or password is incorrect")
		return
	}
	ttl := s.cfg.SessionTTL
	ss := session{ID: security.RandomToken(24), Email: u.Email, Name: u.Name, Role: u.Role, Exp: time.Now().Add(ttl).Unix()}
	if err := s.store.CreateSession(r.Context(), ss.ID, u.Email, u.Role, s.clientIP(r).String(), time.Unix(ss.Exp, 0)); err != nil {
		s.writeErr(w, r, err)
		return
	}
	s.setSessionCookie(w, r, s.encodeSession(ss), int(ttl.Seconds()))
	s.store.Audit(r.Context(), "console:"+u.Email, "console.login", "console", u.Email, map[string]any{"ip": s.clientIP(r).String()})
	writeJSON(w, http.StatusOK, map[string]string{"email": u.Email, "name": u.Name, "role": u.Role})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if ss, ok := s.sessionFrom(r); ok {
		_ = s.store.RevokeSession(r.Context(), ss.ID)
		s.store.Audit(r.Context(), "console:"+ss.Email, "console.logout", "console", ss.Email, map[string]any{"ip": s.clientIP(r).String()})
	}
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
