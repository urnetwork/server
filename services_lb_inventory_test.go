package server

import (
	"reflect"
	"testing"
)

// A synthetic services.yml in the vault's shape: the active version's lb
// interfaces with yaml anchors and fields the inventory does not read, a
// transparent host, an address-less interface, a historical version and the
// optional move weights.
const servicesLbInventoryTestYaml = `
domain: synthetic.example
rate_limit: &default_rate_limit
    requests_per_second: 10
versions:
-   lb:
        ports:
            - 443
        interfaces:
            alpha-edge-0.synthetic.example:
                eno2:
                    docker_network: warpeno2
                    rate_limit: *default_rate_limit
                    dns_weight: 10
                    ipv4: 192.0.2.10
                    ipv6: "2001:db8::10"
                eno1:
                    dns_weight: 150
                    ipv4: 192.0.2.11
                eno3:
                    docker_network: warpeno3
            alpha-edge-1.synthetic.example:
                eno1np0:
                    ipv6: "2001:db8::21"
                    router: router-0
            relay.synthetic.example:
                eno1np0:
                    transparent: true
                    ipv4: 198.51.100.5
        move_weights:
            alpha-edge-1.synthetic.example: 1.5
    services:
        connect:
            blocks:
                - g1: 24
-   lb:
        interfaces:
            alpha-edge-0.synthetic.example:
                old0:
                    ipv4: 203.0.113.200
`

func TestParseServicesLbInventoryReadsActiveEdgeInterfaces(t *testing.T) {
	inventory, err := ParseServicesLbInventory([]byte(servicesLbInventoryTestYaml))
	if err != nil {
		t.Fatal(err)
	}
	if inventory.Domain != "synthetic.example" {
		t.Fatalf("domain = %q", inventory.Domain)
	}
	want := map[string][]ServicesLbInterface{
		"alpha-edge-0": {
			// sorted by interface; an unset weight takes warp's default
			{Interface: "eno1", Ipv4: "192.0.2.11", DnsWeight: 150},
			{Interface: "eno2", Ipv4: "192.0.2.10", Ipv6: "2001:db8::10", DnsWeight: 10},
		},
		"alpha-edge-1": {
			{Interface: "eno1np0", Ipv6: "2001:db8::21", DnsWeight: ServicesLbDefaultDnsWeight},
		},
	}
	if !reflect.DeepEqual(inventory.Interfaces, want) {
		t.Fatalf("interfaces = %+v, want %+v", inventory.Interfaces, want)
	}
	if !reflect.DeepEqual(inventory.MoveWeights, map[string]float64{"alpha-edge-1": 1.5}) {
		t.Fatalf("move weights = %+v", inventory.MoveWeights)
	}
}

func TestParseServicesLbInventoryRefusesIncompleteDocuments(t *testing.T) {
	for _, document := range []string{
		// not yaml
		"versions: [",
		// no active version
		"domain: synthetic.example\nversions: []\n",
		// host names cannot be qualified without the domain
		"versions:\n-   lb:\n        interfaces: {}\n",
	} {
		if _, err := ParseServicesLbInventory([]byte(document)); err == nil {
			t.Fatalf("parsed %q", document)
		}
	}
}

func TestActiveEdgeLbInterfacesSkipsNonEdgeHostsAndInterfaces(t *testing.T) {
	byHost, err := ActiveEdgeLbInterfaces(" synthetic.example ", map[string]map[string]ServicesLbInterfaceConfig{
		"beta-edge-2.synthetic.example": {
			"a": {Ipv4: " 192.0.2.30 ", DnsWeight: 7},
			"b": {Transparent: true, Ipv4: "192.0.2.31"},
			"c": {},
		},
		// not an edge
		"beta-api-0.synthetic.example": {
			"a": {Ipv4: "192.0.2.40"},
		},
		// unqualified names keep their own name
		"gamma-edge-3": {
			"a": {Ipv6: "2001:db8::50"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]ServicesLbInterface{
		"beta-edge-2":  {{Interface: "a", Ipv4: "192.0.2.30", DnsWeight: 7}},
		"gamma-edge-3": {{Interface: "a", Ipv6: "2001:db8::50", DnsWeight: ServicesLbDefaultDnsWeight}},
	}
	if !reflect.DeepEqual(byHost, want) {
		t.Fatalf("interfaces = %+v, want %+v", byHost, want)
	}
}
