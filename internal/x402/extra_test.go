package x402

import (
	"encoding/json"
	"strings"
	"testing"
)

// ExtraRaw replaces Extra on the wire, so a field this package does not model reaches the
// buyer and the facilitator; without it, Extra is encoded as before.
func TestRequirementsExtraRaw(t *testing.T) {
	r := Requirements{Scheme: "exact", Network: "eip155:1", Amount: "1", Asset: "0xa", PayTo: "0xb", MaxTimeoutSeconds: 60,
		Extra: &ExactExtra{AssetTransferMethod: "eip3009", Name: "N", Version: "1"}}
	plain, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(plain), `"extra":{"assetTransferMethod":"eip3009","name":"N","version":"1"}`) {
		t.Fatalf("without ExtraRaw: %s, %v", plain, err)
	}
	r.ExtraRaw = `{"name":"N","version":"1","new":[1,2]}`
	out, err := json.Marshal(r)
	if err != nil || strings.Count(string(out), `"extra"`) != 1 || !strings.Contains(string(out), `"extra":{"name":"N","version":"1","new":[1,2]}`) {
		t.Fatalf("with ExtraRaw: %s, %v", out, err)
	}
	// Inside a PaymentRequired too.
	pr, _ := json.Marshal(PaymentRequired{X402Version: 2, Accepts: []Requirements{r}})
	if !strings.Contains(string(pr), `"new":[1,2]`) {
		t.Errorf("PaymentRequired: %s", pr)
	}
	// Decoding reads extra into Extra, never ExtraRaw.
	var back Requirements
	if err := json.Unmarshal(out, &back); err != nil || back.ExtraRaw != "" || back.Extra == nil || back.Extra.Name != "N" {
		t.Errorf("decoded %+v, %v", back, err)
	}
}
