// Command x402-buyer is the paying side of x402 V2 on Radius, for testing the gateway.
//
//	x402-buyer keygen                          make a key file and print its address
//	x402-buyer address                         print the wallet address
//	x402-buyer drip                            fund the wallet from the signed testnet faucet
//	x402-buyer balance [ADDRESS]               SBC balance of the wallet, or of ADDRESS
//	x402-buyer settle -pay-to A -amount 0.001  sign, verify locally, then the facilitator's
//	                                           /verify and /settle, and check the chain
//	x402-buyer pay [-max 0.01] URL             request URL, pay its 402, print the answer
//
// The key is read from -key (default $X402_BUYER_KEY_FILE, else ~/.x402/buyer-<network>.key)
// and never printed. Mainnet needs -allow-mainnet on every command that spends.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amoghjay/x402_gateway/internal/buildinfo"
	"github.com/amoghjay/x402_gateway/internal/buyer"
	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/facilitator"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402"
	"github.com/amoghjay/x402_gateway/internal/x402/verify"
)

const usage = `usage: x402-buyer <command> [flags]

commands:
  keygen            make a key file and print its address
  address           print the wallet address
  drip              fund the wallet from the signed testnet faucet
  balance [ADDR]    SBC balance of the wallet, or of ADDR
  settle            pay -pay-to through the facilitator directly (the M2 done-check)
  pay URL           request URL, pay its 402, print the answer
  version           print the version

Run "x402-buyer <command> -h" for the flags of one command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"keygen": keygen, "address": address, "drip": drip, "balance": balance,
		"settle": settle, "pay": pay,
		"version": func([]string) error { fmt.Println(buildinfo.Version); return nil },
	}
	run, ok := cmds[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "x402-buyer:", err)
		}
		os.Exit(1)
	}
}

// common holds the flags every command shares.
type common struct {
	fs           *flag.FlagSet
	network      string
	keyFile      string
	allowMainnet bool
	timeout      time.Duration
}

func newCommon(name string) *common {
	c := &common{fs: flag.NewFlagSet(name, flag.ContinueOnError)}
	c.fs.StringVar(&c.network, "network", "testnet", "testnet or mainnet")
	c.fs.StringVar(&c.keyFile, "key", os.Getenv("X402_BUYER_KEY_FILE"), "key file (default ~/.x402/buyer-<network>.key)")
	c.fs.BoolVar(&c.allowMainnet, "allow-mainnet", false, "allow spending real SBC on mainnet")
	c.fs.DurationVar(&c.timeout, "timeout", 30*time.Second, "limit for the whole command")
	return c
}

func (c *common) parse(args []string) (radius.Profile, error) {
	if err := c.fs.Parse(args); err != nil {
		return radius.Profile{}, err
	}
	p, err := radius.Lookup(c.network)
	if err != nil {
		return radius.Profile{}, err
	}
	if c.keyFile == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return radius.Profile{}, err
		}
		c.keyFile = filepath.Join(home, ".x402", "buyer-"+p.Name+".key")
	}
	return p, nil
}

// spends refuses mainnet unless the caller said so.
func (c *common) spends(p radius.Profile) error {
	if p.Name == radius.Mainnet.Name && !c.allowMainnet {
		return errors.New("this spends real SBC on mainnet; pass -allow-mainnet if you mean it")
	}
	return nil
}

func (c *common) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.timeout)
}

func (c *common) wallet(p radius.Profile) (*buyer.Wallet, error) {
	b, err := os.ReadFile(c.keyFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no key at %s; run x402-buyer keygen first", c.keyFile)
	}
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(c.keyFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by others (%v); chmod 600 it\n", c.keyFile, fi.Mode().Perm())
	}
	k, err := evm.ParsePrivateKey(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.keyFile, err)
	}
	return &buyer.Wallet{Key: k, Network: p, HTTP: &http.Client{}}, nil
}

func rpcFor(p radius.Profile) *radius.RPC { return &radius.RPC{URL: p.RPC, HTTP: &http.Client{}} }

func keygen(args []string) error {
	c := newCommon("keygen")
	p, err := c.parse(args)
	if err != nil {
		return err
	}
	if _, err := os.Stat(c.keyFile); err == nil {
		return fmt.Errorf("%s exists; not overwriting it", c.keyFile)
	}
	k, err := evm.GeneratePrivateKey()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.keyFile), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(c.keyFile, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, k.Hex()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Printf("%s key written to %s\naddress %s\n", p.Name, c.keyFile, k.Address())
	return nil
}

func address(args []string) error {
	c := newCommon("address")
	p, err := c.parse(args)
	if err != nil {
		return err
	}
	w, err := c.wallet(p)
	if err != nil {
		return err
	}
	fmt.Println(w.Address())
	return nil
}

func drip(args []string) error {
	c := newCommon("drip")
	p, err := c.parse(args)
	if err != nil {
		return err
	}
	w, err := c.wallet(p)
	if err != nil {
		return err
	}
	ctx, cancel := c.context()
	defer cancel()
	res, err := w.Drip(ctx)
	if err != nil {
		if res.RetryAfterMS > 0 {
			return fmt.Errorf("%w (retry after %s)", err, time.Duration(res.RetryAfterMS)*time.Millisecond)
		}
		return err
	}
	fmt.Printf("dripped SBC to %s\ntx %s\n", w.Address(), p.TxURL(res.TxHash))
	return nil
}

func balance(args []string) error {
	c := newCommon("balance")
	p, err := c.parse(args)
	if err != nil {
		return err
	}
	var who evm.Address
	if c.fs.NArg() > 0 {
		if who, err = evm.ParseAddress(c.fs.Arg(0)); err != nil {
			return err
		}
	} else {
		w, err := c.wallet(p)
		if err != nil {
			return err
		}
		who = w.Address()
	}
	ctx, cancel := c.context()
	defer cancel()
	bal, err := rpcFor(p).BalanceOf(ctx, radius.SBCAddress, who)
	if err != nil {
		return err
	}
	fmt.Printf("%s SBC  %s on %s\n", radius.FormatSBC(bal), who, p.Name)
	return nil
}

// settle is the M2 done-check: one payment signed by our buyer, accepted by our own
// verifier, then verified and settled by the Radius facilitator, and the transfer seen
// on chain. No server of ours is involved yet; that is M3.
func settle(args []string) error {
	c := newCommon("settle")
	payTo := c.fs.String("pay-to", "", "recipient address (required)")
	amount := c.fs.String("amount", "0.001", "price in SBC")
	maxTimeout := c.fs.Int("max-timeout", 300, "the requirement's maxTimeoutSeconds")
	validFor := c.fs.Duration("valid-for", 60*time.Second, "how long the signature stays valid")
	verifyOnly := c.fs.Bool("verify-only", false, "stop after the facilitator's /verify; nothing is spent")
	p, err := c.parse(args)
	if err != nil {
		return err
	}
	if !*verifyOnly {
		if err := c.spends(p); err != nil {
			return err
		}
	}
	to, err := evm.ParseAddress(*payTo)
	if err != nil {
		return fmt.Errorf("-pay-to: %w", err)
	}
	atomic, err := radius.ParseSBC(*amount)
	if err != nil {
		return err
	}
	w, err := c.wallet(p)
	if err != nil {
		return err
	}
	ctx, cancel := c.context()
	defer cancel()

	req := p.ExactRequirements(atomic.String(), to.String(), *maxTimeout)
	resource := &x402.Resource{URL: "urn:x402-buyer:settle", Description: "x402-buyer settle check"}
	now := time.Now()
	pp, err := w.Sign(req, resource, now, *validFor)
	if err != nil {
		return err
	}
	fmt.Printf("payer   %s\npayTo   %s\namount  %s SBC (%s atomic) on %s\n", w.Address(), to, radius.FormatSBC(atomic), atomic, p.Network())

	t0 := time.Now()
	res, err := verify.Verify(now, req, w.Domain(), pp)
	if err != nil {
		return fmt.Errorf("local verify refused our own payment: %w", err)
	}
	fmt.Printf("local verify         ok, payer %s, %s\n", res.Payer, time.Since(t0).Round(time.Microsecond))

	rpc := rpcFor(p)
	payerBefore, err := rpc.BalanceOf(ctx, radius.SBCAddress, w.Address())
	if err != nil {
		return err
	}
	toBefore, err := rpc.BalanceOf(ctx, radius.SBCAddress, to)
	if err != nil {
		return err
	}
	if payerBefore.Cmp(atomic) < 0 {
		fmt.Printf("warning: payer holds %s SBC, less than the amount; expect insufficient_funds (run drip)\n", radius.FormatSBC(payerBefore))
	}

	fac := &facilitator.Client{URL: p.Facilitator, HTTP: &http.Client{}}
	t0 = time.Now()
	vr, err := fac.Verify(ctx, pp, req)
	if err != nil {
		return fmt.Errorf("facilitator /verify: %w", err)
	}
	if !vr.IsValid {
		return fmt.Errorf("facilitator /verify: invalid: %s", vr.InvalidReason)
	}
	fmt.Printf("facilitator /verify  valid, payer %s, %s\n", vr.Payer, time.Since(t0).Round(time.Millisecond))
	if *verifyOnly {
		return nil
	}

	t0 = time.Now()
	sr, err := fac.Settle(ctx, pp, req)
	if err != nil {
		return fmt.Errorf("facilitator /settle: %w", err)
	}
	if !sr.Success {
		return fmt.Errorf("facilitator /settle: %s (tx %q)", sr.ErrorReason, sr.Transaction)
	}
	fmt.Printf("facilitator /settle  success, %s\ntx %s\n", time.Since(t0).Round(time.Millisecond), p.TxURL(sr.Transaction))

	// The facilitator answers once the transaction is included; read both balances
	// until the transfer shows, so the check rests on the chain and not on its word.
	want := new(big.Int).Sub(payerBefore, atomic)
	for {
		payerAfter, err := rpc.BalanceOf(ctx, radius.SBCAddress, w.Address())
		if err != nil {
			return err
		}
		if payerAfter.Cmp(want) <= 0 {
			toAfter, err := rpc.BalanceOf(ctx, radius.SBCAddress, to)
			if err != nil {
				return err
			}
			fmt.Printf("on chain             payer -%s SBC, payTo +%s SBC\n",
				radius.FormatSBC(new(big.Int).Sub(payerBefore, payerAfter)),
				radius.FormatSBC(new(big.Int).Sub(toAfter, toBefore)))
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("settled, but the payer's balance has not moved yet: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// headerFlag collects repeated -H "Name: value" flags.
type headerFlag http.Header

func (h headerFlag) String() string { return "" }

func (h headerFlag) Set(s string) error {
	name, value, ok := strings.Cut(s, ":")
	if !ok {
		return fmt.Errorf("header %q: want Name: value", s)
	}
	http.Header(h).Add(strings.TrimSpace(name), strings.TrimSpace(value))
	return nil
}

func pay(args []string) error {
	c := newCommon("pay")
	method := c.fs.String("X", http.MethodGet, "HTTP method")
	data := c.fs.String("d", "", "request body")
	maxSBC := c.fs.String("max", "0.01", "refuse an offer above this many SBC")
	validFor := c.fs.Duration("valid-for", 60*time.Second, "how long the signature stays valid")
	headers := headerFlag{}
	c.fs.Var(headers, "H", "extra request header, repeatable")
	p, err := c.parse(args)
	if err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return errors.New("pay: want exactly one URL")
	}
	if err := c.spends(p); err != nil {
		return err
	}
	limit, err := radius.ParseSBC(*maxSBC)
	if err != nil {
		return err
	}
	w, err := c.wallet(p)
	if err != nil {
		return err
	}
	ctx, cancel := c.context()
	defer cancel()

	var body []byte
	if *data != "" {
		body = []byte(*data)
	}
	out, err := w.Pay(ctx, *method, c.fs.Arg(0), body, http.Header(headers), limit, *validFor)
	if err != nil {
		return err
	}
	// The answer's body goes to stdout so it can be piped; everything else to stderr.
	if out.Paid.Amount != "" {
		amount, _ := evm.ParseUint256(out.Paid.Amount)
		fmt.Fprintf(os.Stderr, "paid %s SBC to %s on %s\n", radius.FormatSBC(amount), out.Paid.PayTo, out.Paid.Network)
	} else {
		fmt.Fprintln(os.Stderr, "no 402; nothing paid")
	}
	fmt.Fprintf(os.Stderr, "HTTP %d\n", out.Status)
	if s := out.Settlement; s != nil {
		switch {
		case s.Transaction != "":
			fmt.Fprintf(os.Stderr, "settlement %v, tx %s\n", s.Success, p.TxURL(s.Transaction))
		case s.ErrorReason != "":
			fmt.Fprintf(os.Stderr, "settlement %v, %s\n", s.Success, s.ErrorReason)
		default:
			fmt.Fprintf(os.Stderr, "settlement %v, pending %v\n", s.Success, s.Extensions[x402.SettlementExtension])
		}
	}
	if _, err := os.Stdout.Write(out.Body); err != nil {
		return err
	}
	if out.Status >= 400 {
		return fmt.Errorf("HTTP %d", out.Status)
	}
	return nil
}
