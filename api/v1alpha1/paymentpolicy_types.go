package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// ControllerName is the Gateway API controller name written into every status entry,
// so tooling can tell which controller produced it.
const ControllerName = "x402.dev/payment-policy-controller"

// Condition reasons on the Accepted condition. The Gateway API ones are reused so a
// PaymentPolicy reads like any other policy; Conflicted is mirrored from the
// EnvoyExtensionPolicy the controller writes.
const (
	ReasonAccepted       = gatewayv1.PolicyReasonAccepted
	ReasonInvalid        = gatewayv1.PolicyReasonInvalid
	ReasonTargetNotFound = gatewayv1.PolicyReasonTargetNotFound
	ReasonConflicted     = gatewayv1.PolicyReasonConflicted
)

// PaymentPolicySpec says what one HTTPRoute costs. It states intent only: the price and
// who is paid. Everything Envoy needs (timeouts, failOpen, the filter address) is the
// controller's, so an app team cannot weaken enforcement by editing its own policy.
//
// Validation here is the schema's (pattern, enum, bounds): it is checked by the API
// server before the controller ever sees the object, so `kubectl apply` reports a
// mistake at once. The same rules run again in internal/pricing when the controller
// projects the policy into the price table, so the filter can never load a table the
// schema would have refused.
type PaymentPolicySpec struct {
	// The HTTPRoute, in this namespace, that this policy prices. Immutable: to price a
	// different route, create another policy.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="targetRef is immutable"
	TargetRef TargetRef `json:"targetRef"`

	// The Radius network to charge on. The controller resolves the chain ID, the SBC
	// contract and the facilitator from this name, so a policy never carries addresses
	// that could be mistyped. Only testnet until the Phase 6 soak gate passes.
	// +kubebuilder:validation:Enum=testnet
	Network string `json:"network"`

	// Price per request in atomic units of SBC (6 decimals): "1000" is 0.001 SBC. A
	// decimal string, so "0.001" is refused as a unit mistake, not read as a small price.
	// At most 15 digits: the filter's Redis arithmetic is exact below 2^53.
	// +kubebuilder:validation:Pattern=`^[0-9]+$`
	// +kubebuilder:validation:MaxLength=15
	Price string `json:"price"`

	// The address that receives every payment for this route.
	// +kubebuilder:validation:Pattern=`^0x[0-9a-fA-F]{40}$`
	PayTo string `json:"payTo"`

	// How the buyer authorizes the transfer. Only EIP-3009 is built (no approve
	// transaction, no gas for the buyer); Permit2 is a backlog item.
	// +kubebuilder:validation:Enum=eip3009
	// +kubebuilder:default=eip3009
	// +optional
	AssetTransferMethod string `json:"assetTransferMethod,omitempty"`

	// How long a signed payment stays valid, in seconds. Buyers sign validBefore = now +
	// this; it also bounds how long the filter remembers a nonce. 30 to 600 (radius-sdk
	// clamps at 600).
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:validation:Maximum=600
	// +kubebuilder:default=300
	// +optional
	MaxTimeoutSeconds int32 `json:"maxTimeoutSeconds,omitempty"`

	// What the filter answers when one of its own dependencies (Redis, the RPC) is down.
	// Closed refuses with a 503; Open (backlog) would serve unpaid. Never Envoy's failOpen,
	// which stays false: a hung filter is always a 504.
	// +kubebuilder:validation:Enum=Closed
	// +kubebuilder:default=Closed
	// +optional
	FailureMode string `json:"failureMode,omitempty"`

	// Async settles after the response is released; Sync (backlog) would settle before.
	// +kubebuilder:validation:Enum=Async
	// +kubebuilder:default=Async
	// +optional
	CaptureMode string `json:"captureMode,omitempty"`

	// Shown to buyers in the 402 as the resource description.
	// +kubebuilder:validation:MaxLength=256
	// +optional
	Description string `json:"description,omitempty"`

	// The media type the route returns, advertised in the 402.
	// +kubebuilder:validation:MaxLength=128
	// +optional
	MimeType string `json:"mimeType,omitempty"`

	// Overrides for the how-to-pay text in the 402.
	// +optional
	Help *Help `json:"help,omitempty"`
}

// TargetRef names the HTTPRoute a policy attaches to. Only HTTPRoutes in the policy's
// own namespace can be targeted, like Envoy Gateway's own policies: a team prices its
// routes, not another team's.
type TargetRef struct {
	// +kubebuilder:validation:Enum=gateway.networking.k8s.io
	// +kubebuilder:default=gateway.networking.k8s.io
	// +optional
	Group string `json:"group,omitempty"`

	// +kubebuilder:validation:Enum=HTTPRoute
	// +kubebuilder:default=HTTPRoute
	// +optional
	Kind string `json:"kind,omitempty"`

	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// Help overrides the how-to-pay defaults of the route's network.
type Help struct {
	// Where the 402's Link: rel="help" points. https only.
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	// +kubebuilder:validation:MaxLength=512
	// +optional
	DocsURL string `json:"docsURL,omitempty"`

	// One line added to the 402's error hint. It travels inside a header, so printable
	// ASCII only, at most 200 characters.
	// +kubebuilder:validation:Pattern=`^[\x20-\x7e]*$`
	// +kubebuilder:validation:MaxLength=200
	// +optional
	Message string `json:"message,omitempty"`
}

// PaymentPolicy prices one HTTPRoute per request. Apply it and the route answers 402
// until a buyer pays; delete it and the route is free again. The backend is never
// changed or restarted.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=pp
// +kubebuilder:printcolumn:name="Route",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.network`
// +kubebuilder:printcolumn:name="Price",type=string,JSONPath=`.spec.price`
// +kubebuilder:printcolumn:name="Accepted",type=string,JSONPath=`.status.ancestors[0].conditions[?(@.type=="Accepted")].status`
// +kubebuilder:printcolumn:name="Reason",type=string,JSONPath=`.status.ancestors[0].conditions[?(@.type=="Accepted")].reason`,priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PaymentPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec PaymentPolicySpec `json:"spec"`

	// Status is the Gateway API policy status (GEP-713): one ancestor entry per
	// target, here always the HTTPRoute, with an Accepted condition.
	// +optional
	Status gatewayv1.PolicyStatus `json:"status,omitempty"`
}

// PaymentPolicyList is a list of PaymentPolicy.
//
// +kubebuilder:object:root=true
type PaymentPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PaymentPolicy `json:"items"`
}
