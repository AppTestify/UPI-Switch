// Package security holds request signing, the HSM signer abstraction, VPA tokenisation and masking.
package security

import (
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SignRequest computes the tenant API signature:
// hex(HMAC-SHA256(secret, timestamp "\n" METHOD "\n" path "\n" hex(sha256(body)))).
func SignRequest(secret, timestamp, method, path string, body []byte) string {
	bh := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "\n" + strings.ToUpper(method) + "\n" + path + "\n" + hex.EncodeToString(bh[:])))
	return hex.EncodeToString(mac.Sum(nil))
}

// SignWebhook computes the outbound webhook signature: hex(HMAC-SHA256(secret, timestamp "." body)).
func SignWebhook(secret, timestamp string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// EqualHex compares two hex signatures in constant time.
func EqualHex(a, b string) bool {
	return hmac.Equal([]byte(strings.ToLower(a)), []byte(strings.ToLower(b)))
}

// RandomToken returns n random bytes hex encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Signer is the HSM abstraction used for NPCI message signing and signed intents.
// The software implementation keeps the key in process memory; a PKCS#11 HSM implements the same interface.
type Signer interface {
	Sign(data []byte) ([]byte, error)
	KeyID() string
	Public() *rsa.PublicKey
}

// SoftHSM is an RSA-SHA256 signer backed by an in-memory key.
type SoftHSM struct {
	key   *rsa.PrivateKey
	keyID string
}

// NewSoftHSM wraps an RSA private key.
func NewSoftHSM(key *rsa.PrivateKey, keyID string) *SoftHSM { return &SoftHSM{key: key, keyID: keyID} }

// GenerateSoftHSM creates a fresh 2048-bit key; for tests and local runs.
func GenerateSoftHSM(keyID string) *SoftHSM {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return NewSoftHSM(k, keyID)
}

func (s *SoftHSM) Sign(data []byte) ([]byte, error) {
	h := sha256.Sum256(data)
	return rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, h[:])
}
func (s *SoftHSM) KeyID() string          { return s.keyID }
func (s *SoftHSM) Public() *rsa.PublicKey { return &s.key.PublicKey }

// Verify checks an RSA-SHA256 PKCS#1 v1.5 signature.
func Verify(pub *rsa.PublicKey, data, sig []byte) error {
	h := sha256.Sum256(data)
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig)
}

// LoadPrivateKey reads a PEM RSA private key (PKCS#1 or PKCS#8).
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, fmt.Errorf("%s: no PEM block", path)
	}
	if k, err := x509.ParsePKCS1PrivateKey(blk.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

// LoadPublicKey reads a PEM RSA public key (PKIX).
func LoadPublicKey(path string) (*rsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePublicKeyPEM(b)
}

// ParsePublicKeyPEM parses a PEM PKIX RSA public key.
func ParsePublicKeyPEM(b []byte) (*rsa.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("no PEM block")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	rk, ok := k.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("not an RSA key")
	}
	return rk, nil
}

// EncodePrivateKeyPEM encodes a private key as PKCS#1 PEM.
func EncodePrivateKeyPEM(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// EncodePublicKeyPEM encodes a public key as PKIX PEM.
func EncodePublicKeyPEM(k *rsa.PublicKey) []byte {
	b, _ := x509.MarshalPKIXPublicKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b})
}

// Vault tokenises and encrypts customer identifiers (VPA, mobile) so they are never stored in clear.
type Vault struct {
	tokenKey []byte
	aead     cipher.AEAD
}

// NewVault builds a vault from a 32-byte data key (hex or raw string, hashed to 32 bytes).
func NewVault(dataKey string) *Vault {
	k := sha256.Sum256([]byte("enc:" + dataKey))
	t := sha256.Sum256([]byte("tok:" + dataKey))
	block, err := aes.NewCipher(k[:])
	if err != nil {
		panic(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		panic(err)
	}
	return &Vault{tokenKey: t[:], aead: aead}
}

// Token returns a stable keyed hash of the value, used for lookups and velocity checks.
func (v *Vault) Token(value string) string {
	if value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, v.tokenKey)
	mac.Write([]byte(strings.ToLower(strings.TrimSpace(value))))
	return hex.EncodeToString(mac.Sum(nil))
}

// Encrypt seals the value with AES-GCM.
func (v *Vault) Encrypt(value string) string {
	if value == "" {
		return ""
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		panic(err)
	}
	return base64.StdEncoding.EncodeToString(v.aead.Seal(nonce, nonce, []byte(value), nil))
}

// Decrypt opens a value sealed by Encrypt.
func (v *Vault) Decrypt(enc string) (string, error) {
	if enc == "" {
		return "", nil
	}
	b, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	ns := v.aead.NonceSize()
	if len(b) < ns {
		return "", errors.New("ciphertext too short")
	}
	out, err := v.aead.Open(nil, b[:ns], b[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// MaskVPA hides most of the user part: "rahul.kumar@okbank" → "ra*******r@okbank".
func MaskVPA(vpa string) string {
	user, handle, ok := strings.Cut(vpa, "@")
	if !ok {
		return MaskMobile(vpa)
	}
	if len(user) <= 3 {
		return strings.Repeat("*", len(user)) + "@" + handle
	}
	return user[:2] + strings.Repeat("*", len(user)-3) + user[len(user)-1:] + "@" + handle
}

// MaskMobile keeps the last four digits.
func MaskMobile(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("*", len(s))
	}
	return strings.Repeat("*", len(s)-4) + s[len(s)-4:]
}
