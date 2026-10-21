// Dispatch admission belongs after a saturated slot handoff; already-started
// owners still finish when the fake dispatch budget expires.
package work

import (
	"testing"
	"testing/synctest"
)

// The first owner holds the only slot until the dispatcher is durably blocked
// behind it. Expiring admission at that barrier must leave the last owner due.
func TestContractOwnerBatchesRecheckBudgetAfterSaturatedSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan bool)
		admitted := true
		started := []int{}
		go func() {
			done <- runContractOwnerBatches([]int{0, 1}, 1, func() bool {
				return admitted
			}, func(owner int) {
				started = append(started, owner)
				if owner == 0 {
					close(entered)
					<-release
				}
			})
		}()
		<-entered
		synctest.Wait()
		admitted = false
		close(release)
		if complete := <-done; complete {
			t.Fatal("the last owner was admitted after its slot wait exhausted the budget")
		}
		if len(started) != 1 || started[0] != 0 {
			t.Fatalf("started owners %v, want only the owner already running at expiry", started)
		}
	})
}
