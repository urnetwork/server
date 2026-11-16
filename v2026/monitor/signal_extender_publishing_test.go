package monitor

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"
)

// §27.2 over synthetic series: half an hour of one-minute evaluations, the
// longest hold, of the gossip queue's two kinds and the dns tick's three
// record types.

const extenderPublishingTestPoints = 30

// The series of one read: the oldest unreleased gossip message per kind, and
// the failed and accepted dns changes per record type over the window, one
// value per evaluation (NaN for none). A nil map leaves the part out.
type extenderPublishingTestSeries struct {
	gossipOldest map[string][]float64
	dnsFailed    map[string][]float64
	dnsAccepted  map[string][]float64
}

// Main healthy: a queue drained within seconds, and every tick's changes
// accepted.
func extenderPublishingTestHealthy() extenderPublishingTestSeries {
	return extenderPublishingTestSeries{
		gossipOldest: map[string][]float64{
			"record":     stormRepeat(3, extenderPublishingTestPoints),
			"revocation": stormRepeat(0, extenderPublishingTestPoints),
		},
		dnsFailed: map[string][]float64{
			"A":    stormRepeat(0, extenderPublishingTestPoints),
			"AAAA": stormRepeat(0, extenderPublishingTestPoints),
			"TXT":  stormRepeat(0, extenderPublishingTestPoints),
		},
		dnsAccepted: map[string][]float64{
			"A":    stormRepeat(7, extenderPublishingTestPoints),
			"AAAA": stormRepeat(6, extenderPublishingTestPoints),
			"TXT":  stormRepeat(7, extenderPublishingTestPoints),
		},
	}
}

func extenderPublishingTestResponse(t *testing.T, series extenderPublishingTestSeries) string {
	t.Helper()
	synthetic := []stormSyntheticSeries{}
	for kind, values := range series.gossipOldest {
		synthetic = append(synthetic, stormSyntheticSeries{part: "gossip_oldest", labels: map[string]string{"kind": kind}, values: values})
	}
	for recordType, values := range series.dnsFailed {
		synthetic = append(synthetic, stormSyntheticSeries{part: "dns_failed", labels: map[string]string{"record_type": recordType}, values: values})
	}
	for recordType, values := range series.dnsAccepted {
		synthetic = append(synthetic, stormSyntheticSeries{part: "dns_accepted", labels: map[string]string{"record_type": recordType}, values: values})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(extenderPublishingTestPoints), time.Minute, synthetic)
}

// Values that change at a step: before for the first count, after for the
// rest.
func extenderPublishingTestStep(before float64, after float64, count int) []float64 {
	values := stormRepeat(after, extenderPublishingTestPoints)
	for index := range count {
		values[index] = before
	}
	return values
}

func TestExtenderPublishingJudgesGossipAndDns(t *testing.T) {
	probe := extenderPublishingProbe{}
	const (
		recordStalled     = "extender-gossip-stalled main/extender-gossip/record"
		revocationStalled = "extender-gossip-stalled main/extender-gossip/revocation"
		aFailing          = "extender-dns-publish-failing main/extender-dns/A"
		aaaaFailing       = "extender-dns-publish-failing main/extender-dns/AAAA"
		txtFailing        = "extender-dns-publish-failing main/extender-dns/TXT"
		visibility        = "extender-publishing-unobservable main/extender-publishing"
	)

	// the gossip queue of 2026-11-11: nothing released for five and a half
	// days
	stalled := extenderPublishingTestHealthy()
	stalled.gossipOldest["record"] = stormRepeat(479_691, extenderPublishingTestPoints)
	stalled.gossipOldest["revocation"] = stormRepeat(477_560, extenderPublishingTestPoints)

	// a drain that came back three evaluations ago
	recovering := extenderPublishingTestHealthy()
	recovering.gossipOldest["record"] = extenderPublishingTestStep(4_000, 4, extenderPublishingTestPoints-3)

	// every tick's TXT sets refused, as Route 53 refused every set of 10-28
	txtRefused := extenderPublishingTestHealthy()
	txtRefused.dnsFailed["TXT"] = stormRepeat(5, extenderPublishingTestPoints)
	txtRefused.dnsAccepted["TXT"] = stormRepeat(0, extenderPublishingTestPoints)

	// one failed tick, fifteen minutes in the window and gone again
	oneFailure := extenderPublishingTestHealthy()
	oneFailure.dnsFailed["A"] = extenderPublishingTestStep(1, 0, 15)

	// nothing sent at all: the zone did not resolve, every change failed
	unreachable := extenderPublishingTestHealthy()
	for _, recordType := range []string{"A", "AAAA", "TXT"} {
		unreachable.dnsFailed[recordType] = stormRepeat(10, extenderPublishingTestPoints)
		unreachable.dnsAccepted[recordType] = stormRepeat(0, extenderPublishingTestPoints)
	}

	gappy := extenderPublishingTestHealthy()
	gappy.dnsFailed["AAAA"][12] = math.NaN()

	for _, test := range []struct {
		name   string
		series extenderPublishingTestSeries
		want   map[string]string
	}{
		{name: "healthy", series: extenderPublishingTestHealthy(), want: map[string]string{
			recordStalled: "healthy", revocationStalled: "healthy",
			aFailing: "healthy", aaaaFailing: "healthy", txtFailing: "healthy", visibility: "healthy",
		}},
		{name: "the gossip drain stopped", series: stalled, want: map[string]string{
			recordStalled: "warn", revocationStalled: "warn", txtFailing: "healthy", visibility: "healthy",
		}},
		{name: "the gossip drain recovering", series: recovering, want: map[string]string{
			recordStalled: "hold", revocationStalled: "healthy",
		}},
		{name: "every TXT set refused", series: txtRefused, want: map[string]string{
			txtFailing: "warn", aFailing: "healthy", aaaaFailing: "healthy", recordStalled: "healthy",
		}},
		{name: "one failed tick", series: oneFailure, want: map[string]string{
			aFailing: "hold", txtFailing: "healthy",
		}},
		{name: "route 53 unreachable", series: unreachable, want: map[string]string{
			aFailing: "warn", aaaaFailing: "warn", txtFailing: "warn",
		}},
		{name: "an evaluation missing", series: gappy, want: map[string]string{
			aaaaFailing: "hold", visibility: "unobservable",
		}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, extenderPublishingTestResponse(t, test.series), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

// A part no publisher exported is unknown, not healthy: Main on 11-11 ran a
// taskworker that predates the dns changes counter, so the signal judges the
// gossip queue and reports the dns state missing.
func TestExtenderPublishingReportsMissingState(t *testing.T) {
	probe := extenderPublishingProbe{}

	predatesTheCounter := extenderPublishingTestHealthy()
	predatesTheCounter.gossipOldest["record"] = stormRepeat(479_691, extenderPublishingTestPoints)
	predatesTheCounter.dnsFailed = nil
	predatesTheCounter.dnsAccepted = nil
	states := stormFindingStates(t, probe, stormSyntheticNow, extenderPublishingTestResponse(t, predatesTheCounter), nil)
	requireStormStates(t, "a build without the counter", states, map[string]string{
		"extender-gossip-stalled main/extender-gossip/record":       "warn",
		"extender-publishing-unobservable main/extender-publishing": "unobservable",
	})
	alerts := runStormSynthetic(t, NewExtenderPublishingSignal(), stormSyntheticNow, extenderPublishingTestResponse(t, predatesTheCounter), nil)
	sawMissing := false
	for _, alert := range alerts {
		if alert.Class == "extender-publishing-unobservable" {
			sawMissing = strings.Contains(alert.Observed, "dns_state_missing")
		}
	}
	if !sawMissing {
		t.Fatalf("no dns_state_missing visibility alert in %+v", alerts)
	}

	nothing := extenderPublishingTestSeries{}
	states = stormFindingStates(t, probe, stormSyntheticNow, extenderPublishingTestResponse(t, nothing), nil)
	requireStormStates(t, "nothing exported", states, map[string]string{
		"extender-publishing-unobservable main/extender-publishing": "unobservable",
	})
	if len(states) != 1 {
		t.Fatalf("nothing exported judged sources: %v", states)
	}
	alerts = runStormSynthetic(t, NewExtenderPublishingSignal(), stormSyntheticNow, extenderPublishingTestResponse(t, nothing), nil)
	if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "gossip_state_missing") || !strings.Contains(alerts[0].Observed, "dns_state_missing") {
		t.Fatalf("nothing exported: %+v", alerts)
	}
}

// Alerts name a kind or a record type, and the query reads the series §27.2
// names, over the window it names.
func TestExtenderPublishingQueryAndLabels(t *testing.T) {
	settings := DefaultExtenderPublishingSettings()
	if settings.GossipMaxPendingAge != 10*time.Minute || settings.GossipHold != 5*time.Minute ||
		settings.DnsFailureWindow != 15*time.Minute || settings.DnsHold != 30*time.Minute {
		t.Fatalf("§27.2 policy changed: %+v", settings)
	}
	query := extenderPublishingQuery("main", settings)
	for _, want := range []string{
		`max by (kind) (urnetwork_stats_extender_gossip_oldest_pending_seconds{env="main"})`,
		`sum by (record_type) (increase(urnetwork_taskworker_extender_dns_changes_total{env="main",outcome="failed"}[15m]))`,
		`sum by (record_type) (increase(urnetwork_taskworker_extender_dns_changes_total{env="main",outcome="accepted"}[15m]))`,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query lost %s:\n%s", want, query)
		}
	}

	series := extenderPublishingTestHealthy()
	series.gossipOldest["revocation"] = stormRepeat(3_600, extenderPublishingTestPoints)
	series.dnsFailed["TXT"] = stormRepeat(5, extenderPublishingTestPoints)
	alerts := runStormSynthetic(t, NewExtenderPublishingSignal(), stormSyntheticNow, extenderPublishingTestResponse(t, series), nil)
	found := map[string]bool{}
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "203.0.113", "2001:db8", "https://")
		if alert.SignalNumber != "27.2" || alert.Severity != SeverityWarn {
			continue
		}
		switch alert.Class {
		case "extender-gossip-stalled":
			found[alert.Class] = strings.Contains(alert.Observed, "kind=revocation") && strings.Contains(alert.Observed, "oldest_pending_seconds=3600")
		case "extender-dns-publish-failing":
			found[alert.Class] = strings.Contains(alert.Observed, "record_type=TXT") && strings.Contains(alert.Observed, "failed_changes_15m=5.0")
		}
	}
	if !found["extender-gossip-stalled"] || !found["extender-dns-publish-failing"] {
		t.Fatalf("alerts = %+v", alerts)
	}
}

func TestExtenderPublishingRejectsInvalidPolicyWithoutReading(t *testing.T) {
	signal := NewExtenderPublishingSignalWithSettings(&ExtenderPublishingSettings{})
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		t.Fatalf("read its source under an invalid policy")
		return "", nil
	}}
	alerts, err := signal.Run(context.Background(), stormMainSettings(stormSyntheticNow, source))
	if err != nil || len(alerts) != 1 || alerts[0].Class != "extender-publishing-unobservable" || !strings.Contains(alerts[0].Observed, "invalid_policy") {
		t.Fatalf("%v %+v", err, alerts)
	}
}
