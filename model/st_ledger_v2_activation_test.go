package model

// st_ledger_v2_activation_test.go — pg-backed tests of the settlement v2
// activation record (SV-9).

import (
	"context"
	"errors"
	"reflect"
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
