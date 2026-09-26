package prober

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Tests of the bandwidth sample riding a probe: the same tunnel, never failing
// the probe, and skipped after a failed one.

// Records the failure class reported for each probe, which is the
// server-visible record of whether the probe succeeded.
type stubAttempts struct {
	failures []string
}

// Implements AttemptReporter.
func (self *stubAttempts) ReportAttempt(ctx context.Context, id string, probeFailure string) error {
	self.failures = append(self.failures, probeFailure)
	return nil
}

// Returns a prober whose every probe succeeds over one tunnel client, and that
// client.
func bandwidthProber(t *testing.T, sub *stubSubmitter) (*Prober, *http.Client) {
	t.Helper()
	tunnelClient := &http.Client{}
	return &Prober{
		Open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
			return tunnelClient, func() error { return nil }, nil
		},
		Health: answers(exitResult()),
		Submit: sub,
	}, tunnelClient
}

// The measurement must reuse the
// client the health run already went over. A second tunnel would put
// the provider under contract twice and would measure a different session than
// the one the location came from.
func TestProbeOneRunsBandwidthOverTheSameTunnel(t *testing.T) {
	sub := &stubSubmitter{}
	p, tunnelClient := bandwidthProber(t, sub)

	var gotClient *http.Client
	var gotProvider string
	calls := 0
	p.Bandwidth = func(ctx context.Context, providerClientId string, client *http.Client) {
		calls++
		gotProvider = providerClientId
		gotClient = client
	}

	if err := p.ProbeOne(context.Background(), provider("provider-1")); err != nil {
		t.Fatalf("ProbeOne err = %v", err)
	}
	if calls != 1 {
		t.Fatalf("bandwidth sampler ran %d times, want 1", calls)
	}
	if gotProvider != "provider-1" {
		t.Errorf("sampler got provider %q", gotProvider)
	}
	if gotClient != tunnelClient {
		t.Error("the bandwidth sampler was handed a different client than the health run used -- it must ride the same tunnel")
	}
}

// The property the task
// requires of a budget skip: a provider skipped for lack of byte budget is a
// clean skip, not a failed probe. The same must hold for any other bandwidth
// outcome, so a sampler that measured nothing at all and one that blew up
// internally are both covered here -- neither may change ProbeOne's error nor
// the failure class recorded server-side, which is what decides when the
// provider is probed again.
func TestProbeOneBandwidthOutcomeNeverFailsTheProbe(t *testing.T) {
	cases := []struct {
		name    string
		sampler BandwidthSampler
	}{
		{
			name:    "no sampler configured",
			sampler: nil,
		},
		{
			name: "every target skipped for budget",
			sampler: func(ctx context.Context, id string, client *http.Client) {
				// what bandwidth.Sampler does on a 429: it measures nothing,
				// reports nothing, and returns
			},
		},
		{
			name: "the sampler consumed the context",
			sampler: func(ctx context.Context, id string, client *http.Client) {
				inner, cancel := context.WithCancel(ctx)
				cancel()
				_ = inner.Err()
			},
		},
	}

	for _, c := range cases {
		sub := &stubSubmitter{}
		p, _ := bandwidthProber(t, sub)
		attempts := &stubAttempts{}
		p.Attempts = attempts
		p.Bandwidth = c.sampler

		if err := p.ProbeOne(context.Background(), provider("provider-1")); err != nil {
			t.Fatalf("%s: ProbeOne err = %v, want nil: a bandwidth outcome must not fail the probe", c.name, err)
		}
		if sub.calls != 1 {
			t.Errorf("%s: submit calls = %d, want 1", c.name, sub.calls)
		}
		if len(attempts.failures) != 1 || attempts.failures[0] != "" {
			t.Errorf("%s: reported failure classes = %q, want exactly one success (\"\")", c.name, attempts.failures)
		}
	}
}

// The byte budget is scarce and
// each measurement is real paid traffic, so it is not spent on a tunnel that
// has already failed to carry the probe.
func TestProbeOneSkipsBandwidthWhenTheProbeFailed(t *testing.T) {
	noExit := exitResult()
	noExit.ExitIp, noExit.IpEchoErr = "", "the /ip echo answered status 502"
	cases := []struct {
		name   string
		open   TunnelOpener
		health EgressHealthChecker
		submi  error
	}{
		{
			name: "tunnel failed",
			open: func(ctx context.Context, id string) (*http.Client, func() error, error) {
				return nil, nil, errors.New("no route to provider")
			},
		},
		{
			name:   "no exit address",
			health: answers(noExit),
		},
		{
			name:  "submission rejected",
			submi: errors.New("server rejected the submission"),
		},
	}

	for _, c := range cases {
		sub := &stubSubmitter{err: c.submi}
		p, _ := bandwidthProber(t, sub)
		if c.open != nil {
			p.Open = c.open
		}
		if c.health != nil {
			p.Health = c.health
		}
		p.Bandwidth = func(ctx context.Context, id string, client *http.Client) {
			t.Errorf("%s: bandwidth must not be sampled for a probe that did not succeed", c.name)
		}

		if err := p.ProbeOne(context.Background(), provider("provider-1")); err == nil {
			t.Fatalf("%s: expected the probe to fail", c.name)
		}
	}
}
