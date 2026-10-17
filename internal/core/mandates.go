package core

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

var frequencies = []string{"ONETIME", "DAILY", "WEEKLY", "FORTNIGHTLY", "MONTHLY", "BIMONTHLY", "QUARTERLY", "HALFYEARLY", "YEARLY", "ASPRESENTED"}

// MandateRequest is the body of POST /v1/upi/mandate.
type MandateRequest struct {
	MerchantRef string   `json:"merchant_ref"`
	PayerVPA    string   `json:"payer_vpa"`
	MaxAmount   string   `json:"max_amount"`
	Frequency   string   `json:"frequency"`
	StartDate   string   `json:"start_date"` // YYYY-MM-DD
	EndDate     string   `json:"end_date"`
	Merchant    Merchant `json:"merchant"`
}

// CreateMandate registers a UPI AutoPay mandate: ReqMandate CREATE goes to the PSP and the payer
// approves it in their UPI app; RespMandate activates it with a UMN.
func (s *Service) CreateMandate(ctx context.Context, t *domain.Tenant, req MandateRequest) (*domain.Mandate, error) {
	if err := validateRef("merchant_ref", req.MerchantRef); err != nil {
		return nil, err
	}
	if err := req.Merchant.validate(); err != nil {
		return nil, err
	}
	if !ValidVPA(req.PayerVPA) {
		return nil, errValidation("payer_vpa is not a valid VPA")
	}
	if !slices.Contains(frequencies, req.Frequency) {
		return nil, errValidation("frequency must be one of %v", frequencies)
	}
	maxAmt, err := domain.ParseAmount(req.MaxAmount)
	if err != nil {
		return nil, errValidation("%v", err)
	}
	start, err1 := time.Parse("2006-01-02", req.StartDate)
	end, err2 := time.Parse("2006-01-02", req.EndDate)
	if err1 != nil || err2 != nil || end.Before(start) {
		return nil, errValidation("start_date and end_date must be YYYY-MM-DD with end on or after start")
	}
	if err := s.checkRisk(ctx, t, maxAmt, req.Merchant.MCC, ""); err != nil {
		return nil, err
	}
	psps, err := s.router.Rank(t.Routing)
	if err != nil {
		return nil, errUpstream("%v", err)
	}
	md := &domain.Mandate{
		ID: npci.NewTxnID(s.cfg.OrgID), TenantID: t.ID, MerchantRef: req.MerchantRef, Status: domain.MandatePending,
		PayerVPAMasked: security.MaskVPA(req.PayerVPA), PayerVPAToken: s.vault.Token(req.PayerVPA), PayerVPAEnc: s.vault.Encrypt(req.PayerVPA),
		PayeeVPA: payeeVPA(t, req.Merchant.ID, psps[0].Handle), MerchantID: req.Merchant.ID, MCC: req.Merchant.MCC,
		MaxAmountPaise: maxAmt, Frequency: req.Frequency, StartDate: start, EndDate: end, PSPID: psps[0].ID,
	}
	if err := s.store.InsertMandate(ctx, md); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, &Error{Status: http.StatusConflict, Code: "duplicate_merchant_ref", Message: "merchant_ref already used"}
		}
		return nil, err
	}
	var lastErr error
	for _, p := range psps {
		m := s.reqMandate(md, "CREATE", req.PayerVPA, payeeVPA(t, req.Merchant.ID, p.Handle), req.Merchant.Name)
		if _, err := s.exchange(ctx, p.ID, m); err != nil {
			lastErr = err
			if npci.IsTechnical(err) {
				continue
			}
			break
		}
		return md, s.store.InTx(ctx, func(tx *store.Store) error {
			cur, err := tx.LockMandate(ctx, md.ID)
			if err != nil {
				return err
			}
			*md = *cur
			if p.ID != md.PSPID || m.Payee().Addr != md.PayeeVPA {
				if err := tx.SetMandatePSP(ctx, md.ID, p.ID, m.Payee().Addr); err != nil {
					return err
				}
				md.PSPID, md.PayeeVPA = p.ID, m.Payee().Addr
			}
			return nil
		})
	}
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		cur, err := tx.LockMandate(ctx, md.ID)
		if err != nil {
			return err
		}
		*md = *cur
		if md.Status != domain.MandatePending {
			return nil
		}
		if err := tx.UpdateMandate(ctx, md, domain.MandateFailed, "", npci.CodeBankDown, "router"); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, md.TenantID, eventID(md.ID, string(domain.MandateFailed)), "mandate.failed", md)
	})
	if err != nil {
		return nil, err
	}
	s.log.Warn("mandate create failed on all PSPs", "mandate", md.ID, "err", lastErr)
	return md, nil
}

func (s *Service) reqMandate(md *domain.Mandate, typ, payerVPA, payee, payeeName string) *npci.Message {
	m := npci.New(npci.APIReqMandate, s.cfg.OrgID)
	m.Txn = &npci.Txn{ID: md.ID, RefID: md.MerchantRef, Ts: npci.Timestamp(time.Now()), Type: npci.TxnMandate, SubType: typ}
	m.Payer = &npci.Party{Addr: payerVPA, SeqNum: "1", Type: "PERSON"}
	m.Payees = &npci.Payees{Payee: []npci.Party{{Addr: payee, Name: payeeName, SeqNum: "1", Type: "ENTITY", Code: md.MCC}}}
	m.Mandate = &npci.Mandate{
		Type: typ, Name: md.MerchantRef, UMN: md.UMN, Recurrence: md.Frequency, AmountRule: "MAX",
		ValidityStart: md.StartDate.Format("02012006"), ValidityEnd: md.EndDate.Format("02012006"),
		Amount: &npci.Amount{Value: domain.FormatAmount(md.MaxAmountPaise), Curr: "INR"},
	}
	return m
}

func (s *Service) mandateCallback(ctx context.Context, tx *store.Store, m *npci.Message) (string, error) {
	md, err := tx.LockMandate(ctx, m.Txn.ID)
	if errors.Is(err, store.ErrNotFound) {
		return AckTxnNotFound, nil
	}
	if err != nil {
		return "", err
	}
	if md.Status != domain.MandatePending {
		return "", nil
	}
	umn := ""
	if m.Mandate != nil {
		umn = m.Mandate.UMN
	}
	to, evt := domain.MandateFailed, "mandate.failed"
	if m.Resp.Result == npci.ResultSuccess {
		to, evt = domain.MandateActive, "mandate.active"
	}
	if err := tx.UpdateMandate(ctx, md, to, umn, m.Resp.ErrCode, "callback"); err != nil {
		return "", err
	}
	return "", s.enqueue(ctx, tx, md.TenantID, eventID(md.ID, string(to)), evt, md)
}

// GetMandate returns a tenant's mandate.
func (s *Service) GetMandate(ctx context.Context, tenantID, id string) (*domain.Mandate, error) {
	m, err := s.store.GetMandate(ctx, tenantID, id)
	return m, storeErr(err, "mandate")
}

// ExecuteRequest is the body of POST /v1/upi/mandate/{id}/execute.
type ExecuteRequest struct {
	MerchantRef string `json:"merchant_ref"`
	Amount      string `json:"amount"`
	Note        string `json:"note"`
}

// ExecuteMandate debits an ACTIVE mandate (ReqPay MANDATE_EXECUTE with the UMN). The result arrives
// as RespPay like any collect.
func (s *Service) ExecuteMandate(ctx context.Context, t *domain.Tenant, mandateID string, req ExecuteRequest) (*domain.Txn, error) {
	if err := validateRef("merchant_ref", req.MerchantRef); err != nil {
		return nil, err
	}
	md, err := s.GetMandate(ctx, t.ID, mandateID)
	if err != nil {
		return nil, err
	}
	if md.Status != domain.MandateActive {
		return nil, errUnprocessable("mandate_not_active", "mandate is %s", md.Status)
	}
	amt, err := domain.ParseAmount(req.Amount)
	if err != nil {
		return nil, errValidation("%v", err)
	}
	if amt > md.MaxAmountPaise {
		return nil, errUnprocessable("exceeds_mandate", "amount exceeds mandate maximum %s", domain.FormatAmount(md.MaxAmountPaise))
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	if today.Before(md.StartDate) || today.After(md.EndDate) {
		return nil, errUnprocessable("mandate_out_of_validity", "mandate is valid %s to %s", md.StartDate.Format("2006-01-02"), md.EndDate.Format("2006-01-02"))
	}
	if err := s.checkRisk(ctx, t, amt, md.MCC, md.PayerVPAToken); err != nil {
		return nil, err
	}
	payer, err := s.vault.Decrypt(md.PayerVPAEnc)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	txn := &domain.Txn{
		ID: npci.NewTxnID(s.cfg.OrgID), TenantID: t.ID, MerchantRef: req.MerchantRef, Type: domain.TypeMandateDebit,
		Status: domain.StatusInitiated, AmountPaise: amt, Currency: "INR", PayeeVPA: md.PayeeVPA, PayeeName: md.MerchantID,
		PayerVPAMasked: md.PayerVPAMasked, PayerVPAToken: md.PayerVPAToken, PayerVPAEnc: md.PayerVPAEnc,
		MerchantID: md.MerchantID, MCC: md.MCC, Note: req.Note, PSPID: md.PSPID, MandateID: md.ID, ExpiresAt: now.Add(s.cfg.CollectExpiry),
	}
	if err := s.store.InsertTxn(ctx, txn); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, s.duplicateRef(ctx, t.ID, req.MerchantRef)
		}
		return nil, err
	}
	msg := s.reqPay(txn, npci.TxnMandateExe, payer, md.PayeeVPA, 0)
	msg.Mandate = &npci.Mandate{Type: "EXECUTE", UMN: md.UMN}
	if _, err := s.exchange(ctx, md.PSPID, msg); err != nil {
		if npci.IsTechnical(err) {
			return txn, s.failSend(ctx, txn.ID, err)
		}
		return txn, s.finishSend(ctx, txn.ID, md.PSPID, "", err.Error())
	}
	return txn, s.finishSend(ctx, txn.ID, md.PSPID, "", "")
}

// RevokeMandate cancels an ACTIVE or PENDING mandate.
func (s *Service) RevokeMandate(ctx context.Context, t *domain.Tenant, mandateID string) (*domain.Mandate, error) {
	md, err := s.GetMandate(ctx, t.ID, mandateID)
	if err != nil {
		return nil, err
	}
	if md.Status != domain.MandateActive && md.Status != domain.MandatePending {
		return nil, errUnprocessable("mandate_not_revocable", "mandate is %s", md.Status)
	}
	payer, err := s.vault.Decrypt(md.PayerVPAEnc)
	if err != nil {
		return nil, err
	}
	if _, err := s.exchange(ctx, md.PSPID, s.reqMandate(md, "REVOKE", payer, md.PayeeVPA, md.MerchantID)); err != nil {
		return nil, errUpstream("revoke failed at bank: %v", err)
	}
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		cur, err := tx.LockMandate(ctx, md.ID)
		if err != nil {
			return err
		}
		*md = *cur
		if err := tx.UpdateMandate(ctx, md, domain.MandateRevoked, "", "", "api"); err != nil {
			return err
		}
		if err := tx.Audit(ctx, "tenant:"+t.ID, "mandate.revoke", "mandate", md.ID, map[string]any{"umn": md.UMN}); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, md.TenantID, eventID(md.ID, string(domain.MandateRevoked)), "mandate.revoked", md)
	})
	return md, err
}
