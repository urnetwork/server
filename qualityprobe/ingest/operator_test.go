package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// The builders every transport shares hold the client-side rules the HTTP
// client always applied: these pin them where an in-process operator can see
// them.

func TestPrepareDueRequestSendsTheServersGeometry(t *testing.T) {
	for _, test := range []struct {
		limit, shardIndex, shardCount int
		wantIndex, wantCount          int
		wantErr                       string
	}{
		{limit: 1, shardIndex: 0, shardCount: 0, wantIndex: 0, wantCount: 1},
		{limit: 1, shardIndex: 7, shardCount: 1, wantIndex: 0, wantCount: 1},
		{limit: 5, shardIndex: 3, shardCount: 8, wantIndex: 3, wantCount: 8},
		{limit: 0, shardIndex: 0, shardCount: 1, wantErr: "ingest: due limit must be positive (got 0)"},
		{limit: 1, shardIndex: 8, shardCount: 8, wantErr: "ingest: shard index 8 is out of range for shard count 8"},
		{limit: 1, shardIndex: -1, shardCount: 2, wantErr: "ingest: shard index -1 is out of range for shard count 2"},
	} {
		index, count, err := PrepareDueRequest("due", test.limit, test.shardIndex, test.shardCount)
		if test.wantErr != "" {
			if err == nil || err.Error() != test.wantErr {
				t.Fatalf("%+v: err = %v, want %q", test, err, test.wantErr)
			}
			continue
		}
		if err != nil || index != test.wantIndex || count != test.wantCount {
			t.Fatalf("%+v: got %d/%d err=%v", test, index, count, err)
		}
	}
}

// What the client sends for an omitted geometry is what the server reads.
func TestDueOmitsTheGeometryPrepareDueRequestReadsAsWhole(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"providers":[]}`))
	}))
	defer srv.Close()
	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s", Http: srv.Client(), ShardIndex: 5, ShardCount: 1}
	if _, err := c.Due(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if query != "limit=3" {
		t.Fatalf("query = %q, want the shard parameters omitted", query)
	}
}

func TestRejectedErrorIsTheClientsReadingOfAResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unknown client.", http.StatusBadRequest)
	}))
	defer srv.Close()
	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s", Http: srv.Client()}
	err := c.ReportAttempt(context.Background(), "provider", "")
	want := RejectedError(http.StatusBadRequest, "Unknown client.\n")
	if !errors.Is(err, ErrRejected) || err.Error() != want.Error() || want.Error() != "ingest: server rejected the submission: status 400: Unknown client." {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestNewAttemptReportCutsTheFailureClass(t *testing.T) {
	report := NewAttemptReport("provider", strings.Repeat("é", MaxProbeFailureLen))
	if len(report.ProbeFailure) > MaxProbeFailureLen || !strings.HasPrefix(strings.Repeat("é", MaxProbeFailureLen), report.ProbeFailure) {
		t.Fatalf("failure class = %q", report.ProbeFailure)
	}
}

func TestNewEgressHealthSubmissionKeepsTheRunIdentity(t *testing.T) {
	if NewEgressHealthSubmission("provider", nil) != nil {
		t.Fatal("a nil run became a submission")
	}
	res := &egresshealth.Result{}
	first := NewEgressHealthSubmission("provider", res)
	second := NewEgressHealthSubmission("provider", res)
	if first.RunId == "" || first.RunId != second.RunId || res.RunId != first.RunId {
		t.Fatal("a run without an id did not keep the one it was given across retries")
	}
	if first.ClassResults == nil {
		t.Fatal("the class map is nil; the server decodes {} into an empty map")
	}
}

func TestPrepareUrlProbeCompletionAndReceipt(t *testing.T) {
	completedAt := time.Now().UTC()
	if _, err := PrepareUrlProbeCompletion(qualityprobe.UrlProbeCompletion{ClaimOrdinal: 0, CompletedAt: completedAt}); err == nil {
		t.Fatal("a completion without a claim was prepared")
	}
	if _, err := PrepareUrlProbeCompletion(qualityprobe.UrlProbeCompletion{ClaimOrdinal: 1}); err == nil {
		t.Fatal("a completion without a time was prepared")
	}
	completion, err := PrepareUrlProbeCompletion(qualityprobe.UrlProbeCompletion{ClaimOrdinal: 3, CompletedAt: completedAt, ProbeFailure: strings.Repeat("x", 100)})
	if err != nil || len(completion.ProbeFailure) != MaxProbeFailureLen {
		t.Fatalf("completion = %+v err = %v", completion, err)
	}
	if err := CheckUrlProbeCompletionReceipt(completion, UrlProbeCompletionReceipt{ClaimOrdinal: 3, AttemptAt: completedAt, ReceivedAt: completedAt}); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []UrlProbeCompletionReceipt{
		{ClaimOrdinal: 4, AttemptAt: completedAt, ReceivedAt: completedAt},
		{ClaimOrdinal: 3, ReceivedAt: completedAt},
		{ClaimOrdinal: 3, AttemptAt: completedAt},
	} {
		if err := CheckUrlProbeCompletionReceipt(completion, receipt); !errors.Is(err, qualityprobe.ErrUrlProbeCompletionUnsupported) {
			t.Fatalf("receipt %+v certified the completion: %v", receipt, err)
		}
	}
}

// The HTTP operator fetches the pool where the taskworker always fetched it,
// under the same secret, and refuses an unusable answer as ErrPoolUnavailable.
func TestClientDestinationPoolFetchesTheOperatorsCatalog(t *testing.T) {
	pool := egresshealth.Pool{Version: 9, Destinations: []egresshealth.Destination{
		{Name: "synthetic-site", Class: egresshealth.ClassSite, Url: "https://site.example/"},
	}}
	var path, secret string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, secret = r.URL.Path, r.Header.Get("X-UR-Operator-Secret")
		_ = json.NewEncoder(w).Encode(pool)
	}))
	defer srv.Close()
	c := &Client{ServerUrl: srv.URL + "/", OperatorSecret: "s3cret", Http: srv.Client()}
	got, err := c.DestinationPool(context.Background())
	if err != nil || got.Version != 9 || len(got.Destinations) != 1 {
		t.Fatalf("pool = %+v err = %v", got, err)
	}
	if path != egresshealth.PoolPath || secret != "s3cret" {
		t.Fatalf("fetched %q with secret %q", path, secret)
	}
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer refusing.Close()
	c = &Client{ServerUrl: refusing.URL, OperatorSecret: "s3cret", Http: refusing.Client()}
	if _, err := c.DestinationPool(context.Background()); !errors.Is(err, egresshealth.ErrPoolUnavailable) {
		t.Fatalf("a refused pool fetch: %v", err)
	}
}
