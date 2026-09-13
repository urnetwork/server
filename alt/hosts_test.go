package alt

import (
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/warp/services"
	"gopkg.in/yaml.v3"
)

// A synthetic services config in the shape of the real one: the anchor block
// the lb blocks alias, two domains, and the api and connect services with the
// family and wildcard aliases alt must answer for.
const testServicesYml = `
default_rate_limit: &default_rate_limit
    requests_per_minute: 120
    burst: 2
    net_connections: 2
    exclude_subnets:
        - "192.0.2.0/24"
        - "2001:db8::/32"

domain: alt.example
domains:
    alt.example: route53
    alt2.example: route53

versions:
-   services:
        api:
            expose_aliases:
                - api.alt.example
                - api-v4.alt.example
                - api-v6.alt.example
        connect:
            rate_limit: *default_rate_limit
            expose_aliases:
                - connect.alt.example
                - "*.connect.alt.example"
                - connect-v4.alt.example
                - whodis.alt.example
        private:
            exposed: false
            expose_aliases:
                - private.alt.example
`

func testServicesConfig(t testing.TB) *services.ServicesConfig {
	servicesConfig := &services.ServicesConfig{}
	if err := yaml.Unmarshal([]byte(testServicesYml), servicesConfig); err != nil {
		t.Fatal(err)
	}
	return servicesConfig
}

// Alt answers for exactly the names the api and connect certificates already
// cover: the env forms for every domain plus the service's own aliases.
func TestServiceHostsAreTheExposedNamesOfTheService(t *testing.T) {
	servicesConfig := testServicesConfig(t)

	apiHosts := serviceHosts(servicesConfig, "test", ApiServiceName)
	wantApiHosts := []string{
		"test-api.alt.example",
		"test-api.alt2.example",
		"api.alt.example",
		"api-v4.alt.example",
		"api-v6.alt.example",
	}
	if !slices.Equal(apiHosts, wantApiHosts) {
		t.Fatalf("api hosts = %v, want %v", apiHosts, wantApiHosts)
	}

	connectHosts := serviceHosts(servicesConfig, "test", ConnectServiceName)
	wantConnectHosts := []string{
		"test-connect.alt.example",
		"test-connect.alt2.example",
		"connect.alt.example",
		"*.connect.alt.example",
		"connect-v4.alt.example",
		"whodis.alt.example",
	}
	if !slices.Equal(connectHosts, wantConnectHosts) {
		t.Fatalf("connect hosts = %v, want %v", connectHosts, wantConnectHosts)
	}
}

// A service that is not exposed has no public name, so alt must not answer
// for one, and an unknown service has none at all.
func TestServiceHostsSkipUnexposedAndUnknownServices(t *testing.T) {
	servicesConfig := testServicesConfig(t)
	if hosts := serviceHosts(servicesConfig, "test", "private"); len(hosts) != 0 {
		t.Fatalf("unexposed service hosts = %v", hosts)
	}
	if hosts := serviceHosts(servicesConfig, "test", "nosuchservice"); len(hosts) != 0 {
		t.Fatalf("unknown service hosts = %v", hosts)
	}
}

// The sni a client presents may differ from the configured name in case and
// in its trailing dot, and a wildcard covers exactly one label, as the
// certificate behind the same name does.
func TestHostSetMatchesTheNamesACertificateCovers(t *testing.T) {
	hostSet := NewHostSet(serviceHosts(testServicesConfig(t), "test", ConnectServiceName))
	cases := []struct {
		serverName string
		contains   bool
	}{
		{serverName: "connect.alt.example", contains: true},
		{serverName: "CONNECT.alt.example", contains: true},
		{serverName: "connect.alt.example.", contains: true},
		{serverName: "test-connect.alt2.example", contains: true},
		{serverName: "g1.connect.alt.example", contains: true},
		{serverName: "g1.b.connect.alt.example", contains: false},
		{serverName: "connect-v6.alt.example", contains: false},
		{serverName: "alt.example", contains: false},
		{serverName: "", contains: false},
		{serverName: "*.connect.alt.example", contains: false},
	}
	for _, c := range cases {
		if contains := hostSet.Contains(c.serverName); contains != c.contains {
			t.Errorf("contains(%q) = %t, want %t", c.serverName, contains, c.contains)
		}
	}
}

// A name in both lists would make the dispatch depend on the order of the
// checks rather than on the name, so construction refuses it.
func TestNewAltRefusesOverlappingHostLists(t *testing.T) {
	err := validateDisjointHosts(
		NewHostSet([]string{"api.alt.example"}),
		NewHostSet([]string{"connect.alt.example"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	err = validateDisjointHosts(
		NewHostSet([]string{"g1.connect.alt.example"}),
		NewHostSet([]string{"*.connect.alt.example"}),
	)
	if err == nil {
		t.Fatal("an api name under a connect wildcard was accepted")
	}
	if !strings.Contains(err.Error(), "g1.connect.alt.example") {
		t.Fatalf("overlap error = %s", err)
	}

	// a presented name never matches a wildcard entry, so two equal wildcards
	// are invisible to the name checks above and have to be compared directly
	err = validateDisjointHosts(
		NewHostSet([]string{"*.connect.alt.example"}),
		NewHostSet([]string{"*.connect.alt.example"}),
	)
	if err == nil {
		t.Fatal("one wildcard on both fronts was accepted")
	}
	if !strings.Contains(err.Error(), "*.connect.alt.example") {
		t.Fatalf("wildcard overlap error = %s", err)
	}

	// and a wildcard that covers no name of the other front is not an overlap
	if err := validateDisjointHosts(
		NewHostSet([]string{"*.api.alt.example", "api.alt.example"}),
		NewHostSet([]string{"*.connect.alt.example", "connect.alt.example"}),
	); err != nil {
		t.Fatalf("two disjoint wildcards were refused: %s", err)
	}
}
