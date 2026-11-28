package controller

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The camouflage report route (connect/EXTENDER-SPLICE-ROLLOUT.md, Reporting
// path): auth is the wrapper's and is covered at the route (api), and everything
// past it is here, against a fake Redis, a fake fleet and a fake location. Every
// address is a documentation one and every key synthetic.

// The caller of every report: an ordinary client at a documentation address the
// fake lookup places in `de`.
const (
	testCamouflageCallerIp   = "198.51.100.30"
	testCamouflageRelayIp    = "203.0.113.7"
	testCamouflageExtenderIp = "192.0.2.10"
)

// A report as the sender posts it, every field set. The same body is pinned on the
// sender's side (connect ExtenderCamouflageReportArgs), so a change on either side
// shows on that side.
const testCamouflageReportBody = `{
	"report_id": "0123456789abcdef0123456789abcdef",
	"client_version": "2026.10.30-1085916410",
	"resource_version": 3,
	"ip_versions": [4, 6],
	"client": {
		"camouflage_won": 1,
		"legacy_won": 2,
		"both_failed": 3,
		"refused": 4,
		"camouflage_unauthenticated": 5,
		"camouflage_leaf_rejected": 6,
		"legacy_only_no_key": 7,
		"legacy_only_fingerprint": 8,
		"legacy_only_no_front_name": 9
	},
	"extender": {
		"authenticated": 11,
		"spliced": 12,
		"terminated": 13,
		"splice_fallback_terminated": 14,
		"replay_refused": 15,
		"time_window_failed": 16,
		"splice_dial_failed": 17,
		"borrow_verify_failed": 18,
		"targets": [
			{"name": "Listed.Example", "splice_count": 21, "relayed_byte_count": 2100},
			{"name": "unlisted.example", "splice_count": 22, "relayed_byte_count": 2200}
		],
		"covered": false,
		"degraded_reason": "splice_off"
	}
}`

type testCamouflageReportHarness struct {
	t         *testing.T
	reporting *extenderCamouflageReporting
	metrics   *extenderCamouflageReportMetrics
	registry  *prometheus.Registry

	now time.Time

	stateLock    sync.Mutex
	enabled      bool
	budgetSpends map[server.Id]int
	budgetErr    error
	claims       map[string]bool
	claimCount   int
	claimErr     error
	fleet        *extenderCamouflageFleet
	countries    map[netip.Addr]string
}

func newTestCamouflageReportHarness(t *testing.T) *testCamouflageReportHarness {
	t.Helper()
	registry := prometheus.NewRegistry()
	self := &testCamouflageReportHarness{
		t:            t,
		metrics:      newExtenderCamouflageReportMetrics(registry),
		registry:     registry,
		now:          time.Date(2026, 11, 2, 7, 15, 0, 0, time.UTC),
		enabled:      true,
		budgetSpends: map[server.Id]int{},
		claims:       map[string]bool{},
		fleet:        newExtenderCamouflageFleet(nil),
		countries: map[netip.Addr]string{
			netip.MustParseAddr(testCamouflageCallerIp):   "de",
			netip.MustParseAddr(testCamouflageRelayIp):    "nl",
			netip.MustParseAddr(testCamouflageExtenderIp): "fr",
		},
	}
	self.reporting = &extenderCamouflageReporting{
		enabled: func() bool {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.enabled
		},
		now: func() time.Time { return self.now },
		spendRateBudget: func(ctx context.Context, clientId server.Id, now time.Time) (bool, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.budgetErr != nil {
				return false, self.budgetErr
			}
			self.budgetSpends[clientId] += 1
			return self.budgetSpends[clientId] <= extenderCamouflageReportRateLimit, nil
		},
		claimReport: func(ctx context.Context, reportId string) (bool, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.claimErr != nil {
				return false, self.claimErr
			}
			self.claimCount += 1
			if self.claims[reportId] {
				return false, nil
			}
			self.claims[reportId] = true
			return true, nil
		},
		fleet: func(ctx context.Context, now time.Time) *extenderCamouflageFleet {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.fleet
		},
		country: func(ip netip.Addr) (string, error) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			countryCode, ok := self.countries[ip]
			if !ok {
				return "", errors.New("no record")
			}
			return countryCode, nil
		},
		labels: func() *extenderCamouflageReportLabels {
			return newExtenderCamouflageReportLabels([]*connect.ExtenderBorrowList{{
				Version: 3,
				Names:   []connect.ExtenderBorrowName{{Name: "listed.example"}},
			}})
		},
		metrics: self.metrics,
	}
	return self
}

func (self *testCamouflageReportHarness) session(clientId server.Id, clientIp string) *session.ClientSession {
	self.t.Helper()
	clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{
		NetworkId: server.NewId(),
		UserId:    server.NewId(),
		ClientId:  &clientId,
	})
	self.t.Cleanup(clientSession.Cancel)
	clientSession.ClientAddress = netip.AddrPortFrom(netip.MustParseAddr(clientIp), 4500).String()
	return clientSession
}

// One report from an ordinary client at the usual address.
func (self *testCamouflageReportHarness) post(body string) *ExtenderCamouflageReportResult {
	self.t.Helper()
	return self.reporting.report(strings.NewReader(body), self.session(server.NewId(), testCamouflageCallerIp))
}

// Reads one report outcome.
func (self *testCamouflageReportHarness) reportCount(outcome string) float64 {
	self.t.Helper()
	return testutil.ToFloat64(self.metrics.reports.WithLabelValues(outcome))
}

// Every counted value, as "metric{label=value,...} count", of the families that
// count what a report holds. The reports counter says how reports ended, not what
// they held, so it is left out.
func (self *testCamouflageReportHarness) counted() map[string]float64 {
	self.t.Helper()
	families, err := self.registry.Gather()
	if err != nil {
		self.t.Fatal(err)
	}
	counted := map[string]float64{}
	for _, family := range families {
		if family.GetName() == "urnetwork_extender_camouflage_reports_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := []string{}
			for _, label := range metric.GetLabel() {
				labels = append(labels, label.GetName()+"="+label.GetValue())
			}
			slices.Sort(labels)
			counted[family.GetName()+"{"+strings.Join(labels, ",")+"}"] = metric.GetCounter().GetValue()
		}
	}
	return counted
}

// Every label value of every family, the reports counter included.
func (self *testCamouflageReportHarness) labelValues() []string {
	self.t.Helper()
	families, err := self.registry.Gather()
	if err != nil {
		self.t.Fatal(err)
	}
	values := []string{}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				values = append(values, label.GetValue())
			}
		}
	}
	return values
}

func (self *testCamouflageReportHarness) requireNothingCounted() {
	self.t.Helper()
	if counted := self.counted(); len(counted) != 0 {
		self.t.Fatalf("counted = %v, want nothing", counted)
	}
}

// A body that differs from the full report by the edit, with a fresh id so no
// claim in the harness answers for it.
func testCamouflageReportBodyWith(t *testing.T, edit func(fields map[string]any)) string {
	t.Helper()
	fields := map[string]any{}
	if err := json.Unmarshal([]byte(testCamouflageReportBody), &fields); err != nil {
		t.Fatal(err)
	}
	fields["report_id"] = strings.ReplaceAll(server.NewId().String(), "-", "")
	edit(fields)
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestExtenderCamouflageReportCountsAReportUnderFixedLabels(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	clientId := server.NewId()
	harness.fleet = newExtenderCamouflageFleet([]*model.NetworkExtenderOwnerAddress{{
		ClientId: clientId,
		Ip:       netip.MustParseAddr(testCamouflageExtenderIp),
	}})

	result := harness.reporting.report(strings.NewReader(testCamouflageReportBody), harness.session(clientId, testCamouflageExtenderIp))
	if result.Outcome != ExtenderCamouflageReportAccepted || result.RetryAfterSeconds != 0 {
		t.Fatalf("result = %+v", result)
	}

	want := map[string]float64{}
	for outcome, count := range map[string]float64{
		"camouflage_won": 1, "legacy_won": 2, "both_failed": 3, "refused": 4,
		"camouflage_unauthenticated": 5, "camouflage_leaf_rejected": 6,
		"legacy_only_no_key": 7, "legacy_only_fingerprint": 8, "legacy_only_no_front_name": 9,
	} {
		want[fmt.Sprintf("urnetwork_extender_camouflage_client_outcomes_total{country=fr,family=any,outcome=%s}", outcome)] = count
		want[fmt.Sprintf("urnetwork_extender_camouflage_client_version_outcomes_total{client_version=2026.10.30,outcome=%s,resource_version=3}", outcome)] = count
	}
	for outcome, count := range map[string]float64{
		"authenticated": 11, "spliced": 12, "terminated": 13, "splice_fallback_terminated": 14,
		"replay_refused": 15, "time_window_failed": 16, "splice_dial_failed": 17, "borrow_verify_failed": 18,
	} {
		want[fmt.Sprintf("urnetwork_extender_camouflage_extender_outcomes_total{country=fr,family=any,outcome=%s}", outcome)] = count
		want[fmt.Sprintf("urnetwork_extender_camouflage_extender_version_outcomes_total{client_version=2026.10.30,outcome=%s,resource_version=3}", outcome)] = count
	}
	want["urnetwork_extender_camouflage_status_reports_total{country=fr,family=any,verdict=splice_off}"] = 1
	want["urnetwork_extender_camouflage_status_version_reports_total{client_version=2026.10.30,resource_version=3,verdict=splice_off}"] = 1
	want["urnetwork_extender_camouflage_target_splices_total{target=listed.example}"] = 21
	want["urnetwork_extender_camouflage_target_splices_total{target=other}"] = 22
	want["urnetwork_extender_camouflage_target_relayed_bytes_total{target=listed.example}"] = 2100
	want["urnetwork_extender_camouflage_target_relayed_bytes_total{target=other}"] = 2200

	counted := harness.counted()
	for series, count := range want {
		if counted[series] != count {
			t.Errorf("%s = %v, want %v", series, counted[series], count)
		}
	}
	for series := range counted {
		if _, ok := want[series]; !ok {
			t.Errorf("unexpected series %s = %v", series, counted[series])
		}
	}
	if count := harness.reportCount(extenderCamouflageReportOutcomeAccepted); count != 1 {
		t.Errorf("accepted = %v", count)
	}
}

// A counter with nothing to add makes no series, so a quiet fleet costs none.
func TestExtenderCamouflageReportZeroCountsMakeNoSeries(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	result := harness.post(`{"report_id":"0123456789abcdef0123456789abcdef","client":{}}`)
	if result.Outcome != ExtenderCamouflageReportAccepted {
		t.Fatalf("result = %+v", result)
	}
	harness.requireNothingCounted()
}

// The kill switch: off, the route counts nothing and asks nothing of Redis, and
// the sender is told to go away.
func TestExtenderCamouflageReportDisabledAnswersAndCountsNothing(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	harness.enabled = false

	result := harness.post(testCamouflageReportBody)
	if result.Outcome != ExtenderCamouflageReportDisabled {
		t.Fatalf("result = %+v", result)
	}
	harness.requireNothingCounted()
	if count := harness.reportCount(extenderCamouflageReportOutcomeDisabled); count != 1 {
		t.Errorf("disabled = %v", count)
	}
	if len(harness.budgetSpends) != 0 || harness.claimCount != 0 {
		t.Errorf("spends = %v, claims = %d: the switch must come first", harness.budgetSpends, harness.claimCount)
	}
}

// The switch is `camouflage_report.enabled` in `extender.yml`, and off is what an
// operator gets by saying nothing.
func TestExtenderCamouflageReportIsOffUntilExtenderConfigSaysOn(t *testing.T) {
	for _, c := range []struct {
		name    string
		yaml    string
		enabled bool
	}{
		{name: "no block", yaml: "network_host: report.example", enabled: false},
		{name: "off", yaml: "network_host: report.example\ncamouflage_report:\n  enabled: false", enabled: false},
		{name: "on", yaml: "network_host: report.example\ncamouflage_report:\n  enabled: true", enabled: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			installTestExtenderConfigYaml(t, c.yaml)
			if enabled := extenderCamouflageReportEnabled(); enabled != c.enabled {
				t.Fatalf("enabled = %v, want %v", enabled, c.enabled)
			}
		})
	}
}

// A retry of a report whose answer was lost adds nothing: the second post of an id
// says duplicate, and a sender stops on either answer.
func TestExtenderCamouflageReportRetryIsCountedOnce(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	body := `{"report_id":"0123456789abcdef0123456789abcdef","client":{"both_failed":5}}`
	series := "urnetwork_extender_camouflage_client_outcomes_total{country=de,family=any,outcome=both_failed}"

	if result := harness.post(body); result.Outcome != ExtenderCamouflageReportAccepted {
		t.Fatalf("first = %+v", result)
	}
	for range 3 {
		if result := harness.post(body); result.Outcome != ExtenderCamouflageReportDuplicate {
			t.Fatalf("retry = %+v", result)
		}
	}
	if count := harness.counted()[series]; count != 5 {
		t.Fatalf("%s = %v, want 5", series, count)
	}
	if count := harness.reportCount(extenderCamouflageReportOutcomeDuplicate); count != 3 {
		t.Errorf("duplicates = %v", count)
	}
	// a new report of the same numbers is new counts
	if result := harness.post(`{"report_id":"fedcba9876543210fedcba9876543210","client":{"both_failed":5}}`); result.Outcome != ExtenderCamouflageReportAccepted {
		t.Fatalf("next = %+v", result)
	}
	if count := harness.counted()[series]; count != 10 {
		t.Fatalf("%s = %v, want 10", series, count)
	}
}

// One identity past its budget is refused, told when, and counts nothing, and
// another identity is not touched by it.
func TestExtenderCamouflageReportRateLimitsPerIdentity(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	greedy := server.NewId()
	for i := range extenderCamouflageReportRateLimit {
		body := fmt.Sprintf(`{"report_id":"%032x","client":{"refused":1}}`, i)
		result := harness.reporting.report(strings.NewReader(body), harness.session(greedy, testCamouflageCallerIp))
		if result.Outcome != ExtenderCamouflageReportAccepted {
			t.Fatalf("report %d = %+v", i, result)
		}
	}
	countedBefore := harness.counted()
	body := fmt.Sprintf(`{"report_id":"%032x","client":{"refused":1}}`, 1000)
	result := harness.reporting.report(strings.NewReader(body), harness.session(greedy, testCamouflageCallerIp))
	if result.Outcome != ExtenderCamouflageReportRateLimited {
		t.Fatalf("over budget = %+v", result)
	}
	// 07:15 in an hour window
	if result.RetryAfterSeconds != 45*60 {
		t.Errorf("retry after = %d, want the rest of the window", result.RetryAfterSeconds)
	}
	if count := harness.reportCount(extenderCamouflageReportOutcomeRateLimited); count != 1 {
		t.Errorf("rate limited = %v", count)
	}
	if counted := harness.counted(); !mapsEqualFloat(counted, countedBefore) {
		t.Errorf("counted past the budget: %v, was %v", counted, countedBefore)
	}
	// refused before the claim, so its retry can still count
	if harness.claims[fmt.Sprintf("%032x", 1000)] {
		t.Errorf("a refused report claimed its id")
	}

	other := harness.reporting.report(strings.NewReader(body), harness.session(server.NewId(), testCamouflageCallerIp))
	if other.Outcome != ExtenderCamouflageReportAccepted {
		t.Errorf("another identity = %+v", other)
	}
}

func mapsEqualFloat(a map[string]float64, b map[string]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// What a client got wrong is refused whole, counted by its reason, costs no
// budget and no claim, and adds nothing.
func TestExtenderCamouflageReportHoldsItsBounds(t *testing.T) {
	longName := strings.Repeat("a", extenderCamouflageReportMaxTargetNameByteCount+1)
	manyTargets := []any{}
	for range extenderCamouflageReportMaxTargetCount + 1 {
		manyTargets = append(manyTargets, map[string]any{"name": "listed.example"})
	}
	edit := func(edit func(fields map[string]any)) func(t *testing.T) string {
		return func(t *testing.T) string { return testCamouflageReportBodyWith(t, edit) }
	}
	literal := func(body string) func(t *testing.T) string {
		return func(t *testing.T) string { return body }
	}
	extenderField := func(name string, value any) func(t *testing.T) string {
		return edit(func(fields map[string]any) { fields["extender"].(map[string]any)[name] = value })
	}
	clientField := func(name string, value any) func(t *testing.T) string {
		return edit(func(fields map[string]any) { fields["client"].(map[string]any)[name] = value })
	}
	targetField := func(name string, value any) func(t *testing.T) string {
		return edit(func(fields map[string]any) {
			fields["extender"].(map[string]any)["targets"] = []any{map[string]any{"name": "listed.example", name: value}}
		})
	}
	for _, c := range []struct {
		name   string
		body   func(t *testing.T) string
		reason string
	}{
		{"not json", literal(`{"report_id":`), extenderCamouflageReportOutcomeMalformed},
		{"not an object", literal(`[]`), extenderCamouflageReportOutcomeMalformed},
		{"a count that is a string", clientField("refused", "1"), extenderCamouflageReportOutcomeMalformed},
		{"a count that is a fraction", clientField("refused", 1.5), extenderCamouflageReportOutcomeMalformed},
		{"a count past int64", literal(`{"report_id":"0123456789abcdef0123456789abcdef","client":{"refused":9223372036854775808}}`), extenderCamouflageReportOutcomeMalformed},
		{"null", literal(`null`), extenderCamouflageReportOutcomeOutOfBounds},
		{"nothing to count", literal(`{"report_id":"0123456789abcdef0123456789abcdef"}`), extenderCamouflageReportOutcomeOutOfBounds},
		{"no id", edit(func(fields map[string]any) { delete(fields, "report_id") }), extenderCamouflageReportOutcomeOutOfBounds},
		{"a short id", edit(func(fields map[string]any) { fields["report_id"] = "abc" }), extenderCamouflageReportOutcomeOutOfBounds},
		{"an upper case id", edit(func(fields map[string]any) { fields["report_id"] = strings.Repeat("A", 32) }), extenderCamouflageReportOutcomeOutOfBounds},
		{"a key in the id", edit(func(fields map[string]any) { fields["report_id"] = strings.Repeat("*", 32) }), extenderCamouflageReportOutcomeOutOfBounds},
		{"a long version", edit(func(fields map[string]any) {
			fields["client_version"] = strings.Repeat("1", extenderCamouflageReportMaxClientVersionByteCount+1)
		}), extenderCamouflageReportOutcomeOutOfBounds},
		{"a negative resource version", edit(func(fields map[string]any) { fields["resource_version"] = -1 }), extenderCamouflageReportOutcomeOutOfBounds},
		{"a huge resource version", edit(func(fields map[string]any) {
			fields["resource_version"] = extenderCamouflageReportMaxResourceVersion + 1
		}), extenderCamouflageReportOutcomeOutOfBounds},
		{"a family that is not one", edit(func(fields map[string]any) { fields["ip_versions"] = []int{5} }), extenderCamouflageReportOutcomeOutOfBounds},
		{"too many families", edit(func(fields map[string]any) { fields["ip_versions"] = []int{4, 6, 4} }), extenderCamouflageReportOutcomeOutOfBounds},
		{"a negative client count", clientField("refused", -1), extenderCamouflageReportOutcomeOutOfBounds},
		{"a client count past its bound", clientField("both_failed", extenderCamouflageReportMaxClientCount+1), extenderCamouflageReportOutcomeOutOfBounds},
		{"a negative extender count", extenderField("spliced", -1), extenderCamouflageReportOutcomeOutOfBounds},
		{"an extender count past its bound", extenderField("spliced", extenderCamouflageReportMaxExtenderCount+1), extenderCamouflageReportOutcomeOutOfBounds},
		{"a long reason", extenderField("degraded_reason", strings.Repeat("r", extenderCamouflageReportMaxDegradedReasonByteCount+1)), extenderCamouflageReportOutcomeOutOfBounds},
		{"too many targets", extenderField("targets", manyTargets), extenderCamouflageReportOutcomeOutOfBounds},
		{"a null target", extenderField("targets", []any{nil}), extenderCamouflageReportOutcomeOutOfBounds},
		{"a long target name", extenderField("targets", []any{map[string]any{"name": longName}}), extenderCamouflageReportOutcomeOutOfBounds},
		{"a negative splice count", targetField("splice_count", -1), extenderCamouflageReportOutcomeOutOfBounds},
		{"a splice count past its bound", targetField("splice_count", extenderCamouflageReportMaxTargetSpliceCount+1), extenderCamouflageReportOutcomeOutOfBounds},
		{"a byte count past its bound", targetField("relayed_byte_count", extenderCamouflageReportMaxTargetByteCount+1), extenderCamouflageReportOutcomeOutOfBounds},
	} {
		t.Run(c.name, func(t *testing.T) {
			harness := newTestCamouflageReportHarness(t)
			result := harness.post(c.body(t))
			if result.Outcome != ExtenderCamouflageReportInvalid {
				t.Fatalf("result = %+v, want invalid", result)
			}
			if count := harness.reportCount(c.reason); count != 1 {
				t.Errorf("%s = %v, want 1", c.reason, count)
			}
			harness.requireNothingCounted()
			if len(harness.budgetSpends) != 0 || harness.claimCount != 0 {
				t.Errorf("spends = %v, claims = %d", harness.budgetSpends, harness.claimCount)
			}
		})
	}
}

// Values at each bound are a report that counts, so a sender may fill them.
func TestExtenderCamouflageReportAcceptsItsBounds(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	targets := []any{}
	for i := range extenderCamouflageReportMaxTargetCount {
		targets = append(targets, map[string]any{
			"name":               fmt.Sprintf("%s%d", strings.Repeat("a", extenderCamouflageReportMaxTargetNameByteCount-2), 10+i),
			"splice_count":       extenderCamouflageReportMaxTargetSpliceCount,
			"relayed_byte_count": extenderCamouflageReportMaxTargetByteCount,
		})
	}
	body := testCamouflageReportBodyWith(t, func(fields map[string]any) {
		fields["client_version"] = strings.Repeat("1", extenderCamouflageReportMaxClientVersionByteCount)
		fields["resource_version"] = extenderCamouflageReportMaxResourceVersion
		fields["ip_versions"] = []int{6, 4}
		fields["client"] = map[string]any{"both_failed": extenderCamouflageReportMaxClientCount}
		fields["extender"] = map[string]any{
			"spliced":                    extenderCamouflageReportMaxExtenderCount,
			"degraded_reason":            strings.Repeat("r", extenderCamouflageReportMaxDegradedReasonByteCount),
			"targets":                    targets,
			"camouflage_unauthenticated": 0,
		}
	})
	if ExtenderCamouflageReportMaxByteCount < len(body) {
		t.Errorf("a report at every bound is %d bytes, past the route's %d", len(body), ExtenderCamouflageReportMaxByteCount)
	}
	if result := harness.post(body); result.Outcome != ExtenderCamouflageReportAccepted {
		t.Fatalf("result = %+v", result)
	}
}

// The body bound is the handler's reader: past it, the post is refused for its
// size and counted.
func TestExtenderCamouflageReportRefusesABodyPastItsBound(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	request := httptest.NewRequest(http.MethodPost, "/network/extender-camouflage-report", strings.NewReader(
		`{"report_id":"`+strings.Repeat("x", ExtenderCamouflageReportMaxByteCount)+`"}`,
	))
	body := http.MaxBytesReader(httptest.NewRecorder(), request.Body, ExtenderCamouflageReportMaxByteCount)
	result := harness.reporting.report(body, harness.session(server.NewId(), testCamouflageCallerIp))
	if result.Outcome != ExtenderCamouflageReportInvalid {
		t.Fatalf("result = %+v", result)
	}
	if count := harness.reportCount(extenderCamouflageReportOutcomeTooLarge); count != 1 {
		t.Errorf("too large = %v", count)
	}
}

// A sender of a later version may add fields, and a hostile one may try naming
// things: neither has any effect. The country is the server's own lookup.
func TestExtenderCamouflageReportIgnoresUnknownFieldsAndClaims(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	result := harness.post(`{
		"report_id": "0123456789abcdef0123456789abcdef",
		"country": "zz",
		"country_code": "zz",
		"family": "ipv9",
		"client_id": "client",
		"later_field": {"nested": [1, 2, 3]},
		"client": {"refused": 2, "later_count": 7, "country": "zz"}
	}`)
	if result.Outcome != ExtenderCamouflageReportAccepted {
		t.Fatalf("result = %+v", result)
	}
	counted := harness.counted()
	series := "urnetwork_extender_camouflage_client_outcomes_total{country=de,family=any,outcome=refused}"
	if counted[series] != 2 {
		t.Fatalf("counted = %v", counted)
	}
	if slices.Contains(harness.labelValues(), "zz") || slices.Contains(harness.labelValues(), "ipv9") {
		t.Errorf("a claim reached a label: %v", harness.labelValues())
	}
}

// Client words never become labels: each dimension reads through its fixed set.
func TestExtenderCamouflageReportLabelsAreFixedSets(t *testing.T) {
	now := time.Date(2026, 11, 2, 7, 15, 0, 0, time.UTC)
	t.Run("client version", func(t *testing.T) {
		for version, want := range map[string]string{
			"":                             "unknown",
			"2026.10.30-1085916410":        "2026.10.30",
			"2026.1.2":                     "2026.1",
			"connectctl 0.0.1":             "other",
			"<script>":                     "other",
			"198.51.100.30":                "other",
			"01HZXK0000000000000000000000": "other",
			"2099.1.1":                     "other",
		} {
			if label := server.AppVersionLabel(version, now); label != want {
				t.Errorf("version %q = %q, want %q", version, label, want)
			}
		}
	})
	t.Run("resource version", func(t *testing.T) {
		labels := newExtenderCamouflageReportLabels([]*connect.ExtenderBorrowList{{Version: 10}})
		for version, want := range map[int]string{
			0: "none", 5: "other", 6: "6", 10: "10", 14: "14", 15: "other", 1 << 20: "other",
		} {
			if label := labels.resourceVersion(version); label != want {
				t.Errorf("version %d = %q, want %q", version, label, want)
			}
		}
		// an unversioned build still reads a client that holds a newer list
		unversioned := newExtenderCamouflageReportLabels([]*connect.ExtenderBorrowList{{}})
		for version, want := range map[int]string{0: "none", 1: "1", 4: "4", 5: "other"} {
			if label := unversioned.resourceVersion(version); label != want {
				t.Errorf("unversioned %d = %q, want %q", version, label, want)
			}
		}
	})
	t.Run("target", func(t *testing.T) {
		labels := newExtenderCamouflageReportLabels([]*connect.ExtenderBorrowList{
			{Names: []connect.ExtenderBorrowName{{Name: "Global.Example"}}},
			{CountryCode: "ir", Names: []connect.ExtenderBorrowName{
				{Name: "country.example"},
				{Name: "staged.example", SelectableFromVersion: 9},
				{Name: "retiring.example", SelectableUntilVersion: 1},
			}},
		})
		for name, want := range map[string]string{
			"global.example":   "global.example",
			" GLOBAL.example ": "global.example",
			"country.example":  "country.example",
			"staged.example":   "staged.example",
			"retiring.example": "retiring.example",
			"other.example":    "other",
			"":                 "other",
			"192.0.2.10":       "other",
			"other":            "other",
		} {
			if label := labels.target(name); label != want {
				t.Errorf("name %q = %q, want %q", name, label, want)
			}
		}
	})
	t.Run("the allowlist is bounded", func(t *testing.T) {
		names := []connect.ExtenderBorrowName{}
		for i := range extenderCamouflageReportMaxTargetLabelCount + 10 {
			names = append(names, connect.ExtenderBorrowName{Name: fmt.Sprintf("name%d.example", i)})
		}
		labels := newExtenderCamouflageReportLabels([]*connect.ExtenderBorrowList{{Names: names}})
		if len(labels.targets) != extenderCamouflageReportMaxTargetLabelCount {
			t.Errorf("allowlist = %d names", len(labels.targets))
		}
	})
	t.Run("the verdict", func(t *testing.T) {
		for _, c := range []struct {
			covered bool
			reason  string
			want    string
		}{
			{true, "", "covered"},
			{true, "splice_off", "other"},
			{false, "", "other"},
			{false, extender.ExtenderCamouflageDegradedCamouflageOff, "camouflage_off"},
			{false, extender.ExtenderCamouflageDegradedSpliceOff, "splice_off"},
			{false, extender.ExtenderCamouflageDegradedGateClosed, "splice_gate_closed"},
			{false, extender.ExtenderCamouflageDegradedNoVerifiedTarget, "no_verified_target"},
			{false, extender.ExtenderCamouflageDegradedFallbackActive, "splice_fallback_active"},
			{false, extender.ExtenderCamouflageDegradedCoverageIncomplete, "coverage_incomplete"},
			{false, "covered", "other"},
			{false, "198.51.100.30", "other"},
		} {
			if verdict := extenderCamouflageReportVerdict(c.covered, c.reason); verdict != c.want {
				t.Errorf("verdict(%v, %q) = %q, want %q", c.covered, c.reason, verdict, c.want)
			}
		}
	})
}

// With today's bundled lists, which are empty placeholders, every target reads
// other and no name reaches a label. When the lists are measured this reads the
// names, and the allowlist is what the test above pins.
func TestExtenderCamouflageReportBundledAllowlistHoldsOnlyBundledNames(t *testing.T) {
	bundled := map[string]bool{}
	for _, borrowList := range connect.ExtenderCamouflageBorrowLists() {
		for _, name := range borrowList.AcceptNames() {
			bundled[name] = true
		}
	}
	labels := bundledExtenderCamouflageReportLabels()
	for name := range labels.targets {
		if !bundled[name] {
			t.Errorf("allowlist holds %q, which no bundled list does", name)
		}
	}
	if label := labels.target("not-bundled.invalid"); label != extenderCamouflageReportLabelOther {
		t.Errorf("an unlisted name reads %q", label)
	}
}

// Whatever a report says, and whoever posts it, no identifier reaches a label: not
// the caller's ids or address, not an extender's key or address, and not any
// string the report supplied. The caller's identity spends a budget and goes no
// further.
func TestExtenderCamouflageReportNoIdentifierReachesALabel(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	clientId := server.NewId()
	networkId := server.NewId()
	userId := server.NewId()
	extenderKeySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	extenderKeyHex := connect.ExtenderKeySeedHex(extenderKeySeed)
	harness.fleet = newExtenderCamouflageFleet([]*model.NetworkExtenderOwnerAddress{{
		ClientId: clientId,
		Ip:       netip.MustParseAddr(testCamouflageExtenderIp),
	}})
	identifiers := []string{
		clientId.String(), networkId.String(), userId.String(),
		extenderKeyHex,
		testCamouflageCallerIp, testCamouflageRelayIp, testCamouflageExtenderIp,
		"secret.example",
	}

	// a report that puts an identifier in every string it holds
	for _, identifier := range identifiers {
		body := testCamouflageReportBodyWith(t, func(fields map[string]any) {
			if len(identifier) <= extenderCamouflageReportMaxClientVersionByteCount {
				fields["client_version"] = identifier
			}
			fields["country"] = identifier
			extenderFields := fields["extender"].(map[string]any)
			extenderFields["degraded_reason"] = identifier[:min(len(identifier), extenderCamouflageReportMaxDegradedReasonByteCount)]
			extenderFields["targets"] = []any{map[string]any{"name": identifier, "splice_count": 1, "relayed_byte_count": 1}}
		})
		clientSession := session.Testing_CreateClientSession(context.Background(), &session.ByJwt{
			NetworkId: networkId,
			UserId:    userId,
			ClientId:  &clientId,
		})
		t.Cleanup(clientSession.Cancel)
		clientSession.ClientAddress = netip.AddrPortFrom(netip.MustParseAddr(testCamouflageExtenderIp), 4500).String()
		if result := harness.reporting.report(strings.NewReader(body), clientSession); result.Outcome != ExtenderCamouflageReportAccepted {
			t.Fatalf("result = %+v", result)
		}
	}

	values := harness.labelValues()
	if len(values) == 0 {
		t.Fatal("nothing was counted")
	}
	for _, value := range values {
		for _, identifier := range identifiers {
			if strings.Contains(value, identifier) {
				t.Errorf("label value %q holds identifier %q", value, identifier)
			}
		}
	}
	// and the client id is the only identity the budget saw, in the fake; the real
	// key is a keyed hash, which the redis test reads
	if len(harness.budgetSpends) != 1 || harness.budgetSpends[clientId] == 0 {
		t.Errorf("spends = %v", harness.budgetSpends)
	}
}

// An extender's part counts only from a client that activated an active extender.
// Rollback reads those counters, so anyone else's is dropped, the client's part
// stands, and the sender hears nothing different.
func TestExtenderCamouflageReportTakesAnExtenderPartOnlyFromItsOwner(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	ownerId := server.NewId()
	harness.fleet = newExtenderCamouflageFleet([]*model.NetworkExtenderOwnerAddress{{
		ClientId: ownerId,
		Ip:       netip.MustParseAddr(testCamouflageExtenderIp),
	}})

	result := harness.reporting.report(strings.NewReader(testCamouflageReportBody), harness.session(server.NewId(), testCamouflageCallerIp))
	if result.Outcome != ExtenderCamouflageReportAccepted {
		t.Fatalf("result = %+v", result)
	}
	if count := harness.reportCount(extenderCamouflageReportOutcomeAcceptedExtenderIgnored); count != 1 {
		t.Errorf("ignored = %v", count)
	}
	for series := range harness.counted() {
		if !strings.HasPrefix(series, "urnetwork_extender_camouflage_client_") {
			t.Errorf("a stranger's report counted %s", series)
		}
	}
	if len(harness.counted()) == 0 {
		t.Errorf("the client part did not count")
	}

	// and the claim is spent, so a retry does not count the client part again
	second := harness.reporting.report(strings.NewReader(testCamouflageReportBody), harness.session(ownerId, testCamouflageExtenderIp))
	if second.Outcome != ExtenderCamouflageReportDuplicate {
		t.Errorf("second = %+v", second)
	}
}

// The country is the server's own lookup of the caller, and a report that crossed
// an extender does not take that extender's country.
func TestExtenderCamouflageReportCountryIsTheServersLookup(t *testing.T) {
	ownerId := server.NewId()
	relayOwnerId := server.NewId()
	fleet := newExtenderCamouflageFleet([]*model.NetworkExtenderOwnerAddress{
		{ClientId: ownerId, Ip: netip.MustParseAddr(testCamouflageExtenderIp)},
		{ClientId: relayOwnerId, Ip: netip.MustParseAddr(testCamouflageRelayIp)},
		{ClientId: relayOwnerId, Ip: netip.MustParseAddr("2001:db8:1:2::10")},
	})
	for _, c := range []struct {
		name      string
		clientId  server.Id
		address   string
		countries map[netip.Addr]string
		want      string
	}{
		{name: "an ordinary caller", clientId: server.NewId(), address: testCamouflageCallerIp + ":4500", want: "de"},
		{name: "an extender on its own address", clientId: ownerId, address: testCamouflageExtenderIp + ":4500", want: "fr"},
		{name: "through an extender", clientId: server.NewId(), address: testCamouflageRelayIp + ":4500", want: "relayed"},
		{name: "an extender through another", clientId: ownerId, address: testCamouflageRelayIp + ":4500", want: "relayed"},
		{name: "through another address of a v6 extender's network", clientId: server.NewId(), address: "[2001:db8:1:2::77]:4500", want: "relayed"},
		{name: "beside a v6 extender's network", clientId: server.NewId(), address: "[2001:db8:1:3::77]:4500", want: "unknown"},
		{name: "beside a v4 extender", clientId: server.NewId(), address: "203.0.113.8:4500", want: "unknown"},
		{name: "unlocated", clientId: server.NewId(), address: "198.51.100.99:4500", want: "unknown"},
		{name: "unreadable", clientId: server.NewId(), address: "not an address", want: "unknown"},
		{
			name:      "a location past two letters",
			clientId:  server.NewId(),
			address:   testCamouflageCallerIp + ":4500",
			countries: map[netip.Addr]string{netip.MustParseAddr(testCamouflageCallerIp): "<script>"},
			want:      "unknown",
		},
		{
			name:      "an upper case location",
			clientId:  server.NewId(),
			address:   testCamouflageCallerIp + ":4500",
			countries: map[netip.Addr]string{netip.MustParseAddr(testCamouflageCallerIp): "NL"},
			want:      "nl",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			harness := newTestCamouflageReportHarness(t)
			if c.countries != nil {
				harness.countries = c.countries
			}
			if country := harness.reporting.reportCountry(c.address, c.clientId, fleet); country != c.want {
				t.Errorf("country = %q, want %q", country, c.want)
			}
		})
	}
}

// The process's own lookup does without a database: an api refuses to start without
// one, but a process that has none, where every lookup panics, reads unknown.
func TestExtenderCamouflageReportCountryLookupDoesNotPanic(t *testing.T) {
	countryCode, err := extenderCamouflageReportCountryCode(netip.MustParseAddr(testCamouflageCallerIp))
	if err != nil && countryCode != "" {
		t.Errorf("country = %q with err = %v", countryCode, err)
	}
}

// Every failure past the client undercounts: nothing is added, the sender may post
// again, and when the claim was not made the report is not lost by it.
func TestExtenderCamouflageReportFailuresCountNothing(t *testing.T) {
	failure := errors.New("no route to redis")
	for _, c := range []struct {
		name  string
		setup func(harness *testCamouflageReportHarness)
		// the post that came before its claim can count later
		claimed bool
	}{
		{name: "the budget", setup: func(harness *testCamouflageReportHarness) { harness.budgetErr = failure }},
		{name: "no fleet yet", setup: func(harness *testCamouflageReportHarness) { harness.fleet = nil }},
		{name: "the claim", setup: func(harness *testCamouflageReportHarness) { harness.claimErr = failure }},
	} {
		t.Run(c.name, func(t *testing.T) {
			harness := newTestCamouflageReportHarness(t)
			c.setup(harness)
			result := harness.post(testCamouflageReportBody)
			if result.Outcome != ExtenderCamouflageReportUnavailable {
				t.Fatalf("result = %+v", result)
			}
			harness.requireNothingCounted()
			if count := harness.reportCount(extenderCamouflageReportOutcomeUnavailable); count != 1 {
				t.Errorf("unavailable = %v", count)
			}
			if len(harness.claims) != 0 {
				t.Errorf("claims = %v", harness.claims)
			}

			harness.budgetErr = nil
			harness.claimErr = nil
			harness.fleet = newExtenderCamouflageFleet(nil)
			if result := harness.post(testCamouflageReportBody); result.Outcome != ExtenderCamouflageReportAccepted {
				t.Errorf("the post after = %+v", result)
			}
		})
	}
}

// The fleet is read once per interval per process, a failed read keeps the copy at
// hand, and the first request gets what the first read found.
func TestExtenderCamouflageFleetCacheReadsOncePerInterval(t *testing.T) {
	stateLock := sync.Mutex{}
	readCount := 0
	var readErr error
	ownerId := server.NewId()
	cache := &extenderCamouflageFleetCache{
		read: func(ctx context.Context) []*model.NetworkExtenderOwnerAddress {
			stateLock.Lock()
			defer stateLock.Unlock()
			readCount += 1
			if _, bounded := ctx.Deadline(); !bounded {
				t.Errorf("a read with no bound")
			}
			if readErr != nil {
				panic(readErr)
			}
			return []*model.NetworkExtenderOwnerAddress{{ClientId: ownerId, Ip: netip.MustParseAddr(testCamouflageExtenderIp)}}
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	now := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)

	fleet := cache.get(ctx, now)
	if fleet == nil || !fleet.owner(ownerId) {
		t.Fatalf("first read = %+v", fleet)
	}
	if fleet := cache.get(ctx, now.Add(extenderCamouflageFleetCacheTimeout-time.Second)); fleet == nil || readCount != 1 {
		t.Fatalf("within the interval: %+v, reads = %d", fleet, readCount)
	}

	// a request that has gone away does not cancel the process's read
	cancel()
	readErr = errors.New("the database is away")
	if fleet := cache.get(ctx, now.Add(extenderCamouflageFleetCacheTimeout)); fleet == nil || !fleet.owner(ownerId) || readCount != 2 {
		t.Fatalf("failed read: %+v, reads = %d, want the copy at hand", fleet, readCount)
	}
	// and the failure waits out an interval too, so an outage costs one read each
	if fleet := cache.get(ctx, now.Add(extenderCamouflageFleetCacheTimeout+time.Second)); fleet == nil || readCount != 2 {
		t.Fatalf("after the failed read: %+v, reads = %d", fleet, readCount)
	}

	empty := &extenderCamouflageFleetCache{read: func(ctx context.Context) []*model.NetworkExtenderOwnerAddress {
		panic(errors.New("the database is away"))
	}}
	if fleet := empty.get(context.Background(), now); fleet != nil {
		t.Fatalf("no read has worked: %+v", fleet)
	}
}

// While one request reads the fleet, the others are answered from the copy at hand
// and do not wait for it.
func TestExtenderCamouflageFleetCacheAnswersOthersDuringARead(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	ownerId := server.NewId()
	cache := &extenderCamouflageFleetCache{
		read: func(ctx context.Context) []*model.NetworkExtenderOwnerAddress {
			close(started)
			<-release
			return []*model.NetworkExtenderOwnerAddress{{ClientId: ownerId, Ip: netip.MustParseAddr(testCamouflageExtenderIp)}}
		},
	}
	now := time.Date(2026, 11, 2, 7, 0, 0, 0, time.UTC)
	done := make(chan *extenderCamouflageFleet, 1)
	go func() {
		done <- cache.get(context.Background(), now)
	}()
	<-started
	if fleet := cache.get(context.Background(), now); fleet != nil {
		t.Errorf("no copy yet: %+v", fleet)
	}
	close(release)
	if fleet := <-done; fleet == nil || !fleet.owner(ownerId) {
		t.Errorf("the read = %+v", fleet)
	}
}

// The real Redis commands and the real read: the budget holds per client across
// its window and names no client in its key, the claim holds and expires, and the
// fleet is the active extenders, with no row written anywhere.
func TestExtenderCamouflageReportAgainstRedisAndTheDatabase(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		installTestReleaseConfig(t)

		greedy := server.NewId()
		other := server.NewId()
		// a fixed time, so a window turning over mid test does not make room
		budgetTime := server.NowUtc()
		spend := func(clientId server.Id) bool {
			t.Helper()
			allowed, err := spendExtenderCamouflageReportRateBudget(ctx, clientId, budgetTime)
			if err != nil {
				t.Fatal(err)
			}
			return allowed
		}
		for i := range extenderCamouflageReportRateLimit {
			if !spend(greedy) {
				t.Fatalf("report %d of the budget was refused", i)
			}
		}
		if spend(greedy) {
			t.Errorf("the budget never refused")
		}
		if !spend(other) {
			t.Errorf("one client's spend reached another's budget")
		}
		// the next window is a new budget
		budgetTime = budgetTime.Add(extenderCamouflageReportRateWindow)
		if !spend(greedy) {
			t.Errorf("the next window held the last one's spends")
		}
		budgetTime = budgetTime.Add(-extenderCamouflageReportRateWindow)

		server.Redis(ctx, func(r server.RedisClient) {
			keys, err := r.Keys(ctx, "extender_camouflage_report_rate_*").Result()
			server.Raise(err)
			if len(keys) != 3 {
				t.Errorf("budget keys = %d, want two clients and the next window", len(keys))
			}
			// what the store holds names no client, and a key goes when its window ends
			for _, key := range keys {
				for _, clientId := range []server.Id{greedy, other} {
					if strings.Contains(key, clientId.String()) || strings.Contains(key, hex.EncodeToString(clientId.Bytes())) {
						t.Errorf("budget key %q names a client", key)
					}
				}
				ttl, err := r.TTL(ctx, key).Result()
				server.Raise(err)
				if ttl <= 0 || 2*extenderCamouflageReportRateWindow < ttl {
					t.Errorf("budget key ttl = %s", ttl)
				}
			}
		})

		reportId := strings.Repeat("ab", extenderCamouflageReportIdLength/2)
		first, err := claimExtenderCamouflageReport(ctx, reportId)
		if err != nil || !first {
			t.Fatalf("first claim = %v, %v", first, err)
		}
		again, err := claimExtenderCamouflageReport(ctx, reportId)
		if err != nil || again {
			t.Fatalf("second claim = %v, %v", again, err)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			key := "extender_camouflage_report_id_" + reportId
			ttl, err := r.TTL(ctx, key).Result()
			server.Raise(err)
			if ttl <= extenderCamouflageReportClaimTtl-time.Minute || extenderCamouflageReportClaimTtl < ttl {
				t.Errorf("claim ttl = %s", ttl)
			}
			value, err := r.Get(ctx, key).Result()
			server.Raise(err)
			if value != "1" {
				t.Errorf("claim value = %q", value)
			}
		})

		activeKeyHex, _ := createTestReleaseExtender(t, ctx, 1)
		_, inactiveExtenderId := createTestReleaseExtender(t, ctx, 2)
		setTestReleaseExtenderActive(t, ctx, inactiveExtenderId, false)
		ownerAddresses := model.GetActiveNetworkExtenderOwnerAddresses(ctx)
		if len(ownerAddresses) != 1 {
			t.Fatalf("owner addresses = %d", len(ownerAddresses))
		}
		activeKey, err := hex.DecodeString(activeKeyHex)
		if err != nil {
			t.Fatal(err)
		}
		active := model.GetNetworkExtenderByPublicKey(ctx, activeKey)
		if active == nil || ownerAddresses[0].ClientId != active.ClientId ||
			ownerAddresses[0].Ip != netip.MustParseAddr("192.0.2.11") {
			t.Errorf("owner address = %+v", ownerAddresses[0])
		}

		// the whole route on the process's own parts: the first report of an id
		// counts and is not written anywhere but the counters
		registry := prometheus.NewRegistry()
		metrics := newExtenderCamouflageReportMetrics(registry)
		fleetCache := &extenderCamouflageFleetCache{read: model.GetActiveNetworkExtenderOwnerAddresses}
		reporting := &extenderCamouflageReporting{
			enabled:         func() bool { return true },
			now:             server.NowUtc,
			spendRateBudget: spendExtenderCamouflageReportRateBudget,
			claimReport:     claimExtenderCamouflageReport,
			fleet:           fleetCache.get,
			country:         func(ip netip.Addr) (string, error) { return "de", nil },
			labels:          bundledExtenderCamouflageReportLabels,
			metrics:         metrics,
		}
		clientId := server.NewId()
		clientSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: server.NewId(),
			UserId:    server.NewId(),
			ClientId:  &clientId,
		})
		defer clientSession.Cancel()
		clientSession.ClientAddress = testCamouflageCallerIp + ":4500"
		body := `{"report_id":"cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd","client":{"both_failed":3}}`
		attemptsBefore := testCamouflageReportAccountActionCount(t, ctx)
		if result := reporting.report(strings.NewReader(body), clientSession); result.Outcome != ExtenderCamouflageReportAccepted {
			t.Fatalf("first report = %+v", result)
		}
		if result := reporting.report(strings.NewReader(body), clientSession); result.Outcome != ExtenderCamouflageReportDuplicate {
			t.Fatalf("second report = %+v", result)
		}
		if count := testutil.ToFloat64(metrics.clientOutcomes.WithLabelValues("both_failed", "de", "any")); count != 3 {
			t.Errorf("both_failed = %v", count)
		}
		if attempts := testCamouflageReportAccountActionCount(t, ctx); attempts != attemptsBefore {
			t.Errorf("a report wrote %d account action rows", attempts-attemptsBefore)
		}
	})
}

// The rows of the table the database rate limits write to, which the report must
// not: it records no identity.
func testCamouflageReportAccountActionCount(t testing.TB, ctx context.Context) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM network_user_action_attempt`)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}
