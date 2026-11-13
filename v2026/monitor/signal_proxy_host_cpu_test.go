package monitor

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Incident B replayed from recorded Mimir history: both proxy hosts pass 85%
// at 15:17Z on 10-26 and warn at 15:26Z, not a minute earlier; the healthy
// reference of 10-20 (9-24%) raises nothing.
func TestProxyHostCpuReplaysIncidentB(t *testing.T) {
	cases := loadStormReplayFixture(t, "proxy-host-cpu")
	for _, test := range []struct {
		name    string
		targets []string
	}{
		{name: "incident-b", targets: []string{"crisp", "fireside"}},
		{name: "incident-b-before"},
		{name: "healthy"},
	} {
		alerts := runStormReplay(t, NewProxyHostCpuSignal(), cases[test.name], nil)
		want := map[string][]string{}
		if 0 < len(test.targets) {
			want["proxy-host-cpu"] = test.targets
		}
		requireStormAlerts(t, test.name, alerts, want)
		for _, alert := range alerts {
			if alert.Severity != SeverityWarn || alert.SignalNumber != "26.9" {
				t.Fatalf("%s: %+v", test.name, alert)
			}
		}
	}
	crisp := stormAlertsByTarget(runStormReplay(t, NewProxyHostCpuSignal(), cases["incident-b"], nil), "proxy-host-cpu")["crisp"]
	if !strings.Contains(crisp.Observed, "busy_share_now=0.943") {
		t.Fatalf("observed %q", crisp.Observed)
	}
}

func proxyHostCpuResponse(t *testing.T, hosts map[string][]float64) string {
	series := []stormSyntheticSeries{}
	for name, values := range hosts {
		series = append(series, stormSyntheticSeries{part: "host_busy", labels: map[string]string{"host": name}, values: values})
	}
	return stormSyntheticResponse(t, stormSyntheticStart(10), time.Minute, series)
}

// The hosts come from the proxy's placement; each host's band is strict and
// needs all ten minutes; a missing host is unknown.
func TestProxyHostCpuHostsAndBands(t *testing.T) {
	probe := proxyHostCpuProbe{}
	crisp, fireside := "proxy-host-cpu crisp", "proxy-host-cpu fireside"
	visibility := "proxy-host-cpu-unobservable main/proxy-host-cpu"
	gap := stormRepeat(0.9, 10)
	gap[4] = math.NaN()
	for _, test := range []struct {
		name  string
		hosts map[string][]float64
		want  map[string]string
	}{
		{name: "healthy", hosts: map[string][]float64{"crisp": stormRepeat(0.4, 10), "fireside": stormRepeat(0.4, 10)}, want: map[string]string{crisp: "healthy", fireside: "healthy", visibility: "healthy"}},
		{name: "at the level", hosts: map[string][]float64{"crisp": stormRepeat(0.85, 10), "fireside": stormRepeat(0.4, 10)}, want: map[string]string{crisp: "healthy"}},
		{name: "one host above", hosts: map[string][]float64{"crisp": stormRepeat(0.86, 10), "fireside": stormRepeat(0.4, 10)}, want: map[string]string{crisp: "warn", fireside: "healthy"}},
		{name: "nine minutes", hosts: map[string][]float64{"crisp": append([]float64{0.71}, stormRepeat(0.9, 9)...), "fireside": stormRepeat(0.4, 10)}, want: map[string]string{crisp: "hold"}},
		{name: "a missing host", hosts: map[string][]float64{"crisp": stormRepeat(0.4, 10)}, want: map[string]string{crisp: "healthy", fireside: "hold", visibility: "unobservable"}},
		{name: "a gap", hosts: map[string][]float64{"crisp": gap, "fireside": stormRepeat(0.4, 10)}, want: map[string]string{crisp: "hold", visibility: "unobservable"}},
	} {
		states := stormFindingStates(t, probe, stormSyntheticNow, proxyHostCpuResponse(t, test.hosts), nil)
		requireStormStates(t, test.name, states, test.want)
	}
}

func TestProxyHostCpuQueryIsTheCatalogExpressionOverPlacedHosts(t *testing.T) {
	settings := DefaultProxyHostCpuSettings()
	if settings.HostBusy != 0.85 || settings.Hold != 10*time.Minute || settings.Window != 5*time.Minute {
		t.Fatalf("§26.9 policy changed: %+v", settings)
	}
	want := `1 - sum by (host) (rate(node_cpu_seconds_total{env="main",host=~"crisp|fireside",mode=~"idle|iowait"}[5m])) / sum by (host) (rate(node_cpu_seconds_total{env="main",host=~"crisp|fireside"}[5m]))`
	if query := proxyHostCpuQuery("main", []string{"crisp", "fireside"}, settings); !strings.Contains(query, want) {
		t.Fatalf("query lost the catalog expression:\n%s", query)
	}
}
