package router

import (
	"testing"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
)

func banks() []domain.PSPBank {
	return []domain.PSPBank{
		{ID: "a", Priority: 1, CostBps: 2, Enabled: true},
		{ID: "b", Priority: 2, CostBps: 2, Enabled: true},
		{ID: "c", Priority: 3, CostBps: 2, Enabled: false},
	}
}

func TestRankAndFailover(t *testing.T) {
	r := New(banks())
	got, _ := r.Rank(domain.Routing{})
	if len(got) != 2 || got[0].ID != "a" {
		t.Fatalf("initial rank %v", got)
	}
	for i := 0; i < 3; i++ {
		r.Record("a", 50*time.Millisecond, true, "timeout")
	}
	got, _ = r.Rank(domain.Routing{})
	if got[0].ID != "b" || got[1].ID != "a" {
		t.Fatalf("after failures want b first, got %v", got)
	}
	if h := r.Health(); !h[0].CircuitOpen {
		t.Fatalf("circuit should be open: %+v", h[0])
	}
	r.Record("a", 10*time.Millisecond, false, "")
	if h := r.Health(); h[0].CircuitOpen {
		t.Fatal("success should close circuit")
	}
}

func TestTenantRules(t *testing.T) {
	r := New(banks())
	got, _ := r.Rank(domain.Routing{AllowedPSPs: []string{"b"}})
	if len(got) != 1 || got[0].ID != "b" {
		t.Fatalf("allowed filter %v", got)
	}
	got, _ = r.Rank(domain.Routing{PreferredPSPs: []string{"b", "a"}})
	if got[0].ID != "b" {
		t.Fatalf("preference %v", got)
	}
	if _, err := r.Rank(domain.Routing{AllowedPSPs: []string{"c"}}); err != ErrNoPSP {
		t.Fatalf("disabled only → ErrNoPSP, got %v", err)
	}
}

func TestCostAndLatencyScore(t *testing.T) {
	r := New([]domain.PSPBank{{ID: "cheap", CostBps: 0, Priority: 2, Enabled: true}, {ID: "dear", CostBps: 50, Priority: 1, Enabled: true}})
	got, _ := r.Rank(domain.Routing{})
	if got[0].ID != "cheap" {
		t.Fatalf("cost should win, got %v", got)
	}
}
