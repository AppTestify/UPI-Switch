package risk

import (
	"context"
	"testing"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
)

type fakeStats struct {
	volume int64
	count  int
}

func (f fakeStats) TenantVolumeSince(context.Context, string, time.Time) (int64, error) {
	return f.volume, nil
}
func (f fakeStats) PayerCountSince(context.Context, string, string, time.Time) (int, error) {
	return f.count, nil
}

func TestRules(t *testing.T) {
	tenant := &domain.Tenant{ID: "t", Limits: domain.Limits{MinTxnPaise: 100, MaxTxnPaise: 10000, DailyPaise: 50000, VelocityPerVPA: 3, BlockedMCC: []string{"7995"}}}
	cases := []struct {
		name  string
		stats fakeStats
		check Check
		rule  string
	}{
		{"ok", fakeStats{}, Check{AmountPaise: 500, MCC: "5411"}, ""},
		{"min", fakeStats{}, Check{AmountPaise: 50}, "min_amount"},
		{"max", fakeStats{}, Check{AmountPaise: 20000}, "max_amount"},
		{"tenant mcc", fakeStats{}, Check{AmountPaise: 500, MCC: "7995"}, "mcc"},
		{"global mcc", fakeStats{}, Check{AmountPaise: 500, MCC: "6051"}, "mcc"},
		{"daily", fakeStats{volume: 49900}, Check{AmountPaise: 500}, "daily_limit"},
		{"velocity", fakeStats{count: 3}, Check{AmountPaise: 500, PayerToken: "p"}, "velocity"},
		{"velocity ignores unknown payer", fakeStats{count: 3}, Check{AmountPaise: 500}, ""},
	}
	for _, c := range cases {
		e := &Engine{Stats: c.stats, GlobalMCC: []string{"6051"}}
		c.check.Tenant = tenant
		d, err := e.Evaluate(context.Background(), c.check)
		if err != nil {
			t.Fatal(err)
		}
		if d.Rule != c.rule || d.Allow != (c.rule == "") {
			t.Errorf("%s: got %+v want rule %q", c.name, d, c.rule)
		}
	}
}
