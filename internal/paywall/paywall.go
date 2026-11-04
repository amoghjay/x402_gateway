// Package paywall renders the 402 for a priced route: what a caller without a payment
// gets back, and everything they need to pay.
//
// The PAYMENT-REQUIRED header is the protocol: x402 V2 clients (radius-cli, radius-sdk,
// @x402/fetch) read it and ignore the body whenever it is present. Everything else is for
// the people and agents who don't speak x402 yet, through channels that cannot break the
// clients that do:
//
//   - the `error` field: one line saying the price and the command that pays it;
//   - a JSON body: the same PaymentRequired, plus extensions["how-to-pay"]. It is kept
//     out of the header on purpose, because clients echo every header extension back in
//     PAYMENT-SIGNATURE, so it would ride along on each paid request;
//   - `Link: <docs>; rel="help"` (RFC 8288);
//   - an HTML page for browsers, detected the way coinbase/x402 does (Accept contains
//     text/html AND User-Agent contains Mozilla), so radius-cli, which sends neither,
//     always gets JSON.
//
// Every 402 carries the header, `Cache-Control: no-store`, and exposes the header to
// browser JavaScript.
package paywall

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"

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
}

// Response is the 402 to send.
type Response struct {
	Status  int
	Headers http.Header
	Body    []byte
}

// HowToPay is the `info` of the how-to-pay extension.
type HowToPay struct {
	Price   string `json:"price"`   // display units, e.g. "0.001"
	Asset   string `json:"asset"`   // "SBC"
	Network string `json:"network"` // "Radius testnet"
	Docs    string `json:"docs"`
	CLI     CLI    `json:"cli"`
	SDK     SDK    `json:"sdk"`
	Faucet  string `json:"faucet,omitempty"` // testnet only: the faucet API
	Message string `json:"message,omitempty"`
}

// CLI is how to pay from a terminal or an agent.
type CLI struct {
	Install string `json:"install"`
	Command string `json:"command"`
}

// SDK is how to pay from application code.
type SDK struct {
	Package string `json:"package"`
	Import  string `json:"import"`
	Entry   string `json:"entry"`
}

// howToPaySchema is the JSON Schema of HowToPay; the spec asks every extension for one.
var howToPaySchema = json.RawMessage(`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object",` +
	`"required":["price","asset","network","docs","cli","sdk"],"properties":{` +
	`"price":{"type":"string"},"asset":{"type":"string"},"network":{"type":"string"},"docs":{"type":"string","format":"uri"},` +
	`"cli":{"type":"object","properties":{"install":{"type":"string"},"command":{"type":"string"}}},` +
	`"sdk":{"type":"object","properties":{"package":{"type":"string"},"import":{"type":"string"},"entry":{"type":"string"}}},` +
	`"faucet":{"type":"string","format":"uri"},"message":{"type":"string"}}}`)

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
	h.Set("Link", "<"+help.Docs+`>; rel="help"`)
	h.Set("Access-Control-Expose-Headers", x402.HeaderPaymentRequired)

	var body []byte
	if isBrowser(req) {
		var buf bytes.Buffer
		if err := page.Execute(&buf, pageData{HowToPay: help, PayTo: e.Requirements.PayTo, URL: req.URL, Description: e.Route.Description}); err != nil {
			return Response{}, err
		}
		body = buf.Bytes()
		h.Set("Content-Type", "text/html; charset=utf-8")
	} else {
		info, err := json.Marshal(help)
		if err != nil {
			return Response{}, err
		}
		pr.Extensions = map[string]any{HowToPayExtension: map[string]json.RawMessage{"info": info, "schema": howToPaySchema}}
		if body, err = json.Marshal(pr); err != nil {
			return Response{}, err
		}
		h.Set("Content-Type", "application/json")
	}
	return Response{Status: http.StatusPaymentRequired, Headers: h, Body: body}, nil
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
	return HowToPay{
		Price:   radius.FormatSBC(e.Price),
		Asset:   radius.DisplayAsset,
		Network: "Radius " + e.Network.Name,
		Docs:    docs,
		CLI:     CLI{Install: radius.CLIInstall, Command: cliCommand(e, req.Method, req.URL)},
		SDK:     SDK{Package: radius.SDKPackage, Import: radius.SDKImport, Entry: radius.SDKEntry},
		Faucet:  e.Network.Faucet,
		Message: message,
	}
}

// cliCommand is the radius-cli line that pays this request. --threshold is the price in
// display units, so the CLI refuses anything dearer than what this 402 asked. The URL is
// shell-quoted: people paste this, and query strings carry & and ?.
func cliCommand(e *pricing.Entry, method, url string) string {
	verb := strings.ToLower(method)
	if verb == "" {
		verb = "get"
	}
	cmd := fmt.Sprintf("radius-cli wallet pay %s %s --threshold %s", verb, shellQuote(url), radius.FormatSBC(e.Price))
	if e.Network.Name != radius.Mainnet.Name {
		cmd = "RADIUS_NETWORK=" + e.Network.Name + " " + cmd
	}
	return cmd
}

// hint is the `error` line: what it costs and the one command that pays it. It goes into
// a header (inside base64 JSON), so it is kept to one line.
func hint(h HowToPay, req Request) string {
	url := req.URL
	if len(url) > maxURLInHint {
		url = "<this URL>"
	}
	cmd := h.CLI.Command
	if len(req.URL) > maxURLInHint {
		cmd = strings.Replace(cmd, shellQuote(req.URL), url, 1)
	}
	s := fmt.Sprintf("Payment required: %s %s on %s. Pay with `%s` (install: %s), or %s from %s in code. Docs: %s",
		h.Price, h.Asset, h.Network, cmd, h.CLI.Install, h.SDK.Entry, h.SDK.Import, h.Docs)
	if h.Message != "" {
		s += " " + h.Message
	}
	return s
}

// shellQuote wraps s in single quotes for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func isBrowser(req Request) bool {
	return strings.Contains(req.Accept, "text/html") && strings.Contains(req.UserAgent, "Mozilla")
}

type pageData struct {
	HowToPay
	PayTo       string
	URL         string
	Description string
}

//go:embed page.html
var pageHTML string

// page is html/template, so the URL and every other value are escaped for their context.
var page = template.Must(template.New("402").Parse(pageHTML))
