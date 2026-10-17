package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// RefundRequest is the body of POST /v1/upi/refund (and each item of a bulk refund).
type RefundRequest struct {
	TxnID             string `json:"txn_id"`
	MerchantRefundRef string `json:"merchant_refund_ref"`
	Amount            string `json:"amount"`
	Mode              string `json:"mode"` // ONLINE (default) | OFFLINE
}

// CreateRefund validates against the original payment and refunded total, then executes online
// (ReqPay REFUND via the acquiring PSP) or queues for the offline refund file. An online refund
// that cannot reach the bank falls back to the offline queue.
func (s *Service) CreateRefund(ctx context.Context, t *domain.Tenant, req RefundRequest, bulk bool) (*domain.Refund, error) {
	if err := validateRef("merchant_refund_ref", req.MerchantRefundRef); err != nil {
		return nil, err
	}
	amt, err := domain.ParseAmount(req.Amount)
	if err != nil {
		return nil, errValidation("%v", err)
	}
	mode := domain.RefundOnline
	switch req.Mode {
	case "", string(domain.RefundOnline):
		if bulk {
			mode = domain.RefundBulk
		}
	case string(domain.RefundOffline):
		mode = domain.RefundOffline
	default:
		return nil, errValidation("mode must be ONLINE or OFFLINE")
	}
	var r *domain.Refund
	var txn *domain.Txn
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		var err error
		txn, err = tx.GetTxn(ctx, t.ID, req.TxnID)
		if err != nil {
			return storeErr(err, "transaction")
		}
		if _, err := tx.LockTxn(ctx, txn.ID); err != nil { // serialise refunds per payment
			return err
		}
		if txn.Status != domain.StatusSuccess {
			return errUnprocessable("not_refundable", "only SUCCESS payments can be refunded (status %s)", txn.Status)
		}
		done, err := tx.RefundedTotal(ctx, txn.ID)
		if err != nil {
			return err
		}
		if done+amt > txn.AmountPaise {
			return errUnprocessable("refund_exceeds_payment", "refund %s exceeds refundable balance %s",
				domain.FormatAmount(amt), domain.FormatAmount(txn.AmountPaise-done))
		}
		r = &domain.Refund{
			ID: npci.NewTxnID(s.cfg.OrgID), TenantID: t.ID, TxnID: txn.ID, MerchantRefundRef: req.MerchantRefundRef,
			AmountPaise: amt, Status: domain.StatusInitiated, Mode: mode, PSPID: txn.PSPID,
		}
		if mode == domain.RefundOffline {
			r.Status = domain.StatusOfflineQueued
		}
		if err := tx.InsertRefund(ctx, r); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return &Error{Status: http.StatusConflict, Code: "duplicate_refund_ref", Message: "merchant_refund_ref already used"}
			}
			return err
		}
		return tx.Audit(ctx, "tenant:"+t.ID, "refund.create", "refund", r.ID, map[string]any{"txn_id": txn.ID, "amount": domain.FormatAmount(amt), "mode": mode})
	})
	if err != nil {
		return nil, err
	}
	s.m.Transition("refund", string(r.Status))
	if mode == domain.RefundOffline {
		return r, nil
	}
	return r, s.sendRefund(ctx, txn, r)
}

func (s *Service) sendRefund(ctx context.Context, txn *domain.Txn, r *domain.Refund) error {
	payer, err := s.vault.Decrypt(txn.PayerVPAEnc)
	if err != nil {
		return err
	}
	m := npci.New(npci.APIReqPay, s.cfg.OrgID)
	m.Txn = &npci.Txn{ID: r.ID, RefID: r.MerchantRefundRef, Ts: npci.Timestamp(time.Now()), Type: npci.TxnRefund,
		OrgTxnID: txn.ID, OrgRRN: txn.RRN, Note: "Refund"}
	amt := &npci.Amount{Value: domain.FormatAmount(r.AmountPaise), Curr: "INR"}
	// The merchant pays the customer back: payee of the original becomes the payer.
	m.Payer = &npci.Party{Addr: txn.PayeeVPA, Name: txn.PayeeName, SeqNum: "1", Type: "ENTITY", Code: txn.MCC, Amount: amt}
	m.Payees = &npci.Payees{Payee: []npci.Party{{Addr: payer, SeqNum: "1", Type: "PERSON", Amount: amt}}}
	_, sendErr := s.exchange(ctx, r.PSPID, m)
	return s.store.InTx(ctx, func(tx *store.Store) error {
		cur, err := tx.LockRefund(ctx, r.ID)
		if err != nil {
			return err
		}
		*r = *cur
		if cur.Status != domain.StatusInitiated {
			return nil
		}
		switch {
		case sendErr == nil:
			next := time.Now().Add(s.cfg.FirstCheckAfter)
			if err := tx.UpdateRefund(ctx, r, domain.StatusPending, "", "", "", &next, "ack"); err != nil {
				return err
			}
		case npci.IsTechnical(sendErr):
			s.log.Warn("online refund failed; queued offline", "refund", r.ID, "err", sendErr)
			if err := tx.QueueRefundOffline(ctx, r); err != nil {
				return err
			}
		default:
			if err := s.applyRefund(ctx, tx, r, npci.ResultFailure, npci.CodeTechnical, sendErr.Error(), "", "ack"); err != nil {
				return err
			}
		}
		s.m.Transition("refund", string(r.Status))
		return nil
	})
}

func (s *Service) refundCallback(ctx context.Context, tx *store.Store, m *npci.Message) (string, error) {
	r, err := tx.LockRefund(ctx, m.Txn.ID)
	if errors.Is(err, store.ErrNotFound) {
		return AckTxnNotFound, nil
	}
	if err != nil {
		return "", err
	}
	return "", s.applyRefund(ctx, tx, r, m.Resp.Result, m.Resp.ErrCode, reasonFor(m.Resp.ErrCode), m.Txn.CustRef, "callback")
}

func (s *Service) applyRefund(ctx context.Context, tx *store.Store, r *domain.Refund, result, code, reason, rrn, source string) error {
	var to domain.TxnStatus
	switch result {
	case npci.ResultSuccess:
		to, code = domain.StatusSuccess, npci.CodeApproved
	case npci.ResultFailure:
		to = domain.StatusFailed
	case npci.ResultDeemed:
		to = domain.StatusDeemed
	default:
		return fmt.Errorf("unexpected result %q", result)
	}
	if r.Status == to {
		return nil
	}
	if !domain.CanTransition(r.Status, to) && !(r.Status == domain.StatusOfflineQueued && to.IsFinal()) {
		s.log.Warn("ignoring conflicting refund outcome", "refund", r.ID, "status", r.Status, "reported", to)
		return nil
	}
	if err := tx.UpdateRefund(ctx, r, to, rrn, code, reason, nil, source); err != nil {
		return err
	}
	s.m.Transition("refund", string(to))
	evt := map[domain.TxnStatus]string{domain.StatusSuccess: "refund.success", domain.StatusFailed: "refund.failed", domain.StatusDeemed: "refund.deemed"}[to]
	return s.enqueue(ctx, tx, r.TenantID, eventID(r.ID, string(to)), evt, r)
}

func (s *Service) checkRefund(ctx context.Context, snap *domain.Refund) error {
	resp, chkErr := s.chkTxn(ctx, snap.PSPID, snap.ID)
	return s.store.InTx(ctx, func(tx *store.Store) error {
		r, err := tx.LockRefund(ctx, snap.ID)
		if err != nil {
			return err
		}
		if r.Status != domain.StatusPending {
			return tx.UpdateRefund(ctx, r, "", "", "", "", nil, "poller")
		}
		if chkErr == nil && resp.Resp != nil {
			switch resp.Resp.Result {
			case npci.ResultSuccess, npci.ResultFailure, npci.ResultDeemed:
				rrn := ""
				if resp.Txn != nil {
					rrn = resp.Txn.CustRef
				}
				return s.applyRefund(ctx, tx, r, resp.Resp.Result, resp.Resp.ErrCode, reasonFor(resp.Resp.ErrCode), rrn, "chktxn")
			}
		}
		r.CheckAttempts++
		if r.CheckAttempts >= s.cfg.MaxCheckAttempts {
			return s.applyRefund(ctx, tx, r, npci.ResultDeemed, npci.CodeDeemed, "no final status after status checks", "", "chktxn")
		}
		next := time.Now().Add(s.backoff(r.CheckAttempts - 1))
		return tx.UpdateRefund(ctx, r, "", "", "", "", &next, "poller")
	})
}

// GetRefund returns a tenant's refund.
func (s *Service) GetRefund(ctx context.Context, tenantID, id string) (*domain.Refund, error) {
	r, err := s.store.GetRefund(ctx, tenantID, id)
	return r, storeErr(err, "refund")
}

// OfflineRefundResult is one line of the bank's offline refund response file.
type OfflineRefundResult struct {
	RefundID string `json:"refund_id"`
	Status   string `json:"status"` // SUCCESS | FAILED
	RRN      string `json:"rrn"`
	RespCode string `json:"resp_code"`
}

// ApplyOfflineRefundResults closes OFFLINE_QUEUED refunds from the bank's response file.
func (s *Service) ApplyOfflineRefundResults(ctx context.Context, actor string, results []OfflineRefundResult) (int, error) {
	n := 0
	for _, res := range results {
		err := s.store.InTx(ctx, func(tx *store.Store) error {
			r, err := tx.LockRefund(ctx, res.RefundID)
			if err != nil {
				return err
			}
			if r.Status != domain.StatusOfflineQueued {
				return nil
			}
			result := npci.ResultFailure
			if res.Status == string(domain.StatusSuccess) {
				result = npci.ResultSuccess
			}
			if err := s.applyRefund(ctx, tx, r, result, res.RespCode, "offline refund file", res.RRN, "offline_file"); err != nil {
				return err
			}
			n++
			return tx.Audit(ctx, actor, "refund.offline_result", "refund", r.ID, res)
		})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return n, err
		}
	}
	return n, nil
}
