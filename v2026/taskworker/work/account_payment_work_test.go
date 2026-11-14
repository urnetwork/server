package work

import (
	"encoding/json"
	"testing"
)

// USDC payouts ended (owner decision 2026-11-12). The payout target plans and
// sends nothing, a retry row's arguments still decode, and its post schedules
// no next run. Before, the run planned and sent through its session, and its
// post scheduled the next Sunday's run, or a retry, in its transaction; here
// there is neither.
func TestPayoutIsANoOp(t *testing.T) {
	var args SchedulePayoutArgs
	if err := json.Unmarshal([]byte(`{"retry":true}`), &args); err != nil {
		t.Fatal(err)
	}
	result, err := Payout(&args, nil)
	if err != nil || result == nil {
		t.Fatalf("payout returned %v, %v", result, err)
	}
	if err := PayoutPost(&args, result, nil, nil); err != nil {
		t.Fatal(err)
	}
}
