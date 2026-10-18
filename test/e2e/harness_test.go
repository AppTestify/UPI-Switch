// Package e2e runs the whole switch against PostgreSQL, two simulated sponsor banks and a
// webhook receiver. Set TEST_DATABASE_URL to a disposable database; the schema is reset.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/AppTestify/UPI-Switch/internal/app"
	"github.com/AppTestify/UPI-Switch/internal/config"
	"github.com/AppTestify/UPI-Switch/internal/pspsim"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
	"github.com/AppTestify/UPI-Switch/internal/webhook"
)

const (
	adminToken      = "admin-token-0123456789"
	viewerToken     = "viewer-token-0123456789"
	consolePassword = "console-pass-123"
)

type harness struct {
	t        *testing.T
	app      *app.App
	url      string
	sims     map[string]*pspsim.Sim
	simURLs  map[string]string
	hooks    *receiver
	hooksURL string
	simKeys  map[string]*security.SoftHSM
}

// receiver is a payment gateway's webhook endpoint that verifies signatures.
type receiver struct {
	mu      sync.Mutex
	secret  string
	events  []map[string]any
	badSigs int
	fail    atomic.Bool
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if rc.fail.Load() {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	want := "sha256=" + security.SignWebhook(rc.secret, r.Header.Get(webhook.HeaderTimestamp), body)
	if r.Header.Get(webhook.HeaderSignature) != want {
		rc.badSigs++
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var e map[string]any
	json.Unmarshal(body, &e)
	rc.events = append(rc.events, e)
	w.WriteHeader(http.StatusOK)
}

func (rc *receiver) find(typ, entityID string) map[string]any {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	for _, e := range rc.events {
		if e["type"] != typ {
			continue
		}
		d, _ := e["data"].(map[string]any)
		for _, k := range []string{"txn_id", "refund_id", "mandate_id", "dispute_id", "merchant_id", "psp_id"} {
			if entityID == "" || d[k] == entityID {
				return e
			}
		}
	}
	return nil
}

func resetDB(t *testing.T, url string) {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Skipf("TEST_DATABASE_URL not reachable: %v", err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
}

func newHarness(t *testing.T) *harness {
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		t.Skip("set TEST_DATABASE_URL to run end-to-end tests")
	}
	resetDB(t, dbURL)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("E2E_VERBOSE") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	switchSigner := security.GenerateSoftHSM("switch-test")

	var handler atomic.Value
	sw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.Load().(http.Handler).ServeHTTP(w, r)
	}))
	t.Cleanup(sw.Close)

	h := &harness{t: t, url: sw.URL, sims: map[string]*pspsim.Sim{}, simURLs: map[string]string{}, hooks: &receiver{}, simKeys: map[string]*security.SoftHSM{}}
	type simDef struct {
		id, org, handle string
		prio            int
	}
	defs := []simDef{{"bank-a", "BKA", "banka", 1}, {"bank-b", "BKB", "bankb", 2}}
	var seeds []store.PSPRecord
	for _, d := range defs {
		k := security.GenerateSoftHSM(d.id)
		sim := pspsim.New(pspsim.Config{ID: d.id, OrgID: d.org, Handle: d.handle, CallbackURL: sw.URL + "/v1/npci/callback/" + d.id}, k, switchSigner.Public(), log)
		srv := httptest.NewServer(sim.Handler())
		t.Cleanup(srv.Close)
		h.sims[d.id], h.simURLs[d.id], h.simKeys[d.id] = sim, srv.URL, k
		rec := store.PSPRecord{PublicKeyPEM: string(security.EncodePublicKeyPEM(k.Public()))}
		rec.ID, rec.Name, rec.Endpoint, rec.Handle, rec.CostBps, rec.Priority, rec.Enabled = d.id, d.id, srv.URL, d.handle, 0, d.prio, true
		seeds = append(seeds, rec)
	}
	hooks := httptest.NewServer(h.hooks)
	t.Cleanup(hooks.Close)

	cfg := config.Switch{
		DatabaseURL: dbURL, OrgID: "ATF", DataKey: "e2e-data-key",
		AdminTokens:  "root:admin:" + adminToken + ",audit:viewer:" + viewerToken,
		ConsoleUsers: "ops@example.com:ops:" + consolePassword + ",viewer@example.com:viewer:" + consolePassword,
		ReconTZ:      "UTC", PollEvery: time.Hour, WebhookEvery: time.Hour, HeartbeatEvery: time.Hour,
		FirstCheckAfter: 50 * time.Millisecond, IntentExpiry: 15 * time.Minute, MaxCheckAttempts: 2, AllowPeakMandates: true,
		BlockedMCC: []string{"6051"},
	}
	a, err := app.New(context.Background(), cfg, app.Options{Signer: switchSigner, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	for _, s := range seeds {
		if err := a.Store.UpsertPSP(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Service.ReloadPSPs(context.Background()); err != nil {
		t.Fatal(err)
	}
	a.Dispatcher.Schedule = []time.Duration{0}
	a.Dispatcher.MaxAttempts = 3
	handler.Store(a.Server.Handler())
	h.app = a
	h.hooksURL = hooks.URL
	return h
}

// ---- HTTP helpers ----

type tenantClient struct {
	h        *harness
	keyID    string
	secret   string
	tenantID string
}

type resp struct {
	Code   int
	Body   map[string]any
	Raw    []byte
	Header http.Header
}

func (h *harness) do(method, path string, body any, hdr map[string]string) resp {
	h.t.Helper()
	var b []byte
	if body != nil {
		switch v := body.(type) {
		case []byte:
			b = v
		default:
			b, _ = json.Marshal(v)
		}
	}
	req, _ := http.NewRequest(method, h.url+path, bytes.NewReader(b))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	r, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{Code: r.StatusCode, Raw: raw, Header: r.Header}
	json.Unmarshal(raw, &out.Body)
	return out
}

func (h *harness) admin(method, path string, body any) resp {
	return h.do(method, path, body, map[string]string{"Authorization": "Bearer " + adminToken})
}

func (c *tenantClient) call(method, path string, body any, idemKey string) resp {
	c.h.t.Helper()
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	hdr := map[string]string{
		"X-Client-Id": c.keyID, "X-Timestamp": ts, "Content-Type": "application/json",
		"X-Signature": security.SignRequest(c.secret, ts, method, path, b),
	}
	if idemKey != "" {
		hdr["Idempotency-Key"] = idemKey
	}
	return c.h.do(method, path, b, hdr)
}

var idemSeq atomic.Int64

func (c *tenantClient) post(path string, body any) resp {
	return c.call(http.MethodPost, path, body, fmt.Sprintf("idem-%d-%d", time.Now().UnixNano(), idemSeq.Add(1)))
}

func (c *tenantClient) get(path string) resp { return c.call(http.MethodGet, path, nil, "") }

func (h *harness) onboard(name, prefix, webhookURL string) *tenantClient {
	h.t.Helper()
	r := h.admin("POST", "/admin/tenants", map[string]any{"name": name, "vpa_prefix": prefix, "webhook_url": webhookURL, "fee_bps": 30,
		"require_kyc": false}) // most tests pay merchants registered on their first payment
	if r.Code != http.StatusCreated {
		h.t.Fatalf("onboard: %d %s", r.Code, r.Raw)
	}
	tn := r.Body["tenant"].(map[string]any)
	if webhookURL == h.hooksURL {
		h.hooks.mu.Lock()
		h.hooks.secret = r.Body["webhook_secret"].(string)
		h.hooks.mu.Unlock()
	}
	tc := &tenantClient{h: h, keyID: r.Body["api_key_id"].(string), secret: r.Body["hmac_secret"].(string), tenantID: tn["tenant_id"].(string)}
	// Collect and AutoPay need a merchant registered beforehand; the tests use these for pull payments.
	for i, id := range []string{"m1", "m3", "ott"} {
		m := merchant(id)
		reg := tc.post("/v1/upi/merchants", map[string]any{"merchant_id": id, "legal_name": m["name"], "mcc": m["mcc"], "pan": fmt.Sprintf("ABCPE%04dF", 1000+i)})
		if reg.Code != http.StatusCreated {
			h.t.Fatalf("register merchant %s: %d %s", id, reg.Code, reg.Raw)
		}
	}
	return tc
}

// settle waits for simulator callbacks, then drains the poller and the webhook outbox.
func (h *harness) settle() {
	for _, s := range h.sims {
		s.Wait()
	}
	if _, err := h.app.Dispatcher.DeliverDue(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) poll() {
	time.Sleep(60 * time.Millisecond)
	if _, err := h.app.Service.PollOnce(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.settle()
}

func (h *harness) simPay(psp, uri, payer string) resp {
	h.t.Helper()
	b, _ := json.Marshal(map[string]string{"intent_uri": uri, "payer_vpa": payer})
	r, err := http.Post(h.simURLs[psp]+"/sim/pay", "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	defer r.Body.Close()
	raw, _ := io.ReadAll(r.Body)
	out := resp{Code: r.StatusCode, Raw: raw}
	json.Unmarshal(raw, &out.Body)
	return out
}

func (c *tenantClient) status(txnID string) map[string]any {
	c.h.t.Helper()
	r := c.get("/v1/upi/status/" + txnID)
	if r.Code != 200 {
		c.h.t.Fatalf("status %s: %d %s", txnID, r.Code, r.Raw)
	}
	return r.Body["transaction"].(map[string]any)
}

func merchant(id string) map[string]string {
	return map[string]string{"id": id, "name": "Chai Point " + id, "mcc": "5812"}
}

// errCode is the machine code of an API error response.
func errCode(r resp) string {
	e, _ := r.Body["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}
