package npci

import (
	"bytes"
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AppTestify/UPI-Switch/internal/security"
)

// TechnicalError marks a transport-level or bank-system failure; the router fails over on it.
type TechnicalError struct{ Err error }

func (e *TechnicalError) Error() string { return "technical: " + e.Err.Error() }
func (e *TechnicalError) Unwrap() error { return e.Err }

// IsTechnical reports whether err is a TechnicalError.
func IsTechnical(err error) bool {
	var te *TechnicalError
	return errors.As(err, &te)
}

// Peer is a PSP bank endpoint and its verification key.
type Peer struct {
	ID       string
	Endpoint string
	Key      *rsa.PublicKey
}

// Client sends signed UPI messages to PSP banks over HTTPS (mTLS configured on the http.Client).
type Client struct {
	OrgID  string
	Signer security.Signer
	HTTP   *http.Client
}

// Exchange posts the signed message to {endpoint}/upi/{API} and returns the verified response message.
// Async APIs (ReqPay, ReqMandate, ReqComplaint) get an Ack; sync APIs get their Resp* message.
func (c *Client) Exchange(ctx context.Context, peer Peer, m *Message) (*Message, error) {
	if m.Head.OrgID == "" {
		m.Head.OrgID = c.OrgID
	}
	body, err := Marshal(m, c.Signer)
	if err != nil {
		return nil, err
	}
	url := strings.TrimRight(peer.Endpoint, "/") + "/upi/" + m.API()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/xml")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, &TechnicalError{Err: err}
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &TechnicalError{Err: err}
	}
	if resp.StatusCode >= 500 {
		return nil, &TechnicalError{Err: fmt.Errorf("%s returned HTTP %d", peer.ID, resp.StatusCode)}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d: %s", peer.ID, resp.StatusCode, truncate(rb))
	}
	out, err := Parse(rb, peer.Key)
	if err != nil {
		return nil, &TechnicalError{Err: err}
	}
	if out.Resp != nil && out.Resp.ReqMsgID != m.Head.MsgID {
		return nil, &TechnicalError{Err: errors.New("response reqMsgId does not match request")}
	}
	if out.API() == APIAck && out.Resp != nil && out.Resp.Result != ResultSuccess {
		if IsTechnicalDecline(out.Resp.ErrCode) {
			return out, &TechnicalError{Err: fmt.Errorf("negative ack %s", out.Resp.ErrCode)}
		}
		return out, fmt.Errorf("negative ack %s", out.Resp.ErrCode)
	}
	return out, nil
}

func truncate(b []byte) string {
	if len(b) > 200 {
		return string(b[:200])
	}
	return string(b)
}
