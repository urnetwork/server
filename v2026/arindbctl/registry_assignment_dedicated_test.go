package main

import (
	"path/filepath"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func TestRegistryDedicatedRequiresServerEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, netname string
		descr         []string
		want          string
	}{
		{"dedicated Internet access", "CORPORATE-DEDICATED-INTERNET-ACCESS", nil, "other"},
		{"dedicated Internet customers", "CUSTOMER-ACCESS", []string{"Network assigned to Internet dedicated customers"}, "other"},
		{"business access", "BUSINESS-SERVICES", []string{"Dedicated to business services including Wireless Local Loop and Leased Lines"}, "other"},
		{"Wi-Fi Internet service", "WIFI-CITY", []string{"Subnet dedicated to Wifi Internet service"}, "other"},
		{"VSAT Internet service", "VSAT-ACCESS", []string{"Subnet dedicated to customers using VSAT Internet service"}, "other"},
		{"university Internet access", "UNIVERSITY", []string{"Dedicated for a university Internet access project"}, "other"},
		{"generic dedicated clients", "CUSTOMER-DEDI", []string{"Dedicated client IPs"}, "other"},
		{"generic dedicated segment", "NETWORK", []string{"Dedicated Segment"}, "other"},
		{"standalone numbered adjective", "DEDICATED2", nil, "other"},
		{"dedicated servers", "DEDI", []string{"Dedicated Servers Range 2"}, "hosting"},
		{"server noun in another description", "DEDICATED", []string{"Customer service", "Servers"}, "hosting"},
		{"numbered server noun", "DEDI2-SERVER01", nil, "hosting"},
		{"server noun alone", "SERVERS", []string{"Customer application servers"}, "other"},
		{"independent hosting token", "DEDICATED-INTERNET", []string{"Hosting customer addresses"}, "hosting"},
		{"independent cloud token", "DEDICATED-CLOUD", nil, "hosting"},
		{"independent VPS token", "DEDI-VPS2", nil, "hosting"},
		{"independent datacenter token", "DEDICATED-DATACENTER", nil, "hosting"},
		{"mixed access remains mixed", "DEDICATED-SERVERS-DSL", nil, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registryAssignmentKind(tc.netname, tc.descr); got != tc.want {
				t.Errorf("assignment kind=%s, want=%s", got, tc.want)
			}
		})
	}
}

// A generic adjective cannot withhold an identified ISP, including a narrow
// customer assignment inside a hosting parent. Other discriminators still win.
func TestRegistryDedicatedAccessRetainsOtherDiscriminators(t *testing.T) {
	catalog := subscriberFixtureCatalogHeader + `registry_assignment_sources:
  - id: rir
    url: https://registry.example/inetnum
    file: assignments.db
    sha256: SHA256_ASSIGNMENTS_DB
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: rpsl
address_risk_sources:
  - id: proxy-address
    url: https://evidence.example/proxy-addresses
    file: proxy.txt
    sha256: SHA256_PROXY_TXT
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: address-list
    category: proxy
    reason: exact synthetic proxy address
` + subscriberFixtureOperators
	dump := `inetnum: 192.0.2.0 - 192.0.2.255
netname: EXAMPLE-HOSTING
descr: Hosting services

inetnum: 192.0.2.0 - 192.0.2.127
netname: DEDICATED-CUSTOMER-ACCESS
descr: Dedicated Internet access customers

inetnum: 192.0.2.128 - 192.0.2.191
netname: DEDI-SERVERS
descr: Dedicated
 servers

inetnum: 192.0.2.192 - 192.0.2.207
netname: DEDICATED-CLOUD
descr: Customer services

inetnum: 192.0.2.208 - 192.0.2.223
netname: CUSTOMER-DEDI
descr: Dedicated client IPs

inetnum: 192.0.2.224 - 192.0.2.239
netname: DEDICATED-INTERNET-ACCESS
descr: Customer Internet access
`
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n", map[string][]byte{"assignments.db": []byte(dump), "proxy.txt": []byte("192.0.2.2\n")}, catalog)
	out, err := fixture.augment(t, "out", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(out, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		address, state, withheld string
		risk                     bool
	}{
		{"192.0.2.1", "subscriber", "", false},
		{"192.0.2.2", "excluded", "", true},
		{"192.0.2.130", "unknown", "registry-hosting-assignment", false},
		{"192.0.2.193", "unknown", "registry-hosting-assignment", false},
		{"192.0.2.209", "subscriber", "", false},
		{"192.0.2.225", "excluded", "", false},
		{"192.0.2.233", "subscriber", "", true},
		{"192.0.2.241", "unknown", "registry-hosting-assignment", false},
	} {
		record := lookupSubscriberRecord(t, db, tc.address)
		withheld, _ := record["origin_withheld_reason"].(mmdbtype.String)
		if record["quality_state"] != mmdbtype.String(tc.state) || string(withheld) != tc.withheld || record["risk"] != mmdbtype.Bool(tc.risk) {
			t.Errorf("%s: unexpected classification or risk: %+v", tc.address, record)
		}
	}
}
