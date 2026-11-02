package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// An award checks its window's reliability retention when it starts and again
// on the clock after its reads, which take hours on Main. One that started an
// hour ago for a window whose retention ran out since fails closed, as the
// contract horizon reaper, which keeps a block's sweeps only while its
// reliability is retained, may have removed them by then.
func TestOperatorBlockReliabilityChecksRetentionAgainOnTheClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		started := now.Add(-time.Hour)
		// retained when the award started, past retention now
		start := now.Add(-ClientReliabilityRetention - 30*time.Minute)
		end := start.Add(SubnetBlockDuration)
		minBlockNumber, _ := operatorBlockReliabilityBlockWindow(start, end)
		// one drained block, so the read judges the window
		coverClientReliabilityBlock(ctx, minBlockNumber)

		if !operatorBlockReliabilityRetained(minBlockNumber, started) || operatorBlockReliabilityRetained(minBlockNumber, now) {
			t.Fatal("the window is not retained at the award's start and expired now")
		}
		_, err := readOperatorBlockReliability(ctx, start, end, started)
		if !errors.Is(err, ErrOperatorBlockPointsReliabilityExpired) {
			t.Fatalf("an award that outlived its window's retention read on: %v", err)
		}
	})
}
