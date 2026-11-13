// A share lane keeps a minimum claim rate behind a dominant older backlog.
// These controls count exact claim turns, not throughput over time.
package task

import (
	"fmt"
	"testing"
	"time"
)

// Sixty-two empty lanes beside the share lane and the backlog's lane, as Main
// registers about a hundred functions.
func claimShareTestAliases() []string {
	aliases := make([]string, 0, 62)
	for index := range 62 {
		aliases = append(aliases, fmt.Sprintf("fixture.example/tasks.EmptyLane%02d", index))
	}
	return aliases
}

// Every other ordinary turn goes to the share lane, and each lane keeps its
// rotation turns. Without the share, the lane gets one turn per full rotation.
func TestTaskClaimShareTakesHalfOfTheOrdinaryTurns(t *testing.T) {
	share := NewTaskTarget(claimProfileAllowed).TargetFunctionName()
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	settings.TargetClaimShares = map[string]int{share: 2}
	worker := NewTaskWorker(t.Context(), settings)
	defer worker.Close()
	worker.AddTargets(NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded, claimShareTestAliases()...))
	lanes := len(worker.claimFunctionNames)
	poll := &taskClaimPoll{}
	rotation := map[string]int{}
	ordinaryNames := []string{}
	for turn := range 4 * lanes {
		name := worker.nextClaimFunction(taskClaimOptions{poll: poll})
		if turn%2 == 0 {
			rotation[name]++
		} else {
			ordinaryNames = append(ordinaryNames, name)
		}
	}
	for _, name := range worker.claimFunctionNames {
		if rotation[name] != 2 {
			t.Fatal("the share changed a lane's rotation turns", name, rotation[name])
		}
	}
	shareTurns := 0
	for index, name := range ordinaryNames {
		want := ""
		if index%2 == 1 {
			want = share
			shareTurns++
		}
		if name != want {
			t.Fatalf("ordinary turn %d went to %q instead of %q", index, name, want)
		}
	}
	// A quarter of all claims, against two rotation turns in 4*lanes claims.
	if shareTurns != lanes {
		t.Fatal("the share lane lost its turns", shareTurns, lanes)
	}
}

// A saturated share target leaves its turn to the ordinary queue and keeps its
// credit, so it takes the first ordinary turn after capacity returns.
func TestTaskClaimShareYieldsWhileSaturatedAndKeepsItsTurn(t *testing.T) {
	share := NewTaskTarget(claimProfileAllowed).TargetFunctionName()
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	settings.TargetClaimShares = map[string]int{share: 2}
	settings.TargetClaimLimits = map[string]int{share: 1}
	worker := NewTaskWorker(t.Context(), settings)
	defer worker.Close()
	worker.AddTargets(NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded))
	ordinary := func() string {
		worker.nextClaimFunction(taskClaimOptions{poll: &taskClaimPoll{}})
		return worker.nextClaimFunction(taskClaimOptions{poll: &taskClaimPoll{}})
	}
	reservation, admitted := worker.reserveTaskClaim(share)
	if !admitted || reservation == nil {
		t.Fatal("could not saturate the share target")
	}
	for index := range 4 {
		if name := ordinary(); name != "" {
			t.Fatal("a saturated share target took an ordinary turn", index, name)
		}
	}
	reservation.release()
	if name := ordinary(); name != share {
		t.Fatal("the share lane lost its credit while saturated", name)
	}
	if name := ordinary(); name != "" {
		t.Fatal("the share lane took two ordinary turns in a row", name)
	}
}

// A claim's read of the lane from its start did not complete, so the lane has
// no floor. It takes no ordinary turn until its next read from the start is
// due, and it keeps its share once a claim has read it.
func TestTaskClaimShareWaitsForAFloorAfterAnIncompleteFullRead(t *testing.T) {
	share := NewTaskTarget(claimProfileAllowed).TargetFunctionName()
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	settings.TargetClaimShares = map[string]int{share: 2}
	worker := NewTaskWorker(t.Context(), settings)
	defer worker.Close()
	worker.AddTargets(NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded))
	now := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	worker.claimNow = func() time.Time { return now }
	ordinary := func() string {
		worker.nextClaimFunction(taskClaimOptions{poll: &taskClaimPoll{}})
		return worker.nextClaimFunction(taskClaimOptions{poll: &taskClaimPoll{}})
	}
	if first, second := ordinary(), ordinary(); first != "" || second != share {
		t.Fatal("the lane's first read from its start was not due", first, second)
	}
	// The turn's claim starts the full read and fails before it sees the lane.
	if isShare, floorBlock, reset := worker.claimShareFloor(share, now); !isShare || floorBlock != 0 || !reset {
		t.Fatal("the lane's first claim did not read from its start", isShare, floorBlock, reset)
	}
	for index := range 4 {
		if name := ordinary(); name != "" {
			t.Fatal("a lane without a floor took an ordinary turn before its next full read", index, name)
		}
	}
	now = now.Add(claimShareFloorReset)
	if name := ordinary(); name != share {
		t.Fatal("the lane's next read from its start was not attempted", name)
	}
	if isShare, floorBlock, reset := worker.claimShareFloor(share, now); !isShare || floorBlock != 0 || !reset {
		t.Fatal("the lane's due claim did not read from its start", isShare, floorBlock, reset)
	}
	firstBlock := now.Unix() - 3600
	worker.observeClaimShareFloor(share, &firstBlock, now.Unix(), true)
	if first, second := ordinary(), ordinary(); first != "" || second != share {
		t.Fatal("a lane with a floor lost its share", first, second)
	}
	if isShare, floorBlock, reset := worker.claimShareFloor(share, now); !isShare || floorBlock != firstBlock || reset {
		t.Fatal("a claim did not read from the lane's floor", isShare, floorBlock, reset)
	}
}

// Shares are named, at least two ordinary turns apart, and leave the ordinary
// queue at least half of its turns.
func TestTaskClaimShareSettingsAreBounded(t *testing.T) {
	share := NewTaskTarget(claimProfileAllowed).TargetFunctionName()
	other := NewTaskTarget(claimProfileExcluded).TargetFunctionName()
	for _, shares := range []map[string]int{
		{share: 1},
		{"": 2},
		{share: 2, other: 4},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Error("worker accepted unbounded claim shares", shares)
				}
			}()
			settings := DefaultTaskWorkerSettings()
			settings.TargetClaimShares = shares
			NewTaskWorker(t.Context(), settings).Close()
		}()
	}
	settings := DefaultTaskWorkerSettings()
	settings.TargetClaimShares = map[string]int{share: 4, other: 4}
	worker := NewTaskWorker(t.Context(), settings)
	defer worker.Close()
	settings.TargetClaimShares[share] = 1
	if worker.settings.TargetClaimShares[share] != 4 {
		t.Fatal("worker borrowed the caller's mutable share map")
	}
}
