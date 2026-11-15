package work

import "testing"

// The actual pass and worker pool must admit the whole selected250 while all
// checks and the independent full lane are held on synthetic barriers.
func TestProviderEgressParallelTailAllSelectedStartBeforeCompletion(t *testing.T) {
	got := testProviderEgressParallelRun(t, true, false, false)
	if got.initialStarted != 250 || got.peak != 250 || got.allStarted != 250 ||
		got.err != nil || got.result == nil || got.result.Checked != 250 || got.result.Submitted != 8 || len(got.checks) != 250 {
		t.Fatalf("full lane stole blackhole slots: initial=%d all=%d peak=%d checks=%d err=%v result=%+v",
			got.initialStarted, got.allStarted, got.peak, len(got.checks), got.err, got.result)
	}
}

func TestProviderEgressParallelTailNoFullKeepsSelectedCapacity(t *testing.T) {
	got := testProviderEgressParallelRun(t, false, false, false)
	if got.initialStarted != 250 || got.peak != 250 || got.err != nil || len(got.checks) != 250 {
		t.Fatalf("no-full geometry changed: initial=%d peak=%d checks=%d err=%v", got.initialStarted, got.peak, len(got.checks), got.err)
	}
}
