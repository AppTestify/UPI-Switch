// Package domain holds the switch's core types and the transaction state machine.
package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// TxnStatus is the lifecycle state of a payment, refund or mandate debit.
// INITIATED → PENDING → SUCCESS | FAILED | DEEMED, and DEEMED → SUCCESS | FAILED via reconciliation.
type TxnStatus string

const (
	StatusInitiated TxnStatus = "INITIATED"
	StatusPending   TxnStatus = "PENDING"
	StatusSuccess   TxnStatus = "SUCCESS"
	StatusFailed    TxnStatus = "FAILED"
	StatusDeemed    TxnStatus = "DEEMED"
)

var transitions = map[TxnStatus][]TxnStatus{
	StatusInitiated: {StatusPending, StatusSuccess, StatusFailed, StatusDeemed},
	StatusPending:   {StatusSuccess, StatusFailed, StatusDeemed},
	StatusDeemed:    {StatusSuccess, StatusFailed},
}

// ErrInvalidTransition is returned when a status change is not allowed by the state machine.
var ErrInvalidTransition = errors.New("invalid status transition")

// CanTransition reports whether from → to is a legal move.
func CanTransition(from, to TxnStatus) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// IsFinal reports whether no further automatic transitions happen (DEEMED waits for recon).
func (s TxnStatus) IsFinal() bool { return s == StatusSuccess || s == StatusFailed }

// TxnType is the kind of UPI payment.
type TxnType string

const (
	TypeIntent       TxnType = "INTENT"
	TypeQR           TxnType = "QR"
	TypeCollect      TxnType = "COLLECT"
	TypeMandateDebit TxnType = "MANDATE_DEBIT"
)

// Txn is one payment in the ledger.
type Txn struct {
	ID             string     `json:"txn_id"`
	TenantID       string     `json:"tenant_id"`
	MerchantRef    string     `json:"merchant_ref"`
	Type           TxnType    `json:"type"`
	Status         TxnStatus  `json:"status"`
	AmountPaise    int64      `json:"-"`
	Currency       string     `json:"currency"`
	PayeeVPA       string     `json:"payee_vpa"`
	PayeeName      string     `json:"payee_name"`
	PayerVPAMasked string     `json:"payer_vpa,omitempty"`
	PayerVPAToken  string     `json:"-"`
	PayerVPAEnc    string     `json:"-"`
	MerchantID     string     `json:"merchant_id"`
	MCC            string     `json:"mcc"`
	Note           string     `json:"note,omitempty"`
	PSPID          string     `json:"psp"`
	RRN            string     `json:"rrn,omitempty"`
	RespCode       string     `json:"resp_code,omitempty"`
	RespReason     string     `json:"resp_reason,omitempty"`
	IntentURI      string     `json:"intent_uri,omitempty"`
	MandateID      string     `json:"mandate_id,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
	CheckAttempts  int        `json:"-"`
	NextCheckAt    *time.Time `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// Amount renders the amount as rupees with two decimals.
func (t *Txn) Amount() string { return FormatAmount(t.AmountPaise) }

// RefundStatus mirrors TxnStatus for refunds; OFFLINE_QUEUED waits for a bank refund file.
type RefundStatus = TxnStatus

const StatusOfflineQueued TxnStatus = "OFFLINE_QUEUED"

// RefundMode is how a refund is executed.
type RefundMode string

const (
	RefundOnline  RefundMode = "ONLINE"
	RefundBulk    RefundMode = "BULK"
	RefundOffline RefundMode = "OFFLINE"
)

// Refund is a full or partial refund against a successful payment.
type Refund struct {
	ID                string     `json:"refund_id"`
	TenantID          string     `json:"tenant_id"`
	TxnID             string     `json:"txn_id"`
	MerchantRefundRef string     `json:"merchant_refund_ref"`
	AmountPaise       int64      `json:"-"`
	Status            TxnStatus  `json:"status"`
	Mode              RefundMode `json:"mode"`
	PSPID             string     `json:"psp"`
	RRN               string     `json:"rrn,omitempty"`
	RespCode          string     `json:"resp_code,omitempty"`
	RespReason        string     `json:"resp_reason,omitempty"`
	CheckAttempts     int        `json:"-"`
	NextCheckAt       *time.Time `json:"-"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// MandateStatus is the lifecycle of a UPI AutoPay mandate.
type MandateStatus string

const (
	MandatePending MandateStatus = "PENDING"
	MandateActive  MandateStatus = "ACTIVE"
	MandateRevoked MandateStatus = "REVOKED"
	MandateFailed  MandateStatus = "FAILED"
)

// Mandate is a recurring-debit authorisation (UPI AutoPay).
type Mandate struct {
	ID             string        `json:"mandate_id"`
	TenantID       string        `json:"tenant_id"`
	MerchantRef    string        `json:"merchant_ref"`
	Status         MandateStatus `json:"status"`
	PayerVPAMasked string        `json:"payer_vpa"`
	PayerVPAEnc    string        `json:"-"`
	PayerVPAToken  string        `json:"-"`
	PayeeVPA       string        `json:"payee_vpa"`
	MerchantID     string        `json:"merchant_id"`
	MCC            string        `json:"mcc"`
	MaxAmountPaise int64         `json:"-"`
	Frequency      string        `json:"frequency"`
	StartDate      time.Time     `json:"start_date"`
	EndDate        time.Time     `json:"end_date"`
	UMN            string        `json:"umn,omitempty"`
	PSPID          string        `json:"psp"`
	RespCode       string        `json:"resp_code,omitempty"`
	CreatedAt      time.Time     `json:"created_at"`
	UpdatedAt      time.Time     `json:"updated_at"`
}

// DisputeType follows the UDIR categories.
type DisputeType string

const (
	DisputeTCC        DisputeType = "TCC"        // transaction credit confirmation (customer debited, merchant not credited)
	DisputeRET        DisputeType = "RET"        // return / refund request
	DisputeChargeback DisputeType = "CHARGEBACK" // customer chargeback raised via remitter bank
	DisputeComplaint  DisputeType = "COMPLAINT"  // generic UDIR complaint
)

// DisputeStatus is the dispute lifecycle.
type DisputeStatus string

const (
	DisputeRaised   DisputeStatus = "RAISED"
	DisputeReview   DisputeStatus = "UNDER_REVIEW"
	DisputeAccepted DisputeStatus = "RESOLVED_ACCEPTED"
	DisputeRejected DisputeStatus = "RESOLVED_REJECTED"
)

// Dispute is a UDIR complaint against a transaction.
type Dispute struct {
	ID          string        `json:"dispute_id"`
	TenantID    string        `json:"tenant_id"`
	TxnID       string        `json:"txn_id"`
	Type        DisputeType   `json:"type"`
	Status      DisputeStatus `json:"status"`
	ReasonCode  string        `json:"reason_code"`
	Remarks     string        `json:"remarks,omitempty"`
	AmountPaise int64         `json:"-"`
	Resolution  string        `json:"resolution,omitempty"`
	Source      string        `json:"source"`
	CreatedAt   time.Time     `json:"created_at"`
	UpdatedAt   time.Time     `json:"updated_at"`
}

// Limits are per-tenant risk limits.
type Limits struct {
	MinTxnPaise        int64    `json:"min_txn_paise"`
	MaxTxnPaise        int64    `json:"max_txn_paise"`
	DailyPaise         int64    `json:"daily_paise"`
	VelocityPerVPA     int      `json:"velocity_per_vpa"` // max payments per payer VPA in VelocityWindowSecs
	VelocityWindowSecs int      `json:"velocity_window_secs"`
	BlockedMCC         []string `json:"blocked_mcc"`
	RateLimitPerSec    int      `json:"rate_limit_per_sec"`
}

// Routing are per-tenant sponsor-bank routing rules.
type Routing struct {
	AllowedPSPs   []string `json:"allowed_psps"`   // empty = all enabled PSPs
	PreferredPSPs []string `json:"preferred_psps"` // tie-break order
}

// Tenant is one payment gateway / aggregator plugged into the switch.
type Tenant struct {
	ID            string    `json:"tenant_id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	APIKeyID      string    `json:"api_key_id"`
	HMACSecret    string    `json:"-"`
	WebhookURL    string    `json:"webhook_url"`
	WebhookSecret string    `json:"-"`
	VPAPrefix     string    `json:"vpa_prefix"`
	IPAllowlist   []string  `json:"ip_allowlist"`
	CertCN        string    `json:"cert_cn,omitempty"`
	FeeBps        int       `json:"fee_bps"`
	Limits        Limits    `json:"limits"`
	Routing       Routing   `json:"routing"`
	CreatedAt     time.Time `json:"created_at"`
}

// PSPBank is a sponsor PSP bank the switch connects to.
type PSPBank struct {
	ID       string `json:"psp_id"`
	Name     string `json:"name"`
	Endpoint string `json:"endpoint"`
	Handle   string `json:"handle"`
	CostBps  int    `json:"cost_bps"`
	Priority int    `json:"priority"`
	Enabled  bool   `json:"enabled"`
}

// ParseAmount turns "149.50" into 14950 paise. It rejects negatives and more than 2 decimals.
func ParseAmount(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("amount is required")
	}
	whole, frac, hasFrac := strings.Cut(s, ".")
	if hasFrac && (len(frac) == 0 || len(frac) > 2) {
		return 0, fmt.Errorf("amount %q must have at most 2 decimals", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 || strings.HasPrefix(whole, "+") || strings.HasPrefix(whole, "-") {
		return 0, fmt.Errorf("amount %q is not a valid positive number", s)
	}
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil || f < 0 || strings.ContainsAny(frac, "+-") {
		return 0, fmt.Errorf("amount %q is not a valid positive number", s)
	}
	if w > 1_000_000_000 {
		return 0, fmt.Errorf("amount %q is too large", s)
	}
	return w*100 + f, nil
}

// FormatAmount renders paise as "149.50".
func FormatAmount(paise int64) string {
	return fmt.Sprintf("%d.%02d", paise/100, paise%100)
}

// MarshalJSON adds the rupee amount to the JSON view.
func (t Txn) MarshalJSON() ([]byte, error) {
	type alias Txn
	return json.Marshal(struct {
		alias
		Amount string `json:"amount"`
	}{alias(t), FormatAmount(t.AmountPaise)})
}

// MarshalJSON adds the rupee amount to the JSON view.
func (r Refund) MarshalJSON() ([]byte, error) {
	type alias Refund
	return json.Marshal(struct {
		alias
		Amount string `json:"amount"`
	}{alias(r), FormatAmount(r.AmountPaise)})
}

// MarshalJSON adds the rupee amount to the JSON view.
func (m Mandate) MarshalJSON() ([]byte, error) {
	type alias Mandate
	return json.Marshal(struct {
		alias
		MaxAmount string `json:"max_amount"`
		StartDate string `json:"start_date"`
		EndDate   string `json:"end_date"`
	}{alias(m), FormatAmount(m.MaxAmountPaise), m.StartDate.Format("2006-01-02"), m.EndDate.Format("2006-01-02")})
}

// MarshalJSON adds the rupee amount to the JSON view.
func (d Dispute) MarshalJSON() ([]byte, error) {
	type alias Dispute
	return json.Marshal(struct {
		alias
		Amount string `json:"amount"`
	}{alias(d), FormatAmount(d.AmountPaise)})
}
