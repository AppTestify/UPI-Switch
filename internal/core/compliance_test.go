package core

import (
	"testing"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

var ist = time.FixedZone("IST", 5*3600+1800)

func TestEvaluateTAT(t *testing.T) {
	created := time.Date(2026, 10, 1, 14, 0, 0, 0, ist)
	deadline := time.Date(2026, 10, 7, 0, 0, 0, 0, ist) // end of T+5
	at := func(d time.Time) *time.Time { return &d }
	cases := []struct {
		name     string
		resolved *time.Time
		now      time.Time
		state    string
		days     int
	}{
		{"open, plenty of time", nil, created.Add(48 * time.Hour), TATWithin, 0},
		{"open, due within a day", nil, deadline.Add(-3 * time.Hour), TATDueSoon, 0},
		{"open, past deadline", nil, deadline.Add(50 * time.Hour), TATBreached, 3},
		{"reversed in time", at(created.Add(26 * time.Hour)), deadline.Add(100 * time.Hour), TATResolvedInTime, 0},
		{"reversed late", at(deadline.Add(time.Hour)), deadline.Add(100 * time.Hour), TATResolvedLate, 1},
	}
	for _, c := range cases {
		v := EvaluateTAT(store.TATCase{CreatedAt: created, ResolvedAt: c.resolved}, c.now, ist)
		if !v.Deadline.Equal(deadline) || v.State != c.state || v.DaysLate != c.days || v.CompensationPaise != int64(c.days)*CompensationPerDay {
			t.Errorf("%s: got %s, %d days late, %d paise, deadline %s", c.name, v.State, v.DaysLate, v.CompensationPaise, v.Deadline)
		}
	}
}

func TestAutoPayRules(t *testing.T) {
	day := func(h, m int) time.Time { return time.Date(2026, 10, 20, h, m, 0, 0, ist) }
	for _, c := range []struct {
		at   time.Time
		ok   bool
		next string
	}{
		{day(9, 59), true, ""}, {day(10, 0), false, "13:00"}, {day(12, 59), false, "13:00"}, {day(13, 0), true, ""},
		{day(17, 30), false, "21:30"}, {day(21, 30), true, ""}, {day(2, 0), true, ""},
	} {
		ok, opens := OffPeak(c.at, ist)
		if ok != c.ok || (!ok && opens.Format("15:04") != c.next) {
			t.Errorf("%s: off-peak %v, opens %s", c.at.Format("15:04"), ok, opens.Format("15:04"))
		}
	}
	if AFACap("5411") != 15_000*100 || AFACap("6300") != 100_000*100 || AFACap("6211") != 100_000*100 {
		t.Error("AFA caps")
	}
}

func TestCheckAllowed(t *testing.T) {
	s := &Service{cfg: Config{}}
	s.cfg.defaults()
	now := time.Now()
	pending := func(age time.Duration, attempts int) *domain.Txn {
		return &domain.Txn{Status: domain.StatusPending, Type: domain.TypeCollect, CreatedAt: now.Add(-age), ExpiresAt: now.Add(time.Hour), CheckAttempts: attempts}
	}
	for _, c := range []struct {
		name string
		txn  *domain.Txn
		code string
	}{
		{"too early", pending(30*time.Second, 0), "check_too_early"},
		{"allowed", pending(2*time.Minute, 1), ""},
		{"limit reached", pending(10*time.Minute, 3), "check_limit_reached"},
		{"window closed", pending(3*time.Hour, 1), "check_window_closed"},
		{"final", &domain.Txn{Status: domain.StatusSuccess}, "txn_final"},
		{"unpaid intent", &domain.Txn{Status: domain.StatusInitiated, Type: domain.TypeIntent, CreatedAt: now.Add(-5 * time.Minute), ExpiresAt: now.Add(time.Minute)}, "awaiting_payer"},
	} {
		err := s.CheckAllowed(c.txn, now)
		got := ""
		if e, ok := err.(*Error); ok {
			got = e.Code
		}
		if got != c.code {
			t.Errorf("%s: got %q want %q (%v)", c.name, got, c.code, err)
		}
	}
}
