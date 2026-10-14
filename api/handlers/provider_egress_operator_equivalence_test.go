package handlers

// The operator ingest has two transports: ingest.Client over the operator API
// these handlers serve, and localoperator.Operator, which the taskworker runs
// in-process against the same controller code. These tests run one scenario --
// every operator call the URL prober and the blackhole sweep make, with their
// edge cases and refusals -- through each transport in its own fresh database
// with the same fixed identities, and require the same returns, the same
// errors and the same durable rows.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/localoperator"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/router"
)

const operatorEquivalenceSecret = "operator-equivalence-secret-0123456789"

// Identities fixed across both databases, so hash-derived pacing, shard slots
// and claim ordinals agree run for run.
var (
	operatorEquivalenceNetworkA = server.RequireParseId("019a0f00-0000-4000-8000-00000000a001")
	operatorEquivalenceDeviceA  = server.RequireParseId("019a0f00-0000-4000-8000-00000000a002")
	operatorEquivalenceClientA  = server.RequireParseId("019a0f00-0000-4000-8000-00000000a003")
	operatorEquivalenceNetworkB = server.RequireParseId("019a0f00-0000-4000-8000-00000000b001")
	operatorEquivalenceDeviceB  = server.RequireParseId("019a0f00-0000-4000-8000-00000000b002")
	operatorEquivalenceClientB  = server.RequireParseId("019a0f00-0000-4000-8000-00000000b003")
	operatorEquivalenceUnknown  = server.RequireParseId("019a0f00-0000-4000-8000-00000000c003")
	operatorEquivalenceRun      = server.RequireParseId("019a0f00-0000-4000-8000-00000000d001")
	operatorEquivalenceRunBad   = server.RequireParseId("019a0f00-0000-4000-8000-00000000d002")
)

// The labels the normalized transcript keeps for the fixed identities; every
// other id is a fixture's random one and reads "<id>".
var operatorEquivalenceLabels = map[string]string{
	operatorEquivalenceNetworkA.String(): "network-a",
	operatorEquivalenceDeviceA.String():  "device-a",
	operatorEquivalenceClientA.String():  "client-a",
	operatorEquivalenceNetworkB.String(): "network-b",
	operatorEquivalenceDeviceB.String():  "device-b",
	operatorEquivalenceClientB.String():  "client-b",
	operatorEquivalenceUnknown.String():  "client-unknown",
	operatorEquivalenceRun.String():      "run",
	operatorEquivalenceRunBad.String():   "run-bad",
}

// A small URL catalog, so the destination pool is served from config this test
// owns rather than a workstation's qualityprobe.yml.
const operatorEquivalenceCatalog = `
schema_version: 1
destinations:
  - {name: equivalence-dns, class: dns, category: dns, url: "https://dns.example/dns-query?name=example.com&type=A", verify: {kind: dns_json}}
  - {name: equivalence-connectivity, class: connectivity, category: connectivity, url: "https://connectivity.example/", verify: {kind: contains, text: equivalence-success}}
  - {name: equivalence-cdn, class: cdn, category: cdn, url: "https://cdn.example/"}
  - {name: equivalence-news, class: site, category: news, url: "https://news.example/"}
settings:
  site_pool_size: {dns: 1, connectivity: 1, cdn: 1, site: 1}
  site_sample_size: {dns: 1, connectivity: 1, cdn: 1, site: 1}
candidates: []
`

// One provider placed at Palo Alto, connected and providing publicly.
func operatorEquivalenceProvider(t testing.TB, ctx context.Context, networkId, deviceId, clientId, locationId server.Id, address string) {
	model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "", "")
	handlerId := model.CreateNetworkClientHandler(ctx)
	connectionId, _, _, _, err := model.ConnectNetworkClient(ctx, clientId, address, handlerId)
	if err != nil {
		t.Fatalf("connect client: %s", err)
	}
	if err := model.SetConnectionLocation(ctx, connectionId, locationId, &model.ConnectionLocationScores{}); err != nil {
		t.Fatalf("set connection location: %s", err)
	}
	model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
		model.ProvideModePublic: []byte("provide-secret"),
	})
}

// The same database state in both runs: two due providers and one observed pin.
func operatorEquivalenceSeed(t testing.TB, ctx context.Context) {
	city := &model.Location{
		LocationType: model.LocationTypeCity,
		City:         "Palo Alto",
		Region:       "California",
		Country:      "United States",
		CountryCode:  "us",
	}
	model.CreateLocation(ctx, city)
	operatorEquivalenceProvider(t, ctx, operatorEquivalenceNetworkA, operatorEquivalenceDeviceA, operatorEquivalenceClientA, city.LocationId, "192.0.2.1:0")
	operatorEquivalenceProvider(t, ctx, operatorEquivalenceNetworkB, operatorEquivalenceDeviceB, operatorEquivalenceClientB, city.LocationId, "192.0.2.2:0")
	model.UpdateClientLocationReliabilities(ctx, server.NowUtc().Add(-time.Hour), server.NowUtc())
	model.SetGeolocationSourcePin(ctx, &model.GeolocationSourcePin{
		Host:             "pins.example",
		LeafSpki:         "bGVhZi1zcGtp",
		IntermediateSpki: "aW50ZXJtZWRpYXRlLXNwa2k=",
		ObservedAt:       server.NowUtc(),
	})
}

// The production router, so a handler panic gets the router's own 500/503.
func operatorEquivalenceRouter(ctx context.Context) http.Handler {
	return router.NewRouter(ctx, []*router.Route{
		router.NewRoute("POST", "/network/provider-egress-location", ProviderEgressLocationSubmit),
		router.NewRoute("GET", "/network/provider-egress-due", ProviderEgressLocationDue),
		router.NewRoute("GET", "/network/provider-blackhole-due", ProviderBlackholeCheckDue),
		router.NewRoute("POST", "/network/provider-blackhole-checks", SubmitProviderBlackholeChecks),
		router.NewRoute("POST", "/network/provider-egress-attempt", ProviderEgressLocationAttempt),
		router.NewRoute("GET", "/network/provider-egress-destinations", ProviderEgressDestinations),
		router.NewRoute("GET", "/network/geolocation-source-pins", GeolocationSourcePins),
		router.NewRoute("POST", "/network/provider-bandwidth-result", ProviderBandwidthResult),
		router.NewRoute("POST", "/network/provider-bandwidth-reserve", ProviderBandwidthReserve),
		router.NewRoute("POST", "/network/provider-egress-health", ProviderEgressHealthResult),
	})
}

// One call of the scenario: what it returned and the error, by its text.
type operatorEquivalenceStep struct {
	Name   string `json:"name"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	// The sentinels a caller branches on, which must agree as well as the text.
	Rejected    bool `json:"rejected,omitempty"`
	Unsupported bool `json:"unsupported,omitempty"`
}

func operatorEquivalenceRecord(steps *[]operatorEquivalenceStep, name string, result any, err error) {
	step := operatorEquivalenceStep{Name: name, Result: result}
	if err != nil {
		step.Error = err.Error()
		step.Rejected = errors.Is(err, ingest.ErrRejected)
		step.Unsupported = errors.Is(err, qualityprobe.ErrUrlProbeCompletionUnsupported) ||
			errors.Is(err, ingest.ErrDueUnsupported) || errors.Is(err, ingest.ErrBlackholeUnsupported)
	}
	*steps = append(*steps, step)
}

// One accepted URL outcome for the claim's cycle.
func operatorEquivalenceUrlResult(runId server.Id, cycleStartedAt time.Time, measuredAt time.Time) *egresshealth.Result {
	destination := egresshealth.Destination{Name: "equivalence-news", Class: egresshealth.ClassSite, Url: "https://news.example/"}
	return &egresshealth.Result{
		RunId:          runId.String(),
		CycleStartedAt: cycleStartedAt,
		OkCount:        1,
		Total:          1,
		ByClass:        map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 1, Total: 1}},
		UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
			PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
			Destination: destination, MeasuredAt: measuredAt, ContentMatcherVersion: 1,
			Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: measuredAt, TlsAuthenticated: true}},
			StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
			RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10, BodyMillis: 1, BodyBitsPerSecond: 248000,
			ContentClassification: "content", PerformanceClassification: "insufficient_sample",
		},
	}
}

// Every operator call of a URL pass and a blackhole sweep, with the edge cases
// each transport must refuse identically. newOperator builds the transport for
// one shard geometry.
func operatorEquivalenceScenario(t testing.TB, ctx context.Context, newOperator func(shardIndex, shardCount int) ingest.Operator) []operatorEquivalenceStep {
	steps := []operatorEquivalenceStep{}
	operator := newOperator(0, 1)
	clientA, clientB := operatorEquivalenceClientA.String(), operatorEquivalenceClientB.String()
	unknown := operatorEquivalenceUnknown.String()

	// the per-pass catalogs
	pool, err := operator.DestinationPool(ctx)
	if pool != nil {
		// generation time is the server's clock at the request
		pool.GeneratedAt = time.Time{}
	}
	operatorEquivalenceRecord(&steps, "pool", pool, err)
	pins, err := operator.GeolocationPins(ctx)
	operatorEquivalenceRecord(&steps, "pins", pins, err)

	// the URL due queue and its client- and server-side refusals
	_, err = operator.Due(ctx, 0)
	operatorEquivalenceRecord(&steps, "due_zero_limit", nil, err)
	_, err = newOperator(2, 2).Due(ctx, 1)
	operatorEquivalenceRecord(&steps, "due_shard_out_of_range", nil, err)
	_, err = newOperator(0, 2048).Due(ctx, 1)
	operatorEquivalenceRecord(&steps, "due_shard_count_too_large", nil, err)
	due, err := operator.Due(ctx, 10)
	operatorEquivalenceRecord(&steps, "due", due, err)
	claims := map[string]ingest.DueProvider{}
	for _, provider := range due {
		claims[provider.ClientId] = provider
	}
	claimA, claimB := claims[clientA], claims[clientB]
	if claimA.ClaimOrdinal <= 0 || claimB.ClaimOrdinal <= 0 {
		t.Fatalf("the scenario needs both providers claimed: %+v", due)
	}

	// one accepted URL outcome, its replay, and every refusal of the health route
	now := time.Now().UTC()
	err = operator.SubmitEgressHealth(ctx, clientA, operatorEquivalenceUrlResult(operatorEquivalenceRun, claimA.CycleStartedAt, now))
	operatorEquivalenceRecord(&steps, "health", nil, err)
	err = operator.SubmitEgressHealth(ctx, clientA, operatorEquivalenceUrlResult(operatorEquivalenceRun, claimA.CycleStartedAt, now))
	operatorEquivalenceRecord(&steps, "health_replay", nil, err)
	err = operator.SubmitEgressHealth(ctx, clientA, nil)
	operatorEquivalenceRecord(&steps, "health_nil", nil, err)
	refusals := map[string]*egresshealth.Result{
		"ok_above_total": {RunId: operatorEquivalenceRunBad.String(), OkCount: 2, Total: 1,
			ByClass: map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 2, Total: 1}}},
		"classes_do_not_sum": {RunId: operatorEquivalenceRunBad.String(), OkCount: 1, Total: 2,
			ByClass: map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 1, Total: 1}}},
		"reputation_class": {RunId: operatorEquivalenceRunBad.String(), OkCount: 1, Total: 1,
			ByClass: map[egresshealth.Class]egresshealth.ClassSummary{"reputation": {Ok: 1, Total: 1}}},
	}
	future := operatorEquivalenceUrlResult(operatorEquivalenceRunBad, claimA.CycleStartedAt, now.Add(10*time.Minute))
	refusals["future_evidence"] = future
	disagreeing := operatorEquivalenceUrlResult(operatorEquivalenceRunBad, claimA.CycleStartedAt, now)
	disagreeing.UrlProbeEvidence.Security[0].TlsFailure = true
	disagreeing.UrlProbeEvidence.Security[0].TlsAuthenticated = false
	refusals["tls_disagrees"] = disagreeing
	scheduledTwo := operatorEquivalenceUrlResult(operatorEquivalenceRunBad, claimA.CycleStartedAt, now)
	scheduledTwo.OkCount, scheduledTwo.Total = 2, 2
	scheduledTwo.ByClass = map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 2, Total: 2}}
	refusals["scheduled_two_outcomes"] = scheduledTwo
	names := []string{}
	for name := range refusals {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		err = operator.SubmitEgressHealth(ctx, clientA, refusals[name])
		operatorEquivalenceRecord(&steps, "health_refused_"+name, nil, err)
	}
	err = operator.SubmitEgressHealth(ctx, "not-a-client-id", &egresshealth.Result{RunId: operatorEquivalenceRunBad.String()})
	operatorEquivalenceRecord(&steps, "health_bad_client_id", nil, err)

	// completion of both claims: success, replay, local failure, and refusals
	completion := qualityprobe.UrlProbeCompletion{ClientId: clientA, ClaimOrdinal: claimA.ClaimOrdinal, CompletedAt: time.Now().UTC(), AllowPacing: true}
	err = operator.ReportUrlProbeCompletion(ctx, completion)
	operatorEquivalenceRecord(&steps, "completion", nil, err)
	err = operator.ReportUrlProbeCompletion(ctx, completion)
	operatorEquivalenceRecord(&steps, "completion_replay", nil, err)
	err = operator.ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: clientA, ClaimOrdinal: claimA.ClaimOrdinal + 100, CompletedAt: time.Now().UTC()})
	operatorEquivalenceRecord(&steps, "completion_unknown_claim", nil, err)
	err = operator.ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: clientB, ClaimOrdinal: claimB.ClaimOrdinal, CompletedAt: time.Now().UTC().Add(time.Hour)})
	operatorEquivalenceRecord(&steps, "completion_outside_claim", nil, err)
	err = operator.ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: unknown, ClaimOrdinal: 1, CompletedAt: time.Now().UTC()})
	operatorEquivalenceRecord(&steps, "completion_unknown_client", nil, err)
	err = operator.ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: clientA, CompletedAt: time.Now().UTC()})
	operatorEquivalenceRecord(&steps, "completion_without_claim", nil, err)
	err = operator.ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{ClientId: "not-a-client-id", ClaimOrdinal: 1, CompletedAt: time.Now().UTC()})
	operatorEquivalenceRecord(&steps, "completion_bad_client_id", nil, err)
	err = operator.ReportUrlProbeCompletion(ctx, qualityprobe.UrlProbeCompletion{
		ClientId: clientB, ClaimOrdinal: claimB.ClaimOrdinal, CompletedAt: time.Now().UTC(),
		ProbeFailure: "tunnel_failed" + strings.Repeat("-and-a-very-long-tail", 8), AllowPacing: true,
	})
	operatorEquivalenceRecord(&steps, "completion_local_failure", nil, err)

	// the legacy attempt without a claim
	err = operator.ReportAttempt(ctx, clientB, "tunnel_failed")
	operatorEquivalenceRecord(&steps, "attempt", nil, err)
	err = operator.ReportAttempt(ctx, clientB, strings.Repeat("x", 100))
	operatorEquivalenceRecord(&steps, "attempt_long_failure", nil, err)
	err = operator.ReportAttempt(ctx, unknown, "tunnel_failed")
	operatorEquivalenceRecord(&steps, "attempt_unknown_client", nil, err)

	// the exit location
	err = operator.Submit(ctx, clientA, "not-an-address", time.Now().UTC())
	operatorEquivalenceRecord(&steps, "location_bad_address", nil, err)
	err = operator.Submit(ctx, clientA, "192.0.2.10", time.Time{})
	operatorEquivalenceRecord(&steps, "location_zero_time", nil, err)
	err = operator.Submit(ctx, clientA, "8.8.8.8", time.Now().UTC())
	operatorEquivalenceRecord(&steps, "location", nil, err)
	err = operator.Submit(ctx, unknown, "8.8.8.8", time.Now().UTC())
	operatorEquivalenceRecord(&steps, "location_unknown_client", nil, err)
	err = operator.Submit(ctx, clientA, "8.8.8.8", time.Now().UTC().Add(-48*time.Hour))
	operatorEquivalenceRecord(&steps, "location_too_old", nil, err)

	// the blackhole sweep
	_, err = operator.BlackholeDue(ctx, 0)
	operatorEquivalenceRecord(&steps, "blackhole_due_zero_limit", nil, err)
	blackholeDue, err := operator.BlackholeDue(ctx, 10)
	operatorEquivalenceRecord(&steps, "blackhole_due", blackholeDue, err)
	checkedAt := time.Now().UTC()
	err = operator.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{
		{ClientId: clientA, Ok: true, Failure: "ignored when ok", NotMeasured: true, CheckedAt: checkedAt},
		{ClientId: clientB, Failure: "all_destinations_failed" + strings.Repeat("!", 80), CheckedAt: checkedAt},
	})
	operatorEquivalenceRecord(&steps, "blackhole_checks", nil, err)
	err = operator.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{
		{ClientId: clientB, Failure: "not_measured", NotMeasured: true, CheckedAt: checkedAt.Add(time.Second)},
	})
	operatorEquivalenceRecord(&steps, "blackhole_checks_not_measured", nil, err)
	err = operator.SubmitBlackholeChecks(ctx, nil)
	operatorEquivalenceRecord(&steps, "blackhole_checks_empty", nil, err)
	err = operator.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{{ClientId: clientA, Ok: true}})
	operatorEquivalenceRecord(&steps, "blackhole_checks_zero_time", nil, err)
	err = operator.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{{ClientId: clientA, Failure: " ", CheckedAt: checkedAt}})
	operatorEquivalenceRecord(&steps, "blackhole_checks_unnamed_failure", nil, err)
	err = operator.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{{ClientId: clientA, Ok: true, CheckedAt: time.Now().UTC().Add(10 * time.Minute)}})
	operatorEquivalenceRecord(&steps, "blackhole_checks_future", nil, err)
	err = operator.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{{ClientId: server.Id{}.String(), Ok: true, CheckedAt: checkedAt}})
	operatorEquivalenceRecord(&steps, "blackhole_checks_zero_client", nil, err)
	err = operator.SubmitBlackholeChecks(ctx, []ingest.BlackholeCheck{{ClientId: "not-a-client-id", Ok: true, CheckedAt: checkedAt}})
	operatorEquivalenceRecord(&steps, "blackhole_checks_bad_client_id", nil, err)
	tooMany := make([]ingest.BlackholeCheck, maxProviderBlackholeChecksPerRequest+1)
	for i := range tooMany {
		tooMany[i] = ingest.BlackholeCheck{ClientId: clientA, Ok: true, CheckedAt: checkedAt}
	}
	err = operator.SubmitBlackholeChecks(ctx, tooMany)
	operatorEquivalenceRecord(&steps, "blackhole_checks_too_many", nil, err)

	// active bandwidth
	err = operator.ReserveBandwidth(ctx, clientA, 1024*1024)
	operatorEquivalenceRecord(&steps, "bandwidth_reserve", nil, err)
	err = operator.ReserveBandwidth(ctx, clientA, 0)
	operatorEquivalenceRecord(&steps, "bandwidth_reserve_zero", nil, err)
	err = operator.SubmitBandwidth(ctx, clientA, model.ProviderBandwidthSourceActiveOperator, 1e6, 1024*1024)
	operatorEquivalenceRecord(&steps, "bandwidth_submit", nil, err)
	err = operator.SubmitBandwidth(ctx, clientA, "passive", 1e6, 1024*1024)
	operatorEquivalenceRecord(&steps, "bandwidth_submit_passive", nil, err)

	// the queue after every acknowledgement
	after, err := operator.Due(ctx, 10)
	operatorEquivalenceRecord(&steps, "due_after", after, err)
	return steps
}

// Every row of every provider_* table that names the scenario's providers.
func operatorEquivalenceRows(t testing.TB, ctx context.Context) map[string][]string {
	rows := map[string][]string{}
	server.Db(ctx, func(conn server.PgConn) {
		tables := []string{}
		result, err := conn.Query(ctx, `
			SELECT table_name FROM information_schema.columns
			WHERE table_schema = 'public' AND column_name = 'client_id' AND table_name LIKE 'provider%'
			ORDER BY table_name`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var table string
				server.Raise(result.Scan(&table))
				tables = append(tables, table)
			}
		})
		ids := []string{operatorEquivalenceClientA.String(), operatorEquivalenceClientB.String(), operatorEquivalenceUnknown.String()}
		for _, table := range tables {
			result, err := conn.Query(ctx, fmt.Sprintf(`SELECT row_to_json(t)::text FROM %s t WHERE client_id::text = ANY($1)`, table), ids)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var row string
					server.Raise(result.Scan(&row))
					rows[table] = append(rows[table], row)
				}
			})
		}
	})
	return rows
}

var operatorEquivalenceUuid = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// A server-clock instant, as seconds from the run's start.
type operatorEquivalenceInstant struct {
	Offset float64
}

// Replaces ids and instants so two runs can be compared: fixed ids keep their
// labels, random ones read "<id>", and every timestamp becomes its offset from
// the run's start, which two runs must agree on within a tolerance.
func operatorEquivalenceNormalize(value any, start time.Time) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, item := range v {
			out[key] = operatorEquivalenceNormalize(item, start)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, operatorEquivalenceNormalize(item, start))
		}
		return out
	case string:
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
			if at, err := time.Parse(layout, v); err == nil && 4 < len(v) && strings.Contains(v, "T") {
				if at.IsZero() {
					return "<zero time>"
				}
				return operatorEquivalenceInstant{Offset: at.Sub(start).Seconds()}
			}
		}
		return operatorEquivalenceUuid.ReplaceAllStringFunc(v, func(id string) string {
			if label, ok := operatorEquivalenceLabels[strings.ToLower(id)]; ok {
				return label
			}
			return "<id>"
		})
	default:
		return v
	}
}

// The JSON form of a value, normalized.
func operatorEquivalenceJson(t testing.TB, value any, start time.Time) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %s", err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode: %s", err)
	}
	return operatorEquivalenceNormalize(decoded, start)
}

// Two normalized values are equal when their instants agree within tolerance.
func operatorEquivalenceEqual(left, right any, tolerance time.Duration) bool {
	switch l := left.(type) {
	case operatorEquivalenceInstant:
		r, ok := right.(operatorEquivalenceInstant)
		return ok && math.Abs(l.Offset-r.Offset) <= tolerance.Seconds()
	case map[string]any:
		r, ok := right.(map[string]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for key, item := range l {
			other, ok := r[key]
			if !ok || !operatorEquivalenceEqual(item, other, tolerance) {
				return false
			}
		}
		return true
	case []any:
		r, ok := right.([]any)
		if !ok || len(l) != len(r) {
			return false
		}
		for i := range l {
			if !operatorEquivalenceEqual(l[i], r[i], tolerance) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left, right)
	}
}

// Rows of one table, normalized and in an order that does not depend on clocks.
func operatorEquivalenceTableRows(t testing.TB, rows []string, start time.Time) []any {
	type keyed struct {
		key   string
		value any
	}
	out := []keyed{}
	for _, row := range rows {
		var decoded any
		if err := json.Unmarshal([]byte(row), &decoded); err != nil {
			t.Fatalf("decode row: %s", err)
		}
		if columns, ok := decoded.(map[string]any); ok {
			// A PostgreSQL transaction id is the shared cluster's counter, not
			// anything either transport chose.
			if _, ok := columns["transaction_id"]; ok {
				columns["transaction_id"] = "<xid>"
			}
			// An hourly budget bucket is a wall-clock hour: it must be the hour of
			// its own reservation, whenever the run happened.
			bucket, bucketOk := columns["bucket_start"].(string)
			created, createdOk := columns["create_time"].(string)
			if bucketOk && createdOk {
				bucketAt, bucketErr := time.Parse("2006-01-02T15:04:05.999999999", bucket)
				createdAt, createdErr := time.Parse("2006-01-02T15:04:05.999999999", created)
				if bucketErr == nil && createdErr == nil && bucketAt.Equal(createdAt.Truncate(time.Hour)) {
					columns["bucket_start"] = "<hour of create_time>"
				}
			}
		}
		normalized := operatorEquivalenceNormalize(decoded, start)
		key, _ := json.Marshal(operatorEquivalenceNormalize(decoded, time.Time{}))
		out = append(out, keyed{key: operatorEquivalenceUuid.ReplaceAllString(string(key), ""), value: normalized})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	values := make([]any, 0, len(out))
	for _, row := range out {
		values = append(values, row.value)
	}
	return values
}

type operatorEquivalenceOutcome struct {
	start time.Time
	steps []operatorEquivalenceStep
	rows  map[string][]string
}

// Runs the scenario in a fresh database through one transport.
func operatorEquivalenceExecute(t *testing.T, transport func(context.Context, testing.TB) (func(shardIndex, shardCount int) ingest.Operator, func())) operatorEquivalenceOutcome {
	var run operatorEquivalenceOutcome
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer server.Config.PushSimpleResource(model.ProviderEgressSitesResourceName, []byte(operatorEquivalenceCatalog))()
		operatorEquivalenceSeed(tb, ctx)
		newOperator, closeTransport := transport(ctx, tb)
		defer closeTransport()
		start := server.NowUtc()
		steps := operatorEquivalenceScenario(tb, ctx, newOperator)
		run = operatorEquivalenceOutcome{start: start, steps: steps, rows: operatorEquivalenceRows(tb, ctx)}
	})
	return run
}

// The same scenario through the operator API and through the in-process
// operator returns the same values and errors and leaves the same rows.
func TestOperatorIngestInProcessMatchesOperatorApi(t *testing.T) {
	t.Setenv("WARP_ENV", "local")
	var httpRequests atomic.Int64
	overHttp := operatorEquivalenceExecute(t, func(ctx context.Context, tb testing.TB) (func(int, int) ingest.Operator, func()) {
		restoreSecret := withStubOperatorIngestSecret(operatorEquivalenceSecret)
		routes := operatorEquivalenceRouter(ctx)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			httpRequests.Add(1)
			routes.ServeHTTP(w, r)
		}))
		return func(shardIndex, shardCount int) ingest.Operator {
				return &ingest.Client{ServerUrl: srv.URL, OperatorSecret: operatorEquivalenceSecret, Http: srv.Client(), ShardIndex: shardIndex, ShardCount: shardCount}
			}, func() {
				srv.Close()
				restoreSecret()
			}
	})
	if httpRequests.Load() == 0 {
		t.Fatal("the HTTP transport made no request; the comparison would be vacuous")
	}
	inProcess := operatorEquivalenceExecute(t, func(ctx context.Context, tb testing.TB) (func(int, int) ingest.Operator, func()) {
		// no listener and no secret: the in-process operator needs neither. It
		// runs with the taskworker's default bound of 8 calls at once.
		return func(shardIndex, shardCount int) ingest.Operator {
			operator, err := localoperator.New(shardIndex, shardCount, 30*time.Second, 8)
			if err != nil {
				tb.Fatal(err)
			}
			return operator
		}, func() {}
	})

	const tolerance = 10 * time.Second
	if len(overHttp.steps) != len(inProcess.steps) {
		t.Fatalf("the transports ran different scenarios: %d and %d steps", len(overHttp.steps), len(inProcess.steps))
	}
	for i := range overHttp.steps {
		httpStep := operatorEquivalenceJson(t, overHttp.steps[i], overHttp.start)
		localStep := operatorEquivalenceJson(t, inProcess.steps[i], inProcess.start)
		if !operatorEquivalenceEqual(httpStep, localStep, tolerance) {
			t.Errorf("step %s differs:\n http: %+v\nlocal: %+v", overHttp.steps[i].Name, httpStep, localStep)
		}
	}
	tables := map[string]bool{}
	for table := range overHttp.rows {
		tables[table] = true
	}
	for table := range inProcess.rows {
		tables[table] = true
	}
	if len(tables) == 0 {
		t.Fatal("the scenario left no provider rows; the comparison would be vacuous")
	}
	for table := range tables {
		httpRows := operatorEquivalenceTableRows(t, overHttp.rows[table], overHttp.start)
		localRows := operatorEquivalenceTableRows(t, inProcess.rows[table], inProcess.start)
		if !operatorEquivalenceEqual(any(httpRows), any(localRows), tolerance) {
			t.Errorf("table %s differs:\n http: %+v\nlocal: %+v", table, httpRows, localRows)
		}
	}
	for _, step := range overHttp.steps {
		t.Logf("%-36s error=%q", step.Name, step.Error)
	}
	for table := range tables {
		t.Logf("table %-40s rows=%d", table, len(overHttp.rows[table]))
	}
}
