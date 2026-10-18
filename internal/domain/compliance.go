package domain

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// KYC states of a sub-merchant.
const (
	KYCPending  = "PENDING"
	KYCVerified = "VERIFIED"
	KYCRejected = "REJECTED"
)

// Merchant statuses.
const (
	MerchantActive    = "ACTIVE"
	MerchantSuspended = "SUSPENDED"
)

// KYCMethods are the due-diligence routes in the RBI PA Directions (RBI/DPSS/2025-26/141 para 13):
// a CKYCR record, or for small merchants (turnover up to ₹40 lakh) PAN or Form 60 with contact-point
// verification, or an officially valid document (OVD).
var KYCMethods = []string{"CKYC", "PAN", "FORM60", "OVD"}

// Merchant is a payment gateway's sub-merchant as registered with the switch.
type Merchant struct {
	TenantID      string     `json:"tenant_id"`
	ID            string     `json:"merchant_id"`
	MID           string     `json:"mid"`
	LegalName     string     `json:"legal_name"`
	DisplayName   string     `json:"display_name"`
	MCC           string     `json:"mcc"`
	BusinessType  string     `json:"business_type"` // SMALL (turnover ≤ ₹40 lakh) | REGULAR
	KYCStatus     string     `json:"kyc_status"`
	KYCMethod     string     `json:"kyc_method,omitempty"`
	KYCRef        string     `json:"kyc_ref,omitempty"`
	PANMasked     string     `json:"pan,omitempty"`
	PANEnc        string     `json:"-"`
	GSTIN         string     `json:"gstin,omitempty"`
	Status        string     `json:"status"`
	DeclaredPaise int64      `json:"-"`
	Source        string     `json:"source"`
	Remarks       string     `json:"remarks,omitempty"`
	VerifiedAt    *time.Time `json:"verified_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// Verified reports whether the merchant passed KYC, which unlocks NPCI's higher category limits.
func (m *Merchant) Verified() bool { return m.KYCStatus == KYCVerified }

// MarshalJSON adds the category and the declared monthly volume in rupees.
func (m Merchant) MarshalJSON() ([]byte, error) {
	type alias Merchant
	c := CategoryFor(m.MCC)
	return json.Marshal(struct {
		alias
		Declared string `json:"declared_monthly_volume"`
		Category string `json:"category"`
	}{alias(m), FormatAmount(m.DeclaredPaise), c.Name})
}

// MandateNotification is an AutoPay pre-debit notification sent to the payer before a debit.
type MandateNotification struct {
	ID          string    `json:"notification_id"`
	MandateID   string    `json:"mandate_id"`
	TenantID    string    `json:"tenant_id"`
	Seq         int       `json:"seq"`
	AmountPaise int64     `json:"-"`
	DebitDate   time.Time `json:"-"`
	Status      string    `json:"status"` // NOTIFIED | EXECUTED | FAILED
	Attempts    int       `json:"attempts"`
	TxnID       string    `json:"txn_id,omitempty"`
	NotifiedAt  time.Time `json:"notified_at"`
}

// MarshalJSON renders the amount in rupees and the debit date as YYYY-MM-DD.
func (n MandateNotification) MarshalJSON() ([]byte, error) {
	type alias MandateNotification
	return json.Marshal(struct {
		alias
		Amount    string `json:"amount"`
		DebitDate string `json:"debit_date"`
	}{alias(n), FormatAmount(n.AmountPaise), n.DebitDate.Format("2006-01-02")})
}

// Incident is a sponsor-bank downtime episode.
type Incident struct {
	ID         string     `json:"incident_id"`
	PSPID      string     `json:"psp_id"`
	Severity   string     `json:"severity"` // HIGH | MEDIUM
	Status     string     `json:"status"`   // ACTIVE | RESOLVED
	Reason     string     `json:"reason"`
	TDRate     float64    `json:"td_rate"`
	PeakTDRate float64    `json:"peak_td_rate"`
	Affected   int64      `json:"affected"`
	StartedAt  time.Time  `json:"started_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// TechnicalDeclineCodes are the NPCI response codes counted as technical declines (TD): failures at
// a bank or in the network rather than the customer's choice. NPCI targets TD below 1%.
var TechnicalDeclineCodes = []string{"U09", "U90", "XY", "U91", "96", "U30", "U28", "BT"}

// IsTechnicalDecline reports whether a response code is a technical decline.
func IsTechnicalDecline(code string) bool { return slices.Contains(TechnicalDeclineCodes, code) }

// Category is an NPCI merchant category with its own per-transaction and per-day limits.
type Category struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	MCCs       []string `json:"mccs"`
	TxnPaise   int64    `json:"-"`
	DailyPaise int64    `json:"-"`
	Source     string   `json:"source"`
}

// MarshalJSON renders the limits in rupees.
func (c Category) MarshalJSON() ([]byte, error) {
	type alias Category
	return json.Marshal(struct {
		alias
		TxnLimit   string `json:"txn_limit"`
		DailyLimit string `json:"daily_limit"`
	}{alias(c), FormatAmount(c.TxnPaise), FormatAmount(c.DailyPaise)})
}

const lakh = 100_000 * 100 // ₹1 lakh in paise

// GeneralCategory is UPI's standard P2M limit, and the limit for any merchant that has not passed KYC.
var GeneralCategory = Category{Key: "general", Name: "General", TxnPaise: lakh, DailyPaise: lakh, Source: "NPCI UPI standard limit"}

// Categories are NPCI's higher-limit merchant categories for verified merchants (NPCI circulars of
// 8 Dec 2023, 24 Aug 2024 and 28 Aug 2025). The MCC lists are the switch's mapping; confirm them
// with the sponsor bank before go-live.
var Categories = []Category{
	{Key: "capital_markets", Name: "Capital markets", MCCs: []string{"6211"}, TxnPaise: 5 * lakh, DailyPaise: 10 * lakh, Source: "NPCI, 28 Aug 2025"},
	{Key: "insurance", Name: "Insurance", MCCs: []string{"6300", "6381", "6399"}, TxnPaise: 5 * lakh, DailyPaise: 10 * lakh, Source: "NPCI, 28 Aug 2025"},
	{Key: "travel", Name: "Travel", MCCs: []string{"3000-3350", "4112", "4411", "4511", "4722", "7011"}, TxnPaise: 5 * lakh, DailyPaise: 10 * lakh, Source: "NPCI, 28 Aug 2025"},
	{Key: "government", Name: "Government and tax (incl. GeM)", MCCs: []string{"9211", "9222", "9311", "9399"}, TxnPaise: 5 * lakh, DailyPaise: 10 * lakh, Source: "NPCI, 24 Aug 2024 and 28 Aug 2025"},
	{Key: "hospitals_education", Name: "Hospitals and education", MCCs: []string{"8011", "8062", "8071", "8099", "8211", "8220", "8241", "8244", "8249", "8299"}, TxnPaise: 5 * lakh, DailyPaise: 5 * lakh, Source: "NPCI, 8 Dec 2023"},
	{Key: "jewellery", Name: "Jewellery", MCCs: []string{"5944"}, TxnPaise: 2 * lakh, DailyPaise: 6 * lakh, Source: "NPCI, 28 Aug 2025"},
}

func mccIn(mcc string, list []string) bool {
	n, err := strconv.Atoi(mcc)
	for _, m := range list {
		if m == mcc {
			return true
		}
		if lo, hi, ok := cutRange(m); ok && err == nil && n >= lo && n <= hi {
			return true
		}
	}
	return false
}

func cutRange(s string) (int, int, bool) {
	for i := range s {
		if s[i] == '-' {
			lo, err1 := strconv.Atoi(s[:i])
			hi, err2 := strconv.Atoi(s[i+1:])
			return lo, hi, err1 == nil && err2 == nil
		}
	}
	return 0, 0, false
}

// MCCList expands the category's MCCs and ranges into individual codes (nil for the general category,
// whose daily limit counts every merchant payment).
func (c Category) MCCList() []string {
	if c.Key == GeneralCategory.Key {
		return nil
	}
	var out []string
	for _, m := range c.MCCs {
		if lo, hi, ok := cutRange(m); ok {
			for n := lo; n <= hi; n++ {
				out = append(out, fmt.Sprintf("%04d", n))
			}
			continue
		}
		out = append(out, m)
	}
	return out
}

// CategoryFor returns the NPCI category of an MCC (GeneralCategory when it has no higher limit).
func CategoryFor(mcc string) Category {
	for _, c := range Categories {
		if mccIn(mcc, c.MCCs) {
			return c
		}
	}
	return GeneralCategory
}

// LimitsFor returns the category limits that apply to a merchant: higher limits only after KYC.
func LimitsFor(mcc string, verified bool) Category {
	if !verified {
		return GeneralCategory
	}
	return CategoryFor(mcc)
}
