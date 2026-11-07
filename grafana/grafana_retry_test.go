package grafana

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Records the retry waits instead of sleeping, for the length of one test.
func recordGrafanaRetryWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	waits := &[]time.Duration{}
	var mutex sync.Mutex
	previous := grafanaRetrySleep
	grafanaRetrySleep = func(ctx context.Context, wait time.Duration) error {
		mutex.Lock()
		defer mutex.Unlock()
		*waits = append(*waits, wait)
		return ctx.Err()
	}
	t.Cleanup(func() { grafanaRetrySleep = previous })
	return waits
}

// A server that answers the given statuses in order, then 200 "ok", and
// records each request's method and body.
type grafanaRetryServer struct {
	mutex      sync.Mutex
	statuses   []int
	retryAfter string
	requests   []string
}

func (self *grafanaRetryServer) serve(response http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	self.mutex.Lock()
	self.requests = append(self.requests, request.Method+" "+string(body))
	status := http.StatusOK
	if 0 < len(self.statuses) {
		status, self.statuses = self.statuses[0], self.statuses[1:]
	}
	self.mutex.Unlock()
	if status != http.StatusOK && self.retryAfter != "" {
		response.Header().Set("Retry-After", self.retryAfter)
	}
	response.WriteHeader(status)
	if status == http.StatusOK {
		_, _ = response.Write([]byte("ok"))
	}
}

func (self *grafanaRetryServer) send(t *testing.T, method string, body []byte) (int, []byte, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(self.serve))
	t.Cleanup(server.Close)
	return grafanaSend(t.Context(), server.Client(), method, server.URL+"/api/folders/urnetwork", "synthetic-admin", "synthetic-password", body, nil, 1024)
}

// The load balancer's 429 never reached Grafana: retried, with the backoff
// doubling from 1 s, until Grafana answers.
func TestGrafanaSendRetriesTheLoadBalancers429(t *testing.T) {
	waits := recordGrafanaRetryWaits(t)
	fake := &grafanaRetryServer{statuses: []int{429, 429}}
	status, body, err := fake.send(t, http.MethodGet, nil)
	if err != nil || status != http.StatusOK || string(body) != "ok" {
		t.Fatalf("got %d %q %v, want 200 ok", status, body, err)
	}
	if len(fake.requests) != 3 {
		t.Fatalf("requests = %v, want 3", fake.requests)
	}
	if !slices.Equal(*waits, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("waits = %v, want [1s 2s]", *waits)
	}
}

// Retry-After in whole seconds sets the wait, capped at grafanaRetryMaxWait.
func TestGrafanaSendFollowsRetryAfter(t *testing.T) {
	for retryAfter, want := range map[string]time.Duration{"3": 3 * time.Second, "120": grafanaRetryMaxWait, "soon": time.Second} {
		waits := recordGrafanaRetryWaits(t)
		fake := &grafanaRetryServer{statuses: []int{429}, retryAfter: retryAfter}
		if status, _, err := fake.send(t, http.MethodGet, nil); err != nil || status != http.StatusOK {
			t.Fatalf("Retry-After %q: got %d %v", retryAfter, status, err)
		}
		if !slices.Equal(*waits, []time.Duration{want}) {
			t.Fatalf("Retry-After %q: waits = %v, want [%v]", retryAfter, *waits, want)
		}
	}
}

// A POST's 429 is retried with the same body; its 503 is not retried, because
// it may have been applied.
func TestGrafanaSendRetriesWritesOnlyOn429(t *testing.T) {
	recordGrafanaRetryWaits(t)
	fake := &grafanaRetryServer{statuses: []int{429}}
	if status, _, err := fake.send(t, http.MethodPost, []byte(`{"uid":"urnetwork"}`)); err != nil || status != http.StatusOK {
		t.Fatalf("POST after 429: got %d %v", status, err)
	}
	if want := []string{`POST {"uid":"urnetwork"}`, `POST {"uid":"urnetwork"}`}; !slices.Equal(fake.requests, want) {
		t.Fatalf("requests = %v, want the same body twice", fake.requests)
	}

	unavailable := &grafanaRetryServer{statuses: []int{503}}
	if status, _, err := unavailable.send(t, http.MethodPost, []byte(`{}`)); err != nil || status != http.StatusServiceUnavailable {
		t.Fatalf("POST 503: got %d %v, want 503 without a retry", status, err)
	}
	if len(unavailable.requests) != 1 {
		t.Fatalf("POST 503 requests = %v, want 1", unavailable.requests)
	}

	rolling := &grafanaRetryServer{statuses: []int{502, 503, 504}}
	if status, _, err := rolling.send(t, http.MethodGet, nil); err != nil || status != http.StatusOK {
		t.Fatalf("GET through 502-504: got %d %v, want 200", status, err)
	}
	if len(rolling.requests) != 4 {
		t.Fatalf("GET 502-504 requests = %v, want 4", rolling.requests)
	}
}

// A sustained 429 is reported after grafanaRetryAttempts tries.
func TestGrafanaSendGivesUpAfterTheLastAttempt(t *testing.T) {
	waits := recordGrafanaRetryWaits(t)
	statuses := make([]int, 10)
	for i := range statuses {
		statuses[i] = 429
	}
	fake := &grafanaRetryServer{statuses: statuses}
	status, _, err := fake.send(t, http.MethodGet, nil)
	if err != nil || status != http.StatusTooManyRequests {
		t.Fatalf("got %d %v, want 429", status, err)
	}
	if len(fake.requests) != grafanaRetryAttempts || len(*waits) != grafanaRetryAttempts-1 {
		t.Fatalf("requests %d waits %v, want %d requests", len(fake.requests), *waits, grafanaRetryAttempts)
	}
}

// A canceled context ends the wait with the context's error.
func TestGrafanaSendStopsWhenTheContextEnds(t *testing.T) {
	previous := grafanaRetrySleep
	t.Cleanup(func() { grafanaRetrySleep = previous })
	grafanaRetrySleep = func(ctx context.Context, wait time.Duration) error { return context.Canceled }
	fake := &grafanaRetryServer{statuses: []int{429}}
	if _, _, err := fake.send(t, http.MethodGet, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// The 2026-11-07 failure: the load balancer refused LoadDefaults' first call,
// the folder read, with 429 ("Could not read folder (429).") while Grafana was
// healthy. The load now waits and completes.
func TestLoadDefaultsSurvivesALoadBalancer429OnTheFolderRead(t *testing.T) {
	waits := recordGrafanaRetryWaits(t)
	fixture := newPrivatePublicationFixture(t)
	refused := 0
	var mutex sync.Mutex
	inner := fixture.server.Config.Handler
	fixture.server.Config.Handler = http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		first := refused < 2 && request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/api/folders/")
		if first {
			refused++
		}
		mutex.Unlock()
		if first {
			response.WriteHeader(http.StatusTooManyRequests)
			return
		}
		inner.ServeHTTP(response, request)
	})
	if _, _, err := fixture.load(t); err != nil {
		t.Fatalf("LoadDefaults after two 429s on the folder read: %v", err)
	}
	if refused != 2 || len(*waits) < 2 {
		t.Fatalf("refused %d, waits %v: the 429s were not exercised", refused, *waits)
	}
}
