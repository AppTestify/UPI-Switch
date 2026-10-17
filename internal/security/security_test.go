package security

import "testing"

func TestVault(t *testing.T) {
	v := NewVault("test-key")
	enc := v.Encrypt("alice@okbank")
	if enc == "alice@okbank" || enc == v.Encrypt("alice@okbank") {
		t.Fatal("encryption must be randomised")
	}
	dec, err := v.Decrypt(enc)
	if err != nil || dec != "alice@okbank" {
		t.Fatalf("decrypt %q %v", dec, err)
	}
	if v.Token("Alice@OKBANK") != v.Token("alice@okbank") || v.Token("a@b") == NewVault("other").Token("a@b") {
		t.Fatal("token must be stable per key and case-insensitive")
	}
	if _, err := NewVault("other").Decrypt(enc); err == nil {
		t.Fatal("wrong key must fail")
	}
}

func TestMaskAndSign(t *testing.T) {
	if MaskVPA("rahulkumar@okbank") != "ra*******r@okbank" || MaskVPA("ab@x") != "**@x" || MaskMobile("9876543210") != "******3210" {
		t.Fatal("masking")
	}
	a := SignRequest("s", "1", "post", "/v1/x", []byte("{}"))
	if !EqualHex(a, SignRequest("s", "1", "POST", "/v1/x", []byte("{}"))) || EqualHex(a, SignRequest("s", "1", "POST", "/v1/x", []byte("{ }"))) {
		t.Fatal("request signature")
	}
}
