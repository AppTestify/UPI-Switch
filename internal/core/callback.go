package core

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/domain"
	"github.com/AppTestify/UPI-Switch/internal/npci"
	"github.com/AppTestify/UPI-Switch/internal/store"
)

// Ack error codes the switch returns to banks on callbacks it cannot apply.
const (
	AckTxnNotFound    = "TNF"
	AckAmountMismatch = "AMM"
	AckPayeeMismatch  = "PVM"
)

// HandleCallback is interface 4: a PSP bank posts RespPay / RespMandate / RespComplaint.
// It verifies the XML signature, enforces the replay window, dedupes on msgId and applies the
// outcome in a single transaction, then returns a signed Ack.
func (s *Service) HandleCallback(ctx context.Context, pspID string, body []byte) ([]byte, error) {
	p, err := s.peer(pspID)
	if err != nil {
		return nil, errNotFound("psp")
	}
	m, err := npci.Parse(body, p.Key)
	if err != nil {
		s.m.Inc("upi_callbacks_total", "psp", pspID, "result", "bad_signature")
		return nil, &Error{Status: http.StatusUnauthorized, Code: "bad_signature", Message: err.Error()}
	}
	ts, err := npci.ParseTimestamp(m.Head.Ts)
	if err != nil || absDur(time.Since(ts)) > s.cfg.CallbackMaxSkew {
		s.m.Inc("upi_callbacks_total", "psp", pspID, "result", "stale")
		return nil, &Error{Status: http.StatusBadRequest, Code: "stale_message", Message: "message timestamp outside replay window"}
	}
	if m.Txn == nil || m.Resp == nil {
		return nil, errValidation("callback must carry Txn and Resp")
	}
	ackCode := ""
	err = s.store.InTx(ctx, func(tx *store.Store) error {
		fresh, err := tx.RecordCallback(ctx, m.Head.MsgID, pspID, m.API())
		if err != nil {
			return err
		}
		if !fresh {
			s.m.Inc("upi_callbacks_total", "psp", pspID, "result", "duplicate")
			return nil
		}
		switch m.API() {
		case npci.APIRespPay:
			if m.Txn.Type == npci.TxnRefund {
				ackCode, err = s.refundCallback(ctx, tx, m)
			} else {
				ackCode, err = s.paymentCallback(ctx, tx, pspID, m)
			}
		case npci.APIRespMandate:
			ackCode, err = s.mandateCallback(ctx, tx, m)
		case npci.APIRespComplaint:
			ackCode, err = s.complaintCallback(ctx, tx, m)
		default:
			return errValidation("unsupported callback %s", m.API())
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	res := "applied"
	if ackCode != "" {
		res = "rejected_" + ackCode
	}
	s.m.Inc("upi_callbacks_total", "psp", pspID, "result", res)
	return npci.Marshal(npci.Ack(m, s.cfg.OrgID, ackCode), s.signer)
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (s *Service) paymentCallback(ctx context.Context, tx *store.Store, pspID string, m *npci.Message) (string, error) {
	t, err := tx.LockTxn(ctx, m.Txn.ID)
	if errors.Is(err, store.ErrNotFound) && m.Txn.RefID != "" {
		t, err = tx.LockTxn(ctx, m.Txn.RefID)
	}
	if errors.Is(err, store.ErrNotFound) {
		s.log.Warn("callback for unknown txn", "txn", m.Txn.ID, "psp", pspID)
		return AckTxnNotFound, nil
	}
	if err != nil {
		return "", err
	}
	if m.Payer != nil && m.Payer.Amount != nil {
		amt, err := domain.ParseAmount(m.Payer.Amount.Value)
		if err != nil || amt != t.AmountPaise {
			s.log.Error("callback amount mismatch", "txn", t.ID, "expected", t.Amount(), "got", m.Payer.Amount.Value)
			return AckAmountMismatch, nil
		}
	}
	if pe := m.Payee(); pe != nil && pe.Addr != "" && !strings.EqualFold(pe.Addr, t.PayeeVPA) {
		s.log.Error("callback payee mismatch", "txn", t.ID, "expected", t.PayeeVPA, "got", pe.Addr)
		return AckPayeeMismatch, nil
	}
	if npci.IsTechnicalDecline(m.Resp.ErrCode) {
		s.router.Record(pspID, 0, true, "callback "+m.Resp.ErrCode)
	}
	return "", s.applyPayment(ctx, tx, t, m.Resp.Result, m.Resp.ErrCode, reasonFor(m.Resp.ErrCode), m.Txn.CustRef, m.Payer, "callback")
}

var reasons = map[string]string{
	npci.CodeInsufficientFunds: "insufficient funds in payer account",
	npci.CodeInvalidPIN:        "incorrect UPI PIN",
	npci.CodeInvalidVPA:        "invalid VPA",
	npci.CodeCollectExpired:    "collect request expired",
	npci.CodeCollectDeclined:   "payer declined the request",
	npci.CodeDeemed:            "deemed approved; awaiting reconciliation",
	npci.CodeRemitterTimeout:   "remitter bank timeout",
	npci.CodeBankDown:          "bank unavailable",
	npci.CodeTechnical:         "technical decline",
}

func reasonFor(code string) string { return reasons[code] }
