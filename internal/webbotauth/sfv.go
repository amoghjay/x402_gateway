package webbotauth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The subset of RFC 8941 structured fields that HTTP message signatures need: dictionaries
// whose members are items or inner lists, with parameters, and the bare item types integer,
// string, token, byte sequence and boolean. Decimals are refused: no signature field uses
// them. Serialisation is the canonical form, which RFC 9421 rebuilds the signature base from.

// Token is a structured-field token, kept apart from a String so it serialises unquoted.
type Token string

// Param is one parameter. Value is int64, string, Token, []byte or bool.
type Param struct {
	Key   string
	Value any
}

// Params keeps parameters in the order they were sent: the order is part of the signed text.
type Params []Param

// Get returns the value of the first parameter named key.
func (ps Params) Get(key string) (any, bool) {
	for _, p := range ps {
		if p.Key == key {
			return p.Value, true
		}
	}
	return nil, false
}

// Item is a bare item with its parameters.
type Item struct {
	Value  any
	Params Params
}

// Member is a dictionary member's value: an Item, or an inner list when List is true.
type Member struct {
	List   bool
	Item   Item   // when !List
	Items  []Item // when List
	Params Params // the inner list's own parameters, when List
}

// DictEntry is one dictionary member in order.
type DictEntry struct {
	Key   string
	Value Member
}

// Dictionary is a parsed dictionary, members in order. A repeated key keeps the last value
// in the first one's place, as RFC 8941 says.
type Dictionary []DictEntry

// Get returns the member named key.
func (d Dictionary) Get(key string) (Member, bool) {
	for _, e := range d {
		if e.Key == key {
			return e.Value, true
		}
	}
	return Member{}, false
}

var errSyntax = errors.New("structured field syntax")

type parser struct {
	s string
	i int
}

func (p *parser) eof() bool { return p.i >= len(p.s) }
func (p *parser) peek() byte {
	if p.eof() {
		return 0
	}
	return p.s[p.i]
}
func (p *parser) skipSP() {
	for !p.eof() && p.s[p.i] == ' ' {
		p.i++
	}
}
func (p *parser) skipOWS() {
	for !p.eof() && (p.s[p.i] == ' ' || p.s[p.i] == '\t') {
		p.i++
	}
}

// ParseDictionary parses a whole field value as a dictionary.
func ParseDictionary(field string) (Dictionary, error) {
	p := &parser{s: strings.Trim(field, " \t")}
	var d Dictionary
	for !p.eof() {
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		var m Member
		if p.peek() == '=' {
			p.i++
			if m, err = p.member(); err != nil {
				return nil, err
			}
		} else {
			ps, err := p.params()
			if err != nil {
				return nil, err
			}
			m = Member{Item: Item{Value: true, Params: ps}}
		}
		replaced := false
		for j := range d {
			if d[j].Key == key {
				d[j].Value, replaced = m, true
			}
		}
		if !replaced {
			d = append(d, DictEntry{Key: key, Value: m})
		}
		p.skipOWS()
		if p.eof() {
			return d, nil
		}
		if p.peek() != ',' {
			return nil, fmt.Errorf("%w: expected ',' at %d", errSyntax, p.i)
		}
		p.i++
		p.skipOWS()
		if p.eof() {
			return nil, fmt.Errorf("%w: trailing ','", errSyntax)
		}
	}
	return d, nil
}

// ParseItem parses a whole field value as one item.
func ParseItem(field string) (Item, error) {
	p := &parser{s: strings.Trim(field, " \t")}
	it, err := p.item()
	if err != nil {
		return Item{}, err
	}
	if !p.eof() {
		return Item{}, fmt.Errorf("%w: trailing characters", errSyntax)
	}
	return it, nil
}

func (p *parser) member() (Member, error) {
	if p.peek() == '(' {
		items, ps, err := p.innerList()
		return Member{List: true, Items: items, Params: ps}, err
	}
	it, err := p.item()
	return Member{Item: it}, err
}

func (p *parser) innerList() ([]Item, Params, error) {
	p.i++ // (
	var items []Item
	for {
		p.skipSP()
		if p.eof() {
			return nil, nil, fmt.Errorf("%w: unterminated inner list", errSyntax)
		}
		if p.peek() == ')' {
			p.i++
			ps, err := p.params()
			return items, ps, err
		}
		it, err := p.item()
		if err != nil {
			return nil, nil, err
		}
		items = append(items, it)
		if c := p.peek(); c != ' ' && c != ')' {
			return nil, nil, fmt.Errorf("%w: expected ' ' or ')' at %d", errSyntax, p.i)
		}
	}
}

func (p *parser) item() (Item, error) {
	v, err := p.bareItem()
	if err != nil {
		return Item{}, err
	}
	ps, err := p.params()
	return Item{Value: v, Params: ps}, err
}

func (p *parser) params() (Params, error) {
	var ps Params
	for p.peek() == ';' {
		p.i++
		p.skipSP()
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		var v any = true
		if p.peek() == '=' {
			p.i++
			if v, err = p.bareItem(); err != nil {
				return nil, err
			}
		}
		replaced := false
		for j := range ps {
			if ps[j].Key == key {
				ps[j].Value, replaced = v, true
			}
		}
		if !replaced {
			ps = append(ps, Param{Key: key, Value: v})
		}
	}
	return ps, nil
}

func (p *parser) key() (string, error) {
	start := p.i
	if c := p.peek(); (c < 'a' || c > 'z') && c != '*' {
		return "", fmt.Errorf("%w: key at %d", errSyntax, p.i)
	}
	for !p.eof() {
		c := p.s[p.i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' || c == '.' || c == '*' {
			p.i++
			continue
		}
		break
	}
	return p.s[start:p.i], nil
}

func (p *parser) bareItem() (any, error) {
	c := p.peek()
	switch {
	case c == '-' || (c >= '0' && c <= '9'):
		return p.integer()
	case c == '"':
		return p.str()
	case c == ':':
		return p.bytes()
	case c == '?':
		return p.boolean()
	case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '*':
		return p.token(), nil
	}
	return nil, fmt.Errorf("%w: bare item at %d", errSyntax, p.i)
}

func (p *parser) integer() (int64, error) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	digits := 0
	for !p.eof() && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
		digits++
	}
	if digits == 0 || digits > 15 {
		return 0, fmt.Errorf("%w: integer at %d", errSyntax, start)
	}
	if p.peek() == '.' {
		return 0, fmt.Errorf("%w: decimals are not supported", errSyntax)
	}
	return strconv.ParseInt(p.s[start:p.i], 10, 64)
}

func (p *parser) str() (string, error) {
	p.i++ // "
	var b strings.Builder
	for !p.eof() {
		c := p.s[p.i]
		p.i++
		switch {
		case c == '\\':
			if p.eof() {
				return "", fmt.Errorf("%w: string escape", errSyntax)
			}
			n := p.s[p.i]
			if n != '"' && n != '\\' {
				return "", fmt.Errorf("%w: string escape", errSyntax)
			}
			b.WriteByte(n)
			p.i++
		case c == '"':
			return b.String(), nil
		case c < 0x20 || c > 0x7e:
			return "", fmt.Errorf("%w: string character", errSyntax)
		default:
			b.WriteByte(c)
		}
	}
	return "", fmt.Errorf("%w: unterminated string", errSyntax)
}

// token reads ALPHA / "*" followed by tchar, ":" or "/" (RFC 8941 §3.3.4).
func (p *parser) token() Token {
	start := p.i
	p.i++
	for !p.eof() && (isTchar(p.s[p.i]) || p.s[p.i] == ':' || p.s[p.i] == '/') {
		p.i++
	}
	return Token(p.s[start:p.i])
}

func isTchar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
		strings.IndexByte("!#$%&'*+-.^_`|~", c) >= 0
}

func (p *parser) bytes() ([]byte, error) {
	p.i++ // :
	end := strings.IndexByte(p.s[p.i:], ':')
	if end < 0 {
		return nil, fmt.Errorf("%w: unterminated byte sequence", errSyntax)
	}
	raw := p.s[p.i : p.i+end]
	p.i += end + 1
	b, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: byte sequence: %w", errSyntax, err)
	}
	return b, nil
}

func (p *parser) boolean() (bool, error) {
	p.i++ // ?
	switch p.peek() {
	case '1':
		p.i++
		return true, nil
	case '0':
		p.i++
		return false, nil
	}
	return false, fmt.Errorf("%w: boolean at %d", errSyntax, p.i)
}

// serializeBare writes a bare item in canonical form.
func serializeBare(b *strings.Builder, v any) {
	switch x := v.(type) {
	case int64:
		b.WriteString(strconv.FormatInt(x, 10))
	case string:
		b.WriteByte('"')
		for i := 0; i < len(x); i++ {
			if x[i] == '"' || x[i] == '\\' {
				b.WriteByte('\\')
			}
			b.WriteByte(x[i])
		}
		b.WriteByte('"')
	case Token:
		b.WriteString(string(x))
	case []byte:
		b.WriteByte(':')
		b.WriteString(base64.StdEncoding.EncodeToString(x))
		b.WriteByte(':')
	case bool:
		if x {
			b.WriteString("?1")
		} else {
			b.WriteString("?0")
		}
	}
}

func serializeParams(b *strings.Builder, ps Params) {
	for _, p := range ps {
		b.WriteByte(';')
		b.WriteString(p.Key)
		if v, ok := p.Value.(bool); ok && v {
			continue
		}
		b.WriteByte('=')
		serializeBare(b, p.Value)
	}
}

// SerializeItem is an item in canonical form.
func SerializeItem(it Item) string {
	var b strings.Builder
	serializeBare(&b, it.Value)
	serializeParams(&b, it.Params)
	return b.String()
}

// SerializeInnerList is an inner list in canonical form.
func SerializeInnerList(items []Item, ps Params) string {
	var b strings.Builder
	b.WriteByte('(')
	for i, it := range items {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(SerializeItem(it))
	}
	b.WriteByte(')')
	serializeParams(&b, ps)
	return b.String()
}
