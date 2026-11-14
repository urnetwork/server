package proxy

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A source with one nonzero value per series.
type windowClientStatsTestSource struct{}

func (windowClientStatsTestSource) VisitCandidates(visit func(window string, state string, outcome string, count uint64)) {
	visit("quality", "strict_hole", "declined_strict_cap", 7)
}

func (windowClientStatsTestSource) VisitExpandPasses(visit func(window string, state string, count uint64)) {
	visit("speed", "skipped_strict_full", 3)
}

func (windowClientStatsTestSource) VisitWarnings(visit func(window string, cause string, count uint64)) {
	visit("quality", "drain", 2)
}

func (windowClientStatsTestSource) VisitRemovals(visit func(window string, cause string, count uint64)) {
	visit("quality", "handover", 5)
}

func (windowClientStatsTestSource) VisitBalanceProbes(visit func(window string, outcome string, count uint64)) {
	visit("quality", "short", 13)
}

func (windowClientStatsTestSource) VisitWindows(visit func(window string, state string, count int64)) {
	visit("quality", "strict_hole", 11)
}

func (windowClientStatsTestSource) VisitWindowClients(visit func(window string, ip string, protocol string, count int64)) {
	visit("limited", "ipv4", "tcp", 3)
}

func (windowClientStatsTestSource) VisitWindowFlows(visit func(window string, ipVersion string, protocol string, count uint64)) {
	visit("limited", "4", "tcp", 17)
}

func (windowClientStatsTestSource) VisitNoCompatible(visit func(ipVersion string, protocol string, count uint64)) {
	visit("4", "udp", 2)
}

// Every visited value is exported under its series with its label values.
func TestWindowClientStatsCollectorExportsVisitedValues(t *testing.T) {
	collector := newWindowClientStatsCollector(windowClientStatsTestSource{})
	expected := `
# HELP urnetwork_proxy_window_balance_probes_total Balance probes hosted windows sent while the payer's balance refused their contracts, by what each learned
# TYPE urnetwork_proxy_window_balance_probes_total counter
urnetwork_proxy_window_balance_probes_total{outcome="short",window="quality"} 13
# HELP urnetwork_proxy_window_candidates_total Window client candidates hosted windows built, by the state of the expansion pass that built them and how they ended
# TYPE urnetwork_proxy_window_candidates_total counter
urnetwork_proxy_window_candidates_total{outcome="declined_strict_cap",state="strict_hole",window="quality"} 7
# HELP urnetwork_proxy_window_clients Live hosted window clients by window and the ip and protocol labels FindProviders2 gave them
# TYPE urnetwork_proxy_window_clients gauge
urnetwork_proxy_window_clients{ip="ipv4",protocol="tcp",window="limited"} 3
# HELP urnetwork_proxy_window_expand_passes_total Hosted window resize passes that wanted to grow, by the window state they found
# TYPE urnetwork_proxy_window_expand_passes_total counter
urnetwork_proxy_window_expand_passes_total{state="skipped_strict_full",window="speed"} 3
# HELP urnetwork_proxy_window_flows_total Hosted device flows by the window of the exit that first carried them, their ip version and protocol
# TYPE urnetwork_proxy_window_flows_total counter
urnetwork_proxy_window_flows_total{ip_version="4",protocol="tcp",window="limited"} 17
# HELP urnetwork_proxy_window_no_compatible_total Hosted device flows answered unreachable because no window held an exit labelled to carry them, by ip version and protocol
# TYPE urnetwork_proxy_window_no_compatible_total counter
urnetwork_proxy_window_no_compatible_total{ip_version="4",protocol="udp"} 2
# HELP urnetwork_proxy_window_removals_total Hosted window clients removed from their window, by cause
# TYPE urnetwork_proxy_window_removals_total counter
urnetwork_proxy_window_removals_total{cause="handover",window="quality"} 5
# HELP urnetwork_proxy_window_warnings_total Hosted window clients that entered the warned state, by cause
# TYPE urnetwork_proxy_window_warnings_total counter
urnetwork_proxy_window_warnings_total{cause="drain",window="quality"} 2
# HELP urnetwork_proxy_windows Live hosted windows by the state their last resize pass ended in
# TYPE urnetwork_proxy_windows gauge
urnetwork_proxy_windows{state="strict_hole",window="quality"} 11
`
	if err := testutil.CollectAndCompare(collector, strings.NewReader(expected)); err != nil {
		t.Fatal(err)
	}
}

// The process collector exports connect's full fixed vocabulary from the
// first scrape, zeros included, so a rate has a series before its first
// event.
func TestWindowClientStatsCollectorExportsTheFixedVocabulary(t *testing.T) {
	collector := newWindowClientStatsCollector(connect.NewWindowClientStats())
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(collector)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	labels := map[string]map[string]bool{}
	for _, family := range families {
		counts[family.GetName()] = len(family.GetMetric())
		for _, metric := range family.GetMetric() {
			for _, pair := range metric.GetLabel() {
				if labels[pair.GetName()] == nil {
					labels[pair.GetName()] = map[string]bool{}
				}
				labels[pair.GetName()][pair.GetValue()] = true
			}
		}
	}
	// 4 windows (quality, speed, limited, auto) x 3 building pass states x 9
	// outcomes, 4 x 7 pass states, 4 x 7 warning causes, 4 x 7 removal
	// causes, 4 x 4 balance probe outcomes, 4 x 5 window states; the
	// capability series: 4 windows x 2 ip x 2 protocol labels, 4 x 2 ip
	// versions x 3 flow protocols, and 2 x 3 without a window
	for name, want := range map[string]int{
		"urnetwork_proxy_window_candidates_total":     108,
		"urnetwork_proxy_window_expand_passes_total":  28,
		"urnetwork_proxy_window_warnings_total":       28,
		"urnetwork_proxy_window_removals_total":       28,
		"urnetwork_proxy_window_balance_probes_total": 16,
		"urnetwork_proxy_windows":                     20,
		"urnetwork_proxy_window_clients":              16,
		"urnetwork_proxy_window_flows_total":          24,
		"urnetwork_proxy_window_no_compatible_total":  6,
	} {
		if counts[name] != want {
			t.Errorf("%s exports %d series, want %d", name, counts[name], want)
		}
	}
	for _, outcome := range []string{"admitted", "admitted_handover", "declined_strict_cap", "declined_replacement", "not_admitted_surplus", "failed_setup", "failed_ping", "failed_contract", "canceled"} {
		if !labels["outcome"][outcome] {
			t.Errorf("no candidate series has outcome %q", outcome)
		}
	}
	for _, outcome := range []string{"short", "limited", "restored", "unknown"} {
		if !labels["outcome"][outcome] {
			t.Errorf("no balance probe series has outcome %q", outcome)
		}
	}
	for _, state := range []string{"below_min", "below_target", "strict_hole", "skipped_strict_full", "deferred", "held_insufficient_balance", "held_idle", "selectable_full", "no_selectable"} {
		if !labels["state"][state] {
			t.Errorf("no series has state %q", state)
		}
	}
	for _, cause := range []string{"drain", "silent", "unhealthy", "starved", "capacity", "quarantine", "family_swap", "verdict", "transport_down", "collapse", "handover", "closed"} {
		if !labels["cause"][cause] {
			t.Errorf("no series has cause %q", cause)
		}
	}
	for value := range labels["window"] {
		if value != "quality" && value != "speed" && value != "limited" && value != "auto" {
			t.Errorf("window label %q is outside the fixed vocabulary", value)
		}
	}
	for _, values := range []struct {
		label string
		want  []string
	}{
		{label: "ip", want: []string{"dualstack", "ipv4"}},
		{label: "protocol", want: []string{"dualproto", "tcp", "udp", "icmp"}},
		{label: "ip_version", want: []string{"4", "6"}},
	} {
		for _, value := range values.want {
			if !labels[values.label][value] {
				t.Errorf("no series has %s %q", values.label, value)
			}
		}
	}
}

// The process registers its collector, and every device the default manager
// settings open counts into the process instance it exports.
func TestProxyDevicesCountIntoTheProcessWindowClientStats(t *testing.T) {
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	registered := false
	for _, family := range families {
		if family.GetName() == "urnetwork_proxy_window_candidates_total" {
			registered = true
		}
	}
	if !registered {
		t.Fatal("the process does not export the window client counters")
	}

	if DefaultProxyDeviceManagerSettings().WindowClientStats != defaultWindowClientStats {
		t.Fatal("the default manager settings do not carry the process's window client counters")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := budgetTestManager(t, ctx)
	defer manager.CloseAndWait(context.Background())
	settings := manager.proxyDeviceSettings()
	if settings.windowClientStats != defaultWindowClientStats {
		t.Fatal("the manager does not open its devices with the process's window client counters")
	}
	settings.DisableWindowIdentityPersistence = true
	deviceLocalSettings := newProxyDeviceLocalSettings(ctx, &model.ProxyDeviceConfig{}, settings)
	if deviceLocalSettings.WindowClientStats != defaultWindowClientStats {
		t.Fatal("a hosted device's settings do not carry the process's window client counters")
	}
}
