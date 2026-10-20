// Host policy and the immediate Grafana fallback consume the same bounded graph.
package monitor

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"testing"
)

// Constant diagnostics allow deliberate malformed transport unwrap shapes.
type monitorCauseTestOne struct{ cause error }

func (self *monitorCauseTestOne) Error() string { return "synthetic monitor cause" }
func (self *monitorCauseTestOne) Unwrap() error { return self.cause }

// Nil branches remain explicit for the intentional-scope policy.
type monitorCauseTestMany struct{ causes []error }

func (self *monitorCauseTestMany) Error() string   { return "synthetic monitor joined cause" }
func (self *monitorCauseTestMany) Unwrap() []error { return self.causes }

// Arbitrary matching methods cannot impersonate scope or transport permission.
type monitorCauseTestMatch struct{}

func (self *monitorCauseTestMatch) Error() string { return "synthetic monitor matcher" }
func (self *monitorCauseTestMatch) Is(error) bool { panic("monitor custom Is invoked") }
func (self *monitorCauseTestMatch) As(any) bool   { panic("monitor custom As invoked") }

// An incomplete scope result must not recurse again in the fallback classifier.
func TestGrafanaPublicObservationBoundsMalformedScopeAndTransport(t *testing.T) {
	cycle := &monitorCauseTestOne{}
	cycle.cause = cycle
	var deep error = syscall.EHOSTUNREACH
	for range 40 {
		deep = &monitorCauseTestOne{cause: deep}
	}
	wide := make([]error, 256)
	for index := range wide {
		wide[index] = syscall.EHOSTUNREACH
	}
	for index, cause := range []error{cycle, deep, &monitorCauseTestMany{causes: wide}, &monitorCauseTestMany{},
		&monitorCauseTestMany{causes: []error{nil, nil}}, &monitorCauseTestOne{}, &monitorCauseTestMatch{},
		errors.Join(syscall.EHOSTUNREACH, &monitorCauseTestMatch{}),
		errors.Join(context.DeadlineExceeded, &hostScopeExcludedError{})} {
		primaryCalls, fallbackCalls := 0, 0
		client := newGrafanaObservationHTTPClientWithClients(
			grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) { primaryCalls++; return nil, cause }),
			grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
				fallbackCalls++
				return nil, errors.New("unexpected fallback")
			}),
		)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://grafana.fixture.example/api/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if response != nil || err == nil || hostScopeOnlyError(err) || primaryCalls != 1 || fallbackCalls != 0 {
			t.Fatal("unknown Grafana cause became scope success or fallback", index, primaryCalls, fallbackCalls)
		}
	}
}

// Complete scope denial is intentional, while typed transport recovery remains usable.
func TestGrafanaPublicObservationRetainsScopeCancelAndTransientPolicies(t *testing.T) {
	for _, item := range []struct {
		cause           error
		scope, fallback bool
	}{
		{cause: &monitorCauseTestOne{cause: &hostScopeExcludedError{}}, scope: true},
		{cause: errors.Join(&hostScopeExcludedError{}, &hostScopeExcludedError{}), scope: true},
		{cause: errors.Join(syscall.EHOSTUNREACH, syscall.ECONNRESET), fallback: true},
		{cause: &net.DNSError{Err: "synthetic timeout", IsTimeout: true}, fallback: true},
		{cause: &net.DNSError{Err: "synthetic not found", IsNotFound: true}},
		{cause: errors.Join(syscall.EHOSTUNREACH, context.Canceled)},
		{cause: &monitorCauseTestMany{causes: []error{nil, &hostScopeExcludedError{}}}},
	} {
		fallbackCalls := 0
		client := newGrafanaObservationHTTPClientWithClients(
			grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) { return nil, item.cause }),
			grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
				fallbackCalls++
				return grafanaFixtureResponse(http.StatusOK, "recovered"), nil
			}),
		)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://grafana.fixture.example/api/health", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if response != nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatal(closeErr)
			}
		}
		if hostScopeOnlyError(err) != item.scope || (fallbackCalls == 1) != item.fallback || (err == nil) != item.fallback {
			t.Fatal("public scope/cancellation/fallback policy changed", item.scope, item.fallback, fallbackCalls)
		}
	}
}
