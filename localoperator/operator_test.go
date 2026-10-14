package localoperator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
)

// The in-process operator hands the controller what the handler's JSON decoder
// would have made of the same call, and returns what ingest.Client makes of
// the handler's answer. These tests hold both halves to the real encoders and
// decoders, without a database.

// An id parses in-process exactly when server.Id's JSON decoding accepts it,
// and to the same value.
func TestWireIdMatchesServerIdJson(t *testing.T) {
	valid := server.NewId().String()
	for _, value := range []string{
		valid,
		strings.ToUpper(valid),
		server.Id{}.String(),
		"",
		"not-a-client-id",
		strings.ReplaceAll(valid, "-", ""),
		"{" + valid + "}",
		"urn:uuid:" + valid,
		" " + valid,
		valid + " ",
		valid[:35] + "g",
		strings.Repeat("-", 36),
	} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded server.Id
		decodeErr := json.Unmarshal(encoded, &decoded)
		parsed, parseErr := wireId(value)
		if (decodeErr == nil) != (parseErr == nil) || (decodeErr == nil && decoded != parsed) {
			t.Fatalf("%q: JSON decoding err=%v id=%s, in-process err=%v id=%s", value, decodeErr, decoded, parseErr, parsed)
		}
		if parseErr != nil && rejected(parseErr).Error() != ingest.RejectedError(http.StatusBadRequest, "Bad request").Error() {
			t.Fatalf("%q: an undecodable id is not the handler's 400: %v", value, rejected(parseErr))
		}
	}
}

// Decodes body into out the way the strict operator handlers do.
func strictDecode(t *testing.T, body any, out any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		t.Fatalf("the handler would refuse this body: %v", err)
	}
}

func testUrlResult(measuredAt time.Time) *egresshealth.Result {
	destination := egresshealth.Destination{Name: "synthetic-news", Class: egresshealth.ClassSite, Url: "https://news.example/",
		Headers: map[string]string{"Accept": "text/html"}, Incompatible: []egresshealth.Place{{Country: "zz"}}}
	return &egresshealth.Result{
		RunId:          server.NewId().String(),
		CycleStartedAt: measuredAt.Add(-time.Hour),
		OkCount:        1,
		Total:          1,
		NotMeasured:    2,
		ByClass:        map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassSite: {Ok: 1, Total: 1}},
		ShortClasses:   []egresshealth.Class{egresshealth.ClassCdn, egresshealth.ClassDns},
		Checks: []egresshealth.CheckResult{
			{Name: "synthetic-news", Class: egresshealth.ClassSite, Ok: true, Attempts: 1},
			{Name: "unmeasured-" + strings.Repeat("x", 600), Class: egresshealth.ClassCdn, NotMeasured: true},
			{Name: "canary-site", Class: egresshealth.ClassSite, Canary: true},
		},
		UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
			PolicyVersion: egresshealth.UrlProbePolicyVersion, Policy: egresshealth.DefaultUrlProbePolicy(),
			Destination: destination, MeasuredAt: measuredAt, ContentMatcherVersion: 1,
			Security:   []egresshealth.UrlProbeSecurityEvent{{Destination: destination, MeasuredAt: measuredAt, TlsAuthenticated: true}},
			StatusCode: 200, ByteCount: 32, WireByteCount: 32, WireSampleByteCount: 31, BodyComplete: true,
			RequestWritten: true, FirstByteReceived: true, TtfbMillis: 10.25, BodyMillis: 1.5, BodyBitsPerSecond: 248000.125,
			ContentClassification: "content", PerformanceClassification: "insufficient_sample",
		},
	}
}

// The controller receives, field for field, what the health handler's strict
// decoder makes of the HTTP body: the same truncated name lists, the same
// evidence, the same tallies, and nil classes stay nil.
func TestEgressHealthArgsMatchTheStrictHandlerDecode(t *testing.T) {
	// A whole-second UTC instant has the same representation before and after
	// a JSON round trip, so reflect.DeepEqual can compare the two sides.
	measuredAt := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	for name, res := range map[string]*egresshealth.Result{
		"url_outcome":      testUrlResult(measuredAt),
		"aggregate":        {RunId: server.NewId().String(), OkCount: 3, Total: 5, ByClass: map[egresshealth.Class]egresshealth.ClassSummary{egresshealth.ClassDns: {Ok: 1, Total: 2}, egresshealth.ClassSite: {Ok: 2, Total: 3}}, TlsAuthenticationFailure: true},
		"nothing_measured": {},
	} {
		t.Run(name, func(t *testing.T) {
			submission := ingest.NewEgressHealthSubmission(server.NewId().String(), res)
			var decoded controller.SubmitProviderEgressHealthArgs
			strictDecode(t, submission, &decoded)
			args, err := EgressHealthArgsOf(submission)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*args, decoded) {
				t.Fatalf("in-process arguments differ from the handler's decode:\n in-process: %+v\n    decoded: %+v", *args, decoded)
			}
		})
	}
}

// The same for a blackhole batch, including the client-side cleanup of a
// passing check and an over-long failure class.
func TestBlackholeChecksArgsMatchTheStrictHandlerDecode(t *testing.T) {
	checkedAt := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	submission, err := ingest.NewBlackholeChecksSubmission([]ingest.BlackholeCheck{
		{ClientId: server.NewId().String(), Ok: true, Failure: "ignored", NotMeasured: true, CheckedAt: checkedAt},
		{ClientId: server.NewId().String(), Failure: strings.Repeat("f", 90), CheckedAt: checkedAt},
		{ClientId: server.NewId().String(), Failure: "not_measured", NotMeasured: true, CheckedAt: checkedAt.Add(time.Second)},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded controller.SubmitProviderBlackholeChecksArgs
	strictDecode(t, submission, &decoded)
	args, err := BlackholeChecksArgsOf(submission)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(*args, decoded) {
		t.Fatalf("in-process batch differs from the handler's decode:\n in-process: %+v\n    decoded: %+v", *args, decoded)
	}
}

// A claimed due list reaches the pass exactly as ingest.Client decodes it from
// the handler's JSON, network and security destinations included.
func TestDueProvidersMatchTheClientDecode(t *testing.T) {
	networkId := server.NewId()
	completed := int64(7)
	claimedAt := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	providers := []model.ProviderUrlProbeDue{
		{ClientId: server.NewId(), CountryCode: "us", Region: "California", CycleStartedAt: claimedAt.Add(-time.Hour),
			RunsNeeded: 10, SuccessesNeeded: 10, OutcomeCount: 3, ClaimOrdinal: 41, ClaimedAt: claimedAt,
			CompletedRunCount: &completed, NetworkId: &networkId,
			SecurityDestinations: []egresshealth.Destination{{Name: "recheck", Class: egresshealth.ClassSite, Url: "https://recheck.example/"}}},
		{ClientId: server.NewId(), ClaimOrdinal: 42, ClaimedAt: claimedAt, SecurityDestinations: []egresshealth.Destination{}},
		{ClientId: server.NewId(), ClaimOrdinal: 43},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(model.ProviderUrlProbeDueResult{Providers: providers})
	}))
	defer srv.Close()
	client := &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "synthetic", Http: srv.Client()}
	decoded, err := client.Due(context.Background(), len(providers))
	if err != nil {
		t.Fatal(err)
	}
	if got := DueProvidersOf(providers); !reflect.DeepEqual(got, decoded) {
		t.Fatalf("in-process due list differs from the client's decode:\n in-process: %+v\n    decoded: %+v", got, decoded)
	}
	// independent of the model's slices: a pass may keep the list
	got := DueProvidersOf(providers)
	*got[0].CompletedRunCount = 0
	if completed != 7 {
		t.Fatal("the due list aliases the model's completed-run count")
	}
}

// A synthetic model claim for the Due path.
func testOperator(claim controller.ProviderUrlProbeDueClaim, timing *controller.ProviderUrlProbeDueTiming) *Operator {
	return &Operator{ShardIndex: 0, ShardCount: 1, CallTimeout: time.Minute, claimDue: claim, dueTiming: timing}
}

// The Due path observes its claim into the API's URL Due series, returns the
// maintenance sentinel the client returns, and maps the router's answers to a
// panicking handler -- 503 for a done error with a live caller, 500 otherwise
// -- while a caller that gave up gets its own context error.
func TestDueMapsOutcomesLikeTheOperatorApi(t *testing.T) {
	timing := controller.NewProviderUrlProbeDueTiming(prometheus.NewPedanticRegistry())
	claimId := server.NewId()
	ok := testOperator(func(ctx context.Context, now time.Time, limit, shardIndex, shardCount int, observation *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		if now.Location() != time.UTC || limit != 5 || shardIndex != 0 || shardCount != 1 {
			t.Errorf("claim arguments changed: %s %d %d/%d", now.Location(), limit, shardIndex, shardCount)
		}
		observation.Phases[model.ProviderUrlProbeDueExpiry] = server.DbTimingSample{Count: 1, Duration: time.Millisecond}
		return model.ProviderUrlProbeDueResult{Providers: []model.ProviderUrlProbeDue{{ClientId: claimId, ClaimOrdinal: 1}}}
	}, timing)
	due, err := ok.Due(context.Background(), 5)
	if err != nil || len(due) != 1 || due[0].ClientId != claimId.String() {
		t.Fatalf("due: %+v %v", due, err)
	}
	if testutil.ToFloat64(timing.Requests.WithLabelValues("returned")) != 1 || testutil.ToFloat64(timing.Observations.WithLabelValues("expiry")) != 1 ||
		testutil.ToFloat64(timing.Observations.WithLabelValues("admitted_handler")) != 1 || testutil.ToFloat64(timing.Observations.WithLabelValues("encode")) != 0 {
		t.Fatal("the in-process claim was not observed as one request without an encoding")
	}

	maintenance := testOperator(func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		return model.ProviderUrlProbeDueResult{Providers: []model.ProviderUrlProbeDue{}, PriorityMaintenancePending: true}
	}, timing)
	if _, err := maintenance.Due(context.Background(), 5); !errors.Is(err, ingest.ErrDuePriorityMaintenancePending) {
		t.Fatalf("maintenance page: %v", err)
	}
	if testutil.ToFloat64(timing.Requests.WithLabelValues("maintenance")) != 1 {
		t.Fatal("maintenance outcome missing")
	}

	for name, test := range map[string]struct {
		panicWith any
		status    int
	}{
		"done_error":    {fmt.Errorf("claim: %w", server.DbContextDoneError), http.StatusServiceUnavailable},
		"model_failure": {errors.New("synthetic model failure"), http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			failing := testOperator(func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
				panic(test.panicWith)
			}, timing)
			_, err := failing.Due(context.Background(), 5)
			if !errors.Is(err, ingest.ErrRejected) || !strings.Contains(err.Error(), fmt.Sprintf("status %d", test.status)) {
				t.Fatalf("a panicking claim is not the router's %d: %v", test.status, err)
			}
		})
	}
	if testutil.ToFloat64(timing.Requests.WithLabelValues("panic")) != 2 {
		t.Fatal("panicking claims were not observed")
	}

	ctx, cancel := context.WithCancel(context.Background())
	abandoned := testOperator(func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		cancel()
		panic(fmt.Errorf("claim: %w", context.Canceled))
	}, timing)
	if _, err := abandoned.Due(ctx, 5); !errors.Is(err, context.Canceled) || errors.Is(err, ingest.ErrRejected) {
		t.Fatalf("an abandoned caller did not get its own cancellation: %v", err)
	}

	bounded := testOperator(func(ctx context.Context, _ time.Time, _, _, _ int, _ *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		<-ctx.Done()
		panic(ctx.Err())
	}, timing)
	bounded.CallTimeout = 10 * time.Millisecond
	if _, err := bounded.Due(context.Background(), 5); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the per-call bound did not end a stalled claim: %v", err)
	}
}

// Both transports refuse the same malformed due requests before any claim,
// with the same errors.
func TestDueClientSideRefusalsMatchTheClient(t *testing.T) {
	claimed := false
	operator := testOperator(func(context.Context, time.Time, int, int, int, *model.ProviderUrlProbeDueObservation) model.ProviderUrlProbeDueResult {
		claimed = true
		return model.ProviderUrlProbeDueResult{}
	}, nil)
	client := &ingest.Client{ServerUrl: "http://127.0.0.1:1", OperatorSecret: "synthetic"}
	for _, test := range []struct{ limit, shardIndex, shardCount int }{{0, 0, 1}, {-1, 0, 1}, {1, 2, 2}, {1, -1, 4}} {
		operator.ShardIndex, operator.ShardCount = test.shardIndex, test.shardCount
		client.ShardIndex, client.ShardCount = test.shardIndex, test.shardCount
		_, localErr := operator.Due(context.Background(), test.limit)
		_, httpErr := client.Due(context.Background(), test.limit)
		if localErr == nil || httpErr == nil || localErr.Error() != httpErr.Error() {
			t.Fatalf("%+v: in-process %v, HTTP %v", test, localErr, httpErr)
		}
		_, localErr = operator.BlackholeDue(context.Background(), test.limit)
		_, httpErr = client.BlackholeDue(context.Background(), test.limit)
		if localErr == nil || httpErr == nil || localErr.Error() != httpErr.Error() {
			t.Fatalf("blackhole %+v: in-process %v, HTTP %v", test, localErr, httpErr)
		}
	}
	if claimed {
		t.Fatal("a refused request reached the claim")
	}
}

// A refusal reads exactly as the client reads the handler's http.Error.
func TestRefusalsReadAsTheClientReadsTheHandler(t *testing.T) {
	refusal := refusalOf(fmt.Errorf("wrapped: %w", &controller.ProviderEgressIngestRefusal{Status: http.StatusBadRequest, Message: "class \"site\": ok must not exceed total."}))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "class \"site\": ok must not exceed total.", http.StatusBadRequest)
	}))
	defer srv.Close()
	client := &ingest.Client{ServerUrl: srv.URL, OperatorSecret: "synthetic", Http: srv.Client()}
	httpErr := client.ReportAttempt(context.Background(), server.NewId().String(), "")
	if rejected(refusal).Error() != httpErr.Error() || !errors.Is(rejected(refusal), ingest.ErrRejected) {
		t.Fatalf("in-process %q, HTTP %q", rejected(refusal), httpErr)
	}
	// a plain controller error is the 400 the attempt and location handlers answer
	if got := rejected(refusalOf(errors.New("Unknown client."))).Error(); got != ingest.RejectedError(http.StatusBadRequest, "Unknown client.").Error() {
		t.Fatalf("plain controller error: %q", got)
	}
}
