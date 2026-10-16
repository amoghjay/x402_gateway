package bench

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

func typed() apitypes.TypedData {
	return apitypes.TypedData{
		Types: apitypes.Types{
			"EIP712Domain":              {{Name: "name", Type: "string"}, {Name: "chainId", Type: "uint256"}, {Name: "verifyingContract", Type: "address"}},
			"PermitWitnessTransferFrom": {{Name: "permitted", Type: "TokenPermissions"}, {Name: "spender", Type: "address"}, {Name: "nonce", Type: "uint256"}, {Name: "deadline", Type: "uint256"}, {Name: "witness", Type: "Witness"}},
			"TokenPermissions":          {{Name: "token", Type: "address"}, {Name: "amount", Type: "uint256"}},
			"Witness":                   {{Name: "to", Type: "address"}, {Name: "validAfter", Type: "uint256"}},
		},
		PrimaryType: "PermitWitnessTransferFrom",
		Domain:      apitypes.TypedDataDomain{Name: "Permit2", ChainId: math.NewHexOrDecimal256(72344), VerifyingContract: "0x000000000022D473030F116dDEE9F6B43aC78BA3"},
		Message: apitypes.TypedDataMessage{
			"permitted": map[string]interface{}{"token": "0x33ad9e4BD16B69B5BFdED37D8B5D9fF9aba014Fb", "amount": "1000"},
			"spender":   "0x402085c248EeA27D92E8b30b2C58ed07f9E20001",
			"nonce":     "123456789",
			"deadline":  "1792160000",
			"witness":   map[string]interface{}{"to": "0x402085c248EeA27D92E8b30b2C58ed07f9E20001", "validAfter": "0"},
		},
	}
}

func BenchmarkVerifyPermit2(b *testing.B) {
	key, _ := crypto.GenerateKey()
	want := crypto.PubkeyToAddress(key.PublicKey)
	hash, _, err := apitypes.TypedDataAndHash(typed())
	if err != nil {
		b.Fatal(err)
	}
	sig, _ := crypto.Sign(hash, key)
	_ = big.NewInt(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		h, _, err := apitypes.TypedDataAndHash(typed())
		if err != nil {
			b.Fatal(err)
		}
		pub, err := crypto.SigToPub(h, sig)
		if err != nil || crypto.PubkeyToAddress(*pub) != want {
			b.Fatal("mismatch")
		}
	}
	_ = common.Address{}
}
