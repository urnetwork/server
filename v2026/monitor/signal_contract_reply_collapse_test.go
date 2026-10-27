package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Incident B replayed from recorded Mimir history: the proxy's replies fall
// under 25% of their two-hour median at 14:47Z on 10-26 and the page lands at
// 14:56Z (27.7 replies/s against 204.9), not a minute earlier; the healthy
// reference raises nothing.
func TestContractReplyCollapseReplaysIncidentB(t *testing.T) {
	cases := loadStormReplayFixture(t, "contract-reply-collapse")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "incident-b", targets: []string{"main/proxy"}},
		{name: "incident-b-before"},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewContractReplyCollapseSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["contract-reply-collapse"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
	}
	alert := requireAlertClass(t, runStormReplay(t, NewContractReplyCollapseSignal(), cases["incident-b"], nil), "contract-reply-collapse")
	if alert.Severity != SeverityPage || alert.SignalNumber != "26.10" || !strings.Contains(alert.Observed, "replies_per_second_now=27.7") || !strings.Contains(alert.Observed, "reference_replies_per_second=204.9") {
		t.Fatalf("%+v", alert)
	}
}

func contractReplyCollapseResponse(t *testing.T, ratio, replies, reference []float64) string {
	return stormSyntheticResponse(t, stormSyntheticStart(10), time.Minute, []stormSyntheticSeries{
		{part: "ratio", values: ratio},
		{part: "replies", values: replies},
		{part: "reference", values: reference},
	})
}

// The band is under its level, strictly, for all ten minutes; a zero
// reference is nothing to collapse from; a missing ratio otherwise is unknown.
func TestContractReplyCollapseBands(t *testing.T) {
	probe := contractReplyCollapseProbe{}
	identity := "contract-reply-collapse main/proxy"
	visibility := "contract-reply-collapse-unobservable main/contract-reply-collapse"
	nan := stormRepeat(math.NaN(), 10)
	gap := stormRepeat(0.1, 10)
	gap[2] = math.NaN()
	for _, test := range []struct {
		name                      string
		ratio, replies, reference []float64
		want                      map[string]string
	}{
		{name: "healthy", ratio: stormRepeat(1.1, 10), replies: stormRepeat(220, 10), reference: stormRepeat(200, 10), want: map[string]string{identity: "healthy", visibility: "healthy"}},
		{name: "at the level", ratio: stormRepeat(0.25, 10), replies: stormRepeat(50, 10), reference: stormRepeat(200, 10), want: map[string]string{identity: "healthy"}},
		{name: "under the level", ratio: stormRepeat(0.135, 10), replies: stormRepeat(27, 10), reference: stormRepeat(200, 10), want: map[string]string{identity: "page"}},
		{name: "nine minutes", ratio: append([]float64{0.3}, stormRepeat(0.1, 9)...), replies: stormRepeat(20, 10), reference: stormRepeat(200, 10), want: map[string]string{identity: "hold"}},
		{name: "a zero reference", ratio: nan, replies: stormRepeat(0, 10), reference: stormRepeat(0, 10), want: map[string]string{identity: "healthy", visibility: "healthy"}},
		{name: "a missing ratio", ratio: gap, replies: stormRepeat(20, 10), reference: stormRepeat(200, 10), want: map[string]string{identity: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, contractReplyCollapseResponse(t, test.ratio, test.replies, test.reference), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestContractReplyCollapseQueryIsTheCatalogExpression(t *testing.T) {
	settings := DefaultContractReplyCollapseSettings()
	if settings.PageRatio != 0.25 || settings.Hold != 10*time.Minute || settings.BaselineSpan != 2*time.Hour || settings.BaselineStep != 5*time.Minute || settings.BaselineOffset != 10*time.Minute {
		t.Fatalf("§26.10 policy changed: %+v", settings)
	}
	replies := `sum(rate(urnetwork_contract_creation_completed_total{env="main",service="proxy",outcome="contract_reply"}[5m]))`
	want := replies + ` / quantile_over_time(0.5, ` + replies + `[2h:5m] offset 10m)`
	if query := contractReplyCollapseQuery("main", settings); !strings.Contains(query, want) {
		t.Fatalf("query lost the catalog expression:\n%s", query)
	}
}
