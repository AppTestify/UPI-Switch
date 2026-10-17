// Package intent builds signed upi://pay intent URIs and their QR codes.
package intent

import (
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/AppTestify/UPI-Switch/internal/security"
)

// Params are the UPI deep-link parameters.
type Params struct {
	PayeeVPA  string // pa
	PayeeName string // pn
	MCC       string // mc
	TxnID     string // tid
	TxnRef    string // tr
	Note      string // tn
	Amount    string // am
	Currency  string // cu
	Mode      string // mode: 04 intent, 15/16 QR
	OrgID     string // orgid
	MinAmount string // mam (optional)
}

// order is fixed so the signed payload is deterministic.
func (p Params) pairs() [][2]string {
	kv := [][2]string{
		{"pa", p.PayeeVPA}, {"pn", p.PayeeName}, {"mc", p.MCC}, {"tid", p.TxnID}, {"tr", p.TxnRef},
		{"tn", p.Note}, {"am", p.Amount}, {"cu", p.Currency}, {"mode", p.Mode}, {"orgid", p.OrgID},
	}
	if p.MinAmount != "" {
		kv = append(kv, [2]string{"mam", p.MinAmount})
	}
	out := kv[:0]
	for _, x := range kv {
		if x[1] != "" {
			out = append(out, x)
		}
	}
	return out
}

func encode(pairs [][2]string) string {
	parts := make([]string, len(pairs))
	for i, x := range pairs {
		parts[i] = x[0] + "=" + url.QueryEscape(x[1])
	}
	return strings.Join(parts, "&")
}

// Build returns the signed intent URI: upi://pay?<params>&sign=<base64 RSA-SHA256 over params>.
func Build(p Params, signer security.Signer) (string, error) {
	if p.PayeeVPA == "" || p.TxnRef == "" {
		return "", errors.New("intent: payee VPA and txn ref are required")
	}
	q := encode(p.pairs())
	sig, err := signer.Sign([]byte(q))
	if err != nil {
		return "", fmt.Errorf("intent: sign: %w", err)
	}
	return "upi://pay?" + q + "&sign=" + url.QueryEscape(base64.StdEncoding.EncodeToString(sig)), nil
}

// Parse splits a upi://pay URI into its params and verifies the signature when pub is given.
func Parse(uri string, pub *rsa.PublicKey) (url.Values, error) {
	rest, ok := strings.CutPrefix(uri, "upi://pay?")
	if !ok {
		return nil, errors.New("intent: not a upi://pay URI")
	}
	signedPart, sigPart, hasSig := strings.Cut(rest, "&sign=")
	vals, err := url.ParseQuery(signedPart)
	if err != nil {
		return nil, err
	}
	if pub != nil {
		if !hasSig {
			return nil, errors.New("intent: unsigned URI")
		}
		s, err := url.QueryUnescape(sigPart)
		if err != nil {
			return nil, err
		}
		sig, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		if err := security.Verify(pub, []byte(signedPart), sig); err != nil {
			return nil, errors.New("intent: bad signature")
		}
	}
	return vals, nil
}

// QRPNG renders the URI as a PNG QR code.
func QRPNG(uri string, size int) ([]byte, error) {
	return qrcode.Encode(uri, qrcode.Medium, size)
}
