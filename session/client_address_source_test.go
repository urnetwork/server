package session

import (
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The partition must stay closed: the resolver decides every value, so an
// extra child means a branch started deriving a label from request content.
func TestClientAddressResolutionPartitionIsBounded(t *testing.T) {
	if count := testutil.CollectAndCount(clientAddressResolutionCounter); count != len(clientAddressSources) {
		t.Fatalf("client address resolution series = %d, want %d", count, len(clientAddressSources))
	}
}

func resolutionCounts() map[clientAddressSource]float64 {
	counts := map[clientAddressSource]float64{}
	for _, source := range clientAddressSources {
		counts[source] = testutil.ToFloat64(clientAddressResolutionCounter.WithLabelValues(string(source)))
	}
	return counts
}

// Every assertion below compares deltas, never absolute values: the rest of
// the package moves the same process-global counters. session/ declares no
// t.Parallel, which is what makes a delta well defined.
func TestAbsentUrForwardedHeaderIsCountedAsPeerAbsent(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "192.0.2.44:54321"

	before := resolutionCounts()
	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	after := resolutionCounts()

	// The resolved address is the contract; instrumentation may not move it.
	if got != "192.0.2.44:54321" {
		t.Fatalf("resolved address = %q, want the socket peer", got)
	}
	assertOnlyDelta(t, before, after, clientAddressSourcePeerAbsent)
}

func TestForwardedUrHeaderIsNotCountedAsPeerAbsent(t *testing.T) {
	req := httptest.NewRequest("POST", "/auth/network-create", nil)
	req.RemoteAddr = "65.49.70.82:52344"
	req.Header.Set("X-UR-Forwarded-For", "173.25.160.143:41001")

	before := resolutionCounts()
	got, err := ResolveClientAddress(req)
	if err != nil {
		t.Fatal(err)
	}
	after := resolutionCounts()

	if got != "173.25.160.143:41001" {
		t.Fatalf("resolved address = %q, want the ingress-supplied client address", got)
	}
	assertOnlyDelta(t, before, after, clientAddressSourceUrHeader)
}

// Each anomalous header shape has its own label, and none of them is
// peer_absent: an ingress that appends, or that interpolates an unresolved
// variable, is a different misconfiguration from one that never sets the
// header, and collapsing them would hide which one an operator has.
func TestAnomalousUrForwardedHeadersAreCountedSeparately(t *testing.T) {
	tests := []struct {
		name   string
		values []string
		source clientAddressSource
	}{
		{"repeated", []string{"173.25.160.143:41001", "173.25.160.144:41002"}, clientAddressSourcePeerRepeated},
		{"empty", []string{""}, clientAddressSourcePeerEmpty},
		{"whitespace", []string{"   "}, clientAddressSourcePeerEmpty},
		{"malformed", []string{"not-an-address"}, clientAddressSourcePeerMalformed},
		{"bare ip", []string{"173.25.160.143"}, clientAddressSourcePeerMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/auth/login", nil)
			req.RemoteAddr = "65.49.70.82:52344"
			for _, value := range test.values {
				req.Header.Add("X-UR-Forwarded-For", value)
			}

			before := resolutionCounts()
			got, err := ResolveClientAddress(req)
			if err != nil {
				t.Fatal(err)
			}
			after := resolutionCounts()

			if got != "65.49.70.82:52344" {
				t.Fatalf("resolved address = %q, want the socket peer", got)
			}
			assertOnlyDelta(t, before, after, test.source)
		})
	}
}

// The session built by the HTTP entry point carries the same address, and one
// request counts exactly once wherever it enters.
func TestClientSessionFromRequestCountsOnce(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "192.0.2.44:54321"

	before := resolutionCounts()
	clientSession, err := NewClientSessionFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Cancel()
	after := resolutionCounts()

	if clientSession.ClientAddress != "192.0.2.44:54321" {
		t.Fatalf("session client address = %q, want the socket peer", clientSession.ClientAddress)
	}
	assertOnlyDelta(t, before, after, clientAddressSourcePeerAbsent)
}

// An unparseable peer resolves no address, so it joins no partition.
func TestUnresolvableRemoteAddressCountsNothing(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.RemoteAddr = "not-an-address"

	before := resolutionCounts()
	if _, err := ResolveClientAddress(req); err == nil {
		t.Fatal("expected an unparseable remote address to fail")
	}
	after := resolutionCounts()

	for _, source := range clientAddressSources {
		if after[source] != before[source] {
			t.Fatalf("source %s moved by %v on an unresolved address, want 0", source, after[source]-before[source])
		}
	}
}

func assertOnlyDelta(t *testing.T, before map[clientAddressSource]float64, after map[clientAddressSource]float64, want clientAddressSource) {
	t.Helper()
	for _, source := range clientAddressSources {
		delta := after[source] - before[source]
		expected := float64(0)
		if source == want {
			expected = 1
		}
		if delta != expected {
			t.Errorf("source %s moved by %v, want %v", source, delta, expected)
		}
	}
}
