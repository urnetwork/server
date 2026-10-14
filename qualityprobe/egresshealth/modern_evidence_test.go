// A URL probe reports what its session observed of the provider for the
// modern provider gate, from the transport its last load rode, and nothing
// from a transport that observes nothing.
package egresshealth

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A transport that answers every request with content and reports the
// session evidence it holds.
type modernEvidenceTestTransport struct {
	evidence ModernEvidence
}

// Implements http.RoundTripper.
func (self modernEvidenceTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("synthetic body")), Header: http.Header{}}, nil
	}).RoundTrip(request)
}

// Implements ModernEvidenceSource.
func (self modernEvidenceTestTransport) ProviderModernEvidence() ModernEvidence {
	return self.evidence
}

// A path whose current client is fixed, as a tunnel that was re-opened
// before the run's last load leaves it.
type modernEvidenceTestPath struct {
	client *http.Client
}

// Implements Path.
func (self modernEvidenceTestPath) Current() (*http.Client, context.Context) {
	return self.client, nil
}

// Implements Path.
func (self modernEvidenceTestPath) Reopen(context.Context) error {
	return fmt.Errorf("the test path is never lost")
}

// One URL turn through client, or through path when it is set.
func modernEvidenceTestRun(t *testing.T, client *http.Client, path Path) *Result {
	t.Helper()
	destinations := []Destination{}
	for i := range 4 {
		destinations = append(destinations, Destination{Name: fmt.Sprintf("synthetic-%d", i), Url: fmt.Sprintf("https://site-%d.example/", i), Class: ClassSite})
	}
	result, err := Check(context.Background(), client, Options{
		UrlProbe: true, Destinations: destinations, Rand: rand.New(rand.NewSource(7)), Path: path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Fatalf("the URL turn measured %d outcomes, want 1", result.Total)
	}
	return result
}

// The evidence is the observation of the transport that carried the run, a
// zero observation and a transport without the source report none, and with
// a path the observation is the path's current client's, the tunnel the last
// load rode, not the client the run started with.
func TestUrlProbeReportsTheSessionModernEvidence(t *testing.T) {
	observed := ModernEvidence{
		E2eNegotiated:             true,
		KeyExchange:               "X25519MLKEM768",
		PostQuantum:               true,
		WindowAdvertised:          true,
		MaxReceiveWindowByteCount: 4 * 1024 * 1024,
		ObservedAt:                time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC),
	}
	result := modernEvidenceTestRun(t, &http.Client{Transport: modernEvidenceTestTransport{evidence: observed}}, nil)
	if result.ModernEvidence == nil || *result.ModernEvidence != observed {
		t.Fatalf("modern evidence %+v, want %+v", result.ModernEvidence, observed)
	}

	unobserved := ModernEvidence{E2eNegotiated: true, PostQuantum: true, WindowAdvertised: true}
	if result := modernEvidenceTestRun(t, &http.Client{Transport: modernEvidenceTestTransport{evidence: unobserved}}, nil); result.ModernEvidence != nil {
		t.Errorf("an observation without a time was reported: %+v", result.ModernEvidence)
	}
	plain := &http.Client{Transport: modernEvidenceTestTransport{}.plain()}
	if result := modernEvidenceTestRun(t, plain, nil); result.ModernEvidence != nil {
		t.Errorf("a transport without the source reported %+v", result.ModernEvidence)
	}

	reopened := &http.Client{Transport: modernEvidenceTestTransport{evidence: observed}}
	result = modernEvidenceTestRun(t, plain, modernEvidenceTestPath{client: reopened})
	if result.ModernEvidence == nil || *result.ModernEvidence != observed {
		t.Fatalf("modern evidence through a path %+v, want the current client's %+v", result.ModernEvidence, observed)
	}
}

// The same answers through a transport that is no ModernEvidenceSource.
func (self modernEvidenceTestTransport) plain() http.RoundTripper {
	return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		return self.RoundTrip(request)
	})
}
