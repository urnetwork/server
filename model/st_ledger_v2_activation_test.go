package model

// st_ledger_v2_activation_test.go — pg-backed tests of the settlement v2
// activation record (SV-9).

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/urnetwork/server"
)

// SV-9: a deployment's activation is recorded once. The same record again
// changes nothing; a record that differs in the pool, the first epoch, the
// vault2 or the STSettlement2 address is refused, naming the recorded one;
// another deployment records its own.
func TestStLedgerV2ActivationIsRecordedOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key, other := testStDeploymentKey, StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		if activation := GetStLedgerV2Activation(ctx, key); activation != nil {
			t.Fatalf("an activation before any record: %+v", activation)
		}
		activation := StLedgerV2Activation{NoId: 1, FirstEpoch: 5, VaultV2Address: "0x00000000000000000000000000000000000000a2", SettlementV2Address: "0x00000000000000000000000000000000000000a1"}
		if err := RecordStLedgerV2Activation(ctx, key, &activation); err != nil {
			t.Fatal(err)
		}
		recorded := GetStLedgerV2Activation(ctx, key)
		if recorded == nil || recorded.NoId != 1 || recorded.FirstEpoch != 5 || recorded.VaultV2Address != activation.VaultV2Address ||
			recorded.SettlementV2Address != activation.SettlementV2Address || recorded.CreateTime.IsZero() {
			t.Fatalf("recorded %+v", recorded)
		}
		repeat := activation
		if err := RecordStLedgerV2Activation(ctx, key, &repeat); err != nil {
			t.Fatalf("repeating the record: %v", err)
		}
		if again := GetStLedgerV2Activation(ctx, key); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("a repeated record changed %+v to %+v", recorded, again)
		}

		const want = "st ledger v2: the deployment's activation is already recorded: pool 1 from epoch 5 with vault_v2_address 0x00000000000000000000000000000000000000a2 and settlement_v2_address 0x00000000000000000000000000000000000000a1"
		for _, test := range []struct {
			name   string
			change func(*StLedgerV2Activation)
		}{
			{"pool", func(a *StLedgerV2Activation) { a.NoId = 2 }},
			{"first epoch", func(a *StLedgerV2Activation) { a.FirstEpoch = 6 }},
			{"vault2", func(a *StLedgerV2Activation) { a.VaultV2Address = "0x00000000000000000000000000000000000000b2" }},
			{"STSettlement2", func(a *StLedgerV2Activation) { a.SettlementV2Address = "0x00000000000000000000000000000000000000b1" }},
		} {
			conflicting := activation
			test.change(&conflicting)
			if err := RecordStLedgerV2Activation(ctx, key, &conflicting); !errors.Is(err, ErrStLedgerV2ActivationRecorded) || err.Error() != want {
				t.Fatalf("a different %s: %v", test.name, err)
			}
		}
		if again := GetStLedgerV2Activation(ctx, key); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("a refused record changed %+v to %+v", recorded, again)
		}

		if activation := GetStLedgerV2Activation(ctx, other); activation != nil {
			t.Fatalf("another deployment sees %+v", activation)
		}
		second := StLedgerV2Activation{NoId: 2, FirstEpoch: 9, VaultV2Address: "0x00000000000000000000000000000000000000c2", SettlementV2Address: "0x00000000000000000000000000000000000000c1"}
		if err := RecordStLedgerV2Activation(ctx, other, &second); err != nil {
			t.Fatalf("another deployment's record: %v", err)
		}
		if got := GetStLedgerV2Activation(ctx, other); got == nil || got.NoId != 2 || got.FirstEpoch != 9 || got.VaultV2Address != second.VaultV2Address || got.SettlementV2Address != second.SettlementV2Address {
			t.Fatalf("another deployment recorded %+v", got)
		}
	})
}

// SV-9 (clear): a recorded activation is cleared only by naming each of its
// values exactly; anything else leaves it. In a database without the record's
// table the tolerant read finds no activation, where the plain read fails.
func TestStLedgerV2ActivationIsClearedByItsExactValues(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		key := testStDeploymentKey
		activation := StLedgerV2Activation{NoId: 1, FirstEpoch: 5, VaultV2Address: "0x00000000000000000000000000000000000000a2", SettlementV2Address: "0x00000000000000000000000000000000000000a1"}
		if ClearStLedgerV2Activation(ctx, key, &activation) {
			t.Fatal("cleared an activation that is not recorded")
		}
		if err := RecordStLedgerV2Activation(ctx, key, &activation); err != nil {
			t.Fatal(err)
		}
		recorded := GetStLedgerV2Activation(ctx, key)
		if installed := GetStLedgerV2ActivationIfInstalled(ctx, key); !reflect.DeepEqual(installed, recorded) || installed == nil {
			t.Fatalf("the tolerant read returned %+v, want %+v", installed, recorded)
		}
		for name, change := range map[string]func(*StLedgerV2Activation){
			"pool":          func(a *StLedgerV2Activation) { a.NoId = 2 },
			"first epoch":   func(a *StLedgerV2Activation) { a.FirstEpoch = 6 },
			"vault2":        func(a *StLedgerV2Activation) { a.VaultV2Address = "0x00000000000000000000000000000000000000A2" },
			"STSettlement2": func(a *StLedgerV2Activation) { a.SettlementV2Address = "0x00000000000000000000000000000000000000b1" },
		} {
			named := activation
			change(&named)
			if ClearStLedgerV2Activation(ctx, key, &named) {
				t.Fatalf("cleared by a different %s", name)
			}
		}
		if ClearStLedgerV2Activation(ctx, StDeploymentKey("945:0x1000000000000000000000000000000000000001"), &activation) {
			t.Fatal("cleared through another deployment")
		}
		if again := GetStLedgerV2Activation(ctx, key); !reflect.DeepEqual(again, recorded) {
			t.Fatalf("a refused clear changed %+v to %+v", recorded, again)
		}
		if !ClearStLedgerV2Activation(ctx, key, &activation) || GetStLedgerV2Activation(ctx, key) != nil || ClearStLedgerV2Activation(ctx, key, &activation) {
			t.Fatal("the exact values did not clear the activation once")
		}

		if err := RecordStLedgerV2Activation(ctx, key, &activation); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE st_ledger_v2_activation`))
		})
		if installed := GetStLedgerV2ActivationIfInstalled(ctx, key); installed != nil {
			t.Fatalf("an activation without its table: %+v", installed)
		}
		if failed := server.HandleError(func() { GetStLedgerV2Activation(ctx, key) }); failed == nil {
			t.Fatal("the plain read ran without the table")
		}
	})
}

// SV-9 (write race): processes that observe the registration at once all
// record without an error or panic and leave one row. Of processes that
// record different activations at once exactly one wins, every other is
// refused with ErrStLedgerV2ActivationRecorded, and the row is the winner's.
func TestStLedgerV2ActivationConcurrentRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const writers = 24
		record := func(key StDeploymentKey, firstEpoch func(int) uint64) ([]error, []any) {
			errs, panics := make([]error, writers), make([]any, writers)
			start := make(chan struct{})
			var wait sync.WaitGroup
			for i := range writers {
				wait.Add(1)
				go func() {
					defer wait.Done()
					defer func() { panics[i] = recover() }()
					<-start
					errs[i] = RecordStLedgerV2Activation(ctx, key, &StLedgerV2Activation{NoId: 1, FirstEpoch: firstEpoch(i),
						VaultV2Address: "0x00000000000000000000000000000000000000a2", SettlementV2Address: "0x00000000000000000000000000000000000000a1"})
				}()
			}
			close(start)
			wait.Wait()
			return errs, panics
		}
		rows := func(key StDeploymentKey) (count int) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(tx.QueryRow(ctx, `SELECT COUNT(*) FROM st_ledger_v2_activation WHERE deployment_key = $1`, string(key)).Scan(&count))
			})
			return count
		}

		same := testStDeploymentKey
		errs, panics := record(same, func(int) uint64 { return 5 })
		for i := range writers {
			if errs[i] != nil || panics[i] != nil {
				t.Fatalf("writer %d of the same activation: %v, panic %v", i, errs[i], panics[i])
			}
		}
		if got := GetStLedgerV2Activation(ctx, same); got == nil || got.FirstEpoch != 5 || rows(same) != 1 {
			t.Fatalf("recorded %+v in %d rows", got, rows(same))
		}

		differing := StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		errs, panics = record(differing, func(i int) uint64 { return uint64(100 + i) })
		winners := []uint64{}
		for i := range writers {
			switch {
			case panics[i] != nil:
				t.Fatalf("writer %d of a differing activation panicked: %v", i, panics[i])
			case errs[i] == nil:
				winners = append(winners, uint64(100+i))
			case !errors.Is(errs[i], ErrStLedgerV2ActivationRecorded):
				t.Fatalf("writer %d of a differing activation: %v", i, errs[i])
			}
		}
		got := GetStLedgerV2Activation(ctx, differing)
		if len(winners) != 1 || got == nil || got.FirstEpoch != winners[0] || rows(differing) != 1 {
			t.Fatalf("winners %v, recorded %+v in %d rows", winners, got, rows(differing))
		}
	})
}
