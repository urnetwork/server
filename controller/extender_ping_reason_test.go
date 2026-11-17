package controller

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// What the operator can read about refused pings (the extender ping review of
// 2026-11-11). From 2026-11-07, 87% of the extender pingers' pings were
// refused, 98.7% of them because the target's directory no longer held the
// pinger's record; the window gauge had no reason, so the dashboards and the
// monitor read a gossip outage as targets refusing honest claims. And the
// claims the report route refuses itself were not stored, logged or metered.
// Both are read here by name, as the dashboard and the scrape read them.

// The window gauge carries the refusal reason beside the kind, verdict and
// relay, so an unknown pinger is a series of its own rather than part of a
// rejected total.
func TestExtenderPings24hGaugeCarriesTheReason(t *testing.T) {
	labelValues := []string{"extender", "rejected", "no", "unknown_pinger"}
	if _, err := statsExtenderPings24hGauge.gauge.GetMetricWithLabelValues(labelValues...); err != nil {
		t.Fatalf("the ping window gauge cannot say why a target refused: %v", err)
	}
	statsExtenderPings24hGauge.gauge.DeleteLabelValues(labelValues...)
}

// The report route's refusals are registered in the process, as every other
// application metric is, so the api serves them.
func TestExtenderPingReportRefusalsAreRegistered(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, family := range families {
		names = append(names, family.GetName())
	}
	if !slices.Contains(names, "urnetwork_extender_ping_report_refused_total") {
		t.Fatal("the claims the ping report refuses are counted nowhere: urnetwork_extender_ping_report_refused_total is not registered")
	}
}
