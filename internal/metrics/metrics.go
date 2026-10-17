// Package metrics is a small Prometheus text-format registry (counters and latency histograms).
package metrics

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

var buckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.15, 0.25, 0.5, 1, 2.5, 5, 10}

type hist struct {
	counts []uint64
	sum    float64
	n      uint64
}

// Registry holds labelled counters and histograms.
type Registry struct {
	mu       sync.Mutex
	counters map[string]map[string]float64 // name → labels → value
	hists    map[string]map[string]*hist
	help     map[string]string
}

// New creates an empty registry.
func New() *Registry {
	return &Registry{counters: map[string]map[string]float64{}, hists: map[string]map[string]*hist{}, help: map[string]string{
		"upi_http_requests_total":      "HTTP requests served by the switch.",
		"upi_http_request_seconds":     "Switch processing time per HTTP route.",
		"upi_bank_requests_total":      "Messages sent to sponsor PSP banks.",
		"upi_bank_request_seconds":     "Round-trip time to sponsor PSP banks.",
		"upi_txn_transitions_total":    "Ledger status transitions.",
		"upi_webhook_deliveries_total": "Webhook delivery attempts to payment gateways.",
		"upi_callbacks_total":          "Bank callbacks received.",
		"upi_risk_declines_total":      "Payments declined by the risk engine.",
	}}
}

func labels(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", kv[i], kv[i+1])
	}
	return b.String()
}

// Inc adds 1 to a counter.
func (r *Registry) Inc(name string, kv ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.counters[name]
	if m == nil {
		m = map[string]float64{}
		r.counters[name] = m
	}
	m[labels(kv...)]++
}

// Observe records a duration in a histogram.
func (r *Registry) Observe(name string, d time.Duration, kv ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.hists[name]
	if m == nil {
		m = map[string]*hist{}
		r.hists[name] = m
	}
	l := labels(kv...)
	h := m[l]
	if h == nil {
		h = &hist{counts: make([]uint64, len(buckets))}
		m[l] = h
	}
	v := d.Seconds()
	for i, b := range buckets {
		if v <= b {
			h.counts[i]++
		}
	}
	h.sum += v
	h.n++
}

// ObserveBank records a PSP round trip.
func (r *Registry) ObserveBank(psp, api string, d time.Duration, ok bool) {
	res := "ok"
	if !ok {
		res = "error"
	}
	r.Inc("upi_bank_requests_total", "psp", psp, "api", api, "result", res)
	r.Observe("upi_bank_request_seconds", d, "psp", psp, "api", api)
}

// Transition counts a ledger status change.
func (r *Registry) Transition(kind, to string) {
	r.Inc("upi_txn_transitions_total", "kind", kind, "to", to)
}

// Write renders the registry in Prometheus text format.
func (r *Registry) Write(w io.Writer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.counters))
	for n := range r.counters {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s counter\n", n, r.help[n], n)
		ls := sortedKeys(r.counters[n])
		for _, l := range ls {
			fmt.Fprintf(w, "%s{%s} %g\n", n, l, r.counters[n][l])
		}
	}
	names = names[:0]
	for n := range r.hists {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", n, r.help[n], n)
		for _, l := range sortedKeys(r.hists[n]) {
			h := r.hists[n][l]
			sep := ""
			if l != "" {
				sep = ","
			}
			for i, b := range buckets {
				fmt.Fprintf(w, "%s_bucket{%s%sle=\"%g\"} %d\n", n, l, sep, b, h.counts[i])
			}
			fmt.Fprintf(w, "%s_bucket{%s%sle=\"+Inf\"} %d\n", n, l, sep, h.n)
			fmt.Fprintf(w, "%s_sum{%s} %g\n%s_count{%s} %d\n", n, l, h.sum, n, l, h.n)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
