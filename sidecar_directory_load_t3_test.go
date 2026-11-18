//go:build sidecar_localstack && sidecar_testhooks

// FS-86's card commits on T3 (§22.4, §22.8, D-TEST-17; §16.11's cards ref commits row, D-MCP-31; RECORD-M10-07): the
// schedule sidecartest/fault exports (fs86_cards_evs.go), run on T3's Net in physical time, where sidecart3 names
// evshost's block as the evs world's instance (sidecart3/evs_tier.go). So the team directory's repo and its one cards
// ref's head stand on the private Redis and the local stack's MinIO, and 16 devices' card writes land on that head
// through the handler server runs, 30 s at a column's peak rate, the peak hour at reduced scale, and then 15 s at three
// times it; a reader then finds every card as its last push left it.
//
//	Team limit   2.3 pushes a second, then 6.9: held to the row's ceilings, since a miss at the Team limit blocks the
//	             gate. At the peak p99 <= 300 ms from arrival to the write's return, at most 0.5 412 reruns a commit on
//	             average and no 429 BUSY; at the burst p99 <= 1 s, at most 1 rerun on average, BUSY on at most 0.1% of
//	             pushes, each committing within 5 s; no FOLD_BEHIND in either
//	stress case  5.8 pushes a second, then 17.4, the load the row's trap names: every push commits and the reader
//	             reads them, and each miss of the same ceilings is logged for the record and fails nothing
//
// The stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it. The local stack is one
// MinIO in Docker on the machine that runs the tests, so what these measure is that machine under whatever else it
// runs, and each run logs what a commit took alone beside its phases.
//
// Not here, and so not run on T3: the day's readers against the golden schedule, the three mutants and the table's
// byte rows, which run on T2 over sidecartest's load (sidecar's fs86_cards_test.go and directory_cost_cards_test.go);
// format 2, so no push here carries an index entry or writes an index head; the year-one mix beside the directory's
// pushes; a whole hour and the 26 h run; the governor's FI-14; and the 4-host file-cluster layouts, whose drives are
// the ones the NVMe rows price.

package server_test

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// One run of the card commits' schedule under settings on T3's Net: its state once every step ran, and how it ended.
func sidecarDirectoryLoadT3Run(tb testing.TB, settings *fault.Fs86CardsEvsSettings) (*fault.Fs86CardsEvs, error) {
	ctx := tb.Context()
	// a phase is one step that takes its whole duration in real time, its actor waiting on the wall clock between
	// arrivals: the driver's wait covers both phases twice over, and a waiting actor is not a stalled one
	netSettings := sidecart3.NetSettings(tb, nil)
	barriers := *netSettings.Barriers
	barriers.AwaitTimeout = 2 * (settings.PeakDuration + settings.BurstDuration)
	barriers.StallTimeout = 0
	netSettings.Barriers = &barriers
	net := sidecartest.NewNet(ctx, netSettings)
	defer net.Close()
	schedule, state := fault.Fs86CardsEvsCommitsOf(net, settings)
	result := fault.Run(ctx, schedule, net)
	for _, phase := range state.Phases() {
		tb.Log(phase)
	}
	return state, result.Err
}

func TestFs86CardCommitsAtTheTeamLimitOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		state, err := sidecarDirectoryLoadT3Run(tb, fault.DefaultFs86CardsEvsSettings())
		if err != nil {
			tb.Fatal(err)
		}
		if phases := state.Phases(); len(phases) != 3 {
			tb.Fatalf("%d phases ran", len(phases))
		}
	})
}

func TestFs86CardCommitsAtTheStressCaseOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		settings := fault.DefaultFs86CardsEvsSettings()
		// §16.11's 5,000-member column: 5.8 pushes a second on the cards ref at its peak
		settings.PushesPerSecond = 5.8
		ceilings := settings.Ceilings
		settings.Ceilings = nil
		state, err := sidecarDirectoryLoadT3Run(tb, settings)
		if err != nil {
			tb.Fatal(err)
		}
		if phases := state.Phases(); len(phases) != 3 {
			tb.Fatalf("%d phases ran", len(phases))
		}
		for _, miss := range state.Misses(ceilings) {
			tb.Logf("the stress case, recorded and not gating: %s", miss)
		}
	})
}
