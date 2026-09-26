package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/urnetwork/operator-proxy/egresshealth"
)

// Tests of the egress-health submission: the wire body, the TLS flag, the
// class decomposition and the status contract.

// A provider whose tunnel works, which two CDNs refuse,
// one site of which went unmeasured when its tunnel died and could not be
// re-created, and which loaded one canary from a place the site is marked
// incompatible with. OkCount/Total are over the measured, scored loads only
// (9/11): the unmeasured load and the canary are in neither.
func ingestHealthResult() *egresshealth.Result {
	return &egresshealth.Result{
		Checks: []egresshealth.CheckResult{
			{Name: "cloudflare-doh", Class: egresshealth.ClassDns, Ok: true},
			{Name: "google-doh", Class: egresshealth.ClassDns, Ok: true},
			{Name: "adguard-doh", Class: egresshealth.ClassDns, Ok: true},
			{Name: "google-generate-204", Class: egresshealth.ClassConnectivity, Ok: true},
			{Name: "cloudflare-cp-204", Class: egresshealth.ClassConnectivity, Ok: true},
			{Name: "cloudflare-cdn", Class: egresshealth.ClassCdn, Ok: true},
			{Name: "jsdelivr-fastly-mirror", Class: egresshealth.ClassCdn},
			{Name: "amazon-cloudfront", Class: egresshealth.ClassCdn},
			{Name: "wikipedia", Class: egresshealth.ClassSite, Ok: true},
			{Name: "github", Class: egresshealth.ClassSite, Ok: true},
			{Name: "naver", Class: egresshealth.ClassSite, Ok: true},
			{Name: "reddit", Class: egresshealth.ClassSite, NotMeasured: true},
			{Name: "etsy", Class: egresshealth.ClassSite, Canary: true, Ok: true},
		},
		OkCount: 9,
		Total:   11,
		ByClass: map[egresshealth.Class]egresshealth.ClassSummary{
			egresshealth.ClassDns:          {Ok: 3, Total: 3},
			egresshealth.ClassConnectivity: {Ok: 2, Total: 2},
			egresshealth.ClassCdn:          {Ok: 1, Total: 3},
			egresshealth.ClassSite:         {Ok: 3, Total: 3},
		},
		NotMeasured:  1,
		ShortClasses: []egresshealth.Class{egresshealth.ClassDns, egresshealth.ClassSite},
		TableTotal:   139,
	}
}

// Pins the wire contract against
// the server's handlers.SubmitProviderEgressHealthArgs.
//
// The server rejects unknown fields and requires class_results to sum to
// exactly ok_count/total_count, so a field-name drift on either side turns
// every submission into a 400 -- and because the prober submits
// fire-and-forget with dedup-logged errors, the visible symptom is one log
// line followed by permanent silence while nothing is stored. Hence the
// assertion on the fully decoded body rather than on a couple of fields.
func TestSubmitEgressHealthSendsAWellFormedBody(t *testing.T) {
	var gotMethod, gotPath, gotSecret, gotContentType string
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotSecret = r.Header.Get("X-UR-Operator-Secret")
		gotContentType = r.Header.Get("Content-Type")
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
	if err := client.SubmitEgressHealth(context.Background(), "provider-1", ingestHealthResult()); err != nil {
		t.Fatalf("SubmitEgressHealth: %s", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/network/provider-egress-health" {
		t.Errorf("path = %s, want /network/provider-egress-health", gotPath)
	}
	if gotSecret != "s3cret" {
		t.Errorf("operator secret header = %q", gotSecret)
	}
	if gotContentType != "application/json" {
		t.Errorf("content-type = %q", gotContentType)
	}

	var got submitEgressHealthBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal body: %s (raw = %s)", err, raw)
	}
	want := submitEgressHealthBody{
		ClientId:   "provider-1",
		OkCount:    9,
		TotalCount: 11,
		ClassResults: map[string]egressHealthClassBody{
			"dns":          {Ok: 3, Total: 3},
			"connectivity": {Ok: 2, Total: 2},
			"cdn":          {Ok: 1, Total: 3},
			"site":         {Ok: 3, Total: 3},
		},
		// the dissolved reputation class: on the wire for one release, zero
		ReputationOk:          0,
		ReputationTotal:       0,
		ReputationFailedNames: "",
		// measured failures only: the unmeasured load and the canary are not
		// failed sites
		FailedNames:       "jsdelivr-fastly-mirror,amazon-cloudfront",
		NotMeasuredCount:  1,
		NotMeasuredNames:  "reddit",
		CanaryPassedNames: "etsy",
		CanaryFailedNames: "",
		ShortClasses:      "dns,site",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body =\n%+v\nwant\n%+v", got, want)
	}

	// the server rejects unknown fields, so the wire form must carry exactly
	// the keys it declares -- decode into a bare map to catch an extra one that
	// the typed decode above would silently ignore
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatalf("unmarshal body as a map: %s", err)
	}
	wantKeys := map[string]bool{
		"client_id":                  true,
		"ok_count":                   true,
		"total_count":                true,
		"class_results":              true,
		"reputation_ok":              true,
		"reputation_total":           true,
		"failed_names":               true,
		"reputation_failed_names":    true,
		"tls_authentication_failure": true,
		"not_measured_count":         true,
		"not_measured_names":         true,
		"canary_passed_names":        true,
		"canary_failed_names":        true,
		"short_classes":              true,
	}
	for k := range keys {
		if !wantKeys[k] {
			t.Errorf("body carries %q, which the server does not declare and will reject", k)
		}
	}
	for k := range wantKeys {
		if _, present := keys[k]; !present {
			t.Errorf("body is missing %q", k)
		}
	}
}

// A TLS identity failure is a hard signal, not one failed request diluted into
// ok_count/total_count. Pin the wire field that lets the server enforce that
// distinction independently of the broad health-score rollout switch.
func TestSubmitEgressHealthSendsTlsAuthenticationFailure(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := ingestHealthResult()
	result.TlsAuthenticationFailure = true
	client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
	if err := client.SubmitEgressHealth(context.Background(), "provider-1", result); err != nil {
		t.Fatalf("SubmitEgressHealth: %s", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal body: %s", err)
	}
	if value, ok := got["tls_authentication_failure"].(bool); !ok || !value {
		t.Fatalf("tls_authentication_failure = %#v, want true", got["tls_authentication_failure"])
	}
}

// The server checks the
// class map sums to exactly ok_count/total_count, which is what makes a
// smuggled-in class impossible to hide -- and a "reputation" class, which the
// server rejects outright, must never appear.
func TestSubmitEgressHealthClassResultsDecomposeTheScore(t *testing.T) {
	var raw []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
	if err := client.SubmitEgressHealth(context.Background(), "provider-1", ingestHealthResult()); err != nil {
		t.Fatalf("SubmitEgressHealth: %s", err)
	}

	var got submitEgressHealthBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal body: %s", err)
	}
	if _, present := got.ClassResults["reputation"]; present {
		t.Fatal("reputation was submitted as a class; the server rejects that outright")
	}
	sumOk, sumTotal := 0, 0
	for _, c := range got.ClassResults {
		sumOk += c.Ok
		sumTotal += c.Total
	}
	if sumOk != got.OkCount || sumTotal != got.TotalCount {
		t.Fatalf("class_results sum to %d/%d, want %d/%d", sumOk, sumTotal, got.OkCount, got.TotalCount)
	}
}

// Pins the status contract. The 404
// case is the one that matters: a server that has not shipped the endpoint
// must be a clean skip (egresshealth.ErrUnsupported), not a failure, so a
// prober pointed at an older deployment keeps probing.
func TestSubmitEgressHealthMapsStatusToOutcome(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{name: "stored", status: http.StatusOK, body: `{}`},
		{name: "server has no endpoint", status: http.StatusNotFound, wantErr: egresshealth.ErrUnsupported},
		{name: "wrong secret", status: http.StatusUnauthorized, wantErr: ErrUnauthorized},
		{name: "rejected payload", status: http.StatusBadRequest, body: "ok_count must not exceed total_count.", wantErr: ErrRejected},
		{name: "server error", status: http.StatusInternalServerError, body: "boom", wantErr: ErrRejected},
	}

	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		}))
		client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
		err := client.SubmitEgressHealth(context.Background(), "provider-1", ingestHealthResult())
		srv.Close()
		if c.wantErr == nil {
			if err != nil {
				t.Errorf("%s: err = %v, want nil", c.name, err)
			}
			continue
		}
		if !errors.Is(err, c.wantErr) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.wantErr)
		}
	}
}

// The prober only submits after a run
// that produced a result, but a nil here must never become a submitted zero --
// that is indistinguishable from a total blackhole and would be a false
// accusation against a provider whose check simply did not run.
func TestSubmitEgressHealthIgnoresANilResult(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
	if err := client.SubmitEgressHealth(context.Background(), "provider-1", nil); err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if called {
		t.Fatal("a nil result was submitted as a zero score")
	}
}
