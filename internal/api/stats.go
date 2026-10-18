package api

import (
	"cmp"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/core"
	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/router"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

type pspStat struct {
	store.GroupStat
	Name       string         `json:"name"`
	TD         int64          `json:"technical_declines"`
	SR         float64        `json:"success_rate"`
	TDRate     float64        `json:"td_rate"`
	Share      float64        `json:"share"`
	Health     *router.Health `json:"health,omitempty"`
	Enabled    bool           `json:"enabled"`
	Configured bool           `json:"configured"`
}

type groupOut struct {
	store.GroupStat
	Name  string  `json:"name"`
	SR    float64 `json:"success_rate"`
	Share float64 `json:"share"`
}

type declineOut struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
	Paise  int64  `json:"paise"`
	Kind   string `json:"kind"` // TD (technical, counts against the bank) or BD (business, the customer's doing)
}

// isTechnical classifies a decline the way NPCI reports it (technical vs business decline). It is wider
// than npci.IsTechnicalDecline, which only decides failover.
func isTechnical(code string) bool { return domain.IsTechnicalDecline(code) }

var technicalCodes = domain.TechnicalDeclineCodes

// ratio returns a/b rounded to four places, or 0.
func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*10000) / 10000
}

// statsWindow turns a range preset into [from, to), the bucket size and the previous window's start.
func statsWindow(rng string, now time.Time, loc *time.Location) (from, to, prevFrom time.Time, bucket string) {
	to = now
	switch rng {
	case "today":
		l := now.In(loc)
		from = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
		return from, to, from.Add(-24 * time.Hour), "hour"
	case "7d":
		from, bucket = now.Add(-7*24*time.Hour), "hour"
	case "30d":
		from, bucket = now.Add(-30*24*time.Hour), "day"
	default:
		from, bucket = now.Add(-24*time.Hour), "hour"
	}
	return from, to, from.Add(-to.Sub(from)), bucket
}

// fillSeries adds empty buckets so charts show quiet hours as zero rather than skipping them.
func fillSeries(got []store.Bucket, from, to time.Time, bucket string, loc *time.Location) []store.Bucket {
	idx := map[int64]store.Bucket{}
	for _, b := range got {
		idx[b.At.Unix()] = b
	}
	l := from.In(loc)
	cur := time.Date(l.Year(), l.Month(), l.Day(), l.Hour(), 0, 0, 0, loc)
	step := func(t time.Time) time.Time { return t.Add(time.Hour) }
	if bucket == "day" {
		cur = time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	}
	out := []store.Bucket{}
	for ; cur.Before(to); cur = step(cur) {
		b, ok := idx[cur.Unix()]
		if !ok {
			b = store.Bucket{At: cur}
		}
		out = append(out, b)
	}
	return out
}

// overview powers the console's landing dashboard: headline KPIs with the previous period for
// comparison, a volume/success time series, sponsor-bank and gateway breakdowns, the decline mix
// split into technical (TD) and business (BD) declines, live throughput and open exceptions.
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	loc := s.cfg.Location
	now := time.Now()
	rng := q.Get("range")
	if rng != "today" && rng != "7d" && rng != "30d" {
		rng = "24h"
	}
	from, to, prevFrom, bucket := statsWindow(rng, now, loc)
	f := store.StatsFilter{From: from, To: to, TenantID: q.Get("tenant"), Bucket: bucket, TZ: loc.String(), TDCodes: technicalCodes}
	pf := f
	pf.From, pf.To = prevFrom, from

	// The queries are independent scans of the ledger, so run them side by side.
	var (
		totals, prev, lastMin, last15 store.Totals
		series                        []store.Bucket
		byPSP, byTenant, byType       []store.GroupStat
		declines, prevDeclines        []store.CodeStat
		refunds, refundPaise          int64
		deemed, stuck, disputes, dead int64
		psps                          []store.PSPRecord
		tenants                       []*domain.Tenant
		wg                            sync.WaitGroup
		mu                            sync.Mutex
		firstErr                      error
	)
	lf := f
	lf.From, lf.To = now.Add(-time.Minute), now.Add(time.Second)
	l15 := lf
	l15.From = now.Add(-15 * time.Minute)
	run := func(fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil {
				mu.Lock()
				firstErr = cmp.Or(firstErr, err)
				mu.Unlock()
			}
		}()
	}
	run(func() (err error) { totals, err = s.store.StatsTotals(ctx, f); return })
	run(func() (err error) { prev, err = s.store.StatsTotals(ctx, pf); return })
	run(func() (err error) { lastMin, err = s.store.StatsTotals(ctx, lf); return })
	run(func() (err error) { last15, err = s.store.StatsTotals(ctx, l15); return })
	run(func() (err error) { series, err = s.store.StatsSeries(ctx, f); return })
	run(func() (err error) { byPSP, err = s.store.StatsBy(ctx, f, "psp_id"); return })
	run(func() (err error) { byTenant, err = s.store.StatsBy(ctx, f, "tenant_id"); return })
	run(func() (err error) { byType, err = s.store.StatsBy(ctx, f, "type"); return })
	run(func() (err error) { declines, err = s.store.StatsDeclines(ctx, f); return })
	run(func() (err error) { prevDeclines, err = s.store.StatsDeclines(ctx, pf); return })
	run(func() (err error) { refunds, refundPaise, err = s.store.StatsRefunds(ctx, f); return })
	run(func() (err error) {
		deemed, stuck, disputes, dead, err = s.store.StatsOpenExceptions(ctx, f.TenantID)
		return
	})
	var incidents, kycPending, tatBreached int64
	run(func() (err error) {
		incidents, kycPending, tatBreached, err = s.store.StatsCompliance(ctx, f.TenantID, core.TATDeadline(now, loc).AddDate(0, 0, -2*core.TATDays-1))
		return
	})
	run(func() (err error) { psps, err = s.store.ListPSPs(ctx); return })
	run(func() (err error) { tenants, err = s.store.ListTenants(ctx); return })
	wg.Wait()
	if firstErr != nil {
		s.writeErr(w, r, firstErr)
		return
	}

	// Technical vs business declines, overall and per bank.
	tdByPSP := map[string]int64{}
	codes := map[string]*declineOut{}
	var td, bd, tdPaise int64
	for _, d := range declines {
		kind := "BD"
		if isTechnical(d.Code) {
			kind = "TD"
			tdByPSP[d.PSPID] += d.Count
			td += d.Count
			tdPaise += d.Paise
		} else {
			bd += d.Count
		}
		c := codes[d.Code]
		if c == nil {
			c = &declineOut{Code: d.Code, Reason: d.Reason, Kind: kind}
			codes[d.Code] = c
		}
		c.Count += d.Count
		c.Paise += d.Paise
	}
	var prevTD int64
	for _, d := range prevDeclines {
		if isTechnical(d.Code) {
			prevTD += d.Count
		}
	}
	declineList := make([]declineOut, 0, len(codes))
	for _, c := range codes {
		declineList = append(declineList, *c)
	}
	sort.Slice(declineList, func(i, j int) bool { return declineList[i].Count > declineList[j].Count })

	health := map[string]router.Health{}
	for _, h := range s.svc.Router().Health() {
		health[h.PSPID] = h
	}
	seen := map[string]bool{}
	pspOut := []pspStat{}
	addPSP := func(g store.GroupStat) {
		p := pspStat{GroupStat: g, Name: g.Key, TD: tdByPSP[g.Key], SR: ratio(g.Success, g.Count),
			TDRate: ratio(tdByPSP[g.Key], g.Count), Share: ratio(g.Count, totals.Count)}
		for _, rec := range psps {
			if rec.ID == g.Key {
				p.Name, p.Enabled, p.Configured = rec.Name, rec.Enabled, true
			}
		}
		if h, ok := health[g.Key]; ok {
			p.Health = &h
		}
		seen[g.Key] = true
		pspOut = append(pspOut, p)
	}
	for _, g := range byPSP {
		addPSP(g)
	}
	for _, rec := range psps {
		if !seen[rec.ID] {
			addPSP(store.GroupStat{Key: rec.ID})
		}
	}

	names := map[string]string{}
	for _, t := range tenants {
		names[t.ID] = t.Name
	}
	group := func(gs []store.GroupStat, named bool) []groupOut {
		out := make([]groupOut, 0, len(gs))
		for _, g := range gs {
			o := groupOut{GroupStat: g, Name: g.Key, SR: ratio(g.Success, g.Count), Share: ratio(g.Count, totals.Count)}
			if n, ok := names[g.Key]; named && ok {
				o.Name = n
			}
			out = append(out, o)
		}
		return out
	}

	var avgTicket, prevAvg int64
	if totals.Success > 0 {
		avgTicket = totals.VolumePaise / totals.Success
	}
	if prev.Success > 0 {
		prevAvg = prev.VolumePaise / prev.Success
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rng, "from": from, "to": to, "bucket": bucket, "timezone": loc.String(),
		"totals": totals, "previous": prev,
		"success_rate": ratio(totals.Success, totals.Count), "prev_success_rate": ratio(prev.Success, prev.Count),
		"td_rate": ratio(td, totals.Count), "bd_rate": ratio(bd, totals.Count), "prev_td_rate": ratio(prevTD, prev.Count),
		"technical_declines": td, "business_declines": bd, "td_lost_paise": tdPaise,
		"avg_ticket_paise": avgTicket, "prev_avg_ticket_paise": prevAvg,
		"refunds": map[string]int64{"count": refunds, "paise": refundPaise},
		"series":  fillSeries(series, from, to, bucket, loc),
		"psps":    pspOut, "tenants": group(byTenant, true), "types": group(byType, false), "declines": declineList,
		"live": map[string]any{"tps": math.Round(float64(lastMin.Count)/60*100) / 100, "last_minute": lastMin.Count,
			"success_rate_15m": ratio(last15.Success, last15.Success+last15.Failed), "count_15m": last15.Count},
		"open": map[string]int64{"deemed": deemed, "stuck_pending": stuck, "disputes": disputes, "dead_webhooks": dead,
			"bank_incidents": incidents, "kyc_pending": kycPending, "tat_breached": tatBreached},
	})
}
