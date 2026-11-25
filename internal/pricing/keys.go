package pricing

import (
	"errors"
	"fmt"
	"regexp"
)

// KeyStyle is how the table's keys relate to the route names Envoy sends.
type KeyStyle int

const (
	// KeysHTTPRoute: Envoy Gateway names a route "httproute/<ns>/<name>/rule/<i>/match/<j>",
	// and the table is keyed per HTTPRoute, "httproute/<ns>/<name>". What the controller
	// writes; the cluster mode.
	KeysHTTPRoute KeyStyle = iota
	// KeysName: the key is Envoy's route name itself, as written in envoy.yaml. The
	// standalone mode.
	KeysName
)

// routeName is a route name a table can key on: short, and made of characters that are
// safe in a log line and a metric label.
var routeName = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// ParseKeyStyle reads the -route-keys flag.
func ParseKeyStyle(s string) (KeyStyle, error) {
	switch s {
	case "httproute":
		return KeysHTTPRoute, nil
	case "name":
		return KeysName, nil
	}
	return 0, fmt.Errorf("route keys %q: want httproute or name", s)
}

func (k KeyStyle) String() string {
	if k == KeysName {
		return "name"
	}
	return "httproute"
}

// Key turns Envoy's xds.route_name into a table key.
func (k KeyStyle) Key(xdsRouteName string) (string, bool) {
	if k == KeysName {
		return xdsRouteName, routeName.MatchString(xdsRouteName)
	}
	return RouteKey(xdsRouteName)
}

// check reports why key cannot be a table key in this style.
func (k KeyStyle) check(key string) error {
	if k == KeysName {
		if !routeName.MatchString(key) {
			return errors.New(`key must be an Envoy route name: 1 to 128 characters from A-Z a-z 0-9 . _ : / -`)
		}
		return nil
	}
	if rk, ok := RouteKey(key + "/"); !ok || rk != key {
		return errors.New(`key must be "httproute/<namespace>/<name>"`)
	}
	return nil
}
