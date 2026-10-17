package recon

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestParseCSV(t *testing.T) {
	data := []byte("txn_id,rrn,type,amount,status,date\nATF1,123,PAYMENT,10.50,success,2026-08-01\n")
	sum := sha256.Sum256(data)
	rows, err := ParseCSV(data, hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].AmountPaise != 1050 || rows[0].Status != "SUCCESS" {
		t.Fatalf("rows %+v", rows)
	}
	if _, err := ParseCSV(data, "deadbeef"); err == nil {
		t.Fatal("checksum mismatch must fail")
	}
	if _, err := ParseCSV([]byte("ATF1,1,PAYMENT\n"), ""); err == nil {
		t.Fatal("short line must fail")
	}
}
