package core

import (
	"context"

	"github.com/AppTestify/UPI-Switch/internal/store"
)

// ResolvePayment applies a reconciliation outcome to a payment (DEEMED/PENDING → SUCCESS|FAILED).
func (s *Service) ResolvePayment(ctx context.Context, txnID, result, rrn string) error {
	return s.store.InTx(ctx, func(tx *store.Store) error {
		t, err := tx.LockTxn(ctx, txnID)
		if err != nil {
			return err
		}
		code := ""
		if result != "SUCCESS" {
			code = "RCN"
		}
		return s.applyPayment(ctx, tx, t, result, code, "resolved by T+1 reconciliation", rrn, nil, "recon")
	})
}

// ResolveRefund applies a reconciliation outcome to a refund.
func (s *Service) ResolveRefund(ctx context.Context, refundID, result, rrn string) error {
	return s.store.InTx(ctx, func(tx *store.Store) error {
		r, err := tx.LockRefund(ctx, refundID)
		if err != nil {
			return err
		}
		code := ""
		if result != "SUCCESS" {
			code = "RCN"
		}
		return s.applyRefund(ctx, tx, r, result, code, "resolved by T+1 reconciliation", rrn, "recon")
	})
}
