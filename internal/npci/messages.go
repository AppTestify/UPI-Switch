// Package npci builds, signs, verifies and transports NPCI UPI XML messages
// (ReqPay, RespPay, ReqChkTxn, ReqValAdd, ReqHbt, ReqMandate, ReqComplaint and their responses).
//
// Element and attribute names follow the NPCI UPI specification. The signature is an enveloped
// RSA-SHA256 signature over the exact document bytes with the <Signature> element removed.
package npci

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// Namespace is the UPI schema namespace.
const Namespace = "http://npci.org/upi/schema/"

// API names.
const (
	APIReqPay        = "ReqPay"
	APIRespPay       = "RespPay"
	APIReqChkTxn     = "ReqChkTxn"
	APIRespChkTxn    = "RespChkTxn"
	APIReqValAdd     = "ReqValAdd"
	APIRespValAdd    = "RespValAdd"
	APIReqHbt        = "ReqHbt"
	APIRespHbt       = "RespHbt"
	APIReqMandate    = "ReqMandate"
	APIRespMandate   = "RespMandate"
	APIReqComplaint  = "ReqComplaint"
	APIRespComplaint = "RespComplaint"
	APIAck           = "Ack"
)

// Txn types carried in <Txn type="...">.
const (
	TxnPay        = "PAY"
	TxnCollect    = "COLLECT"
	TxnDebit      = "DEBIT"
	TxnCredit     = "CREDIT"
	TxnRefund     = "REFUND"
	TxnMandateExe = "MANDATE_EXECUTE"
	TxnChkTxn     = "ChkTxn"
	TxnValAdd     = "ValAdd"
	TxnHbt        = "Hbt"
	TxnMandate    = "MANDATE"
	TxnComplaint  = "COMPLAINT"
)

// Results in <Resp result="...">.
const (
	ResultSuccess  = "SUCCESS"
	ResultFailure  = "FAILURE"
	ResultDeemed   = "DEEMED"
	ResultPending  = "PENDING"
	ResultNotFound = "NOT_FOUND"
)

// Response codes used by the switch. "00" is approval; the technical-decline set triggers PSP failover.
const (
	CodeApproved          = "00"
	CodeInsufficientFunds = "Z9"
	CodeInvalidPIN        = "ZM"
	CodeInvalidVPA        = "ZH"
	CodeCollectExpired    = "U69"
	CodeCollectDeclined   = "ZA"
	CodeDeemed            = "RB"
	CodeRemitterTimeout   = "U09"
	CodeBankDown          = "U90"
	CodeTechnical         = "XY"
	CodeRiskDecline       = "RD"
)

// IsTechnicalDecline reports codes that indicate a bank/system issue rather than a customer decision.
func IsTechnicalDecline(code string) bool {
	switch code {
	case CodeRemitterTimeout, CodeBankDown, CodeTechnical, "U91", "96":
		return true
	}
	return false
}

// Head is the common message header.
type Head struct {
	Ver   string `xml:"ver,attr"`
	Ts    string `xml:"ts,attr"`
	OrgID string `xml:"orgId,attr"`
	MsgID string `xml:"msgId,attr"`
}

// Txn describes the transaction a message is about.
type Txn struct {
	ID             string `xml:"id,attr"`
	Note           string `xml:"note,attr,omitempty"`
	RefID          string `xml:"refId,attr,omitempty"`
	RefURL         string `xml:"refUrl,attr,omitempty"`
	Ts             string `xml:"ts,attr"`
	Type           string `xml:"type,attr"`
	CustRef        string `xml:"custRef,attr,omitempty"` // RRN
	OrgTxnID       string `xml:"orgTxnId,attr,omitempty"`
	OrgRRN         string `xml:"orgRrn,attr,omitempty"`
	InitiationMode string `xml:"initiationMode,attr,omitempty"`
	Purpose        string `xml:"purpose,attr,omitempty"`
	SubType        string `xml:"subType,attr,omitempty"`
	ExpireAfter    int    `xml:"expireAfter,attr,omitempty"` // minutes, for COLLECT
}

// Amount is a value in rupees with currency.
type Amount struct {
	Value string `xml:"value,attr"`
	Curr  string `xml:"curr,attr"`
}

// Party is a payer or payee.
type Party struct {
	Addr   string  `xml:"addr,attr"`
	Name   string  `xml:"name,attr,omitempty"`
	SeqNum string  `xml:"seqNum,attr,omitempty"`
	Type   string  `xml:"type,attr,omitempty"`
	Code   string  `xml:"code,attr,omitempty"` // MCC
	Amount *Amount `xml:"Amount,omitempty"`
}

// Payees wraps the payee list.
type Payees struct {
	Payee []Party `xml:"Payee"`
}

// Ref is a per-party result reference inside Resp.
type Ref struct {
	Type        string `xml:"type,attr"`
	Addr        string `xml:"addr,attr,omitempty"`
	RespCode    string `xml:"respCode,attr,omitempty"`
	ApprovalNum string `xml:"approvalNum,attr,omitempty"`
	SettAmount  string `xml:"settAmount,attr,omitempty"`
}

// Resp is the result block of a response message.
type Resp struct {
	ReqMsgID    string `xml:"reqMsgId,attr"`
	Result      string `xml:"result,attr"`
	ErrCode     string `xml:"errCode,attr,omitempty"`
	ApprovalNum string `xml:"approvalNum,attr,omitempty"`
	MaskName    string `xml:"maskName,attr,omitempty"`
	Code        string `xml:"code,attr,omitempty"`
	Type        string `xml:"type,attr,omitempty"`
	Ref         []Ref  `xml:"Ref,omitempty"`
}

// Mandate is the AutoPay mandate block.
type Mandate struct {
	Type          string  `xml:"type,attr"` // CREATE | REVOKE
	Name          string  `xml:"name,attr,omitempty"`
	UMN           string  `xml:"umn,attr,omitempty"`
	Recurrence    string  `xml:"recurrence,attr,omitempty"`
	ValidityStart string  `xml:"validityStart,attr,omitempty"`
	ValidityEnd   string  `xml:"validityEnd,attr,omitempty"`
	AmountRule    string  `xml:"amountRule,attr,omitempty"` // MAX | EXACT
	Amount        *Amount `xml:"Amount,omitempty"`
}

// Complaint is the UDIR complaint block.
type Complaint struct {
	Type       string `xml:"type,attr"`
	ReasonCode string `xml:"reasonCode,attr,omitempty"`
	Remarks    string `xml:"remarks,attr,omitempty"`
	AdjAmount  string `xml:"adjAmount,attr,omitempty"`
	AdjFlag    string `xml:"adjFlag,attr,omitempty"` // resolution, in responses
	CRN        string `xml:"crn,attr,omitempty"`
}

// Signature is the enveloped signature block.
type Signature struct {
	KeyID          string `xml:"SignedInfo>KeyId"`
	DigestMethod   string `xml:"SignedInfo>DigestMethod"`
	DigestValue    string `xml:"SignedInfo>DigestValue"`
	SignatureValue string `xml:"SignatureValue"`
}

// Message is any UPI message; the root element name is the API name.
type Message struct {
	XMLName   xml.Name
	Xmlns     string     `xml:"xmlns,attr,omitempty"`
	Head      Head       `xml:"Head"`
	Txn       *Txn       `xml:"Txn,omitempty"`
	Payer     *Party     `xml:"Payer,omitempty"`
	Payees    *Payees    `xml:"Payees,omitempty"`
	Mandate   *Mandate   `xml:"Mandate,omitempty"`
	Complaint *Complaint `xml:"Complaint,omitempty"`
	Resp      *Resp      `xml:"Resp,omitempty"`
	Signature *Signature `xml:"Signature,omitempty"`
}

// API returns the root element name.
func (m *Message) API() string { return m.XMLName.Local }

// Payee returns the first payee, if any.
func (m *Message) Payee() *Party {
	if m.Payees == nil || len(m.Payees.Payee) == 0 {
		return nil
	}
	return &m.Payees.Payee[0]
}

// New builds a message skeleton with a fresh msgId.
func New(api, orgID string) *Message {
	return &Message{
		XMLName: xml.Name{Local: api},
		Xmlns:   Namespace,
		Head:    Head{Ver: "2.0", Ts: Timestamp(time.Now()), OrgID: orgID, MsgID: NewMsgID(orgID)},
	}
}

// Timestamp formats a time the way UPI messages carry it.
func Timestamp(t time.Time) string { return t.Format("2006-01-02T15:04:05.000-07:00") }

// ParseTimestamp parses a UPI message timestamp.
func ParseTimestamp(s string) (time.Time, error) {
	return time.Parse("2006-01-02T15:04:05.000-07:00", s)
}

// NewMsgID returns a 35-character id prefixed with the org id.
func NewMsgID(orgID string) string { return NewTxnID(orgID) }

// NewTxnID returns a 35-character UPI transaction id: 3-char org prefix + 32 hex.
func NewTxnID(orgID string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	p := strings.ToUpper(orgID)
	if len(p) > 3 {
		p = p[:3]
	}
	for len(p) < 3 {
		p += "X"
	}
	return p + strings.ToUpper(hex.EncodeToString(b))
}

// NewRRN returns a 12-digit retrieval reference number.
func NewRRN() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	var n uint64
	for _, x := range b {
		n = n<<8 | uint64(x)
	}
	return fmt.Sprintf("%012d", n%1_000_000_000_000)
}

// Response builds a response message for req with the given result.
func Response(req *Message, api, orgID, result, errCode string) *Message {
	r := New(api, orgID)
	if req.Txn != nil {
		t := *req.Txn
		t.Ts = Timestamp(time.Now())
		r.Txn = &t
	}
	r.Resp = &Resp{ReqMsgID: req.Head.MsgID, Result: result, ErrCode: errCode}
	return r
}

// Ack builds the synchronous acknowledgement for an async request.
func Ack(req *Message, orgID string, errCode string) *Message {
	a := New(APIAck, orgID)
	res := ResultSuccess
	if errCode != "" {
		res = ResultFailure
	}
	a.Resp = &Resp{ReqMsgID: req.Head.MsgID, Result: res, ErrCode: errCode, Type: req.API()}
	return a
}
