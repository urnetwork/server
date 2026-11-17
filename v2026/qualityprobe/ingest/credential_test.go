package ingest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// credentialServer mirrors pinServer: it serves one status and one body at the
// credential path, 404s everything else, and records the operator secret it
// was sent.
func credentialServer(t *testing.T, status int, body string, gotSecret *string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network/prober-credential" {
			http.NotFound(w, r)
			return
		}
		if calls != nil {
			*calls++
		}
		if gotSecret != nil {
			*gotSecret = r.Header.Get("X-UR-Operator-Secret")
		}
		if r.Method != http.MethodGet {
			t.Errorf("credential request method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// TestProberCredentialDecodesTheServedCredential is the happy path, and it is
// also what holds the WIRE FIELD NAME. The body here is written from the
// server's documented result shape, not from the Go struct: by_client_jwt and
// client_id. A struct tag derived from the local name instead (-by-jwt /
// UR_PROBER_BY_JWT, so "by_jwt" is the natural typo) would decode this body
// into an empty string, which is why the assertion is on the value rather than
// only on err == nil.
func TestProberCredentialDecodesTheServedCredential(t *testing.T) {
	var secret string
	srv := credentialServer(t, http.StatusOK,
		`{"by_client_jwt":"eyJhbGciOiJIUzI1NiJ9.payload.sig","client_id":"019f8835-158d-6fd8-e9dd-fd0e4c6d6792"}`,
		&secret, nil)
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
	cred, err := c.ProberCredential(context.Background())
	if err != nil {
		t.Fatalf("ProberCredential err = %v", err)
	}
	if secret != "s3cret" {
		t.Errorf("operator secret header = %q, want the configured secret; this endpoint authenticates the same way as the other operator endpoints", secret)
	}
	if cred.ByClientJwt != "eyJhbGciOiJIUzI1NiJ9.payload.sig" {
		t.Errorf("ByClientJwt = %q, want the served by_client_jwt", cred.ByClientJwt)
	}
	if cred.ClientId != "019f8835-158d-6fd8-e9dd-fd0e4c6d6792" {
		t.Errorf("ClientId = %q, want the served client_id", cred.ClientId)
	}
}

// TestProberCredentialTrailingSlashServerURL: the other methods all build
// their url with strings.TrimRight(ServerUrl, "/"), and an -api-url written
// with a trailing slash is an ordinary way to configure a deployment.
func TestProberCredentialTrailingSlashServerURL(t *testing.T) {
	srv := credentialServer(t, http.StatusOK, `{"by_client_jwt":"j","client_id":"c"}`, nil, nil)
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL + "/", OperatorSecret: "s3cret"}
	if _, err := c.ProberCredential(context.Background()); err != nil {
		t.Fatalf("ProberCredential err = %v; a trailing slash on ServerUrl must not produce a double slash the server 404s", err)
	}
}

// TestProberCredentialNotReadyOn404 is the requirement that keeps a prober
// started before the server's 6h bootstrap task alive.
//
// The assertion that matters is not just "an error": it is that the error is
// ErrCredentialNotReady and NOTHING ELSE. A caller distinguishes three
// outcomes by errors.Is, so if 404 also matched the retryable sentinel or the
// unauthorized one, a correctly-written caller could still take the wrong
// branch. pins_test.go asserts the same kind of non-overlap for the same
// reason.
func TestProberCredentialNotReadyOn404(t *testing.T) {
	srv := credentialServer(t, http.StatusNotFound, "", nil, nil)
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
	cred, err := c.ProberCredential(context.Background())
	if cred != nil {
		t.Errorf("ProberCredential returned a credential %+v alongside the 404", cred)
	}
	if !errors.Is(err, ErrCredentialNotReady) {
		t.Fatalf("err = %v, want ErrCredentialNotReady so the prober waits for the bootstrap task instead of exiting", err)
	}
	if errors.Is(err, ErrUnauthorized) {
		t.Error("the 404 also matches ErrUnauthorized; a not-yet-bootstrapped server would be reported as a misconfigured secret and the prober would exit instead of waiting")
	}
	if errors.Is(err, ErrCredentialUnavailable) {
		t.Error("the 404 also matches ErrCredentialUnavailable; \"not ready\" would be logged as a fetch failure, hiding the one message that means a real fault")
	}
	// The two "older server, carry on without it" sentinels. There is no
	// carrying on without a jwt, so matching either would be a fail-open path.
	if errors.Is(err, ErrDueUnsupported) || errors.Is(err, ErrAttemptUnsupported) {
		t.Error("the 404 matches a sentinel that means \"degrade and continue\"; there is no degraded mode without a jwt")
	}
}

// TestProberCredentialUnauthorizedOn401: a wrong operator secret must be loud.
// It must NOT match the retryable sentinel, or a caller that retries on
// ErrCredentialUnavailable would sit re-asking a server that will never say
// yes -- the silent misconfiguration this error exists to prevent.
func TestProberCredentialUnauthorizedOn401(t *testing.T) {
	srv := credentialServer(t, http.StatusUnauthorized, "", nil, nil)
	defer srv.Close()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "wrong"}
	cred, err := c.ProberCredential(context.Background())
	if cred != nil {
		t.Errorf("ProberCredential returned a credential %+v alongside the 401", cred)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized so the operator is told which secret to check", err)
	}
	if errors.Is(err, ErrCredentialNotReady) {
		t.Error("the 401 also matches ErrCredentialNotReady; a wrong secret would be polled forever as though the bootstrap task had not run")
	}
	if errors.Is(err, ErrCredentialUnavailable) {
		t.Error("the 401 also matches ErrCredentialUnavailable, which the caller retries; a wrong secret must never be retried forever")
	}
}

// TestProberCredentialRejectsAnUnusableBody covers every 200 that is not a
// usable credential.
//
// The `{"by_jwt":...}` case is the one worth naming: it is what a wrong struct
// tag looks like from the wire side, it decodes without error, and without the
// emptiness check it would be returned as a successful fetch of "". The prober
// would then fail far away from here, at parseByJwtClientId or at the tunnel,
// with an error that says nothing about the real cause.
func TestProberCredentialRejectsAnUnusableBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "not json", body: `{"by_client_jwt":`},
		{name: "not an object", body: `["nope"]`},
		// The one case the emptiness check cannot backstop, and so the only
		// one that actually holds the decode-error branch: encoding/json
		// reports a type mismatch but still populates the fields it could, so
		// a usable-looking jwt arrives alongside the error. Ignoring the error
		// here would return a plausible credential from a body the server did
		// not mean to send.
		{name: "client_id is not a string", body: `{"by_client_jwt":"a.b.c","client_id":5}`},
		{name: "null", body: `null`},
		{name: "empty object", body: `{}`},
		{name: "empty jwt", body: `{"by_client_jwt":"","client_id":"c"}`},
		{name: "blank jwt", body: `{"by_client_jwt":"   ","client_id":"c"}`},
		{name: "wrong field name (by_jwt)", body: `{"by_jwt":"a.b.c","client_id":"c"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := credentialServer(t, http.StatusOK, tc.body, nil, nil)
			defer srv.Close()

			c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
			cred, err := c.ProberCredential(context.Background())
			if err == nil {
				t.Fatalf("body %s returned credential %+v and no error; a 200 that carries no usable jwt must not read as a successful fetch", tc.body, cred)
			}
			if cred != nil {
				t.Errorf("body %s returned both an error and a credential %+v", tc.body, cred)
			}
			if !errors.Is(err, ErrCredentialUnavailable) {
				t.Errorf("body %s: err = %v, want it to wrap ErrCredentialUnavailable so the caller retries", tc.body, err)
			}
			if errors.Is(err, ErrCredentialNotReady) {
				t.Errorf("body %s: err matches ErrCredentialNotReady, but the server answered 200; a broken body is not \"not yet\"", tc.body)
			}
		})
	}
}

// TestProberCredentialRetryableOnServerErrors: everything that is not 200, 404
// or 401 is a transient server fault the prober should keep asking through.
func TestProberCredentialRetryableOnServerErrors(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusTooManyRequests,
		http.StatusForbidden,
		http.StatusNoContent,
	} {
		srv := credentialServer(t, status, "upstream is having a moment", nil, nil)
		c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
		_, err := c.ProberCredential(context.Background())
		srv.Close()

		if !errors.Is(err, ErrCredentialUnavailable) {
			t.Errorf("status %d: err = %v, want ErrCredentialUnavailable", status, err)
		}
		if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrCredentialNotReady) {
			t.Errorf("status %d: err = %v matches a sentinel with different handling", status, err)
		}
	}
}

// An unreachable server is retryable too: the prober may well start before the
// api does.
func TestProberCredentialFailsOnAnUnreachableServer(t *testing.T) {
	c := &Client{ServerUrl: "http://127.0.0.1:1", OperatorSecret: "s3cret"}
	cred, err := c.ProberCredential(context.Background())
	if err == nil {
		t.Fatalf("an unreachable server returned credential %+v and no error", cred)
	}
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Errorf("err = %v, want ErrCredentialUnavailable", err)
	}
}

// TestProberCredentialPreservesTheTransportCause: the poll loop built on this
// can sit for hours, so an interrupt landing mid-request is ordinary. Wrapped
// with %s the cancellation would be invisible to errors.Is and a SIGTERM
// during the wait would be reported as a broken deployment.
func TestProberCredentialPreservesTheTransportCause(t *testing.T) {
	srv := credentialServer(t, http.StatusOK, `{"by_client_jwt":"j","client_id":"c"}`, nil, nil)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	c := &Client{ServerUrl: srv.URL, OperatorSecret: "s3cret"}
	_, err := c.ProberCredential(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want it to wrap context.Canceled so an interrupted wait is distinguishable from an unreachable server", err)
	}
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Errorf("err = %v, want it to also carry ErrCredentialUnavailable", err)
	}
}
