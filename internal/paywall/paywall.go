// Package paywall renders the 402 for a priced route: what a caller without a payment
// gets back, and everything they need to pay.
//
// The PAYMENT-REQUIRED header is the protocol: x402 V2 clients (radius-cli, radius-sdk,
// @x402/fetch) read it and ignore the body whenever it is present. Everything else is for
// the people and agents who don't speak x402 yet, through channels that cannot break the
// clients that do. Who reads what decides the shape:
//
//   - `error`, inside the header: x402 clients print it (radius-cli as "reason: ...").
//     One line: the price and the command that pays it.
//   - the body, which is what an agent calling with curl or a fetch tool reads first:
//     JSON by default, the same PaymentRequired plus extensions["how-to-pay"] with ordered
//     steps an agent can follow; markdown when the caller asks for text/markdown, because
//     that is what language-model agents read best (Anteroom answers in markdown too).
//     how-to-pay stays out of the header on purpose: clients echo every header extension
//     back in PAYMENT-SIGNATURE, so it would ride along on each paid request.
//   - an HTML page for browsers, detected the way coinbase/x402 does (Accept contains
//     text/html AND User-Agent contains Mozilla), so radius-cli, which sends neither,
//     never gets it.
//   - `Link: <docs>; rel="help"` (RFC 8288).
//
// All of them are built from one list of steps, so they cannot drift apart. Every 402
// carries the header, `Cache-Control: no-store`, `Vary: Accept, User-Agent`, and exposes
// the header to browser JavaScript.
package paywall

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	texttemplate "text/template"

	"github.com/amoghjay/x402_gateway/internal/pricing"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// HowToPayExtension is the key of our extension in the 402's JSON body.
const HowToPayExtension = "how-to-pay"

// maxURLInHint caps how much of the request URL is repeated in the `error` line. The full
// URL is always in resource.url.
const maxURLInHint = 512

// Request is what the paywall needs from the request.
type Request struct {
	Method    string // GET, POST, ...
	URL       string // the public URL the caller asked for
	Accept    string
	UserAgent string
	// Refused is set when a payment was sent and turned down: the reason code (spec
	// §9 where one exists) leads the `error` line, so a client that prints the 402's
	// error says why, and the how-to-pay hint still follows for the retry.
	Refused string
}

// Response is the 402 to send.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// HowToPay is the `info` of the how-to-pay extension.
type HowToPay struct {
	Summary     string `json:"summary"` // "GET <url> costs 0.001 SBC on Radius testnet."
	Price       string `json:"price"`   // display units, e.g. "0.001"
	Asset       string `json:"asset"`   // "SBC"
	Network     string `json:"network"` // "Radius testnet"
	PayTo       string `json:"payTo"`
	Steps       []Step `json:"steps"`
	SDK         SDK    `json:"sdk"`
	Docs        string `json:"docs"`
	Description string `json:"description,omitempty"` // the route's, from the price table
	Message     string `json:"message,omitempty"`     // the route's help message
}

// Step is one thing to do to pay, in order. Run is a shell command; an agent can run it
// as is. The `pay` step is the one that fetches the resource.
type Step struct {
	Do   string `json:"do"` // install, create-wallet, fund, pay
	Run  string `json:"run"`
	Note string `json:"note,omitempty"`
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
	`"required":["summary","price","asset","network","payTo","steps","sdk","docs"],"properties":{` +
	`"summary":{"type":"string"},"price":{"type":"string"},"asset":{"type":"string"},"network":{"type":"string"},"payTo":{"type":"string"},` +
	`"steps":{"type":"array","items":{"type":"object","required":["do","run"],"properties":{"do":{"type":"string"},"run":{"type":"string"},"note":{"type":"string"}}}},` +
	`"sdk":{"type":"object","properties":{"package":{"type":"string"},"import":{"type":"string"},"entry":{"type":"string"},"note":{"type":"string"}}},` +
	`"docs":{"type":"string","format":"uri"},"description":{"type":"string"},"message":{"type":"string"}}}`)

// Render builds the 402 for a priced route.
func Render(e *pricing.Entry, req Request) (Response, error) {
	help := howToPay(e, req)
	pr := x402.PaymentRequired{
		X402Version: x402.Version,
		Error:       hint(help, req),
		Resource:    x402.Resource{URL: req.URL, Description: e.Route.Description, MimeType: e.Route.MimeType},
		Accepts:     []x402.Requirements{e.Requirements},
	}
	header, err := pr.Encode()
	if err != nil {
		return Response{}, err
	}
	h := http.Header{}
	h.Set(x402.HeaderPaymentRequired, header)
	h.Set("Cache-Control", "no-store")
	h.Set("Vary", "Accept, User-Agent")
	h.Set("Link", "<"+help.Docs+`>; rel="help"`)
	h.Set("Access-Control-Expose-Headers", x402.HeaderPaymentRequired)

	data := pageData{HowToPay: help, URL: req.URL, Terms: e.Requirements}
	var body bytes.Buffer
	switch {
	case isBrowser(req):
		if err := page.Execute(&body, data); err != nil {
			return Response{}, err
		}
		h.Set("Content-Type", "text/html; charset=utf-8")
	case strings.Contains(req.Accept, "text/markdown"):
		if err := markdown.Execute(&body, data); err != nil {
			return Response{}, err
		}
		h.Set("Content-Type", "text/markdown; charset=utf-8")
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
		h.Set("Content-Type", "application/json")
	}
	return Response{Status: http.StatusPaymentRequired, Headers: h, Body: body.Bytes()}, nil
}

func howToPay(e *pricing.Entry, req Request) HowToPay {
	docs := radius.X402Docs
	var message string
	if e.Route.Help != nil {
		if e.Route.Help.DocsURL != "" {
			docs = e.Route.Help.DocsURL
		}
		message = e.Route.Help.Message
	}
	price := radius.FormatSBC(e.Price)
	network := "Radius " + e.Network.Name
	// radius-cli takes the network from RADIUS_NETWORK and defaults to mainnet.
	env := ""
	if e.Network.Name != radius.Mainnet.Name {
		env = "RADIUS_NETWORK=" + e.Network.Name + " "
	}
	steps := []Step{
		{Do: "install", Run: radius.CLIInstall},
		{Do: "create-wallet", Run: "radius-cli wallet new", Note: "once; the key stays in ~/.radius (set RADIUS_HOME to keep it per project)"},
	}
	if e.Network.Faucet != "" {
		steps = append(steps, Step{Do: "fund", Run: env + "radius-cli wallet faucet", Note: "test SBC from the faucet; the payment needs " + price + " " + radius.DisplayAsset})
	} else {
		steps = append(steps, Step{Do: "fund", Run: env + "radius-cli wallet balance", Note: "the wallet needs at least " + price + " " + radius.DisplayAsset})
	}
	steps = append(steps, Step{
		Do:   "pay",
		Run:  cliCommand(env, req.Method, req.URL, price),
		Note: "signs the payment, retries the request and prints the response; --threshold refuses any higher price",
	})
	return HowToPay{
		Summary:     fmt.Sprintf("%s %s costs %s %s on %s.", strings.ToUpper(orGET(req.Method)), req.URL, price, radius.DisplayAsset, network),
		Price:       price,
		Asset:       radius.DisplayAsset,
		Network:     network,
		PayTo:       e.Requirements.PayTo,
		Steps:       steps,
		SDK:         SDK{Package: radius.SDKPackage, Import: radius.SDKImport, Entry: radius.SDKEntry, Note: "a fetch that reads this 402, signs the payment and retries"},
		Docs:        docs,
		Description: e.Route.Description,
		Message:     message,
	}
}

// cliCommand is the radius-cli line that pays this request. --threshold is the price in
// display units, so the CLI refuses anything dearer than what this 402 asked. The URL is
// shell-quoted: people and agents paste this, and query strings carry & and ?.
func cliCommand(env, method, url, price string) string {
	return fmt.Sprintf("%sradius-cli wallet pay %s %s --threshold %s", env, strings.ToLower(orGET(method)), shellQuote(url), price)
}

// hint is the `error` line: the price and the one command that pays it. x402 clients
// print it as the reason for a 402, so it stays one short line; the other steps are in
// the body.
func hint(h HowToPay, req Request) string {
	pay := h.Steps[len(h.Steps)-1].Run
	if len(req.URL) > maxURLInHint {
		pay = strings.Replace(pay, shellQuote(req.URL), "'<this URL>'", 1)
	}
	line := fmt.Sprintf("Payment required: %s %s on %s. Pay with: %s", h.Price, h.Asset, h.Network, pay)
	if req.Refused != "" {
		return req.Refused + ": payment refused. " + line
	}
	return line
}

func orGET(method string) string {
	if method == "" {
		return http.MethodGet
	}
	return method
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func isBrowser(req Request) bool {
	return strings.Contains(req.Accept, "text/html") && strings.Contains(req.UserAgent, "Mozilla")
}

type pageData struct {
	HowToPay
	URL   string
	Terms x402.Requirements
}

//go:embed page.html
var pageHTML string

// page is html/template, so the URL and every other value are escaped for their context.
var page = template.Must(template.New("402.html").Parse(pageHTML))

//go:embed page.md
var pageMarkdown string

// markdown has no escaping of its own, so the one value a caller controls, the URL, goes
// through md: backticks and line breaks cannot close a code span or start a new block.
var markdown = texttemplate.Must(texttemplate.New("402.md").Funcs(texttemplate.FuncMap{"md": mdSafe, "inc": func(i int) int { return i + 1 }}).Parse(pageMarkdown))

func mdSafe(s string) string {
	return strings.NewReplacer("`", "%60", "\r", "%0D", "\n", "%0A").Replace(s)
}
