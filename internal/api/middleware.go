// Package api is the switch's HTTP surface: the payment-gateway API (interface 1), the bank
// callback listener (interface 4), the admin console API and operational endpoints.
package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/core"
	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// Tenant API headers.
const (
	HeaderClientID    = "X-Client-Id"
	HeaderTimestamp   = "X-Timestamp"
	HeaderSignature   = "X-Signature"
	HeaderIdempotency = "Idempotency-Key"
	HeaderRequestID   = "X-Request-Id"
)

type ctxKey int

const (
	ctxTenant ctxKey = iota
	ctxBody
	ctxActor
	ctxUser
)

func tenantFrom(ctx context.Context) *domain.Tenant {
	t, _ := ctx.Value(ctxTenant).(*domain.Tenant)
	return t
}
func bodyFrom(ctx context.Context) []byte  { b, _ := ctx.Value(ctxBody).([]byte); return b }
func actorFrom(ctx context.Context) string { a, _ := ctx.Value(ctxActor).(string); return a }

type apiError struct {
	Error *core.Error `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var ce *core.Error
	if errors.As(err, &ce) {
		writeJSON(w, ce.Status, apiError{ce})
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, apiError{&core.Error{Code: "not_found", Message: "not found"}})
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeJSON(w, http.StatusConflict, apiError{&core.Error{Code: "conflict", Message: err.Error()}})
		return
	}
	s.log.Error("request failed", "path", r.URL.Path, "err", err, "request_id", w.Header().Get(HeaderRequestID))
	writeJSON(w, http.StatusInternalServerError, apiError{&core.Error{Code: "internal_error", Message: "internal error"}})
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, apiError{&core.Error{Code: code, Message: msg}})
}

// statusRecorder captures status and body for logging, metrics and idempotent replay.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	buf     *bytes.Buffer
	capture bool
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.capture {
		r.buf.Write(b)
	}
	return r.ResponseWriter.Write(b)
}

// observe adds a request id, structured access logs and latency metrics.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get(HeaderRequestID)
		if rid == "" || len(rid) > 64 {
			rid = security.RandomToken(8)
		}
		w.Header().Set(HeaderRequestID, rid)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.secureCookie(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		d := time.Since(start)
		s.metrics.Inc("upi_http_requests_total", "route", route, "code", strconv.Itoa(rec.status))
		s.metrics.Observe("upi_http_request_seconds", d, "route", route)
		lvl := slog.LevelInfo
		if rec.status >= 500 {
			lvl = slog.LevelError
		}
		s.log.Log(r.Context(), lvl, "http", "method", r.Method, "route", route, "status", rec.status,
			"ms", d.Milliseconds(), "request_id", rid)
	})
}

func (s *Server) clientIP(r *http.Request) net.IP {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The right-most entry is the one our proxy added; entries to its left are whatever the client sent.
			hops := strings.Split(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(hops[len(hops)-1])); ip != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return net.ParseIP(host)
}

func ipAllowed(ip net.IP, list []string) bool {
	if len(list) == 0 {
		return true
	}
	if ip == nil {
		return false
	}
	for _, entry := range list {
		if strings.Contains(entry, "/") {
			if _, n, err := net.ParseCIDR(entry); err == nil && n.Contains(ip) {
				return true
			}
		} else if other := net.ParseIP(entry); other != nil && other.Equal(ip) {
			return true
		}
	}
	return false
}

// tenantAuth authenticates a payment gateway: HMAC signature over timestamp, method, path and body,
// a 5-minute replay window, the tenant's IP allow-list, its mTLS certificate CN, and its rate limit.
func (s *Server) tenantAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keyID := r.Header.Get(HeaderClientID)
		ts := r.Header.Get(HeaderTimestamp)
		sig := r.Header.Get(HeaderSignature)
		if keyID == "" || ts == "" || sig == "" {
			fail(w, http.StatusUnauthorized, "unauthenticated", "X-Client-Id, X-Timestamp and X-Signature are required")
			return
		}
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || absDur(time.Since(time.Unix(sec, 0))) > s.cfg.MaxClockSkew {
			fail(w, http.StatusUnauthorized, "stale_request", "X-Timestamp outside the allowed window")
			return
		}
		t, err := s.store.GetTenantByAPIKey(r.Context(), keyID)
		if err != nil {
			fail(w, http.StatusUnauthorized, "unauthenticated", "unknown client")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil || len(body) > 1<<20 {
			fail(w, http.StatusRequestEntityTooLarge, "body_too_large", "request body over 1 MiB")
			return
		}
		if !security.EqualHex(sig, security.SignRequest(t.HMACSecret, ts, r.Method, r.URL.RequestURI(), body)) {
			fail(w, http.StatusUnauthorized, "bad_signature", "signature mismatch")
			return
		}
		if t.Status != "ACTIVE" {
			fail(w, http.StatusForbidden, "tenant_suspended", "tenant is not active")
			return
		}
		if !ipAllowed(s.clientIP(r), t.IPAllowlist) {
			fail(w, http.StatusForbidden, "ip_not_allowed", "source IP not in tenant allow-list")
			return
		}
		if t.CertCN != "" {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != t.CertCN {
				fail(w, http.StatusForbidden, "client_cert_required", "mTLS client certificate does not match tenant")
				return
			}
		}
		rate := t.Limits.RateLimitPerSec
		if rate == 0 {
			rate = s.cfg.DefaultRateLimit
		}
		if ok, err := s.limiter.Allow(r.Context(), t.ID, rate); err != nil {
			s.log.Warn("rate limiter unavailable", "err", err)
		} else if !ok {
			w.Header().Set("Retry-After", "1")
			fail(w, http.StatusTooManyRequests, "rate_limited", "tenant rate limit exceeded")
			return
		}
		ctx := context.WithValue(r.Context(), ctxTenant, t)
		ctx = context.WithValue(ctx, ctxBody, body)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// idempotent makes a POST safe to retry: the first response for (tenant, Idempotency-Key) is stored
// and replayed; reusing a key with a different body is rejected.
func (s *Server) idempotent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get(HeaderIdempotency)
		if key == "" || len(key) > 128 {
			fail(w, http.StatusBadRequest, "idempotency_key_required", "Idempotency-Key header (max 128 chars) is required")
			return
		}
		t := tenantFrom(r.Context())
		h := sha256.Sum256(append([]byte(r.Method+" "+r.URL.Path+"\n"), bodyFrom(r.Context())...))
		res, err := s.store.ClaimIdempotency(r.Context(), t.ID, key, hex.EncodeToString(h[:]))
		if err != nil {
			s.writeErr(w, r, err)
			return
		}
		switch {
		case res.HashMismatch:
			fail(w, http.StatusUnprocessableEntity, "idempotency_key_reused", "Idempotency-Key was used with a different request")
			return
		case res.InProgress:
			fail(w, http.StatusConflict, "request_in_progress", "a request with this Idempotency-Key is still processing")
			return
		case !res.New:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Idempotent-Replayed", "true")
			w.WriteHeader(res.Code)
			w.Write(res.Body)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, buf: &bytes.Buffer{}, capture: true}
		next.ServeHTTP(rec, r)
		ctx := context.WithoutCancel(r.Context())
		if rec.status >= 500 || rec.status == http.StatusTooManyRequests {
			s.store.ReleaseIdempotency(ctx, t.ID, key)
			return
		}
		if err := s.store.CompleteIdempotency(ctx, t.ID, key, rec.status, rec.buf.Bytes()); err != nil {
			s.log.Error("idempotency store", "err", err)
		}
	})
}

// Roles for the admin console (RBAC).
const (
	RoleAdmin  = "admin"
	RoleOps    = "ops"
	RoleViewer = "viewer"
)

var roleRank = map[string]int{RoleViewer: 1, RoleOps: 2, RoleAdmin: 3}

// adminAuth accepts a bearer token or a signed console session cookie and checks that its role is at least need.
func (s *Server) adminAuth(need string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var u ConsoleUser
		var actor string
		if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
			for _, a := range s.cfg.AdminTokens {
				if security.EqualHex(hexOf(a.Token), hexOf(tok)) {
					u, actor = ConsoleUser{Email: a.Name, Name: a.Name, Role: a.Role}, "admin:"+a.Name
					break
				}
			}
		} else if ss, ok := s.sessionFrom(r); ok {
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Header.Get(HeaderConsole) == "" {
				fail(w, http.StatusForbidden, "csrf", "console requests must carry the "+HeaderConsole+" header")
				return
			}
			u, actor = ConsoleUser{Email: ss.Email, Name: ss.Name, Role: ss.Role}, "console:"+ss.Email
		}
		if u.Role == "" {
			fail(w, http.StatusUnauthorized, "unauthenticated", "sign in or send a valid admin bearer token")
			return
		}
		if roleRank[u.Role] < roleRank[need] {
			fail(w, http.StatusForbidden, "forbidden", "role "+u.Role+" cannot perform this action")
			return
		}
		next(w, r.WithContext(withUser(r.Context(), u, actor)))
	})
}

func hexOf(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func decode(r *http.Request, body []byte, v any) error {
	if body == nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &core.Error{Status: http.StatusBadRequest, Code: "invalid_json", Message: err.Error()}
	}
	return nil
}
