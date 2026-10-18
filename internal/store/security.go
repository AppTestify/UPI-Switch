package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// ConsoleSession is a signed-in console user, recorded server-side.
type ConsoleSession struct {
	ID, Email, Role string
	LastSeenAt      time.Time
	ExpiresAt       time.Time
}

// CreateSession records a new console session.
func (s *Store) CreateSession(ctx context.Context, id, email, role, ip string, expires time.Time) error {
	_, err := s.q.Exec(ctx, `INSERT INTO console_sessions (id, email, role, ip, expires_at) VALUES ($1,$2,$3,$4,$5)`, id, email, role, ip, expires)
	return err
}

// TouchSession returns a live session (not revoked, not expired, used within idle) and records the use.
// A session idle for longer is revoked.
func (s *Store) TouchSession(ctx context.Context, id string, idle time.Duration) (*ConsoleSession, error) {
	var c ConsoleSession
	err := s.q.QueryRow(ctx, `UPDATE console_sessions SET last_seen_at=now()
		WHERE id=$1 AND revoked_at IS NULL AND expires_at > now() AND last_seen_at > now() - make_interval(secs => $2)
		RETURNING id, email, role, last_seen_at, expires_at`, id, idle.Seconds()).Scan(&c.ID, &c.Email, &c.Role, &c.LastSeenAt, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = s.q.Exec(ctx, `UPDATE console_sessions SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
		return nil, ErrNotFound
	}
	return &c, err
}

// RevokeSession ends a session.
func (s *Store) RevokeSession(ctx context.Context, id string) error {
	_, err := s.q.Exec(ctx, `UPDATE console_sessions SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`, id)
	return err
}

// RecentLoginFailures counts failed sign-ins for actor since its last successful one, within window.
func (s *Store) RecentLoginFailures(ctx context.Context, actor string, window time.Duration) (int, time.Time, error) {
	var n int
	var last *time.Time
	err := s.q.QueryRow(ctx, `SELECT COUNT(*), MAX(at) FROM audit_log WHERE actor=$1 AND action='console.login_failed'
		AND at > GREATEST(now() - make_interval(secs => $2),
			COALESCE((SELECT MAX(at) FROM audit_log WHERE actor=$1 AND action='console.login'), '-infinity'))`,
		actor, window.Seconds()).Scan(&n, &last)
	if last == nil {
		return n, time.Time{}, err
	}
	return n, *last, err
}
