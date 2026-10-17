package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestLocal(t *testing.T) {
	l := NewLocal()
	now := time.Unix(0, 0)
	l.now = func() time.Time { return now }
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if ok, _ := l.Allow(ctx, "t", 5); !ok {
			t.Fatalf("request %d should pass", i)
		}
	}
	if ok, _ := l.Allow(ctx, "t", 5); ok {
		t.Fatal("6th request in the same instant should be limited")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Allow(ctx, "t", 5); !ok {
		t.Fatal("bucket should refill")
	}
}
