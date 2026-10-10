package connect

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	connectcore "github.com/urnetwork/connect/v2026"
)

func ownerMetricFamilies(t *testing.T, registry *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*dto.MetricFamily{}
	for _, family := range families {
		out[strings.TrimPrefix(family.GetName(), "urnetwork_connect_transfer_owner_")] = family
	}
	return out
}

func ownerMetricValue(t *testing.T, families map[string]*dto.MetricFamily, name string, labels ...string) float64 {
	t.Helper()
	for _, metric := range families[name].GetMetric() {
		if len(metric.Label) != len(labels)/2 {
			continue
		}
		matches := true
		for i := 0; i < len(labels); i += 2 {
			found := false
			for _, pair := range metric.Label {
				found = found || pair.GetName() == labels[i] && pair.GetValue() == labels[i+1]
			}
			matches = matches && found
		}
		if matches {
			if metric.Gauge != nil {
				return metric.Gauge.GetValue()
			}
			return metric.Counter.GetValue()
		}
	}
	t.Fatalf("missing fixed metric %s %v", name, labels)
	return 0
}

func TestTransferMemoryOwnerMetricsCoherentAndUnavailable(t *testing.T) {
	healthy := connectcore.TransferMemoryOwnerSnapshot{
		Enabled: true, Complete: true,
		Send: connectcore.TransferMemoryOwnerGroup{
			Workers: 3, CleanupWorkers: 1, KnownSequenceStructBytes: 192,
			KnownChannelSlotBytes: 4096, CleanupKnownChannelSlotBytes: 1024,
			AdmittedTotal: 8, FinishedTotal: 5,
		},
		Receive: connectcore.TransferMemoryOwnerGroup{Workers: 1, KnownSequenceStructBytes: 64, KnownChannelSlotBytes: 128, AdmittedTotal: 1},
	}
	sample, calls := healthy, 0
	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newTransferMemoryOwnerCollector(func() connectcore.TransferMemoryOwnerSnapshot {
		calls++
		return sample
	}))
	families := ownerMetricFamilies(t, registry)
	if calls != 1 || len(families) != 8 {
		t.Fatalf("snapshot calls=%d metric families=%d", calls, len(families))
	}
	series := 0
	for _, family := range families {
		series += len(family.Metric)
		for _, metric := range family.Metric {
			for _, pair := range metric.Label {
				allowed := map[string]string{"kind": "|send|receive|forward|", "phase": "|running|cleanup|", "state": "|disabled|overlap|invalid|complete|"}
				if !strings.Contains(allowed[pair.GetName()], "|"+pair.GetValue()+"|") {
					t.Fatal("nonfinite or identity-bearing metric label")
				}
			}
		}
	}
	if series != 27 {
		t.Fatalf("series=%d, want fixed 27", series)
	}
	for _, row := range []struct {
		name   string
		labels []string
		want   float64
	}{
		{"enabled", nil, 1}, {"sample_complete", nil, 1},
		{"workers", []string{"kind", "send", "phase", "running"}, 2},
		{"workers", []string{"kind", "send", "phase", "cleanup"}, 1},
		{"known_channel_slot_bytes", []string{"kind", "send", "phase", "running"}, 3072},
		{"known_channel_slot_bytes", []string{"kind", "send", "phase", "cleanup"}, 1024},
		{"known_sequence_struct_bytes", []string{"kind", "send"}, 192},
		{"admitted_total", []string{"kind", "send"}, 8},
		{"finished_total", []string{"kind", "send"}, 5},
		{"workers", []string{"kind", "forward", "phase", "running"}, 0},
	} {
		if got := ownerMetricValue(t, families, row.name, row.labels...); got != row.want {
			t.Fatalf("%s=%g want %g", row.name, got, row.want)
		}
	}
	for _, row := range []struct {
		name, state string
		change      func(*connectcore.TransferMemoryOwnerSnapshot)
	}{
		{"disabled", "disabled", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Enabled = false }},
		{"overlap", "overlap", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Complete = false }},
		{"negative", "invalid", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Send.Workers = -1 }},
		{"cleanup-workers", "invalid", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Send.CleanupWorkers = 4 }},
		{"cleanup-bytes", "invalid", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Send.CleanupKnownChannelSlotBytes = 4097 }},
		{"finished-over-admitted", "invalid", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Send.FinishedTotal = 9 }},
		{"conservation", "invalid", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Send.AdmittedTotal = 7 }},
		{"imprecise-bytes", "invalid", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Send.KnownSequenceStructBytes = 1<<53 + 1 }},
		{"imprecise-total", "invalid", func(s *connectcore.TransferMemoryOwnerSnapshot) { s.Send.AdmittedTotal = 1<<53 + 1 }},
	} {
		t.Run(row.name, func(t *testing.T) {
			sample = healthy
			row.change(&sample)
			before := calls
			families := ownerMetricFamilies(t, registry)
			if calls != before+1 || len(families) != 3 {
				t.Fatalf("unavailable sample retained counts/bytes or retried: calls=%d families=%d", calls-before, len(families))
			}
			if ownerMetricValue(t, families, "sample_complete") != 0 || ownerMetricValue(t, families, "sample_state", "state", row.state) != 1 {
				t.Fatal("unavailable sample lost its finite cause")
			}
		})
	}
}

func TestTransferMemoryOwnerMetricsRegistrationScope(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	stop, err := registerTransferMemoryOwnerMetrics(registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if len(ownerMetricFamilies(t, registry)) != 0 {
		t.Fatal("default-off registered an owner census")
	}
	var ledger connectcore.TransferMemoryOwnerLedger
	stop, err = registerTransferMemoryOwnerMetrics(registry, &ledger)
	if err != nil {
		t.Fatal(err)
	}
	if ownerMetricValue(t, ownerMetricFamilies(t, registry), "sample_complete") != 1 {
		t.Fatal("enabled empty scope unavailable")
	}
	if _, err = registerTransferMemoryOwnerMetrics(registry, &connectcore.TransferMemoryOwnerLedger{}); err == nil {
		t.Fatal("multiple ledgers silently published under one process metric scope")
	}
	stop()
	if len(ownerMetricFamilies(t, registry)) != 0 {
		t.Fatal("collector survived its owner")
	}
	stop, err = registerTransferMemoryOwnerMetrics(registry, &connectcore.TransferMemoryOwnerLedger{})
	if err != nil {
		t.Fatal(err)
	}
	stop()
}

func TestTransferMemoryOwnerRunSettingsCapture(t *testing.T) {
	if exchangeSettingsForRun(RunOptions{}).MemoryOwnerLedger != nil {
		t.Fatal("default run enables owner accounting")
	}
	settings := exchangeSettingsForRun(RunOptions{MemoryOwnerLedger: true})
	ledger := settings.MemoryOwnerLedger
	if ledger == nil || ledger == exchangeSettingsForRun(RunOptions{MemoryOwnerLedger: true}).MemoryOwnerLedger {
		t.Fatal("enabled runs did not get independent scopes")
	}
	settings.KeyEventDelivery.Enabled = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exchange := NewExchange(ctx, "synthetic", "connect", "synthetic", nil, nil, settings)
	defer exchange.Close()
	settings.MemoryOwnerLedger = &connectcore.TransferMemoryOwnerLedger{}
	first, second := exchange.residentClientSettings(), exchange.residentClientSettings()
	if first.MemoryOwnerLedger != ledger || second.MemoryOwnerLedger != ledger || first == second {
		t.Fatal("residents did not retain the captured Exchange ledger with independent settings")
	}
}
