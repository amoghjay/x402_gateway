package webbotauth

import (
	"fmt"
	"strings"
	"testing"
)

// Bodies of the live directories, fetched 2026-11-23.
const (
	chatgptDirectory = `{"keys":[{"crv":"Ed25519","kty":"OKP","x":"P_1c4SX6fCavDcq6FRegWcVvaOKsp_aXHPEN1FcKDIA","kid":"ArwjqcCqtA5oxEWkzXGC-To_0A9whHmajCHtfe8I9aY","use":"sig","nbf":1735689600,"exp":1796067228}],"signature_agent":"https://chatgpt.com","purpose":"ai"}`
	googleDirectory  = `{"keys":[` +
		`{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","key_ops":["verify"],"kid":"mhxuPw","x":"X53rIypL6muU_xeVU6Vm0uoGQcKKxm5FCRIFZHR0CZA"},` +
		`{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","key_ops":["verify"],"kid":"cYSMkA","x":"BCEFRL2wi2rfWoqfuneHGVmh_axPFaXrXNQrfxUpZqM"},` +
		`{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","key_ops":["verify"],"kid":"bUCe2A","x":"W1nRrCPqci1T40ppv1ULK6q67a14hjLScZ955FPxQQM"},` +
		`{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","key_ops":["verify"],"kid":"DYiMjA","x":"0cFRtAeGOa46EsjXFVKw1o_pMPFBGq4ReeznYs_RBrs"},` +
		`{"kty":"OKP","crv":"Ed25519","alg":"EdDSA","use":"sig","key_ops":["verify"],"kid":"Ggh50g","x":"7ZziBznI1XsHehATtJjS5XD6QrS_LJlLD8jHml5RJVI"}]}`
)

func TestThumbprint(t *testing.T) {
	if got := Thumbprint(testX); got != testThumb {
		t.Fatalf("RFC 9421 test key thumbprint %s, want %s", got, testThumb)
	}
}

func TestParseDirectoryChatGPT(t *testing.T) {
	d, err := ParseDirectory([]byte(chatgptDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Keys) != 1 {
		t.Fatalf("%d keys", len(d.Keys))
	}
	k := d.Keys[0]
	if k.Thumbprint != k.Kid {
		t.Errorf("OpenAI's kid is its thumbprint: kid %s, thumbprint %s", k.Kid, k.Thumbprint)
	}
	if k.NotBefore != 1735689600 || k.Expires != 1796067228 || k.Public() == nil {
		t.Errorf("key %+v", k)
	}
}

func TestParseDirectoryGoogle(t *testing.T) {
	d, err := ParseDirectory([]byte(googleDirectory))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Keys) != 5 {
		t.Fatalf("%d keys", len(d.Keys))
	}
	// Google's kids are short, not thumbprints; both find the key.
	for kid, thumb := range map[string]string{ //nolint:gosec // public key thumbprints, not credentials
		"mhxuPw": "_fmS_hfpckRfp1x7fDExtVKVaYtcu1WJX1Io5LIow24",
		"DYiMjA": "ImfgehCo9CQ0CTvJyuAY3dt6p5xMvJVutWoGpHtmVME",
	} {
		byKid, ok1 := d.Find(kid)
		byThumb, ok2 := d.Find(thumb)
		if !ok1 || !ok2 || byKid != byThumb || byKid.Thumbprint != thumb {
			t.Errorf("kid %s: by kid %+v, by thumbprint %+v", kid, byKid, byThumb)
		}
	}
	if _, ok := d.Find("unknown"); ok {
		t.Error("found an unknown keyid")
	}
}

func TestParseDirectoryRefusals(t *testing.T) {
	many := make([]string, MaxDirectoryKeys+1)
	for i := range many {
		many[i] = fmt.Sprintf(`{"kty":"OKP","crv":"Ed25519","x":"%s"}`, testX)
	}
	for name, body := range map[string]string{
		"not JSON":             `<html>`,
		"no keys":              `{"keys":[]}`,
		"only other key types": `{"keys":[{"kty":"EC","crv":"P-256","x":"a","y":"b"},{"kty":"RSA","n":"a","e":"AQAB"}]}`,
		"a short x":            `{"keys":[{"kty":"OKP","crv":"Ed25519","x":"AAAA"}]}`,
		"too many keys":        `{"keys":[` + strings.Join(many, ",") + `]}`,
	} {
		if _, err := ParseDirectory([]byte(body)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseDirectorySkipsOtherKeys(t *testing.T) {
	d, err := ParseDirectory([]byte(`{"keys":[{"kty":"RSA","n":"a","e":"AQAB"},{"kty":"OKP","crv":"Ed25519","x":"` + testX + `","exp":1.7e9}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Keys) != 1 || d.Keys[0].Expires != 1700000000 || d.Keys[0].Kid != "" {
		t.Fatalf("keys %+v", d.Keys)
	}
	if _, ok := d.Find(testThumb); !ok {
		t.Error("a key without a kid is not found by its thumbprint")
	}
}

func TestKeyValidAt(t *testing.T) {
	k := Key{NotBefore: 1000, Expires: 2000}
	for now, want := range map[int64]bool{900: false, 950: true, 1500: true, 2050: true, 2100: false} {
		if got := k.ValidAt(now, 60); got != want {
			t.Errorf("ValidAt(%d) = %v", now, got)
		}
	}
	if !(&Key{}).ValidAt(0, 0) {
		t.Error("a key without nbf/exp is not valid")
	}
}
