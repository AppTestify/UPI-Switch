package npci

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/AppTestify/UPI-Switch/internal/security"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	key := security.GenerateSoftHSM("k1")
	m := New(APIReqPay, "ATF")
	m.Txn = &Txn{ID: NewTxnID("ATF"), Ts: "2026-08-01T10:00:00.000+05:30", Type: TxnCollect, Note: "order 1"}
	m.Payer = &Party{Addr: "alice@okbank", Amount: &Amount{Value: "10.00", Curr: "INR"}}
	m.Payees = &Payees{Payee: []Party{{Addr: "pg.m1@banka", Code: "5411"}}}
	doc, err := Marshal(m, key)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "<Signature>") {
		t.Fatal("no signature element")
	}
	got, err := Parse(doc, key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if got.API() != APIReqPay || got.Payer.Addr != "alice@okbank" || got.Payee().Code != "5411" || got.Txn.ID != m.Txn.ID {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	tampered := bytes.Replace(doc, []byte(`value="10.00"`), []byte(`value="99.00"`), 1)
	if _, err := Parse(tampered, key.Public()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("tampered message must fail verification, got %v", err)
	}
	other := security.GenerateSoftHSM("k2")
	if _, err := Parse(doc, other.Public()); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("wrong key must fail, got %v", err)
	}
}

func TestIDs(t *testing.T) {
	id := NewTxnID("ATF")
	if len(id) != 35 || !strings.HasPrefix(id, "ATF") {
		t.Fatalf("txn id %q", id)
	}
	if r := NewRRN(); len(r) != 12 {
		t.Fatalf("rrn %q", r)
	}
	if !IsTechnicalDecline(CodeBankDown) || IsTechnicalDecline(CodeInsufficientFunds) {
		t.Fatal("technical decline classification")
	}
}
