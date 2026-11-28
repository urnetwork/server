package controller

// The validator data grant's outcomes and their counter, against the local
// database.

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Each outcome is returned and counted once: created, then skipped while the
// grant is active, disabled under settings that grant nothing, and failed,
// without raising, when the database work cannot run or the network does not
// exist.
func TestGrantSnValidatorDataCountsEachOutcome(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "validator-grant-outcomes", server.NewId())
		var hotkey [32]byte
		for i := range hotkey {
			hotkey[i] = 0x71 + byte(i)
		}
		config := model.Pro()
		previous := config.ValidatorDataGrant
		defer func() { config.ValidatorDataGrant = previous }()
		config.ValidatorDataGrant = model.DefaultValidatorDataGrantSettings()
		now := server.NowUtc()

		counts := func() map[string]float64 {
			values := map[string]float64{}
			for _, outcome := range []string{SnValidatorDataGrantCreated, SnValidatorDataGrantSkipped, SnValidatorDataGrantDisabled, SnValidatorDataGrantFailed} {
				values[outcome] = testutil.ToFloat64(snValidatorDataGrants.WithLabelValues(outcome))
			}
			return values
		}
		expect := func(outcome string, grant func() string) {
			before := counts()
			connect.AssertEqual(t, grant(), outcome)
			after := counts()
			for name, value := range after {
				want := before[name]
				if name == outcome {
					want += 1
				}
				if value != want {
					t.Fatalf("after a grant answered %s, %s counted %v, want %v", outcome, name, value, want)
				}
			}
		}

		expect(SnValidatorDataGrantCreated, func() string {
			return GrantSnValidatorData(ctx, networkId, hotkey, now)
		})
		expect(SnValidatorDataGrantSkipped, func() string {
			return GrantSnValidatorData(ctx, networkId, hotkey, now.Add(time.Minute))
		})
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		expect(SnValidatorDataGrantFailed, func() string {
			return GrantSnValidatorData(canceled, networkId, hotkey, now.Add(40*24*time.Hour))
		})
		expect(SnValidatorDataGrantFailed, func() string {
			return GrantSnValidatorData(ctx, server.NewId(), hotkey, now.Add(40*24*time.Hour))
		})
		config.ValidatorDataGrant.Duration = 0
		expect(SnValidatorDataGrantDisabled, func() string {
			return GrantSnValidatorData(ctx, networkId, hotkey, now.Add(40*24*time.Hour))
		})
	})
}
