package main

import (
	"maps"
	"testing"

	"github.com/amoghjay/x402_gateway/internal/radius"
)

func TestFacilitatorURLs(t *testing.T) {
	one := []radius.Profile{radius.Testnet}
	two := []radius.Profile{radius.Testnet, radius.BaseSepolia}
	for name, c := range map[string]struct {
		v    string
		nets []radius.Profile
		want map[string]string // nil = an error
	}{
		"unset":                      {"", two, map[string]string{}},
		"bare URL, one network":      {"http://f:8080", one, map[string]string{"testnet": "http://f:8080"}},
		"bare URL with a query":      {"https://f/x?k=v", one, map[string]string{"testnet": "https://f/x?k=v"}},
		"bare URL, two networks":     {"http://f:8080", two, nil},
		"one pair, two networks":     {"testnet=http://f:8080", two, map[string]string{"testnet": "http://f:8080"}},
		"two pairs":                  {"testnet=http://a, base-sepolia=https://b", two, map[string]string{"testnet": "http://a", "base-sepolia": "https://b"}},
		"pair with a query":          {"testnet=https://f/x?k=v", two, map[string]string{"testnet": "https://f/x?k=v"}},
		"network not configured":     {"mainnet=http://f", two, nil},
		"typo in the network":        {"tesnet=http://f", two, nil},
		"same network twice":         {"testnet=http://a,testnet=http://b", two, nil},
		"not a URL":                  {"testnet=f:8080", two, nil},
		"bare URL mixed into a list": {"http://a,testnet=http://b", one, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := facilitatorURLs(c.v, c.nets)
			if c.want == nil {
				if err == nil {
					t.Fatalf("got %v, want an error", got)
				}
				return
			}
			if err != nil || !maps.Equal(got, c.want) {
				t.Fatalf("got %v, %v; want %v", got, err, c.want)
			}
		})
	}
}
