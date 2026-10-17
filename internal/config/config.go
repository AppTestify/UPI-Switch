// Package config reads switch configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Switch is the switch process configuration.
type Switch struct {
	ListenAddr       string
	DatabaseURL      string
	RedisURL         string
	OrgID            string
	SigningKeyPath   string // RSA private key used by the software HSM
	SigningKeyID     string
	DataKey          string // vault key for tokenisation/encryption of VPAs and tenant secrets
	AdminTokens      string // "name:role:token,name:role:token"
	TLSCert, TLSKey  string // serve HTTPS when set
	TLSClientCA      string // require/verify client certs (mTLS) when set
	BankTLSCert      string // client cert presented to PSP banks
	BankTLSKey       string
	BankTLSCA        string
	BankIPAllowlist  []string
	TrustProxy       bool
	ReconTZ          string
	PollEvery        time.Duration
	WebhookEvery     time.Duration
	HeartbeatEvery   time.Duration
	FirstCheckAfter  time.Duration
	IntentExpiry     time.Duration
	MaxCheckAttempts int
	BlockedMCC       []string
	PSPSeed          string // optional JSON file of PSP banks to upsert at startup
	LogLevel         string
}

// Get returns an env var or a default.
func Get(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func dur(key string, def time.Duration) time.Duration {
	v := Get(key, "")
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		panic(fmt.Sprintf("%s: %v", key, err))
	}
	return d
}

func list(key string) []string {
	v := Get(key, "")
	if v == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// LoadSwitch reads the switch configuration.
func LoadSwitch() Switch {
	attempts, _ := strconv.Atoi(Get("MAX_CHECK_ATTEMPTS", "6"))
	return Switch{
		ListenAddr:       Get("LISTEN_ADDR", ":8080"),
		DatabaseURL:      Get("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/upiswitch?sslmode=disable"),
		RedisURL:         Get("REDIS_URL", ""),
		OrgID:            Get("ORG_ID", "ATF"),
		SigningKeyPath:   Get("SIGNING_KEY", "keys/switch.key"),
		SigningKeyID:     Get("SIGNING_KEY_ID", "atf-switch-1"),
		DataKey:          Get("DATA_KEY", ""),
		AdminTokens:      Get("ADMIN_TOKENS", ""),
		TLSCert:          Get("TLS_CERT", ""),
		TLSKey:           Get("TLS_KEY", ""),
		TLSClientCA:      Get("TLS_CLIENT_CA", ""),
		BankTLSCert:      Get("BANK_TLS_CERT", ""),
		BankTLSKey:       Get("BANK_TLS_KEY", ""),
		BankTLSCA:        Get("BANK_TLS_CA", ""),
		BankIPAllowlist:  list("BANK_IP_ALLOWLIST"),
		TrustProxy:       Get("TRUST_PROXY", "false") == "true",
		ReconTZ:          Get("RECON_TZ", "Asia/Kolkata"),
		PollEvery:        dur("POLL_EVERY", 5*time.Second),
		WebhookEvery:     dur("WEBHOOK_EVERY", 2*time.Second),
		HeartbeatEvery:   dur("HEARTBEAT_EVERY", 15*time.Second),
		FirstCheckAfter:  dur("FIRST_CHECK_AFTER", 60*time.Second),
		IntentExpiry:     dur("INTENT_EXPIRY", 15*time.Minute),
		MaxCheckAttempts: attempts,
		BlockedMCC:       list("BLOCKED_MCC"),
		PSPSeed:          Get("PSP_SEED", ""),
		LogLevel:         Get("LOG_LEVEL", "info"),
	}
}
