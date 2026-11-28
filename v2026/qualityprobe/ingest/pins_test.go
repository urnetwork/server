package ingest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func pinServer(t *testing.T, status int, body string, gotSecret *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotSecret != nil {
			*gotSecret = r.Header.Get("X-UR-Operator-Secret")
		}
		if r.URL.Path != "/network/geolocation-source-pins" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestGeolocationPinsDecodesTheServedSet(t *testing.T) {
	var secret string
	srv := pinServer(t, http.StatusOK,
		`{"ipinfo.io":{"leaf":"leaf-ipinfo","intermediate":"int-ipinfo"},"api.i.pn":{"leaf":"leaf-ipn","intermediate":"int-ipn"}}`,
		&secret)
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
	pins, err := c.GeolocationPins(context.Background())
	if err != nil {
		t.Fatalf("GeolocationPins err = %v", err)
	}
	if secret != "s3cret" {
		t.Errorf("operator secret header = %q, want the configured secret", secret)
	}
	if len(pins) != 2 {
		t.Fatalf("pins = %v, want 2 hosts", pins)
	}
	if pins["ipinfo.io"].Leaf != "leaf-ipinfo" || pins["ipinfo.io"].Intermediate != "int-ipinfo" {
		t.Errorf("ipinfo.io = %+v, want the served leaf and intermediate", pins["ipinfo.io"])
	}
}

// TestGeolocationPinsFailsClosedOnEveryNon200 is the one that matters.
//
// Due and ReportAttempt map 404 to a sentinel the caller treats as "this server
// is older, carry on without it". Copying that shape here -- the obvious thing
// to do when following the existing client's pattern -- would build a fail-OPEN
// path straight into the mechanism whose entire purpose is to fail closed: the
// prober would go on probing through provider tunnels with no pins, producing
// location data that looks fine and can be forged by the provider under test.
//
// So every status other than 200 is an error, and none of them is
// distinguishable by errors.Is from anything the prober is allowed to continue
// past.
func TestGeolocationPinsFailsClosedOnEveryNon200(t *testing.T) {
	for _, status := range []int{
		http.StatusNotFound,
		http.StatusUnauthorized,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusNoContent,
	} {
		srv := pinServer(t, status, `{"ipinfo.io":{"leaf":"l","intermediate":"i"}}`, nil)
		c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
		pins, err := c.GeolocationPins(context.Background())
		srv.Close()

		if err == nil {
			t.Errorf("status %d returned pins %v and no error; every non-200 must fail closed", status, pins)
			continue
		}
		if pins != nil {
			t.Errorf("status %d returned both an error and pins %v", status, pins)
		}
		if !errors.Is(err, ErrPinsUnavailable) {
			t.Errorf("status %d error = %v, want it to wrap ErrPinsUnavailable", status, err)
		}
		if errors.Is(err, ErrDueUnsupported) || errors.Is(err, ErrAttemptUnsupported) {
			t.Errorf("status %d error = %v, which matches a sentinel the prober is allowed to continue past", status, err)
		}
	}
}

// A 401 is worth naming in the message -- a wrong operator secret is a
// different thing for an operator to fix than a 500 -- but it is still fatal,
// which is what the test above already asserts.
func TestGeolocationPinsNames401(t *testing.T) {
	srv := pinServer(t, http.StatusUnauthorized, "", nil)
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "wrong"}
	_, err := c.GeolocationPins(context.Background())
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want it to wrap ErrUnauthorized so the operator is told which secret to check", err)
	}
}

// An unreachable server is the startup case the prober must not probe through.
func TestGeolocationPinsFailsOnAnUnreachableServer(t *testing.T) {
	c := &Client{ServerUrl: "http://127.0.0.1:1", OperatorSecret: "s3cret"}
	pins, err := c.GeolocationPins(context.Background())
	if err == nil {
		t.Fatalf("an unreachable server returned pins %v and no error", pins)
	}
	if !errors.Is(err, ErrPinsUnavailable) {
		t.Errorf("err = %v, want it to wrap ErrPinsUnavailable", err)
	}
}

// TestGeolocationPinsPreservesTheTransportCause: the transport error is
// wrapped with %w so a caller triaging a shutdown can still tell a cancelled
// startup from a server that is genuinely unreachable. Wrapped with %s the
// cause was flattened to text and errors.Is stopped working, while the 401
// path beside it kept its sentinel.
func TestGeolocationPinsPreservesTheTransportCause(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Client{ServerUrl: "http://127.0.0.1:1", OperatorSecret: "s3cret"}
	_, err := c.GeolocationPins(ctx)
	if !errors.Is(err, ErrPinsUnavailable) {
		t.Fatalf("err = %v, want it to wrap ErrPinsUnavailable", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to also wrap context.Canceled", err)
	}
}

// A body of `null` decodes into a nil map with no error. Returning that as a
// successful fetch would hand the caller an empty set that looks fetched, so
// the check belongs here rather than in every caller.
func TestGeolocationPinsRejectsANullBody(t *testing.T) {
	for _, body := range []string{"null", ""} {
		srv := pinServer(t, http.StatusOK, body, nil)
		c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
		pins, err := c.GeolocationPins(context.Background())
		srv.Close()
		if err == nil {
			t.Errorf("body %q returned pins %v and no error", body, pins)
		}
	}
}
