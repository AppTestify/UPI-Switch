package core

import (
	"context"
	"errors"
	"fmt"
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
	mer, err := s.merchantFor(ctx, t, req.Merchant)
	if err != nil {
		return nil, err
	}
	if err := s.checkRisk(ctx, t, maxAmt, req.Merchant.MCC, "", mer.Verified()); err != nil {
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
		m := s.reqMandate(md, "CREATE", req.PayerVPA, payeeVPA(t, req.Merchant.ID, p.Handle), payeeName(mer))
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

// AutoPay rules (RBI e-mandate framework, RBI/DPSS/2026-27/396; NPCI UPI-OC-215-A FY25-26).
const (
	afaCapPaise     = 15_000 * 100  // debits above this need the payer's additional authentication
	afaCapHighPaise = 100_000 * 100 // ₹1 lakh for insurance, mutual funds and other capital markets
	maxDebitTries   = 4             // one attempt and three retries per pre-debit notification
)

// peakWindows are NPCI's peak hours, when AutoPay debits must not be presented.
var peakWindows = [][2]int{{10 * 60, 13 * 60}, {17 * 60, 21*60 + 30}}

// AFACap is the largest AutoPay debit that can run without the payer authenticating it.
func AFACap(mcc string) int64 {
	switch domain.CategoryFor(mcc).Key {
	case "insurance", "capital_markets":
		return afaCapHighPaise
	}
	return afaCapPaise
}

// OffPeak reports whether t is outside NPCI's peak hours, and when the next off-peak window opens.
func OffPeak(t time.Time, loc *time.Location) (bool, time.Time) {
	lt := t.In(loc)
	mins := lt.Hour()*60 + lt.Minute()
	for _, w := range peakWindows {
		if mins >= w[0] && mins < w[1] {
			return false, time.Date(lt.Year(), lt.Month(), lt.Day(), w[1]/60, w[1]%60, 0, 0, loc)
		}
	}
	return true, t
}

// NotifyRequest is the body of POST /v1/upi/mandate/{id}/notify.
type NotifyRequest struct {
	Amount    string `json:"amount"`
	DebitDate string `json:"debit_date"` // YYYY-MM-DD
}

// NotifyMandate sends the payer a pre-debit notification through the sponsor bank. RBI requires one at
// least 24 hours before every AutoPay debit; a new notification replaces any earlier unused one.
func (s *Service) NotifyMandate(ctx context.Context, t *domain.Tenant, mandateID string, req NotifyRequest) (*domain.MandateNotification, error) {
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
	day, err := time.ParseInLocation("2006-01-02", req.DebitDate, s.cfg.Location)
	if err != nil {
		return nil, errValidation("debit_date must be YYYY-MM-DD")
	}
	now := time.Now()
	earliest := now.Add(s.cfg.PreDebitNotice).In(s.cfg.Location)
	if day.Before(time.Date(earliest.Year(), earliest.Month(), earliest.Day(), 0, 0, 0, 0, s.cfg.Location)) {
		return nil, errUnprocessable("notice_too_short", "the payer must be notified at least %s before the debit; the earliest debit date is %s",
			s.cfg.PreDebitNotice, earliest.Format("2006-01-02"))
	}
	if day.Before(md.StartDate) || day.After(md.EndDate) {
		return nil, errUnprocessable("mandate_out_of_validity", "mandate is valid %s to %s", md.StartDate.Format("2006-01-02"), md.EndDate.Format("2006-01-02"))
	}
	payer, err := s.vault.Decrypt(md.PayerVPAEnc)
	if err != nil {
		return nil, err
	}
	msg := s.reqMandate(md, "NOTIFY", payer, md.PayeeVPA, md.MerchantID)
	msg.Mandate.Amount = &npci.Amount{Value: domain.FormatAmount(amt), Curr: "INR"}
	if _, err := s.exchange(ctx, md.PSPID, msg); err != nil {
		return nil, errUpstream("pre-debit notification failed at the bank: %v", err)
	}
	n := &domain.MandateNotification{ID: npci.NewTxnID(s.cfg.OrgID), MandateID: md.ID, TenantID: t.ID, AmountPaise: amt, DebitDate: day}
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		if old, err := tx.LockOpenNotification(ctx, md.ID); err == nil {
			if err := tx.SetNotificationStatus(ctx, old.ID, "SUPERSEDED"); err != nil {
				return err
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.InsertNotification(ctx, n); err != nil {
			return err
		}
		if err := tx.AddEvent(ctx, md.ID, string(md.Status), string(md.Status), "api",
			fmt.Sprintf("pre-debit notification %d: %s on %s", n.Seq, domain.FormatAmount(amt), req.DebitDate)); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, t.ID, eventID(n.ID, "NOTIFIED"), "mandate.notified", n)
	})
	return n, err
}

// ExecuteMandate debits an ACTIVE mandate (ReqPay MANDATE_EXECUTE with the UMN). The result arrives
// as RespPay like any collect. The debit needs a pre-debit notification at least PreDebitNotice old,
// must stay within the notified amount and NPCI's retry limit, and is refused during peak hours.
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
	now := time.Now()
	today := now.In(s.cfg.Location)
	today = time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	if today.Before(md.StartDate) || today.After(md.EndDate) {
		return nil, errUnprocessable("mandate_out_of_validity", "mandate is valid %s to %s", md.StartDate.Format("2006-01-02"), md.EndDate.Format("2006-01-02"))
	}
	if cap := AFACap(md.MCC); amt > cap {
		return nil, errUnprocessable("afa_required", "AutoPay debits above %s need the payer to authenticate with their UPI PIN", domain.FormatAmount(cap))
	}
	if !s.cfg.AllowPeakMandates {
		if ok, opens := OffPeak(now, s.cfg.Location); !ok {
			return nil, errUnprocessable("peak_hours", "NPCI does not allow AutoPay debits between 10:00–13:00 and 17:00–21:30; retry after %s",
				opens.Format("15:04 MST"))
		}
	}
	name := md.MerchantID
	if mer, err := s.store.GetMerchant(ctx, t.ID, md.MerchantID); err == nil {
		if mer.Status == domain.MerchantSuspended {
			return nil, errUnprocessable("merchant_suspended", "merchant %s is suspended", mer.ID)
		}
		name = payeeName(mer)
		if err := s.checkRisk(ctx, t, amt, md.MCC, md.PayerVPAToken, mer.Verified()); err != nil {
			return nil, err
		}
	} else if errors.Is(err, store.ErrNotFound) {
		if err := s.checkRisk(ctx, t, amt, md.MCC, md.PayerVPAToken, false); err != nil {
			return nil, err
		}
	} else {
		return nil, err
	}
	payer, err := s.vault.Decrypt(md.PayerVPAEnc)
	if err != nil {
		return nil, err
	}
	txn := &domain.Txn{
		ID: npci.NewTxnID(s.cfg.OrgID), TenantID: t.ID, MerchantRef: req.MerchantRef, Type: domain.TypeMandateDebit,
		Status: domain.StatusInitiated, AmountPaise: amt, Currency: "INR", PayeeVPA: md.PayeeVPA, PayeeName: name,
		PayerVPAMasked: md.PayerVPAMasked, PayerVPAToken: md.PayerVPAToken, PayerVPAEnc: md.PayerVPAEnc,
		MerchantID: md.MerchantID, MCC: md.MCC, Note: req.Note, PSPID: md.PSPID, MandateID: md.ID, ExpiresAt: now.Add(s.cfg.CollectExpiry),
	}
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		n, err := tx.LockOpenNotification(ctx, md.ID)
		if errors.Is(err, store.ErrNotFound) {
			return errUnprocessable("pre_debit_notice_required", "notify the payer (POST /v1/upi/mandate/%s/notify) at least %s before debiting", md.ID, s.cfg.PreDebitNotice)
		}
		if err != nil {
			return err
		}
		if ready := n.NotifiedAt.Add(s.cfg.PreDebitNotice); now.Before(ready) {
			return errUnprocessable("pre_debit_notice_too_recent", "the payer was notified at %s; the debit can run from %s",
				n.NotifiedAt.In(s.cfg.Location).Format("2006-01-02 15:04"), ready.In(s.cfg.Location).Format("2006-01-02 15:04"))
		}
		if amt > n.AmountPaise {
			return errUnprocessable("exceeds_notified_amount", "the payer was notified of %s", domain.FormatAmount(n.AmountPaise))
		}
		if n.Attempts >= maxDebitTries {
			return errUnprocessable("retry_limit_reached", "NPCI allows one attempt and three retries per notification; send a new pre-debit notification")
		}
		if err := tx.InsertTxn(ctx, txn); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return s.duplicateRef(ctx, t.ID, req.MerchantRef)
			}
			return err
		}
		return tx.MarkNotificationAttempt(ctx, n.ID, txn.ID)
	})
	if err != nil {
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
	return s.RevokeMandateBy(ctx, t, mandateID, "tenant:"+t.ID)
}

// RevokeMandateBy cancels a mandate on behalf of actor (the gateway, or an operator in the console).
func (s *Service) RevokeMandateBy(ctx context.Context, t *domain.Tenant, mandateID, actor string) (*domain.Mandate, error) {
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
		if err := tx.Audit(ctx, actor, "mandate.revoke", "mandate", md.ID, map[string]any{"umn": md.UMN}); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, md.TenantID, eventID(md.ID, string(domain.MandateRevoked)), "mandate.revoked", md)
	})
	return md, err
}
