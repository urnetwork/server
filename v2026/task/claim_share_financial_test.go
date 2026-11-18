// Three share lanes in the production order: the sender, the provider totals
// and the debit flush. These controls count exact claim turns.
package task

import (
	"testing"

	"github.com/urnetwork/server/v2026/session"
)

// A third registered target whose lane sorts after the two profile fixtures,
// as the debit flush sorts after the sender and the totals.
func claimShareThirdLane(_ *claimProfileArgs, _ *session.ClientSession) (*claimProfileResult, error) {
	return &claimProfileResult{}, nil
}

// With the sender and the flush at period eight and the totals at four, each
// lane takes exactly its share of the ordinary turns while all are due, and
// the other half of the ordinary turns stays oldest first.
func TestTaskClaimShareFinancialPeriodsServeEveryLane(t *testing.T) {
	names := [3]string{
		NewTaskTarget(claimProfileAllowed).TargetFunctionName(),
		NewTaskTarget(claimProfileExcluded).TargetFunctionName(),
		NewTaskTarget(claimShareThirdLane).TargetFunctionName(),
	}
	settings := DefaultTaskWorkerSettings()
	settings.FairClaimFunctions = true
	settings.TargetClaimShares = map[string]int{names[0]: 8, names[1]: 4, names[2]: 8}
	worker := NewTaskWorker(t.Context(), settings)
	defer worker.Close()
	worker.AddTargets(NewTaskTarget(claimProfileAllowed), NewTaskTarget(claimProfileExcluded), NewTaskTarget(claimShareThirdLane))
	counts := map[string]int{}
	ordinaryTurns := 0
	for turn := range 2 * (16 + 800) {
		name := worker.nextClaimFunction(taskClaimOptions{poll: &taskClaimPoll{}})
		// skip the credit warm-up; count only ordinary (odd) turns
		if turn%2 == 1 && 2*16 <= turn {
			counts[name]++
			ordinaryTurns++
		}
	}
	if counts[names[0]] != ordinaryTurns/8 || counts[names[1]] != ordinaryTurns/4 || counts[names[2]] != ordinaryTurns/8 || counts[""] != ordinaryTurns/2 {
		t.Fatalf("share lanes did not each take their period: sender=%d totals=%d flush=%d oldest-first=%d of %d",
			counts[names[0]], counts[names[1]], counts[names[2]], counts[""], ordinaryTurns)
	}
}
