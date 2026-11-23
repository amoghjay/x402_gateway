package webbotauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// The RFC 9421 Appendix B.1.4 test key. Its RFC 7638 thumbprint is the keyid the Web Bot Auth
// draft's examples use.
const (
	testSeed  = "n4Ni-HpISpVObnQMW0wOhCKROaIKqKtW_2ZYb2p9KcU"
	testX     = "JrQLj5P_89iXES9-vFgrIy29clF9CC_oPPsw3c5D0bs"
	testThumb = "poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U"
)

func testKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	seed, err := base64.RawURLEncoding.DecodeString(testSeed)
	if err != nil {
		t.Fatal(err)
	}
	k := ed25519.NewKeyFromSeed(seed)
	if got := base64.RawURLEncoding.EncodeToString(k.Public().(ed25519.PublicKey)); got != testX {
		t.Fatalf("test key public half %s, want %s", got, testX)
	}
	return k
}

func otherKey() (ed25519.PrivateKey, string) {
	k := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	return k, base64.RawURLEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

func dirsOf(m map[string]*Directory) Directories {
	return func(origin string) (*Directory, bool) {
		d, ok := m[origin]
		return d, ok
	}
}

func request(h map[string]string) Request {
	return Request{
		Method: "GET", Scheme: "https", Authority: "example.com", Target: "/build?x=1",
		Header: func(name string) (string, bool) { v, ok := h[name]; return v, ok },
	}
}

// sign builds Signature-Input and Signature for one label over r, as an agent would.
func sign(t *testing.T, key ed25519.PrivateKey, r Request, label, components string) (string, string) {
	t.Helper()
	d, err := ParseDictionary(label + "=" + components)
	if err != nil {
		t.Fatal(err)
	}
	base, err := signatureBase(r, d[0].Value)
	if err != nil {
		t.Fatalf("signature base: %v", err)
	}
	sig := ed25519.Sign(key, []byte(base))
	return label + "=" + components, label + "=:" + base64.StdEncoding.EncodeToString(sig) + ":"
}

// The Web Bot Auth WG draft's vectors (draft-ietf-webbotauth-httpsig-protocol-01, Appendix
// E.2), signed with the B.1.4 key for @authority example.com.
func TestDraftVectors(t *testing.T) {
	dirs := dirsOf(map[string]*Directory{"https://signature-agent.test": {Keys: []Key{{Kid: testThumb, Thumbprint: testThumb, X: testX}}}})
	cases := []struct {
		name string
		h    map[string]string
		now  int64
		lim  Limits
	}{
		{"E.2.1 dictionary form", map[string]string{
			"signature-agent": `sig2="https://signature-agent.test"`,
			"signature-input": `sig2=("@authority" "signature-agent";key="sig2");created=1735689600;keyid="poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U";alg="ed25519";expires=4889289600;nonce="n9p433xm+NJ3ph3upfBIGmsuwHw387YV7Q/F+6BSpGCVjYCqQw6rznNA8PVVLySrAWsv0hQtFioQb6E1YsauiA==";tag="web-bot-auth"`,
			"signature":       `sig2=:CGokAoY/FrPpyuzTe1Fb2kLTAH9Idw6qSFd1+D77r1firsMRCxeuLu5e6O+ek3zQgDMAzjiXtLVfqE00xnABCQ==:`,
		}, 1735689700, Limits{Skew: time.Minute, MaxLifetime: 200 * 365 * 24 * time.Hour}},
		{"E.2.2 string form", map[string]string{
			"signature-agent": `"https://signature-agent.test"`,
			"signature-input": `sig2=("@authority" "signature-agent");created=1735689600;keyid="poqkLGiymh_W0uP6PZFw-dvez3QJT5SolqXBCW38r0U";alg="ed25519";expires=1735693200;nonce="e8N7S2MFd/qrd6T2R3tdfAuuANngKI7LFtKYI/vowzk4lAZYadIX6wW25MwG7DCT9RUKAJ0qVkU0mEeLElW1qg==";tag="web-bot-auth"`,
			"signature":       `sig2=:jdq0SqOwHdyHr9+r5jw3iYZH6aNGKijYp/EstF4RQTQdi5N5YYKrD+mCT1HA1nZDsi6nJKuHxUi/5Syp3rLWBA==:`,
		}, 1735690000, DefaultLimits},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Verify(request(c.h), dirs, time.Unix(c.now, 0), c.lim)
			if got != "https://signature-agent.test" {
				t.Fatalf("Verify = %q, %v", got, err)
			}
		})
	}
}

const chatgpt = "https://chatgpt.com"

// signed is a request signed the way ChatGPT's agent signs: the string form of
// Signature-Agent, an hour's validity, @authority @method @path and signature-agent covered.
func signed(t *testing.T, key ed25519.PrivateKey, now int64, mutate func(map[string]string)) (Request, map[string]string) {
	t.Helper()
	h := map[string]string{"signature-agent": `"` + chatgpt + `"`}
	if mutate != nil {
		mutate(h)
	}
	r := request(h)
	comps := h["components"]
	if comps == "" {
		comps = `("@authority" "@method" "@path" "signature-agent");created=` + int64String(now) + `;expires=` + int64String(now+3600) +
			`;keyid="` + testThumb + `";alg="ed25519";nonce="abc";tag="web-bot-auth"`
	}
	delete(h, "components")
	h["signature-input"], h["signature"] = sign(t, key, r, "sig1", comps)
	return r, h
}

func int64String(i int64) string {
	var b strings.Builder
	serializeBare(&b, i)
	return b.String()
}

func TestVerify(t *testing.T) {
	key := testKey(t)
	other, otherX := otherKey()
	now := int64(1795462428)
	at := time.Unix(now, 0)
	openai := &Directory{Keys: []Key{{Kid: testThumb, Thumbprint: testThumb, X: testX}}}
	dirs := dirsOf(map[string]*Directory{chatgpt: openai})

	verify := func(r Request) (string, error) { return Verify(r, dirs, at, DefaultLimits) }

	t.Run("ChatGPT-shaped request verifies", func(t *testing.T) {
		r, _ := signed(t, key, now, nil)
		if got, err := verify(r); got != chatgpt {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("an uncovered path is not checked: a signature is a bearer token for its host", func(t *testing.T) {
		r, h := signed(t, key, now, func(h map[string]string) {
			h["components"] = `("@authority" "signature-agent");created=` + int64String(now) + `;expires=` + int64String(now+60) + `;keyid="` + testThumb + `"`
		})
		r2 := request(h)
		r2.Target = "/somewhere/else"
		if got, _ := verify(r); got != chatgpt {
			t.Fatal("original did not verify")
		}
		if got, _ := verify(r2); got != chatgpt {
			t.Fatal("other path did not verify")
		}
	})

	t.Run("a covered path changed fails", func(t *testing.T) {
		_, h := signed(t, key, now, nil)
		r := request(h)
		r.Target = "/other"
		if got, _ := verify(r); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("authority is compared lower-cased without the default port", func(t *testing.T) {
		_, h := signed(t, key, now, nil)
		r := request(h)
		r.Authority = "Example.COM:443"
		if got, err := verify(r); got != chatgpt {
			t.Fatalf("got %q, %v", got, err)
		}
		r.Authority = "example.com:8443"
		if got, _ := verify(r); got != "" {
			t.Fatal("another port verified")
		}
	})

	t.Run("dictionary form with Google's label and short kid", func(t *testing.T) {
		google := "https://agent.bot.goog"
		gdirs := dirsOf(map[string]*Directory{google: {Keys: []Key{
			{Kid: "mhxuPw", Thumbprint: Thumbprint(otherX), X: otherX},
			{Kid: "DYiMjA", Thumbprint: testThumb, X: testX},
		}}})
		h := map[string]string{"signature-agent": `g="` + google + `"`}
		r := request(h)
		h["signature-input"], h["signature"] = sign(t, key, r, "g",
			`("@authority" "signature-agent";key="g");created=`+int64String(now)+`;expires=`+int64String(now+300)+`;keyid="DYiMjA";tag="web-bot-auth"`)
		if got, err := Verify(r, gdirs, at, DefaultLimits); got != google {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("a key found by thumbprint when its kid differs", func(t *testing.T) {
		d := dirsOf(map[string]*Directory{chatgpt: {Keys: []Key{{Kid: "short", Thumbprint: testThumb, X: testX}}}})
		r, _ := signed(t, key, now, nil)
		if got, err := Verify(r, d, at, DefaultLimits); got != chatgpt {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("an origin with no directory is not verified", func(t *testing.T) {
		r, _ := signed(t, key, now, nil)
		if got, _ := Verify(r, dirsOf(nil), at, DefaultLimits); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("keys are looked up under the origin the header names", func(t *testing.T) {
		// The signer's key is published by another origin only; naming chatgpt.com fails.
		d := dirsOf(map[string]*Directory{
			chatgpt:                {Keys: []Key{{Kid: testThumb, Thumbprint: Thumbprint(otherX), X: otherX}}},
			"https://evil.example": {Keys: []Key{{Kid: testThumb, Thumbprint: testThumb, X: testX}}},
		})
		r, _ := signed(t, key, now, nil)
		if got, _ := Verify(r, d, at, DefaultLimits); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("a signature by another key fails", func(t *testing.T) {
		r, _ := signed(t, other, now, nil)
		if got, _ := verify(r); got != "" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("the second signature verifies when the first does not", func(t *testing.T) {
		h := map[string]string{"signature-agent": `a="https://unknown.example", b="` + chatgpt + `"`}
		r := request(h)
		inA, sigA := sign(t, key, r, "a", `("@authority" "signature-agent";key="a");created=`+int64String(now)+`;expires=`+int64String(now+60)+`;keyid="`+testThumb+`"`)
		inB, sigB := sign(t, key, r, "b", `("@authority" "signature-agent";key="b");created=`+int64String(now)+`;expires=`+int64String(now+60)+`;keyid="`+testThumb+`"`)
		h["signature-input"], h["signature"] = inA+", "+inB, sigA+", "+sigB
		if got, err := verify(r); got != chatgpt {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	refused := []struct {
		name       string
		reason     string
		components string
		mutate     func(map[string]string)
		key        ed25519.PrivateKey
	}{
		{name: "@authority not covered", reason: "@authority is not covered", components: `("@method" "@path");created=%c;expires=%e;keyid="%k"`},
		{name: "expired", reason: "expired", components: `("@authority");created=%c-7200;expires=%c-3600;keyid="%k"`},
		{name: "created in the future", reason: "created in the future", components: `("@authority");created=%c+120;expires=%c+600;keyid="%k"`},
		{name: "valid for more than an hour", reason: "lifetime out of bounds", components: `("@authority");created=%c;expires=%c+3601;keyid="%k"`},
		{name: "expires before created", reason: "lifetime out of bounds", components: `("@authority");created=%c;expires=%c-1;keyid="%k"`},
		{name: "no expires", reason: "keyid, created and expires are required", components: `("@authority");created=%c;keyid="%k"`},
		{name: "no keyid", reason: "keyid, created and expires are required", components: `("@authority");created=%c;expires=%e`},
		{name: "another alg", reason: "only ed25519", components: `("@authority");created=%c;expires=%e;keyid="%k";alg="rsa-pss-sha512"`},
		{name: "an unknown parameter", reason: "unknown parameter", components: `("@authority");created=%c;expires=%e;keyid="%k";foo=1`},
		{name: "an unsupported derived component", reason: "@status not supported", components: `("@authority" "@status");created=%c;expires=%e;keyid="%k"`},
		{name: "a component with an unsupported parameter", reason: "parameters not supported", components: `("@authority" "signature-agent";sf);created=%c;expires=%e;keyid="%k"`},
		{name: "a covered header that is absent", reason: "header absent", components: `("@authority" "x-missing");created=%c;expires=%e;keyid="%k"`},
		{name: "an unknown keyid", reason: "no key \"nope\"", components: `("@authority");created=%c;expires=%e;keyid="nope"`},
		{name: "a Signature-Agent member with parameters", reason: "not a plain string", components: `("@authority");created=%c;expires=%e;keyid="%k"`,
			mutate: func(h map[string]string) { h["signature-agent"] = `sig1="` + chatgpt + `";type=jwks_uri` }},
		{name: "an http origin", reason: "not an https origin", components: `("@authority");created=%c;expires=%e;keyid="%k"`,
			mutate: func(h map[string]string) { h["signature-agent"] = `"http://chatgpt.com"` }},
		{name: "an origin with a path", reason: "not an https origin", components: `("@authority");created=%c;expires=%e;keyid="%k"`,
			mutate: func(h map[string]string) { h["signature-agent"] = `"https://chatgpt.com/agent"` }},
		{name: "no member for the label", reason: "no signature-agent member", components: `("@authority");created=%c;expires=%e;keyid="%k"`,
			mutate: func(h map[string]string) { h["signature-agent"] = `other="` + chatgpt + `"` }},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			comps := strings.NewReplacer("%c-7200", int64String(now-7200), "%c-3600", int64String(now-3600),
				"%c+120", int64String(now+120), "%c+600", int64String(now+600), "%c+3601", int64String(now+3601),
				"%c-1", int64String(now-1), "%c", int64String(now), "%e", int64String(now+60), "%k", testThumb).Replace(c.components)
			h := map[string]string{"signature-agent": `"` + chatgpt + `"`, "x-present": "1"}
			if c.mutate != nil {
				c.mutate(h)
			}
			r := request(h)
			d, err := ParseDictionary("sig1=" + comps)
			if err != nil {
				t.Fatal(err)
			}
			// Sign whatever base can be built, so the refusal is the rule's, not a bad signature.
			if base, err := signatureBase(r, d[0].Value); err == nil {
				h["signature"] = "sig1=:" + base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(base))) + ":"
			} else {
				h["signature"] = "sig1=:" + base64.StdEncoding.EncodeToString(make([]byte, 64)) + ":"
			}
			h["signature-input"] = "sig1=" + comps
			got, err := verify(r)
			if got != "" || err == nil || !strings.Contains(err.Error(), c.reason) {
				t.Fatalf("got %q, err %v; want refusal %q", got, err, c.reason)
			}
		})
	}

	t.Run("more than MaxSignatures is not tried", func(t *testing.T) {
		r, h := signed(t, key, now, nil)
		extra := make([]string, MaxSignatures)
		for i := range extra {
			extra[i] = "x" + int64String(int64(i)) + `=("@authority");created=1;expires=2;keyid="k"`
		}
		h["signature-input"] = strings.Join(extra, ", ") + ", " + h["signature-input"]
		if got, err := verify(r); got != "" || err == nil || !strings.Contains(err.Error(), "at most") {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("within the skew is accepted", func(t *testing.T) {
		r, _ := signed(t, key, now+50, nil)
		if got, err := verify(r); got != chatgpt {
			t.Fatalf("got %q, %v", got, err)
		}
	})

	t.Run("a key outside its nbf/exp is not used", func(t *testing.T) {
		for _, k := range []Key{
			{Kid: testThumb, Thumbprint: testThumb, X: testX, NotBefore: now + 3600},
			{Kid: testThumb, Thumbprint: testThumb, X: testX, Expires: now - 3600},
		} {
			d := dirsOf(map[string]*Directory{chatgpt: {Keys: []Key{k}}})
			r, _ := signed(t, key, now, nil)
			if got, _ := Verify(r, d, at, DefaultLimits); got != "" {
				t.Fatalf("key %+v: got %q", k, got)
			}
		}
	})

	t.Run("unsigned or malformed reads as no agent", func(t *testing.T) {
		for _, h := range []map[string]string{
			{},
			{"signature-agent": `"` + chatgpt + `"`, "signature-input": "sig1=(", "signature": "sig1=:AA==:"},
			{"signature-agent": "not valid", "signature-input": `sig1=("@authority")`, "signature": "sig1=:AA==:"},
		} {
			if got, _ := verify(request(h)); got != "" {
				t.Fatalf("%v: got %q", h, got)
			}
		}
	})
}

func TestOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://chatgpt.com":       "https://chatgpt.com",
		"https://ChatGPT.com/":      "https://chatgpt.com",
		"https://chatgpt.com:443":   "https://chatgpt.com",
		"https://example.com:8443":  "https://example.com:8443",
		"http://chatgpt.com":        "",
		"https://chatgpt.com/x":     "",
		"https://chatgpt.com?q=1":   "",
		"https://u@chatgpt.com":     "",
		"https://chatgpt.com/#frag": "",
	} {
		got, err := Origin(Item{Value: in})
		if got != want || (want == "") != (err != nil) {
			t.Errorf("Origin(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
