// Package paywall renders the 402 for a priced route: what a caller without a payment
// gets back, and everything they need to pay.
//
// The PAYMENT-REQUIRED header is the protocol: x402 V2 clients (radius-cli, radius-sdk,
// @x402/fetch) read it and ignore the body whenever it is present. Everything else is for
// the people and agents who don't speak x402 yet, through channels that cannot break the
// clients that do. Who reads what decides the shape:
//
//   - `error`, inside the header: x402 clients print it (radius-cli as "reason: ...").
//     One line: the price and the one command that pays it.
//   - the body, which is what an agent calling with curl or a fetch tool reads first:
//     JSON by default, the same PaymentRequired plus extensions["how-to-pay"] with ordered
//     steps an agent can follow and every payment option; markdown when the caller asks
//     for text/markdown, because that is what language-model agents read best (Anteroom
//     answers in markdown too); an HTML page when it asks for text/html, which browsers
//     do. how-to-pay stays out of the header on purpose: clients echo every header
//     extension back in PAYMENT-SIGNATURE, so it would ride along on each paid request.
//   - `Link: <docs>; rel="help"` (RFC 8288).
//
// The body's format is chosen by the Accept header's q-values and nothing else: only an
// explicit text/markdown, text/html or application/json counts, never a wildcard, so
// curl, radius-cli and HTTP libraries, which send */* or nothing, get JSON. The
// User-Agent plays no part: a header any client can set is no reason to answer it
// differently.
//
// All of them are built from one list of offers and one list of steps, so they cannot
// drift apart. When radius-cli can pay any offer, the pay step is one radius-cli command
// covering all of those offers. Every 402 carries the header, `Cache-Control: no-store`,
// `Vary: Accept`, and exposes the header to browser JavaScript.
package paywall

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"math/big"
	"net/http"
	"strings"
	texttemplate "text/template"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// HowToPayExtension is the key of our extension in the 402's JSON body.
const HowToPayExtension = "how-to-pay"

// maxURLInHint caps how much of the request URL is repeated in the `error` line. The full
// URL is always in resource.url.
const maxURLInHint = 512

// maxTokenDecimals bounds the decimals an offer may claim for a token this build has no
// profile for. No ERC-20 in use comes near it.
const maxTokenDecimals = 36

// Request is what the paywall needs from the request.
type Request struct {
	Method string // GET, POST, ...
	URL    string // the public URL the caller asked for
	Accept string
	// Refused is set when a payment was sent and turned down: the reason code (spec
	// §9 where one exists) leads the `error` line, so a client that prints the 402's
	// error says why, and the how-to-pay hint still follows for the retry.
	Refused string
	// Offers, when set, is every way to pay, offered in this order in place of the
	// entry's one requirement (a Radius Platform rule with several payment options).
	// Unset, the entry's requirement is the one offer.
	Offers []Offer
}

// Offer is one way to pay: the requirements, which go into the PAYMENT-REQUIRED header as
// they are, and what the text needs to describe them. Build one with NewOffer.
type Offer struct {
	Requirements x402.Requirements
	Profile      *radius.Profile // the network's; nil when this build has no profile for it
	Price        string          // display units, e.g. "0.001"
	Symbol       string          // "SBC", "USDC"; "tokens" for a token with no profile
	KnownToken   bool            // the asset is Profile's token
	RadiusCLI    bool            // radius-cli pays it
	amount       *big.Rat        // display units, to find the dearest offer; nil if unreadable
}

// NewOffer describes one payment option. decimals is the option's own word for its token,
// used only for a token this build has no profile for. It is bounded: a policy's decimals
// are not validated, and the price is formatted with them.
func NewOffer(req x402.Requirements, decimals int) Offer {
	p, err := radius.ByNetwork(req.Network)
	if err != nil {
		return newOffer(req, nil, decimals)
	}
	return newOffer(req, &p, decimals)
}

func newOffer(req x402.Requirements, p *radius.Profile, decimals int) Offer {
	o := Offer{Requirements: req, Profile: p}
	tok := radius.Token{Symbol: "tokens", Address: req.Asset, Decimals: min(max(decimals, 0), maxTokenDecimals)}
	if p != nil && strings.EqualFold(req.Asset, p.Token.Address) {
		tok, o.KnownToken = p.Token, true
	}
	o.Symbol = tok.Symbol
	atomic, err := evm.ParseUint256(req.Amount)
	if err != nil {
		o.Price = req.Amount + " atomic units"
		return o
	}
	o.Price = tok.Format(atomic)
	o.amount = new(big.Rat).SetFrac(atomic, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(tok.Decimals)), nil))
	// radius-cli pays `exact` in a network's own token, by EIP-3009 or Permit2, and refuses
	// anything else before signing.
	method := req.TransferMethod()
	o.RadiusCLI = o.KnownToken && p.RadiusCLI && p.CLINetwork != "" && req.Scheme == x402.SchemeExact &&
		(method == x402.TransferEIP3009 || method == x402.TransferPermit2)
	return o
}

// network is the offer's network for people: "Radius testnet", or the CAIP-2 name.
func (o Offer) network() string {
	if o.Profile != nil {
		return o.Profile.Display
	}
	return o.Requirements.Network
}

// side is radius-cli's --network for the offer: one flag picks testnet or mainnet for
// every chain the command pays on.
func (o Offer) side() string {
	if o.Profile.Testnet {
		return "testnet"
	}
	return "mainnet"
}

// Response is the 402 to send.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// HowToPay is the `info` of the how-to-pay extension. The top-level price, asset, network
// and payTo describe the first option; Options lists them all.
type HowToPay struct {
	Summary     string   `json:"summary"` // "GET <url> costs 0.001 SBC on Radius testnet."
	Price       string   `json:"price"`   // display units, e.g. "0.001"
	Asset       string   `json:"asset"`   // "SBC"
	Network     string   `json:"network"` // "Radius testnet"
	PayTo       string   `json:"payTo"`
	Steps       []Step   `json:"steps"`
	Options     []Option `json:"options"`
	SDK         SDK      `json:"sdk"`
	Docs        string   `json:"docs"`
	Description string   `json:"description,omitempty"` // the route's, from the price table
	Message     string   `json:"message,omitempty"`     // the route's help message
}

// Step is one thing to do to pay, in order. Run is a shell command; an agent can run it
// as is. The `pay` step is the one that fetches the resource.
type Step struct {
	Do   string `json:"do"` // install, create-wallet, fund, pay
	Run  string `json:"run"`
	Note string `json:"note,omitempty"`
}

// Option is one way to pay, as the body describes it, in the order of the header's
// accepts.
type Option struct {
	Price        string `json:"price"`        // display units
	Asset        string `json:"asset"`        // symbol: "SBC", "USDC", or "tokens" when unknown
	AssetAddress string `json:"assetAddress"` // the token contract
	Network      string `json:"network"`      // "Radius testnet"
	NetworkID    string `json:"networkId"`    // CAIP-2: "eip155:72344"
	PayTo        string `json:"payTo"`
	Method       string `json:"method"` // "eip3009" or "permit2", as extra.assetTransferMethod
	// PayWith is "radius-cli" when the pay step's command pays this option, else the
	// easiest way to pay it.
	PayWith    string `json:"payWith"`
	knownToken bool
	tokenName  string // for a token with no profile: the EIP-712 name the option gives, if any
	chain      string // "Radius", "Base"; the CAIP-2 name for a network with no profile
	testnet    bool
}

// Chain is the option's chain for the page, without its side: the page says "test
// network" once, as a badge, instead of on every line.
func (o Option) Chain() string { return o.chain }

// chainNames names well-known chains this build has no profile for, so the page says
// "Base" rather than "eip155:8453". A name only: it says nothing about paying there.
var chainNames = map[string]string{
	"eip155:1": "Ethereum", "eip155:10": "OP Mainnet", "eip155:137": "Polygon",
	"eip155:8453": "Base", "eip155:42161": "Arbitrum One",
}

func chainName(caip2 string) string {
	if n, ok := chainNames[caip2]; ok {
		return n
	}
	return caip2
}

// KnownToken reports whether this build has a profile for the option's token.
func (o Option) KnownToken() bool { return o.knownToken }

// Testnet reports whether the option is on a test network.
func (o Option) Testnet() bool { return o.testnet }

// Token is the option's token for the page: its symbol, or the seller's name for a token
// this build has no profile for.
func (o Option) Token() string {
	switch {
	case o.knownToken:
		return o.Asset
	case o.tokenName != "":
		return o.tokenName
	}
	return "tokens"
}

// ShortPayTo is the recipient shortened for the page: 0x2a52…eBe4. The full address is
// beside it.
func (o Option) ShortPayTo() string {
	if len(o.PayTo) <= 14 {
		return o.PayTo
	}
	return o.PayTo[:6] + "…" + o.PayTo[len(o.PayTo)-4:]
}

// PriceLabel is the price with its token, for the tables: the contract stands in for a
// symbol this build does not know.
func (o Option) PriceLabel() string {
	if o.knownToken {
		return o.Price + " " + o.Asset
	}
	if o.tokenName != "" {
		return o.Price + " " + o.tokenName + " (token " + o.AssetAddress + ")"
	}
	return o.Price + " (token " + o.AssetAddress + ")"
}

// tokenName is an unknown token's EIP-712 name as the seller's option gives it ("Stable
// Coin"), for people: one line, at most 32 characters. It is a label, not a symbol, so the
// contract address stays beside it.
func tokenName(name string) string {
	name = strings.Join(strings.Fields(name), " ")
	if r := []rune(name); len(r) > 32 {
		name = string(r[:32])
	}
	return name
}

// MethodName is the transfer method for people.
func (o Option) MethodName() string { return methodName(o.Method) }

func methodName(m string) string {
	switch m {
	case x402.TransferEIP3009:
		return "EIP-3009"
	case x402.TransferPermit2:
		return "Permit2"
	}
	return m
}

// SDK is how to pay from application code.
type SDK struct {
	Package string `json:"package"`
	Import  string `json:"import"`
	Entry   string `json:"entry"`
	Note    string `json:"note"`
}

// howToPaySchema is the JSON Schema of HowToPay; the spec asks every extension for one.
var howToPaySchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",` +
	`"required":["summary","price","asset","network","payTo","steps","options","sdk","docs"],"properties":{` +
	`"summary":{"type":"string"},"price":{"type":"string"},"asset":{"type":"string"},"network":{"type":"string"},"payTo":{"type":"string"},` +
	`"steps":{"type":"array","items":{"type":"object","required":["do","run"],"properties":{"do":{"type":"string"},"run":{"type":"string"},"note":{"type":"string"}}}},` +
	`"options":{"type":"array","items":{"type":"object","required":["price","asset","assetAddress","network","networkId","payTo","method","payWith"],"properties":{` +
	`"price":{"type":"string"},"asset":{"type":"string"},"assetAddress":{"type":"string"},"network":{"type":"string"},"networkId":{"type":"string"},` +
	`"payTo":{"type":"string"},"method":{"type":"string"},"payWith":{"type":"string"}}}},` +
	`"sdk":{"type":"object","properties":{"package":{"type":"string"},"import":{"type":"string"},"entry":{"type":"string"},"note":{"type":"string"}}},` +
	`"docs":{"type":"string","format":"uri"},"description":{"type":"string"},"message":{"type":"string"}}}`)

// Render builds the 402 for a priced route.
func Render(e *pricing.Entry, req Request) (Response, error) {
	offers := req.Offers
	if len(offers) == 0 {
		offers = []Offer{newOffer(e.Requirements, &e.Network, e.Network.Token.Decimals)}
	}
	help, commands := howToPay(e, req, offers)
	pr := x402.PaymentRequired{
		X402Version: x402.Version,
		Error:       hint(help, commands, req, len(offers)),
		Resource:    x402.Resource{URL: req.URL, Description: e.Route.Description, MimeType: e.Route.MimeType},
		Accepts:     make([]x402.Requirements, len(offers)),
	}
	for i, o := range offers {
		pr.Accepts[i] = o.Requirements
	}
	header, err := pr.Encode()
	if err != nil {
		return Response{}, err
	}
	h := http.Header{}
	h.Set(x402.HeaderPaymentRequired, header)
	h.Set("Cache-Control", "no-store")
	h.Set("Vary", "Accept")
	h.Set("Link", "<"+help.Docs+`>; rel="help"`)
	h.Set("Access-Control-Expose-Headers", x402.HeaderPaymentRequired)

	data := newPageData(help, req.URL, commands, agentPrompt(help, commands, req))
	var body bytes.Buffer
	switch ct := negotiate(req.Accept); ct {
	case typeHTML:
		if err := page.Execute(&body, data); err != nil {
			return Response{}, err
		}
		h.Set("Content-Type", ct+"; charset=utf-8")
	case typeMarkdown:
		if err := markdown.Execute(&body, data); err != nil {
			return Response{}, err
		}
		h.Set("Content-Type", ct+"; charset=utf-8")
	default:
		info, err := json.Marshal(help)
		if err != nil {
			return Response{}, err
		}
		pr.Extensions = map[string]any{HowToPayExtension: map[string]json.RawMessage{"info": info, "schema": howToPaySchema}}
		enc := json.NewEncoder(&body)
		enc.SetIndent("", "  ") // read by people and agents as often as by programs
		if err := enc.Encode(pr); err != nil {
			return Response{}, err
		}
		h.Set("Content-Type", typeJSON)
	}
	return Response{Status: http.StatusPaymentRequired, Headers: h, Body: body.Bytes()}, nil
}

// howToPay builds the body's guidance for the offers, and the radius-cli commands that pay
// them (none when radius-cli pays no offer).
func howToPay(e *pricing.Entry, req Request, offers []Offer) (HowToPay, []string) {
	first := offers[0]
	sides := cliSides(offers)
	var commands []string
	for _, s := range sides {
		commands = append(commands, s.command(req.Method, req.URL))
	}

	// The docs follow the steps: Radius's x402 docs explain radius-cli.
	docs := "https://x402.org"
	switch {
	case len(commands) > 0:
		docs = radius.X402Docs
	case first.Profile != nil:
		docs = first.Profile.Docs
	}
	var message string
	if e.Route.Help != nil {
		if e.Route.Help.DocsURL != "" {
			docs = e.Route.Help.DocsURL
		}
		message = e.Route.Help.Message
	}
	summary := fmt.Sprintf("%s %s costs %s %s on %s.", strings.ToUpper(orGET(req.Method)), req.URL, first.Price, first.Symbol, first.network())
	if len(offers) > 1 {
		summary = strings.TrimSuffix(summary, ".") + fmt.Sprintf(" (one of %d payment options).", len(offers))
	}
	h := HowToPay{
		Summary:     summary,
		Price:       first.Price,
		Asset:       first.Symbol,
		Network:     first.network(),
		PayTo:       first.Requirements.PayTo,
		Options:     options(offers),
		Docs:        docs,
		Description: e.Route.Description,
		Message:     message,
	}
	if len(sides) == 0 {
		// No one command pays here, so the steps say what any x402 client has to do. They
		// are shell comments: pasted into a terminal, they do nothing.
		note := "token contract " + first.Requirements.Asset
		if len(offers) > 1 {
			note += "; or pay one of the other options instead"
		}
		h.Steps = []Step{
			{Do: "fund", Run: "# the paying wallet needs " + first.Price + " " + first.Symbol + " on " + first.network(), Note: note},
			{Do: "pay", Run: genericPay(first), Note: "the terms are in this response's PAYMENT-REQUIRED header (base64 JSON)"},
		}
		h.SDK = SDK{Package: "@x402/fetch", Import: "@x402/fetch", Entry: "wrapFetchWithPayment", Note: "a fetch that reads this 402, signs the payment and retries"}
		return h, nil
	}
	h.Steps = []Step{
		{Do: "install", Run: radius.CLIInstall, Note: "needs Node 22 or later"},
		{Do: "create-wallet", Run: "radius-cli wallet new", Note: "once; the key stays in ~/.radius (set RADIUS_HOME to keep it per project)"},
	}
	for i, s := range sides {
		note := "signs the payment, retries the request and prints the response; --threshold refuses any higher price"
		if len(s.chains) > 1 {
			note += "; it pays on the first of --networks the wallet can cover"
		}
		h.Steps = append(h.Steps, s.fund(), Step{Do: "pay", Run: commands[i], Note: note})
	}
	h.SDK = SDK{Package: radius.SDKPackage, Import: radius.SDKImport, Entry: radius.SDKEntry, Note: "a fetch that reads this 402, signs the payment and retries"}
	return h, commands
}

// options describes every offer for the body, in the order offered.
func options(offers []Offer) []Option {
	cli := 0
	for _, o := range offers {
		if o.RadiusCLI {
			cli++
		}
	}
	out := make([]Option, len(offers))
	for i, o := range offers {
		method := o.Requirements.TransferMethod()
		payWith := "radius-cli"
		if !o.RadiusCLI {
			payWith = "an x402 v2 client that supports " + methodName(method) + ", e.g. @x402/fetch"
			switch {
			case cli > 0 && cli == len(offers)-1:
				payWith += "; radius-cli pays the other options"
			case cli > 0:
				payWith += "; radius-cli pays the options marked radius-cli"
			}
		}
		out[i] = Option{
			Price: o.Price, Asset: o.Symbol, AssetAddress: o.Requirements.Asset,
			Network: o.network(), NetworkID: o.Requirements.Network, PayTo: o.Requirements.PayTo,
			Method: method, PayWith: payWith, knownToken: o.KnownToken,
		}
		if !o.KnownToken && o.Requirements.Extra != nil {
			out[i].tokenName = tokenName(o.Requirements.Extra.Name)
		}
		out[i].chain = chainName(o.Requirements.Network)
		if o.Profile != nil {
			out[i].chain, out[i].testnet = o.Profile.Chain, o.Profile.Testnet
		}
	}
	return out
}

// cliSide is what one radius-cli command pays: the offers it can pay on one side
// (testnet or mainnet). radius-cli takes the side once for every chain, so offers on
// both sides need a command each.
type cliSide struct {
	name    string  // "testnet" or "mainnet": radius-cli's --network
	chains  []Offer // per radius-cli chain, in the order offered: its dearest offer
	dearest Offer   // the dearest offer on this side: the command's --threshold
}

// cliSides groups the offers radius-cli pays by side, in the order offered.
func cliSides(offers []Offer) []cliSide {
	var out []cliSide
	for _, o := range offers {
		if !o.RadiusCLI {
			continue
		}
		i := 0
		for i < len(out) && out[i].name != o.side() {
			i++
		}
		if i == len(out) {
			out = append(out, cliSide{name: o.side(), dearest: o})
		}
		s := &out[i]
		if o.amount.Cmp(s.dearest.amount) > 0 {
			s.dearest = o
		}
		j := 0
		for j < len(s.chains) && s.chains[j].Profile.CLINetwork != o.Profile.CLINetwork {
			j++
		}
		switch {
		case j == len(s.chains):
			s.chains = append(s.chains, o)
		case o.amount.Cmp(s.chains[j].amount) > 0:
			s.chains[j] = o
		}
	}
	return out
}

// command is the radius-cli line that pays this request. --network is always explicit:
// otherwise a buyer's ~/.radius/config.json could switch the side. --networks lists the
// chains in the order offered, which is the order radius-cli tries them; it is left out
// when it would be radius-cli's default, Radius alone. --threshold is the dearest offer
// in display units (SBC and USDC are both dollars at 6 decimals), so the CLI refuses
// anything dearer than what this 402 asked. The URL is shell-quoted: people and agents
// paste this, and query strings carry & and ?.
func (s cliSide) command(method, url string) string {
	cmd := fmt.Sprintf("radius-cli --network %s wallet pay %s %s", s.name, strings.ToLower(orGET(method)), shellQuote(url))
	if len(s.chains) > 1 || s.chains[0].Profile.CLINetwork != radius.Testnet.CLINetwork {
		names := make([]string, len(s.chains))
		for i, c := range s.chains {
			names[i] = c.Profile.CLINetwork
		}
		cmd += " --networks " + strings.Join(names, ",")
	}
	return cmd + " --threshold " + s.dearest.Price
}

// fund is the step that gets the wallet what this side's command spends. Radius testnet
// has a faucet radius-cli drips from, and that is enough: the command pays on whichever
// of its chains the wallet can cover.
func (s cliSide) fund() Step {
	var faucet *Offer
	var needs, others []string
	for i, c := range s.chains {
		need := c.Price + " " + c.Symbol + " on " + c.network()
		needs = append(needs, need)
		if faucet == nil && c.Profile.Faucet != "" && c.Profile.CLINetwork == radius.Testnet.CLINetwork {
			faucet = &s.chains[i]
		} else {
			others = append(others, need)
		}
	}
	if faucet != nil {
		note := "test " + faucet.Symbol + " from the faucet; the payment needs " + faucet.Price + " " + faucet.Symbol
		if len(others) > 0 {
			note += "; or hold " + strings.Join(others, " or ") + " instead: the pay command pays on whichever of its networks the wallet can cover"
		}
		return Step{Do: "fund", Run: "radius-cli --network " + s.name + " wallet faucet", Note: note}
	}
	// No faucet: the wallet is funded from outside, so the step prints where to send it.
	return Step{Do: "fund", Run: "radius-cli wallet address", Note: "send at least " + strings.Join(needs, " or ") +
		" to this address; `radius-cli --network " + s.name + " wallet balance` shows when it has arrived"}
}

// genericPay is the pay step when radius-cli pays no offer.
func genericPay(first Offer) string {
	return "# any x402 v2 client: sign the exact (" + methodName(first.Requirements.TransferMethod()) + ") terms and retry with a PAYMENT-SIGNATURE header"
}

// hint is the `error` line: the first offer's price and the one command that pays it. x402
// clients print it as the reason for a 402, so it stays one short line; the other steps
// and options are in the body.
func hint(h HowToPay, commands []string, req Request, offers int) string {
	var line string
	if len(commands) > 0 {
		pay := commands[0]
		if len(req.URL) > maxURLInHint {
			pay = strings.Replace(pay, shellQuote(req.URL), "'<this URL>'", 1)
		}
		line = fmt.Sprintf("Payment required: %s %s on %s. Pay with: %s", h.Price, h.Asset, h.Network, pay)
	} else {
		line = fmt.Sprintf("Payment required: %s %s on %s. Pay with any x402 v2 client (exact, %s).", h.Price, h.Asset, h.Network, methodName(h.Options[0].Method))
	}
	if offers > 1 {
		line += fmt.Sprintf(" (%d payment options; see the response body)", offers)
	}
	if req.Refused != "" {
		return req.Refused + ": payment refused. " + line
	}
	return line
}

// agentPrompt is a message a person can hand to their agent to fetch this resource: what
// it costs, and the steps, in order.
func agentPrompt(h HowToPay, commands []string, req Request) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fetch this for me: %s %s\n", strings.ToUpper(orGET(req.Method)), req.URL)
	fmt.Fprintf(&b, "It is paid per request with x402 (HTTP 402): %s %s on %s", h.Price, h.Asset, h.Network)
	switch n := len(h.Options) - 1; {
	case n == 1:
		b.WriteString(", or the other option in the 402's PAYMENT-REQUIRED header")
	case n > 1:
		fmt.Fprintf(&b, ", or one of the other %d options in the 402's PAYMENT-REQUIRED header", n)
	}
	if len(commands) == 0 {
		b.WriteString(".\nPay it with an x402 v2 client such as @x402/fetch: it reads the 402, signs one of the offered terms and retries.\n")
		fmt.Fprintf(&b, "Do not pay more than %s %s without asking me.", h.Price, h.Asset)
		return b.String()
	}
	b.WriteString(".\nPay it with radius-cli. Run these in order, skipping any already done:\n")
	for i, s := range h.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, s.Run)
	}
	b.WriteString("`wallet pay` signs the payment, retries the request and prints the response; its --threshold is the most it will pay. Do not raise it without asking me.")
	return b.String()
}

func orGET(method string) string {
	if method == "" {
		return http.MethodGet
	}
	return method
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// The body formats, by media type.
const (
	typeMarkdown = "text/markdown"
	typeHTML     = "text/html"
	typeJSON     = "application/json"
)

// negotiate picks the body's media type from Accept (RFC 9110 §12.5.1). Only an explicit
// text/markdown, text/html or application/json counts; a wildcard (*/*, text/*,
// application/*) counts for none of them, because */* is what curl and most HTTP
// libraries send by default, and they get JSON. The highest q wins; q=0 means "not this
// one"; a tie goes to markdown, then HTML, then JSON, the order of how much a reader who
// named them gets out of each. A header that names none of them, or that cannot be read,
// gets JSON.
func negotiate(accept string) string {
	q := map[string]int{} // q in thousandths
	for _, r := range strings.Split(accept, ",") {
		params := strings.Split(r, ";")
		media := strings.ToLower(strings.TrimSpace(params[0]))
		if media != typeMarkdown && media != typeHTML && media != typeJSON {
			continue
		}
		w, ok := 1000, true
		for _, p := range params[1:] {
			if k, v, _ := strings.Cut(p, "="); strings.EqualFold(strings.TrimSpace(k), "q") {
				w, ok = qvalue(strings.TrimSpace(v))
			}
		}
		if ok && w > q[media] {
			q[media] = w
		}
	}
	best, bestQ := typeJSON, 0
	for _, t := range []string{typeMarkdown, typeHTML, typeJSON} {
		if q[t] > bestQ {
			best, bestQ = t, q[t]
		}
	}
	return best
}

// qvalue parses a weight as RFC 9110 §12.4.2 writes it: 0 to 1 with at most three
// decimals. It returns thousandths, so weights compare exactly.
func qvalue(s string) (int, bool) {
	whole, frac, _ := strings.Cut(s, ".")
	if (whole != "0" && whole != "1") || len(frac) > 3 || strings.Trim(frac, "0123456789") != "" {
		return 0, false
	}
	n := 0
	for i := range 3 {
		n *= 10
		if i < len(frac) {
			n += int(frac[i] - '0')
		}
	}
	if whole == "1" {
		if n != 0 {
			return 0, false
		}
		return 1000, true
	}
	return n, true
}

type pageData struct {
	HowToPay
	URL         string
	Commands    []string // the radius-cli commands that pay this; none when it pays no option
	AgentPrompt string
	Setup       []Step   // the steps before paying: install, wallet, fund
	First       Option   // the headline price
	Others      []Option // every other option, for "also accepted"
	Testnet     bool     // every option is on a test network
	SomeTestnet bool     // some options are, so each row says which
	Host, Path  string   // the resource, split for the heading
}

// newPageData is what the HTML page shows: the same how-to-pay as the other formats,
// arranged for a person.
func newPageData(h HowToPay, url string, commands []string, prompt string) pageData {
	d := pageData{HowToPay: h, URL: url, Commands: commands, AgentPrompt: prompt, Host: url}
	for _, s := range h.Steps {
		if s.Do != "pay" {
			d.Setup = append(d.Setup, s)
		}
	}
	if len(h.Options) > 0 {
		d.First, d.Others = h.Options[0], h.Options[1:]
		d.Testnet = true
		for _, o := range h.Options {
			d.Testnet = d.Testnet && o.testnet
			d.SomeTestnet = d.SomeTestnet || o.testnet
		}
		d.SomeTestnet = d.SomeTestnet && !d.Testnet
	}
	if _, rest, ok := strings.Cut(url, "://"); ok {
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			d.Host, d.Path = rest[:i], rest[i:]
		} else {
			d.Host, d.Path = rest, "/"
		}
	}
	return d
}

//go:embed page.html
var pageHTML string

// page is html/template, so the URL and every other value are escaped for their context.
// Its one script is the copy buttons: each copies the text of the element just before it
// and reads nothing else. The clipboard API exists only in a secure context, and a 402
// is often served over plain HTTP, so there the button selects the text for Ctrl+C
// instead. The 402 sends no Content-Security-Policy, so the inline script runs as is.
// (The script's own comments would be stripped by html/template, so they live here.)
var page = template.Must(template.New("402.html").Parse(pageHTML))

//go:embed page.md
var pageMarkdown string

// markdown has no escaping of its own, so the one value a caller controls, the URL, goes
// through md: backticks and line breaks cannot close a code span or start a new block.
// Table cells go through cell, which also keeps a | in a policy's text from splitting
// the row.
var markdown = texttemplate.Must(texttemplate.New("402.md").Funcs(texttemplate.FuncMap{
	"md":   mdSafe,
	"cell": func(s string) string { return strings.ReplaceAll(mdSafe(s), "|", `\|`) },
	"inc":  func(i int) int { return i + 1 },
}).Parse(pageMarkdown))

func mdSafe(s string) string {
	return strings.NewReplacer("`", "%60", "\r", "%0D", "\n", "%0A").Replace(s)
}
