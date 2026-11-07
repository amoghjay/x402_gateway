// x402-settlebench measures what it costs to settle an EIP-3009 payment ourselves,
// with eth_sendRawTransactionSync, instead of through the facilitator. It answers the
// questions in the plan's "M6 research": latency from GKE, the fee per settle, what a
// revert costs, how a burst behaves on one RPC key, and the exact revert strings.
//
//	x402-settlebench -mode serial  -n 500 -interval 500ms   steady rate, one at a time
//	x402-settlebench -mode burst   -n 100                   all at once, one key, one nonce run
//	x402-settlebench -mode reverts                          expired, not yet valid, bad sig, unfunded, reused, cancelled
//
// Keys come from files (SUBMITTER_KEY_FILE pays gas; PAYER_KEY_FILE signs the
// authorizations) and the RPC URL from RADIUS_RPC_URL, so a keyed URL stays in a
// Secret. Testnet only: it spends the payer's SBC and the submitter's fees.
//
// Every sample is one JSON line on stdout; the last line is the summary.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/radius"
	"github.com/amoghjay/x402_gateway/internal/x402/eip3009"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "x402-settlebench:", err)
		os.Exit(1)
	}
}

type bench struct {
	rpc       *radius.RPC
	prof      radius.Profile
	submitter *evm.PrivateKey
	payer     *evm.PrivateKey
	payTo     evm.Address
	amount    *big.Int
	gas       uint64
	gasPrice  *big.Int
	wait      time.Duration
	timeout   time.Duration
	preflight bool

	mu    sync.Mutex
	nonce uint64 // next nonce for the submitter
}

// sample is one submission, printed as it completes.
type sample struct {
	I         int     `json:"i"`
	Nonce     uint64  `json:"nonce"`
	Preflight float64 `json:"preflight_ms,omitempty"`
	Latency   float64 `json:"latency_ms"`
	Outcome   string  `json:"outcome"` // ok | reverted | err:<code> | http
	GasUsed   uint64  `json:"gas_used,omitempty"`
	Tx        string  `json:"tx,omitempty"`
	Err       string  `json:"err,omitempty"`
}

func run(args []string) error {
	fs := flag.NewFlagSet("x402-settlebench", flag.ContinueOnError)
	var (
		network  = fs.String("network", "testnet", "testnet only")
		mode     = fs.String("mode", "serial", "serial, burst or reverts")
		n        = fs.Int("n", 100, "samples (serial, burst)")
		interval = fs.Duration("interval", 500*time.Millisecond, "gap between serial samples")
		wait     = fs.Duration("wait", 2*time.Second, "maxWait passed to eth_sendRawTransactionSync (0 = node default)")
		timeout  = fs.Duration("timeout", 5*time.Second, "HTTP timeout per RPC call")
		amount   = fs.String("amount", "1", "atomic units per transfer (1 = 0.000001 SBC)")
		payTo    = fs.String("pay-to", "0x2a521BC1C1fda5C366aEf14A8326152de5EBfBe4", "recipient (the demo seller)")
		gas      = fs.Uint64("gas", 160_000, "gas limit (live transfers use ~141k)")
		pre      = fs.Bool("preflight", false, "serial: eth_call the same calldata first and time it")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	prof, err := radius.Lookup(*network)
	if err != nil {
		return err
	}
	if prof.Name == radius.Mainnet.Name {
		return errors.New("the bench spends; mainnet is refused")
	}
	b := &bench{prof: prof, gas: *gas, wait: *wait, timeout: *timeout, preflight: *pre}
	if b.amount, err = evm.ParseUint256(*amount); err != nil {
		return fmt.Errorf("-amount: %w", err)
	}
	if b.payTo, err = evm.ParseAddress(*payTo); err != nil {
		return fmt.Errorf("-pay-to: %w", err)
	}
	if b.submitter, err = keyFromFile("SUBMITTER_KEY_FILE"); err != nil {
		return err
	}
	if b.payer, err = keyFromFile("PAYER_KEY_FILE"); err != nil {
		return err
	}
	url := prof.RPC
	keyed := false
	if u := os.Getenv("RADIUS_RPC_URL"); u != "" {
		url, keyed = u, true
	}
	b.rpc = &radius.RPC{URL: url, HTTP: radius.NewHTTPClient()}

	ctx := context.Background()
	id, err := b.rpc.ChainID(ctx)
	if err != nil {
		return fmt.Errorf("chain id: %w", err)
	}
	if id != prof.ChainID {
		return fmt.Errorf("chain id %d, want %d", id, prof.ChainID)
	}
	if b.gasPrice, err = b.rpc.GasPrice(ctx); err != nil {
		return err
	}
	if b.nonce, err = b.rpc.TransactionCount(ctx, b.submitter.Address()); err != nil {
		return err
	}
	before, err := b.balances(ctx)
	if err != nil {
		return err
	}
	fmt.Println(jsonLine(map[string]any{
		"start": time.Now().UTC().Format(time.RFC3339), "mode": *mode, "network": prof.Name, "keyed_rpc": keyed,
		"submitter": b.submitter.Address().String(), "payer": b.payer.Address().String(), "nonce": b.nonce,
		"gas_price_wei": b.gasPrice.String(), "wait_ms": b.wait.Milliseconds(), "balances_before": before,
	}))

	var samples []sample
	var extra map[string]any
	switch *mode {
	case "serial":
		samples = b.serial(ctx, *n, *interval)
	case "burst":
		samples = b.burst(ctx, *n)
	case "reverts":
		extra = b.reverts(ctx)
	default:
		return fmt.Errorf("-mode %q", *mode)
	}
	after, err := b.balances(ctx)
	if err != nil {
		return err
	}
	fmt.Println(jsonLine(summary(samples, before, after, extra)))
	return nil
}

func keyFromFile(env string) (*evm.PrivateKey, error) {
	path := os.Getenv(env)
	if path == "" {
		return nil, fmt.Errorf("%s is not set", env)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the operator names the key file on purpose
	if err != nil {
		return nil, err
	}
	k, err := evm.ParsePrivateKey(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", env, err)
	}
	return k, nil
}

// balances reads the submitter's SBC and RUSD (gas) and the payer's SBC, as decimal strings.
func (b *bench) balances(ctx context.Context) (map[string]string, error) {
	sbc, err := b.rpc.BalanceOf(ctx, radius.SBCAddress, b.submitter.Address())
	if err != nil {
		return nil, err
	}
	rusd, err := b.rpc.NativeBalance(ctx, b.submitter.Address())
	if err != nil {
		return nil, err
	}
	payer, err := b.rpc.BalanceOf(ctx, radius.SBCAddress, b.payer.Address())
	if err != nil {
		return nil, err
	}
	return map[string]string{"submitter_sbc": radius.FormatSBC(sbc), "submitter_rusd_wei": rusd.String(), "payer_sbc": radius.FormatSBC(payer)}, nil
}

func (b *bench) domain() eip3009.Domain {
	return eip3009.Domain{Name: radius.SBCName, Version: radius.SBCVersion, ChainID: b.prof.ChainID, VerifyingContract: radius.SBCAddress}
}

// authorize signs a fresh transfer from payer to payTo, valid for the given window
// around now (negative "before" makes an expired one).
func (b *bench) authorize(payer *evm.PrivateKey, after, before time.Duration) (eip3009.Authorization, []byte) {
	now := time.Now()
	a := eip3009.Authorization{
		From: payer.Address(), To: b.payTo, Value: b.amount,
		ValidAfter: big.NewInt(now.Add(after).Unix()), ValidBefore: big.NewInt(now.Add(before).Unix()),
	}
	if _, err := rand.Read(a.Nonce[:]); err != nil {
		panic(err)
	}
	return a, payer.Sign(eip3009.Digest(b.domain(), a))
}

func (b *bench) nextNonce() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := b.nonce
	b.nonce++
	return n
}

// submit signs and sends calldata from the submitter key with the given nonce.
func (b *bench) submit(ctx context.Context, i int, nonce uint64, calldata []byte) sample {
	s := sample{I: i, Nonce: nonce}
	raw, _, err := evm.SignLegacy(b.submitter, b.prof.ChainID, evm.LegacyTx{Nonce: nonce, GasPrice: b.gasPrice, Gas: b.gas, To: radius.SBCAddress, Data: calldata})
	if err != nil {
		s.Outcome, s.Err = "sign", err.Error()
		return s
	}
	cctx, cancel := context.WithTimeout(ctx, b.timeout)
	defer cancel()
	start := time.Now()
	rcpt, err := b.rpc.SendRawTransactionSync(cctx, raw, b.wait)
	s.Latency = ms(time.Since(start))
	if err == nil {
		s.GasUsed, s.Tx = rcpt.GasUsed, rcpt.TransactionHash
		s.Outcome = "reverted"
		if rcpt.Status == 1 {
			s.Outcome = "ok"
		}
		return s
	}
	var rpcErr *radius.RPCError
	if errors.As(err, &rpcErr) {
		s.Outcome = fmt.Sprintf("err:%d", rpcErr.Code)
		s.Err = rpcErr.Error()
		if rpcErr.Code == radius.SyncErrTimeout || rpcErr.Code == radius.SyncErrRejected {
			s.Tx = rpcErr.DataString()
		}
		return s
	}
	s.Outcome, s.Err = "http", err.Error()
	return s
}

// transfer is one valid payment: sign, optionally pre-flight with eth_call, submit.
func (b *bench) transfer(ctx context.Context, i int) sample {
	a, sig := b.authorize(b.payer, -time.Minute, 5*time.Minute)
	calldata, err := a.TransferCalldata(sig)
	if err != nil {
		return sample{I: i, Outcome: "calldata", Err: err.Error()}
	}
	var pre float64
	if b.preflight {
		cctx, cancel := context.WithTimeout(ctx, b.timeout)
		start := time.Now()
		_, err := b.rpc.EthCall(cctx, b.submitter.Address(), radius.SBCAddress, calldata)
		pre = ms(time.Since(start))
		cancel()
		if err != nil {
			return sample{I: i, Preflight: pre, Outcome: "preflight", Err: err.Error()}
		}
	}
	s := b.submit(ctx, i, b.nextNonce(), calldata)
	s.Preflight = pre
	return s
}

func (b *bench) serial(ctx context.Context, n int, interval time.Duration) []sample {
	out := make([]sample, 0, n)
	for i := range n {
		s := b.transfer(ctx, i)
		fmt.Println(jsonLine(s))
		out = append(out, s)
		if s.Outcome == "err:6" { // nonce gap: the node told us what it expects
			b.resync(ctx)
		}
		time.Sleep(interval)
	}
	return out
}

// burst signs n transfers up front, then submits them all at once with consecutive
// nonces, which is the shape a filter under load would produce from one key.
func (b *bench) burst(ctx context.Context, n int) []sample {
	type job struct {
		nonce    uint64
		calldata []byte
	}
	jobs := make([]job, n)
	for i := range jobs {
		a, sig := b.authorize(b.payer, -time.Minute, 5*time.Minute)
		calldata, err := a.TransferCalldata(sig)
		if err != nil {
			panic(err)
		}
		jobs[i] = job{b.nextNonce(), calldata}
	}
	out := make([]sample, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i, j := range jobs {
		wg.Go(func() { out[i] = b.submit(ctx, i, j.nonce, j.calldata) })
	}
	wg.Wait()
	for _, s := range out {
		fmt.Println(jsonLine(s))
	}
	fmt.Println(jsonLine(map[string]any{"burst_wall_ms": ms(time.Since(start)), "n": n}))
	b.resync(ctx)
	return out
}

func (b *bench) resync(ctx context.Context) {
	if n, err := b.rpc.TransactionCount(ctx, b.submitter.Address()); err == nil {
		b.mu.Lock()
		b.nonce = n
		b.mu.Unlock()
	}
}

// reverts runs each failure the settler must classify and records, per case: what
// eth_call says, what the sync submit returns, whether the submitter's nonce moved,
// and what the fee was. The cancelled case first spends a transaction from the payer.
func (b *bench) reverts(ctx context.Context) map[string]any {
	type result struct {
		Preflight     string  `json:"preflight"` // revert reason or "ok"
		PreflightMs   float64 `json:"preflight_ms"`
		Outcome       string  `json:"outcome"`
		Err           string  `json:"err,omitempty"`
		Tx            string  `json:"tx,omitempty"`
		LatencyMs     float64 `json:"latency_ms"`
		NonceConsumed bool    `json:"nonce_consumed"`
		FeeWei        string  `json:"fee_wei"`
	}
	results := map[string]result{}
	unfunded, err := evm.GeneratePrivateKey()
	if err != nil {
		panic(err)
	}
	// reused and cancelled need a prepared authorization; the others are one-shot.
	reusedAuth, reusedSig := b.authorize(b.payer, -time.Minute, 5*time.Minute)
	cancelAuth, cancelSig := b.authorize(b.payer, -time.Minute, 5*time.Minute)

	cases := []struct {
		name string
		prep func() ([]byte, error)
	}{
		{"expired", func() ([]byte, error) {
			a, s := b.authorize(b.payer, -2*time.Minute, -time.Minute)
			return a.TransferCalldata(s)
		}},
		{"not_yet_valid", func() ([]byte, error) {
			a, s := b.authorize(b.payer, time.Hour, 2*time.Hour)
			return a.TransferCalldata(s)
		}},
		{"bad_signature", func() ([]byte, error) {
			a, s := b.authorize(b.payer, -time.Minute, 5*time.Minute)
			s[5] ^= 0xff
			return a.TransferCalldata(s)
		}},
		{"unfunded_payer", func() ([]byte, error) {
			a, s := b.authorize(unfunded, -time.Minute, 5*time.Minute)
			return a.TransferCalldata(s)
		}},
		{"reused_first", func() ([]byte, error) { return reusedAuth.TransferCalldata(reusedSig) }},
		{"reused_second", func() ([]byte, error) { return reusedAuth.TransferCalldata(reusedSig) }},
		{"cancelled", func() ([]byte, error) {
			// The payer revokes the nonce on chain, paying its own gas, then we try to settle it.
			sig := b.payer.Sign(eip3009.CancelDigest(b.domain(), b.payer.Address(), cancelAuth.Nonce))
			data, err := eip3009.CancelCalldata(b.payer.Address(), cancelAuth.Nonce, sig)
			if err != nil {
				return nil, err
			}
			pn, err := b.rpc.TransactionCount(ctx, b.payer.Address())
			if err != nil {
				return nil, err
			}
			raw, _, err := evm.SignLegacy(b.payer, b.prof.ChainID, evm.LegacyTx{Nonce: pn, GasPrice: b.gasPrice, Gas: b.gas, To: radius.SBCAddress, Data: data})
			if err != nil {
				return nil, err
			}
			rcpt, err := b.rpc.SendRawTransactionSync(ctx, raw, b.wait)
			if err != nil {
				return nil, fmt.Errorf("cancel tx: %w", err)
			}
			fmt.Println(jsonLine(map[string]any{"case": "cancelled", "cancel_tx": rcpt.TransactionHash, "cancel_status": rcpt.Status, "cancel_gas": rcpt.GasUsed}))
			return cancelAuth.TransferCalldata(cancelSig)
		}},
	}
	for _, c := range cases {
		var r result
		calldata, err := c.prep()
		if err != nil {
			r.Outcome, r.Err = "prep", err.Error()
			results[c.name] = r
			fmt.Println(jsonLine(map[string]any{"case": c.name, "result": r}))
			continue
		}
		start := time.Now()
		_, err = b.rpc.EthCall(ctx, b.submitter.Address(), radius.SBCAddress, calldata)
		r.PreflightMs = ms(time.Since(start))
		r.Preflight = "ok"
		var rpcErr *radius.RPCError
		if errors.As(err, &rpcErr) {
			r.Preflight = rpcErr.Error()
		} else if err != nil {
			r.Preflight = err.Error()
		}
		nonceBefore, _ := b.rpc.TransactionCount(ctx, b.submitter.Address())
		rusdBefore, _ := b.rpc.NativeBalance(ctx, b.submitter.Address())
		sbcBefore, _ := b.rpc.BalanceOf(ctx, radius.SBCAddress, b.submitter.Address())
		s := b.submit(ctx, 0, nonceBefore, calldata)
		r.Outcome, r.Err, r.Tx, r.LatencyMs = s.Outcome, s.Err, s.Tx, s.Latency
		nonceAfter, _ := b.rpc.TransactionCount(ctx, b.submitter.Address())
		rusdAfter, _ := b.rpc.NativeBalance(ctx, b.submitter.Address())
		sbcAfter, _ := b.rpc.BalanceOf(ctx, radius.SBCAddress, b.submitter.Address())
		r.NonceConsumed = nonceAfter > nonceBefore
		r.FeeWei = feeDelta(rusdBefore, rusdAfter, sbcBefore, sbcAfter)
		results[c.name] = r
		fmt.Println(jsonLine(map[string]any{"case": c.name, "result": r}))
		b.resync(ctx)
	}
	return map[string]any{"reverts": results}
}

// feeDelta is what the submitter lost across one submission, in RUSD wei: the RUSD
// drop plus any SBC the Turnstile converted (SBC has 6 decimals, RUSD 18).
func feeDelta(rusdBefore, rusdAfter, sbcBefore, sbcAfter *big.Int) string {
	if rusdBefore == nil || rusdAfter == nil || sbcBefore == nil || sbcAfter == nil {
		return "?"
	}
	d := new(big.Int).Sub(rusdBefore, rusdAfter)
	sbc := new(big.Int).Sub(sbcBefore, sbcAfter)
	sbc.Mul(sbc, big.NewInt(1_000_000_000_000)) // 1e12: SBC units → wei scale
	return d.Add(d, sbc).String()
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func jsonLine(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(b)
}

// summary is the last line: latency quantiles for successful settles, the share over
// the budgets we care about, outcome counts, and the fee per settle from the balances.
func summary(samples []sample, before, after map[string]string, extra map[string]any) map[string]any {
	out := map[string]any{"end": time.Now().UTC().Format(time.RFC3339), "balances_before": before, "balances_after": after}
	for k, v := range extra {
		out[k] = v
	}
	if len(samples) == 0 {
		return out
	}
	counts := map[string]int{}
	var ok, pre []float64
	var gas uint64
	for _, s := range samples {
		counts[s.Outcome]++
		if s.Outcome == "ok" {
			ok = append(ok, s.Latency)
			gas += s.GasUsed
		}
		if s.Preflight > 0 {
			pre = append(pre, s.Preflight)
		}
	}
	out["n"], out["outcomes"] = len(samples), counts
	if len(ok) > 0 {
		out["settle_ms"] = quantiles(ok)
		out["over_budget"] = map[string]string{
			"150ms": share(ok, 150), "250ms": share(ok, 250), "400ms": share(ok, 400),
		}
		out["gas_used_avg"] = gas / uint64(len(ok))
	}
	if len(pre) > 0 {
		out["preflight_ms"] = quantiles(pre)
	}
	return out
}

func quantiles(v []float64) map[string]float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	q := func(p float64) float64 { return s[min(len(s)-1, int(float64(len(s))*p))] }
	return map[string]float64{"p50": q(0.5), "p90": q(0.9), "p99": q(0.99), "max": s[len(s)-1], "min": s[0]}
}

func share(v []float64, limit float64) string {
	n := 0
	for _, x := range v {
		if x > limit {
			n++
		}
	}
	return fmt.Sprintf("%.2f%%", 100*float64(n)/float64(len(v)))
}
