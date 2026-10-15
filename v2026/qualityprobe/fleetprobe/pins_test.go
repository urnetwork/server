package fleetprobe

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

// Tests of the pass inputs: pin validation, the pool fallback, the derived
// operator urls, and blackhole submissions.

// Any complete pin the server serves is kept, no host is
// required to have one (a host without a pin is verified by WebPKI), an empty
// set is valid, and half a pin makes the whole set an error.
func TestValidatePins(t *testing.T) {
	pins, err := ValidatePins(map[string]ingest.GeolocationPin{
		"api.operator.example": {Leaf: "leaf", Intermediate: "int"},
		"site.example":         {Leaf: "leaf2", Intermediate: "int2"},
	})
	if err != nil || len(pins) != 2 || pins["api.operator.example"][0] != "leaf" || pins["api.operator.example"][1] != "int" {
		t.Fatalf("ValidatePins = %v, %v", pins, err)
	}
	for _, served := range []map[string]ingest.GeolocationPin{nil, {}} {
		if pins, err := ValidatePins(served); err != nil || pins == nil || len(pins) != 0 {
			t.Errorf("ValidatePins(%v) = %v, %v; an empty set is valid now", served, pins, err)
		}
	}
	for _, half := range []ingest.GeolocationPin{{Leaf: "leaf"}, {Intermediate: "int"}, {}} {
		pins, err := ValidatePins(map[string]ingest.GeolocationPin{"a.example": {Leaf: "l", Intermediate: "i"}, "b.example": half})
		if err == nil || pins != nil || !strings.Contains(err.Error(), "b.example") {
			t.Errorf("half pin %+v: pins = %v err = %v, want an error naming the host", half, pins, err)
		}
	}
}

// A failed catalog fetch returns a local error and no replacement targets.
func TestLoadPoolRefusesUnavailableCatalog(t *testing.T) {
	served := &egresshealth.Pool{Version: 12,
		Destinations: []egresshealth.Destination{{Name: "synthetic-site", Class: egresshealth.ClassSite, Url: "https://site.example/"}},
		Profile:      egresshealth.DefaultRequestProfile()}
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != egresshealth.PoolPath || r.Header.Get("X-UR-Operator-Secret") != "s3cret" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(served)
	}))
	defer good.Close()

	pool, err := LoadPool(context.Background(), good.Client(), PoolUrl(good.URL+"/"), "s3cret")
	if err != nil || pool == nil || pool.Version != 12 {
		t.Fatalf("LoadPool = %v, %v; want the server's pool", pool, err)
	}

	for name, url := range map[string]string{
		"wrong secret": PoolUrl(good.URL),
		"unreachable":  "http://127.0.0.1:1" + egresshealth.PoolPath,
	} {
		secret := "s3cret"
		if name == "wrong secret" {
			secret = "wrong"
		}
		pool, err := LoadPool(context.Background(), good.Client(), url, secret)
		if !errors.Is(err, egresshealth.ErrPoolUnavailable) {
			t.Errorf("%s: err = %v, want ErrPoolUnavailable", name, err)
		}
		if pool != nil {
			t.Errorf("%s: invented fallback catalog %+v", name, pool)
		}
	}
}

// The pool and the echo are derived from the api url the
// same way whatever its trailing slash, and an empty api url has no echo.
func TestOperatorUrls(t *testing.T) {
	if got := PoolUrl("https://api.example/"); got != "https://api.example/network/provider-egress-destinations" {
		t.Errorf("PoolUrl = %q", got)
	}
	if got := IpEchoUrl("https://api.example/"); got != "https://api.example/my-ip-info" {
		t.Errorf("IpEchoUrl = %q", got)
	}
	if got := IpEchoUrl("  "); got != "" {
		t.Errorf("IpEchoUrl of nothing = %q, want empty", got)
	}
}

// A missing source does not invent configured URLs.
func TestPoolSourceNeverInventsCatalog(t *testing.T) {
	var none PoolSource
	if got := none.pool(); len(got.Destinations) != 0 {
		t.Error("a nil pool source invented destinations")
	}
	empty := PoolSource(func() *egresshealth.Pool { return nil })
	if got := empty.pool(); len(got.Destinations) != 0 {
		t.Error("a nil catalog invented destinations")
	}
}

// A check's outcome becomes its submission -- a pass,
// a failure that counts towards dark, and a not-measured check that is never
// dark and says so on the wire.
func TestBlackholeResultOf(t *testing.T) {
	at := time.Unix(1, 0).UTC()
	ok := blackholeResultOf("p", at, &egresshealth.BlackholeResult{Ok: true}, 0)
	if !ok.Check.Ok || ok.Dark || ok.NotMeasured || ok.Check.Failure != "" {
		t.Errorf("pass = %+v", ok)
	}
	dark := blackholeResultOf("p", at, &egresshealth.BlackholeResult{Failure: egresshealth.FailureAllDestinationsFailed}, 0)
	if dark.Check.Ok || !dark.Dark || dark.NotMeasured || dark.Check.NotMeasured {
		t.Errorf("failure = %+v", dark)
	}
	unmeasured := blackholeResultOf("p", at, &egresshealth.BlackholeResult{Failure: egresshealth.FailureNotMeasured, NotMeasured: 3}, 2)
	if unmeasured.Dark || !unmeasured.NotMeasured || !unmeasured.Check.NotMeasured || unmeasured.Check.Failure != egresshealth.FailureNotMeasured {
		t.Errorf("not measured = %+v, want never dark and marked on the wire", unmeasured)
	}
	if !strings.Contains(unmeasured.Details, "tunnel_recreated=2") {
		t.Errorf("details = %q, want the re-creations named", unmeasured.Details)
	}
	tls := blackholeResultOf("p", at, &egresshealth.BlackholeResult{Failure: egresshealth.FailureTlsAuthentication}, 0)
	if !tls.Dark || tls.NotMeasured {
		t.Errorf("tls = %+v, want the hard failure it has always been", tls)
	}
	staged := blackholeResultOf("synthetic-provider", at, &egresshealth.BlackholeResult{
		Failure: egresshealth.FailureAllDestinationsFailed,
		Results: []egresshealth.CheckResult{{Name: "synthetic-check", Err: "context deadline exceeded", FailureStage: "request_response_timeout", Attempts: 3}},
	}, 0)
	if !strings.Contains(staged.Details, "failure_stages=request_response_timeout:1") {
		t.Errorf("blackhole details omit sanitized failure stage: %q", staged.Details)
	}
}
