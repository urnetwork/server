package work

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// An ingest that refuses every outcome.
type udpCountryTestRefusingIngest struct {
	egressProbeIngest
}

func (*udpCountryTestRefusingIngest) SubmitEgressHealth(context.Context, string, *egresshealth.Result) error {
	return errors.New("synthetic refusal")
}

// The HTTP/3 legs a country turns off are counted by the claim's country, and
// in none of the UDP transport outcomes: not measured, never a success, a
// silence or a skip. A country that measures UDP counts its legs as before,
// and a turn the ingest did not acknowledge counts nothing.
func TestUdpCountrySwitchCountsSkippedLegsByCountry(t *testing.T) {
	disabled := func(country string) float64 {
		return testutil.ToFloat64(urlProbeUdpCountryDisabled.WithLabelValues(country))
	}
	transport := func(transport string, outcome string) float64 {
		return testutil.ToFloat64(urlProbeTransportOutcomes.WithLabelValues(transport, outcome))
	}
	udp := func() float64 {
		sum := 0.0
		for _, outcome := range []string{"success", "answered", "silent", "not_measured", "skipped"} {
			sum += transport("udp", outcome)
		}
		return sum
	}
	for _, c := range []struct {
		country                         string
		irDisabled, deDisabled, udpLegs float64
	}{
		{"ir", 1, 0, 0},
		{"de", 0, 0, 1},
	} {
		run := udpCountryTestRun(t, c.country, udpCountryTestSilentLeg())
		beforeIr, beforeDe, beforeUdp, beforeTcp := disabled("ir"), disabled("de"), udp(), transport("tcp", "success")
		reporter := newEgressProbeMetricsReporter(&testH3AckIngest{}, nil)
		ctx := withProviderEgressClaimCountry(context.Background(), "synthetic-provider", c.country)
		if err := reporter.submitEgressHealthScored(ctx, "synthetic-provider", run, run); err != nil {
			t.Fatal(err)
		}
		got := [4]float64{disabled("ir") - beforeIr, disabled("de") - beforeDe, udp() - beforeUdp, transport("tcp", "success") - beforeTcp}
		if want := [4]float64{c.irDisabled, c.deDisabled, c.udpLegs, 1}; got != want {
			t.Errorf("%s: country_disabled ir and de, UDP legs and TCP successes counted %v, want %v", c.country, got, want)
		}
	}

	run := udpCountryTestRun(t, "ir", udpCountryTestSilentLeg())
	before := disabled("ir")
	reporter := newEgressProbeMetricsReporter(&udpCountryTestRefusingIngest{}, nil)
	ctx := withProviderEgressClaimCountry(context.Background(), "synthetic-provider", "ir")
	if err := reporter.submitEgressHealthScored(ctx, "synthetic-provider", run, run); err == nil {
		t.Fatal("the refusing ingest acknowledged the turn")
	}
	if disabled("ir") != before {
		t.Fatal("a turn the ingest refused was counted")
	}
}
