package core

import (
	"context"
	"fmt"
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

// CheckAllowed applies NPCI's status-check rules (UPI-OC-215 FY25-26) to an on-demand check: the first
// ReqChkTxn no earlier than FirstCheckAfter (90 s) after the payment, at most MaxCheckAttempts (3) checks,
// all within CheckWindow (2 h). Past that, the T+1 settlement file decides the outcome.
func (s *Service) CheckAllowed(t *domain.Txn, now time.Time) error {
	if t.Status != domain.StatusInitiated && t.Status != domain.StatusPending {
		return errUnprocessable("txn_final", "transaction is already %s", t.Status)
	}
	if t.Status == domain.StatusInitiated && t.Type != domain.TypeCollect && t.Type != domain.TypeMandateDebit && now.Before(t.ExpiresAt) {
		return errUnprocessable("awaiting_payer", "the customer has until %s to pay; there is nothing to check yet", t.ExpiresAt.Format(time.RFC3339))
	}
	if wait := t.CreatedAt.Add(s.cfg.FirstCheckAfter).Sub(now); wait > 0 {
		return errUnprocessable("check_too_early", "NPCI allows the first status check %s after the payment; try again in %s",
			s.cfg.FirstCheckAfter, wait.Round(time.Second))
	}
	if t.CheckAttempts >= s.cfg.MaxCheckAttempts {
		return errUnprocessable("check_limit_reached", "NPCI allows %d status checks per payment and all have been used; the settlement file will confirm the outcome",
			s.cfg.MaxCheckAttempts)
	}
	if now.Sub(t.CreatedAt) > s.cfg.CheckWindow {
		return errUnprocessable("check_window_closed", "NPCI allows status checks only within %s of the payment; the settlement file will confirm the outcome", s.cfg.CheckWindow)
	}
	return nil
}

// CheckTxnNow runs a status check immediately (GET status?refresh=true and the admin console) when NPCI's rules allow one.
func (s *Service) CheckTxnNow(ctx context.Context, t *domain.Txn) error {
	if err := s.CheckAllowed(t, time.Now()); err != nil {
		return err
	}
	return s.checkTxn(ctx, t)
}

func (s *Service) checkTxn(ctx context.Context, snapshot *domain.Txn) error {
	now := time.Now()
	// An intent or QR nobody has paid yet has nothing to check: NPCI treats such checks as API misuse.
	// Wait until it expires, then check once whether a payment slipped through without a callback.
	if snapshot.Status == domain.StatusInitiated && snapshot.Type != domain.TypeCollect && snapshot.Type != domain.TypeMandateDebit &&
		now.Before(snapshot.ExpiresAt) {
		return s.store.InTx(ctx, func(tx *store.Store) error {
			t, err := tx.LockTxn(ctx, snapshot.ID)
			if err != nil {
				return err
			}
			next := t.ExpiresAt.Add(s.cfg.FirstCheckAfter)
			return tx.UpdateTxn(ctx, t, store.TxnUpdate{NextCheckAt: &next}, "poller", "")
		})
	}
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
		source := fmt.Sprintf("chktxn %d/%d", attempts, s.cfg.MaxCheckAttempts)
		switch result {
		case npci.ResultSuccess, npci.ResultFailure, npci.ResultDeemed:
			return s.applyPayment(ctx, tx, t, result, code, reasonFor(code), rrn, payer, source)
		}
		if result == npci.ResultNotFound && now.After(t.ExpiresAt) {
			return s.applyPayment(ctx, tx, t, npci.ResultFailure, npci.CodeCollectExpired, "expired without payment", "", nil, source)
		}
		next := now.Add(s.backoff(attempts - 1))
		awaitingPayer := t.Status == domain.StatusInitiated && now.Before(t.ExpiresAt)
		if awaitingPayer && next.After(t.ExpiresAt) {
			next = t.ExpiresAt.Add(time.Second)
		}
		if attempts >= s.cfg.MaxCheckAttempts || next.Sub(t.CreatedAt) > s.cfg.CheckWindow {
			if awaitingPayer {
				// A collect the payer hasn't approved yet: stop checking and let it expire.
				exp := t.ExpiresAt.Add(time.Second)
				return tx.UpdateTxn(ctx, t, store.TxnUpdate{NextCheckAt: &exp, CheckAttempts: &attempts}, source, "awaiting payer approval")
			}
			return s.applyPayment(ctx, tx, t, npci.ResultDeemed, npci.CodeDeemed,
				fmt.Sprintf("no final status after %d status checks; the settlement file will confirm it", attempts), "", nil, source)
		}
		detail := "still pending"
		if chkErr != nil {
			detail = "bank did not answer the status check"
		}
		return tx.UpdateTxn(ctx, t, store.TxnUpdate{NextCheckAt: &next, CheckAttempts: &attempts}, source, detail)
	})
}
