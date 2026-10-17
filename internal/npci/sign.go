package npci

import (
	"bytes"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"

	"github.com/AppTestify/UPI-Switch/internal/security"
)

var (
	sigOpen  = []byte("<Signature>")
	sigClose = []byte("</Signature>")
)

// Marshal signs m with signer and returns the signed document bytes.
func Marshal(m *Message, signer security.Signer) ([]byte, error) {
	m.Signature = nil
	body, err := xml.Marshal(m)
	if err != nil {
		return nil, err
	}
	sig, err := signer.Sign(body)
	if err != nil {
		return nil, fmt.Errorf("hsm sign: %w", err)
	}
	digest := sha256.Sum256(body)
	sb, err := xml.Marshal(Signature{
		KeyID:          signer.KeyID(),
		DigestMethod:   "SHA256",
		DigestValue:    base64.StdEncoding.EncodeToString(digest[:]),
		SignatureValue: base64.StdEncoding.EncodeToString(sig),
	})
	if err != nil {
		return nil, err
	}
	// Insert the signature as the last child of the root element.
	end := bytes.LastIndex(body, []byte("</"))
	if end < 0 {
		return nil, errors.New("malformed message")
	}
	out := make([]byte, 0, len(body)+len(sb))
	out = append(out, body[:end]...)
	out = append(out, sb...)
	out = append(out, body[end:]...)
	return out, nil
}

// ErrBadSignature is returned when a message signature does not verify.
var ErrBadSignature = errors.New("npci: signature verification failed")

// Parse verifies the enveloped signature with pub and decodes the message.
// If pub is nil the signature is not checked (only for diagnostics).
func Parse(doc []byte, pub *rsa.PublicKey) (*Message, error) {
	var m Message
	if err := xml.Unmarshal(doc, &m); err != nil {
		return nil, fmt.Errorf("npci: decode: %w", err)
	}
	if pub == nil {
		return &m, nil
	}
	if m.Signature == nil {
		return nil, fmt.Errorf("%w: missing signature", ErrBadSignature)
	}
	start := bytes.LastIndex(doc, sigOpen)
	stop := bytes.LastIndex(doc, sigClose)
	if start < 0 || stop < start {
		return nil, fmt.Errorf("%w: signature element not found", ErrBadSignature)
	}
	signed := make([]byte, 0, len(doc))
	signed = append(signed, doc[:start]...)
	signed = append(signed, doc[stop+len(sigClose):]...)
	digest := sha256.Sum256(signed)
	if base64.StdEncoding.EncodeToString(digest[:]) != m.Signature.DigestValue {
		return nil, fmt.Errorf("%w: digest mismatch", ErrBadSignature)
	}
	sig, err := base64.StdEncoding.DecodeString(m.Signature.SignatureValue)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadSignature, err)
	}
	if err := security.Verify(pub, signed, sig); err != nil {
		return nil, ErrBadSignature
	}
	return &m, nil
}
