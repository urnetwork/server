package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func subscriberFixtureGzip(t *testing.T, at time.Time, body string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	w := gzip.NewWriter(&buffer)
	if _, err := fmt.Fprintf(w, "%% This file was generated at %s.\n%s", at.Format("Mon Jan _2 15:04:05 MST 2006"), body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func subscriberFixtureRecord(state string, risk bool) mmdbtype.Map {
	return mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String(state), "non_quality": mmdbtype.Bool(state != "subscriber"), "risk": mmdbtype.Bool(risk), "geographic_risk": mmdbtype.Bool(risk), "org_handle": mmdbtype.String("TEST-UNREVIEWED-CHILD")}
}

func subscriberFixtureRoute(peers uint32, asns ...uint32) subscriberOriginRoute {
	route := subscriberOriginRoute{rpki: "unchecked"}
	for _, asn := range asns {
		route.addOrigin(asn, peers)
	}
	route.visibility = route.ownVisibility()
	return route
}

func subscriberFixtureByASN() map[uint32][]subscriberOperator {
	return map[uint32][]subscriberOperator{
		64500: {{Id: "access", Usage: "subscriber", Source: "https://evidence.example/access"}},
		64502: {{Id: "proxy", Usage: "virtual_isp", Source: "https://evidence.example/proxy"}},
		64503: {{Id: "host", Usage: "hosting", Source: "https://evidence.example/host"}},
		64504: {{Id: "transit", Usage: "transit", Source: "https://evidence.example/transit"}},
	}
}

func TestSubscriberOriginCleanDefaultKeepsEveryAdditionalDiscriminator(t *testing.T) {
	byASN := subscriberFixtureByASN()
	for _, tc := range []struct {
		name, prior, want string
		asns              []uint32
		risk              bool
	}{
		{"unreviewed customer of identified ISP", "unknown", "subscriber", []uint32{64500}, false},
		{"unidentified operator", "unknown", "unknown", []uint32{64501}, false},
		{"explicit registration hosting veto", "excluded", "excluded", []uint32{64500}, false},
		{"registration conflict", "ambiguous", "ambiguous", []uint32{64500}, false},
		{"known hosting origin", "subscriber", "excluded", []uint32{64503}, false},
		{"known transit origin", "unknown", "excluded", []uint32{64504}, false},
		{"proxy origin", "subscriber", "excluded", []uint32{64502}, true},
		{"incomparable known and unknown origins", "unknown", "ambiguous", []uint32{64500, 64501}, false},
		{"known negative origin wins", "unknown", "excluded", []uint32{64500, 64503}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := subscriberFixtureRecord(tc.prior, false)
			got, err := augmentSubscriberRecord(base, subscriberOriginDecision(subscriberFixtureRoute(50, tc.asns...), byASN, defaultMinimumOriginPeers))
			if err != nil {
				t.Fatal(err)
			}
			if got["quality_state"] != mmdbtype.String(tc.want) || got["non_quality"] != mmdbtype.Bool(tc.want != "subscriber") || got["risk"] != mmdbtype.Bool(tc.risk) {
				t.Fatalf("unexpected decision: %+v", got)
			}
			if base["quality_state"] != mmdbtype.String(tc.prior) || base["risk"] != mmdbtype.Bool(false) {
				t.Fatal("augmentation mutated the base record")
			}
		})
	}
	base := subscriberFixtureRecord("unknown", true)
	base["network_risk"], base["network_risk_evidence"] = mmdbtype.Bool(true), mmdbtype.Slice{mmdbtype.Map{"category": mmdbtype.String("proxy")}}
	got, err := augmentSubscriberRecord(base, subscriberOriginDecision(subscriberFixtureRoute(50, 64500), byASN, defaultMinimumOriginPeers))
	if err != nil || got["risk"] != mmdbtype.Bool(true) || got["network_risk"] != mmdbtype.Bool(true) || len(got["network_risk_evidence"].(mmdbtype.Slice)) != 1 || got["geographic_risk"] != mmdbtype.Bool(true) {
		t.Fatal("inferred clean access cleared existing risk")
	}
	if _, err := augmentSubscriberRecord(got, nil); err == nil {
		t.Fatal("a previous augmented database retained stale inferred approval")
	}
	byASN[64500] = append(byASN[64500], subscriberOperator{Id: "same-asn-hosting", Usage: "hosting", Source: "https://evidence.example/mixed-use"})
	if subscriberOriginDecision(subscriberFixtureRoute(50, 64500), byASN, defaultMinimumOriginPeers)["state"] != mmdbtype.String("excluded") {
		t.Fatal("identified ISP waived a same-ASN use discriminator")
	}
}

// Visibility and origin validity withhold only the positive inference. A
// withheld identity is recorded for review and never changes the base state;
// negative evidence applies at any visibility.
func TestSubscriberOriginWithholdsInferenceWithoutVisibilityOrAuthorization(t *testing.T) {
	byASN := subscriberFixtureByASN()
	for _, tc := range []struct {
		name, prior, want, reason, rpki string
		peers                           uint32
		asns                            []uint32
	}{
		{"single-peer subscriber route", "unknown", "unknown", "insufficient-origin-visibility", "unchecked", 1, []uint32{64500}},
		{"visible subscriber route", "unknown", "subscriber", "", "unchecked", 10, []uint32{64500}},
		{"rpki-invalid subscriber route", "unknown", "unknown", "rpki-invalid-origin", "invalid", 300, []uint32{64500}},
		{"rpki-valid subscriber route", "unknown", "subscriber", "", "valid", 300, []uint32{64500}},
		{"rpki-not-found subscriber route", "unknown", "subscriber", "", "not-found", 300, []uint32{64500}},
		{"direct approval survives withholding", "subscriber", "subscriber", "insufficient-origin-visibility", "unchecked", 1, []uint32{64500}},
		{"single-peer hosting origin still excludes", "subscriber", "excluded", "", "unchecked", 1, []uint32{64503}},
		{"rpki-invalid proxy origin still excludes", "unknown", "excluded", "", "invalid", 1, []uint32{64502}},
		{"single-peer unknown origin still blocks", "unknown", "ambiguous", "", "unchecked", 1, []uint32{64500, 64501}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := subscriberFixtureRoute(tc.peers, tc.asns...)
			route.rpki = tc.rpki
			decision := subscriberOriginDecision(route, byASN, defaultMinimumOriginPeers)
			got, err := augmentSubscriberRecord(subscriberFixtureRecord(tc.prior, false), decision)
			if err != nil {
				t.Fatal(err)
			}
			reason, _ := got["origin_withheld_reason"].(mmdbtype.String)
			if got["quality_state"] != mmdbtype.String(tc.want) || string(reason) != tc.reason || got["origin_peers"] != mmdbtype.Uint32(tc.peers) {
				t.Fatalf("unexpected decision: %+v", got)
			}
			if tc.reason != "" && (got["origin_use_state"] != mmdbtype.String("withheld") || got["subscriber_evidence_kind"] != nil || decision["risk_evidence"] != nil) {
				t.Fatalf("withheld identity leaked an inference: %+v", got)
			}
			if tc.rpki != "unchecked" && got["origin_rpki_validity"] != mmdbtype.String(tc.rpki) {
				t.Fatal("origin validity was not retained")
			}
		})
	}
}

// An operator's own more-specific inherits its visible aggregate; a route with a
// different origin set, or without a covering route, stands on its own count.
func TestSubscriberOriginVisibilityInheritsOnlyIdenticalAggregates(t *testing.T) {
	at := time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC)
	routes := map[netip.Prefix]subscriberOriginRoute{}
	data := subscriberFixtureGzip(t, at, "64500 192.0.2.0/24 300\n64500 192.0.2.0/25 1\n64500 192.0.2.0/26 2\n64501 192.0.2.128/25 1\n64500 192.0.2.128/26 1\n64500 198.51.100.0/24 3\n{64500,64501} 203.0.113.0/24 7\n64500 203.0.113.0/24 2\n64509 192.0.2.64/26 1\n64501 198.51.100.0/25 1\n")
	if _, _, err := readSubscriberOrigins(t.Context(), bytes.NewReader(data), at, routes); err != nil {
		t.Fatal(err)
	}
	byASN := subscriberFixtureByASN()
	byASN[64509] = byASN[64500] // a sibling ASN of the same reviewed operator
	if _, err := resolveSubscriberOriginEvidence(t.Context(), routes, nil, byASN); err != nil {
		t.Fatal(err)
	}
	for prefix, want := range map[string]uint32{"192.0.2.0/24": 300, "192.0.2.0/25": 300, "192.0.2.0/26": 300, "192.0.2.64/26": 300, "192.0.2.128/25": 1, "192.0.2.128/26": 1, "198.51.100.0/24": 3, "198.51.100.0/25": 1, "203.0.113.0/24": 7} {
		if got := routes[netip.MustParsePrefix(prefix)].visibility; got != want {
			t.Fatalf("%s: visibility=%d want=%d", prefix, got, want)
		}
	}
	// Merged equal-prefix origins keep each origin's own best peer count.
	if route := routes[netip.MustParsePrefix("203.0.113.0/24")]; len(route.origins) != 2 || route.origins[0].peers != 7 || route.origins[1].peers != 7 {
		t.Fatalf("merged origins lost peer counts: %+v", route)
	}
}

func TestSubscriberOriginSnapshotRejectsMalformedStaleAndPartialEvidence(t *testing.T) {
	at := time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC)
	valid := subscriberFixtureGzip(t, at.Add(-time.Hour), "64500 192.0.2.0/24 4\n64501 192.0.2.0/24 3\n{64500,64501} 192.0.2.128/25 2\n64500 0.0.0.0/0 1\n64502 ::ffff:192.0.2.0/120 1\n")
	routes := map[netip.Prefix]subscriberOriginRoute{}
	if rows, _, err := readSubscriberOrigins(t.Context(), bytes.NewReader(valid), at, routes); err != nil || rows != 5 || len(routes) != 2 || len(routes[netip.MustParsePrefix("192.0.2.0/24")].origins) != 2 {
		t.Fatalf("complete snapshot failed: rows=%d err=%v", rows, err)
	}
	for _, data := range [][]byte{
		valid[:len(valid)-5],
		subscriberFixtureGzip(t, at.Add(-49*time.Hour), "64500 192.0.2.0/24 1\n"),
		subscriberFixtureGzip(t, at.Add(time.Second), "64500 192.0.2.0/24 1\n"),
		subscriberFixtureGzip(t, at, "64500 192.0.2.1/24 1\n"),
		subscriberFixtureGzip(t, at, "64500 192.0.2.0/24 0\n"),
		subscriberFixtureGzip(t, at, "64500 192.0.2.0/24\n"),
		subscriberFixtureGzip(t, at, "{64500,x} 192.0.2.0/24 1\n"),
		subscriberFixtureGzip(t, at, ""),
	} {
		if _, _, err := readSubscriberOrigins(t.Context(), bytes.NewReader(data), at, map[netip.Prefix]subscriberOriginRoute{}); err == nil {
			t.Fatal("bad origin snapshot was accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := readSubscriberOrigins(ctx, bytes.NewReader(valid), at, map[netip.Prefix]subscriberOriginRoute{}); err == nil {
		t.Fatal("cancelled origin read continued")
	}
}

func TestSubscriberOriginIgnoresEveryMMDBIPv4Alias(t *testing.T) {
	at := time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC)
	for _, prefix := range []string{"::/80", "::192.0.2.0/120", "::ffff:192.0.2.0/120", "2001::/32", "2001:0:c000::/48", "2002::/16", "2002:c000:200::/48"} {
		if !subscriberOriginAliasesIPv4(netip.MustParsePrefix(prefix)) {
			t.Fatal("IPv4 alias observation was not excluded")
		}
		routes := map[netip.Prefix]subscriberOriginRoute{}
		data := subscriberFixtureGzip(t, at, "64500 192.0.2.0/24 1\n64503 "+prefix+" 1\n")
		if rows, _, err := readSubscriberOrigins(t.Context(), bytes.NewReader(data), at, routes); err != nil || rows != 2 || len(routes) != 1 {
			t.Fatal("IPv4 alias observation contaminated native origin data")
		}
	}
	for _, prefix := range []string{"192.0.2.0/24", "2000::/3", "2001:db8::/32", "2001:100::/32", "2003::/16"} {
		if subscriberOriginAliasesIPv4(netip.MustParsePrefix(prefix)) {
			t.Fatal("native origin network was treated as an IPv4 alias")
		}
	}
}

type subscriberBuildFixture struct {
	dir, base, catalog, geo string
	at                      time.Time
	catalogText             string
	routePath               string
	routes                  []byte
}

// Synthetic registration base with excluded and risky pockets, pinned routes,
// and a catalog whose placeholders the test fills with exact snapshot hashes.
func newSubscriberBuildFixture(t *testing.T, routeTable string, extraSources map[string][]byte, catalogTemplate string) *subscriberBuildFixture {
	t.Helper()
	dir := t.TempDir()
	at := time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC)
	writer, err := mmdbwriter.New(mmdbwriter.Options{BuildEpoch: at.Add(-time.Hour).Unix(), DatabaseType: "urnetwork arindb", IncludeReservedNetworks: true, RecordSize: 32, Description: map[string]string{"en": "synthetic registration base"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		prefix, state string
		risk          bool
	}{
		{"0.0.0.0/0", "unknown", false}, {"::/0", "unknown", false},
		{"192.0.2.224/29", "excluded", false}, {"192.0.2.232/29", "unknown", true},
	} {
		_, network, _ := net.ParseCIDR(row.prefix)
		if err := writer.Insert(network, subscriberFixtureRecord(row.state, row.risk)); err != nil {
			t.Fatal(err)
		}
	}
	var baseBytes bytes.Buffer
	if _, err := writer.WriteTo(&baseBytes); err != nil {
		t.Fatal(err)
	}
	fixture := &subscriberBuildFixture{dir: dir, at: at}
	fixture.base = writeTestInput(t, filepath.Join(dir, "base.mmdb"), baseBytes.Bytes())
	fixture.geo = writeTestInput(t, filepath.Join(dir, "geo.mmdb"), testGeoDatabase(t))
	fixture.routes = subscriberFixtureGzip(t, at.Add(-time.Hour), routeTable)
	fixture.routePath = writeTestInput(t, filepath.Join(dir, "origins.gz"), fixture.routes)
	hashes := map[string]string{"ROUTES": fixtureSha256(fixture.routes)}
	for name, content := range extraSources {
		writeTestInput(t, filepath.Join(dir, name), content)
		hashes[strings.ToUpper(strings.ReplaceAll(name, ".", "_"))] = fixtureSha256(content)
	}
	text := strings.ReplaceAll(catalogTemplate, "OBSERVED", at.Add(-time.Hour).Format(time.RFC3339))
	text = strings.ReplaceAll(text, "EXPIRES", at.Add(time.Hour).Format(time.RFC3339))
	for key, hash := range hashes {
		text = strings.ReplaceAll(text, "SHA256_"+key, hash)
	}
	fixture.catalogText = text
	fixture.catalog = writeTestInput(t, filepath.Join(dir, "catalog.yml"), []byte(text))
	return fixture
}

func fixtureSha256(content []byte) string {
	hash := sha256.Sum256(content)
	return hex.EncodeToString(hash[:])
}

func (self *subscriberBuildFixture) augment(t *testing.T, name string, geolite2 string) (string, error) {
	t.Helper()
	out := filepath.Join(self.dir, name)
	return out, publishDirectory(out, func(stage string) error {
		return augmentSubscriberDatabase(t.Context(), self.base, self.catalog, geolite2, stage, self.at)
	})
}

func lookupSubscriberRecord(t *testing.T, db *mmdb.Reader, address string) mmdbtype.Map {
	t.Helper()
	var record mmdbtype.Map
	if err := db.Lookup(netip.MustParseAddr(address)).Decode(&record); err != nil {
		t.Fatal(err)
	}
	return record
}

const subscriberFixtureCatalogHeader = `version: 1
policy: identified-subscriber-default
minimum_origin_peers: 3
origin_sources:
  - id: synthetic-ris
    url: https://evidence.example/ris.gz
    file: origins.gz
    sha256: SHA256_ROUTES
    observed_at: OBSERVED
    expires_at: EXPIRES
`

const subscriberFixtureOperators = `operators:
  - {id: access, name: Synthetic Subscriber ISP, asns: [64500], usage: subscriber, source: 'https://evidence.example/access', countries: [IN]}
  - {id: proxy, name: Synthetic Proxy, asns: [64502], usage: virtual_isp, source: 'https://evidence.example/proxy', countries: [US]}
  - {id: host, name: Synthetic Hosting, asns: [64503], usage: hosting, source: 'https://evidence.example/hosting', countries: [GB]}
  - {id: transit, name: Synthetic Transit, asns: [64504], usage: transit, source: 'https://evidence.example/transit', countries: [ZA]}
`

func TestSubscriberOriginBuildGlobalPrefixesAndPreservesBaseCoverage(t *testing.T) {
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 4\n64501 192.0.2.64/26 3\n64502 192.0.2.128/27 4\n64503 192.0.2.160/28 4\n64504 192.0.2.176/28 4\n64500 192.0.2.192/28 4\n64501 192.0.2.192/28 4\n64500 192.0.2.208/28 1\n64500 2001:db8::/32 4\n64500 0.0.0.0/0 1\n64500 ::ffff:192.0.2.64/122 1\n64503 ::ffff:192.0.2.0/123 1\n64500 198.51.100.0/24 2\n", nil, subscriberFixtureCatalogHeader+subscriberFixtureOperators)
	out, err := fixture.augment(t, "out", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(out, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for n := 0; n < 256; n++ {
		state, risk := "subscriber", false
		switch {
		case 64 <= n && n < 128:
			state = "unknown"
		case 128 <= n && n < 160:
			state, risk = "excluded", true
		case 160 <= n && n < 192:
			state = "excluded"
		case 192 <= n && n < 208:
			state = "ambiguous"
		case 224 <= n && n < 232:
			state = "excluded"
		case 232 <= n && n < 240:
			risk = true
		}
		record := lookupSubscriberRecord(t, db, fmt.Sprintf("192.0.2.%d", n))
		if record["quality_state"] != mmdbtype.String(state) || record["risk"] != mmdbtype.Bool(risk) || record["org_handle"] != mmdbtype.String("TEST-UNREVIEWED-CHILD") {
			t.Fatalf("offset=%d lost subscriber/veto/registration facts: %+v", n, record)
		}
		// The operator's single-peer more-specific inherits its visible aggregate.
		if 208 <= n && n < 224 && record["origin_peers"] != mmdbtype.Uint32(4) {
			t.Fatalf("offset=%d own more-specific lost aggregate visibility: %+v", n, record)
		}
	}
	for _, tc := range []struct{ address, state, withheld string }{{"198.51.100.1", "unknown", "insufficient-origin-visibility"}, {"203.0.113.1", "unknown", ""}, {"2001:db8::1", "subscriber", ""}, {"2001:db9::1", "unknown", ""}} {
		record := lookupSubscriberRecord(t, db, tc.address)
		withheld, _ := record["origin_withheld_reason"].(mmdbtype.String)
		if record["quality_state"] != mmdbtype.String(tc.state) || string(withheld) != tc.withheld {
			t.Fatalf("global coverage mismatch at %s: %+v", tc.address, record)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Withheld     map[string]int `json:"withheld_partitions"`
		MinimumPeers uint32         `json:"minimum_origin_peers"`
	}
	if err := json.Unmarshal(manifest, &parsed); err != nil || parsed.MinimumPeers != 3 || parsed.Withheld["insufficient-origin-visibility"] != 1 {
		t.Fatalf("manifest lacks visibility provenance: %s", manifest)
	}
	if err := os.WriteFile(fixture.routePath, append(fixture.routes, 1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.augment(t, "changed", ""); err == nil {
		t.Fatal("changed source snapshot was published")
	}
	if _, err := fixture.augment(t, "unused-geo", fixture.geo); err == nil {
		t.Fatal("geolite2 input without a reviewed country policy was accepted")
	}
	for _, invalid := range []string{
		strings.Replace(fixture.catalogText, "usage: subscriber", "usage: unknown", 1),
		strings.Replace(fixture.catalogText, "asns: [64500]", "asns: [64512]", 1),
		strings.Replace(fixture.catalogText, "file: origins.gz", "file: ../origins.gz", 1),
		strings.Replace(fixture.catalogText, "minimum_origin_peers: 3", "minimum_origin_peers: 0", 1),
		strings.Replace(fixture.catalogText, "policy: identified-subscriber-default", "policy: identified-subscriber-default\norigin_country_policy: trust-catalog-countries", 1),
	} {
		p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(invalid))
		if _, err := loadSubscriberOriginCatalog(p); err == nil {
			t.Fatal("invalid reviewed identity, snapshot path or policy accepted")
		}
	}
}

// The reviewed-country discriminator withholds an inference only where the
// associated geography is outside every identified operator's review context,
// at that geography's own boundary, and records the country it observed.
func TestSubscriberOriginWithholdsInferenceOutsideReviewedCountries(t *testing.T) {
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n64503 192.0.2.160/28 40\n", nil, strings.Replace(subscriberFixtureCatalogHeader, "policy: identified-subscriber-default", "policy: identified-subscriber-default\norigin_country_policy: withhold-outside-reviewed-countries", 1)+strings.Replace(subscriberFixtureOperators, "countries: [IN]", "countries: [US]", 1))
	if _, err := fixture.augment(t, "missing-geo", ""); err == nil {
		t.Fatal("country policy without geolite2 was accepted")
	}
	out, err := fixture.augment(t, "out", fixture.geo)
	if err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(out, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct{ address, state, withheld, country string }{
		{"192.0.2.1", "subscriber", "", ""}, {"192.0.2.127", "subscriber", "", ""},
		{"192.0.2.128", "unknown", "outside-reviewed-countries", "ca"}, {"192.0.2.159", "unknown", "outside-reviewed-countries", "ca"},
		{"192.0.2.165", "excluded", "", ""}, {"192.0.2.200", "unknown", "outside-reviewed-countries", "ca"},
		{"192.0.2.225", "excluded", "", ""}, {"192.0.2.233", "unknown", "outside-reviewed-countries", "ca"},
	} {
		record := lookupSubscriberRecord(t, db, tc.address)
		withheld, _ := record["origin_withheld_reason"].(mmdbtype.String)
		country, _ := record["associated_country"].(mmdbtype.String)
		if record["quality_state"] != mmdbtype.String(tc.state) || string(withheld) != tc.withheld || string(country) != tc.country || record["org_handle"] != mmdbtype.String("TEST-UNREVIEWED-CHILD") {
			t.Fatalf("%s: %+v", tc.address, record)
		}
		if tc.withheld != "" && (record["subscriber_evidence_kind"] != nil || record["origin_operator_ids"] == nil || record["non_quality"] != mmdbtype.Bool(true)) {
			t.Fatalf("%s: withheld record leaked inference: %+v", tc.address, record)
		}
	}
	if risk := lookupSubscriberRecord(t, db, "192.0.2.233"); risk["risk"] != mmdbtype.Bool(true) {
		t.Fatal("withheld record cleared existing risk")
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Withheld map[string]int    `json:"withheld_partitions"`
		Policy   string            `json:"origin_country_policy"`
		Inputs   map[string]string `json:"inputs_sha256"`
	}
	if err := json.Unmarshal(manifest, &parsed); err != nil || parsed.Policy != originCountryPolicyWithhold || parsed.Withheld["outside-reviewed-countries"] == 0 || parsed.Inputs["geolite2"] == "" {
		t.Fatalf("manifest lacks country policy provenance: %s", manifest)
	}
}

// Address-level findings apply on top of any classification: they add
// independent risk, exclude subscriber use at that exact address, and keep
// every surrounding address unchanged.
func TestSubscriberAddressRiskListsExcludeExactAddressesOnly(t *testing.T) {
	exits := []byte("ExitNode ABC\nPublished 2026-10-04 04:01:42\nExitAddress 192.0.2.10 2026-10-04 05:00:00\nExitNode DEF\nExitAddress 192.0.2.226 2026-10-04 05:00:00\nExitAddress 2001:db8::10 2026-10-04 05:00:00\n")
	feed := []byte("# relay egress\n192.0.2.64/27,US,US-CA,Los Angeles,\n198.51.100.0/24,GB,GB-ENG,London,\n")
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, _ = zip.Write([]byte("192.0.2.40\n# comment\n\n203.0.113.0/24\n"))
	_ = zip.Close()
	catalog := subscriberFixtureCatalogHeader + `address_risk_sources:
  - id: tor-exits
    url: https://check.torproject.org/exit-addresses
    file: exits.txt
    sha256: SHA256_EXITS_TXT
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: tor-exit-addresses
    category: tor
    reason: measured Tor exit egress address
  - id: relay-feed
    url: https://relay.example/egress.csv
    file: feed.csv
    sha256: SHA256_FEED_CSV
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: rfc8805-geofeed
    category: vpn
    reason: operator-published relay egress ranges
  - id: proxy-list
    url: https://proxy.example/list.gz
    file: list.gz
    sha256: SHA256_LIST_GZ
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: address-list
    category: proxy
    reason: reviewed proxy exits
` + subscriberFixtureOperators
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n64500 2001:db8::/32 40\n", map[string][]byte{"exits.txt": exits, "feed.csv": feed, "list.gz": compressed.Bytes()}, catalog)
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
		address, state, category string
		risk                     bool
	}{
		{"192.0.2.9", "subscriber", "", false}, {"192.0.2.10", "excluded", "tor", true}, {"192.0.2.11", "subscriber", "", false},
		{"192.0.2.63", "subscriber", "", false}, {"192.0.2.64", "excluded", "vpn", true}, {"192.0.2.95", "excluded", "vpn", true}, {"192.0.2.96", "subscriber", "", false},
		{"192.0.2.40", "excluded", "proxy", true}, {"192.0.2.226", "excluded", "tor", true}, {"192.0.2.227", "excluded", "", false},
		{"198.51.100.7", "excluded", "vpn", true}, {"203.0.113.9", "excluded", "proxy", true}, {"203.0.114.1", "unknown", "", false},
		{"2001:db8::10", "excluded", "tor", true}, {"2001:db8::11", "subscriber", "", false},
	} {
		record := lookupSubscriberRecord(t, db, tc.address)
		if record["quality_state"] != mmdbtype.String(tc.state) || record["non_quality"] != mmdbtype.Bool(tc.state != "subscriber") || record["risk"] != mmdbtype.Bool(tc.risk) {
			t.Fatalf("%s: %+v", tc.address, record)
		}
		evidence, _ := record["network_risk_evidence"].(mmdbtype.Slice)
		if tc.category == "" {
			if len(evidence) != 0 || record["address_risk_source_ids"] != nil {
				t.Fatalf("%s: unrelated address acquired risk: %+v", tc.address, record)
			}
			continue
		}
		if len(evidence) != 1 || evidence[0].(mmdbtype.Map)["category"] != mmdbtype.String(tc.category) || record["network_risk"] != mmdbtype.Bool(true) || record["subscriber_evidence_kind"] != nil || len(record["address_risk_source_ids"].(mmdbtype.Slice)) != 1 {
			t.Fatalf("%s: address finding lost provenance: %+v", tc.address, record)
		}
		if _, err := augmentSubscriberRecord(record, nil); err == nil {
			t.Fatal("address-level findings were accepted as an unaugmented base")
		}
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Entries int               `json:"address_risk_entries"`
		Inputs  map[string]string `json:"inputs_sha256"`
	}
	if err := json.Unmarshal(manifest, &parsed); err != nil || parsed.Entries != 7 || parsed.Inputs["address_risk/tor-exits"] != fixtureSha256(exits) {
		t.Fatalf("manifest lacks address risk provenance: %s", manifest)
	}
	for _, bad := range []string{"192.0.2.1/24\n", "::ffff:192.0.2.1\n", "0.0.0.0/0\n", "not-an-address\n", "\n# only comments\n"} {
		if _, err := readAddressRiskList(t.Context(), "address-list", strings.NewReader(bad)); err == nil {
			t.Fatalf("malformed address list %q was accepted", bad)
		}
	}
	for _, invalid := range []string{
		strings.Replace(fixture.catalogText, "category: tor", "category: hosting", 1),
		strings.Replace(fixture.catalogText, "format: tor-exit-addresses", "format: onionoo", 1),
		strings.Replace(fixture.catalogText, "    reason: measured Tor exit egress address\n", "", 1),
		strings.Replace(fixture.catalogText, "id: tor-exits", "id: synthetic-ris", 1),
	} {
		p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(invalid))
		if _, err := loadSubscriberOriginCatalog(p); err == nil {
			t.Fatal("invalid address risk source accepted")
		}
	}
}

// Both published VRP formats produce the same RFC 6811 validity, and only an
// invalid origin withholds the inference in a full build.
func TestSubscriberRpkiSnapshotsWithholdInvalidOrigins(t *testing.T) {
	jsonSnapshot := []byte(`{"metadata":{"buildtime":"2026-10-04T05:00:00Z"},"roas":[{"asn":64500,"prefix":"192.0.2.0/24","maxLength":25,"ta":"test"},{"asn":"AS64510","prefix":"198.51.100.0/24","maxLength":24,"ta":"test"},{"asn":0,"prefix":"203.0.113.128/25","maxLength":25,"ta":"test"}],"aspas":[]}`)
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, _ = zip.Write([]byte("URI,ASN,IP Prefix,Max Length,Not Before,Not After\nrsync://x/a.roa,AS64500,2001:db8::/32,48,2026-01-01 00:00:00,2027-01-01 00:00:00\n"))
	_ = zip.Close()
	catalog := subscriberFixtureCatalogHeader + `rpki_sources:
  - id: rpki-json
    url: https://rpki.example/rpki.json
    file: rpki.json
    sha256: SHA256_RPKI_JSON
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: rpki-client-json
  - id: rpki-csv
    url: https://rpki.example/roas.csv.gz
    file: roas.csv.gz
    sha256: SHA256_ROAS_CSV_GZ
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: routinator-csv
` + subscriberFixtureOperators
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n64500 192.0.2.0/25 40\n64500 192.0.2.128/26 40\n64500 198.51.100.0/24 40\n64500 203.0.113.0/24 40\n64500 2001:db8::/32 40\n64500 2001:db8:1::/48 40\n64500 2001:db8:1:1::/64 40\n64503 198.51.100.128/25 40\n64500 192.0.2.192/27 40\n64501 192.0.2.160/27 40\n", map[string][]byte{"rpki.json": jsonSnapshot, "roas.csv.gz": compressed.Bytes()}, catalog)
	authorizations, err := loadRpkiAuthorizations(t.Context(), fixture.catalog, mustLoadCatalog(t, fixture.catalog).RpkiSources)
	if err != nil || authorizations.count != 4 {
		t.Fatalf("rpki snapshots failed: %v", err)
	}
	for prefix, want := range map[string]string{"192.0.2.0/24": "valid", "192.0.2.0/25": "valid", "192.0.2.128/26": "invalid", "198.51.100.0/24": "invalid", "203.0.113.0/24": "not-found", "203.0.113.128/25": "invalid", "2001:db8::/32": "valid", "2001:db8:1::/48": "valid", "2001:db8:1:1::/64": "invalid"} {
		if got := authorizations.validity(netip.MustParsePrefix(prefix), 64500); got != want {
			t.Fatalf("%s: validity=%s want=%s", prefix, got, want)
		}
	}
	if authorizations.routeValidity(netip.MustParsePrefix("198.51.100.0/24"), subscriberFixtureRoute(5, 64500, 64510)) != "invalid" {
		t.Fatal("a mixed valid/invalid origin set was not invalid")
	}
	out, err := fixture.augment(t, "out", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.Open(filepath.Join(out, "arin.mmdb"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct{ address, state, withheld, validity string }{
		{"192.0.2.1", "subscriber", "", "valid"}, {"192.0.2.129", "subscriber", "", "valid-aggregate"}, {"192.0.2.200", "subscriber", "", "valid-aggregate"}, {"192.0.2.161", "unknown", "", "invalid"},
		{"198.51.100.1", "unknown", "rpki-invalid-origin", "invalid"}, {"198.51.100.129", "excluded", "", "invalid"}, {"203.0.113.1", "subscriber", "", "not-found"},
		{"2001:db8::1", "subscriber", "", "valid"}, {"2001:db8:1:1::1", "subscriber", "", "valid-aggregate"},
	} {
		record := lookupSubscriberRecord(t, db, tc.address)
		withheld, _ := record["origin_withheld_reason"].(mmdbtype.String)
		validity, _ := record["origin_rpki_validity"].(mmdbtype.String)
		if record["quality_state"] != mmdbtype.String(tc.state) || string(withheld) != tc.withheld || (tc.state != "unknown" || tc.withheld != "") && string(validity) != tc.validity {
			t.Fatalf("%s: %+v", tc.address, record)
		}
	}
	for _, bad := range []string{`{"roas":[{"asn":64500,"prefix":"192.0.2.0/24","maxLength":23}]}`, `{"roas":[{"asn":"x","prefix":"192.0.2.0/24","maxLength":24}]}`, `{"metadata":{}}`, `{"roas":[{"asn":64500,"prefix":"192.0.2.0/24","maxLength":24}]`} {
		if err := readRpkiClientJson(t.Context(), strings.NewReader(bad), &rpkiAuthorizations{}); err == nil {
			t.Fatalf("malformed rpki json %q accepted", bad)
		}
	}
	for _, bad := range []string{"ASN,IP Prefix\nAS1,192.0.2.0/24\n", "ASN,IP Prefix,Max Length\n", "ASN,IP Prefix,Max Length\nAS64500,192.0.2.1/24,24\n"} {
		if err := readRoutinatorCsv(t.Context(), strings.NewReader(bad), &rpkiAuthorizations{}); err == nil {
			t.Fatalf("malformed rpki csv %q accepted", bad)
		}
	}
	p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(strings.Replace(fixture.catalogText, "format: rpki-client-json", "format: rpki-rtr", 1)))
	if _, err := loadSubscriberOriginCatalog(p); err == nil {
		t.Fatal("unsupported rpki format accepted")
	}
}

func mustLoadCatalog(t *testing.T, path string) subscriberOriginCatalog {
	t.Helper()
	catalog, err := loadSubscriberOriginCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

// The audit flags identities whose routed geography contradicts their reviewed
// countries, ASNs that originate nothing, and queues unreviewed origins per
// associated country without ever approving them.
func TestSubscriberCatalogAuditFlagsIdentitiesAndQueuesUnreviewedOrigins(t *testing.T) {
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/25 40\n64505 192.0.2.128/25 40\n64501 192.0.2.0/26 40\n64501 203.0.113.0/24 1\n64506 192.0.2.128/26 40\n{64500,64505} 2001:db8::/32 40\n", nil, subscriberFixtureCatalogHeader+subscriberFixtureOperators+"  - {id: misplaced, name: Misplaced ISP, asns: [64505, 64507], usage: subscriber, source: 'https://evidence.example/misplaced', countries: [US]}\n")
	if err := publishDirectory(filepath.Join(fixture.dir, "no-geo"), func(stage string) error {
		return auditSubscriberCatalog(t.Context(), fixture.catalog, "", stage, fixture.at)
	}); err == nil {
		t.Fatal("audit without geolite2 was accepted")
	}
	out := filepath.Join(fixture.dir, "audit")
	if err := publishDirectory(out, func(stage string) error {
		return auditSubscriberCatalog(t.Context(), fixture.catalog, fixture.geo, stage, fixture.at)
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(out, "catalog-audit.json"))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Operators []subscriberCatalogAuditOperator              `json:"operators"`
		Queue     map[string][]subscriberCatalogReviewCandidate `json:"unreviewed_review_queue_by_country"`
		Withheld  map[string]int                                `json:"withheld_routes"`
		Summary   map[string]int                                `json:"summary"`
	}
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	byId := map[string]subscriberCatalogAuditOperator{}
	for _, operator := range report.Operators {
		byId[operator.Id] = operator
	}
	access, misplaced, host := byId["access"], byId["misplaced"], byId["host"]
	if access.Routes != 2 || access.Ipv4Addresses != 128 || access.OutsideReviewedShare != 1 || !strings.Contains(strings.Join(access.Flags, " "), "identity-review-suggested") || access.SharedOriginRoutes != 1 {
		t.Fatalf("access audit: %+v", access)
	}
	if misplaced.Routes != 2 || misplaced.OutsideReviewedShare != 1 || misplaced.CountryShares["ca"] != 1 || len(misplaced.UnobservedASNs) != 1 || misplaced.UnobservedASNs[0] != 64507 {
		t.Fatalf("misplaced audit: %+v", misplaced)
	}
	if host.Routes != 0 || !strings.Contains(strings.Join(host.Flags, " "), "no-observed-routes") {
		t.Fatalf("host audit: %+v", host)
	}
	if queue := report.Queue["us"]; len(queue) != 1 || queue[0].ASN != 64501 || queue[0].Ipv4Addresses != 64 || queue[0].CountryShare != 0.2 || len(report.Queue["ca"]) != 1 || report.Queue["ca"][0].ASN != 64506 {
		t.Fatalf("review queue: %+v", report.Queue)
	}
	if report.Withheld["insufficient-origin-visibility"] != 0 || report.Summary["identity_review_suggested"] != 2 || report.Summary["operators_without_routes"] != 3 || report.Summary["unreviewed_origin_asns"] != 2 {
		t.Fatalf("audit summary: withheld=%v summary=%v", report.Withheld, report.Summary)
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil || !strings.Contains(string(manifest), "catalog-audit.json") || !strings.Contains(string(manifest), fixtureSha256(fixture.routes)) {
		t.Fatalf("audit manifest lacks input binding: %v %s", err, manifest)
	}
}
