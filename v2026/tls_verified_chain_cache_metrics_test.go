package server

import (
	"reflect"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// The collector reports connect's process verified-chain cache under the
// names the signals dashboard reads, and passes a pedantic registry.
func TestTlsVerifiedChainCacheCollectorReportsConnectCounts(t *testing.T) {
	registry := prometheus.NewPedanticRegistry()
	if err := registry.Register(newTlsVerifiedChainCacheCollector()); err != nil {
		t.Fatal(err)
	}
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seriesCounts := map[string]int{}
	for _, family := range families {
		seriesCounts[family.GetName()] = len(family.GetMetric())
	}
	want := map[string]int{
		"urnetwork_tls_verified_chain_cache_lookups_total":   2,
		"urnetwork_tls_verified_chain_cache_stores_total":    1,
		"urnetwork_tls_verified_chain_cache_evictions_total": 1,
		"urnetwork_tls_verified_chain_cache_entries":         1,
	}
	if !reflect.DeepEqual(seriesCounts, want) {
		t.Fatalf("series = %v, want %v", seriesCounts, want)
	}
}
