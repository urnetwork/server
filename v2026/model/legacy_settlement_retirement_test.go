// The legacy settlement queue is retired (PARTITION-MAINTENANCE-PLAN.md, Part
// B). No model source reads or writes its table, so a later release can drop
// the table without leaving a reader behind.
package model

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelSourcesDoNotReferenceRetiredLegacySettlementQueue(t *testing.T) {
	paths, err := filepath.Glob("*.go")
	if err != nil || len(paths) == 0 {
		t.Fatal("model sources not found", err)
	}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(source), "legacy_settlement_intent") {
			t.Errorf("%s still references the retired legacy_settlement_intent table", path)
		}
	}
}
