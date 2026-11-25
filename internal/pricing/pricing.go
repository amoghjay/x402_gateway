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
// A broken table document (not JSON, an unknown top-level key, an unknown version) is
// rejected whole, and the caller keeps serving the last good one. A broken route is not
// dropped: it is blocked, and the filter refuses it with a 503 until it is fixed. So a
// mistyped field never turns a priced route into a free one, and one bad route cannot
// keep every other route (or a new filter pod) from loading. The controller blocks a
// route the same way when its PaymentPolicy is invalid (Table.Blocked).
package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"net/url"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/metrics"
	compiledpolicy "github.com/amoghjay/x402_gateway/internal/policy"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/webbotauth"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
)

// SchemaVersion is the only version this filter reads.
const SchemaVersion = 1

// ConfigMapKey is the ConfigMap data key that holds the table.
const ConfigMapKey = "prices.json"

// AgentsKey is the ConfigMap data key holding the Web Bot Auth key directories of the
// origins the loaded policies name: a JSON object from origin to webbotauth.Directory.
const AgentsKey = "agents.json"

// PolicyKey is the ConfigMap data key holding a Radius Platform project's compiled policy,
// beside the table: one key per project that a PaymentPolicy names, the document as served.
func PolicyKey(projectID string) string { return "policy-" + strings.ToLower(projectID) + ".json" }

// The modes that are built. Open is in the schema already (backlog) and is refused
// until it exists, rather than silently treated as the default.
const (
	FailureClosed = "Closed"
	CaptureAsync  = "Async" // release the response, then settle
	CaptureSync   = "Sync"  // settle while the response is held, then release
)

// Envoy's per-message timeout for a route, by capture mode: how long Envoy waits for
// the filter's answer to one phase before it fails the request. An Async route never
// leaves the cluster on the response phase. A Sync route calls the facilitator there, so
// the filter's settle budget (internal/extproc) has to fit under SyncMessageTimeout.
const (
	AsyncMessageTimeout = 300 * time.Millisecond
	SyncMessageTimeout  = 10 * time.Second
)

// MaxPrice is 2^53 - 1 atomic units (about 9e9 SBC): the filter's in-flight sums are
// compared in Redis Lua, whose numbers are doubles, exact only up to there. The CRD
// caps price at 15 digits for the same reason.
var MaxPrice = new(big.Int).SetUint64(1<<53 - 1)

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
	// Blocked lists routes whose PaymentPolicy exists but is invalid, with the reason.
	// The filter answers 503 on them rather than serving them free.
	Blocked map[string]string `json:"blocked,omitempty"`
	// Platform lists the routes a Radius Platform project's compiled policy governs. The
	// documents are in the same ConfigMap, under PolicyKey(project). Omitted when empty, so
	// a table without platform routes still loads in a filter from before them.
	Platform map[string]PlatformRoute `json:"platform,omitempty"`
}

// PlatformRoute names the project whose compiled policy decides a route's requests.
type PlatformRoute struct {
	Project string `json:"project"` // the project ID, lower case
	// NoPolicy is what the route answers while no document for the project was ever
	// loaded: "deny" (503 policy_unavailable) or "allow".
	NoPolicy string `json:"noPolicy"`
}

// Route is the price of one HTTPRoute. Keys in Table.Routes are "httproute/<ns>/<name>".
type Route struct {
	Network             string `json:"network"` // CAIP-2, e.g. "eip155:72344"
	Price               string `json:"price"`   // atomic units of the network's token, decimal string: "1000" = 0.001 at 6 decimals
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
	Domain       eip3009.Domain    // the token's EIP-712 domain, which signatures are recovered against
}

// Prices is a validated, immutable table.
type Prices struct {
	entries  map[string]*Entry
	blocked  map[string]string                 // route key → why it is refused
	platform map[string]PlatformRoute          // route key → its project
	policies map[string]*compiledpolicy.Policy // project → its loaded compiled policy (Watcher)
	origins  map[string]map[string]bool        // project → the signature_agent origins its policy names
	agents   map[string]*webbotauth.Directory  // origin → its Web Bot Auth keys (Watcher)
	networks []radius.Profile                  // the networks this filter charges on, as passed to Parse
}

// Network returns the profile of a network this filter charges on, by CAIP-2 name. It is
// safe on a nil *Prices.
func (p *Prices) Network(caip2 string) (radius.Profile, bool) {
	if p == nil {
		return radius.Profile{}, false
	}
	for _, n := range p.networks {
		if n.Network() == caip2 {
			return n, true
		}
	}
	return radius.Profile{}, false
}

// Platform returns the project that governs a route, if one does. It is safe on a nil
// *Prices.
func (p *Prices) Platform(key string) (PlatformRoute, bool) {
	if p == nil {
		return PlatformRoute{}, false
	}
	r, ok := p.platform[key]
	return r, ok
}

// Policy returns a project's loaded compiled policy, or nil when none was ever loaded. It is
// safe on a nil *Prices.
func (p *Prices) Policy(project string) *compiledpolicy.Policy {
	if p == nil {
		return nil
	}
	return p.policies[strings.ToLower(project)]
}

// Directories returns what a project's signatures may verify against: the keys of the
// origins its policy names, and no others. Nil when the policy names none, so the caller
// skips verification. It is safe on a nil *Prices.
func (p *Prices) Directories(project string) webbotauth.Directories {
	if p == nil {
		return nil
	}
	named := p.origins[strings.ToLower(project)]
	if len(named) == 0 {
		return nil
	}
	return func(origin string) (*webbotauth.Directory, bool) {
		if !named[origin] {
			return nil, false
		}
		d, ok := p.agents[origin]
		return d, ok
	}
}

// PlatformLen is the number of routes a platform project governs.
func (p *Prices) PlatformLen() int {
	if p == nil {
		return 0
	}
	return len(p.platform)
}

// Sync reports whether the route settles before its response is released.
func (e *Entry) Sync() bool { return e.Route.CaptureMode == CaptureSync }

// Lookup returns the entry for a route key. It is safe on a nil *Prices.
func (p *Prices) Lookup(key string) (*Entry, bool) {
	if p == nil {
		return nil, false
	}
	e, ok := p.entries[key]
	return e, ok
}

// Blocked reports whether a route is refused (a broken entry, or a policy the
// controller found invalid), and why. It is safe on a nil *Prices.
func (p *Prices) Blocked(key string) (string, bool) {
	if p == nil {
		return "", false
	}
	why, ok := p.blocked[key]
	return why, ok
}

// BlockedKeys lists the refused routes, sorted.
func (p *Prices) BlockedKeys() []string {
	if p == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(p.blocked))
}

// Keys lists the priced routes' keys, sorted.
func (p *Prices) Keys() []string {
	if p == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(p.entries))
}

// PlatformKeys lists the routes a Radius Platform project governs, sorted.
func (p *Prices) PlatformKeys() []string {
	if p == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(p.platform))
}

// Len is the number of priced routes.
func (p *Prices) Len() int {
	if p == nil {
		return 0
	}
	return len(p.entries)
}

// Parse decodes and validates a table. networks lists the networks this filter may
// charge on; a route on any other network is blocked (mainnet stays out until the Phase
// 6 gate by not being listed). Every problem is reported, not just the first.
//
// When the document itself is unusable the table is nil. When only routes are broken,
// the table is returned with those routes blocked, together with an error naming each
// one: a caller that loads tables (the filter) serves it and logs the error; a caller
// that checks tables (the controller) treats any error as a refusal.
func Parse(data []byte, networks ...radius.Profile) (*Prices, error) {
	return ParseKeys(data, KeysHTTPRoute, networks...)
}

// ParseKeys is Parse for a table keyed in the given style: the controller's tables are
// KeysHTTPRoute, a standalone filter's are KeysName.
func ParseKeys(data []byte, keys KeyStyle, networks ...radius.Profile) (*Prices, error) {
	// Unmarshal also rejects trailing data after the table.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("pricing: decode: %w", err)
	}
	if err := keysOf(top, Table{}, "table"); err != nil {
		return nil, fmt.Errorf("pricing: decode: %w", err)
	}
	var version int
	if err := strictDecode(top["schemaVersion"], &version); err != nil && top["schemaVersion"] != nil {
		return nil, fmt.Errorf("pricing: decode: schemaVersion: %w", err)
	}
	if version != SchemaVersion {
		return nil, fmt.Errorf("pricing: schemaVersion %d, this filter reads %d", version, SchemaVersion)
	}
	var routes map[string]json.RawMessage
	if raw, ok := top["routes"]; ok {
		if err := strictDecode(raw, &routes); err != nil {
			return nil, fmt.Errorf("pricing: decode: routes: %w", err)
		}
	}
	var blocked map[string]string
	if raw, ok := top["blocked"]; ok {
		if err := strictDecode(raw, &blocked); err != nil {
			return nil, fmt.Errorf("pricing: decode: blocked: %w", err)
		}
	}
	var platform map[string]json.RawMessage
	if raw, ok := top["platform"]; ok {
		if err := strictDecode(raw, &platform); err != nil {
			return nil, fmt.Errorf("pricing: decode: platform: %w", err)
		}
	}

	out := &Prices{entries: make(map[string]*Entry, len(routes)), blocked: map[string]string{}, platform: map[string]PlatformRoute{}, networks: networks}
	for k, why := range blocked {
		if err := keys.check(k); err != nil {
			return nil, fmt.Errorf("pricing: blocked route %q: %w", k, err)
		}
		out.blocked[k] = "policy invalid: " + why
	}
	var errs []error
	for _, k := range slices.Sorted(maps.Keys(platform)) {
		if _, ok := out.blocked[k]; ok {
			continue // blocked wins
		}
		pr, err := parsePlatformRoute(k, platform[k], keys)
		if err == nil {
			if _, priced := routes[k]; priced {
				err = errors.New("listed with a price and a platform project")
			}
		}
		if err != nil {
			err = fmt.Errorf("route %q: %w", k, err)
			out.blocked[k] = err.Error()
			errs = append(errs, err)
			continue
		}
		out.platform[k] = pr
	}
	for _, k := range slices.Sorted(maps.Keys(routes)) {
		if _, ok := out.blocked[k]; ok {
			continue // listed as blocked as well as priced: blocked wins
		}
		if _, ok := platform[k]; ok {
			continue // blocked above, for being both
		}
		e, err := parseRoute(k, routes[k], networks, keys)
		if err != nil {
			err = fmt.Errorf("route %q: %w", k, err)
			out.blocked[k] = err.Error()
			errs = append(errs, err)
			continue
		}
		out.entries[k] = e
	}
	if len(errs) > 0 {
		return out, fmt.Errorf("pricing: %w", errors.Join(errs...))
	}
	return out, nil
}

// parsePlatformRoute decodes and checks one platform entry.
func parsePlatformRoute(key string, raw json.RawMessage, keys KeyStyle) (PlatformRoute, error) {
	if err := keys.check(key); err != nil {
		return PlatformRoute{}, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return PlatformRoute{}, fmt.Errorf("decode: %w", err)
	}
	if err := keysOf(obj, PlatformRoute{}, "platform route"); err != nil {
		return PlatformRoute{}, fmt.Errorf("decode: %w", err)
	}
	var r PlatformRoute
	if err := strictDecode(raw, &r); err != nil {
		return PlatformRoute{}, fmt.Errorf("decode: %w", err)
	}
	if !projectID.MatchString(r.Project) {
		return PlatformRoute{}, fmt.Errorf("project %q is not a lower-case UUID", r.Project)
	}
	if r.NoPolicy != compiledpolicy.Allow && r.NoPolicy != compiledpolicy.Deny {
		return PlatformRoute{}, fmt.Errorf("noPolicy %q is not allow or deny", r.NoPolicy)
	}
	return r, nil
}

var projectID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// ValidateRoute is Parse's check of one route: what the controller runs on a policy
// before it puts the route in the table.
func ValidateRoute(key string, r Route, networks ...radius.Profile) error {
	_, err := validate(key, r, networks, KeysHTTPRoute)
	return err
}

// parseRoute decodes one route strictly (exact key spelling, no unknown fields) and
// validates it.
func parseRoute(key string, raw json.RawMessage, networks []radius.Profile, keys KeyStyle) (*Entry, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if err := keysOf(obj, Route{}, "route"); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var help map[string]json.RawMessage
	if json.Unmarshal(obj["help"], &help) == nil && help != nil {
		if err := keysOf(help, Help{}, "help"); err != nil {
			return nil, fmt.Errorf("decode: %w", err)
		}
	}
	var r Route
	if err := strictDecode(raw, &r); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return validate(key, r, networks, keys)
}

// strictDecode is json.Unmarshal that refuses unknown fields.
func strictDecode(raw json.RawMessage, v any) error {
	if raw == nil {
		return errors.New("missing")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// keysOf refuses any key that is not spelled exactly as a field's json tag.
// encoding/json matches keys case-insensitively and DisallowUnknownFields does not see
// case, so "payto" would be read as payTo, and a second spelling could override the first.
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

func validate(key string, r Route, networks []radius.Profile, keys KeyStyle) (*Entry, error) {
	if err := keys.check(key); err != nil {
		return nil, err
	}
	i := slices.IndexFunc(networks, func(p radius.Profile) bool { return p.Network() == r.Network })
	if i < 0 {
		return nil, fmt.Errorf("network %q is not one this filter charges on", r.Network)
	}
	network := networks[i]
	if !strings.EqualFold(r.Asset, network.Token.Address) {
		return nil, fmt.Errorf("asset %s: only %s (%s) is supported on %s", r.Asset, network.Token.Symbol, network.Token.Address, network.Name)
	}
	if r.AssetTransferMethod != x402.TransferEIP3009 {
		return nil, fmt.Errorf("assetTransferMethod %q: only %q is built", r.AssetTransferMethod, x402.TransferEIP3009)
	}
	// "price" is atomic units, so "0.001" is a unit mistake, not a small price.
	price, err := evm.ParseUint256(r.Price)
	if err != nil || price.Sign() == 0 {
		return nil, fmt.Errorf("price %q: want a positive integer of atomic units (\"1000\" = 0.001 %s)", r.Price, network.Token.Symbol)
	}
	if price.Cmp(MaxPrice) > 0 {
		return nil, fmt.Errorf("price %q: above %s (2^53 - 1), the limit of exact arithmetic in the filter's Redis scripts", r.Price, MaxPrice)
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
	if r.CaptureMode != CaptureAsync && r.CaptureMode != CaptureSync {
		return nil, fmt.Errorf("captureMode %q: want %q or %q", r.CaptureMode, CaptureAsync, CaptureSync)
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
		Domain:       eip3009.Domain{Name: network.Token.Name, Version: network.Token.Version, ChainID: network.ChainID, VerifyingContract: network.TokenAddress()},
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

// Set replaces the table, and records it in the price-table metrics.
func (s *Store) Set(p *Prices) {
	s.p.Store(p)
	metrics.PriceTable(p.Len(), p.PlatformLen(), len(p.BlockedKeys()), time.Now())
	s.once.Do(func() { close(s.loaded) })
}

// Loaded is closed once the first table is stored.
func (s *Store) Loaded() <-chan struct{} { return s.loaded }
