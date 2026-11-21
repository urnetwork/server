package sidecart3

// T3Api's service origins (scenario_sidecar.go, service_origins.go): a served service's host reaches its handler as
// the lb routes it, one nobody serves answers as api does, a name outside ServiceNames is refused, and an answer
// streams: RoundTrip answers once the handler's status is fixed, its body reads what the handler writes as it writes
// it, and a client that closes the body early ends the handler's request. Every wait is on a channel the handler or
// the client closes, bounded in real time only to fail a broken run. No store is touched: api's router answers a path
// it does not route before any.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
)

// A context for one test's waits, which only a broken run outlasts.
func serviceOriginsTestContext(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(t.Context(), 30*time.Second)
}

// T3Api on a T2 Net's capture, which these tests scan nothing of.
func newServiceOriginsTestApi(t *testing.T) *T3Api {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), nil)
	t.Cleanup(net.Close)
	return NewT3Api(t.Context(), net, nil)
}

// One request through T3Api's transport, as a sidecar's service client sends it.
func roundTripServiceOrigin(t *testing.T, ctx context.Context, t3Api *T3Api, method string, url string, body string) (*http.Response, error) {
	t.Helper()
	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer a-service-token")
	return t3Api.Transport().RoundTrip(request)
}

// The answer's status and whole body.
func readServiceAnswer(t *testing.T, response *http.Response) (int, string) {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

func TestT3ApiServesAServiceAtItsOrigin(t *testing.T) {
	t3Api := newServiceOriginsTestApi(t)
	type seen struct {
		method        string
		host          string
		uri           string
		remoteAddr    string
		authorization string
		body          string
	}
	seenRequests := make(chan *seen, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenRequests <- &seen{method: r.Method, host: r.Host, uri: r.RequestURI, remoteAddr: r.RemoteAddr, authorization: r.Header.Get("Authorization"), body: string(body)}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, "served by evs")
	})
	if err := t3Api.ServeService("evs", handler); err != nil {
		t.Fatal(err)
	}
	if t3Api.ServiceOrigin("evs") != "https://evs.sidecart3.example" {
		t.Fatalf("evs's origin is %s", t3Api.ServiceOrigin("evs"))
	}

	response, err := roundTripServiceOrigin(t, t.Context(), t3Api, http.MethodPut, "https://EVS.sidecart3.example/evs/v1/repos/x?fresh=1", "a body")
	if err != nil {
		t.Fatal(err)
	}
	if status, body := readServiceAnswer(t, response); status != http.StatusCreated || body != "served by evs" || response.Header.Get("Content-Type") != "text/plain" {
		t.Fatalf("evs's origin answered %d %q", status, body)
	}
	request := <-seenRequests
	want := &seen{
		method: http.MethodPut, host: "EVS.sidecart3.example", uri: "/evs/v1/repos/x?fresh=1", remoteAddr: t3Api.Settings().RemoteAddr,
		authorization: "Bearer a-service-token", body: "a body",
	}
	if *request != *want {
		t.Fatalf("evs's handler saw %+v, not %+v", request, want)
	}

	// a service nobody serves answers as api does: server's router routes none of its paths
	response, err = roundTripServiceOrigin(t, t.Context(), t3Api, http.MethodGet, t3Api.ServiceOrigin("gkm")+"/gkm/v1/groups", "")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := readServiceAnswer(t, response); status != http.StatusNotFound {
		t.Fatalf("gkm's origin, served by nobody, answered %d", status)
	}

	// a misspelt service and a second handler are refused; stopping evs gives its origin back to api
	if err := t3Api.ServeService("evz", handler); err == nil {
		t.Fatal("a service outside ServiceNames was served")
	}
	if err := t3Api.ServeService("evs", handler); err == nil {
		t.Fatal("a second handler took evs's origin")
	}
	if err := t3Api.ServeService("evs", nil); err != nil {
		t.Fatal(err)
	}
	response, err = roundTripServiceOrigin(t, t.Context(), t3Api, http.MethodGet, t3Api.ServiceOrigin("evs")+"/evs/v1/repos/x", "")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := readServiceAnswer(t, response); status != http.StatusNotFound {
		t.Fatalf("evs's origin, no longer served, answered %d", status)
	}
	select {
	case request := <-seenRequests:
		t.Fatalf("a stopped service's handler saw %+v", request)
	default:
	}
}

func TestT3ApiStreamsAServiceAnswer(t *testing.T) {
	t3Api := newServiceOriginsTestApi(t)
	proceed := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "first ")
		w.(http.Flusher).Flush()
		select {
		case <-proceed:
		case <-r.Context().Done():
			return
		}
		io.WriteString(w, "second")
	})
	if err := t3Api.ServeService("sockets", handler); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := serviceOriginsTestContext(t)
	defer cancel()
	// answered while the handler still waits on proceed: the answer did not wait for its end
	response, err := roundTripServiceOrigin(t, ctx, t3Api, http.MethodGet, t3Api.ServiceOrigin("sockets")+"/socket/registrations", "")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	first := make([]byte, len("first "))
	if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first " {
		t.Fatalf("the stream's first part read %q: %v", first, err)
	}
	close(proceed)
	rest, err := io.ReadAll(response.Body)
	if err != nil || string(rest) != "second" {
		t.Fatalf("the stream's rest read %q: %v", rest, err)
	}
}

func TestT3ApiEndsAServiceRequestItsClientLeft(t *testing.T) {
	t3Api := newServiceOriginsTestApi(t)
	ended := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "waiting")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		ended <- r.Context().Err()
	})
	if err := t3Api.ServeService("mcp", handler); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := serviceOriginsTestContext(t)
	defer cancel()
	response, err := roundTripServiceOrigin(t, ctx, t3Api, http.MethodPost, t3Api.ServiceOrigin("mcp")+"/mcp", "{}")
	if err != nil {
		t.Fatal(err)
	}
	head := make([]byte, len("waiting"))
	if _, err := io.ReadFull(response.Body, head); err != nil {
		t.Fatal(err)
	}
	// the client leaves while the handler waits on its request: the request ends, as a closed connection ends it
	response.Body.Close()
	select {
	case err := <-ended:
		if err == nil {
			t.Fatal("the handler's request ended with no error")
		}
	case <-ctx.Done():
		t.Fatal("the handler's request did not end once its client closed the answer")
	}
}

func TestT3ApiFailsAServiceThatPanicsBeforeItsAnswer(t *testing.T) {
	t3Api := newServiceOriginsTestApi(t)
	if err := t3Api.ServeService("dns", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("a broken handler")
	})); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := serviceOriginsTestContext(t)
	defer cancel()
	response, err := roundTripServiceOrigin(t, ctx, t3Api, http.MethodGet, t3Api.ServiceOrigin("dns")+"/dns/v1/zone", "")
	if err == nil {
		response.Body.Close()
		t.Fatalf("a handler that panicked before its answer answered %d", response.StatusCode)
	}
	if !strings.Contains(err.Error(), "a broken handler") {
		t.Fatalf("the panic's error does not name it: %v", err)
	}
}
