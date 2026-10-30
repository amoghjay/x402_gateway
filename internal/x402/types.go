// Package x402 holds the x402 V2 wire types and the base64-JSON header codecs.
//
// It speaks V2 only. The shapes follow the specification (specs/x402-specification-v2.md
// and transports-v2/http.md in coinbase/x402) and the `exact` EVM scheme. Type names
// follow the spec's object names (PaymentRequired, PaymentPayload, SettleResponse);
// where Anteroom (radiustechsystems/anteroom, internal/payment) uses the same name for
// the same object, such as Requirements and Resource, we keep it so the two read as one
// family.
//
// Nothing here talks to the network or checks a signature. Verification lives in
// internal/x402/verify, Radius chain facts in internal/radius.
package x402

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Version is the only protocol version this package speaks.
const Version = 2

// Header names. All three carry base64-encoded JSON (HTTP transport, V2). Envoy
// lower-cases header names, so compare case-insensitively at the filter.
const (
	HeaderPaymentRequired  = "PAYMENT-REQUIRED"  // server → client, PaymentRequired
	HeaderPaymentSignature = "PAYMENT-SIGNATURE" // client → server, PaymentPayload
	HeaderPaymentResponse  = "PAYMENT-RESPONSE"  // server → client, SettleResponse
)

// SchemeExact is the one scheme on the core path.
const SchemeExact = "exact"

// Asset transfer methods of the `exact` EVM scheme, carried in Requirements.Extra.
const (
	TransferEIP3009 = "eip3009" // the spec's default and the core path
	TransferPermit2 = "permit2" // backlog
)

// Requirements is one way to pay, as advertised in PaymentRequired.Accepts and echoed
// by the client in PaymentPayload.Accepted.
type Requirements struct {
	Scheme            string      `json:"scheme"`
	Network           string      `json:"network"` // CAIP-2, e.g. "eip155:72344"
	Amount            string      `json:"amount"`  // atomic units, decimal string
	Asset             string      `json:"asset"`   // token contract address
	PayTo             string      `json:"payTo"`
	MaxTimeoutSeconds int         `json:"maxTimeoutSeconds"`
	Extra             *ExactExtra `json:"extra,omitempty"`
}

// ExactExtra is the `extra` object of the `exact` EVM scheme: the token's EIP-712 domain
// (required for EIP-3009 signing) and the transfer method. Both Radius facilitators
// advertise exactly these three fields.
type ExactExtra struct {
	AssetTransferMethod string `json:"assetTransferMethod,omitempty"`
	Name                string `json:"name,omitempty"`
	Version             string `json:"version,omitempty"`
}

// TransferMethod returns the asset transfer method, defaulting to EIP-3009 as the
// scheme spec says when `extra.assetTransferMethod` is absent.
func (r Requirements) TransferMethod() string {
	if r.Extra == nil || r.Extra.AssetTransferMethod == "" {
		return TransferEIP3009
	}
	return r.Extra.AssetTransferMethod
}

// Resource describes what is being sold. Only URL is required by the spec.
type Resource struct {
	URL         string   `json:"url"`
	Description string   `json:"description,omitempty"`
	MimeType    string   `json:"mimeType,omitempty"`
	ServiceName string   `json:"serviceName,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	IconURL     string   `json:"iconUrl,omitempty"`
}

// PaymentRequired is the object behind the PAYMENT-REQUIRED header on a 402.
type PaymentRequired struct {
	X402Version int            `json:"x402Version"`
	Error       string         `json:"error,omitempty"`
	Resource    Resource       `json:"resource"`
	Accepts     []Requirements `json:"accepts"`
	Extensions  map[string]any `json:"extensions,omitempty"`
}

// PaymentPayload is the object behind the PAYMENT-SIGNATURE header. Payload is
// scheme-specific and kept raw; EIP3009 decodes it for the core path.
type PaymentPayload struct {
	X402Version int             `json:"x402Version"`
	Resource    *Resource       `json:"resource,omitempty"`
	Accepted    Requirements    `json:"accepted"`
	Payload     json.RawMessage `json:"payload"`
	Extensions  map[string]any  `json:"extensions,omitempty"`
}

// EIP3009Payload is PaymentPayload.Payload for `exact` with the eip3009 transfer method:
// the signature over transferWithAuthorization and the parameters that rebuild it.
type EIP3009Payload struct {
	Signature     string        `json:"signature"`
	Authorization Authorization `json:"authorization"`
}

// Authorization mirrors EIP-3009's TransferWithAuthorization struct. Integers are
// decimal strings on the wire; Nonce is 0x-prefixed hex of 32 bytes.
type Authorization struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	ValidAfter  string `json:"validAfter"`
	ValidBefore string `json:"validBefore"`
	Nonce       string `json:"nonce"`
}

// ErrPayload reports a scheme payload that is not the shape the transfer method needs.
var ErrPayload = errors.New("x402: scheme payload")

// EIP3009 decodes the scheme payload as an EIP-3009 authorization. Every field must be
// present and non-empty; whether the values are well-formed is the verifier's job.
func (p PaymentPayload) EIP3009() (EIP3009Payload, error) {
	var out EIP3009Payload
	if err := json.Unmarshal(p.Payload, &out); err != nil {
		return EIP3009Payload{}, fmt.Errorf("%w: %w", ErrPayload, err)
	}
	a := out.Authorization
	for name, v := range map[string]string{
		"signature": out.Signature, "authorization.from": a.From, "authorization.to": a.To,
		"authorization.value": a.Value, "authorization.validAfter": a.ValidAfter,
		"authorization.validBefore": a.ValidBefore, "authorization.nonce": a.Nonce,
	} {
		if v == "" {
			return EIP3009Payload{}, fmt.Errorf("%w: missing %s", ErrPayload, name)
		}
	}
	return out, nil
}

// VerifyResponse is the facilitator's answer to POST /verify.
type VerifyResponse struct {
	IsValid       bool           `json:"isValid"`
	InvalidReason string         `json:"invalidReason,omitempty"`
	Payer         string         `json:"payer,omitempty"`
	Extensions    map[string]any `json:"extensions,omitempty"`
	Extra         map[string]any `json:"extra,omitempty"`
}

// SettleResponse is the facilitator's answer to POST /settle and the object behind the
// PAYMENT-RESPONSE header. Transaction is "" when nothing was broadcast, and must be set
// when ErrorReason is ReasonSettlementPending.
type SettleResponse struct {
	Success     bool           `json:"success"`
	ErrorReason string         `json:"errorReason,omitempty"`
	Payer       string         `json:"payer,omitempty"`
	Transaction string         `json:"transaction"`
	Network     string         `json:"network"`
	Amount      string         `json:"amount,omitempty"`
	Extensions  map[string]any `json:"extensions,omitempty"`
}

// UnmarshalJSON accepts the transaction hash under the aliases other facilitators use
// (txHash, transactionHash, hash) as well as the spec's transaction. The Radius
// facilitators use the spec's name; the aliases cost nothing and the settler must not
// lose a hash to a field name.
func (s *SettleResponse) UnmarshalJSON(b []byte) error {
	type plain SettleResponse
	var aux struct {
		plain
		TxHash          string `json:"txHash"`
		TransactionHash string `json:"transactionHash"`
		Hash            string `json:"hash"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*s = SettleResponse(aux.plain)
	if s.Transaction == "" {
		for _, alt := range []string{aux.TxHash, aux.TransactionHash, aux.Hash} {
			if alt != "" {
				s.Transaction = alt
				break
			}
		}
	}
	return nil
}

// FacilitatorRequest is the body of POST /verify and POST /settle. PaymentRequirements
// are always the server's own, never the client's echo (legacy REPORT §10.1).
type FacilitatorRequest struct {
	X402Version         int            `json:"x402Version"`
	PaymentPayload      PaymentPayload `json:"paymentPayload"`
	PaymentRequirements Requirements   `json:"paymentRequirements"`
}

// Supported is the answer to GET /supported.
type Supported struct {
	Kinds      []Kind              `json:"kinds"`
	Extensions []string            `json:"extensions"`
	Signers    map[string][]string `json:"signers"`
}

// Kind is one (scheme, network) a facilitator settles. Radius facilitators also name the
// asset and the transfer method in each kind.
type Kind struct {
	X402Version int         `json:"x402Version"`
	Scheme      string      `json:"scheme"`
	Network     string      `json:"network"`
	Asset       string      `json:"asset,omitempty"`
	Extra       *ExactExtra `json:"extra,omitempty"`
}

// Reason codes from the spec's §9, for VerifyResponse.InvalidReason and
// SettleResponse.ErrorReason. The filter emits the same vocabulary so clients see one
// set of reasons whether the facilitator or the data plane refused them.
const (
	ReasonInsufficientFunds   = "insufficient_funds"
	ReasonValidAfter          = "invalid_exact_evm_payload_authorization_valid_after"
	ReasonValidBefore         = "invalid_exact_evm_payload_authorization_valid_before"
	ReasonValueMismatch       = "invalid_exact_evm_payload_authorization_value_mismatch"
	ReasonSignature           = "invalid_exact_evm_payload_signature"
	ReasonRecipientMismatch   = "invalid_exact_evm_payload_recipient_mismatch"
	ReasonInvalidNetwork      = "invalid_network"
	ReasonInvalidPayload      = "invalid_payload"
	ReasonInvalidRequirements = "invalid_payment_requirements"
	ReasonInvalidScheme       = "invalid_scheme"
	ReasonUnsupportedScheme   = "unsupported_scheme"
	ReasonInvalidVersion      = "invalid_x402_version"
	ReasonTransactionState    = "invalid_transaction_state"
	ReasonUnexpectedVerify    = "unexpected_verify_error"
	ReasonUnexpectedSettle    = "unexpected_settle_error"
	ReasonSettlementPending   = "settlement_pending"
)

// SettlementExtension keys our async-settlement status inside SettleResponse.Extensions.
// The response keeps the spec's SettleResponse shape so spec clients accept it; the
// deviation (settling after the response) is the thesis and is documented as such.
const SettlementExtension = "x402.dev/settlement"

// PendingSettlement is the value under SettlementExtension while the settler has not yet
// submitted the payment. PaymentID is the key a receipts endpoint can be asked about.
type PendingSettlement struct {
	Status    string `json:"status"` // "pending"
	PaymentID string `json:"paymentId"`
}

// Pending builds the PAYMENT-RESPONSE for a request the filter released before
// settlement: success with an empty transaction and the pending status in Extensions.
func Pending(network, payer, paymentID string) SettleResponse {
	return SettleResponse{
		Success:     true,
		Transaction: "",
		Network:     network,
		Payer:       payer,
		Extensions:  map[string]any{SettlementExtension: PendingSettlement{Status: "pending", PaymentID: paymentID}},
	}
}
