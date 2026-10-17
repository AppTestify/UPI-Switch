package intent

import (
	"strings"
	"testing"

	"github.com/AppTestify/UPI-Switch/internal/security"
)

func TestBuildParse(t *testing.T) {
	k := security.GenerateSoftHSM("k")
	uri, err := Build(Params{PayeeVPA: "pg.m1@banka", PayeeName: "Chai & Co", MCC: "5812", TxnID: "ATF1", TxnRef: "ATF1", Note: "Order #1", Amount: "49.00", Currency: "INR", Mode: "04"}, k)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(uri, "upi://pay?pa=pg.m1%40banka&pn=Chai+%26+Co") {
		t.Fatalf("unexpected uri %s", uri)
	}
	v, err := Parse(uri, k.Public())
	if err != nil {
		t.Fatal(err)
	}
	if v.Get("am") != "49.00" || v.Get("pn") != "Chai & Co" || v.Get("tr") != "ATF1" {
		t.Fatalf("params %v", v)
	}
	if _, err := Parse(strings.Replace(uri, "am=49.00", "am=1.00", 1), k.Public()); err == nil {
		t.Fatal("tampered amount must fail")
	}
	png, err := QRPNG(uri, 256)
	if err != nil || len(png) < 100 || string(png[1:4]) != "PNG" {
		t.Fatalf("qr: %v", err)
	}
}
