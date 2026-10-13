// Batched wallet resolutions retain v1's rows and refusals
// (VERSION2-FINAL.md §8.2). Identities and keys are synthetic.
package model

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
)

const stResolutionBatchTestKey = StDeploymentKey("945:0x3000000000000000000000000000000000000003")

// count valid resolutions cycling through the three modes.
func stResolutionBatchFixture(count int) []*StPayoutWalletResolution {
	resolutions := make([]*StPayoutWalletResolution, count)
	for index := range count {
		resolution := &StPayoutWalletResolution{ClientId: server.NewId(), NetworkId: server.NewId(), Coldkey: [32]byte{byte(index), byte(index >> 8), 1},
			ConsentHash: [32]byte{byte(index), byte(index >> 8), 2}, ConsentGeneration: uint64(1 + index%5), HeadHash: [32]byte{byte(index), byte(index >> 8), 3}, HeadGeneration: uint64(5 + index%5)}
		switch index % 3 {
		case 0:
			resolution.Mode = protocol.EarningWalletModeProvider
		case 1:
			resolution.Mode = protocol.EarningWalletModeNetwork
		default:
			resolution.Mode = protocol.EarningWalletModeHotkey
			resolution.Hotkey, resolution.HotkeyConsentHash, resolution.HotkeyConsentHeadHash = [32]byte{byte(index), 4}, [32]byte{byte(index), 5}, [32]byte{byte(index), 6}
			resolution.HotkeyConsentGeneration, resolution.HotkeyConsentHeadGeneration = uint64(1+index%3), 3
		}
		resolutions[index] = resolution
	}
	return resolutions
}

// The retained rows of an epoch, by value.
func stResolutionBatchRows(epoch uint64) []StPayoutWalletResolution {
	rows := []StPayoutWalletResolution{}
	for _, resolution := range GetStPayoutWalletResolutions(context.Background(), stResolutionBatchTestKey, epoch, 1) {
		rows = append(rows, *resolution)
	}
	return rows
}

// SV-6: across more than one batch and all three modes, the batched writer
// retains exactly the rows v1 retains, and a retry of either is accepted
// without a change.
func TestStPayoutWalletResolutionsBatchedEqualV1(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		resolutions := stResolutionBatchFixture(stPayoutWalletResolutionBatch + 100)
		for range 2 {
			if err := AddStPayoutWalletResolutions(ctx, stResolutionBatchTestKey, 10, 1, resolutions); err != nil {
				t.Fatal(err)
			}
			if err := AddStPayoutWalletResolutionsBatched(ctx, stResolutionBatchTestKey, 11, 1, resolutions); err != nil {
				t.Fatal(err)
			}
		}
		v1, batched := stResolutionBatchRows(10), stResolutionBatchRows(11)
		if len(v1) != len(resolutions) || !slices.Equal(v1, batched) {
			t.Fatalf("batched rows differ from v1's: %d and %d rows", len(v1), len(batched))
		}
		if err := AddStPayoutWalletResolutionsBatched(ctx, stResolutionBatchTestKey, 12, 1, nil); err != nil || len(stResolutionBatchRows(12)) != 0 {
			t.Fatal("an empty epoch was refused or wrote rows", err)
		}
	})
}

// SV-6: a resolution that differs from a retained one is an integrity error
// and is never overwritten; the new resolutions before it in client order
// stay retained and none after it are written, exactly as v1 leaves them,
// including when the difference is in a later batch.
func TestStPayoutWalletResolutionsBatchedKeepV1PrefixOnMismatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		resolutions := stResolutionBatchFixture(2*stPayoutWalletResolutionBatch + 10)
		slices.SortFunc(resolutions, func(a *StPayoutWalletResolution, b *StPayoutWalletResolution) int {
			return a.ClientId.Cmp(b.ClientId)
		})
		for _, at := range []int{3, stPayoutWalletResolutionBatch + 7} {
			retained := *resolutions[at]
			changed := *resolutions[at]
			changed.Coldkey[31] ^= 1
			input := append([]*StPayoutWalletResolution{}, resolutions...)
			input[at] = &changed
			for _, epoch := range []uint64{20, 21} {
				epoch += uint64(at)
				if err := AddStPayoutWalletResolutions(ctx, stResolutionBatchTestKey, epoch, 1, []*StPayoutWalletResolution{&retained}); err != nil {
					t.Fatal(err)
				}
			}
			v1Err := AddStPayoutWalletResolutions(ctx, stResolutionBatchTestKey, 20+uint64(at), 1, input)
			batchedErr := AddStPayoutWalletResolutionsBatched(ctx, stResolutionBatchTestKey, 21+uint64(at), 1, input)
			if !errors.Is(v1Err, protocol.ErrWalletMappingIntegrity) || !errors.Is(batchedErr, protocol.ErrWalletMappingIntegrity) {
				t.Fatal("a changed resolution was accepted", v1Err, batchedErr)
			}
			v1, batched := stResolutionBatchRows(20+uint64(at)), stResolutionBatchRows(21+uint64(at))
			if len(v1) != at+1 || !slices.Equal(v1, batched) || batched[at] != retained {
				t.Fatalf("mismatch at %d: v1 kept %d rows, batched %d", at, len(v1), len(batched))
			}
		}
	})
}

// SV-6: an exact duplicate is one row; a differing duplicate is an integrity
// error that keeps the earlier input, as a v1 retry would.
func TestStPayoutWalletResolutionsBatchedDuplicates(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		resolutions := stResolutionBatchFixture(4)
		duplicate := *resolutions[1]
		if err := AddStPayoutWalletResolutionsBatched(ctx, stResolutionBatchTestKey, 30, 1, append(resolutions, &duplicate)); err != nil || len(stResolutionBatchRows(30)) != 4 {
			t.Fatal("an exact duplicate was refused or written twice", err)
		}
		changed := *resolutions[1]
		changed.HeadGeneration++
		err := AddStPayoutWalletResolutionsBatched(ctx, stResolutionBatchTestKey, 31, 1, []*StPayoutWalletResolution{resolutions[1], &changed})
		rows := stResolutionBatchRows(31)
		if !errors.Is(err, protocol.ErrWalletMappingIntegrity) || len(rows) != 1 || rows[0] != *resolutions[1] {
			t.Fatal("a differing duplicate was accepted or replaced the earlier input", err, rows)
		}
	})
}

// SV-6: each malformed resolution is refused before any row is written, as
// v1 refuses it.
func TestStPayoutWalletResolutionsBatchedRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		valid := stResolutionBatchFixture(3)
		mutated := func(index int, mutate func(*StPayoutWalletResolution)) *StPayoutWalletResolution {
			resolution := *valid[index]
			mutate(&resolution)
			return &resolution
		}
		for epoch, refused := range []*StPayoutWalletResolution{
			nil,
			mutated(0, func(resolution *StPayoutWalletResolution) { resolution.ClientId = server.Id{} }),
			mutated(0, func(resolution *StPayoutWalletResolution) { resolution.NetworkId = server.Id{} }),
			mutated(0, func(resolution *StPayoutWalletResolution) { resolution.Mode = "roster" }),
			mutated(0, func(resolution *StPayoutWalletResolution) { resolution.Hotkey = [32]byte{1} }),
			mutated(1, func(resolution *StPayoutWalletResolution) { resolution.HotkeyConsentHeadGeneration = 1 }),
			mutated(2, func(resolution *StPayoutWalletResolution) { resolution.HotkeyConsentHash = [32]byte{} }),
		} {
			input := append(append([]*StPayoutWalletResolution{}, valid...), refused)
			v1Err := AddStPayoutWalletResolutions(ctx, stResolutionBatchTestKey, 40+uint64(epoch), 1, input)
			batchedErr := AddStPayoutWalletResolutionsBatched(ctx, stResolutionBatchTestKey, 60+uint64(epoch), 1, input)
			if !errors.Is(v1Err, protocol.ErrWalletMappingIntegrity) || !errors.Is(batchedErr, protocol.ErrWalletMappingIntegrity) || len(stResolutionBatchRows(40+uint64(epoch))) != 0 || len(stResolutionBatchRows(60+uint64(epoch))) != 0 {
				t.Fatalf("malformed resolution %d was accepted: v1 %v, batched %v", epoch, v1Err, batchedErr)
			}
		}
	})
}
