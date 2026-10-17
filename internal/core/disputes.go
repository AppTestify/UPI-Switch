package core

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// DisputeRequest is the body of POST /v1/upi/disputes.
type DisputeRequest struct {
	TxnID      string `json:"txn_id"`
	Type       string `json:"type"` // TCC | RET | CHARGEBACK | COMPLAINT
	ReasonCode string `json:"reason_code"`
	Remarks    string `json:"remarks"`
	Amount     string `json:"amount"` // optional; defaults to the txn amount
}

var disputeTypes = []string{string(domain.DisputeTCC), string(domain.DisputeRET), string(domain.DisputeChargeback), string(domain.DisputeComplaint)}

// RaiseDispute files a UDIR complaint (ReqComplaint) with the acquiring PSP.
func (s *Service) RaiseDispute(ctx context.Context, t *domain.Tenant, req DisputeRequest) (*domain.Dispute, error) {
	if !slices.Contains(disputeTypes, req.Type) {
		return nil, errValidation("type must be one of %v", disputeTypes)
	}
	txn, err := s.store.GetTxn(ctx, t.ID, req.TxnID)
	if err != nil {
		return nil, storeErr(err, "transaction")
	}
	amt := txn.AmountPaise
	if req.Amount != "" {
		if amt, err = domain.ParseAmount(req.Amount); err != nil || amt > txn.AmountPaise {
			return nil, errValidation("amount must be a valid value not above the transaction amount")
		}
	}
	return s.raiseDispute(ctx, txn, domain.DisputeType(req.Type), req.ReasonCode, req.Remarks, amt, "tenant:"+t.ID)
}

func (s *Service) raiseDispute(ctx context.Context, txn *domain.Txn, typ domain.DisputeType, reason, remarks string, amt int64, actor string) (*domain.Dispute, error) {
	if existing, err := s.store.OpenDisputeForTxn(ctx, txn.ID, typ); err == nil {
		return existing, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	d := &domain.Dispute{
		ID: npci.NewTxnID(s.cfg.OrgID), TenantID: txn.TenantID, TxnID: txn.ID, Type: typ, Status: domain.DisputeRaised,
		ReasonCode: reason, Remarks: remarks, AmountPaise: amt, Source: actor,
	}
	err := s.store.InTx(ctx, func(tx *store.Store) error {
		if err := tx.InsertDispute(ctx, d); err != nil {
			return err
		}
		return tx.Audit(ctx, actor, "dispute.raise", "dispute", d.ID, map[string]any{"txn_id": txn.ID, "type": typ})
	})
	if err != nil {
		return nil, err
	}
	m := npci.New(npci.APIReqComplaint, s.cfg.OrgID)
	m.Txn = &npci.Txn{ID: d.ID, Ts: npci.Timestamp(time.Now()), Type: npci.TxnComplaint, OrgTxnID: txn.ID, OrgRRN: txn.RRN}
	m.Complaint = &npci.Complaint{Type: string(typ), ReasonCode: reason, Remarks: remarks, AdjAmount: domain.FormatAmount(amt)}
	if _, err := s.exchange(ctx, txn.PSPID, m); err != nil {
		// Stays RAISED; ops can re-file from the admin console.
		s.log.Warn("ReqComplaint not accepted", "dispute", d.ID, "err", err)
		return d, nil
	}
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		cur, err := tx.LockDispute(ctx, d.ID)
		if err != nil {
			return err
		}
		*d = *cur
		if d.Status != domain.DisputeRaised {
			return nil
		}
		if err := tx.UpdateDispute(ctx, d, domain.DisputeReview, ""); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, d.TenantID, eventID(d.ID, string(d.Status)), "dispute.updated", d)
	})
	return d, err
}

func (s *Service) complaintCallback(ctx context.Context, tx *store.Store, m *npci.Message) (string, error) {
	d, err := tx.LockDispute(ctx, m.Txn.ID)
	if errors.Is(err, store.ErrNotFound) {
		return AckTxnNotFound, nil
	}
	if err != nil {
		return "", err
	}
	if d.Status == domain.DisputeAccepted || d.Status == domain.DisputeRejected {
		return "", nil
	}
	to := domain.DisputeRejected
	resolution := "rejected by bank"
	if m.Resp.Result == npci.ResultSuccess {
		to, resolution = domain.DisputeAccepted, "accepted by bank"
	}
	if m.Complaint != nil && m.Complaint.AdjFlag != "" {
		resolution += " (" + m.Complaint.AdjFlag + ")"
	}
	if err := tx.UpdateDispute(ctx, d, to, resolution); err != nil {
		return "", err
	}
	return "", s.enqueue(ctx, tx, d.TenantID, eventID(d.ID, string(to)), "dispute.updated", d)
}

// GetDispute returns a tenant's dispute.
func (s *Service) GetDispute(ctx context.Context, tenantID, id string) (*domain.Dispute, error) {
	d, err := s.store.GetDispute(ctx, tenantID, id)
	return d, storeErr(err, "dispute")
}

// AutoTCC raises a TCC on behalf of the switch, used by reconciliation when the customer was
// debited but the payment is not credited.
func (s *Service) AutoTCC(ctx context.Context, txn *domain.Txn, remarks string) (*domain.Dispute, error) {
	return s.raiseDispute(ctx, txn, domain.DisputeTCC, "U008", remarks, txn.AmountPaise, "system:recon")
}

// Heartbeat sends ReqHbt to every PSP so the router learns about downtime even without traffic.
func (s *Service) Heartbeat(ctx context.Context) {
	s.mu.RLock()
	ids := make([]string, 0, len(s.peers))
	for id, p := range s.peers {
		if p.Key != nil {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	for _, id := range ids {
		m := npci.New(npci.APIReqHbt, s.cfg.OrgID)
		m.Txn = &npci.Txn{ID: npci.NewTxnID(s.cfg.OrgID), Ts: npci.Timestamp(time.Now()), Type: npci.TxnHbt}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if _, err := s.exchange(cctx, id, m); err != nil {
			s.log.Warn("heartbeat failed", "psp", id, "err", err)
		}
		cancel()
	}
}

// RunHeartbeat sends heartbeats on a timer until ctx is cancelled.
func (s *Service) RunHeartbeat(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Heartbeat(ctx)
		}
	}
}
