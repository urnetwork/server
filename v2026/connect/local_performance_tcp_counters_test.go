package connect_test

import (
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
)

type localPerformanceTCPCounters struct {
	retransmits, timeouts, fastRetransmit, sackRecovery, sendErrors uint64
}

// Stack statistics contain live counter pointers. Keep values at each sample
// boundary so later increments cannot change a previously captured baseline.
func snapshotLocalPerformanceTCPCounters(stats tcpip.TCPStats) localPerformanceTCPCounters {
	return localPerformanceTCPCounters{
		retransmits:    stats.Retransmits.Value(),
		timeouts:       stats.Timeouts.Value(),
		fastRetransmit: stats.FastRetransmit.Value(),
		sackRecovery:   stats.SACKRecovery.Value(),
		sendErrors:     stats.SegmentSendErrors.Value(),
	}
}

func (current localPerformanceTCPCounters) since(previous localPerformanceTCPCounters) localPerformanceTCPCounters {
	return localPerformanceTCPCounters{
		retransmits:    current.retransmits - previous.retransmits,
		timeouts:       current.timeouts - previous.timeouts,
		fastRetransmit: current.fastRetransmit - previous.fastRetransmit,
		sackRecovery:   current.sackRecovery - previous.sackRecovery,
		sendErrors:     current.sendErrors - previous.sendErrors,
	}
}

func TestLocalPerformanceTCPCounterSnapshots(t *testing.T) {
	stats := tcpip.Stats{}.FillIn().TCP
	before := snapshotLocalPerformanceTCPCounters(stats)
	stats.Retransmits.IncrementBy(2)
	stats.Timeouts.IncrementBy(3)
	stats.FastRetransmit.IncrementBy(5)
	stats.SACKRecovery.IncrementBy(7)
	stats.SegmentSendErrors.IncrementBy(11)
	after := snapshotLocalPerformanceTCPCounters(stats)
	wantFirst := localPerformanceTCPCounters{2, 3, 5, 7, 11}
	if got := after.since(before); got != wantFirst {
		t.Fatalf("first interval = %+v, want %+v", got, wantFirst)
	}

	// These are the same live counters returned by subsequent Stats calls.
	// Advancing them must change neither previously captured endpoint.
	stats.Retransmits.IncrementBy(13)
	stats.Timeouts.IncrementBy(17)
	stats.FastRetransmit.IncrementBy(19)
	stats.SACKRecovery.IncrementBy(23)
	stats.SegmentSendErrors.IncrementBy(29)
	if got := after.since(before); got != wantFirst {
		t.Fatalf("later live updates changed the first interval: %+v", got)
	}
	latest := snapshotLocalPerformanceTCPCounters(stats)
	if got, want := latest.since(after), (localPerformanceTCPCounters{13, 17, 19, 23, 29}); got != want {
		t.Fatalf("second interval = %+v, want %+v", got, want)
	}
	if got := snapshotLocalPerformanceTCPCounters(stats).since(latest); got != (localPerformanceTCPCounters{}) {
		t.Fatalf("quiet interval = %+v, want zero", got)
	}
}
