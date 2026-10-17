// Package webhook delivers signed events from the outbox to payment gateways, with retries and a DLQ.
package webhook

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/metrics"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// Headers sent with every webhook.
const (
	HeaderEvent     = "X-ATF-Event"
	HeaderEventID   = "X-ATF-Event-Id"
	HeaderTimestamp = "X-ATF-Timestamp"
	HeaderSignature = "X-ATF-Signature"
)

// Dispatcher drains the outbox.
type Dispatcher struct {
	Store       *store.Store
	HTTP        *http.Client
	Log         *slog.Logger
	Metrics     *metrics.Registry
	MaxAttempts int
	Schedule    []time.Duration // retry delays by attempt; the last one repeats
}

func (d *Dispatcher) delay(attempt int) time.Duration {
	s := d.Schedule
	if len(s) == 0 {
		s = []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour}
	}
	if attempt-1 < len(s) {
		return s[attempt-1]
	}
	return s[len(s)-1]
}

// Run delivers due webhooks until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := d.DeliverDue(ctx); err != nil {
				d.Log.Error("webhook dispatch", "err", err)
			}
		}
	}
}

// DeliverDue sends every due webhook once and returns how many were attempted.
func (d *Dispatcher) DeliverDue(ctx context.Context) (int, error) {
	evts, err := d.Store.ClaimDueWebhooks(ctx, time.Now(), time.Minute, 100)
	if err != nil {
		return 0, err
	}
	for _, e := range evts {
		d.deliver(ctx, e)
	}
	return len(evts), nil
}

func (d *Dispatcher) deliver(ctx context.Context, e store.OutboxEvent) {
	max := d.MaxAttempts
	if max <= 0 {
		max = 8
	}
	attempts := e.Attempts + 1
	err := d.post(ctx, e)
	result := "delivered"
	switch {
	case err == nil:
		err = d.Store.MarkWebhook(ctx, e.ID, "DELIVERED", attempts, time.Now(), "")
	case attempts >= max:
		result = "dead"
		d.Log.Warn("webhook moved to DLQ", "event", e.EventID, "tenant", e.TenantID, "err", err)
		err = d.Store.MarkWebhook(ctx, e.ID, "DEAD", attempts, time.Now(), err.Error())
	default:
		result = "retry"
		err = d.Store.MarkWebhook(ctx, e.ID, "PENDING", attempts, time.Now().Add(d.delay(attempts)), err.Error())
	}
	if d.Metrics != nil {
		d.Metrics.Inc("upi_webhook_deliveries_total", "tenant", e.TenantID, "result", result)
	}
	if err != nil {
		d.Log.Error("webhook mark", "event", e.EventID, "err", err)
	}
}

func (d *Dispatcher) post(ctx context.Context, e store.OutboxEvent) error {
	t, err := d.Store.GetTenant(ctx, e.TenantID)
	if err != nil {
		return err
	}
	if t.WebhookURL == "" {
		return fmt.Errorf("tenant has no webhook_url")
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	body := []byte(e.Payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.WebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(HeaderEvent, e.EventType)
	req.Header.Set(HeaderEventID, e.EventID)
	req.Header.Set(HeaderTimestamp, ts)
	req.Header.Set(HeaderSignature, "sha256="+security.SignWebhook(t.WebhookSecret, ts, body))
	hc := d.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook endpoint returned HTTP %d", resp.StatusCode)
	}
	return nil
}
