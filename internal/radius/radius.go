// Package radius holds the Radius network profiles: everything chain-specific the filter,
// the settler and the buyer need, so that mainnet is a configuration change and no
// address or URL is hard-coded anywhere else.
//
// Values were checked live (eth_getCode, facilitator /supported) on 2026-10-16 and
// 2026-10-30 and match the radius-dev skill's network table; re-verify before a mainnet
// cutover. SBC, Permit2 and the x402 Permit2
// proxy have identical addresses on both networks.
package radius

import (
	"fmt"

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

// Profile is one Radius network.
type Profile struct {
	Name        string
	ChainID     uint64
	RPC         string // JSON-RPC over HTTPS; an API key is appended to the path
	Facilitator string // x402 facilitator base URL (/verify, /settle, /supported)
	Faucet      string // "" where there is none
	Explorer    string // base URL for TxURL
}

// Testnet is Radius testnet.
var Testnet = Profile{
	Name:        "testnet",
	ChainID:     72344,
	RPC:         "https://rpc.testnet.radiustech.xyz",
	Facilitator: "https://facilitator.testnet.radiustech.xyz",
	Faucet:      "https://testnet.radiustech.xyz/api/v1/faucet",
	Explorer:    "https://testnet.radiustech.xyz",
}

// Mainnet is Radius mainnet. Used only after the Phase 6 soak gate.
var Mainnet = Profile{
	Name:        "mainnet",
	ChainID:     723487,
	RPC:         "https://rpc.radiustech.xyz",
	Facilitator: "https://facilitator.radiustech.xyz",
	Explorer:    "https://network.radiustech.xyz",
}

// Lookup returns the profile for a name ("testnet" or "mainnet").
func Lookup(name string) (Profile, error) {
	switch name {
	case Testnet.Name:
		return Testnet, nil
	case Mainnet.Name:
		return Mainnet, nil
	}
	return Profile{}, fmt.Errorf("radius: unknown network %q", name)
}

// Network is the CAIP-2 identifier the x402 wire uses.
func (p Profile) Network() string { return fmt.Sprintf("eip155:%d", p.ChainID) }

// TxURL links a transaction hash on the explorer.
func (p Profile) TxURL(hash string) string { return p.Explorer + "/tx/" + hash }

// ExactRequirements builds the `exact` EIP-3009 requirements for a price on this
// network: the same kind both Radius facilitators advertise under /supported.
func (p Profile) ExactRequirements(amount, payTo string, maxTimeoutSeconds int) x402.Requirements {
	return x402.Requirements{
		Scheme:            x402.SchemeExact,
		Network:           p.Network(),
		Amount:            amount,
		Asset:             SBC,
		PayTo:             payTo,
		MaxTimeoutSeconds: maxTimeoutSeconds,
		Extra:             &x402.ExactExtra{AssetTransferMethod: x402.TransferEIP3009, Name: SBCName, Version: SBCVersion},
	}
}
