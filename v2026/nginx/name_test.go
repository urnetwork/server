package nginx

// Host normalization and name matching tests. normalizeHost is checked
// against the pinned nginx's ngx_http_validate_host, case by case, and the
// matcher against nginx's server_name precedence over a fixture that puts an
// exact name and a longer wildcard under the connect wildcard. Names of 64
// KiB, about as long as a TCP SNI can be, check that matching one walks no
// more than the longest served wildcard base.

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// A 64 KiB run of one-letter labels, each with its dot, to put in front of a
// name.
func newTestLongNamePrefix() string {
	return strings.Repeat("a.", 32*1024)
}

// The part of name after each of its dots, longest first, keeping those of
// at most maxBaseLength bytes: the bases a wildcard covering the name can
// have, by plain enumeration.
func testWildcardBases(name string, maxBaseLength int) []string {
	bases := []string{}
	for i := 0; i < len(name); i += 1 {
		if name[i] == '.' && len(name)-i-1 <= maxBaseLength {
			bases = append(bases, name[i+1:])
		}
	}
	return bases
}

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
		{name: "api.service.example.example", wantOk: false},
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

// A service that isn't exposed has no server in nginx, so the matcher knows
// none of its names or redirect aliases, and a name under its wildcard falls
// to the exposed connect service's shorter wildcard, as nginx's server_name
// lookup finds it (LBALT-IMPL.md §7).
func TestNameMatcherLeavesOutAnUnexposedService(t *testing.T) {
	matcher := newNameMatcher(newTestUnexposedRouteTable(t))
	for _, name := range []string{"mcp.hidden.example", "old-mcp.hidden.example"} {
		if match, ok := matcher.match(name); ok {
			t.Errorf("match(%q) = %+v, want no match", name, match)
		}
	}
	match, ok := matcher.match("x.g1.connect.service.example")
	if !ok || match.name != "*.connect.service.example" || match.class != NameClassConnect {
		t.Errorf("match(\"x.g1.connect.service.example\") = %+v, %t, want connect's *.connect.service.example", match, ok)
	}
	for _, servedMatch := range matcher.servedMatches() {
		if servedMatch.service != nil && servedMatch.service.Name == "mcp" {
			t.Errorf("servedMatches() has %q of the unexposed mcp", servedMatch.name)
		}
	}
}

// The walk yields the part after each dot, longest first, and only the parts
// within the bound, for every name and bound here. A 64 KiB name yields the
// four parts that fit, not one for each of its 32,771 dots.
func TestWildcardBasesYieldsOnlyThePartsWithinTheBound(t *testing.T) {
	collectBases := func(name string, maxBaseLength int) []string {
		bases := []string{}
		for base := range wildcardBases(name, maxBaseLength) {
			bases = append(bases, base)
		}
		return bases
	}
	// names here are long, so failures show their length and their end
	describe := func(name string) string {
		return fmt.Sprintf("%d bytes ending %q", len(name), name[max(0, len(name)-32):])
	}

	longName := newTestLongNamePrefix() + "x.connect.service.example"
	wantBases := []string{"x.connect.service.example", "connect.service.example", "service.example", "example"}
	if bases := collectBases(longName, len("x.connect.service.example")); !slices.Equal(bases, wantBases) {
		t.Fatalf("a name of %s yielded %d bases, want %v", describe(longName), len(bases), wantBases)
	}

	names := []string{
		"",
		"example",
		".",
		"a.",
		"..",
		"a..b.example",
		".connect.service.example",
		"x.connect.service.example",
		longName,
	}
	// around each base length of x.connect.service.example, and the most a
	// served base may have
	maxBaseLengths := []int{-1, 0, 1, 6, 7, 8, 14, 15, 16, 22, 23, 24, 25, 26, 253}
	for _, name := range names {
		for _, maxBaseLength := range maxBaseLengths {
			bases := collectBases(name, maxBaseLength)
			if want := testWildcardBases(name, maxBaseLength); !slices.Equal(bases, want) {
				t.Errorf("a name of %s with bound %d yielded %d bases, want %d", describe(name), maxBaseLength, len(bases), len(want))
			}
		}
	}
}

// With more than 8 wildcards, where each lookup hashes the whole key, a 64
// KiB name walks only the bases as long as the longest served one, and still
// matches as nginx does: under the longest wildcard, whose base begins right
// where the walk starts; under shorter ones; or under none.
func TestNameMatcherWalksOnlyBasesAsLongAsTheLongestServedOne(t *testing.T) {
	// 250 bytes, near the 253 a served name may have
	longestBase := strings.Join([]string{
		strings.Repeat("b", 63),
		strings.Repeat("c", 63),
		strings.Repeat("d", 63),
		strings.Repeat("e", 50),
		"example",
	}, ".")
	tree := routeFixtureTree(t)
	names := routeTreeValue(t, tree, "services", 2, "names").([]any)
	for i := range 9 {
		names = append(names, fmt.Sprintf("*.w%d.service.example", i))
	}
	names = append(names, "*."+longestBase)
	setRouteTreeValue(t, tree, names, "services", 2, "names")
	routeTable, err := ParseRouteTable(encodeRouteTree(t, tree))
	if err != nil {
		t.Fatal(err)
	}
	matcher := newNameMatcher(routeTable)
	if len(matcher.wildcardBaseMatches) <= 8 {
		t.Fatalf("the table has %d wildcards, want more than 8", len(matcher.wildcardBaseMatches))
	}
	if matcher.maxWildcardBaseLength != len(longestBase) {
		t.Fatalf("the bound is %d bytes, want the longest base's %d", matcher.maxWildcardBaseLength, len(longestBase))
	}

	// nginx's rule with no bound: the exact name, then each part after a dot
	unboundedMatch := func(name string) (nameMatch, bool) {
		if match, ok := matcher.exactNameMatches[name]; ok {
			return match, true
		}
		for _, base := range testWildcardBases(name, len(name)) {
			if match, ok := matcher.wildcardBaseMatches[base]; ok {
				return match, true
			}
		}
		return nameMatch{}, false
	}
	longPrefix := newTestLongNamePrefix()
	cases := []struct {
		description string
		name        string
		wantName    string
	}{
		{description: "the longest base", name: longPrefix + longestBase, wantName: "*." + longestBase},
		{description: "a label then the longest base", name: longPrefix + "x." + longestBase, wantName: "*." + longestBase},
		{description: "w8.service.example", name: longPrefix + "w8.service.example", wantName: "*.w8.service.example"},
		{description: "g1.connect.service.example", name: longPrefix + "g1.connect.service.example", wantName: "*.connect.service.example"},
		{description: "unknown.example", name: longPrefix + "unknown.example", wantName: ""},
	}
	for _, c := range cases {
		match, ok := matcher.match(c.name)
		wantMatch, wantOk := unboundedMatch(c.name)
		if ok != wantOk || match != wantMatch {
			t.Errorf("64 KiB ending in %s matched %q, %t, want the unbounded walk's %q, %t", c.description, match.name, ok, wantMatch.name, wantOk)
		}
		if match.name != c.wantName {
			t.Errorf("64 KiB ending in %s matched %q, want %q", c.description, match.name, c.wantName)
		}
		// the bases match may look up: one per dot within the bound
		lookupCount := 0
		for range wildcardBases(c.name, matcher.maxWildcardBaseLength) {
			lookupCount += 1
		}
		if matcher.maxWildcardBaseLength+1 < lookupCount {
			t.Errorf("64 KiB ending in %s may look up %d bases, want at most %d", c.description, lookupCount, matcher.maxWildcardBaseLength+1)
		}
	}
}
