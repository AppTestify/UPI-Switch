// Command traffic keeps a demo switch busy with live UPI payments so the admin console moves while
// you present it. It takes over the seeded demo gateways (rotating their keys through the admin
// API), creates intents, QR and collect payments with the daily traffic curve, and pays them through
// the bank simulators with a realistic mix of outcomes. It also answers the gateways' webhooks.
//
//	SWITCH_URL=http://localhost:8080 ADMIN_TOKEN=... go run ./cmd/traffic
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	_ "time/tzdata"

	"github.com/AppTestify/UPI-Switch/internal/config"
	"github.com/AppTestify/UPI-Switch/internal/demodata"
	"github.com/AppTestify/UPI-Switch/internal/security"
)

var (
	switchURL  = config.Get("SWITCH_URL", "http://localhost:8080")
	adminToken = config.Get("ADMIN_TOKEN", "dev-admin-token-change-me")
	bankURLs   = map[string]string{
		"bank-a": config.Get("BANK_A_URL", "http://localhost:9101"),
		"bank-b": config.Get("BANK_B_URL", "http://localhost:9102"),
		"bank-c": config.Get("BANK_C_URL", "http://localhost:9103"),
	}
	hookAddr = config.Get("TRAFFIC_WEBHOOK_ADDR", "127.0.0.1:9900")
	client   = &http.Client{Timeout: 15 * time.Second}
	rng      = rand.New(rand.NewPCG(uint64(time.Now().UnixNano()), 11))
	rngMu    sync.Mutex
)

type gateway struct {
	demodata.Tenant
	keyID, secret string
}

func main() {
	tps, err := strconv.ParseFloat(config.Get("TRAFFIC_TPS", "0.15"), 64)
	if err != nil || tps <= 0 {
		fail(fmt.Errorf("TRAFFIC_TPS must be a positive number"))
	}
	loc, err := time.LoadLocation(config.Get("RECON_TZ", "Asia/Kolkata"))
	if err != nil {
		fail(err)
	}
	go http.ListenAndServe(hookAddr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))

	gws := connect()
	fmt.Printf("driving live traffic for %d gateways at ~%.2f payments/s (follows the IST daily curve); Ctrl-C to stop\n", len(gws), tps)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)
	sem := make(chan struct{}, 32)
	var ok, failed atomic.Int64
	report := time.NewTicker(time.Minute)
	defer report.Stop()
	for {
		rate := tps * demodata.HourWeight(time.Now().In(loc).Hour())
		wait := time.Duration(rand.ExpFloat64() / rate * float64(time.Second))
		select {
		case <-stop:
			fmt.Println("stopping")
			return
		case <-report.C:
			fmt.Printf("%s  %d payments created, %d errors\n", time.Now().Format("15:04"), ok.Load(), failed.Load())
		case <-time.After(wait):
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				if err := payment(gws); err != nil {
					failed.Add(1)
					fmt.Fprintln(os.Stderr, err)
					return
				}
				ok.Add(1)
			}()
		}
	}
}

// connect finds the seeded gateways and takes over their API keys. Without seeded data it onboards one.
func connect() []gateway {
	var out []gateway
	for _, t := range demodata.Tenants {
		code, body := call("POST", switchURL+"/admin/tenants/"+t.ID+"/rotate-keys", map[string]string{"Authorization": "Bearer " + adminToken}, nil)
		if code == http.StatusNotFound {
			continue
		}
		if code != http.StatusOK {
			fail(fmt.Errorf("rotate keys for %s: %d %v", t.ID, code, body))
		}
		_, tb := call("GET", switchURL+"/admin/tenants/"+t.ID, map[string]string{"Authorization": "Bearer " + adminToken}, nil)
		out = append(out, gateway{t, fmt.Sprint(tb["api_key_id"]), fmt.Sprint(body["hmac_secret"])})
	}
	if len(out) > 0 {
		return out
	}
	t := demodata.Tenants[0]
	t.Prefix = "live" + security.RandomToken(2)
	b, _ := json.Marshal(map[string]any{"name": "Live Demo PG", "vpa_prefix": t.Prefix, "fee_bps": 20, "webhook_url": "http://" + hookAddr + "/webhooks",
		"require_kyc": false})
	code, body := call("POST", switchURL+"/admin/tenants", map[string]string{"Authorization": "Bearer " + adminToken, "Content-Type": "application/json"}, b)
	if code != http.StatusCreated {
		fail(fmt.Errorf("onboard: %d %v", code, body))
	}
	t.Weight = 1
	return []gateway{{t, fmt.Sprint(body["api_key_id"]), fmt.Sprint(body["hmac_secret"])}}
}

// payer picks a customer; the simulator decides outcomes from keywords in the VPA.
func payer() string {
	rngMu.Lock()
	defer rngMu.Unlock()
	v := demodata.PayerVPA(rng)
	kw := ""
	switch x := rng.Float64(); {
	case x < 0.026:
		kw = "nofunds"
	case x < 0.041:
		kw = "badpin"
	case x < 0.047:
		kw = "timeout"
	case x < 0.050:
		kw = "deemed"
	}
	if kw != "" { // keep the keyword inside the masked part of the VPA
		v = v[:3] + kw + v[3:]
	}
	return v
}

func payment(gws []gateway) error {
	rngMu.Lock()
	t := demodata.PickTenant(rng)
	var g gateway
	for _, c := range gws {
		if c.ID == t.ID {
			g = c
		}
	}
	if g.ID == "" {
		g = gws[rng.IntN(len(gws))]
	}
	m := g.Merchants[rng.IntN(len(g.Merchants))]
	amount := demodata.Amount(rng, m)
	collect := rng.IntN(8) == 0
	qr := rng.IntN(3) == 0
	abandon := rng.IntN(60) == 0
	rngMu.Unlock()

	merchant := map[string]string{"id": m.ID, "name": m.Name, "mcc": m.MCC}
	ref := fmt.Sprintf("LIVE-%s-%s", time.Now().Format("150405"), security.RandomToken(3))
	amt := fmt.Sprintf("%d.%02d", amount/100, amount%100)
	if collect {
		_, err := g.do("POST", "/v1/upi/collect", map[string]any{"merchant_ref": ref, "amount": amt, "payer_vpa": payer(), "merchant": merchant})
		return err
	}
	in, err := g.do("POST", "/v1/upi/intent", map[string]any{"merchant_ref": ref, "amount": amt, "merchant": merchant, "qr": qr})
	if err != nil || abandon {
		return err
	}
	time.Sleep(time.Duration(2+math.Floor(rand.Float64()*6)) * time.Second) // the customer opens their UPI app and enters the PIN
	b, _ := json.Marshal(map[string]string{"intent_uri": fmt.Sprint(in["intent_uri"]), "payer_vpa": payer()})
	bank := bankURLs[fmt.Sprint(in["psp"])]
	if bank == "" {
		return fmt.Errorf("no simulator URL for %v", in["psp"])
	}
	if code, out := call("POST", bank+"/sim/pay", map[string]string{"Content-Type": "application/json"}, b); code >= 300 {
		return fmt.Errorf("sim pay: %d %v", code, out)
	}
	return nil
}

func (g gateway) do(method, path string, body any) (map[string]any, error) {
	b, _ := json.Marshal(body)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	code, out := call(method, switchURL+path, map[string]string{"Content-Type": "application/json", "X-Client-Id": g.keyID, "X-Timestamp": ts,
		"X-Signature": security.SignRequest(g.secret, ts, method, path, b), "Idempotency-Key": security.RandomToken(8)}, b)
	if code >= 300 {
		return nil, fmt.Errorf("%s %s: %d %v", method, path, code, out)
	}
	return out, nil
}

func call(method, url string, hdr map[string]string, body []byte) (int, map[string]any) {
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "traffic:", err)
	os.Exit(1)
}
