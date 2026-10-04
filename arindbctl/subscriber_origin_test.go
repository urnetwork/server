package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
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

func TestSubscriberOriginCleanDefaultKeepsEveryAdditionalDiscriminator(t *testing.T) {
	byASN := map[uint32][]subscriberOperator{
		64500: {{Id: "access", Usage: "subscriber", Source: "https://evidence.example/access"}},
		64502: {{Id: "proxy", Usage: "virtual_isp", Source: "https://evidence.example/proxy"}},
		64503: {{Id: "host", Usage: "hosting", Source: "https://evidence.example/host"}},
		64504: {{Id: "transit", Usage: "transit", Source: "https://evidence.example/transit"}},
	}
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
			got, err := augmentSubscriberRecord(base, subscriberOriginDecision(subscriberOriginRoute{asns: tc.asns}, byASN))
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
	got, err := augmentSubscriberRecord(base, subscriberOriginDecision(subscriberOriginRoute{asns: []uint32{64500}}, byASN))
	if err != nil || got["risk"] != mmdbtype.Bool(true) || got["network_risk"] != mmdbtype.Bool(true) || len(got["network_risk_evidence"].(mmdbtype.Slice)) != 1 || got["geographic_risk"] != mmdbtype.Bool(true) {
		t.Fatal("inferred clean access cleared existing risk")
	}
	if _, err := augmentSubscriberRecord(got, nil); err == nil {
		t.Fatal("a previous augmented database retained stale inferred approval")
	}
	byASN[64500] = append(byASN[64500], subscriberOperator{Id: "same-asn-hosting", Usage: "hosting", Source: "https://evidence.example/mixed-use"})
	if subscriberOriginDecision(subscriberOriginRoute{asns: []uint32{64500}}, byASN)["state"] != mmdbtype.String("excluded") {
		t.Fatal("identified ISP waived a same-ASN use discriminator")
	}
}

func TestSubscriberOriginSnapshotRejectsMalformedStaleAndPartialEvidence(t *testing.T) {
	at := time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC)
	valid := subscriberFixtureGzip(t, at.Add(-time.Hour), "64500 192.0.2.0/24 4\n64501 192.0.2.0/24 3\n{64500,64501} 192.0.2.128/25 2\n64500 0.0.0.0/0 1\n")
	routes := map[netip.Prefix]subscriberOriginRoute{}
	if rows, err := readSubscriberOrigins(t.Context(), bytes.NewReader(valid), at, routes); err != nil || rows != 4 || len(routes) != 2 || len(routes[netip.MustParsePrefix("192.0.2.0/24")].asns) != 2 {
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
		if _, err := readSubscriberOrigins(t.Context(), bytes.NewReader(data), at, map[netip.Prefix]subscriberOriginRoute{}); err == nil {
			t.Fatal("bad origin snapshot was accepted")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := readSubscriberOrigins(ctx, bytes.NewReader(valid), at, map[netip.Prefix]subscriberOriginRoute{}); err == nil {
		t.Fatal("cancelled origin read continued")
	}
}

func TestSubscriberOriginBuildGlobalPrefixesAndPreservesBaseCoverage(t *testing.T) {
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
	base := writeTestInput(t, filepath.Join(dir, "base.mmdb"), baseBytes.Bytes())
	routes := subscriberFixtureGzip(t, at.Add(-time.Hour), "64500 192.0.2.0/24 4\n64501 192.0.2.64/26 3\n64502 192.0.2.128/27 4\n64503 192.0.2.160/28 4\n64504 192.0.2.176/28 4\n64500 192.0.2.192/28 4\n64501 192.0.2.192/28 4\n64500 2001:db8::/32 4\n64500 0.0.0.0/0 1\n")
	routePath := writeTestInput(t, filepath.Join(dir, "origins.gz"), routes)
	hash := sha256.Sum256(routes)
	catalog := fmt.Sprintf(`version: 1
policy: identified-subscriber-default
origin_sources:
  - id: synthetic-ris
    url: https://evidence.example/ris.gz
    file: origins.gz
    sha256: %s
    observed_at: %s
    expires_at: %s
operators:
  - {id: access, name: Synthetic Subscriber ISP, asns: [64500], usage: subscriber, source: 'https://evidence.example/access', countries: [IN]}
  - {id: proxy, name: Synthetic Proxy, asns: [64502], usage: virtual_isp, source: 'https://evidence.example/proxy', countries: [US]}
  - {id: host, name: Synthetic Hosting, asns: [64503], usage: hosting, source: 'https://evidence.example/hosting', countries: [GB]}
  - {id: transit, name: Synthetic Transit, asns: [64504], usage: transit, source: 'https://evidence.example/transit', countries: [ZA]}
`, hex.EncodeToString(hash[:]), at.Add(-time.Hour).Format(time.RFC3339), at.Add(time.Hour).Format(time.RFC3339))
	catalogPath := writeTestInput(t, filepath.Join(dir, "catalog.yml"), []byte(catalog))
	out := filepath.Join(dir, "out")
	if err := publishDirectory(out, func(stage string) error { return augmentSubscriberDatabase(t.Context(), base, catalogPath, stage, at) }); err != nil {
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
		var record mmdbtype.Map
		if err := db.Lookup(netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", n))).Decode(&record); err != nil {
			t.Fatal(err)
		}
		if record["quality_state"] != mmdbtype.String(state) || record["risk"] != mmdbtype.Bool(risk) || record["org_handle"] != mmdbtype.String("TEST-UNREVIEWED-CHILD") {
			t.Fatalf("offset=%d lost subscriber/veto/registration facts: %+v", n, record)
		}
	}
	for _, tc := range []struct{ address, state string }{{"198.51.100.1", "unknown"}, {"2001:db8::1", "subscriber"}, {"2001:db9::1", "unknown"}} {
		var record mmdbtype.Map
		if err := db.Lookup(netip.MustParseAddr(tc.address)).Decode(&record); err != nil || record["quality_state"] != mmdbtype.String(tc.state) {
			t.Fatalf("global coverage mismatch at %s: %+v %v", tc.address, record, err)
		}
	}
	if err := os.WriteFile(routePath, append(routes, 1), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishDirectory(filepath.Join(dir, "changed"), func(stage string) error { return augmentSubscriberDatabase(t.Context(), base, catalogPath, stage, at) }); err == nil {
		t.Fatal("changed source snapshot was published")
	}
	for _, invalid := range []string{strings.Replace(catalog, "usage: subscriber", "usage: unknown", 1), strings.Replace(catalog, "asns: [64500]", "asns: [64512]", 1), strings.Replace(catalog, "file: origins.gz", "file: ../origins.gz", 1)} {
		p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(invalid))
		if _, err := loadSubscriberOriginCatalog(p); err == nil {
			t.Fatal("invalid reviewed identity or snapshot path accepted")
		}
	}
}
