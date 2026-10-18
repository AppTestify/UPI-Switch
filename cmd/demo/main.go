// Command demo drives a running switch end to end the way a payment gateway would: it onboards a
// tenant, takes an intent payment (paid through the bank simulator), a collect, a refund and an
// AutoPay mandate, then runs T+1 reconciliation and settlement. Webhooks land on a local receiver.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/AppTestify/UPI-Switch/internal/config"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/webhook"
)

var (
	switchURL  = config.Get("SWITCH_URL", "http://localhost:8080")
	adminToken = config.Get("ADMIN_TOKEN", "dev-admin-token-change-me")
	bankURLs   = map[string]string{
		"bank-a": config.Get("BANK_A_URL", "http://localhost:9101"),
		"bank-b": config.Get("BANK_B_URL", "http://localhost:9102"),
		"bank-c": config.Get("BANK_C_URL", "http://localhost:9103"),
	}
	hookAddr = config.Get("DEMO_WEBHOOK_ADDR", "127.0.0.1:9900")
	hookURL  = config.Get("DEMO_WEBHOOK_URL", "http://127.0.0.1:9900/webhooks")
)

type client struct{ keyID, secret string }

func call(method, url string, hdr map[string]string, body []byte) (int, map[string]any) {
	req, _ := http.NewRequest(method, url, bytes.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request failed:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

func (c client) do(method, path string, body any) map[string]any {
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	hdr := map[string]string{"Content-Type": "application/json", "X-Client-Id": c.keyID, "X-Timestamp": ts,
		"X-Signature": security.SignRequest(c.secret, ts, method, path, b), "Idempotency-Key": security.RandomToken(8)}
	code, out := call(method, switchURL+path, hdr, b)
	if code >= 400 {
		fmt.Fprintf(os.Stderr, "%s %s → %d %v\n", method, path, code, out)
		os.Exit(1)
	}
	return out
}

// try is do without exiting on an error response, for steps that show a rule refusing a request.
func (c client) try(method, path string, body any) map[string]any {
	b, _ := json.Marshal(body)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	hdr := map[string]string{"Content-Type": "application/json", "X-Client-Id": c.keyID, "X-Timestamp": ts,
		"X-Signature": security.SignRequest(c.secret, ts, method, path, b), "Idempotency-Key": security.RandomToken(8)}
	_, out := call(method, switchURL+path, hdr, b)
	return out
}

func admin(method, path string, body any) map[string]any {
	b, _ := json.Marshal(body)
	code, out := call(method, switchURL+path, map[string]string{"Authorization": "Bearer " + adminToken, "Content-Type": "application/json"}, b)
	if code >= 400 {
		fmt.Fprintf(os.Stderr, "%s %s → %d %v\n", method, path, code, out)
		os.Exit(1)
	}
	return out
}

func waitStatus(c client, txnID string, want ...string) map[string]any {
	deadline := time.Now().Add(90 * time.Second)
	for {
		t := c.do("GET", "/v1/upi/status/"+txnID, nil)["transaction"].(map[string]any)
		for _, w := range want {
			if t["status"] == w {
				return t
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintf(os.Stderr, "timed out waiting for %s; last %v\n", txnID, t["status"])
			os.Exit(1)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func step(n int, s string, a ...any) { fmt.Printf("\n[%d] "+s+"\n", append([]any{n}, a...)...) }

func main() {
	var mu sync.Mutex
	var events []string
	var secret string
	ln, err := net.Listen("tcp", hookAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "webhook listener:", err)
		os.Exit(1)
	}
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		ok := r.Header.Get(webhook.HeaderSignature) == "sha256="+security.SignWebhook(secret, r.Header.Get(webhook.HeaderTimestamp), body)
		events = append(events, fmt.Sprintf("%s (signature ok: %v)", r.Header.Get(webhook.HeaderEvent), ok))
	}))

	step(1, "Onboarding a payment gateway tenant from the admin API")
	t := admin("POST", "/admin/tenants", map[string]any{"name": "Demo PG", "vpa_prefix": "demo" + security.RandomToken(2), "webhook_url": hookURL, "fee_bps": 20,
		"require_kyc": false}) // sandbox: merchants are registered on their first payment
	mu.Lock()
	secret = t["webhook_secret"].(string)
	mu.Unlock()
	c := client{keyID: t["api_key_id"].(string), secret: t["hmac_secret"].(string)}
	fmt.Println("    tenant", t["tenant"].(map[string]any)["tenant_id"], "api key", c.keyID)

	ref := "DEMO-" + security.RandomToken(3)
	step(2, "Creating a dynamic QR / intent for ₹149.00")
	in := c.do("POST", "/v1/upi/intent", map[string]any{"merchant_ref": ref + "-1", "amount": "149.00", "merchant": map[string]string{"id": "store42", "name": "Store 42", "mcc": "5411"}, "qr": true})
	fmt.Println("    txn", in["txn_id"], "via", in["psp"], "\n    ", in["intent_uri"])

	step(3, "Customer scans and approves with UPI PIN (bank simulator)")
	b, _ := json.Marshal(map[string]string{"intent_uri": in["intent_uri"].(string), "payer_vpa": "rahul.kumar@okicici"})
	code, pay := call("POST", bankURLs[in["psp"].(string)]+"/sim/pay", map[string]string{"Content-Type": "application/json"}, b)
	fmt.Println("    bank:", code, pay["result"], "rrn", pay["rrn"])
	st := waitStatus(c, in["txn_id"].(string), "SUCCESS", "FAILED")
	fmt.Println("    switch status:", st["status"], "rrn", st["rrn"], "payer", st["payer_vpa"])

	step(4, "Collect request to alice@okaxis for ₹250.00")
	col := c.do("POST", "/v1/upi/collect", map[string]any{"merchant_ref": ref + "-2", "amount": "250.00", "payer_vpa": "alice@okaxis", "merchant": map[string]string{"id": "store42", "name": "Store 42", "mcc": "5411"}})
	st = waitStatus(c, col["txn_id"].(string), "SUCCESS", "FAILED")
	fmt.Println("    collect", col["txn_id"], "→", st["status"])

	step(5, "Partial refund of ₹50.00 on the collect")
	rf := c.do("POST", "/v1/upi/refund", map[string]any{"txn_id": col["txn_id"], "merchant_refund_ref": ref + "-R1", "amount": "50.00"})
	for i := 0; i < 60; i++ {
		rf = c.do("GET", "/v1/upi/refund/"+rf["refund_id"].(string), nil)
		if rf["status"] == "SUCCESS" || rf["status"] == "FAILED" {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("    refund", rf["refund_id"], "→", rf["status"])

	step(6, "UPI AutoPay: create a mandate and send the pre-debit notification")
	md := c.do("POST", "/v1/upi/mandate", map[string]any{"merchant_ref": ref + "-M", "payer_vpa": "carol@okaxis", "max_amount": "999.00", "frequency": "MONTHLY",
		"start_date": time.Now().Format("2006-01-02"), "end_date": time.Now().AddDate(1, 0, 0).Format("2006-01-02"), "merchant": map[string]string{"id": "ott", "name": "OTT Plus", "mcc": "4899"}})
	for i := 0; i < 60 && md["status"] == "PENDING"; i++ {
		time.Sleep(500 * time.Millisecond)
		md = c.do("GET", "/v1/upi/mandate/"+md["mandate_id"].(string), nil)
	}
	fmt.Println("    mandate", md["mandate_id"], "→", md["status"], "umn", md["umn"])
	if md["status"] == "ACTIVE" {
		// NPCI AutoPay: the payer is told at least 24 hours before each debit, so the debit itself runs tomorrow.
		debitOn := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
		n := c.do("POST", "/v1/upi/mandate/"+md["mandate_id"].(string)+"/notify", map[string]any{"amount": "199.00", "debit_date": debitOn})
		fmt.Println("    pre-debit notification", n["notification_id"], "→", n["status"], "for", debitOn)
		ex := c.try("POST", "/v1/upi/mandate/"+md["mandate_id"].(string)+"/execute", map[string]any{"merchant_ref": ref + "-M1", "amount": "199.00"})
		if e, ok := ex["error"].(map[string]any); ok {
			fmt.Println("    debit today is refused as NPCI requires:", e["code"])
		}
	}

	today := time.Now().In(mustLoc(config.Get("RECON_TZ", "Asia/Kolkata"))).Format("2006-01-02")
	step(7, "T+1 reconciliation for %s (ledger × bank MIS × NPCI raw)", today)
	rc := admin("POST", "/admin/recon/run", map[string]any{"date": today})
	run := rc["run"].(map[string]any)
	fmt.Printf("    run %v: %v items, summary %s\n", run["run_id"], rc["items"], mustJSON(run["summary"]))

	step(8, "T+1 settlement")
	stl := admin("POST", "/admin/settlements/run", map[string]any{"date": today})
	for _, l := range stl["settlements"].([]any) {
		m := l.(map[string]any)
		if m["tenant_id"] == t["tenant"].(map[string]any)["tenant_id"] {
			fmt.Printf("    merchant %v: gross %.2f, refunds %.2f, fees %.2f, net %.2f\n", m["merchant_id"],
				m["gross_paise"].(float64)/100, m["refund_paise"].(float64)/100, m["fee_paise"].(float64)/100, m["net_paise"].(float64)/100)
		}
	}

	time.Sleep(4 * time.Second)
	step(9, "Webhooks received by the payment gateway")
	mu.Lock()
	for _, e := range events {
		fmt.Println("    ", e)
	}
	mu.Unlock()
	fmt.Println("\nDone. Admin console:", config.Get("CONSOLE_URL", switchURL+"/admin/"))
}

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return l
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
