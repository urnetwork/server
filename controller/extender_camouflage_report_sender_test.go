package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The real sender against the real route: connect's types and bounds are this
// route's, and what the sender posts is counted once across a lost answer. The two
// are in separate repositories, so this is where a drift between them shows.

// A report from connect's own types, every field set, comes through the route as
// every counter it names. A field the route does not read would count nothing here.
func TestExtenderCamouflageReportReadsTheSendersTypes(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	clientId := server.NewId()
	harness.fleet = newExtenderCamouflageFleet(testCamouflageFleetOf(clientId))

	args := &connect.ExtenderCamouflageReportArgs{
		ReportId:        "0123456789abcdef0123456789abcdef",
		ClientVersion:   "2026.10.30-1085916410",
		ResourceVersion: 3,
		IpVersions:      []int{4},
		Client:          &connect.ExtenderCamouflageClientCounts{},
		Extender: &connect.ExtenderCamouflageExtenderCounts{
			Targets:        []*connect.ExtenderCamouflageTargetCounts{{Name: "listed.example", SpliceCount: 1, RelayedByteCount: 1}},
			DegradedReason: extender.ExtenderCamouflageDegradedNoVerifiedTarget,
		},
	}
	setEveryCount := func(counts any) {
		value := reflect.ValueOf(counts).Elem()
		for i := range value.NumField() {
			if field := value.Field(i); field.Kind() == reflect.Int64 {
				field.SetInt(int64(i + 1))
			}
		}
	}
	setEveryCount(args.Client)
	setEveryCount(args.Extender)
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}

	result := harness.reporting.report(bytes.NewReader(body), harness.session(clientId, testCamouflageExtenderIp))
	if result.Outcome != ExtenderCamouflageReportAccepted {
		t.Fatalf("result = %+v", result)
	}
	outcomes := map[string]map[string]float64{}
	for series, count := range harness.counted() {
		family, labels, _ := strings.Cut(series, "{")
		if outcomes[family] == nil {
			outcomes[family] = map[string]float64{}
		}
		outcomes[family][labels] = count
	}
	clientFields := reflect.TypeOf(connect.ExtenderCamouflageClientCounts{}).NumField()
	if count := len(outcomes["urnetwork_extender_camouflage_client_outcomes_total"]); count != clientFields {
		t.Errorf("%d client outcomes counted of the sender's %d", count, clientFields)
	}
	extenderCounts := 0
	extenderType := reflect.TypeOf(connect.ExtenderCamouflageExtenderCounts{})
	for i := range extenderType.NumField() {
		if extenderType.Field(i).Type.Kind() == reflect.Int64 {
			extenderCounts += 1
		}
	}
	if count := len(outcomes["urnetwork_extender_camouflage_extender_outcomes_total"]); count != extenderCounts {
		t.Errorf("%d extender outcomes counted of the sender's %d", count, extenderCounts)
	}
	if outcomes["urnetwork_extender_camouflage_status_reports_total"]["country=fr,family=ipv4,verdict=no_verified_target}"] != 1 {
		t.Errorf("verdicts = %v", outcomes["urnetwork_extender_camouflage_status_reports_total"])
	}
	if outcomes["urnetwork_extender_camouflage_target_splices_total"]["target=listed.example}"] != 1 {
		t.Errorf("splices = %v", outcomes["urnetwork_extender_camouflage_target_splices_total"])
	}
}

// The sender's bounds and answers are the route's. The sender stays inside them so a
// route never refuses one that behaves, and a sender stops an id before the route lets
// it go.
func TestExtenderCamouflageReportSenderSharesTheRoutesBounds(t *testing.T) {
	for name, pair := range map[string][2]int64{
		"client count":       {connect.ExtenderCamouflageReportMaxClientCount, extenderCamouflageReportMaxClientCount},
		"extender count":     {connect.ExtenderCamouflageReportMaxExtenderCount, extenderCamouflageReportMaxExtenderCount},
		"target splices":     {connect.ExtenderCamouflageReportMaxTargetSpliceCount, extenderCamouflageReportMaxTargetSpliceCount},
		"target bytes":       {connect.ExtenderCamouflageReportMaxTargetByteCount, extenderCamouflageReportMaxTargetByteCount},
		"target count":       {connect.ExtenderCamouflageReportMaxTargetCount, extenderCamouflageReportMaxTargetCount},
		"target name length": {connect.ExtenderCamouflageReportMaxTargetNameByteCount, extenderCamouflageReportMaxTargetNameByteCount},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: the sender's %d, the route's %d", name, pair[0], pair[1])
		}
	}
	for sender, route := range map[string]string{
		connect.ExtenderCamouflageReportAccepted:    ExtenderCamouflageReportAccepted,
		connect.ExtenderCamouflageReportDuplicate:   ExtenderCamouflageReportDuplicate,
		connect.ExtenderCamouflageReportRateLimited: ExtenderCamouflageReportRateLimited,
		connect.ExtenderCamouflageReportDisabled:    ExtenderCamouflageReportDisabled,
		connect.ExtenderCamouflageReportInvalid:     ExtenderCamouflageReportInvalid,
		connect.ExtenderCamouflageReportUnavailable: ExtenderCamouflageReportUnavailable,
	} {
		if sender != route {
			t.Errorf("the sender's %q, the route's %q", sender, route)
		}
	}
	settings := connect.DefaultExtenderCamouflageReporterSettings()
	if extenderCamouflageReportClaimTtl <= settings.MaxReportAge {
		t.Errorf("a sender keeps an id %s, and the route holds it %s", settings.MaxReportAge, extenderCamouflageReportClaimTtl)
	}
	// its worst hour fits the budget: every post of two reports
	if extenderCamouflageReportRateLimit < 2*settings.MaxAttemptCount {
		t.Errorf("the budget is %d an hour, and a sender may post %d", extenderCamouflageReportRateLimit, 2*settings.MaxAttemptCount)
	}
}

// An extender part from the real server, through the real sender, into the real route,
// with its first answer lost: the operator holds exactly what the server counted.
func TestExtenderCamouflageReporterIsCountedOnceByTheRoute(t *testing.T) {
	harness := newTestCamouflageReportHarness(t)
	clientId := server.NewId()
	harness.fleet = newExtenderCamouflageFleet(testCamouflageFleetOf(clientId))

	serverSettings := extender.DefaultExtenderSettings()
	extenderServer := extender.NewExtenderServer(context.Background(), nil, nil, nil, &net.Dialer{}, serverSettings)
	defer extenderServer.CloseAndWait()

	clock := time.Date(2026, 11, 2, 7, 15, 0, 0, time.UTC)
	stepped := make(chan time.Time, 8)
	parked := make(chan struct{}, 8)
	answers := make(chan string, 8)
	lostFirst := true
	settings := connect.DefaultExtenderCamouflageReporterSettings()
	settings.ApiUrl = "https://api.report.example"
	settings.ByJwt = func() string { return "test-token" }
	settings.Now = func() time.Time { return clock }
	settings.After = func(wait time.Duration) <-chan time.Time {
		parked <- struct{}{}
		return stepped
	}
	settings.Random = func() float64 { return 0 }
	settings.IpVersions = func() []int { return []int{4} }
	settings.Post = func(ctx context.Context, requestUrl string, requestBodyBytes []byte, byJwt string) ([]byte, error) {
		if !strings.HasSuffix(requestUrl, "/network/extender-camouflage-report") {
			t.Errorf("url = %q", requestUrl)
		}
		result := harness.reporting.report(bytes.NewReader(requestBodyBytes), harness.session(clientId, testCamouflageExtenderIp))
		answers <- result.Outcome
		if lostFirst {
			lostFirst = false
			return nil, errors.New("the connection dropped after the request was sent")
		}
		return json.Marshal(&connect.ExtenderCamouflageReportResult{
			Outcome:           result.Outcome,
			RetryAfterSeconds: result.RetryAfterSeconds,
		})
	}
	reporter := connect.NewExtenderCamouflageReporter(context.Background(), settings)
	defer reporter.Close()
	reporter.SetSource(extenderServer)
	reporter.SetEnabled(true)

	step := func() string {
		t.Helper()
		select {
		case <-parked:
		case <-time.After(30 * time.Second):
			t.Fatal("the sender did not park")
		}
		clock = clock.Add(time.Hour)
		stepped <- clock
		select {
		case outcome := <-answers:
			return outcome
		case <-time.After(30 * time.Second):
			t.Fatal("the sender did not post")
			return ""
		}
	}
	if outcome := step(); outcome != ExtenderCamouflageReportAccepted {
		t.Errorf("the first post = %q", outcome)
	}
	if outcome := step(); outcome != ExtenderCamouflageReportDuplicate {
		t.Errorf("the retry = %q", outcome)
	}
	// The sender counts the delivery in settle, after Post returns: it has
	// counted once it parks for its next look.
	select {
	case <-parked:
	case <-time.After(30 * time.Second):
		t.Fatal("the sender did not park after the retry")
	}
	series := "urnetwork_extender_camouflage_status_reports_total{country=fr,family=ipv4,verdict=camouflage_off}"
	if count := harness.counted()[series]; count != 1 {
		t.Errorf("%s = %v after a retry", series, count)
	}
	if stats := reporter.Stats(); stats.DeliveredCount != 1 || stats.AttemptCount != 2 {
		t.Errorf("stats = %+v", stats)
	}
	if count := harness.reportCount(extenderCamouflageReportOutcomeDuplicate); count != 1 {
		t.Errorf("duplicates = %v", count)
	}

	// the sender's own identity never reaches the route's labels
	for _, value := range harness.labelValues() {
		if strings.Contains(value, "test-token") || strings.Contains(value, clientId.String()) {
			t.Errorf("label value %q names the caller", value)
		}
	}
}

// A fleet of one extender at the usual address, activated by this client.
func testCamouflageFleetOf(clientId server.Id) []*model.NetworkExtenderOwnerAddress {
	return []*model.NetworkExtenderOwnerAddress{{
		ClientId: clientId,
		Ip:       netip.MustParseAddr(testCamouflageExtenderIp),
	}}
}
