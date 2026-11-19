package nginx

// Host normalization and name matching tests. normalizeHost is checked
// against the pinned nginx's ngx_http_validate_host, case by case, and the
// matcher against nginx's server_name precedence over a fixture that puts an
// exact name and a longer wildcard under the connect wildcard.

import (
	"testing"
)

// Each host nginx's $host accepts normalizes as nginx writes it, and each one
// it declines is refused.
func TestNormalizeHost(t *testing.T) {
	cases := []struct {
		host string
		want string
		ok   bool
	}{
		// lowercased, the port and one trailing dot removed
		{host: "api.service.example", want: "api.service.example", ok: true},
		{host: "API.Service.EXAMPLE", want: "api.service.example", ok: true},
		{host: "api.service.example:443", want: "api.service.example", ok: true},
		{host: "api.service.example.", want: "api.service.example", ok: true},
		{host: "API.service.example.:8443", want: "api.service.example", ok: true},
		{host: "api.service.example:", want: "api.service.example", ok: true},
		{host: "api.service.example:00443", want: "api.service.example", ok: true},
		{host: "api.service.example:65535", want: "api.service.example", ok: true},
		{host: "192.0.2.1:80", want: "192.0.2.1", ok: true},
		// an IPv6 literal keeps its brackets
		{host: "[2001:DB8::1]:443", want: "[2001:db8::1]", ok: true},
		{host: "[2001:db8::1]", want: "[2001:db8::1]", ok: true},
		{host: "[::ffff:192.0.2.1]", want: "[::ffff:192.0.2.1]", ok: true},
		// nginx's allowlist: unreserved, sub-delims and pct-encoded
		{host: "under_score.service.example", want: "under_score.service.example", ok: true},
		{host: "tilde~.service.example", want: "tilde~.service.example", ok: true},
		{host: "*.connect.service.example", want: "*.connect.service.example", ok: true},
		{host: "a!$&'()*+,;=.service.example", want: "a!$&'()*+,;=.service.example", ok: true},
		{host: "pct%41.service.example", want: "pct%41.service.example", ok: true},
		// a leading dot passes, as in nginx
		{host: ".service.example", want: ".service.example", ok: true},

		{host: "", ok: false},
		{host: ".", ok: false},
		{host: ":443", ok: false},
		{host: "..", ok: false},
		{host: "api..service.example", ok: false},
		{host: "api.service.example..", ok: false},
		{host: "api.service.example:65536", ok: false},
		{host: "api.service.example:99999", ok: false},
		{host: "api.service.example:44a", ok: false},
		{host: "api.service.example:443:443", ok: false},
		{host: "api service.example", ok: false},
		{host: "api.service.example/", ok: false},
		{host: "api\x00.service.example", ok: false},
		{host: "api\t.service.example", ok: false},
		{host: "user@api.service.example", ok: false},
		{host: "api[.service.example", ok: false},
		{host: "apié.service.example", ok: false},
		// an unterminated literal, or more than a port after one
		{host: "[2001:db8::1", ok: false},
		{host: "[2001:db8::1]x", ok: false},
		{host: "[2001:db8::1].", ok: false},
		{host: "[2001:db8::%1]", ok: false},
		{host: "[2001:db8::1]:65536", ok: false},
		{host: "[2001:db8:..1]", ok: false},
	}
	for _, c := range cases {
		got, ok := normalizeHost(c.host)
		if ok != c.ok || got != c.want {
			t.Errorf("normalizeHost(%q) = %q, %t, want %q, %t", c.host, got, ok, c.want, c.ok)
		}
	}
}

// The fixture's table with two names added under the connect wildcard
// *.connect.service.example: the exact api.connect.service.example for the
// api, and the longer wildcard *.g1.connect.service.example for mcp.
func newTestNameMatcherTable(t *testing.T) *RouteTable {
	t.Helper()
	tree := routeFixtureTree(t)
	addName := func(serviceIndex int, name string) {
		names := routeTreeValue(t, tree, "services", serviceIndex, "names").([]any)
		setRouteTreeValue(t, tree, append(names, name), "services", serviceIndex, "names")
	}
	addName(0, "api.connect.service.example")
	addName(2, "*.g1.connect.service.example")
	routeTable, err := ParseRouteTable(encodeRouteTree(t, tree))
	if err != nil {
		t.Fatal(err)
	}
	if routeTable.Services[0].Name != "api" || routeTable.Services[2].Name != "mcp" {
		t.Fatalf("the fixture's services moved: %s, %s", routeTable.Services[0].Name, routeTable.Services[2].Name)
	}
	return routeTable
}

// Exact names win, then the longest leading wildcard, which matches one or
// more labels; every class comes from where the table lists the name.
func TestNameMatcherMatchesAsNginxServerNames(t *testing.T) {
	matcher := newNameMatcher(newTestNameMatcherTable(t))
	cases := []struct {
		name               string
		wantOk             bool
		wantName           string
		wantClass          NameClass
		wantServiceName    string
		wantRedirectTarget string
	}{
		{name: "test-lb.service.example", wantOk: true, wantName: "test-lb.service.example", wantClass: NameClassLb},
		{name: "test-lb.other.example", wantOk: true, wantName: "test-lb.other.example", wantClass: NameClassLb},
		{name: "api.service.example", wantOk: true, wantName: "api.service.example", wantClass: NameClassApi, wantServiceName: "api"},
		{name: "test-api.other.example", wantOk: true, wantName: "test-api.other.example", wantClass: NameClassApi, wantServiceName: "api"},
		{name: "connect.service.example", wantOk: true, wantName: "connect.service.example", wantClass: NameClassConnect, wantServiceName: "connect"},
		{name: "test-connect.other.example", wantOk: true, wantName: "test-connect.other.example", wantClass: NameClassConnect, wantServiceName: "connect"},
		{name: "test-mcp.service.example", wantOk: true, wantName: "test-mcp.service.example", wantClass: NameClassForwarded, wantServiceName: "mcp"},
		{name: "old-api.service.example", wantOk: true, wantName: "old-api.service.example", wantClass: NameClassRedirect, wantServiceName: "api", wantRedirectTarget: "api.service.example"},

		// one label under the wildcard, and several
		{name: "g2.connect.service.example", wantOk: true, wantName: "*.connect.service.example", wantClass: NameClassConnect, wantServiceName: "connect"},
		{name: "a.b.connect.service.example", wantOk: true, wantName: "*.connect.service.example", wantClass: NameClassConnect, wantServiceName: "connect"},
		// an exact name under the wildcard wins
		{name: "api.connect.service.example", wantOk: true, wantName: "api.connect.service.example", wantClass: NameClassApi, wantServiceName: "api"},
		// the longest wildcard wins, for one label or several
		{name: "x.g1.connect.service.example", wantOk: true, wantName: "*.g1.connect.service.example", wantClass: NameClassForwarded, wantServiceName: "mcp"},
		{name: "y.x.g1.connect.service.example", wantOk: true, wantName: "*.g1.connect.service.example", wantClass: NameClassForwarded, wantServiceName: "mcp"},
		// a wildcard never matches its own base
		{name: "g1.connect.service.example", wantOk: true, wantName: "*.connect.service.example", wantClass: NameClassConnect, wantServiceName: "connect"},
		// nginx reads an empty first label, or a literal *, as a label
		{name: ".connect.service.example", wantOk: true, wantName: "*.connect.service.example", wantClass: NameClassConnect, wantServiceName: "connect"},
		{name: "*.connect.service.example", wantOk: true, wantName: "*.connect.service.example", wantClass: NameClassConnect, wantServiceName: "connect"},

		{name: "", wantOk: false},
		{name: "unknown.example", wantOk: false},
		{name: "service.example", wantOk: false},
		{name: "example", wantOk: false},
		{name: "api.service.example.other", wantOk: false},
		{name: "x.api.service.example", wantOk: false},
		{name: "x.test-lb.service.example", wantOk: false},
	}
	for _, c := range cases {
		match, ok := matcher.match(c.name)
		if ok != c.wantOk {
			t.Errorf("match(%q) ok = %t, want %t", c.name, ok, c.wantOk)
			continue
		}
		if !ok {
			if match != (nameMatch{}) {
				t.Errorf("match(%q) = %+v with false, want the zero match", c.name, match)
			}
			continue
		}
		serviceName := ""
		if match.service != nil {
			serviceName = match.service.Name
		}
		if match.name != c.wantName || match.class != c.wantClass || serviceName != c.wantServiceName || match.redirectTarget != c.wantRedirectTarget {
			t.Errorf(
				"match(%q) = name %q, class %s, service %q, redirect %q; want name %q, class %s, service %q, redirect %q",
				c.name, match.name, match.class, serviceName, match.redirectTarget,
				c.wantName, c.wantClass, c.wantServiceName, c.wantRedirectTarget,
			)
		}
	}
}

// The matcher serves exactly the names the table serves, so the certificate
// store and the configs built from them agree on every name.
func TestNameMatcherServesTheTablesServedNames(t *testing.T) {
	routeTable := newTestNameMatcherTable(t)
	matcher := newNameMatcher(routeTable)
	servedNames := map[string]bool{}
	for _, name := range routeTable.ServedNames() {
		servedNames[name] = true
	}
	matches := matcher.servedMatches()
	if len(matches) != len(servedNames) {
		t.Fatalf("servedMatches() has %d names, the table serves %d", len(matches), len(servedNames))
	}
	for _, match := range matches {
		if !servedNames[match.name] {
			t.Errorf("servedMatches() has %q, which the table doesn't serve", match.name)
		}
		// each served name matches itself as written
		selfMatch, ok := matcher.match(match.name)
		if !ok || selfMatch.name != match.name {
			t.Errorf("match(%q) = %q, %t, want itself", match.name, selfMatch.name, ok)
		}
	}
}
