package monitor

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSelectionPopulationSignalSyntheticFreshEmptyCache(t *testing.T) {
	encode := func(counts []int) string {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(counts); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "pg_attribute") {
				return []Row{{"t"}}, nil
			}
			for _, want := range []string{"AND NOT peh.tls_authentication_failure", "WHERE tls_authentication_failure"} {
				if !strings.Contains(query, want) {
					t.Fatalf("provider supply query missing TLS-integrity clause %q", want)
				}
			}
			return []Row{{"100442", "88903", "88000", "800", "103", "88000", "0", "88000", "88000", "target-us"}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			key := args[len(args)-1]
			if key == selectionPopulationFamilyReadyKey || !strings.HasSuffix(key, "}c_l") && key != providerEligibilityReadyKey {
				return "\n", nil
			}
			if strings.Contains(joined, providerEligibilityReadyKey) {
				return "", nil
			}
			if strings.Contains(joined, "{cs_0_q_") {
				return encode([]int{}), nil
			}
			return encode([]int{74000, 602}), nil
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "selection-empty")
	if alert.Frame != "gate-wipe" {
		t.Fatalf("frame = %q, want gate-wipe", alert.Frame)
	}
	for _, want := range []string{
		"fresh_passing_excluding_tls=0",
		"tls_integrity_armed=true",
		"tls_authentication_failures=88000",
		"aggregate-only",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("selection alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestSelectionPopulationSignalSyntheticRejectsIneligibleSupply(t *testing.T) {
	encode := func(counts []int) string {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(counts); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "pg_attribute") {
				return []Row{{"f"}}, nil
			}
			for _, want := range []string{
				"INNER JOIN network_client nc",
				"pk.provide_mode IN (1,3)",
				"source_client_id IS NOT NULL",
				"NOT active",
			} {
				if !strings.Contains(query, want) {
					t.Fatalf("provider supply query missing %q", want)
				}
			}
			if strings.Contains(query, "tls_authentication_failure") {
				t.Fatalf("pre-migration population query references the absent TLS column:\n%s", query)
			}
			return []Row{{"150544", "390110", "90298", "297776", "2036", "0", "0", "0", "0", "target-us"}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			joined := strings.Join(args, " ")
			key := args[len(args)-1]
			if key == selectionPopulationFamilyReadyKey || !strings.HasSuffix(key, "}c_l") && key != providerEligibilityReadyKey {
				return "\n", nil
			}
			if strings.Contains(joined, providerEligibilityReadyKey) {
				return "", nil
			}
			return encode([]int{80000}), nil
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "provider-supply-ineligible")
	if alert.Frame != "legacy-filter" {
		t.Fatalf("frame = %q, want legacy-filter", alert.Frame)
	}
	for _, want := range []string{
		"297776 derived",
		"derived_providing=297776",
		"eligibility_ready=false",
		"tls_integrity_armed=false",
		"b7599962",
		"do not delete client",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("provider eligibility alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestSelectionPopulationSignalSyntheticReadyMarkerClearsRawResidue(t *testing.T) {
	encode := func(counts []int) string {
		var b bytes.Buffer
		if err := gob.NewEncoder(&b).Encode(counts); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "pg_attribute") {
				return []Row{{"t"}}, nil
			}
			return []Row{{"150544", "390110", "90298", "297776", "2036", "0", "0", "0", "0", "target-us"}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			key := args[len(args)-1]
			if key == providerEligibilityReadyKey {
				return providerEligibilityReadyValue, nil
			}
			if key == selectionPopulationFamilyReadyKey || !strings.HasSuffix(key, "}c_l") {
				return "\n", nil
			}
			return encode([]int{80000}), nil
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("completed eligibility export retained alerts: %+v", alerts)
	}
}

func TestSelectionPopulationSignalRejectsAmbiguousTLSIntegritySchemaState(t *testing.T) {
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if !strings.Contains(query, "pg_attribute") {
				t.Fatal("population query ran after an ambiguous TLS-integrity schema result")
			}
			return []Row{{"unknown"}}, nil
		},
	}
	_, err := NewSelectionPopulationSignal().Run(context.Background(), syntheticSettings(source))
	if err == nil || !strings.Contains(err.Error(), "invalid TLS-integrity arming state") {
		t.Fatalf("Run error = %v, want explicit ambiguous TLS-integrity schema failure", err)
	}
}

// The old-product RED uses only existing public Signal paths and synthetic
// Redis wire replies; no product schema fields are needed for compilation.
type selectionPopulationFamilyTestFixture struct {
	marker            string
	markerReply       func(int) string
	documents         map[string]string
	eligibilityMarker string
	derived           int
	inactive          int
}

type selectionPopulationFamilyTestCapture struct {
	stateLock   sync.Mutex
	markerReads int
	readKeys    []string
}

func selectionPopulationFamilyTestKey(forced bool, facet string) string {
	mode := 0
	if forced {
		mode = 1
	}
	key := fmt.Sprintf("{cs_%d_q_00000000-0000-0000-0000-000000000000_12345678-1234-4234-8234-123456789abc}c_l", mode)
	if facet != "" {
		key += "_" + facet
	}
	return key
}

func selectionPopulationFamilyTestGob(t testing.TB, counts []int) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := gob.NewEncoder(&buffer).Encode(counts); err != nil {
		t.Fatal(err)
	}
	// redis-cli --raw GET appends a newline even to a binary Gob value.
	return buffer.String() + "\n"
}

func selectionPopulationFamilyTestDocuments(t testing.TB, normal, forced map[string][]int) map[string]string {
	t.Helper()
	documents := map[string]string{}
	for _, facet := range []string{"d", "4", "6"} {
		documents[selectionPopulationFamilyTestKey(false, facet)] = selectionPopulationFamilyTestGob(t, normal[facet])
		documents[selectionPopulationFamilyTestKey(true, facet)] = selectionPopulationFamilyTestGob(t, forced[facet])
	}
	return documents
}

func selectionPopulationFamilyTestSettings(fixture selectionPopulationFamilyTestFixture) (SignalSettings, *selectionPopulationFamilyTestCapture) {
	capture := &selectionPopulationFamilyTestCapture{}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "pg_attribute") {
				return []Row{{"t"}}, nil
			}
			return []Row{{
				"4001", strconv.Itoa(3000 + fixture.derived + fixture.inactive), "3000",
				strconv.Itoa(fixture.derived), strconv.Itoa(fixture.inactive), "2997", "2995", "2996", "2",
				"12345678-1234-4234-8234-123456789abc",
			}}, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if len(args) < 2 || args[len(args)-2] != "GET" {
				return "", fmt.Errorf("unsupported synthetic Redis operation")
			}
			key := args[len(args)-1]
			capture.stateLock.Lock()
			capture.readKeys = append(capture.readKeys, key)
			read := capture.markerReads
			if key == "client_score_ip_family_v1_ready" {
				capture.markerReads++
			}
			capture.stateLock.Unlock()
			if key == "client_score_ip_family_v1_ready" {
				if fixture.markerReply != nil {
					return fixture.markerReply(read), nil
				}
				return fixture.marker, nil
			}
			if key == providerEligibilityReadyKey {
				return fixture.eligibilityMarker, nil
			}
			if value, present := fixture.documents[key]; present {
				return value, nil
			}
			return "\n", nil
		},
	}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return time.Date(2099, 3, 4, 5, 6, 7, 0, time.UTC) }
	settings.Hosts = []HostSettings{
		{Name: "postgres.example.test", Roles: []string{"pg-primary"}, LANAddress: "192.0.2.21", OverlayAddress: "198.51.100.21"},
		{Name: "redis.example.test", Roles: []string{"redis-cluster"}, LANAddress: "192.0.2.22", OverlayAddress: "198.51.100.22", RedisEntryPort: 6390},
	}
	return settings, capture
}

func requireSelectionPopulationFamilyVisibility(t *testing.T, alerts Alerts) {
	t.Helper()
	alert := requireAlertClass(t, alerts, "cannot-observe")
	if alert.SignalNumber != "2.9" || alert.SignalKey != "selection-population" || alert.Severity != SeverityWarn || alert.Sustain != 2 || !strings.Contains(alert.Playbook, "§2.9") {
		t.Fatalf("schema uncertainty lost its owning visibility identity: class=%s", alert.Class)
	}
	for _, candidate := range alerts {
		if candidate.Class == "selection-empty" {
			t.Fatalf("unknown cache source became a definitive empty-market alert: frame=%s", candidate.Frame)
		}
	}
	requireAlertOmits(t, alert, "synthetic-private-payload", "synthetic-private-marker")
}

func TestSelectionPopulationFamilyCompletedFacetsIgnoreRetiredLegacy(t *testing.T) {
	documents := selectionPopulationFamilyTestDocuments(t,
		map[string][]int{"d": {17}, "4": {23, 5}, "6": {}},
		map[string][]int{"d": {18}, "4": {40, 6}, "6": {8}},
	)
	settings, capture := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
		marker: "1\n", documents: documents, eligibilityMarker: "1\n",
	})
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatalf("complete current facets misread as an empty market: alerts=%d err=%v", len(alerts), err)
	}
	if capture.markerReads != 2 {
		t.Fatalf("family schema was not fenced across the cache read: marker_reads=%d", capture.markerReads)
	}
}

func TestSelectionPopulationFamilyReadyMarkerRejectsPartialFacets(t *testing.T) {
	for _, forced := range []bool{false, true} {
		for _, missingFacet := range []string{"d", "4", "6"} {
			documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
			delete(documents, selectionPopulationFamilyTestKey(forced, missingFacet))
			documents[selectionPopulationFamilyTestKey(false, "")] = selectionPopulationFamilyTestGob(t, []int{101})
			documents[selectionPopulationFamilyTestKey(true, "")] = selectionPopulationFamilyTestGob(t, []int{151})
			settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
				marker: "1", documents: documents, eligibilityMarker: "1",
			})
			alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatalf("partial current facets returned an unqualified error: forced=%t facet=%s", forced, missingFacet)
			}
			requireSelectionPopulationFamilyVisibility(t, alerts)
		}
	}
}

func TestSelectionPopulationFamilyPremarkerFacetPresenceBeatsLegacy(t *testing.T) {
	for _, partial := range []bool{false, true} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{}, map[string][]int{"d": {7}, "4": {13}, "6": {5}})
		documents[selectionPopulationFamilyTestKey(false, "")] = selectionPopulationFamilyTestGob(t, []int{101})
		documents[selectionPopulationFamilyTestKey(true, "")] = selectionPopulationFamilyTestGob(t, []int{151})
		if partial {
			delete(documents, selectionPopulationFamilyTestKey(false, "6"))
		}
		settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
			documents: documents, eligibilityMarker: "1",
		})
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		if partial {
			requireSelectionPopulationFamilyVisibility(t, alerts)
			continue
		}
		alert := requireAlertClass(t, alerts, "selection-empty")
		if alert.Frame != "gate-wipe" || !strings.Contains(alert.Observed, "normal=0 forced=25") || alert.Sustain != 2 {
			t.Fatalf("premarker current representation was hidden or dualstack double-counted: frame=%s", alert.Frame)
		}
	}
}

func TestSelectionPopulationFamilyMissingCacheIsNotEncodedEmpty(t *testing.T) {
	for _, reply := range []string{"", "\n", "(nil)\n"} {
		for _, forcedMissing := range []bool{false, true} {
			documents := map[string]string{
				selectionPopulationFamilyTestKey(false, ""): selectionPopulationFamilyTestGob(t, []int{}),
				selectionPopulationFamilyTestKey(true, ""):  selectionPopulationFamilyTestGob(t, []int{}),
			}
			documents[selectionPopulationFamilyTestKey(forcedMissing, "")] = reply
			settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
				documents: documents, eligibilityMarker: "1",
			})
			alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal("missing cache must retain explicit visibility, not an unqualified error")
			}
			requireSelectionPopulationFamilyVisibility(t, alerts)
		}
	}
}

func TestSelectionPopulationFamilyMalformedAndNegativeCountsStayVisibility(t *testing.T) {
	for _, invalid := range []string{"synthetic-private-payload", selectionPopulationFamilyTestGob(t, []int{17, -1})} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
		documents[selectionPopulationFamilyTestKey(false, "d")] = invalid
		documents[selectionPopulationFamilyTestKey(false, "")] = selectionPopulationFamilyTestGob(t, []int{101})
		documents[selectionPopulationFamilyTestKey(true, "")] = selectionPopulationFamilyTestGob(t, []int{151})
		settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
			marker: "1", documents: documents, eligibilityMarker: "1",
		})
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("invalid current cache must retain fixed visibility without its private parse detail")
		}
		requireSelectionPopulationFamilyVisibility(t, alerts)
	}
}

func TestSelectionPopulationFamilySchemaChangeAcrossReadIsUnknown(t *testing.T) {
	for _, first := range []string{"", "1"} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
		documents[selectionPopulationFamilyTestKey(false, "")] = selectionPopulationFamilyTestGob(t, []int{101})
		documents[selectionPopulationFamilyTestKey(true, "")] = selectionPopulationFamilyTestGob(t, []int{151})
		settings, capture := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
			documents: documents, eligibilityMarker: "1",
			markerReply: func(read int) string {
				if read == 0 {
					return first
				}
				if first == "1" {
					return ""
				}
				return "1"
			},
		})
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		requireSelectionPopulationFamilyVisibility(t, alerts)
		if capture.markerReads != 2 {
			t.Fatalf("schema change was not observed by an exact before/after fence: reads=%d", capture.markerReads)
		}
	}
}

func TestSelectionPopulationFamilyPreservesAvailableLegacyControls(t *testing.T) {
	for _, test := range []struct {
		name           string
		normal, forced []int
		frame          string
	}{
		{name: "healthy", normal: []int{31}, forced: []int{57}},
		{name: "encoded-empty", normal: []int{}, forced: []int{}, frame: "upstream-empty"},
		{name: "gate-wipe", normal: []int{}, forced: []int{57}, frame: "gate-wipe"},
	} {
		documents := map[string]string{
			selectionPopulationFamilyTestKey(false, ""): selectionPopulationFamilyTestGob(t, test.normal),
			selectionPopulationFamilyTestKey(true, ""):  selectionPopulationFamilyTestGob(t, test.forced),
		}
		settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
			documents: documents, eligibilityMarker: "1",
		})
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: available legacy control returned error", test.name)
		}
		if test.frame == "" {
			if len(alerts) != 0 {
				t.Fatalf("available legacy healthy control returned %d alerts", len(alerts))
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "selection-empty")
		if alert.Frame != test.frame || alert.Severity != SeverityPage || alert.Sustain != 2 {
			t.Fatalf("%s: complete legacy zero proof lost its existing PAGE: frame=%s", test.name, alert.Frame)
		}
	}
}

func TestSelectionPopulationFamilyCompleteFacetedEmptyRemainsPage(t *testing.T) {
	documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{}, map[string][]int{})
	documents[selectionPopulationFamilyTestKey(false, "")] = selectionPopulationFamilyTestGob(t, []int{101})
	documents[selectionPopulationFamilyTestKey(true, "")] = selectionPopulationFamilyTestGob(t, []int{151})
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
		marker: "1", documents: documents, eligibilityMarker: "1",
	})
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "selection-empty")
	if alert.Frame != "upstream-empty" || alert.Severity != SeverityPage || alert.Sustain != 2 || !strings.Contains(alert.Observed, "normal=0 forced=0") {
		t.Fatalf("complete current encoded-empty proof was weakened: frame=%s", alert.Frame)
	}
}

func TestSelectionPopulationFamilyPreservesEligibilityAlongsideCacheVisibility(t *testing.T) {
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
		derived: 23, inactive: 17,
	})
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	requireSelectionPopulationFamilyVisibility(t, alerts)
	alert := requireAlertClass(t, alerts, "provider-supply-ineligible")
	if alert.Severity != SeverityPage || alert.Sustain != 1 || !strings.Contains(alert.Observed, "derived_providing=23 inactive_top_level_providing=17") {
		t.Fatal("independent eligibility proof was lost with cache visibility")
	}
}

func TestSelectionPopulationFamilyRejectsInvalidMarker(t *testing.T) {
	for _, marker := range []string{"0", "01", "synthetic-private-marker"} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
		documents[selectionPopulationFamilyTestKey(false, "")] = selectionPopulationFamilyTestGob(t, []int{101})
		documents[selectionPopulationFamilyTestKey(true, "")] = selectionPopulationFamilyTestGob(t, []int{151})
		settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
			marker: marker, documents: documents, eligibilityMarker: "1",
		})
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal("invalid family marker must be fixed visibility, not a private-value error")
		}
		requireSelectionPopulationFamilyVisibility(t, alerts)
	}
}

// Context-aware test transport retains the existing synthetic source for all
// unrelated seams and exposes cancellation without wall-clock sleeps.
type selectionPopulationFamilyContextSource struct {
	SignalSource
	postgresContextFn func(context.Context, string) ([]Row, error)
	redisContextFn    func(context.Context, HostSettings, int, ...string) (string, error)
}

// Only owning context observations override the embedded synthetic source.
func (self *selectionPopulationFamilyContextSource) PostgreSQL(ctx context.Context, query string) ([]Row, error) {
	if self.postgresContextFn != nil {
		return self.postgresContextFn(ctx, query)
	}
	return self.SignalSource.PostgreSQL(ctx, query)
}

// The barrier and deadline controls operate on the actual supplied context.
func (self *selectionPopulationFamilyContextSource) Redis(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
	if self.redisContextFn != nil {
		return self.redisContextFn(ctx, host, port, args...)
	}
	return self.SignalSource.Redis(ctx, host, port, args...)
}

// Valid native Gob bytes may themselves end in a newline byte.
func TestSelectionPopulationFamilyStrictCountDocuments(t *testing.T) {
	for _, counts := range [][]int{nil, {}, {5}, {17, 23}} {
		wire := selectionPopulationFamilyTestGob(t, counts)
		want := 0
		for _, count := range counts {
			want += count
		}
		for _, raw := range []string{wire, wire[:len(wire)-1]} {
			got, err := decodeProviderCount([]byte(raw))
			if err != nil || got != want {
				t.Fatalf("complete count document: got=%d want=%d err=%v", got, want, err)
			}
		}
	}
	for _, raw := range []string{
		"", "\n", "(nil)\n", "synthetic-private-payload",
		selectionPopulationFamilyTestGob(t, []int{-1}),
		selectionPopulationFamilyTestGob(t, []int{int(^uint(0) >> 1), 1}),
		selectionPopulationFamilyTestGob(t, []int{31}) + "synthetic-private-payload",
		selectionPopulationFamilyTestGob(t, []int{31}) + selectionPopulationFamilyTestGob(t, []int{57}),
	} {
		if _, err := decodeProviderCount([]byte(raw)); err == nil {
			t.Fatal("missing, incompatible, negative, overflow or additional count data was accepted")
		}
	}
}

// Presence, not stale legacy availability, selects the completed representation.
func TestSelectionPopulationFamilyNeverReadsLegacyForPresentFacets(t *testing.T) {
	for _, marker := range []string{"", "1"} {
		for _, partial := range []bool{false, true} {
			documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
			documents[selectionPopulationFamilyTestKey(false, "")] = selectionPopulationFamilyTestGob(t, []int{101})
			documents[selectionPopulationFamilyTestKey(true, "")] = selectionPopulationFamilyTestGob(t, []int{151})
			if partial {
				delete(documents, selectionPopulationFamilyTestKey(false, "6"))
			}
			settings, capture := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
				marker: marker, documents: documents, eligibilityMarker: "1",
			})
			alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
			if err != nil {
				t.Fatal(err)
			}
			if partial {
				requireSelectionPopulationFamilyVisibility(t, alerts)
			} else if len(alerts) != 0 {
				t.Fatal("complete nonzero facet control returned an alert")
			}
			for _, key := range capture.readKeys {
				if key == selectionPopulationFamilyTestKey(false, "") || key == selectionPopulationFamilyTestKey(true, "") {
					t.Fatal("present facets fell back to the stale legacy pair")
				}
			}
		}
	}
}

// All marker/facet reads and the permitted legacy fallback share one deadline.
func TestSelectionPopulationFamilyUsesOneBoundedRedisDeadline(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
		marker := "1"
		if legacy {
			documents = map[string]string{
				selectionPopulationFamilyTestKey(false, ""): selectionPopulationFamilyTestGob(t, []int{31}),
				selectionPopulationFamilyTestKey(true, ""):  selectionPopulationFamilyTestGob(t, []int{57}),
			}
			marker = ""
		}
		settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
			marker: marker, documents: documents, eligibilityMarker: "1",
		})
		baseSource := settings.Source
		var deadline time.Time
		reads := 0
		settings.Source = &selectionPopulationFamilyContextSource{
			SignalSource: baseSource,
			redisContextFn: func(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
				observedDeadline, available := ctx.Deadline()
				if !available {
					t.Fatal("Redis qualification has no child deadline")
				}
				if reads == 0 {
					deadline = observedDeadline
					remaining := time.Until(deadline)
					if remaining <= 0 || remaining > selectionPopulationRedisQualificationTimeout {
						t.Fatal("Redis qualification deadline is not bounded at sixty seconds")
					}
				} else if observedDeadline != deadline {
					t.Fatal("one Redis read refreshed the shared qualification deadline")
				}
				reads++
				return baseSource.Redis(ctx, host, port, args...)
			},
		}
		alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
		wantReads := 9
		if legacy {
			wantReads = 11
		}
		if err != nil || len(alerts) != 0 || reads != wantReads {
			t.Fatalf("bounded complete source: legacy=%t alerts=%d reads=%d want=%d err=%v", legacy, len(alerts), reads, wantReads, err)
		}
	}
}

// An operation-level deadline is visibility, not authoritative parent lifecycle.
func TestSelectionPopulationFamilyChildSourceFailureRetainsEligibility(t *testing.T) {
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{derived: 23, inactive: 17})
	baseSource := settings.Source
	settings.Source = &selectionPopulationFamilyContextSource{
		SignalSource: baseSource,
		redisContextFn: func(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
			if args[len(args)-1] == selectionPopulationFamilyReadyKey {
				if _, available := ctx.Deadline(); !available {
					t.Fatal("source failure did not receive the qualification context")
				}
				return "", context.DeadlineExceeded
			}
			return baseSource.Redis(ctx, host, port, args...)
		},
	}
	ctx := context.Background()
	alerts, err := NewSelectionPopulationSignal().Run(ctx, settings)
	if err != nil || ctx.Err() != nil {
		t.Fatal("a child source failure became parent cancellation or discarded independent findings")
	}
	requireSelectionPopulationFamilyVisibility(t, alerts)
	eligibility := requireAlertClass(t, alerts, "provider-supply-ineligible")
	if eligibility.Severity != SeverityPage || eligibility.Sustain != 1 ||
		!strings.Contains(eligibility.Observed, "normal_cache_count=unavailable forced_cache_count=unavailable") {
		t.Fatal("child source failure erased eligibility or invented zero cache counts")
	}
}

// A failed marker read cannot invent either absent or completed eligibility.
func TestSelectionPopulationFamilyUnknownEligibilityIsNotIneligible(t *testing.T) {
	documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
		marker: "1", documents: documents, derived: 23, inactive: 17,
	})
	baseSource := settings.Source
	settings.Source = &selectionPopulationFamilyContextSource{
		SignalSource: baseSource,
		redisContextFn: func(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
			if args[len(args)-1] == providerEligibilityReadyKey {
				return "", errors.New("synthetic-private-marker transport unavailable")
			}
			return baseSource.Redis(ctx, host, port, args...)
		},
	}
	alerts, err := NewSelectionPopulationSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 {
		t.Fatalf("unknown eligibility marker: alerts=%d err=%v", len(alerts), err)
	}
	visibility := requireAlertClass(t, alerts, "cannot-observe")
	if visibility.Frame != "eligibility-observation" ||
		!strings.Contains(visibility.Observed, "eligibility_marker_known=false cache_population_known=true") {
		t.Fatal("unknown eligibility marker was presented as known eligibility or unknown valid cache counts")
	}
	requireAlertOmits(t, visibility, "synthetic-private-marker")
}

// Authoritative pre-cancellation cannot invoke either PostgreSQL or Redis.
func TestSelectionPopulationFamilyPreCanceledDoesNotCallSource(t *testing.T) {
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{})
	calls := 0
	settings.Source = &selectionPopulationFamilyContextSource{
		SignalSource: settings.Source,
		postgresContextFn: func(context.Context, string) ([]Row, error) {
			calls++
			return nil, nil
		},
		redisContextFn: func(context.Context, HostSettings, int, ...string) (string, error) {
			calls++
			return "", nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	alerts, err := NewSelectionPopulationSignal().Run(ctx, settings)
	if err != ctx.Err() || len(alerts) != 0 || calls != 0 {
		t.Fatalf("pre-cancellation: calls=%d alerts=%d err=%v", calls, len(alerts), err)
	}
}

// A barrier forces parent cancellation during the actual Redis qualification.
func TestSelectionPopulationFamilyInFlightCancellationIsLifecycle(t *testing.T) {
	documents := selectionPopulationFamilyTestDocuments(t, map[string][]int{"4": {31}}, map[string][]int{"4": {57}})
	settings, _ := selectionPopulationFamilyTestSettings(selectionPopulationFamilyTestFixture{
		marker: "1", documents: documents, derived: 23, inactive: 17,
	})
	baseSource := settings.Source
	entered := make(chan struct{})
	settings.Source = &selectionPopulationFamilyContextSource{
		SignalSource: baseSource,
		redisContextFn: func(ctx context.Context, host HostSettings, port int, args ...string) (string, error) {
			if args[len(args)-1] == selectionPopulationFamilyReadyKey {
				close(entered)
				<-ctx.Done()
				return "", ctx.Err()
			}
			return baseSource.Redis(ctx, host, port, args...)
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		alerts Alerts
		err    error
	}
	done := make(chan result, 1)
	go func() {
		alerts, err := NewSelectionPopulationSignal().Run(ctx, settings)
		done <- result{alerts: alerts, err: err}
	}()
	select {
	case <-entered:
		cancel()
	case completed := <-done:
		t.Fatalf("source barrier was not entered: alerts=%d err=%v", len(completed.alerts), completed.err)
	}
	completed := <-done
	if completed.err != ctx.Err() || len(completed.alerts) != 0 {
		t.Fatalf("in-flight parent cancellation invented findings: alerts=%d err=%v", len(completed.alerts), completed.err)
	}
}
