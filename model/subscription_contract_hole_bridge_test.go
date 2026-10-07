package model

// Rollout policy must distinguish absent evidence from a genuine negative and
// use the same resumable source predicate as normal background reconciliation.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Missing, known-zero, positive, malformed and unavailable observations remain
// distinct without doing any source reconstruction inside the Redis reader.
func TestContractHoleReadStatusDistinguishesNegativeAndUnknown(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		status, err := ReadContractHole(ctx, source, destination)
		if err != nil || status != ContractHoleUnknown {
			t.Fatalf("missing status=%d error=%v", status, err)
		}
		server.Raise(applyContractHoleEvent(ctx, server.NewId(), source, destination, "create"))
		for _, sample := range []struct {
			value     string
			expiry    time.Duration
			status    ContractHoleStatus
			wantError bool
		}{
			{value: "0", expiry: ContractHoleTtl, status: ContractHoleNegative},
			{value: "1", expiry: ContractHoleTtl, status: ContractHolePositive},
			{value: "01", expiry: ContractHoleTtl, status: ContractHoleUnknown, wantError: true},
			{value: "broken", expiry: ContractHoleTtl, status: ContractHoleUnknown, wantError: true},
			{value: "1", expiry: 0, status: ContractHoleUnknown, wantError: true},
			{value: "1", expiry: ContractHoleTtl + time.Hour, status: ContractHoleUnknown, wantError: true},
			{value: "1", expiry: ContractHoleTtl, status: ContractHolePositive},
		} {
			server.Redis(ctx, func(client server.RedisClient) {
				server.Raise(client.Set(ctx, contractHoleKeys(source, destination)[0], sample.value, sample.expiry).Err())
			})
			status, err = ReadContractHole(ctx, source, destination)
			if status != sample.status || (err != nil) != sample.wantError {
				t.Fatalf("value=%q expiry=%s status=%d error=%v", sample.value, sample.expiry, status, err)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if status, err := ReadContractHole(canceled, source, destination); status != ContractHoleUnknown || err == nil {
			t.Fatalf("unavailable status=%d error=%v", status, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.PExpireAt(ctx, contractHoleKeys(source, destination)[0], time.Unix(1, 0)).Err())
		})
		if status, err := ReadContractHole(ctx, source, destination); status != ContractHoleUnknown || err != nil {
			t.Fatalf("expired status=%d error=%v", status, err)
		}
	})
}

// A legacy NULL expiration has no absolute lifetime. A real checkpoint renews
// its quiet period and permits the same contract in both directions.
func TestContractHoleLegacyRolloutHasNoFixedMaximumAge(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		id := insertContractHoleSourceRow(ctx, source, destination)
		cutoff := server.NowUtc().Add(-12 * time.Minute)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET create_time=$2 WHERE contract_id=$1`, id, cutoff.Add(-24*time.Hour)))
			state, err := inspectContractExpiryInTx(ctx, tx, id, cutoff, false)
			if err != nil || state == nil {
				t.Fatalf("quiet old contract was not an expiry candidate: state=%+v error=%v", state, err)
			}
		})
		server.Raise(CloseContract(ctx, id, source, 0, true))
		server.Tx(ctx, func(tx server.PgTx) {
			state, err := inspectContractExpiryInTx(ctx, tx, id, cutoff, false)
			if err != nil || state != nil {
				t.Fatalf("checkpoint did not renew quiet period: state=%+v error=%v", state, err)
			}
		})
		for _, direction := range []bool{false, true} {
			src, dst := source, destination
			if direction {
				src, dst = dst, src
			}
			found, err := HasResumableContractForPair(ctx, src, dst)
			if err != nil || !found {
				t.Fatalf("checkpoint bridge refused direction=%t found=%t error=%v", direction, found, err)
			}
		}
		if HasOpenContractForPair(ctx, source, destination) {
			t.Fatal("old new-forward predicate no longer distinguishes the checkpoint control")
		}
		published, err := refreshContractHole(ctx, source, destination)
		if err != nil || !published {
			t.Fatalf("checkpoint projection refused: published=%t error=%v", published, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 1)
		server.Raise(CloseContract(ctx, id, source, 0, false))
		found, err := HasResumableContractForPair(ctx, source, destination)
		if err != nil || found {
			t.Fatalf("final-close bridge admitted: found=%t error=%v", found, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 0)
		if status, err := ReadContractHole(ctx, source, destination); err != nil || status != ContractHoleUnknown {
			t.Fatalf("deleted counter falsely claimed a known negative: status=%d error=%v", status, err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if found, err := HasResumableContractForPair(canceled, source, destination); err == nil || found {
			t.Fatalf("source failure became negative evidence: found=%t error=%v", found, err)
		}
	})
}
