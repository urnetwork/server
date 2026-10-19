package connect

import (
	"github.com/prometheus/client_golang/prometheus"
	connectcore "github.com/urnetwork/connect/v2026"
)

// The exchange captures its scope before creating residents. Mutating a caller's
// settings later cannot move only some of its clients into another ledger.
func (self *Exchange) residentClientSettings() *connectcore.ClientSettings {
	settings := connectcore.DefaultClientSettingsWithBufferSize(self.settings.ExchangeBufferSize)
	settings.MemoryOwnerLedger = self.memoryOwnerLedger
	settings.PayloadOwnerLedger = self.sdkPayloadOwnerLedger
	return settings
}

// This collector does one fixed-size ledger snapshot. It never traverses
// residents, payloads, channels, goroutines, allocation profiles, or the heap.
// Existing process-stamped metrics transport supplies the identity and time;
// these metrics do not add a listener or a separate sampling loop.
type transferMemoryOwnerCollector struct {
	snapshot                                                              func() connectcore.TransferMemoryOwnerSnapshot
	enabled, complete, state, workers, slots, structs, admitted, finished *prometheus.Desc
}

func newTransferMemoryOwnerCollector(snapshot func() connectcore.TransferMemoryOwnerSnapshot) *transferMemoryOwnerCollector {
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		return prometheus.NewDesc("urnetwork_connect_transfer_owner_"+name, help, labels, nil)
	}
	return &transferMemoryOwnerCollector{
		snapshot: snapshot,
		enabled:  desc("enabled", "Whether this resident SDK transfer ledger is enabled."),
		complete: desc("sample_complete", "Whether this collection contains a coherent registered-worker sample; not a full process or heap census."),
		state:    desc("sample_state", "Finite validity state of this collection; one state is one.", "state"),
		workers:  desc("workers", "Buffer-admitted SDK workers through joined cleanup; running includes owners whose Run has not returned, even if canceled.", "kind", "phase"),
		slots:    desc("known_channel_slot_bytes", "Channel element storage owned by registered workers; excludes payload backing, headers and allocator overhead.", "kind", "phase"),
		structs:  desc("known_sequence_struct_bytes", "Sequence struct sizes times registered workers; excludes referenced allocations.", "kind"),
		admitted: desc("admitted_total", "SDK transfer workers admitted into this process ledger.", "kind"),
		finished: desc("finished_total", "SDK transfer workers whose outer cleanup has finished.", "kind"),
	}
}

func (c *transferMemoryOwnerCollector) Describe(out chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{c.enabled, c.complete, c.state, c.workers, c.slots, c.structs, c.admitted, c.finished} {
		out <- desc
	}
}

func validTransferMemoryOwnerGroup(g connectcore.TransferMemoryOwnerGroup) bool {
	// Prometheus values are float64. Refuse an incoherent or no-longer-exact
	// sample instead of publishing clamped counts or rounded conservation.
	const maxExact = 1 << 53
	for _, value := range [...]int64{g.Workers, g.CleanupWorkers, g.KnownSequenceStructBytes, g.KnownChannelSlotBytes, g.CleanupKnownChannelSlotBytes} {
		if value < 0 || value > maxExact {
			return false
		}
	}
	return g.CleanupWorkers <= g.Workers && g.CleanupKnownChannelSlotBytes <= g.KnownChannelSlotBytes &&
		g.AdmittedTotal <= maxExact && g.FinishedTotal <= g.AdmittedTotal &&
		g.AdmittedTotal-g.FinishedTotal == uint64(g.Workers)
}

func (c *transferMemoryOwnerCollector) Collect(out chan<- prometheus.Metric) {
	snapshot := c.snapshot()
	state := "complete"
	switch {
	case !snapshot.Enabled:
		state = "disabled"
	case !snapshot.Complete:
		state = "overlap"
	case !validTransferMemoryOwnerGroup(snapshot.Send) || !validTransferMemoryOwnerGroup(snapshot.Receive) || !validTransferMemoryOwnerGroup(snapshot.Forward):
		state = "invalid"
	}
	boolValue := func(value bool) float64 {
		if value {
			return 1
		}
		return 0
	}
	out <- prometheus.MustNewConstMetric(c.enabled, prometheus.GaugeValue, boolValue(snapshot.Enabled))
	out <- prometheus.MustNewConstMetric(c.complete, prometheus.GaugeValue, boolValue(state == "complete"))
	for _, candidate := range [...]string{"disabled", "overlap", "invalid", "complete"} {
		out <- prometheus.MustNewConstMetric(c.state, prometheus.GaugeValue, boolValue(candidate == state), candidate)
	}
	if state != "complete" {
		// Never publish false zeros or retain our last coherent sample. A reader
		// must require a fresh complete sample from the same process/start.
		return
	}
	for _, row := range [...]struct {
		kind  string
		group connectcore.TransferMemoryOwnerGroup
	}{{"send", snapshot.Send}, {"receive", snapshot.Receive}, {"forward", snapshot.Forward}} {
		g := row.group
		out <- prometheus.MustNewConstMetric(c.workers, prometheus.GaugeValue, float64(g.Workers-g.CleanupWorkers), row.kind, "running")
		out <- prometheus.MustNewConstMetric(c.workers, prometheus.GaugeValue, float64(g.CleanupWorkers), row.kind, "cleanup")
		out <- prometheus.MustNewConstMetric(c.slots, prometheus.GaugeValue, float64(g.KnownChannelSlotBytes-g.CleanupKnownChannelSlotBytes), row.kind, "running")
		out <- prometheus.MustNewConstMetric(c.slots, prometheus.GaugeValue, float64(g.CleanupKnownChannelSlotBytes), row.kind, "cleanup")
		out <- prometheus.MustNewConstMetric(c.structs, prometheus.GaugeValue, float64(g.KnownSequenceStructBytes), row.kind)
		out <- prometheus.MustNewConstMetric(c.admitted, prometheus.CounterValue, float64(g.AdmittedTotal), row.kind)
		out <- prometheus.MustNewConstMetric(c.finished, prometheus.CounterValue, float64(g.FinishedTotal), row.kind)
	}
}

func registerTransferMemoryOwnerMetrics(registry prometheus.Registerer, ledger *connectcore.TransferMemoryOwnerLedger) (func(), error) {
	if ledger == nil {
		// Default-off adds no collector. Absence is not a zero-owner census.
		return func() {}, nil
	}
	collector := newTransferMemoryOwnerCollector(ledger.Snapshot)
	if err := registry.Register(collector); err != nil {
		return nil, err
	}
	return func() { registry.Unregister(collector) }, nil
}
