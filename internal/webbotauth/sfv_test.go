package webbotauth

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

func TestParseDictionary(t *testing.T) {
	d, err := ParseDictionary(`sig1=("@authority" "signature-agent";key="sig1");created=1;keyid="k";alg="ed25519";tag="web-bot-auth",  b=:AQID:, c, d=?0;x, e=tok/en:1, f=-42`)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 6 {
		t.Fatalf("%d members", len(d))
	}
	m, _ := d.Get("sig1")
	if !m.List || len(m.Items) != 2 || m.Items[1].Params[0].Value != "sig1" {
		t.Fatalf("sig1 %+v", m)
	}
	if got := SerializeInnerList(m.Items, m.Params); got != `("@authority" "signature-agent";key="sig1");created=1;keyid="k";alg="ed25519";tag="web-bot-auth"` {
		t.Errorf("serialised %s", got)
	}
	if b, _ := d.Get("b"); !bytes.Equal(b.Item.Value.([]byte), []byte{1, 2, 3}) {
		t.Errorf("b %+v", b)
	}
	if c, _ := d.Get("c"); c.Item.Value != true {
		t.Errorf("c %+v", c)
	}
	if dd, _ := d.Get("d"); dd.Item.Value != false || SerializeItem(dd.Item) != "?0;x" {
		t.Errorf("d %+v", dd)
	}
	if e, _ := d.Get("e"); e.Item.Value != Token("tok/en:1") {
		t.Errorf("e %+v", e)
	}
	if f, _ := d.Get("f"); f.Item.Value != int64(-42) {
		t.Errorf("f %+v", f)
	}
}

func TestParseDictionaryRepeatedKeyKeepsLast(t *testing.T) {
	d, err := ParseDictionary(`a=1, b=2, a=3`)
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 2 || d[0].Key != "a" || d[0].Value.Item.Value != int64(3) {
		t.Fatalf("%+v", d)
	}
}

func TestStringEscapes(t *testing.T) {
	it, err := ParseItem(`"a\"b\\c"`)
	if err != nil || it.Value != `a"b\c` || SerializeItem(it) != `"a\"b\\c"` {
		t.Fatalf("%+v %v", it, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, s := range []string{
		`a=`, `a=1,`, `A=1`, `a=(1 2`, `a=(1,2)`, `a="unterminated`, `a="\x"`, `a=:not base64!:`,
		`a=1.5`, `a=?2`, `a=1 b=2`, `a=1234567890123456`,
	} {
		if _, err := ParseDictionary(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
	if _, err := ParseItem(`"a" x`); err == nil {
		t.Error("trailing characters accepted")
	}
}

// A field of many distinct keys is refused rather than scanned: repeated keys are found by a
// linear search, and the field arrives on requests nobody has signed.
func TestParseBoundsMembersAndParameters(t *testing.T) {
	members := func(n int, sep string) string {
		keys := make([]string, n)
		for i := range keys {
			keys[i] = fmt.Sprintf("k%d", i)
		}
		return strings.Join(keys, sep)
	}
	if d, err := ParseDictionary(members(maxMembers, ", ")); err != nil || len(d) != maxMembers {
		t.Errorf("%d members: %d, %v", maxMembers, len(d), err)
	}
	if _, err := ParseDictionary(members(maxMembers+1, ", ")); err == nil {
		t.Errorf("%d members accepted", maxMembers+1)
	}
	if _, err := ParseDictionary("a;" + members(maxMembers, ";")); err != nil {
		t.Errorf("%d parameters: %v", maxMembers, err)
	}
	if _, err := ParseDictionary("a;" + members(maxMembers+1, ";")); err == nil {
		t.Errorf("%d parameters accepted", maxMembers+1)
	}
	// A repeated key takes no room.
	if _, err := ParseDictionary(strings.Repeat("a=1, ", 4*maxMembers) + "a=2"); err != nil {
		t.Errorf("repeated key: %v", err)
	}
}
