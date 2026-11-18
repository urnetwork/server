//go:build sidecar_localstack && sidecar_testhooks

// FS-86's card commits on T3's local stack (§22.4, §22.8, D-TEST-17; §16.11's cards ref commits row, D-MCP-31;
// RECORD-M10-07): the schedule sidecartest/fault exports (fs86_cards_evs.go), run on T3's Net in physical time, where
// sidecart3 names evshost's block as the evs world's instance (sidecart3/evs_tier.go). So the team directory's repo and
// its one cards ref's head stand on the private Redis and the local stack's MinIO, and 16 devices' card writes land on
// that head through the handler server runs, 30 s at a column's peak rate, the peak hour at reduced scale, and then
// 15 s at three times it; a reader then finds every card as its last push left it.
//
//	Team limit   2.3 pushes a second, then 6.9
//	stress case  5.8 pushes a second, then 17.4, the load the row's trap names
//
// Neither is the row's reading (QD-1068): the local stack is one MinIO in Docker on the machine that runs the tests, so
// what these measure is that machine under whatever else it runs. Every push must commit and the reader must read
// them, and each miss of the row's ceilings is logged for the record and fails nothing: at the peak p99 <= 300 ms from
// arrival to the write's return, at most 0.5 412 reruns a commit on average and no 429 BUSY; at the burst p99 <= 1 s,
// at most 1 rerun on average, BUSY on at most 0.1% of pushes, each committing within 5 s; no FOLD_BEHIND in either.
// Each run logs what a commit took alone beside its phases. The reading is nightly's, a whole peak hour at the Team
// limit's rate on T3's 4-host file-cluster layouts (sidecar_directory_load_layout_t3_test.go), held to the row.
//
// The stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it.
//
// Not here, and so not run on T3: the day's readers against the golden schedule, the three mutants and the table's
// byte rows, which run on T2 over sidecartest's load (sidecar's fs86_cards_test.go and directory_cost_cards_test.go);
// format 2, so no push here carries an index entry or writes an index head; the year-one mix beside the directory's
// pushes; the 26 h run; and the governor's FI-14.

package server_test

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// One run of the card commits' schedule on the local stack at rate pushes a second, its misses logged as no gate's.
func sidecarDirectoryLoadLocalStackRun(tb testing.TB, name string, rate float64) {
	settings := fault.DefaultFs86CardsEvsSettings()
	settings.PushesPerSecond = rate
	ceilings := settings.Ceilings
	settings.Ceilings = nil
	state, err := fault.RunFs86CardsEvs(tb, sidecart3.NetSettings(tb, nil), settings)
	if err != nil {
		tb.Fatal(err)
	}
	if phases := state.Phases(); len(phases) != 3 {
		tb.Fatalf("%d phases ran", len(phases))
	}
	for _, miss := range state.Misses(ceilings) {
		tb.Logf("%s on the local stack, recorded and not gating: %s", name, miss)
	}
}

func TestFs86CardCommitsAtTheTeamLimitOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		// §16.11's 2,000-member column: 2.3 pushes a second on the cards ref at its peak
		sidecarDirectoryLoadLocalStackRun(tb, "the Team limit", 2.3)
	})
}

func TestFs86CardCommitsAtTheStressCaseOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		// §16.11's 5,000-member column: 5.8 pushes a second on the cards ref at its peak
		sidecarDirectoryLoadLocalStackRun(tb, "the stress case", 5.8)
	})
}
