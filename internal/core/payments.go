package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/intent"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/risk"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// IntentRequest is the body of POST /v1/upi/intent.
type IntentRequest struct {
	MerchantRef   string   `json:"merchant_ref"`
	Amount        string   `json:"amount"`
	Merchant      Merchant `json:"merchant"`
	Note          string   `json:"note"`
	ExpiryMinutes int      `json:"expiry_minutes"`
	QR            bool     `json:"qr"` // dynamic QR: also return a PNG
}

// IntentResult is returned for intent / QR payments.
type IntentResult struct {
	*domain.Txn
	QRPNGBase64 string `json:"qr_png_base64,omitempty"`
}

// MarshalJSON merges the txn view with the QR image (the embedded Txn's MarshalJSON would hide it).
func (r IntentResult) MarshalJSON() ([]byte, error) {
	b, err := json.Marshal(r.Txn)
	if err != nil || r.QRPNGBase64 == "" {
		return b, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	m["qr_png_base64"] = r.QRPNGBase64
	return json.Marshal(m)
}

func (s *Service) checkRisk(ctx context.Context, t *domain.Tenant, amount int64, mcc, payerToken string, verified bool) error {
	d, err := s.risk.Evaluate(ctx, risk.Check{Tenant: t, AmountPaise: amount, MCC: mcc, PayerToken: payerToken, Verified: verified})
	if err != nil {
		return err
	}
	if !d.Allow {
		s.m.Inc("upi_risk_declines_total", "tenant", t.ID, "rule", d.Rule)
		return errUnprocessable("risk_declined", "%s (%s)", d.Reason, d.Rule)
	}
	return nil
}

func (s *Service) duplicateRef(ctx context.Context, tenantID, ref string) error {
	existing, err := s.store.GetTxnByMerchantRef(ctx, tenantID, ref)
	if err != nil {
		return &Error{Status: http.StatusConflict, Code: "duplicate_merchant_ref", Message: "merchant_ref already used"}
	}
	return &Error{Status: http.StatusConflict, Code: "duplicate_merchant_ref", Message: "merchant_ref already used", TxnID: existing.ID}
}

// CreateIntent runs steps 3–5 of the intent / dynamic QR flow: risk check, route PSP,
// persist INITIATED and return a signed upi://pay URI (and QR).
func (s *Service) CreateIntent(ctx context.Context, t *domain.Tenant, req IntentRequest) (*IntentResult, error) {
	if err := validateRef("merchant_ref", req.MerchantRef); err != nil {
		return nil, err
	}
	if err := req.Merchant.validate(); err != nil {
		return nil, err
	}
	amt, err := domain.ParseAmount(req.Amount)
	if err != nil {
		return nil, errValidation("%v", err)
	}
	if len(req.Note) > 50 {
		return nil, errValidation("note must be at most 50 characters")
	}
	mer, err := s.merchantFor(ctx, t, req.Merchant)
	if err != nil {
		return nil, err
	}
	if err := s.checkRisk(ctx, t, amt, req.Merchant.MCC, "", mer.Verified()); err != nil {
		return nil, err
	}
	psps, err := s.router.Rank(t.Routing)
	if err != nil {
		return nil, errUpstream("%v", err)
	}
	psp := psps[0]
	expiry := s.cfg.IntentExpiry
	if req.ExpiryMinutes > 0 {
		expiry = time.Duration(min(req.ExpiryMinutes, 24*60)) * time.Minute
	}
	now := time.Now()
	typ, mode := domain.TypeIntent, "04"
	if req.QR {
		typ, mode = domain.TypeQR, "16"
	}
	txn := &domain.Txn{
		ID: npci.NewTxnID(s.cfg.OrgID), TenantID: t.ID, MerchantRef: req.MerchantRef, Type: typ, Status: domain.StatusInitiated,
		AmountPaise: amt, Currency: "INR", PayeeVPA: payeeVPA(t, req.Merchant.ID, psp.Handle), PayeeName: payeeName(mer),
		MerchantID: req.Merchant.ID, MCC: req.Merchant.MCC, Note: req.Note, PSPID: psp.ID,
		ExpiresAt: now.Add(expiry), NextCheckAt: ptr(now.Add(expiry + s.cfg.FirstCheckAfter)), // see checkTxn
	}
	note := req.Note
	if note == "" {
		note = "Payment to " + txn.PayeeName
	}
	txn.IntentURI, err = intent.Build(intent.Params{
		PayeeVPA: txn.PayeeVPA, PayeeName: txn.PayeeName, MCC: txn.MCC, TxnID: txn.ID, TxnRef: txn.ID, Note: note,
		Amount: txn.Amount(), Currency: "INR", Mode: mode, OrgID: s.cfg.OrgID,
	}, s.signer)
	if err != nil {
		return nil, err
	}
	if err := s.store.InsertTxn(ctx, txn); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, s.duplicateRef(ctx, t.ID, req.MerchantRef)
		}
		return nil, err
	}
	s.m.Transition("payment", string(txn.Status))
	res := &IntentResult{Txn: txn}
	if req.QR {
		png, err := intent.QRPNG(txn.IntentURI, 512)
		if err != nil {
			return nil, err
		}
		res.QRPNGBase64 = base64.StdEncoding.EncodeToString(png)
	}
	return res, nil
}

// CollectRequest is the body of POST /v1/upi/collect.
type CollectRequest struct {
	MerchantRef   string   `json:"merchant_ref"`
	Amount        string   `json:"amount"`
	PayerVPA      string   `json:"payer_vpa"`
	Merchant      Merchant `json:"merchant"`
	Note          string   `json:"note"`
	ExpiryMinutes int      `json:"expiry_minutes"`
}

// CreateCollect validates the payer VPA (ReqValAdd), persists the txn and sends a collect ReqPay,
// failing over to the next PSP on technical declines.
func (s *Service) CreateCollect(ctx context.Context, t *domain.Tenant, req CollectRequest) (*domain.Txn, error) {
	if err := validateRef("merchant_ref", req.MerchantRef); err != nil {
		return nil, err
	}
	if err := req.Merchant.validate(); err != nil {
		return nil, err
	}
	if req.Merchant.MCC == "0000" {
		// NPCI withdrew person-to-person collect requests from 1 Oct 2025; only merchants may collect.
		return nil, errUnprocessable("p2p_collect_not_allowed", "collect requests are only allowed for merchants (MCC 0000 is person-to-person)")
	}
	if !ValidVPA(req.PayerVPA) {
		return nil, errValidation("payer_vpa is not a valid VPA")
	}
	amt, err := domain.ParseAmount(req.Amount)
	if err != nil {
		return nil, errValidation("%v", err)
	}
	mer, err := s.merchantFor(ctx, t, req.Merchant)
	if err != nil {
		return nil, err
	}
	token := s.vault.Token(req.PayerVPA)
	if err := s.checkRisk(ctx, t, amt, req.Merchant.MCC, token, mer.Verified()); err != nil {
		return nil, err
	}
	va, err := s.ValidateVPA(ctx, t, req.PayerVPA)
	if err != nil {
		return nil, err
	}
	if !va.Valid {
		return nil, errUnprocessable("invalid_vpa", "payer VPA could not be verified (%s)", va.RespCode)
	}
	psps, err := s.router.Rank(t.Routing)
	if err != nil {
		return nil, errUpstream("%v", err)
	}
	expiry := s.cfg.CollectExpiry
	if req.ExpiryMinutes > 0 {
		expiry = time.Duration(min(req.ExpiryMinutes, 7*24*60)) * time.Minute
	}
	now := time.Now()
	txn := &domain.Txn{
		ID: npci.NewTxnID(s.cfg.OrgID), TenantID: t.ID, MerchantRef: req.MerchantRef, Type: domain.TypeCollect,
		Status: domain.StatusInitiated, AmountPaise: amt, Currency: "INR", PayeeVPA: payeeVPA(t, req.Merchant.ID, psps[0].Handle),
		PayeeName: payeeName(mer), PayerVPAMasked: security.MaskVPA(req.PayerVPA), PayerVPAToken: token,
		PayerVPAEnc: s.vault.Encrypt(req.PayerVPA), MerchantID: req.Merchant.ID, MCC: req.Merchant.MCC, Note: req.Note,
		PSPID: psps[0].ID, ExpiresAt: now.Add(expiry),
	}
	if err := s.store.InsertTxn(ctx, txn); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, s.duplicateRef(ctx, t.ID, req.MerchantRef)
		}
		return nil, err
	}
	s.m.Transition("payment", string(txn.Status))

	var lastErr error
	for _, p := range psps {
		payee := payeeVPA(t, req.Merchant.ID, p.Handle)
		msg := s.reqPay(txn, npci.TxnCollect, req.PayerVPA, payee, int(expiry.Minutes()))
		_, err := s.exchange(ctx, p.ID, msg)
		if err == nil {
			return txn, s.finishSend(ctx, txn.ID, p.ID, payee, "")
		}
		lastErr = err
		if !npci.IsTechnical(err) {
			return txn, s.finishSend(ctx, txn.ID, p.ID, payee, err.Error())
		}
		s.log.Warn("collect failover", "txn", txn.ID, "psp", p.ID, "err", err)
	}
	return txn, s.failSend(ctx, txn.ID, lastErr)
}

func (s *Service) reqPay(txn *domain.Txn, typ, payerVPA, payee string, expireMins int) *npci.Message {
	m := npci.New(npci.APIReqPay, s.cfg.OrgID)
	m.Txn = &npci.Txn{ID: txn.ID, Note: txn.Note, RefID: txn.MerchantRef, Ts: npci.Timestamp(time.Now()), Type: typ, ExpireAfter: expireMins}
	amt := &npci.Amount{Value: txn.Amount(), Curr: "INR"}
	m.Payer = &npci.Party{Addr: payerVPA, SeqNum: "1", Type: "PERSON", Amount: amt}
	m.Payees = &npci.Payees{Payee: []npci.Party{{Addr: payee, Name: txn.PayeeName, SeqNum: "1", Type: "ENTITY", Code: txn.MCC, Amount: amt}}}
	return m
}

// finishSend records the outcome of handing a ReqPay to a PSP: an ack moves the txn to PENDING,
// a business rejection moves it to FAILED.
func (s *Service) finishSend(ctx context.Context, txnID, pspID, payee, rejectErr string) error {
	return s.store.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockTxn(ctx, txnID)
		if err != nil {
			return err
		}
		if t.Status != domain.StatusInitiated {
			return nil // a callback beat the ack
		}
		if payee != "" && payee != t.PayeeVPA {
			if err := tx.SetPayeeVPA(ctx, t.ID, payee); err != nil {
				return err
			}
			t.PayeeVPA = payee
		}
		if rejectErr != "" {
			return s.applyPayment(ctx, tx, t, npci.ResultFailure, npci.CodeTechnical, rejectErr, "", nil, "ack")
		}
		next := time.Now().Add(s.cfg.FirstCheckAfter)
		if err := tx.UpdateTxn(ctx, t, store.TxnUpdate{Status: domain.StatusPending, PSPID: pspID, NextCheckAt: &next}, "ack", ""); err != nil {
			return err
		}
		s.m.Transition("payment", string(t.Status))
		return nil
	})
}

func (s *Service) failSend(ctx context.Context, txnID string, cause error) error {
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockTxn(ctx, txnID)
		if err != nil {
			return err
		}
		if t.Status != domain.StatusInitiated {
			return nil
		}
		return s.applyPayment(ctx, tx, t, npci.ResultFailure, npci.CodeBankDown, fmt.Sprintf("all PSP banks unavailable: %v", cause), "", nil, "router")
	})
	if err != nil {
		return err
	}
	return nil
}

// VPAResult is the outcome of ReqValAdd.
type VPAResult struct {
	VPA      string `json:"vpa"`
	Valid    bool   `json:"valid"`
	Name     string `json:"name,omitempty"`
	RespCode string `json:"resp_code,omitempty"`
	PSP      string `json:"psp"`
}

// ValidateVPA sends ReqValAdd through the best PSP (failing over on technical errors).
func (s *Service) ValidateVPA(ctx context.Context, t *domain.Tenant, vpa string) (*VPAResult, error) {
	if !ValidVPA(vpa) {
		return &VPAResult{VPA: vpa, Valid: false, RespCode: npci.CodeInvalidVPA}, nil
	}
	psps, err := s.router.Rank(t.Routing)
	if err != nil {
		return nil, errUpstream("%v", err)
	}
	var lastErr error
	for _, p := range psps {
		m := npci.New(npci.APIReqValAdd, s.cfg.OrgID)
		m.Txn = &npci.Txn{ID: npci.NewTxnID(s.cfg.OrgID), Ts: npci.Timestamp(time.Now()), Type: npci.TxnValAdd}
		m.Payees = &npci.Payees{Payee: []npci.Party{{Addr: vpa, SeqNum: "1"}}}
		resp, err := s.exchange(ctx, p.ID, m)
		if err != nil {
			lastErr = err
			if npci.IsTechnical(err) {
				continue
			}
			return nil, errUpstream("%v", err)
		}
		if resp.Resp == nil {
			lastErr = errors.New("empty RespValAdd")
			continue
		}
		return &VPAResult{VPA: security.MaskVPA(vpa), Valid: resp.Resp.Result == npci.ResultSuccess, Name: resp.Resp.MaskName,
			RespCode: resp.Resp.ErrCode, PSP: p.ID}, nil
	}
	return nil, errUpstream("VPA validation failed on all PSP banks: %v", lastErr)
}

// GetTxn returns a tenant's txn by switch id or merchant ref.
func (s *Service) GetTxn(ctx context.Context, tenantID, id, merchantRef string) (*domain.Txn, error) {
	var t *domain.Txn
	var err error
	if id != "" {
		t, err = s.store.GetTxn(ctx, tenantID, id)
	} else {
		t, err = s.store.GetTxnByMerchantRef(ctx, tenantID, merchantRef)
	}
	return t, storeErr(err, "transaction")
}

// applyPayment moves a locked payment txn to the outcome reported by the bank and queues the webhook.
func (s *Service) applyPayment(ctx context.Context, tx *store.Store, t *domain.Txn, result, code, reason, rrn string, payer *npci.Party, source string) error {
	var to domain.TxnStatus
	switch result {
	case npci.ResultSuccess:
		to = domain.StatusSuccess
		code = npci.CodeApproved
	case npci.ResultFailure:
		to = domain.StatusFailed
	case npci.ResultDeemed:
		to = domain.StatusDeemed
		if code == "" {
			code = npci.CodeDeemed
		}
	default:
		return fmt.Errorf("unexpected result %q", result)
	}
	if t.Status == to {
		return nil
	}
	if !domain.CanTransition(t.Status, to) {
		s.log.Warn("ignoring conflicting bank outcome", "txn", t.ID, "status", t.Status, "reported", to, "source", source)
		return nil
	}
	u := store.TxnUpdate{Status: to, RRN: rrn, RespCode: code, RespReason: reason}
	if payer != nil && payer.Addr != "" && t.PayerVPAToken == "" {
		u.PayerVPAMasked, u.PayerVPAToken, u.PayerVPAEnc = security.MaskVPA(payer.Addr), s.vault.Token(payer.Addr), s.vault.Encrypt(payer.Addr)
	}
	if to == domain.StatusDeemed {
		u.ClearCheck = true // T+1 reconciliation closes deemed txns
	}
	if err := tx.UpdateTxn(ctx, t, u, source, reason); err != nil {
		return err
	}
	s.m.Transition("payment", string(to))
	if t.MandateID != "" && to.IsFinal() {
		if err := tx.SettleNotification(ctx, t.ID, to == domain.StatusSuccess, maxDebitTries); err != nil {
			return err
		}
	}
	evt := map[domain.TxnStatus]string{domain.StatusSuccess: "txn.success", domain.StatusFailed: "txn.failed", domain.StatusDeemed: "txn.deemed"}[to]
	return s.enqueue(ctx, tx, t.TenantID, eventID(t.ID, string(to)), evt, t)
}

func (s *Service) enqueue(ctx context.Context, tx *store.Store, tenantID, id, typ string, data any) error {
	return tx.Enqueue(ctx, tenantID, id, typ, map[string]any{
		"event_id": id, "type": typ, "created_at": time.Now().UTC().Format(time.RFC3339), "data": data,
	})
}
