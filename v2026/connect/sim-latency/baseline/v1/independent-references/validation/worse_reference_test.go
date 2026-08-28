package connect

import (
	"context"
	"testing"
	"time"
)

func TestV5WorseReferenceLoadWorkersAreFleetOnly(t *testing.T) {
	testCases := []struct {
		name      string
		arguments []string
		workers   int
	}{
		{name: "empty", arguments: nil, workers: 0},
		{name: "run", arguments: []string{"/opt/urnetwork/bin/sim-latency", "run"}, workers: 0},
		{name: "fleet", arguments: []string{"/opt/urnetwork/bin/sim-latency", "fleet"}, workers: 2},
		{name: "other binary", arguments: []string{"/tmp/other", "fleet"}, workers: 0},
		{name: "other command", arguments: []string{"/opt/urnetwork/bin/sim-latency", "score"}, workers: 0},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if workers := residentContractReferenceLoadWorkers(testCase.arguments); workers != testCase.workers {
				t.Fatalf("workers = %d, want %d", workers, testCase.workers)
			}
		})
	}
}

func TestV5WorseReferenceLoadUsesBoundedDutyCycle(t *testing.T) {
	var limitedTo int
	var burned []time.Duration
	var waited []time.Duration
	runResidentContractReferenceLoad(
		context.Background(),
		residentContractReferenceLoadStartDelay,
		residentContractReferenceLoadBusy,
		residentContractReferenceLoadIdle,
		func(value int) int {
			limitedTo = value
			return value
		},
		func(duration time.Duration) {
			burned = append(burned, duration)
		},
		func(_ context.Context, duration time.Duration) bool {
			waited = append(waited, duration)
			return len(waited) == 1
		},
	)
	if limitedTo != 2 {
		t.Fatalf("GOMAXPROCS limit = %d, want 2", limitedTo)
	}
	if len(burned) != 1 || burned[0] != 90*time.Millisecond {
		t.Fatalf("burn durations = %v, want [90ms]", burned)
	}
	if len(waited) != 2 || waited[0] != 5*time.Minute || waited[1] != 10*time.Millisecond {
		t.Fatalf("wait durations = %v, want [5m 10ms]", waited)
	}
}

func TestV5WorseReferenceLoadStopsBeforeActivation(t *testing.T) {
	limited := false
	burned := false
	runResidentContractReferenceLoad(
		context.Background(),
		residentContractReferenceLoadStartDelay,
		residentContractReferenceLoadBusy,
		residentContractReferenceLoadIdle,
		func(value int) int {
			limited = true
			return value
		},
		func(time.Duration) {
			burned = true
		},
		func(context.Context, time.Duration) bool {
			return false
		},
	)
	if limited || burned {
		t.Fatalf("canceled load activated: limited=%v burned=%v", limited, burned)
	}
}
