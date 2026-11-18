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
// These are the reduced-scale form, and neither is the row's reading (QD-1068; owner decision 598): the local stack is
// one MinIO in Docker on the machine that runs the tests, so the latencies they measure are that machine's under
// whatever else it runs. The row: at the peak p99 <= 300 ms from arrival to the write's return, at most 0.5 412 reruns
// a commit on average and no 429 BUSY; at the burst p99 <= 1 s, at most 1 rerun on average, BUSY on at most 0.1% of
// pushes, each committing within 5 s; no FOLD_BEHIND in either. Every push must commit and the reader must read them.
//
//	Team limit   a p99 miss is logged with its numbers and the machine's load average and fails nothing; the 412
//	             rerun, 429 BUSY and FOLD_BEHIND ceilings still hold, and a miss of one fails the test
//	stress case  no gate's, so every miss is logged so and none fails
//
// Each run logs what a commit took alone beside its phases (go test -v shows them on a pass). The reading is nightly's,
// a whole peak hour at the Team limit's rate on T3's 4-host file-cluster layouts
// (sidecar_directory_load_layout_t3_test.go), held to the whole row.
//
// The stack must be up (sidecar/local/stack.sh), and these fail rather than skip without it.
//
// Not here, and so not run on T3: the day's readers against the golden schedule, the three mutants and the table's
// byte rows, which run on T2 over sidecartest's load (sidecar's fs86_cards_test.go and directory_cost_cards_test.go);
// format 2, so no push here carries an index entry or writes an index head; the year-one mix beside the directory's
// pushes; the 26 h run; and the governor's FI-14.

package server_test

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// One run of the card commits' schedule on the local stack at rate pushes a second, judged against the row once it ends
// (sidecarDirectoryLoadJudge), held to held of it, nil for none.
func sidecarDirectoryLoadLocalStackRun(tb testing.TB, name string, rate float64, held *sidecartest.DirectoryLoadCeilings) {
	settings := fault.DefaultFs86CardsEvsSettings()
	settings.PushesPerSecond = rate
	ceilings := settings.Ceilings
	// the schedule holds nothing itself, so each miss is judged here
	settings.Ceilings = nil
	startLoad := sidecarDirectoryLoadMachineAverage()
	state, err := fault.RunFs86CardsEvs(tb, sidecart3.NetSettings(tb, nil), settings)
	where := fmt.Sprintf("%s on the local stack (load average %s at the start, %s at the end)", name, startLoad, sidecarDirectoryLoadMachineAverage())
	sidecarDirectoryLoadJudge(tb, where, state, ceilings, held)
	if err != nil {
		tb.Fatalf("%s: %v", where, err)
	}
	if phases := state.Phases(); len(phases) != 3 {
		tb.Fatalf("%s: %d phases ran", where, len(phases))
	}
}

// A run's phases against ceilings, the row, as the local stack's runs are judged: each miss of held, the part of the row
// the run holds, nil for none, fails tb, and each other miss of ceilings is logged for the record and fails nothing.
// Each line begins with where, which names the run and the machine's load average. A miss of a ceiling held and
// ceilings share reads alike under both, so it is held and not logged again.
func sidecarDirectoryLoadJudge(tb testing.TB, where string, state *fault.Fs86CardsEvs, ceilings *sidecartest.DirectoryLoadCeilings, held *sidecartest.DirectoryLoadCeilings) {
	heldMisses := map[string]bool{}
	if held != nil {
		for _, miss := range state.Misses(held) {
			heldMisses[miss.Error()] = true
			tb.Errorf("%s: %s", where, miss)
		}
	}
	for _, miss := range state.Misses(ceilings) {
		if !heldMisses[miss.Error()] {
			tb.Logf("%s, recorded and not gating: %s", where, miss)
		}
	}
}

// What of ceilings, the row, the Team limit's run on the local stack holds: all of it but the p99s, which a loaded
// machine's latencies miss (QD-1068; owner decision 598). So its 412 rerun, 429 BUSY and FOLD_BEHIND ceilings hold.
func sidecarDirectoryLoadLocalStackHeld(ceilings *sidecartest.DirectoryLoadCeilings) *sidecartest.DirectoryLoadCeilings {
	held := *ceilings
	held.CardPushP99 = time.Duration(math.MaxInt64)
	held.CardBurstPushP99 = time.Duration(math.MaxInt64)
	return &held
}

func TestFs86CardCommitsAtTheTeamLimitOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		// §16.11's 2,000-member column: 2.3 pushes a second on the cards ref at its peak
		sidecarDirectoryLoadLocalStackRun(tb, "the Team limit", 2.3, sidecarDirectoryLoadLocalStackHeld(sidecartest.DefaultDirectoryLoadCeilings()))
	})
}

func TestFs86CardCommitsAtTheStressCaseOnT3(t *testing.T) {
	sidecarDirectoryT3TestEnv().Run(t, func(tb testing.TB) {
		// §16.11's 5,000-member column: 5.8 pushes a second on the cards ref at its peak, held to nothing
		sidecarDirectoryLoadLocalStackRun(tb, "the stress case", 5.8, nil)
	})
}

// A testing.TB that keeps what a judge reports in place of failing or logging.
type sidecarDirectoryLoadJudgeRecorder struct {
	testing.TB
	errorLines []string
	logLines   []string
}

func (self *sidecarDirectoryLoadJudgeRecorder) Errorf(format string, args ...any) {
	self.errorLines = append(self.errorLines, fmt.Sprintf(format, args...))
}

func (self *sidecarDirectoryLoadJudgeRecorder) Logf(format string, args ...any) {
	self.logLines = append(self.logLines, fmt.Sprintf(format, args...))
}

// The local stack's judging on T2's fakes, where the schedule runs in a second and a half on the real engine and the
// real client's binding, against a row whose p99 and 412 rerun ceilings every phase misses and whose BUSY ceilings none
// can: held as the Team limit is, each rerun miss fails the run and each p99 miss is logged and fails nothing; held to
// nothing, as the stress case is, every miss is logged and none fails. It needs no store, and runs wherever the
// local stack's tests build.
func TestFs86CardCommitsOnTheLocalStackLogTheirP99MissesAndHoldTheRest(t *testing.T) {
	netSettings := sidecartest.DefaultNetSettings()
	// a phase is one step that waits on the wall clock between arrivals: a minute is far past what a loaded machine takes
	netSettings.Barriers.AwaitTimeout = time.Minute
	settings := fault.DefaultFs86CardsEvsSettings()
	settings.WriterCount = 4
	settings.PushesPerSecond = 40
	settings.PeakDuration = time.Second
	settings.BurstFactor = 2
	settings.BurstDuration = 500 * time.Millisecond
	settings.Ceilings = nil
	state, err := fault.RunFs86CardsEvs(t, netSettings, settings)
	if err != nil {
		t.Fatal(err)
	}
	phases := state.Phases()
	if len(phases) != 3 {
		t.Fatalf("%d phases ran", len(phases))
	}
	for _, phase := range phases {
		if len(phase.Latencies) == 0 || phase.FoldBehindCount != 0 {
			t.Fatalf("%s", phase)
		}
	}
	ceilings := sidecartest.DefaultDirectoryLoadCeilings()
	ceilings.CardPushP99 = time.Nanosecond
	ceilings.CardBurstPushP99 = time.Nanosecond
	ceilings.CardRerunMean = -1
	ceilings.CardBurstRerunMean = -1
	ceilings.CardBusyShare = math.Inf(1)
	ceilings.CardBurstBusyShare = math.Inf(1)
	ceilings.CardBurstRetryCommit = time.Duration(math.MaxInt64)
	// the lines that name about
	count := func(lines []string, about string) int {
		found := 0
		for _, line := range lines {
			if strings.Contains(line, about) {
				found += 1
			}
		}
		return found
	}

	// the peak's and the burst's: each rerun miss held, each p99 miss logged
	teamLimit := &sidecarDirectoryLoadJudgeRecorder{TB: t}
	sidecarDirectoryLoadJudge(teamLimit, "the Team limit", state, ceilings, sidecarDirectoryLoadLocalStackHeld(ceilings))
	if len(teamLimit.errorLines) != 2 || count(teamLimit.errorLines, "rerun") != 2 || count(teamLimit.errorLines, "p99") != 0 {
		t.Fatalf("the Team limit's held misses: %q", teamLimit.errorLines)
	}
	if len(teamLimit.logLines) != 2 || count(teamLimit.logLines, "p99") != 2 || count(teamLimit.logLines, "recorded and not gating") != 2 {
		t.Fatalf("the Team limit's logged misses: %q", teamLimit.logLines)
	}

	stress := &sidecarDirectoryLoadJudgeRecorder{TB: t}
	sidecarDirectoryLoadJudge(stress, "the stress case", state, ceilings, nil)
	if len(stress.errorLines) != 0 {
		t.Fatalf("the stress case's held misses: %q", stress.errorLines)
	}
	if len(stress.logLines) != 4 || count(stress.logLines, "p99") != 2 || count(stress.logLines, "rerun") != 2 || count(stress.logLines, "recorded and not gating") != 4 {
		t.Fatalf("the stress case's logged misses: %q", stress.logLines)
	}
}
