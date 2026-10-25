package work

import (
	"os"
	"strings"
	"testing"
)

// The location re-index is one transaction of up to hours, so it replaces the
// main pool's 2 s lock bound with its own (see indexSearchLocationsLockTimeout).
func TestIndexSearchLocationsKeepsItsLockBound(t *testing.T) {
	source, err := os.ReadFile("network_client_location_search_work.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "}, server.TxLockTimeout(indexSearchLocationsLockTimeout))") {
		t.Fatal("IndexSearchLocations lost its lock bound")
	}
	if indexSearchLocationsLockTimeout <= 2e9 {
		t.Fatal("the re-index's lock bound is not longer than the main pool's")
	}
}
