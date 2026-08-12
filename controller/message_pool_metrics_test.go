package controller

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"
)

// The connect library's per-tag `pool[...] r=/t=/c=` line was the largest
// single log source in the connect and api services and is now behind the
// library logger's V(1). Silencing it is only safe because these collectors
// carry the aggregate that the leak signal depends on, so this test pins that
// they exist and track the library's counters. If it fails, pool leaks have
// gone dark rather than merely quiet.
func TestMessagePoolMetricsTrackLibraryCounters(t *testing.T) {
	names := []string{
		"urnetwork_message_pool_taken_total",
		"urnetwork_message_pool_returned_total",
		"urnetwork_message_pool_created_total",
		"urnetwork_message_pool_unpooled_taken_total",
		"urnetwork_message_pool_unpooled_bytes_total",
	}
	for _, name := range names {
		if count := testutil.CollectAndCount(prometheus.DefaultGatherer.(prometheus.Collector), name); count != 1 {
			t.Fatalf("%s is not registered exactly once (got %d)", name, count)
		}
	}

	// take a buffer and confirm the exported taken counter advances with the
	// library's own accounting, so the collector reads live state rather than
	// a value captured at registration
	takenBefore, _, _ := connect.MessagePoolCounts()
	exportedBefore, err := gatherCounter(t, "urnetwork_message_pool_taken_total")
	if err != nil {
		t.Fatal(err)
	}
	if exportedBefore != float64(takenBefore) {
		t.Fatalf("exported taken %v does not match library %v", exportedBefore, takenBefore)
	}

	buffer := connect.MessagePoolGet(1024)
	connect.MessagePoolReturn(buffer)

	takenAfter, _, _ := connect.MessagePoolCounts()
	if takenAfter <= takenBefore {
		t.Skip("library taken counter did not advance; nothing to compare")
	}
	exportedAfter, err := gatherCounter(t, "urnetwork_message_pool_taken_total")
	if err != nil {
		t.Fatal(err)
	}
	if exportedAfter <= exportedBefore {
		t.Fatalf("exported taken did not advance with the library: %v -> %v", exportedBefore, exportedAfter)
	}
}

func gatherCounter(t testing.TB, name string) (float64, error) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, err
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			return metric.GetCounter().GetValue(), nil
		}
	}
	return 0, errNotGathered(name)
}

type errNotGathered string

func (self errNotGathered) Error() string {
	return strings.Join([]string{"metric not gathered:", string(self)}, " ")
}
