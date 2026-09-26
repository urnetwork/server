package ingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Tests of the blackhole submission's wire marks and of the operator-secret
// header the pool fetch shares.

// A check whose tunnel never came
// back measured nothing, and goes to the server as exactly that --
// not_measured, which the server stores without counting a failure -- not as
// a failed check on its way to dark. A passing check never carries the mark.
func TestSubmitBlackholeChecksSendsNotMeasured(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	checkedAt := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s", Http: srv.Client()}
	err := c.SubmitBlackholeChecks(context.Background(), []BlackholeCheck{
		{ClientId: "not-measured", Failure: egresshealth.FailureNotMeasured, NotMeasured: true, CheckedAt: checkedAt},
		{ClientId: "dark", Failure: egresshealth.FailureAllDestinationsFailed, CheckedAt: checkedAt},
		{ClientId: "ok", Ok: true, NotMeasured: true, CheckedAt: checkedAt},
	})
	if err != nil {
		t.Fatalf("SubmitBlackholeChecks: %v", err)
	}

	var body struct {
		Checks []map[string]any `json:"checks"`
	}
	if err := json.Unmarshal(raw, &body); err != nil || len(body.Checks) != 3 {
		t.Fatalf("body = %s (%v)", raw, err)
	}
	if got := body.Checks[0]; got["not_measured"] != true || got["ok"] != false || got["failure"] != "not_measured" {
		t.Errorf("not-measured check = %v, want not_measured true with failure not_measured", got)
	}
	if _, present := body.Checks[1]["not_measured"]; present {
		t.Errorf("a measured failed check carries not_measured: %v", body.Checks[1])
	}
	if _, present := body.Checks[2]["not_measured"]; present {
		t.Errorf("a passing check carries not_measured: %v", body.Checks[2])
	}
}

// Holds egresshealth's
// spelling of the operator-secret header to this package's: the pool is
// fetched under the same header, with the same secret, as every ingest call
// (egresshealth cannot import ingest to share the constant).
func TestFetchPoolSendsTheIngestOperatorSecretHeader(t *testing.T) {
	seen := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Clone()
		if r.URL.Path == egresshealth.PoolPath {
			_ = json.NewEncoder(w).Encode(egresshealth.BuiltinPool())
			return
		}
		_, _ = w.Write([]byte(`{"client_ids":[]}`))
	}))
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
	if _, err := c.Due(context.Background(), 1); err != nil {
		t.Fatalf("Due: %v", err)
	}
	if _, err := egresshealth.FetchPool(context.Background(), srv.Client(), srv.URL+egresshealth.PoolPath, "s3cret"); err != nil {
		t.Fatalf("FetchPool: %v", err)
	}
	ingestHeader, poolHeader := seen["/network/provider-egress-due"], seen[egresshealth.PoolPath]
	for name, values := range ingestHeader {
		if len(values) == 1 && values[0] == "s3cret" {
			if poolHeader.Get(name) != "s3cret" {
				t.Fatalf("ingest sends the secret as %s, FetchPool does not (%v)", name, poolHeader)
			}
			return
		}
	}
	t.Fatalf("ingest sent no header carrying the secret: %v", ingestHeader)
}
