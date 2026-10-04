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
		return refreshSubscriberEvidence(t.Context(), existing, true, false, false, stage, client, at)
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
	if err := publishDirectory(fragment, func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), "", false, false, false, stage, client, at)
	}); err != nil {
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
			if err := publishDirectory(out, func(stage string) error {
				return refreshSubscriberEvidence(t.Context(), "", false, false, false, stage, client, at)
			}); err == nil {
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

func TestLabelSourcesParseEachPublishedFormat(t *testing.T) {
	labels := &asnLabels{names: map[uint32]string{}, class: map[uint32]string{}, tags: map[uint32][]string{}, users: map[uint32]uint64{}, usersByCountry: map[uint32]map[string]uint64{}, hasTags: map[string]bool{}}
	if err := readBgpToolsAsns(t.Context(), strings.NewReader("asn,name,class,cc\nAS64500,Synthetic ISP,Eyeball,IN\nAS64503,\"Synthetic, Hosting\",Content,GB\n"), labels); err != nil {
		t.Fatal(err)
	}
	if err := readBgpToolsTag(t.Context(), strings.NewReader("AS64503,Synthetic Hosting\nAS64500,Synthetic ISP\n"), "vpsh", labels); err != nil {
		t.Fatal(err)
	}
	if err := readBgpToolsTag(t.Context(), strings.NewReader(""), "satnet", labels); err != nil {
		t.Fatal("an empty tag list was rejected")
	}
	aspop := "\"#Report of estimated users per AS - Date: 01/10/2026, Window: 60 days\"\n#Rank,AS,\"AS Name\",CC,\"Users (est.)\",\"% of Country\",\"% of Internet\",\"Samples\"\n1,\"AS64500\",\"SYNTHETIC - Synthetic ISP\",\"IN\",250000,1.00,0.01,9000\n2,\"AS64500\",\"SYNTHETIC - Synthetic ISP\",\"NP\",5000,1.00,0.01,90\n3,\"AS64502\",\"PROXY\",\"US\",80000,0.1,0.001,50\n"
	if err := readApnicAspop(t.Context(), strings.NewReader(aspop), labels); err != nil {
		t.Fatal(err)
	}
	if labels.class[64503] != "Content" || labels.names[64503] != "Synthetic, Hosting" || fmt.Sprint(labels.tags[64500]) != "[vpsh]" || labels.users[64500] != 255000 || labels.usersByCountry[64500]["np"] != 5000 || !labels.hasTags["satnet"] || !labels.hasClass || !labels.hasUsers {
		t.Fatalf("labels: %+v", labels)
	}
	for format, bad := range map[string]string{
		labelFormatBgpToolsAsns: "asn,name\nAS1,x\n",
		labelFormatBgpToolsTag:  "ASx,name\n",
		labelFormatApnicAspop:   "1,\"AS1\",\"x\",\"USA\",10\n",
	} {
		fresh := &asnLabels{names: map[uint32]string{}, class: map[uint32]string{}, tags: map[uint32][]string{}, users: map[uint32]uint64{}, usersByCountry: map[uint32]map[string]uint64{}, hasTags: map[string]bool{}}
		var err error
		switch format {
		case labelFormatBgpToolsAsns:
			err = readBgpToolsAsns(t.Context(), strings.NewReader(bad), fresh)
		case labelFormatBgpToolsTag:
			err = readBgpToolsTag(t.Context(), strings.NewReader(bad), "vpn", fresh)
		default:
			err = readApnicAspop(t.Context(), strings.NewReader(bad), fresh)
		}
		if err == nil {
			t.Fatalf("malformed %s accepted", format)
		}
	}
	for _, source := range []labelSource{{Format: labelFormatBgpToolsTag}, {Format: labelFormatBgpToolsTag, Tag: "a,b"}, {Format: labelFormatBgpToolsAsns, Tag: "dsl"}, {Format: "peeringdb"}} {
		if validLabelSource(source) {
			t.Fatalf("invalid label source accepted: %+v", source)
		}
	}
}

// Verdicts are judged against the reviewed use, and APNIC users never
// contradict an anonymizer because APNIC credits VPN egress with users.
func TestIndependentLabelVerdictsFollowReviewedUse(t *testing.T) {
	labels := &asnLabels{
		names: map[uint32]string{}, hasClass: true, hasUsers: true, hasTags: map[string]bool{"vpsh": true, "dsl": true, "vpn": true},
		class:          map[uint32]string{64500: "Eyeball", 64503: "Content", 64504: "Carrier"},
		tags:           map[uint32][]string{64501: {"vpsh"}, 64502: {"vpn"}, 64505: {"dsl"}},
		users:          map[uint32]uint64{64500: 50000, 64502: 90000, 64503: 20, 64505: 999},
		usersByCountry: map[uint32]map[string]uint64{64500: {"in": 50000}},
	}
	for _, tc := range []struct {
		usage, verdict string
		asns           []uint32
	}{
		{"subscriber", "agrees", []uint32{64500}},
		{"subscriber", "mixed", []uint32{64500, 64501}},
		{"subscriber", "disagrees", []uint32{64503}},
		{"subscriber", "unlabeled", []uint32{64504}},
		{"subscriber", "agrees", []uint32{64505}},
		{"hosting", "agrees", []uint32{64503}},
		{"hosting", "disagrees", []uint32{64500}},
		{"vpn", "agrees", []uint32{64502}},
		{"proxy", "unlabeled", []uint32{64504}},
	} {
		got := labels.operatorLabels(subscriberOperator{Id: "x", Usage: tc.usage, ASNs: tc.asns})
		if got.Verdict != tc.verdict {
			t.Fatalf("%s %v: verdict=%s %+v", tc.usage, tc.asns, got.Verdict, got)
		}
	}
	if got := labels.operatorLabels(subscriberOperator{Usage: "subscriber", ASNs: []uint32{64500}}); got.Users != 50000 || got.UsersByCountry["in"] != 50000 || fmt.Sprint(got.Agreeing) != "[apnic-users class-eyeball]" {
		t.Fatalf("labels lost detail: %+v", got)
	}
}

// The audit reports per-operator verdicts and queues unreviewed ASNs that
// independent sources call eyeball, ranked by users, flagging contrary tags.
func TestSubscriberCatalogAuditReportsIndependentLabels(t *testing.T) {
	asns := []byte("asn,name,class,cc\nAS64500,Synthetic ISP,Eyeball,IN\nAS64503,Synthetic Hosting,Content,GB\nAS64510,Unreviewed ISP,Eyeball,US\nAS64511,Unreviewed Mixed ISP,Eyeball,US\nAS64512,Unreviewed Host,Content,US\n")
	vpsh := []byte("AS64511,Unreviewed Mixed ISP\nAS64512,Unreviewed Host\n")
	aspop := []byte("#Rank,AS,\"AS Name\",CC,\"Users (est.)\"\n1,\"AS64510\",\"UNREVIEWED\",\"US\",90000\n2,\"AS64511\",\"MIXED\",\"US\",120000\n3,\"AS64512\",\"HOST\",\"US\",80000\n4,\"AS64500\",\"ISP\",\"IN\",500000\n5,\"AS64513\",\"TINY\",\"US\",10\n")
	source := func(id, file, format, tag string) string {
		extra := ""
		if tag != "" {
			extra = "\n    tag: " + tag
		}
		key := strings.ToUpper(strings.ReplaceAll(file, ".", "_"))
		return "  - id: " + id + "\n    url: https://labels.example/" + file + "\n    file: " + file + "\n    sha256: SHA256_" + key + "\n    observed_at: OBSERVED\n    expires_at: EXPIRES\n    format: " + format + extra + "\n"
	}
	catalog := subscriberFixtureCatalogHeader + "label_sources:\n" + source("asns", "asns.csv", labelFormatBgpToolsAsns, "") + source("vpsh", "vpsh.csv", labelFormatBgpToolsTag, "vpsh") + source("aspop", "aspop.csv", labelFormatApnicAspop, "") + subscriberFixtureOperators
	fixture := newSubscriberBuildFixture(t, "64500 192.0.2.0/24 40\n64510 198.51.100.0/24 40\n", map[string][]byte{"asns.csv": asns, "vpsh.csv": vpsh, "aspop.csv": aspop}, catalog)
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
		Verdicts  map[string]int                    `json:"independent_label_verdicts"`
		Queue     map[string][]labelReviewCandidate `json:"unreviewed_eyeball_queue_by_country"`
	}
	if err := json.Unmarshal(content, &report); err != nil {
		t.Fatal(err)
	}
	if report.Verdicts["subscriber/agrees"] != 1 || report.Verdicts["hosting/agrees"] != 1 || report.Verdicts["virtual_isp/unlabeled"] != 1 || report.Verdicts["transit/unlabeled"] != 1 {
		t.Fatalf("verdicts: %v", report.Verdicts)
	}
	for _, operator := range report.Operators {
		if operator.Id == "access" && (operator.Independent == nil || operator.Independent.Users != 500000 || operator.Independent.Verdict != "agrees") {
			t.Fatalf("access labels: %+v", operator.Independent)
		}
	}
	queue := report.Queue["us"]
	if len(queue) != 2 || queue[0].ASN != 64511 || !queue[0].Contrary || queue[0].Routed || queue[1].ASN != 64510 || queue[1].Contrary || !queue[1].Routed || queue[1].Name != "Unreviewed ISP" || len(report.Queue["in"]) != 0 {
		t.Fatalf("eyeball queue: %+v", report.Queue)
	}
	p := writeTestInput(t, filepath.Join(t.TempDir(), "catalog.yml"), []byte(strings.Replace(fixture.catalogText, "    tag: vpsh\n", "", 1)))
	if _, err := loadSubscriberOriginCatalog(p); err == nil {
		t.Fatal("tag list without its tag accepted")
	}
}

func TestSubscriberEvidenceRefreshPinsLabelSourcesOnRequest(t *testing.T) {
	at := time.Date(2026, 10, 4, 16, 0, 0, 0, time.UTC)
	bodies := evidenceFixtureBodies(t, at)
	bodies[subscriberEvidenceBgpToolsAsns] = []byte("asn,name,class,cc\nAS64500,Synthetic ISP,Eyeball,IN\n")
	for _, tag := range subscriberEvidenceLabelTags {
		bodies[fmt.Sprintf(subscriberEvidenceBgpToolsTag, tag)] = []byte("AS64500,Synthetic ISP\n")
	}
	bodies[subscriberEvidenceApnicAspop] = []byte("#Rank,AS,\"AS Name\",CC,\"Users (est.)\"\n1,\"AS64500\",\"ISP\",\"IN\",500000\n")
	transport := &evidenceRoundTripper{bodies: bodies}
	out := filepath.Join(t.TempDir(), "refreshed")
	if err := publishDirectory(out, func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), "", false, true, false, stage, &http.Client{Transport: transport}, at)
	}); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(out, "evidence.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"label_sources:", "format: bgp-tools-asns-csv", "tag: vpsh", "format: apnic-aspop-csv", "sources/bgp-tools-tag-dsl.csv.gz"} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("evidence fragment lacks %q: %s", want, content)
		}
	}
	for _, request := range transport.requests {
		if strings.Contains(request, "bgp.tools") && !strings.HasPrefix(request, "https://bgp.tools/") {
			t.Fatalf("unexpected label request %s", request)
		}
	}
	delete(bodies, subscriberEvidenceApnicAspop)
	if err := publishDirectory(filepath.Join(t.TempDir(), "partial"), func(stage string) error {
		return refreshSubscriberEvidence(t.Context(), "", false, true, false, stage, &http.Client{Transport: transport}, at)
	}); err == nil {
		t.Fatal("a missing label source was pinned")
	}
}
