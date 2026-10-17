// Package ratelimit enforces per-tenant request rates. With Redis configured the limit is shared
// across all active-active switch instances; otherwise each instance enforces it locally.
package ratelimit

import (
	"context"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Limiter decides whether one more request is allowed for key at limit per second.
type Limiter interface {
	Allow(ctx context.Context, key string, perSec int) (bool, error)
}

// Local is an in-process token bucket per key.
type Local struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLocal creates an in-process limiter.
func NewLocal() *Local { return &Local{buckets: map[string]*bucket{}, now: time.Now} }

// Allow implements Limiter. Burst equals one second of traffic.
func (l *Local) Allow(_ context.Context, key string, perSec int) (bool, error) {
	if perSec <= 0 {
		return true, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(perSec), last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * float64(perSec)
	if b.tokens > float64(perSec) {
		b.tokens = float64(perSec)
	}
	b.last = now
	if b.tokens < 1 {
		return false, nil
	}
	b.tokens--
	return true, nil
}

// Redis is a fixed one-second window counter in Redis, shared across instances.
type Redis struct{ Client *redis.Client }

// Allow implements Limiter.
func (r Redis) Allow(ctx context.Context, key string, perSec int) (bool, error) {
	if perSec <= 0 {
		return true, nil
	}
	k := "rl:" + key + ":" + strconv.FormatInt(time.Now().Unix(), 10)
	pipe := r.Client.TxPipeline()
	incr := pipe.Incr(ctx, k)
	pipe.Expire(ctx, k, 2*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return true, err // fail open on cache outage; the WAF still applies
	}
	return incr.Val() <= int64(perSec), nil
}
