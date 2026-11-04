// Package pricing is the price table: the contract between the PaymentPolicy controller,
// which writes it, and the filter, which reads it on every request.
//
// The table lives in one ConfigMap under the key "prices.json". It is keyed by route
// identity, not by path: Envoy tells the filter which HTTPRoute matched (the xds.route_name
// attribute), so the filter never re-implements Gateway API path matching, and the path
// tricks other x402 gateways had to patch (`/free/../paid`, `%0A`) cannot reach a price.
//
// The schema is versioned and complete from the start: every field the later stages read
// is defined here, so adding verification, reservation or settlement never changes it.
// A table is accepted whole or not at all. An unknown key, an unknown version or one bad
// route rejects the table, and the caller keeps serving the last good one; a mistyped
// field must never turn a priced route into a free one.
package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// SchemaVersion is the only version this filter reads.
const SchemaVersion = 1

// ConfigMapKey is the ConfigMap data key that holds the table.
const ConfigMapKey = "prices.json"

// The modes that are built. Open and Sync are in the schema already (backlog) and are
// refused until they exist, rather than silently treated as the defaults.
const (
	FailureClosed = "Closed"
	CaptureAsync  = "Async"
)

// Bounds on maxTimeoutSeconds. Buyers clamp it to 600 (radius-sdk) and `radius-cli sell
// check` warns below 30; it also bounds how long a signature, and so a reservation, lives.
const (
	MinTimeoutSeconds = 30
	MaxTimeoutSeconds = 600
)

// Table is the decoded ConfigMap value.
type Table struct {
	SchemaVersion int              `json:"schemaVersion"`
	Routes        map[string]Route `json:"routes"`
}

// Route is the price of one HTTPRoute. Keys in Table.Routes are "httproute/<ns>/<name>".
type Route struct {
	Network             string `json:"network"` // CAIP-2, e.g. "eip155:72344"
	Price               string `json:"price"`   // atomic units, decimal string: "1000" = 0.001 SBC
	Asset               string `json:"asset"`
	PayTo               string `json:"payTo"`
	AssetTransferMethod string `json:"assetTransferMethod"`
	MaxTimeoutSeconds   int    `json:"maxTimeoutSeconds"`
	FailureMode         string `json:"failureMode"`
	CaptureMode         string `json:"captureMode"`
	Description         string `json:"description,omitempty"`
	MimeType            string `json:"mimeType,omitempty"`
	Help                *Help  `json:"help,omitempty"`
}

// Help overrides the how-to-pay defaults of the route's network.
type Help struct {
	DocsURL string `json:"docsURL,omitempty"`
	Message string `json:"message,omitempty"`
}

// Entry is a validated route, with what the request path needs already built.
type Entry struct {
	Key          string
	Route        Route
	Network      radius.Profile
	Price        *big.Int
	Requirements x402.Requirements // the one `accepts` offer for this route
}

// Prices is a validated, immutable table.
type Prices struct {
	entries map[string]*Entry
}

// Lookup returns the entry for a route key. It is safe on a nil *Prices.
func (p *Prices) Lookup(key string) (*Entry, bool) {
	if p == nil {
		return nil, false
	}
	e, ok := p.entries[key]
	return e, ok
}

// Len is the number of priced routes.
func (p *Prices) Len() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// Parse decodes and validates a table. networks lists the networks this filter may
// charge on; a route on any other network rejects the table (mainnet stays out until the
// Phase 6 gate by not being listed). Every problem is reported, not just the first.
func Parse(data []byte, networks ...radius.Profile) (*Prices, error) {
	// exactKeys also rejects trailing data after the table (json.Unmarshal does).
	if err := exactKeys(data); err != nil {
		return nil, fmt.Errorf("pricing: decode: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var t Table
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("pricing: decode: %w", err)
	}
	if t.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("pricing: schemaVersion %d, this filter reads %d", t.SchemaVersion, SchemaVersion)
	}

	keys := make([]string, 0, len(t.Routes))
	for k := range t.Routes {
		keys = append(keys, k)
	}
	slices.Sort(keys)

	out := &Prices{entries: make(map[string]*Entry, len(t.Routes))}
	var errs []error
	for _, k := range keys {
		e, err := validate(k, t.Routes[k], networks)
		if err != nil {
			errs = append(errs, fmt.Errorf("route %q: %w", k, err))
			continue
		}
		out.entries[k] = e
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("pricing: %w", errors.Join(errs...))
	}
	return out, nil
}

// exactKeys refuses any key that is not spelled exactly as a field's json tag.
// encoding/json matches keys case-insensitively and DisallowUnknownFields does not see
// case, so "payto" would be read as payTo, and a second spelling could override the first.
func exactKeys(data []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return err
	}
	if err := keysOf(top, Table{}, "table"); err != nil {
		return err
	}
	var routes map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["routes"], &routes); err != nil || routes == nil {
		return nil // not an object of objects: the typed decode reports it
	}
	for k, r := range routes {
		if err := keysOf(r, Route{}, fmt.Sprintf("route %q", k)); err != nil {
			return err
		}
		var help map[string]json.RawMessage
		if json.Unmarshal(r["help"], &help) == nil && help != nil {
			if err := keysOf(help, Help{}, fmt.Sprintf("route %q help", k)); err != nil {
				return err
			}
		}
	}
	return nil
}

func keysOf(obj map[string]json.RawMessage, v any, where string) error {
	allowed := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		allowed[name] = true
	}
	for k := range obj {
		if !allowed[k] {
			return fmt.Errorf("%s: unknown field %q (keys are case-sensitive)", where, k)
		}
	}
	return nil
}

// dnsName is a Kubernetes namespace or object name, loosely (RFC 1123 subdomain).
var dnsName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

func validate(key string, r Route, networks []radius.Profile) (*Entry, error) {
	if k, ok := RouteKey(key + "/"); !ok || k != key {
		return nil, errors.New(`key must be "httproute/<namespace>/<name>"`)
	}
	i := slices.IndexFunc(networks, func(p radius.Profile) bool { return p.Network() == r.Network })
	if i < 0 {
		return nil, fmt.Errorf("network %q is not one this filter charges on", r.Network)
	}
	network := networks[i]
	if !strings.EqualFold(r.Asset, radius.SBC) {
		return nil, fmt.Errorf("asset %s: only SBC (%s) is supported", r.Asset, radius.SBC)
	}
	if r.AssetTransferMethod != x402.TransferEIP3009 {
		return nil, fmt.Errorf("assetTransferMethod %q: only %q is built", r.AssetTransferMethod, x402.TransferEIP3009)
	}
	// "price" is atomic units, so "0.001" is a unit mistake, not a small price.
	price, err := evm.ParseUint256(r.Price)
	if err != nil || price.Sign() == 0 {
		return nil, fmt.Errorf("price %q: want a positive integer of atomic units (\"1000\" = 0.001 SBC)", r.Price)
	}
	payTo, err := evm.ParseAddress(r.PayTo)
	if err != nil {
		return nil, fmt.Errorf("payTo: %w", err)
	}
	if payTo == (evm.Address{}) {
		return nil, errors.New("payTo is the zero address")
	}
	if r.MaxTimeoutSeconds < MinTimeoutSeconds || r.MaxTimeoutSeconds > MaxTimeoutSeconds {
		return nil, fmt.Errorf("maxTimeoutSeconds %d: want %d to %d", r.MaxTimeoutSeconds, MinTimeoutSeconds, MaxTimeoutSeconds)
	}
	if r.FailureMode != FailureClosed {
		return nil, fmt.Errorf("failureMode %q: only %q is built", r.FailureMode, FailureClosed)
	}
	if r.CaptureMode != CaptureAsync {
		return nil, fmt.Errorf("captureMode %q: only %q is built", r.CaptureMode, CaptureAsync)
	}
	if len(r.Description) > 256 {
		return nil, errors.New("description is longer than 256 bytes")
	}
	if r.Help != nil {
		if r.Help.DocsURL != "" {
			u, err := url.Parse(r.Help.DocsURL)
			if err != nil || u.Scheme != "https" || u.Host == "" {
				return nil, fmt.Errorf("help.docsURL %q: want an https URL", r.Help.DocsURL)
			}
		}
		// The message goes into the 402's `error` line inside a header: keep it short,
		// printable ASCII.
		if len(r.Help.Message) > 200 || strings.IndexFunc(r.Help.Message, func(c rune) bool { return c < 0x20 || c > 0x7e }) >= 0 {
			return nil, errors.New("help.message: want at most 200 printable ASCII characters")
		}
	}
	return &Entry{
		Key:          key,
		Route:        r,
		Network:      network,
		Price:        price,
		Requirements: network.ExactRequirements(price.String(), payTo.String(), r.MaxTimeoutSeconds),
	}, nil
}

// RouteKey turns Envoy's xds.route_name into a table key. Envoy Gateway names routes
// "httproute/<namespace>/<name>/rule/<i>/match/<j>" (internal/gatewayapi/helpers.go
// irRouteName, v1.9.2); prices are per HTTPRoute, so the rule and match are dropped.
// Kubernetes names cannot contain "/", so the split is unambiguous.
func RouteKey(xdsRouteName string) (string, bool) {
	parts := strings.SplitN(xdsRouteName, "/", 4)
	if len(parts) != 4 || parts[0] != "httproute" || !dnsName.MatchString(parts[1]) || !dnsName.MatchString(parts[2]) {
		return "", false
	}
	return parts[0] + "/" + parts[1] + "/" + parts[2], true
}

// Store holds the current table for the request path: one atomic load per request, no
// lock. Until the first table is stored, Load returns nil and the filter is not ready.
type Store struct {
	p      atomic.Pointer[Prices]
	once   sync.Once
	loaded chan struct{}
}

// NewStore returns an empty store.
func NewStore() *Store { return &Store{loaded: make(chan struct{})} }

// Load returns the current table, or nil before the first one.
func (s *Store) Load() *Prices { return s.p.Load() }

// Set replaces the table.
func (s *Store) Set(p *Prices) {
	s.p.Store(p)
	s.once.Do(func() { close(s.loaded) })
}

// Loaded is closed once the first table is stored.
func (s *Store) Loaded() <-chan struct{} { return s.loaded }
