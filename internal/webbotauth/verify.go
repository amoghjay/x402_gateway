// Package webbotauth verifies Web Bot Auth request signatures (RFC 9421 HTTP message
// signatures, Ed25519) the way the Radius gate standard §4.1 asks: a request's signature
// agent is the origin its Signature-Agent names, when the request carries a current
// signature covering @authority that verifies against a key that origin publishes in its
// directory. Anything it cannot verify reads as no signature agent: the caller never refuses
// a request for a bad signature, its agent rules just do not match.
//
// The package is pure. It never fetches a directory: the caller passes the directories it
// has, which hold only origins the policy names.
package webbotauth

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Request is what a signature can cover. Target is the request target as sent (":path",
// with its query). Header returns a field's value, its lines joined with ", ", for a
// lower-case name.
type Request struct {
	Method    string
	Scheme    string // http or https, as the client used it
	Authority string
	Target    string
	Header    func(name string) (string, bool)
}

// Limits are the freshness rules. The defaults are platform-sdk's.
type Limits struct {
	Skew        time.Duration // tolerance on created, expires, nbf and exp
	MaxLifetime time.Duration // expires - created at most
}

// DefaultLimits are 60 s of skew and signatures valid for at most an hour.
var DefaultLimits = Limits{Skew: 60 * time.Second, MaxLifetime: time.Hour}

// MaxSignatures bounds how many signatures on one request are tried: real agents send one or
// two, and each costs a signature check.
const MaxSignatures = 8

// Directories returns the keys an origin published, and whether the caller has them.
type Directories func(origin string) (*Directory, bool)

// Verify returns the origin of the first signature on r that verifies, or "" and why none
// did. Origins without a directory in dirs are never verified.
func Verify(r Request, dirs Directories, now time.Time, lim Limits) (string, error) {
	sigInput, ok1 := r.Header("signature-input")
	sigs, ok2 := r.Header("signature")
	agentField, ok3 := r.Header("signature-agent")
	if !ok1 || !ok2 || !ok3 {
		return "", errors.New("not signed")
	}
	inputs, err := ParseDictionary(sigInput)
	if err != nil {
		return "", fmt.Errorf("signature-input: %w", err)
	}
	signatures, err := ParseDictionary(sigs)
	if err != nil {
		return "", fmt.Errorf("signature: %w", err)
	}
	agents, legacy, err := parseAgent(agentField)
	if err != nil {
		return "", fmt.Errorf("signature-agent: %w", err)
	}
	if len(inputs) > MaxSignatures {
		return "", fmt.Errorf("%d signatures, at most %d", len(inputs), MaxSignatures)
	}
	why := errors.New("no signature")
	for _, in := range inputs {
		origin, err := verifyOne(r, in, signatures, agents, legacy, dirs, now, lim)
		if err == nil {
			return origin, nil
		}
		why = fmt.Errorf("%s: %w", in.Key, err)
	}
	return "", why
}

// parseAgent reads Signature-Agent: a dictionary keyed by signature label, or the older bare
// string, which serves whichever label covers it. A String Item starts with a double quote.
func parseAgent(field string) (Dictionary, *Item, error) {
	if strings.HasPrefix(strings.TrimLeft(field, " \t"), `"`) {
		it, err := ParseItem(field)
		if err != nil {
			return nil, nil, err
		}
		return nil, &it, nil
	}
	d, err := ParseDictionary(field)
	return d, nil, err
}

var signatureParams = map[string]bool{"created": true, "expires": true, "keyid": true, "alg": true, "tag": true, "nonce": true}

func verifyOne(r Request, in DictEntry, signatures, agents Dictionary, legacy *Item, dirs Directories, now time.Time, lim Limits) (string, error) {
	label, m := in.Key, in.Value
	if !m.List {
		return "", errors.New("not an inner list")
	}
	for _, p := range m.Params {
		if !signatureParams[p.Key] {
			return "", fmt.Errorf("unknown parameter %q", p.Key)
		}
	}
	keyid, _ := paramString(m.Params, "keyid")
	created, okC := paramInt(m.Params, "created")
	expires, okE := paramInt(m.Params, "expires")
	if keyid == "" || !okC || !okE {
		return "", errors.New("keyid, created and expires are required")
	}
	if alg, ok := m.Params.Get("alg"); ok && alg != "ed25519" {
		return "", fmt.Errorf("alg %v: only ed25519", alg)
	}
	t, skew := now.Unix(), int64(lim.Skew/time.Second)
	switch {
	case created > t+skew:
		return "", errors.New("created in the future")
	case expires < t-skew:
		return "", errors.New("expired")
	case expires < created || expires-created > int64(lim.MaxLifetime/time.Second):
		return "", errors.New("lifetime out of bounds")
	}

	// The origin is what Signature-Agent names for this label.
	var agent Item
	switch {
	case legacy != nil:
		agent = *legacy
	default:
		am, ok := agents.Get(label)
		if !ok || am.List {
			return "", errors.New("no signature-agent member for this label")
		}
		agent = am.Item
	}
	origin, err := Origin(agent)
	if err != nil {
		return "", err
	}
	dir, ok := dirs(origin)
	if !ok {
		return "", fmt.Errorf("origin %s: no directory", origin)
	}

	base, err := signatureBase(r, m)
	if err != nil {
		return "", err
	}
	sm, ok := signatures.Get(label)
	sig, isBytes := sm.Item.Value.([]byte)
	if !ok || sm.List || !isBytes || len(sig) != ed25519.SignatureSize {
		return "", errors.New("no Ed25519 signature for this label")
	}
	key, ok := dir.Find(keyid)
	if !ok {
		return "", fmt.Errorf("origin %s: no key %q", origin, keyid)
	}
	if !key.ValidAt(t, skew) {
		return "", fmt.Errorf("origin %s: key %q outside nbf/exp", origin, keyid)
	}
	pub := key.Public()
	if pub == nil || !ed25519.Verify(pub, []byte(base), sig) {
		return "", errors.New("signature does not verify")
	}
	return origin, nil
}

// Origin reads a Signature-Agent value as an origin: an https URL with no path beyond "/",
// no query, fragment or userinfo, and no parameters, lower-cased with the default port
// dropped, as the compiled policy writes origins.
func Origin(it Item) (string, error) {
	s, ok := it.Value.(string)
	if !ok || len(it.Params) > 0 {
		return "", errors.New("signature-agent member is not a plain string")
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") {
		return "", fmt.Errorf("signature-agent %q is not an https origin", s)
	}
	host := strings.ToLower(u.Host)
	host = strings.TrimSuffix(host, ":443")
	return "https://" + host, nil
}

// signatureBase rebuilds the text that was signed (RFC 9421 §2.5). Every covered component
// must be one this gate can read, and @authority must be among them (gate standard §4.1).
func signatureBase(r Request, m Member) (string, error) {
	var b strings.Builder
	authority := false
	seen := map[string]bool{}
	for _, c := range m.Items {
		name, ok := c.Value.(string)
		if !ok {
			return "", errors.New("component is not a string")
		}
		id := SerializeItem(c)
		if seen[id] {
			return "", fmt.Errorf("component %s twice", id)
		}
		seen[id] = true
		v, err := componentValue(r, name, c.Params)
		if err != nil {
			return "", err
		}
		if name == "@authority" {
			authority = true
		}
		b.WriteString(id)
		b.WriteString(": ")
		b.WriteString(v)
		b.WriteByte('\n')
	}
	if !authority {
		return "", errors.New("@authority is not covered")
	}
	b.WriteString(`"@signature-params": `)
	b.WriteString(SerializeInnerList(m.Items, m.Params))
	return b.String(), nil
}

func componentValue(r Request, name string, ps Params) (string, error) {
	if strings.HasPrefix(name, "@") {
		if len(ps) > 0 {
			return "", fmt.Errorf("component %s: parameters not supported", name)
		}
		return derived(r, name)
	}
	if name != strings.ToLower(name) {
		return "", fmt.Errorf("component %q is not lower-case", name)
	}
	v, ok := r.Header(name)
	if !ok {
		return "", fmt.Errorf("component %s: header absent", name)
	}
	v = strings.Trim(v, " \t")
	switch {
	case len(ps) == 0:
		return v, nil
	case len(ps) == 1 && ps[0].Key == "key":
		key, ok := ps[0].Value.(string)
		if !ok {
			return "", fmt.Errorf("component %s: key is not a string", name)
		}
		d, err := ParseDictionary(v)
		if err != nil {
			return "", fmt.Errorf("component %s: %w", name, err)
		}
		mm, ok := d.Get(key)
		if !ok {
			return "", fmt.Errorf("component %s: no member %q", name, key)
		}
		if mm.List {
			return SerializeInnerList(mm.Items, mm.Params), nil
		}
		return SerializeItem(mm.Item), nil
	}
	return "", fmt.Errorf("component %s: parameters not supported", name)
}

func derived(r Request, name string) (string, error) {
	scheme := strings.ToLower(r.Scheme)
	path, query, hasQuery := strings.Cut(r.Target, "?")
	switch name {
	case "@method":
		return r.Method, nil
	case "@authority":
		return authority(scheme, r.Authority), nil
	case "@scheme":
		return scheme, nil
	case "@target-uri":
		return scheme + "://" + authority(scheme, r.Authority) + r.Target, nil
	case "@request-target":
		return r.Target, nil
	case "@path":
		if path == "" {
			path = "/"
		}
		return path, nil
	case "@query":
		if !hasQuery {
			return "?", nil
		}
		return "?" + query, nil
	}
	return "", fmt.Errorf("component %s not supported", name)
}

// authority is @authority (RFC 9421 §2.2.3): the host lower-cased, the scheme's default port
// dropped.
func authority(scheme, a string) string {
	a = strings.ToLower(a)
	switch scheme {
	case "https":
		return strings.TrimSuffix(a, ":443")
	case "http":
		return strings.TrimSuffix(a, ":80")
	}
	return a
}

func paramString(ps Params, key string) (string, bool) {
	v, ok := ps.Get(key)
	s, isString := v.(string)
	return s, ok && isString
}

func paramInt(ps Params, key string) (int64, bool) {
	v, ok := ps.Get(key)
	i, isInt := v.(int64)
	return i, ok && isInt
}
