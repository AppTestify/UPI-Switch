package core

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

var (
	panRe   = regexp.MustCompile(`^[A-Z]{5}[0-9]{4}[A-Z]$`)
	gstinRe = regexp.MustCompile(`^[0-9]{2}[A-Z]{5}[0-9]{4}[A-Z][1-9A-Z]Z[0-9A-Z]$`)
	ckycRe  = regexp.MustCompile(`^[0-9]{14}$`)
)

// MerchantRequest is the body of POST /v1/upi/merchants: a payment gateway registers a sub-merchant
// with the due-diligence details the RBI PA Directions ask for.
type MerchantRequest struct {
	ID              string `json:"merchant_id"`
	LegalName       string `json:"legal_name"`
	DisplayName     string `json:"display_name"`
	MCC             string `json:"mcc"`
	BusinessType    string `json:"business_type"` // SMALL | REGULAR
	PAN             string `json:"pan"`
	GSTIN           string `json:"gstin"`
	CKYCNumber      string `json:"ckyc_number"`
	DeclaredMonthly string `json:"declared_monthly_volume"`
}

func maskPAN(p string) string {
	if len(p) != 10 {
		return ""
	}
	return p[:3] + "XXXX" + p[7:]
}

// maskRef keeps the last four characters of an identity reference.
func maskRef(r string) string {
	r = strings.TrimSpace(r)
	if len(r) <= 4 {
		return strings.Repeat("X", len(r))
	}
	return strings.Repeat("X", len(r)-4) + r[len(r)-4:]
}

func newMID() string { return "ATFM" + strings.ToUpper(security.RandomToken(5)) }

// RegisterMerchant onboards a sub-merchant in KYC PENDING; the switch's operations team verifies it.
func (s *Service) RegisterMerchant(ctx context.Context, t *domain.Tenant, req MerchantRequest, actor string) (*domain.Merchant, error) {
	if err := validateRef("merchant_id", req.ID); err != nil {
		return nil, err
	}
	req.PAN, req.GSTIN = strings.ToUpper(strings.TrimSpace(req.PAN)), strings.ToUpper(strings.TrimSpace(req.GSTIN))
	switch {
	case req.LegalName == "" || len(req.LegalName) > 99:
		return nil, errValidation("legal_name is required (max 99 chars): the name on the merchant's bank account, shown to payers")
	case !mccRe.MatchString(req.MCC):
		return nil, errValidation("mcc must be a 4-digit merchant category code")
	case req.PAN != "" && !panRe.MatchString(req.PAN):
		return nil, errValidation("pan is not a valid PAN")
	case req.GSTIN != "" && !gstinRe.MatchString(req.GSTIN):
		return nil, errValidation("gstin is not a valid GSTIN")
	case req.CKYCNumber != "" && !ckycRe.MatchString(req.CKYCNumber):
		return nil, errValidation("ckyc_number must be the 14-digit CKYC identifier")
	case req.PAN == "" && req.CKYCNumber == "":
		return nil, errValidation("pan or ckyc_number is required for merchant due diligence")
	}
	if req.BusinessType == "" {
		req.BusinessType = "SMALL"
	}
	if req.BusinessType != "SMALL" && req.BusinessType != "REGULAR" {
		return nil, errValidation("business_type must be SMALL (turnover up to ₹40 lakh) or REGULAR")
	}
	if req.DisplayName == "" {
		req.DisplayName = req.LegalName
	}
	var declared int64
	if req.DeclaredMonthly != "" {
		v, err := domain.ParseAmount(req.DeclaredMonthly)
		if err != nil {
			return nil, errValidation("declared_monthly_volume: %v", err)
		}
		declared = v
	}
	m := &domain.Merchant{
		TenantID: t.ID, ID: req.ID, MID: newMID(), LegalName: req.LegalName, DisplayName: req.DisplayName, MCC: req.MCC,
		BusinessType: req.BusinessType, KYCStatus: domain.KYCPending, PANMasked: maskPAN(req.PAN), GSTIN: req.GSTIN,
		Status: domain.MerchantActive, DeclaredPaise: declared, Source: "API",
	}
	if req.PAN != "" {
		m.PANEnc = s.vault.Encrypt(req.PAN)
	}
	if req.CKYCNumber != "" {
		m.KYCMethod, m.KYCRef = "CKYC", "XXXXXXXXXX"+req.CKYCNumber[10:]
	} else {
		m.KYCMethod = "PAN"
	}
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.InsertMerchant(ctx, m, false); err != nil {
			return err
		}
		return tx.Audit(ctx, actor, "merchant.register", "merchant", t.ID+"/"+m.ID, map[string]any{"mid": m.MID, "mcc": m.MCC, "kyc_method": m.KYCMethod})
	})
	if errors.Is(err, store.ErrConflict) {
		return nil, errUnprocessable("merchant_exists", "merchant %s is already registered", req.ID)
	}
	if err != nil {
		return nil, err
	}
	return s.store.GetMerchant(ctx, t.ID, m.ID)
}

// GetMerchant returns a tenant's merchant.
func (s *Service) GetMerchant(ctx context.Context, tenantID, id string) (*domain.Merchant, error) {
	m, err := s.store.GetMerchant(ctx, tenantID, id)
	return m, storeErr(err, "merchant")
}

// merchantFor looks up the payment's merchant, registering an unknown one in KYC PENDING so it shows up
// for review. Suspended merchants, rejected KYC and (when the gateway requires it) pending KYC are refused,
// and the MCC must be the one assigned at onboarding. Pull payments (collect, AutoPay) pass pull=true: they
// debit a customer on the merchant's word, so the merchant must already be registered.
func (s *Service) merchantFor(ctx context.Context, t *domain.Tenant, req Merchant, pull bool) (*domain.Merchant, error) {
	m, err := s.store.GetMerchant(ctx, t.ID, req.ID)
	if errors.Is(err, store.ErrNotFound) && pull {
		return nil, errUnprocessable("merchant_not_registered", "merchant %s must be registered (POST /v1/upi/merchants) before it can send collect or AutoPay requests", req.ID)
	}
	if errors.Is(err, store.ErrNotFound) {
		m = &domain.Merchant{TenantID: t.ID, ID: req.ID, MID: newMID(), LegalName: req.Name, DisplayName: req.Name, MCC: req.MCC,
			BusinessType: "SMALL", KYCStatus: domain.KYCPending, Status: domain.MerchantActive, Source: "FIRST_PAYMENT"}
		if err := s.store.InsertMerchant(ctx, m, true); err != nil {
			return nil, err
		}
		m, err = s.store.GetMerchant(ctx, t.ID, req.ID)
	}
	if err != nil {
		return nil, err
	}
	switch {
	case m.Status == domain.MerchantSuspended:
		return nil, errUnprocessable("merchant_suspended", "merchant %s is suspended", m.ID)
	case m.KYCStatus == domain.KYCRejected:
		return nil, errUnprocessable("merchant_kyc_rejected", "merchant %s failed KYC", m.ID)
	case m.KYCStatus == domain.KYCPending && t.Limits.RequireKYC:
		return nil, errUnprocessable("merchant_kyc_pending", "merchant %s has not completed KYC", m.ID)
	case m.MCC != req.MCC:
		return nil, errUnprocessable("mcc_mismatch", "merchant %s is registered with MCC %s", m.ID, m.MCC)
	}
	return m, nil
}

// payeeName is the name payers see: the registered name once KYC is done (NPCI UPI-OC-101-A FY25-26
// requires the ultimate beneficiary's name, not one set in the request).
func payeeName(m *domain.Merchant) string {
	if m.Verified() {
		return m.LegalName
	}
	return m.DisplayName
}

// MerchantDecision is an operations decision on a merchant.
type MerchantDecision struct {
	Action  string `json:"action"` // verify | reject | suspend | reactivate
	Method  string `json:"method"` // KYC route for verify: CKYC | PAN | FORM60 | OVD
	Ref     string `json:"ref"`    // verification reference (CKYC number, document id)
	Remarks string `json:"remarks"`
}

// DecideMerchant records a KYC or status decision, audits it and tells the gateway by webhook.
func (s *Service) DecideMerchant(ctx context.Context, tenantID, id string, d MerchantDecision, actor string) (*domain.Merchant, error) {
	var out *domain.Merchant
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		m, err := tx.LockMerchant(ctx, tenantID, id)
		if err != nil {
			return storeErr(err, "merchant")
		}
		evt := ""
		switch d.Action {
		case "verify":
			if m.KYCStatus == domain.KYCVerified {
				return errUnprocessable("already_verified", "merchant KYC is already verified")
			}
			if d.Method == "" {
				d.Method = m.KYCMethod
			}
			if !slices.Contains(domain.KYCMethods, d.Method) {
				return errValidation("method must be one of %v", domain.KYCMethods)
			}
			if d.Ref == "" && m.KYCRef == "" {
				return errValidation("ref is required: the CKYC number or the id of the document checked")
			}
			now := time.Now()
			m.KYCStatus, m.KYCMethod, m.VerifiedAt, evt = domain.KYCVerified, d.Method, &now, "merchant.kyc_verified"
			if d.Ref != "" {
				d.Ref = maskRef(d.Ref) // the full number stays with the gateway's KYC records, not in the switch or its audit log
				m.KYCRef = d.Ref
			}
		case "reject":
			if d.Remarks == "" {
				return errValidation("remarks are required to reject KYC")
			}
			m.KYCStatus, m.VerifiedAt, evt = domain.KYCRejected, nil, "merchant.kyc_rejected"
		case "suspend":
			if d.Remarks == "" {
				return errValidation("remarks are required to suspend a merchant")
			}
			m.Status, evt = domain.MerchantSuspended, "merchant.suspended"
		case "reactivate":
			m.Status, evt = domain.MerchantActive, "merchant.reactivated"
		default:
			return errValidation("action must be verify, reject, suspend or reactivate")
		}
		if d.Remarks != "" {
			m.Remarks = d.Remarks
		}
		if err := tx.UpdateMerchant(ctx, m); err != nil {
			return err
		}
		if err := tx.Audit(ctx, actor, "merchant."+d.Action, "merchant", tenantID+"/"+id,
			map[string]any{"method": d.Method, "ref": d.Ref, "remarks": d.Remarks}); err != nil {
			return err
		}
		out = m
		return s.enqueue(ctx, tx, tenantID, evt+":"+m.ID+":"+time.Now().UTC().Format("20060102150405.000"), evt, m)
	})
	return out, err
}
