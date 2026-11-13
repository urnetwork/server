// The fixed-suite attestor permits exactly the two selected phases; old or
// arbitrary member names cannot reach dispatch or gain another test execution.
package main

import (
	"context"
	"testing"
)

// Unsupported phases are rejected before any callback or new journal leaf.
func TestSourceTestSnowTwoMemberJournalRejectsRemovedAndUnknownPhases(t *testing.T) {
	for _, phase := range []string{"snow-muse", "snow-synthetic-unapproved", "invocation", "completion"} {
		for _, directory := range []string{"", t.TempDir()} {
			settings := defaultSourceTestModelSettings()
			provider := defaultWinnerSnowProviderSettings()
			settings.SnowProvider, settings.ResponseJournalDirectory = &provider, directory
			calls := 0
			_, err := journalSourceTestProviderStream(context.Background(), phase, []byte("synthetic request"), settings, func() ([]byte, error) {
				calls++
				return []byte("synthetic response"), nil
			})
			if err == nil || calls != 0 {
				t.Errorf("removed or unrelated phase %s reached provider dispatch", phase)
			}
		}
	}
}
