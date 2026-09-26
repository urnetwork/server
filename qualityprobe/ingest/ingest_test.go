package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Tests of the location submission: the wire shape, local refusals and the
// status contract.

// Locks down the wire shape of submitBody against
// the server's contract (controller.SubmitProviderEgressLocationArgs): the
// exit address the /ip echo saw and when, with the vendor-consensus fields
// present for one release and empty, and country_confident false. A field
// renamed or dropped here is a server-side zero value that fails with no
// error anywhere, so every key is asserted.
func TestSubmitPostsTheExitAddress(t *testing.T) {
	var got map[string]any
	var gotSecret, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSecret = r.Header.Get("X-UR-Operator-Secret")
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		_, _ = w.Write([]byte(`{"location_id":"019f0000-0000-0000-0000-000000000000"}`))
	}))
	defer srv.Close()

	observedAt := time.Date(2026, 9, 23, 12, 34, 56, 0, time.UTC)
	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret", Http: srv.Client()}
	if err := c.Submit(context.Background(), "00000000-0000-0000-0000-000000000001", " 2001:DB8::7 ", observedAt); err != nil {
		t.Fatalf("Submit err = %v", err)
	}
	if gotSecret != "s3cret" || gotPath != "/network/provider-egress-location" {
		t.Fatalf("secret = %q path = %q", gotSecret, gotPath)
	}
	if got["client_id"] != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("client_id = %v", got["client_id"])
	}
	// Canonical form: the server stores nothing of the address but its
	// place, and a stable spelling keeps the two ends from disagreeing.
	if got["exit_ip"] != "2001:db8::7" {
		t.Errorf("exit_ip = %v, want the canonical 2001:db8::7", got["exit_ip"])
	}
	if got["country_confident"] != false {
		t.Errorf("country_confident = %v, want false: the prober no longer places the exit", got["country_confident"])
	}
	// The consensus fields ride along empty for one release: the two with no
	// omitempty are present and blank, the rest absent.
	for _, key := range []string{"country_code", "country"} {
		if value, present := got[key]; !present || value != "" {
			t.Errorf("%s = %v (present %t), want present and empty for one release", key, value, present)
		}
	}
	for _, key := range []string{"region", "city", "asn", "org", "hosting", "proxy", "mobile", "city_confident"} {
		if value, present := got[key]; present {
			t.Errorf("%s = %v; the prober no longer fills it", key, value)
		}
	}
	gotObservedAt, ok := got["observed_at"].(string)
	if !ok {
		t.Fatalf("observed_at = %v, want an RFC 3339 string", got["observed_at"])
	}
	if parsed, err := time.Parse(time.RFC3339, gotObservedAt); err != nil || !parsed.Equal(observedAt) {
		t.Fatalf("observed_at = %q (%v), want %v", gotObservedAt, err, observedAt)
	}
	if len(got) != 6 {
		t.Errorf("body carries %d keys (%v), want exactly client_id, exit_ip, country_code, country, country_confident and observed_at", len(got), got)
	}
}

// The address is the whole submission,
// so a missing or unparseable one -- and a zero observation time, which would
// defeat the server's age check -- is refused before any request.
func TestSubmitRefusesWithoutAnExitAddress(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()
	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s", Http: srv.Client()}

	for _, exitIp := range []string{"", "   ", "not-an-address", "203.0.113.7:443", "<html>"} {
		if err := c.Submit(context.Background(), "p", exitIp, time.Now()); !errors.Is(err, ErrMissingExitIp) {
			t.Errorf("exit %q: err = %v, want ErrMissingExitIp", exitIp, err)
		}
	}
	if err := c.Submit(context.Background(), "p", "203.0.113.7", time.Time{}); !errors.Is(err, ErrMissingProbedAt) {
		t.Errorf("zero observation time: err = %v, want ErrMissingProbedAt", err)
	}
	if called {
		t.Fatal("a doomed submission reached the server")
	}
}

// A 400 from the server surfaces as ErrRejected.
func TestSubmitSurfacesRejection(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unknown client.", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s", Http: srv.Client()}
	if err := c.Submit(context.Background(), "p", "203.0.113.7", time.Now().UTC()); !errors.Is(err, ErrRejected) {
		t.Fatalf("a 400 surfaced as %v, want ErrRejected", err)
	}
}

// Every other method on this client maps
// 401, and the CLI keys its remediation advice ("check -operator-secret
// against ingest_secret") off the sentinel. Submit was the one that did not,
// and the gap is reachable: against a server without the due endpoint the
// prober falls back to enumeration, which authenticates with the byJwt, so a
// wrong operator secret let the whole pass proceed and surfaced only as a
// per-provider "status 401" classified submit_failed.
func TestSubmitMaps401ToErrUnauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "wrong", Http: srv.Client()}
	if err := c.Submit(context.Background(), "p", "203.0.113.7", time.Now().UTC()); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want it to wrap ErrUnauthorized", err)
	}
}
