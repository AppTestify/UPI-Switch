package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/security"
)

func TestSwitchEndToEnd(t *testing.T) {
	h := newHarness(t)
	pg := h.onboard("Acme Payments", "acmepg", h.hooksURL)
	today := time.Now().UTC().Format("2006-01-02")

	var intentTxn, collectTxn, lostTxn, deemedTxn string

	t.Run("intent payment succeeds and webhook is signed", func(t *testing.T) {
		r := pg.post("/v1/upi/intent", map[string]any{"merchant_ref": "ORD-1", "amount": "149.00", "merchant": merchant("m1"), "note": "Order 1"})
		if r.Code != http.StatusCreated {
			t.Fatalf("create intent: %d %s", r.Code, r.Raw)
		}
		intentTxn = r.Body["txn_id"].(string)
		uri := r.Body["intent_uri"].(string)
		if r.Body["status"] != "INITIATED" || !strings.HasPrefix(uri, "upi://pay?pa=acmepg.m1%40banka") || !strings.Contains(uri, "&sign=") {
			t.Fatalf("unexpected intent: %s", r.Raw)
		}
		pay := h.simPay("bank-a", uri, "rahul.kumar@okicici")
		if pay.Code != 200 || pay.Body["result"] != "SUCCESS" {
			t.Fatalf("sim pay: %d %s", pay.Code, pay.Raw)
		}
		h.settle()
		txn := pg.status(intentTxn)
		if txn["status"] != "SUCCESS" || txn["rrn"] == "" || txn["payer_vpa"] != "ra********r@okicici" || txn["amount"] != "149.00" {
			t.Fatalf("after pay: %v", txn)
		}
		if e := h.hooks.find("txn.success", intentTxn); e == nil {
			t.Fatal("txn.success webhook not delivered")
		}
		if h.hooks.badSigs != 0 {
			t.Fatal("webhook signature mismatch")
		}
	})

	t.Run("idempotency replays and rejects reuse", func(t *testing.T) {
		body := map[string]any{"merchant_ref": "ORD-IDEM", "amount": "10.00", "merchant": merchant("m1")}
		first := pg.call("POST", "/v1/upi/intent", body, "same-key")
		second := pg.call("POST", "/v1/upi/intent", body, "same-key")
		if first.Code != 201 || second.Code != 201 || second.Header.Get("Idempotent-Replayed") != "true" || first.Body["txn_id"] != second.Body["txn_id"] {
			t.Fatalf("replay: %d %d %s", first.Code, second.Code, second.Raw)
		}
		body["amount"] = "11.00"
		if r := pg.call("POST", "/v1/upi/intent", body, "same-key"); r.Code != 422 {
			t.Fatalf("reused key with new body: %d", r.Code)
		}
		if r := pg.call("POST", "/v1/upi/intent", body, ""); r.Code != 400 {
			t.Fatalf("missing key: %d", r.Code)
		}
		body["amount"] = "10.00"
		if r := pg.post("/v1/upi/intent", body); r.Code != 409 || r.Body["error"].(map[string]any)["txn_id"] != first.Body["txn_id"] {
			t.Fatalf("duplicate merchant_ref: %d %s", r.Code, r.Raw)
		}
	})

	t.Run("dynamic QR returns a PNG", func(t *testing.T) {
		r := pg.post("/v1/upi/qr", map[string]any{"merchant_ref": "ORD-QR", "amount": "5.00", "merchant": merchant("m2")})
		png, err := base64.StdEncoding.DecodeString(r.Body["qr_png_base64"].(string))
		if r.Code != 201 || err != nil || !bytes.HasPrefix(png, []byte("\x89PNG")) || r.Body["type"] != "QR" {
			t.Fatalf("qr: %d %v", r.Code, err)
		}
	})

	t.Run("customer decline marks intent FAILED", func(t *testing.T) {
		r := pg.post("/v1/upi/intent", map[string]any{"merchant_ref": "ORD-NOFUNDS", "amount": "99.00", "merchant": merchant("m1")})
		h.simPay("bank-a", r.Body["intent_uri"].(string), "nofunds@okaxis")
		h.settle()
		txn := pg.status(r.Body["txn_id"].(string))
		if txn["status"] != "FAILED" || txn["resp_code"] != npci.CodeInsufficientFunds {
			t.Fatalf("want FAILED Z9: %v", txn)
		}
		if h.hooks.find("txn.failed", r.Body["txn_id"].(string)) == nil {
			t.Fatal("txn.failed webhook missing")
		}
	})

	t.Run("collect is validated, sent and confirmed", func(t *testing.T) {
		r := pg.post("/v1/upi/collect", map[string]any{"merchant_ref": "COL-1", "amount": "250.00", "payer_vpa": "alice@okaxis", "merchant": merchant("m1")})
		if r.Code != 201 {
			t.Fatalf("collect: %d %s", r.Code, r.Raw)
		}
		collectTxn = r.Body["txn_id"].(string)
		h.settle()
		if txn := pg.status(collectTxn); txn["status"] != "SUCCESS" || txn["psp"] != "bank-a" {
			t.Fatalf("collect status: %v", txn)
		}
		bad := pg.post("/v1/upi/collect", map[string]any{"merchant_ref": "COL-BAD", "amount": "1.00", "payer_vpa": "invalid@okaxis", "merchant": merchant("m1")})
		if bad.Code != 422 || bad.Body["error"].(map[string]any)["code"] != "invalid_vpa" {
			t.Fatalf("invalid vpa: %d %s", bad.Code, bad.Raw)
		}
	})

	t.Run("lost callback is recovered by ReqChkTxn polling", func(t *testing.T) {
		r := pg.post("/v1/upi/collect", map[string]any{"merchant_ref": "COL-TIMEOUT", "amount": "75.00", "payer_vpa": "timeout@okaxis", "merchant": merchant("m1")})
		id := r.Body["txn_id"].(string)
		h.settle()
		if txn := pg.status(id); txn["status"] != "PENDING" {
			t.Fatalf("expected PENDING before poll: %v", txn)
		}
		h.poll()
		if txn := pg.status(id); txn["status"] != "SUCCESS" {
			t.Fatalf("expected SUCCESS after ReqChkTxn: %v", txn)
		}
	})

	t.Run("no final status becomes DEEMED", func(t *testing.T) {
		r := pg.post("/v1/upi/collect", map[string]any{"merchant_ref": "COL-LOST", "amount": "60.00", "payer_vpa": "lost@okaxis", "merchant": merchant("m3")})
		lostTxn = r.Body["txn_id"].(string)
		h.settle()
		h.poll()
		h.poll()
		h.poll()
		if txn := pg.status(lostTxn); txn["status"] != "DEEMED" {
			t.Fatalf("expected DEEMED: %v", txn)
		}
		r = pg.post("/v1/upi/collect", map[string]any{"merchant_ref": "COL-DEEMED", "amount": "40.00", "payer_vpa": "deemed@okaxis", "merchant": merchant("m3")})
		deemedTxn = r.Body["txn_id"].(string)
		h.settle()
		if txn := pg.status(deemedTxn); txn["status"] != "DEEMED" {
			t.Fatalf("expected DEEMED from bank: %v", txn)
		}
	})

	t.Run("refunds: online, over-refund, offline", func(t *testing.T) {
		r := pg.post("/v1/upi/refund", map[string]any{"txn_id": collectTxn, "merchant_refund_ref": "RF-1", "amount": "50.00"})
		if r.Code != 201 {
			t.Fatalf("refund: %d %s", r.Code, r.Raw)
		}
		rid := r.Body["refund_id"].(string)
		h.settle()
		if g := pg.get("/v1/upi/refund/" + rid); g.Body["status"] != "SUCCESS" || g.Body["rrn"] == "" {
			t.Fatalf("refund status: %s", g.Raw)
		}
		if h.hooks.find("refund.success", rid) == nil {
			t.Fatal("refund.success webhook missing")
		}
		over := pg.post("/v1/upi/refund", map[string]any{"txn_id": collectTxn, "merchant_refund_ref": "RF-2", "amount": "200.01"})
		if over.Code != 422 {
			t.Fatalf("over-refund: %d %s", over.Code, over.Raw)
		}
		off := pg.post("/v1/upi/refund", map[string]any{"txn_id": collectTxn, "merchant_refund_ref": "RF-OFF", "amount": "10.00", "mode": "OFFLINE"})
		if off.Body["status"] != "OFFLINE_QUEUED" {
			t.Fatalf("offline: %s", off.Raw)
		}
		file := h.admin("GET", "/admin/refunds/offline", nil)
		if !strings.Contains(string(file.Raw), off.Body["refund_id"].(string)) {
			t.Fatalf("offline file missing refund: %s", file.Raw)
		}
		res := h.admin("POST", "/admin/refunds/offline-results", map[string]any{"results": []map[string]string{{"refund_id": off.Body["refund_id"].(string), "status": "SUCCESS", "rrn": "999999999999"}}})
		if res.Body["applied"].(float64) != 1 {
			t.Fatalf("offline results: %s", res.Raw)
		}
		bulk := pg.post("/v1/upi/refund/bulk", map[string]any{"refunds": []map[string]string{
			{"txn_id": intentTxn, "merchant_refund_ref": "BRF-1", "amount": "9.00"},
			{"txn_id": "nope", "merchant_refund_ref": "BRF-2", "amount": "1.00"},
		}})
		results := bulk.Body["results"].([]any)
		if bulk.Code != 207 || results[0].(map[string]any)["refund"] == nil || results[1].(map[string]any)["error"] == nil {
			t.Fatalf("bulk: %d %s", bulk.Code, bulk.Raw)
		}
		h.settle()
	})

	t.Run("failover to the secondary bank when the primary is down", func(t *testing.T) {
		h.sims["bank-a"].SetDown(true)
		defer h.sims["bank-a"].SetDown(false)
		r := pg.post("/v1/upi/collect", map[string]any{"merchant_ref": "COL-FAILOVER", "amount": "20.00", "payer_vpa": "bob@okhdfc", "merchant": merchant("m1")})
		if r.Code != 201 {
			t.Fatalf("collect during outage: %d %s", r.Code, r.Raw)
		}
		h.settle()
		txn := pg.status(r.Body["txn_id"].(string))
		if txn["status"] != "SUCCESS" || txn["psp"] != "bank-b" || !strings.HasSuffix(txn["payee_vpa"].(string), "@bankb") {
			t.Fatalf("failover: %v", txn)
		}
		psps := h.admin("GET", "/admin/psps", nil)
		for _, hl := range psps.Body["health"].([]any) {
			m := hl.(map[string]any)
			if m["psp_id"] == "bank-a" && m["technical_failures"].(float64) == 0 {
				t.Fatalf("router did not record bank-a failures: %v", m)
			}
		}
		// New intents now route to the healthy bank.
		in := pg.post("/v1/upi/intent", map[string]any{"merchant_ref": "ORD-AFTER-OUTAGE", "amount": "3.00", "merchant": merchant("m1")})
		if in.Body["psp"] != "bank-b" {
			t.Fatalf("intent should route to bank-b during outage: %s", in.Raw)
		}
	})

	t.Run("UPI AutoPay mandate lifecycle", func(t *testing.T) {
		r := pg.post("/v1/upi/mandate", map[string]any{"merchant_ref": "MND-1", "payer_vpa": "carol@okaxis", "max_amount": "999.00",
			"frequency": "MONTHLY", "start_date": today, "end_date": time.Now().AddDate(1, 0, 0).Format("2006-01-02"), "merchant": merchant("ott")})
		if r.Code != 201 {
			t.Fatalf("mandate: %d %s", r.Code, r.Raw)
		}
		mid := r.Body["mandate_id"].(string)
		h.settle()
		m := pg.get("/v1/upi/mandate/" + mid)
		if m.Body["status"] != "ACTIVE" || m.Body["umn"] == "" {
			t.Fatalf("mandate not active: %s", m.Raw)
		}
		if over := pg.post("/v1/upi/mandate/"+mid+"/execute", map[string]any{"merchant_ref": "MND-1-X", "amount": "1000.00"}); over.Code != 422 {
			t.Fatalf("over mandate max: %d", over.Code)
		}
		ex := pg.post("/v1/upi/mandate/"+mid+"/execute", map[string]any{"merchant_ref": "MND-1-OCT", "amount": "499.00"})
		if ex.Code != 201 {
			t.Fatalf("execute: %d %s", ex.Code, ex.Raw)
		}
		h.settle()
		if txn := pg.status(ex.Body["txn_id"].(string)); txn["status"] != "SUCCESS" || txn["type"] != "MANDATE_DEBIT" {
			t.Fatalf("mandate debit: %v", txn)
		}
		if rv := pg.post("/v1/upi/mandate/"+mid+"/revoke", nil); rv.Body["status"] != "REVOKED" {
			t.Fatalf("revoke: %s", rv.Raw)
		}
		h.settle()
		if h.hooks.find("mandate.revoked", mid) == nil {
			t.Fatal("mandate.revoked webhook missing")
		}
	})

	t.Run("dispute raised through UDIR and resolved", func(t *testing.T) {
		r := pg.post("/v1/upi/disputes", map[string]any{"txn_id": intentTxn, "type": "RET", "reason_code": "R01", "remarks": "goods not received"})
		if r.Code != 201 {
			t.Fatalf("dispute: %d %s", r.Code, r.Raw)
		}
		h.settle()
		d := pg.get("/v1/upi/disputes/" + r.Body["dispute_id"].(string))
		if d.Body["status"] != "RESOLVED_ACCEPTED" {
			t.Fatalf("dispute: %s", d.Raw)
		}
	})

	t.Run("risk engine declines", func(t *testing.T) {
		big := pg.post("/v1/upi/intent", map[string]any{"merchant_ref": "ORD-BIG", "amount": "200000.00", "merchant": merchant("m1")})
		mcc := pg.post("/v1/upi/intent", map[string]any{"merchant_ref": "ORD-MCC", "amount": "1.00", "merchant": map[string]string{"id": "x", "name": "X", "mcc": "6051"}})
		if big.Code != 422 || mcc.Code != 422 || big.Body["error"].(map[string]any)["code"] != "risk_declined" {
			t.Fatalf("risk: %d %d %s", big.Code, mcc.Code, big.Raw)
		}
	})

	t.Run("authentication and tenant isolation", func(t *testing.T) {
		bad := *pg
		bad.secret = "wrong"
		if r := bad.get("/v1/upi/status/" + intentTxn); r.Code != 401 {
			t.Fatalf("bad signature: %d", r.Code)
		}
		other := h.onboard("Other PG", "otherpg", "http://127.0.0.1:1/unreachable")
		if r := other.get("/v1/upi/status/" + intentTxn); r.Code != 404 {
			t.Fatalf("cross-tenant read must be 404, got %d", r.Code)
		}
		if r := h.do("POST", "/admin/tenants", map[string]any{"name": "x", "vpa_prefix": "xx"}, map[string]string{"Authorization": "Bearer " + viewerToken}); r.Code != 403 {
			t.Fatalf("viewer onboarding must be forbidden: %d", r.Code)
		}
		// Webhooks to an unreachable endpoint end in the DLQ and can be replayed.
		o := other.post("/v1/upi/collect", map[string]any{"merchant_ref": "O-1", "amount": "1.00", "payer_vpa": "dave@okaxis", "merchant": merchant("m1")})
		h.settle()
		h.settle()
		h.settle()
		dlq := h.admin("GET", "/admin/webhooks?status=DEAD&tenant="+other.tenantID, nil)
		list := dlq.Body["webhooks"].([]any)
		if len(list) == 0 {
			t.Fatalf("expected DLQ entry for %s: %s", o.Body["txn_id"], dlq.Raw)
		}
		id := list[0].(map[string]any)["id"].(float64)
		if r := h.admin("POST", "/admin/webhooks/"+strconv.FormatInt(int64(id), 10)+"/replay", nil); r.Code != 200 {
			t.Fatalf("replay: %d %s", r.Code, r.Raw)
		}
	})

	t.Run("bank callbacks are verified and deduplicated", func(t *testing.T) {
		evil := security.GenerateSoftHSM("evil")
		m := npci.New(npci.APIRespPay, "EVL")
		m.Txn = &npci.Txn{ID: lostTxn, Ts: npci.Timestamp(time.Now()), Type: npci.TxnCollect}
		m.Resp = &npci.Resp{ReqMsgID: "x", Result: npci.ResultSuccess}
		doc, _ := npci.Marshal(m, evil)
		if r := h.do("POST", "/v1/npci/callback/bank-a", doc, nil); r.Code != 401 {
			t.Fatalf("forged callback: %d %s", r.Code, r.Raw)
		}
		if txn := pg.status(lostTxn); txn["status"] != "DEEMED" {
			t.Fatalf("forged callback changed state: %v", txn)
		}

		in := pg.post("/v1/upi/intent", map[string]any{"merchant_ref": "ORD-REPLAY", "amount": "12.00", "merchant": merchant("m4")})
		id, payee := in.Body["txn_id"].(string), in.Body["payee_vpa"].(string)
		cb := func(result, code string, ts time.Time) []byte {
			m := npci.New(npci.APIRespPay, "BKA")
			m.Head.Ts = npci.Timestamp(ts)
			m.Txn = &npci.Txn{ID: id, Ts: npci.Timestamp(ts), Type: npci.TxnPay, CustRef: "123456789012"}
			amt := &npci.Amount{Value: "12.00", Curr: "INR"}
			m.Payer = &npci.Party{Addr: "erin@okaxis", Amount: amt}
			m.Payees = &npci.Payees{Payee: []npci.Party{{Addr: payee, Amount: amt}}}
			m.Resp = &npci.Resp{ReqMsgID: "r", Result: result, ErrCode: code}
			doc, _ := npci.Marshal(m, h.simKeys["bank-a"])
			return doc
		}
		if r := h.do("POST", "/v1/npci/callback/bank-a", cb(npci.ResultSuccess, "", time.Now().Add(-10*time.Minute)), nil); r.Code != 400 {
			t.Fatalf("stale callback must be rejected: %d", r.Code)
		}
		good := cb(npci.ResultSuccess, "", time.Now())
		for i := 0; i < 2; i++ {
			if r := h.do("POST", "/v1/npci/callback/bank-a", good, nil); r.Code != 200 || !strings.Contains(string(r.Raw), `result="SUCCESS"`) {
				t.Fatalf("callback %d: %d %s", i, r.Code, r.Raw)
			}
		}
		if txn := pg.status(id); txn["status"] != "SUCCESS" || txn["rrn"] != "123456789012" {
			t.Fatalf("valid callback: %v", txn)
		}
		// A later conflicting FAILURE for a settled txn does not flip it.
		h.do("POST", "/v1/npci/callback/bank-a", cb(npci.ResultFailure, npci.CodeInsufficientFunds, time.Now()), nil)
		if txn := pg.status(id); txn["status"] != "SUCCESS" {
			t.Fatalf("final state must not change: %v", txn)
		}
		if m := h.do("GET", "/metrics", nil, nil); !strings.Contains(string(m.Raw), `upi_callbacks_total{psp="bank-a",result="duplicate"} 1`) {
			t.Fatalf("duplicate callback not counted: %s", m.Raw)
		}
	})

	t.Run("T+1 recon closes deemed txns, raises TCC, and settlement nets refunds and fees", func(t *testing.T) {
		r := h.admin("POST", "/admin/recon/run", map[string]any{"date": today})
		if r.Code != 200 {
			t.Fatalf("recon: %d %s", r.Code, r.Raw)
		}
		h.settle()
		if txn := pg.status(deemedTxn); txn["status"] != "SUCCESS" {
			t.Fatalf("deemed txn should resolve SUCCESS from NPCI file: %v", txn)
		}
		if txn := pg.status(lostTxn); txn["status"] != "FAILED" {
			t.Fatalf("lost txn should resolve FAILED: %v", txn)
		}
		ds := h.admin("GET", "/admin/disputes", nil)
		foundTCC := false
		for _, d := range ds.Body["disputes"].([]any) {
			dm := d.(map[string]any)
			if dm["txn_id"] == lostTxn && dm["type"] == "TCC" && dm["status"] == "RESOLVED_ACCEPTED" {
				foundTCC = true
			}
		}
		if !foundTCC {
			t.Fatalf("expected resolved TCC for lost txn: %s", ds.Raw)
		}
		rep := pg.get("/v1/upi/reports/recon?date=" + today + "&all=true")
		if rep.Code != 200 || len(rep.Body["items"].([]any)) == 0 {
			t.Fatalf("tenant recon report: %d %s", rep.Code, rep.Raw)
		}
		if h.hooks.find("recon.report", "") == nil {
			t.Fatal("recon.report webhook missing")
		}

		st := h.admin("POST", "/admin/settlements/run", map[string]any{"date": today})
		if st.Code != 200 {
			t.Fatalf("settlement: %d %s", st.Code, st.Raw)
		}
		var m1 map[string]any
		for _, l := range st.Body["settlements"].([]any) {
			lm := l.(map[string]any)
			if lm["tenant_id"] == pg.tenantID && lm["merchant_id"] == "m1" {
				m1 = lm
			}
		}
		if m1 == nil {
			t.Fatalf("no m1 settlement: %s", st.Raw)
		}
		gross, refunds, fee, net := m1["gross_paise"].(float64), m1["refund_paise"].(float64), m1["fee_paise"].(float64), m1["net_paise"].(float64)
		if gross <= 0 || refunds != 6900 || fee != float64(int64(gross)*30/10000) || net != gross-refunds-fee {
			t.Fatalf("settlement math: %v", m1)
		}
		csv := h.admin("GET", "/admin/settlements?format=csv&date="+time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02"), nil)
		if !strings.Contains(string(csv.Raw), pg.tenantID) {
			t.Fatalf("payout file: %s", csv.Raw)
		}
	})

	t.Run("audit chain, metrics, admin console", func(t *testing.T) {
		v := h.admin("GET", "/admin/audit/verify", nil)
		if v.Body["intact"] != true || v.Body["entries"].(float64) < 5 {
			t.Fatalf("audit: %s", v.Raw)
		}
		if _, err := h.app.Pool.Exec(context.Background(), `UPDATE audit_log SET actor='x'`); err == nil {
			t.Fatal("audit log must reject updates")
		}
		m := h.do("GET", "/metrics", nil, nil)
		if !strings.Contains(string(m.Raw), `upi_txn_transitions_total{kind="payment",to="SUCCESS"}`) {
			t.Fatalf("metrics: %s", m.Raw)
		}
		if c := h.do("GET", "/admin/", nil, nil); c.Code != 200 || !strings.Contains(string(c.Raw), "UPI Switch Console") {
			t.Fatalf("console: %d", c.Code)
		}
	})

	t.Run("console sign-in, session RBAC and analytics", func(t *testing.T) {
		if r := h.do("POST", "/admin/login", map[string]string{"email": "ops@example.com", "password": "wrong-password"}, nil); r.Code != 401 {
			t.Fatalf("bad password: %d", r.Code)
		}
		time.Sleep(400 * time.Millisecond) // sign-in attempts are rate limited per IP
		login := h.do("POST", "/admin/login", map[string]string{"email": "OPS@example.com", "password": consolePassword}, nil)
		if login.Code != 200 || login.Body["role"] != "ops" {
			t.Fatalf("login: %d %s", login.Code, login.Raw)
		}
		var cookie string
		for _, c := range login.Header.Values("Set-Cookie") {
			if strings.HasPrefix(c, "atf_session=") {
				cookie, _, _ = strings.Cut(c, ";")
				if !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Strict") {
					t.Fatalf("cookie flags: %s", c)
				}
			}
		}
		if cookie == "" {
			t.Fatal("no session cookie")
		}
		sess := map[string]string{"Cookie": cookie}
		if me := h.do("GET", "/admin/me", nil, sess); me.Code != 200 || me.Body["email"] != "ops@example.com" {
			t.Fatalf("me: %d %s", me.Code, me.Raw)
		}
		if r := h.do("GET", "/admin/me", nil, map[string]string{"Cookie": cookie + "x"}); r.Code != 401 {
			t.Fatalf("tampered cookie: %d", r.Code)
		}
		// A cookie-authenticated write without the console header is refused (CSRF), with it the ops role applies.
		if r := h.do("POST", "/admin/webhooks/1/replay", nil, sess); r.Code != 403 {
			t.Fatalf("csrf: %d", r.Code)
		}
		sess["X-ATF-Console"] = "1"
		if r := h.do("POST", "/admin/tenants", map[string]any{"name": "x", "vpa_prefix": "xx"}, sess); r.Code != 403 {
			t.Fatalf("ops must not onboard tenants: %d", r.Code)
		}

		ov := h.do("GET", "/admin/stats/overview?range=today", nil, sess)
		if ov.Code != 200 {
			t.Fatalf("overview: %d %s", ov.Code, ov.Raw)
		}
		tot := ov.Body["totals"].(map[string]any)
		if tot["count"].(float64) < 5 || tot["success"].(float64) < 1 || ov.Body["success_rate"].(float64) <= 0 {
			t.Fatalf("overview totals: %s", ov.Raw)
		}
		if len(ov.Body["series"].([]any)) == 0 || len(ov.Body["psps"].([]any)) != 2 || len(ov.Body["tenants"].([]any)) == 0 {
			t.Fatalf("overview breakdowns: %s", ov.Raw)
		}
		var sum float64
		for _, b := range ov.Body["series"].([]any) {
			sum += b.(map[string]any)["count"].(float64)
		}
		if sum != tot["count"].(float64) {
			t.Fatalf("series sums to %v, totals say %v", sum, tot["count"])
		}
		for _, d := range ov.Body["declines"].([]any) {
			d := d.(map[string]any)
			if (d["code"] == "Z9" && d["kind"] != "BD") || (d["code"] == "U90" && d["kind"] != "TD") {
				t.Fatalf("decline classification: %v", d)
			}
		}

		found := h.do("GET", "/admin/txns?q="+intentTxn, nil, sess)
		if found.Code != 200 || found.Body["total"].(float64) != 1 || found.Body["transactions"].([]any)[0].(map[string]any)["txn_id"] != intentTxn {
			t.Fatalf("search: %s", found.Raw)
		}
		page := h.do("GET", "/admin/txns?limit=2&offset=1&tenant="+pg.tenantID, nil, sess)
		if page.Code != 200 || len(page.Body["transactions"].([]any)) != 2 || page.Body["total"].(float64) < 3 {
			t.Fatalf("paging: %s", page.Raw)
		}
		if r := h.do("POST", "/admin/logout", nil, sess); r.Code != 204 || !strings.Contains(r.Header.Get("Set-Cookie"), "Max-Age=0") {
			t.Fatalf("logout: %d %v", r.Code, r.Header)
		}
	})
}
