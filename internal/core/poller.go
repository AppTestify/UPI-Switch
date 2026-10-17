package core

import (
	"context"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// RunPoller polls due transactions and refunds until ctx is cancelled (sequence steps 15–16).
func (s *Service) RunPoller(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.PollOnce(ctx); err != nil {
				s.log.Error("poller", "err", err)
			} else if n > 0 {
				s.log.Debug("poller checked", "count", n)
			}
		}
	}
}

// PollOnce claims and checks every due item once; it returns how many were checked.
func (s *Service) PollOnce(ctx context.Context) (int, error) {
	lease := 2 * time.Minute
	txns, err := s.store.ClaimDueTxns(ctx, time.Now(), lease, 200)
	if err != nil {
		return 0, err
	}
	for _, t := range txns {
		if err := s.checkTxn(ctx, t); err != nil {
			s.log.Error("status check failed", "txn", t.ID, "err", err)
		}
	}
	refunds, err := s.store.ClaimDueRefunds(ctx, time.Now(), lease, 200)
	if err != nil {
		return len(txns), err
	}
	for _, r := range refunds {
		if err := s.checkRefund(ctx, r); err != nil {
			s.log.Error("refund status check failed", "refund", r.ID, "err", err)
		}
	}
	return len(txns) + len(refunds), nil
}

func (s *Service) chkTxn(ctx context.Context, pspID, id string) (*npci.Message, error) {
	m := npci.New(npci.APIReqChkTxn, s.cfg.OrgID)
	m.Txn = &npci.Txn{ID: id, OrgTxnID: id, Ts: npci.Timestamp(time.Now()), Type: npci.TxnChkTxn}
	return s.exchange(ctx, pspID, m)
}

// CheckTxnNow runs a status check immediately (used by GET status?refresh=true and the admin console).
func (s *Service) CheckTxnNow(ctx context.Context, t *domain.Txn) error { return s.checkTxn(ctx, t) }

func (s *Service) checkTxn(ctx context.Context, snapshot *domain.Txn) error {
	resp, chkErr := s.chkTxn(ctx, snapshot.PSPID, snapshot.ID)
	return s.store.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockTxn(ctx, snapshot.ID)
		if err != nil {
			return err
		}
		if t.Status != domain.StatusInitiated && t.Status != domain.StatusPending {
			return tx.UpdateTxn(ctx, t, store.TxnUpdate{ClearCheck: true}, "poller", "")
		}
		now := time.Now()
		attempts := t.CheckAttempts + 1
		result, code, rrn := npci.ResultPending, "", ""
		var payer *npci.Party
		if chkErr == nil && resp.Resp != nil {
			result, code = resp.Resp.Result, resp.Resp.ErrCode
			payer = resp.Payer
			if resp.Txn != nil {
				rrn = resp.Txn.CustRef
			}
		} else if chkErr != nil {
			s.log.Warn("ReqChkTxn failed", "txn", t.ID, "psp", t.PSPID, "err", chkErr)
		}
		switch result {
		case npci.ResultSuccess, npci.ResultFailure, npci.ResultDeemed:
			return s.applyPayment(ctx, tx, t, result, code, reasonFor(code), rrn, payer, "chktxn")
		}
		expired := now.After(t.ExpiresAt)
		if result == npci.ResultNotFound && expired {
			return s.applyPayment(ctx, tx, t, npci.ResultFailure, npci.CodeCollectExpired, "expired without payment", "", nil, "chktxn")
		}
		awaitingPayer := t.Status == domain.StatusInitiated && !expired
		if !awaitingPayer && attempts >= s.cfg.MaxCheckAttempts {
			return s.applyPayment(ctx, tx, t, npci.ResultDeemed, npci.CodeDeemed, "no final status after status checks", "", nil, "chktxn")
		}
		next := now.Add(s.backoff(attempts - 1))
		if awaitingPayer && next.After(t.ExpiresAt) {
			next = t.ExpiresAt.Add(time.Second)
		}
		return tx.UpdateTxn(ctx, t, store.TxnUpdate{NextCheckAt: &next, CheckAttempts: &attempts}, "poller", "")
	})
}
