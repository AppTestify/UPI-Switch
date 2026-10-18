package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// ---- RBI turnaround time (TAT) for failed transactions ----

// RBI's harmonised TAT (DPSS.CO.PD No.629/02.01.014/2019-20; NPCI OC-93) for UPI payments to merchants:
// when the customer is debited but the merchant is not confirmed, the payment must be reversed by T+5,
// where T is the day of the payment, after which the customer is owed ₹100 for every day of delay.
const (
	TATDays            = 5
	CompensationPerDay = 100 * 100 // paise
	tatDueSoon         = 24 * time.Hour
	TATWithin          = "WITHIN_TAT"
	TATDueSoon         = "DUE_SOON"
	TATBreached        = "BREACHED"
	TATResolvedInTime  = "RESOLVED_IN_TAT"
	TATResolvedLate    = "RESOLVED_LATE"
)

// TATView is a turnaround case with its deadline and any compensation owed.
type TATView struct {
	store.TATCase
	Amount            string    `json:"amount"`
	Deadline          time.Time `json:"deadline"`
	State             string    `json:"state"`
	DaysLate          int       `json:"days_late"`
	CompensationPaise int64     `json:"-"`
	Compensation      string    `json:"compensation"`
}

// TATDeadline is the end of day T+5 in the business time zone.
func TATDeadline(created time.Time, loc *time.Location) time.Time {
	d := created.In(loc)
	return time.Date(d.Year(), d.Month(), d.Day()+TATDays+1, 0, 0, 0, 0, loc)
}

// EvaluateTAT works out where a case stands against RBI's TAT at now.
func EvaluateTAT(c store.TATCase, now time.Time, loc *time.Location) TATView {
	v := TATView{TATCase: c, Amount: domain.FormatAmount(c.Paise), Deadline: TATDeadline(c.CreatedAt, loc)}
	end := now
	if c.ResolvedAt != nil {
		end = *c.ResolvedAt
	}
	if late := end.Sub(v.Deadline); late > 0 {
		v.DaysLate = int(math.Ceil(late.Hours() / 24))
		v.CompensationPaise = int64(v.DaysLate) * CompensationPerDay
	}
	switch {
	case c.ResolvedAt != nil && v.DaysLate == 0:
		v.State = TATResolvedInTime
	case c.ResolvedAt != nil:
		v.State = TATResolvedLate
	case v.DaysLate > 0:
		v.State = TATBreached
	case v.Deadline.Sub(now) <= tatDueSoon:
		v.State = TATDueSoon
	default:
		v.State = TATWithin
	}
	v.Compensation = domain.FormatAmount(v.CompensationPaise)
	return v
}

// ---- sponsor bank downtime ----

// IncidentPolicy decides when a bank is down. An incident opens when the router's circuit breaker is
// open, or when technical declines reach OpenAt over Window with at least MinSamples payments; it
// closes once the rate falls below CloseBelow (or traffic thins out) and the circuit has closed.
type IncidentPolicy struct {
	Window     time.Duration
	MinSamples int64
	OpenAt     float64
	HighAt     float64
	CloseBelow float64
}

// DefaultIncidentPolicy flags 10% technical declines over 5 minutes, HIGH from 25%.
var DefaultIncidentPolicy = IncidentPolicy{Window: 5 * time.Minute, MinSamples: 20, OpenAt: 0.10, HighAt: 0.25, CloseBelow: 0.03}

// RunIncidentMonitor checks bank health every interval until ctx is cancelled.
func (s *Service) RunIncidentMonitor(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.MonitorBanksOnce(ctx); err != nil {
				s.log.Error("bank monitor", "err", err)
			}
		}
	}
}

// MonitorBanksOnce opens, updates or resolves a downtime incident for every bank, and tells every
// gateway by webhook when a bank goes down or recovers.
func (s *Service) MonitorBanksOnce(ctx context.Context) error {
	p := DefaultIncidentPolicy
	now := time.Now()
	win, err := s.store.BankWindows(ctx, now.Add(-p.Window), domain.TechnicalDeclineCodes)
	if err != nil {
		return err
	}
	for _, h := range s.router.Health() {
		w := win[h.PSPID]
		rate := 0.0
		if w.Count > 0 {
			rate = float64(w.Technical) / float64(w.Count)
		}
		enough := w.Count >= p.MinSamples
		down := h.Enabled && (h.CircuitOpen || (enough && rate >= p.OpenAt))
		recovered := !h.CircuitOpen && (!enough || rate < p.CloseBelow)
		sev, reason := "MEDIUM", fmt.Sprintf("technical declines at %.1f%% over the last %s", rate*100, p.Window)
		if h.CircuitOpen {
			sev, reason = "HIGH", "bank is not responding; payments are failing over to other sponsor banks"
		} else if rate >= p.HighAt {
			sev = "HIGH"
		}
		err := s.store.InTx(ctx, func(tx *store.Store) error {
			cur, err := tx.ActiveIncident(ctx, h.PSPID)
			if errors.Is(err, store.ErrNotFound) {
				if !down {
					return nil
				}
				inc := &domain.Incident{ID: npci.NewTxnID("INC"), PSPID: h.PSPID, Severity: sev, Reason: reason, TDRate: rate, Affected: w.Technical}
				if err := tx.OpenIncident(ctx, inc); err != nil {
					return err
				}
				if err := tx.Audit(ctx, "system:monitor", "bank.downtime_started", "psp", h.PSPID, inc); err != nil {
					return err
				}
				return s.broadcast(ctx, tx, inc.ID+":started", "bank.downtime.started", s.downtimeView(inc))
			}
			if err != nil {
				return err
			}
			if down || !recovered {
				if down {
					cur.Severity, cur.Reason = sev, reason
				}
				cur.TDRate, cur.Affected = rate, max(cur.Affected, w.Technical)
				return tx.UpdateIncident(ctx, cur)
			}
			cur.Status, cur.TDRate, cur.ResolvedAt = "RESOLVED", rate, &now
			if err := tx.UpdateIncident(ctx, cur); err != nil {
				return err
			}
			if err := tx.Audit(ctx, "system:monitor", "bank.downtime_resolved", "psp", h.PSPID, cur); err != nil {
				return err
			}
			return s.broadcast(ctx, tx, cur.ID+":resolved", "bank.downtime.resolved", s.downtimeView(cur))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// Downtime is the gateway-facing view of an incident (webhooks and GET /v1/upi/downtimes).
type Downtime struct {
	*domain.Incident
	Bank   string `json:"bank"`
	Handle string `json:"handle"`
}

func (s *Service) downtimeView(i *domain.Incident) Downtime {
	d := Downtime{Incident: i}
	if p, ok := s.router.PSP(i.PSPID); ok {
		d.Bank, d.Handle = p.Name, "@"+p.Handle
	}
	return d
}

// Downtimes lists active and recent incidents for gateways.
func (s *Service) Downtimes(ctx context.Context, since time.Time) ([]Downtime, error) {
	list, err := s.store.ListIncidents(ctx, since, 100)
	if err != nil {
		return nil, err
	}
	out := make([]Downtime, 0, len(list))
	for _, i := range list {
		out = append(out, s.downtimeView(i))
	}
	return out, nil
}

// broadcast queues the same event to every active gateway.
func (s *Service) broadcast(ctx context.Context, tx *store.Store, id, typ string, data any) error {
	tenants, err := tx.ListTenants(ctx)
	if err != nil {
		return err
	}
	for _, t := range tenants {
		if t.Status != "ACTIVE" {
			continue
		}
		if err := s.enqueue(ctx, tx, t.ID, id+":"+t.ID, typ, data); err != nil {
			return err
		}
	}
	return nil
}
