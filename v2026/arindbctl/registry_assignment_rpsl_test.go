package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

// RPSL attribute values may continue on space, tab or '+' lines, with a
// single joining space. End-of-line comments are not part of the value:
// https://docs.db.ripe.net/RIPE-Database-Structure/Attribute-Values/
// Classification must use the whole value, independent of its presentation,
// and must not treat ignored attributes or comments as use evidence.
func TestRegistryAssignmentRPSLFormattingPreservesSubscriberEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, attributes, want string
	}{
		{"single-line access", "netname: EXAMPLE-HOSTING\ndescr: DSL subscriber connections\n", "subscriber"},
		{"space-continued access", "netname: EXAMPLE-HOSTING\ndescr: Example\n DSL subscriber connections\n", "subscriber"},
		{"tab-continued access", "netname: EXAMPLE-HOSTING\ndescr: Example\n\tDSL subscriber connections\n", "subscriber"},
		{"plus-continued access", "netname: EXAMPLE-HOSTING\ndescr: Example\n+\n+ DSL subscriber connections\n", "subscriber"},
		{"continued hosting", "netname: EXAMPLE-NET\ndescr: Example\n dedicated servers\n", "unknown"},
		{"continued ignored attribute", "netname: EXAMPLE-HOSTING\nremarks: Example\n DSL subscriber connections\n", "unknown"},
		{"comment is not hosting", "netname: EXAMPLE-NET\ndescr: Example network # hosting\n", "subscriber"},
		{"comment is not access", "netname: EXAMPLE-HOSTING\ndescr: Example network # DSL\n", "unknown"},
		{"continued comment is not access", "netname: EXAMPLE-HOSTING\ndescr: Example\n+ # DSL subscriber connections\n", "unknown"},
		{"access after eight descriptions", "netname: EXAMPLE-HOSTING\n" + strings.Repeat("descr: Example\n", 8) + "descr: DSL subscriber connections\n", "subscriber"},
		{"hosting after eight descriptions", "netname: EXAMPLE-NET\n" + strings.Repeat("descr: Example\n", 8) + "descr: dedicated servers\n", "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := subscriberFixtureCatalogHeader + `registry_assignment_sources:
  - id: ripe
    url: https://ftp.ripe.net/ripe/dbase/split/ripe.db.inetnum.gz
    file: ripe.db
    sha256: SHA256_RIPE_DB
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: rpsl
` + subscriberFixtureOperators
			dump := "inetnum: 192.0.2.0 - 192.0.2.63\n" + tc.attributes
			fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n", map[string][]byte{"ripe.db": []byte(dump)}, catalog)
			out, err := fixture.augment(t, "out", "")
			if err != nil {
				t.Fatal(err)
			}
			db, err := mmdb.Open(filepath.Join(out, "arin.mmdb"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			record := lookupSubscriberRecord(t, db, "192.0.2.1")
			if record["quality_state"] != mmdbtype.String(tc.want) || record["risk"] != mmdbtype.Bool(false) {
				t.Fatalf("want state %s without risk, got %+v", tc.want, record)
			}
			if tc.want == "unknown" && record["origin_withheld_reason"] != mmdbtype.String("registry-hosting-assignment") {
				t.Fatalf("hosting assignment lost its withholding reason: %+v", record)
			}
			outside := lookupSubscriberRecord(t, db, "192.0.2.100")
			if outside["quality_state"] != mmdbtype.String("subscriber") {
				t.Fatalf("assignment escaped its boundary: %+v", outside)
			}
		})
	}
}
