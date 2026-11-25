package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/amoghjay/x402_gateway/internal/pricing"
)

// validate is `x402-filter validate [-networks …] [-route-keys …] <dir | prices.json>`:
// it reads a table the way the filter would, through the same code, and prints what each
// route would do. Nothing is started and nothing is called. It returns the exit code: 0
// when every route loads, 1 when the table is unreadable, a route is blocked, or a Radius
// Platform route has no policy file to enforce, 2 for a usage error.
func validate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("validate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	networks := fs.String("networks", "testnet", "comma-separated networks routes may charge on, as the filter's -networks")
	routeKeys := fs.String("route-keys", "name", "httproute (Envoy Gateway's names) or name (the route name itself), as the filter's -route-keys")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: x402-filter validate [-networks testnet] [-route-keys name] <dir | prices.json>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	nets, err := parseNetworks(*networks)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}
	keys, err := pricing.ParseKeyStyle(*routeKeys)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 2
	}

	data, err := readTable(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintln(stdout, "unreadable:", err)
		return 1
	}
	// The policy and key-directory loaders report problems as error logs; each is a failure.
	errs := &counting{Handler: slog.NewTextHandler(stdout, &slog.HandlerOptions{Level: slog.LevelError}), n: new(atomic.Int64)}
	p, err := pricing.Build(data, nil, keys, nets, slog.New(errs))
	if p == nil {
		_, _ = fmt.Fprintln(stdout, "unreadable:", err)
		return 1
	}
	problems := int(errs.n.Load())

	for _, k := range p.Keys() {
		e, _ := p.Lookup(k)
		tok := e.Network.Token
		_, _ = fmt.Fprintf(stdout, "priced    %s: %s %s on %s (%s), %s, pays %s\n",
			k, tok.Format(e.Price), tok.Symbol, e.Network.Name, e.Network.Network(), captureMode(e), e.Requirements.PayTo)
	}
	for _, k := range p.PlatformKeys() {
		r, _ := p.Platform(k)
		if pol := p.Policy(r.Project); pol != nil {
			_, _ = fmt.Fprintf(stdout, "platform  %s: project %s, policy version %d, %d rules\n", k, r.Project, pol.Version, len(pol.Rules))
			continue
		}
		problems++
		_, _ = fmt.Fprintf(stdout, "NO POLICY %s: project %s has no loadable %s beside the table\n", k, r.Project, pricing.PolicyKey(r.Project))
	}
	for _, k := range p.BlockedKeys() {
		why, _ := p.Blocked(k)
		problems++
		_, _ = fmt.Fprintf(stdout, "BLOCKED   %s: %s\n", k, why)
	}
	if problems > 0 {
		_, _ = fmt.Fprintf(stdout, "%d problem(s)\n", problems)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "ok: %d priced, %d platform\n", p.Len(), p.PlatformLen())
	return 0
}

// readTable reads a table directory, or a single prices.json.
func readTable(path string) (map[string]string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return pricing.ReadDir(path)
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	return map[string]string{pricing.ConfigMapKey: string(b)}, nil
}

func captureMode(e *pricing.Entry) string {
	if e.Sync() {
		return "settled before the response is released (Sync)"
	}
	return "response first, settled after (Async)"
}

// counting is a log handler that counts the records it is given, including through
// loggers derived from it with With.
type counting struct {
	slog.Handler
	n *atomic.Int64
}

func (c *counting) Handle(ctx context.Context, r slog.Record) error {
	c.n.Add(1)
	return c.Handler.Handle(ctx, r)
}

func (c *counting) WithAttrs(as []slog.Attr) slog.Handler {
	return &counting{Handler: c.Handler.WithAttrs(as), n: c.n}
}

func (c *counting) WithGroup(name string) slog.Handler {
	return &counting{Handler: c.Handler.WithGroup(name), n: c.n}
}
