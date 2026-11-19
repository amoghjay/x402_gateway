// Package radius holds the network profiles: everything chain-specific the filter, the
// settler and the buyer need (chain id, RPC, facilitator, and the token routes are priced
// in, with its EIP-712 domain), so that another network is a profile and no address or
// URL is hard-coded anywhere else. The package keeps its name from when Radius was the
// only chain; Base Sepolia is here too, and any EVM chain whose stablecoin has EIP-3009
// and whose facilitator speaks x402 v2 is one more Profile.
//
// The Radius values are checked against both live networks by live_test.go; the rest of
// this comment is about them.
//
// Every value here is checked against both live networks by live_test.go
// (`make verify-radius`): chain id, bytecode at the three addresses, SBC's name and
// decimals, its EIP-712 domain through ERC-5267 and DOMAIN_SEPARATOR(), the EIP-3009
// typehash and authorizationState, the facilitators' /health and /supported, the
// explorers and the faucet. Last run green on 2026-10-30. Radius's docs say SBC lacks
// EIP-3009; the chain says otherwise, so trust the test, not the docs. SBC, Permit2 and the x402 Permit2
// proxy have identical addresses on both networks.
package radius

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/amoghjay/x402_gateway/internal/evm"
	"github.com/amoghjay/x402_gateway/internal/x402"
)

// Contracts shared by both networks.
const (
	// SBC is the Radius stablecoin, 6 decimals. It exposes EIP-3009 (checked: settlements
	// on testnet, authorizationState on mainnet) and has no EIP-1271, so ecrecover is a
	// complete signature check.
	SBC = "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb"
	// SBCName and SBCVersion are SBC's EIP-712 domain, as both facilitators advertise it.
	// SBC's version() reverts on chain, so these come from the facilitator, not the token.
	SBCName    = "Stable Coin"
	SBCVersion = "1"
	// SBCDecimals: "1000" atomic units is 0.001 SBC.
	SBCDecimals = 6

	// Permit2 is the canonical Permit2 contract; ExactPermit2Proxy is the x402 proxy that
	// pins the recipient. Both are backlog (Permit2 transfer method).
	Permit2           = "0x000000000022D473030F116dDEE9F6B43aC78BA3"
	ExactPermit2Proxy = "0x402085c248EeA27D92E8b30b2C58ed07f9E20001"
)

// How a buyer pays, for the 402's how-to-pay hints (checked 2026-11-04: npm radius-cli
// 0.7.1 from radiustechsystems/tools; the docs URL is where the old
// /developer-resources/x402-integration redirects).
const (
	X402Docs     = "https://docs.radiustech.xyz/build/x402/"
	CLIInstall   = "npm i -g radius-cli"
	SDKPackage   = "radius-sdk"
	SDKImport    = "radius-sdk/client"
	SDKEntry     = "createRadiusFetch"
	DisplayAsset = "SBC"
)

// Token is the stablecoin a network's routes are priced in. It must expose EIP-3009
// (transferWithAuthorization, authorizationState). Name and Version are its EIP-712
// domain, which a buyer signs against and the filter recovers against: a wrong value
// here makes every signature look forged.
type Token struct {
	Symbol   string // for people: "SBC", "USDC"
	Address  string
	Name     string
	Version  string
	Decimals int
}

// Format renders atomic units as a decimal amount ("1000" → "0.001" at 6 decimals).
func (t Token) Format(atomic *big.Int) string {
	s := atomic.String()
	if len(s) <= t.Decimals {
		s = strings.Repeat("0", t.Decimals-len(s)+1) + s
	}
	whole, frac := s[:len(s)-t.Decimals], strings.TrimRight(s[len(s)-t.Decimals:], "0")
	if frac == "" {
		return whole
	}
	return whole + "." + frac
}

// SBCToken is SBC as a Token: the same contract and domain on both Radius networks.
var SBCToken = Token{Symbol: DisplayAsset, Address: SBC, Name: SBCName, Version: SBCVersion, Decimals: SBCDecimals}

// Profile is one network.
type Profile struct {
	Name        string // the name a PaymentPolicy and the -networks flag use
	Display     string // for people: "Radius testnet"
	ChainID     uint64
	RPC         string // JSON-RPC over HTTPS; on Radius an API key is appended to the path
	Facilitator string // x402 facilitator base URL (/verify, /settle, /supported)
	Faucet      string // "" where there is none
	Explorer    string // base URL for TxURL
	Token       Token
	Docs        string // where the 402 sends a buyer to read how to pay
	RadiusCLI   bool   // radius-cli and radius-sdk can pay here; the 402 says how
}

// Testnet is Radius testnet.
var Testnet = Profile{
	Name:        "testnet",
	Display:     "Radius testnet",
	ChainID:     72344,
	RPC:         "https://rpc.testnet.radiustech.xyz",
	Facilitator: "https://facilitator.testnet.radiustech.xyz",
	Faucet:      "https://testnet.radiustech.xyz/api/v1/faucet",
	Explorer:    "https://testnet.radiustech.xyz",
	Token:       SBCToken,
	Docs:        X402Docs,
	RadiusCLI:   true,
}

// Mainnet is Radius mainnet. Used only after the Phase 6 soak gate.
var Mainnet = Profile{
	Name:        "mainnet",
	Display:     "Radius mainnet",
	ChainID:     723487,
	RPC:         "https://rpc.radiustech.xyz",
	Facilitator: "https://facilitator.radiustech.xyz",
	Explorer:    "https://network.radiustech.xyz",
	Token:       SBCToken,
	Docs:        X402Docs,
	RadiusCLI:   true,
}

// BaseSepolia is Base's testnet, priced in Circle's test USDC, settled through the
// x402 Foundation's public test facilitator (no account). Checked live on 2026-11-19:
// eth_chainId is 84532; the token's name() is "USDC", version() "2", decimals() 6; the
// facilitator's /supported lists exact on eip155:84532 for x402 v2. The public RPC is
// rate-limited: fine for drills, not for load.
var BaseSepolia = Profile{
	Name:        "base-sepolia",
	Display:     "Base Sepolia",
	ChainID:     84532,
	RPC:         "https://sepolia.base.org",
	Facilitator: "https://x402.org/facilitator",
	Explorer:    "https://sepolia.basescan.org",
	Token:       Token{Symbol: "USDC", Address: "0x036CbD53842c5426634e7929541eC2318f3dCF7e", Name: "USDC", Version: "2", Decimals: 6},
	Docs:        "https://docs.x402.org",
}

// Profiles is every network this build knows.
var Profiles = []Profile{Testnet, Mainnet, BaseSepolia}

// Lookup returns the profile for a name ("testnet", "mainnet", "base-sepolia").
func Lookup(name string) (Profile, error) {
	for _, p := range Profiles {
		if p.Name == name {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("radius: unknown network %q", name)
}

// ByNetwork returns the profile for a CAIP-2 identifier ("eip155:72344").
func ByNetwork(caip2 string) (Profile, error) {
	for _, p := range Profiles {
		if p.Network() == caip2 {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("radius: unknown network %q", caip2)
}

// TokenAddress is the token's contract as a typed address.
func (p Profile) TokenAddress() evm.Address { a, _ := evm.ParseAddress(p.Token.Address); return a }

// Network is the CAIP-2 identifier the x402 wire uses.
func (p Profile) Network() string { return fmt.Sprintf("eip155:%d", p.ChainID) }

// TxURL links a transaction hash on the explorer.
func (p Profile) TxURL(hash string) string { return p.Explorer + "/tx/" + hash }

// ExactRequirements builds the `exact` EIP-3009 requirements for a price on this
// network, in its token: the kind its facilitator advertises under /supported.
func (p Profile) ExactRequirements(amount, payTo string, maxTimeoutSeconds int) x402.Requirements {
	return x402.Requirements{
		Scheme:            x402.SchemeExact,
		Network:           p.Network(),
		Amount:            amount,
		Asset:             p.Token.Address,
		PayTo:             payTo,
		MaxTimeoutSeconds: maxTimeoutSeconds,
		Extra:             &x402.ExactExtra{AssetTransferMethod: x402.TransferEIP3009, Name: p.Token.Name, Version: p.Token.Version},
	}
}
