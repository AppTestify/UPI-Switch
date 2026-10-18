// Package risk applies per-tenant limits, payer velocity and MCC rules before a payment is created.
package risk

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
)

// Stats is what the engine needs from the ledger.
type Stats interface {
	// TenantVolumeSince sums non-failed payment amounts for the tenant since t.
	TenantVolumeSince(ctx context.Context, tenantID string, since time.Time) (int64, error)
	// PayerCountSince counts payments by a tokenised payer VPA for the tenant since t.
	PayerCountSince(ctx context.Context, tenantID, payerToken string, since time.Time) (int, error)
	// PayerCategoryVolumeSince sums a payer's non-failed payments to the given MCCs (nil = all) since t, across tenants.
	PayerCategoryVolumeSince(ctx context.Context, payerToken string, mccs []string, since time.Time) (int64, error)
}

// Check is the input to a risk decision.
type Check struct {
	Tenant      *domain.Tenant
	AmountPaise int64
	MCC         string
	PayerToken  string // empty for intent/QR where the payer is unknown at creation
	Verified    bool   // the merchant passed KYC, so NPCI's higher category limits apply
}

// Decision is the engine's verdict.
type Decision struct {
	Allow  bool   `json:"allow"`
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// Engine evaluates rules.
type Engine struct {
	Stats     Stats
	GlobalMCC []string // MCCs blocked for every tenant
	Location  *time.Location
	Now       func() time.Time
}

func deny(rule, format string, a ...any) Decision {
	return Decision{Allow: false, Rule: rule, Reason: fmt.Sprintf(format, a...)}
}

// Evaluate runs the rules in order of cost: static limits first, then ledger lookups.
func (e *Engine) Evaluate(ctx context.Context, c Check) (Decision, error) {
	l := c.Tenant.Limits
	if c.AmountPaise <= 0 {
		return deny("amount", "amount must be positive"), nil
	}
	if l.MinTxnPaise > 0 && c.AmountPaise < l.MinTxnPaise {
		return deny("min_amount", "amount below tenant minimum %s", domain.FormatAmount(l.MinTxnPaise)), nil
	}
	if l.MaxTxnPaise > 0 && c.AmountPaise > l.MaxTxnPaise {
		return deny("max_amount", "amount above tenant per-transaction limit %s", domain.FormatAmount(l.MaxTxnPaise)), nil
	}
	cat := domain.LimitsFor(c.MCC, c.Verified)
	if c.AmountPaise > cat.TxnPaise {
		if !c.Verified && domain.CategoryFor(c.MCC).TxnPaise > cat.TxnPaise {
			return deny("npci_category_limit", "amount above NPCI's %s limit for merchants without completed KYC (%s %s limit needs KYC)",
				domain.FormatAmount(cat.TxnPaise), domain.FormatAmount(domain.CategoryFor(c.MCC).TxnPaise), domain.CategoryFor(c.MCC).Name), nil
		}
		return deny("npci_category_limit", "amount above NPCI's per-transaction limit of %s for %s", domain.FormatAmount(cat.TxnPaise), cat.Name), nil
	}
	if c.MCC != "" && (slices.Contains(l.BlockedMCC, c.MCC) || slices.Contains(e.GlobalMCC, c.MCC)) {
		return deny("mcc", "merchant category %s is blocked", c.MCC), nil
	}
	now := time.Now()
	if e.Now != nil {
		now = e.Now()
	}
	loc := e.Location
	if loc == nil {
		loc = time.UTC
	}
	if l.DailyPaise > 0 {
		lt := now.In(loc)
		dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
		vol, err := e.Stats.TenantVolumeSince(ctx, c.Tenant.ID, dayStart)
		if err != nil {
			return Decision{}, err
		}
		if vol+c.AmountPaise > l.DailyPaise {
			return deny("daily_limit", "tenant daily limit %s would be exceeded", domain.FormatAmount(l.DailyPaise)), nil
		}
	}
	if c.PayerToken != "" {
		lt := now.In(loc)
		dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, loc)
		vol, err := e.Stats.PayerCategoryVolumeSince(ctx, c.PayerToken, cat.MCCList(), dayStart)
		if err != nil {
			return Decision{}, err
		}
		if vol+c.AmountPaise > cat.DailyPaise {
			return deny("npci_daily_limit", "customer would exceed NPCI's daily limit of %s for %s", domain.FormatAmount(cat.DailyPaise), cat.Name), nil
		}
	}
	if c.PayerToken != "" && l.VelocityPerVPA > 0 {
		win := time.Duration(l.VelocityWindowSecs) * time.Second
		if win <= 0 {
			win = 10 * time.Minute
		}
		n, err := e.Stats.PayerCountSince(ctx, c.Tenant.ID, c.PayerToken, now.Add(-win))
		if err != nil {
			return Decision{}, err
		}
		if n >= l.VelocityPerVPA {
			return deny("velocity", "payer exceeded %d payments in %s", l.VelocityPerVPA, win), nil
		}
	}
	return Decision{Allow: true}, nil
}
