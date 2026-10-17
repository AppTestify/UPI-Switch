// Package router is the smart PSP router: it scores sponsor banks on live success rate,
// latency and cost, and fails over on technical declines or downtime.
package router

import (
	"errors"
	"math"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
)

// ErrNoPSP is returned when no sponsor bank is available for a tenant.
var ErrNoPSP = errors.New("router: no sponsor PSP bank available")

// Weights tune the score. Score = W_success*successRate - W_latency*latency/1s - W_cost*cost_bps/100.
type Weights struct {
	Success float64
	Latency float64
	Cost    float64
}

// DefaultWeights favour success rate, then latency, then cost.
var DefaultWeights = Weights{Success: 100, Latency: 20, Cost: 5}

type stats struct {
	successEWMA float64
	latencyEWMA float64 // seconds
	consecFails int
	openUntil   time.Time
	samples     int64
	techFails   int64
	lastError   string
}

// Health is a PSP's live view, exposed on the admin API.
type Health struct {
	PSPID        string  `json:"psp_id"`
	Enabled      bool    `json:"enabled"`
	SuccessRate  float64 `json:"success_rate"`
	LatencyMs    float64 `json:"latency_ms"`
	CostBps      int     `json:"cost_bps"`
	Score        float64 `json:"score"`
	CircuitOpen  bool    `json:"circuit_open"`
	Samples      int64   `json:"samples"`
	TechFailures int64   `json:"technical_failures"`
	LastError    string  `json:"last_error,omitempty"`
}

// Router keeps per-PSP stats in memory. Each switch instance learns independently (active-active safe).
type Router struct {
	mu        sync.RWMutex
	psps      map[string]domain.PSPBank
	stats     map[string]*stats
	weights   Weights
	alpha     float64
	tripAfter int
	cooldown  time.Duration
	now       func() time.Time
}

// New creates a router.
func New(psps []domain.PSPBank) *Router {
	r := &Router{
		psps: map[string]domain.PSPBank{}, stats: map[string]*stats{},
		weights: DefaultWeights, alpha: 0.2, tripAfter: 3, cooldown: 30 * time.Second, now: time.Now,
	}
	r.SetPSPs(psps)
	return r
}

// SetCircuit configures how many consecutive technical failures open the circuit, and for how long.
func (r *Router) SetCircuit(tripAfter int, cooldown time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tripAfter, r.cooldown = tripAfter, cooldown
}

// SetPSPs replaces the PSP configuration (from the config DB), keeping learned stats.
func (r *Router) SetPSPs(psps []domain.PSPBank) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.psps = map[string]domain.PSPBank{}
	for _, p := range psps {
		r.psps[p.ID] = p
		if r.stats[p.ID] == nil {
			r.stats[p.ID] = &stats{successEWMA: 1, latencyEWMA: 0.2}
		}
	}
}

// PSP returns the config for one bank.
func (r *Router) PSP(id string) (domain.PSPBank, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.psps[id]
	return p, ok
}

func (r *Router) score(p domain.PSPBank, s *stats) float64 {
	return r.weights.Success*s.successEWMA - r.weights.Latency*s.latencyEWMA - r.weights.Cost*float64(p.CostBps)/100
}

// Rank returns candidate PSPs for a tenant, best first. Banks with an open circuit go last
// (still usable as a last resort), disabled banks are excluded.
func (r *Router) Rank(rules domain.Routing) ([]domain.PSPBank, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := r.now()
	type cand struct {
		p     domain.PSPBank
		score float64
		open  bool
		pref  int
	}
	var cs []cand
	for id, p := range r.psps {
		if !p.Enabled {
			continue
		}
		if len(rules.AllowedPSPs) > 0 && !slices.Contains(rules.AllowedPSPs, id) {
			continue
		}
		s := r.stats[id]
		pref := slices.Index(rules.PreferredPSPs, id)
		if pref < 0 {
			pref = math.MaxInt32
		}
		cs = append(cs, cand{p: p, score: r.score(p, s), open: now.Before(s.openUntil), pref: pref})
	}
	if len(cs) == 0 {
		return nil, ErrNoPSP
	}
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.open != b.open {
			return !a.open
		}
		if math.Abs(a.score-b.score) > 0.5 {
			return a.score > b.score
		}
		if a.pref != b.pref {
			return a.pref < b.pref
		}
		if a.p.Priority != b.p.Priority {
			return a.p.Priority < b.p.Priority
		}
		return a.p.ID < b.p.ID
	})
	out := make([]domain.PSPBank, len(cs))
	for i, c := range cs {
		out[i] = c.p
	}
	return out, nil
}

// Record feeds an outcome back. technicalFailure is true for timeouts, 5xx, negative acks and
// technical decline codes; customer declines (wrong PIN, no funds) count as successes for the bank.
func (r *Router) Record(pspID string, latency time.Duration, technicalFailure bool, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.stats[pspID]
	if s == nil {
		return
	}
	v := 1.0
	if technicalFailure {
		v = 0
		s.consecFails++
		s.techFails++
		s.lastError = errMsg
		if s.consecFails >= r.tripAfter {
			s.openUntil = r.now().Add(r.cooldown)
		}
	} else {
		s.consecFails = 0
		s.openUntil = time.Time{}
	}
	s.samples++
	s.successEWMA = r.alpha*v + (1-r.alpha)*s.successEWMA
	if latency > 0 {
		s.latencyEWMA = r.alpha*latency.Seconds() + (1-r.alpha)*s.latencyEWMA
	}
}

// Health returns the live view of all PSPs.
func (r *Router) Health() []Health {
	r.mu.RLock()
	defer r.mu.RUnlock()
	now := r.now()
	var out []Health
	for id, p := range r.psps {
		s := r.stats[id]
		out = append(out, Health{
			PSPID: id, Enabled: p.Enabled, SuccessRate: math.Round(s.successEWMA*1000) / 1000,
			LatencyMs: math.Round(s.latencyEWMA * 1000), CostBps: p.CostBps,
			Score: math.Round(r.score(p, s)*100) / 100, CircuitOpen: now.Before(s.openUntil),
			Samples: s.samples, TechFailures: s.techFails, LastError: s.lastError,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PSPID < out[j].PSPID })
	return out
}
