package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Serves fixture bodies per URL; any other URL is a redirect to an unrelated
// host, which the downloader must refuse rather than follow.
type evidenceRoundTripper struct {
	bodies   map[string][]byte
	requests []string
}

func (self *evidenceRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	self.requests = append(self.requests, request.URL.String())
	body, ok := self.bodies[request.URL.String()]
	if !ok {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": {"https://elsewhere.example/evidence"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func evidenceFixtureBodies(t *testing.T, at time.Time) map[string][]byte {
	t.Helper()
	return map[string][]byte{
		subscriberEvidenceRisIPv4Url:    subscriberFixtureGzip(t, at.Add(-2*time.Hour), "64500 192.0.2.0/24 40\n"),
		subscriberEvidenceRisIPv6Url:    subscriberFixtureGzip(t, at.Add(-time.Hour), "64500 2001:db8::/32 40\n"),
		subscriberEvidenceRpkiUrl:       []byte(`{"metadata":{},"roas":[{"asn":64500,"prefix":"192.0.2.0/24","maxLength":24,"ta":"test"}]}`),
		subscriberEvidenceTorUrl:        []byte("ExitNode ABC\nExitAddress 192.0.2.10 2026-10-04 05:00:00\n"),
		subscriberEvidenceRegistryUrl:   []byte("2|nro|20261004|3|19821213|20261004|+0000\nnro|*|asn|*|2|summary\napnic|IN|asn|64500|2|20100101|assigned|A91964B3|e-stats\nripencc|ES|asn|64510|1|20100101|assigned|7836cb32|e-stats\n"),
		subscriberEvidenceAppleRelayUrl: []byte("192.0.2.64/27,US,US-CA,Los Angeles,\n"),
		subscriberEvidenceWarpUrl:       []byte("198.51.100.0/24,GB,GB-ENG,London,\n"),
	}
}

func TestSubscriberEvidenceRefreshPinsValidatedSnapshotsIntoACatalog(t *testing.T) {
	at := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	previous := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n", nil, strings.Replace(subscriberFixtureCatalogHeader, "policy: identified-subscriber-default", "policy: identified-subscriber-default\norigin_country_policy: withhold-outside-reviewed-countries", 1)+subscriberFixtureOperators)
	dir, existing := previous.dir, previous.catalog
	transport := &evidenceRoundTripper{bodies: evidenceFixtureBodies(t, at)}
	client := &http.Client{Transport: transport}
	out := filepath.Join(dir, "refreshed")
	if err := publishDirectory(out, func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), existing, true, stage, client, at)
	}); err != nil {
		t.Fatal(err)
	}
	catalog := mustLoadCatalog(t, filepath.Join(out, "catalog.yml"))
	if len(catalog.OriginSources) != 2 || len(catalog.RpkiSources) != 1 || len(catalog.AddressRiskSources) != 3 || len(catalog.RegistrySources) != 1 || len(catalog.Operators) != 4 || catalog.OriginCountryPolicy != originCountryPolicyWithhold || catalog.minimumOriginPeers() != 3 {
		t.Fatalf("refreshed catalog lost stanzas or policy: %+v", catalog)
	}
	if catalog.OriginSources[0].ObservedAt != at.Add(-2*time.Hour) || catalog.OriginSources[0].ExpiresAt != at.Add(46*time.Hour) || catalog.RpkiSources[0].ObservedAt != at {
		t.Fatalf("observation times do not follow the snapshot generation: %+v", catalog.OriginSources)
	}
	hashes, err := catalog.hashEvidenceSources(t.Context(), filepath.Join(out, "catalog.yml"), at)
	if err != nil || len(hashes) != 7 {
		t.Fatalf("pinned snapshots do not hash as written: %v %v", err, hashes)
	}
	compressed, err := os.ReadFile(filepath.Join(out, "sources", "rpki.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if zip, err := gzip.NewReader(bytes.NewReader(compressed)); err != nil {
		t.Fatal("compressed snapshot is not gzip")
	} else if body, _ := io.ReadAll(zip); !bytes.Equal(body, transport.bodies[subscriberEvidenceRpkiUrl]) {
		t.Fatal("compressed snapshot does not round-trip")
	}
	manifest, err := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Snapshots map[string]struct{ Sha256 string } `json:"snapshots"`
		Hashes    map[string]string                  `json:"sha256"`
	}
	if err := json.Unmarshal(manifest, &parsed); err != nil || len(parsed.Snapshots) != 7 || parsed.Snapshots["tor-exit-addresses"].Sha256 != catalog.AddressRiskSources[0].Sha256 || parsed.Hashes["catalog.yml"] == "" {
		t.Fatalf("manifest lacks snapshot binding: %s", manifest)
	}
	for _, request := range transport.requests {
		if !strings.HasPrefix(request, "https://") {
			t.Fatalf("insecure evidence request %s", request)
		}
	}
	// Without operators the output is a fragment, and the relay feeds are opt-in.
	fragment := filepath.Join(dir, "fragment")
	if err := publishDirectory(fragment, func(stage string) error { return refreshSubscriberEvidence(t.Context(), "", false, stage, client, at) }); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(fragment, "evidence.yml"))
	if err != nil || strings.Contains(string(content), "apple-private-relay") || !strings.Contains(string(content), "nro-delegated-stats") || strings.Contains(string(content), "operators:") {
		t.Fatalf("evidence fragment is wrong: %v %s", err, content)
	}
}

func TestSubscriberEvidenceRefreshRejectsRedirectsAndInvalidSnapshots(t *testing.T) {
	at := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	for name, mutate := range map[string]func(map[string][]byte){
		"redirected source": func(bodies map[string][]byte) { delete(bodies, subscriberEvidenceTorUrl) },
		"stale origin generation": func(bodies map[string][]byte) {
			bodies[subscriberEvidenceRisIPv4Url] = subscriberFixtureGzip(t, at.Add(-49*time.Hour), "64500 192.0.2.0/24 40\n")
		},
		"malformed rpki": func(bodies map[string][]byte) {
			bodies[subscriberEvidenceRpkiUrl] = []byte(`{"roas":[{"asn":64500,"prefix":"192.0.2.0/24","maxLength":23}]}`)
		},
		"empty tor list": func(bodies map[string][]byte) { bodies[subscriberEvidenceTorUrl] = []byte("ExitNode ABC\n") },
		"malformed registry": func(bodies map[string][]byte) {
			bodies[subscriberEvidenceRegistryUrl] = []byte("apnic|IN|asn|x|1|20100101|assigned|A9\n")
		},
		"empty body": func(bodies map[string][]byte) { bodies[subscriberEvidenceRisIPv6Url] = []byte{} },
	} {
		t.Run(name, func(t *testing.T) {
			bodies := evidenceFixtureBodies(t, at)
			mutate(bodies)
			client := &http.Client{Transport: &evidenceRoundTripper{bodies: bodies}}
			out := filepath.Join(t.TempDir(), "refreshed")
			if err := publishDirectory(out, func(stage string) error { return refreshSubscriberEvidence(t.Context(), "", false, stage, client, at) }); err == nil {
				t.Fatal("invalid evidence was pinned")
			}
			if _, err := os.Lstat(out); err == nil {
				t.Fatal("failed refresh published an output directory")
			}
		})
	}
}

func TestRegistryStatisticsGroupSiblingASNsPerRegistry(t *testing.T) {
	stats := "2|nro|20261004|5|19821213|20261004|+0000\nnro|*|asn|*|4|summary\napnic|IN|asn|9498|1|20000101|assigned|A91964B3|e-stats\napnic|IN|asn|24560|1|20000101|assigned|A91964B3|e-stats\napnic|IN|asn|45600|3|20000101|assigned|A91964B3|e-stats\nripencc|ES|asn|12479|1|20000101|assigned|A91964B3|e-stats\narin|US|asn|7922|1|20000101|assigned|3a7b40ab|e-stats\narin|*|asn|64496|1|20000101|reserved||e-stats\narin|US|asn|64500|1|20000101|available||e-stats\n"
	holders := &registryHolders{}
	if err := readNroDelegatedStats(t.Context(), strings.NewReader(stats), holders); err != nil {
		t.Fatal(err)
	}
	if got := holders.siblings([]uint32{24560}); fmt.Sprint(got) != "[9498 45600 45601 45602]" {
		t.Fatalf("siblings=%v", got)
	}
	if got := holders.siblings([]uint32{12479}); len(got) != 0 {
		t.Fatalf("holder ids leaked across registries: %v", got)
	}
	if got := holders.holdersOf([]uint32{7922, 64496, 64500, 1}); fmt.Sprint(got) != "[arin/3a7b40ab]" {
		t.Fatalf("holders=%v", got)
	}
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, _ = zip.Write([]byte(stats))
	_ = zip.Close()
	if err := readNroDelegatedStats(t.Context(), bytes.NewReader(compressed.Bytes()), &registryHolders{}); err != nil {
		t.Fatal("compressed statistics were rejected")
	}
	for _, bad := range []string{"", "nro|*|asn|*|1|summary\napnic|IN|asn|1|1|20000101|assigned|A9|e-stats\n", "2|nro|20261004|1|19821213|20261004|+0000\napnic|IN|asn|1|1|20000101|assigned||e-stats\n", "2|nro|20261004|1|19821213|20261004|+0000\napnic|IN|asn|x|1|20000101|assigned|A9|e-stats\n", "2|nro|20261004|1|19821213|20261004|+0000\napnic|IN|asn|1|1|20000101|assigned|A9|e-stats\nripencc|IN|asn|1|1|20000101|assigned|B9|e-stats\n"} {
		if err := readNroDelegatedStats(t.Context(), strings.NewReader(bad), &registryHolders{}); err == nil {
			t.Fatalf("malformed statistics %q accepted", bad)
		}
	}
}

// Registry siblings and nested announcements both surface as merge
// candidates; neither changes a build decision.
func TestSubscriberCatalogAuditSuggestsSiblingMerges(t *testing.T) {
	stats := []byte("2|nro|20261004|3|19821213|20261004|+0000\napnic|IN|asn|64500|1|20000101|assigned|A91964B3|e-stats\napnic|IN|asn|64505|1|20000101|assigned|A91964B3|e-stats\napnic|IN|asn|64509|1|20000101|assigned|A91964B3|e-stats\nripencc|ES|asn|64506|1|20000101|assigned|7836cb32|e-stats\n")
	catalog := subscriberFixtureCatalogHeader + `registry_sources:
  - id: nro
    url: https://ftp.ripe.net/pub/stats/ripencc/nro-stats/latest/nro-delegated-stats
    file: nro.txt
    sha256: SHA256_NRO_TXT
    observed_at: OBSERVED
    expires_at: EXPIRES
    format: nro-delegated-stats
` + subscriberFixtureOperators + "  - {id: sibling, name: Sibling Brand, asns: [64505], usage: subscriber, source: 'https://evidence.example/sibling', countries: [IN]}\n  - {id: nested, name: Nested Brand, asns: [64506], usage: subscriber, source: 'https://evidence.example/nested', countries: [IN]}\n"
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n64506 192.0.2.128/25 1\n64506 192.0.2.192/26 40\n64505 198.51.100.0/24 40\n", map[string][]byte{"nro.txt": stats}, catalog)
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
		Operators []subscriberCatalogAuditOperator  `json:"operators"`
		Merges    []subscriberCatalogMergeCandidate `json:"operator_merge_candidates"`
	}
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	byId := map[string]subscriberCatalogAuditOperator{}
	for _, operator := range report.Operators {
		byId[operator.Id] = operator
	}
	access := byId["access"]
	if fmt.Sprint(access.RegistryHolders) != "[apnic/A91964B3]" || fmt.Sprint(access.UnreviewedSiblings) != "[64509]" || access.OtherOperatorSibling[64505] != "sibling" || !strings.Contains(strings.Join(access.Flags, " "), "registry-siblings-under-other-operators") {
		t.Fatalf("access siblings: %+v", access)
	}
	if len(report.Merges) != 2 {
		t.Fatalf("merge candidates: %+v", report.Merges)
	}
	nested, holder := report.Merges[0], report.Merges[1]
	if fmt.Sprint(nested.Operators) != "[access nested]" || nested.Reason != "more-specifics-under-another-operator-aggregate" || nested.Routes != 1 || nested.LowVisibilityRoutes != 1 || len(nested.SharedHolders) != 0 {
		t.Fatalf("nested candidate: %+v", nested)
	}
	if fmt.Sprint(holder.Operators) != "[access sibling]" || holder.Reason != "shared-registry-holder" || fmt.Sprint(holder.SharedHolders) != "[apnic/A91964B3]" {
		t.Fatalf("holder candidate: %+v", holder)
	}
	p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(strings.Replace(fixture.catalogText, "format: nro-delegated-stats", "format: delegated-extended", 1)))
	if _, err := loadSubscriberOriginCatalog(p); err == nil {
		t.Fatal("unsupported registry format accepted")
	}
}
