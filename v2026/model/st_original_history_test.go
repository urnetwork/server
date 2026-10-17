// History custody is tested at the actual SQL transaction and append-only guard.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/urnetwork/server/v2026"
)

// Typed original logs retain every field needed to independently decode a bind.
func testStOriginalEvent(t testing.TB) *StChainEvent {
	original := types.Log{Address: common.Address{1}, Topics: []common.Hash{{2}}, Data: []byte{3}, BlockNumber: 41, BlockHash: common.Hash{4}, TxHash: common.Hash{5}, TxIndex: 2, Index: 1}
	encoded, err := json.Marshal(&original)
	if err != nil {
		t.Fatal(err)
	}
	return &StChainEvent{BlockNumber: original.BlockNumber, BlockHash: original.BlockHash.Hex(), LogIndex: int(original.Index), TxHash: original.TxHash.Hex(), Kind: "HeadBound", DataJson: `{"synthetic":true}`, OriginalLog: encoded}
}

// Same-slot contradictory data is a hard refusal; exact repeats remain harmless.
func TestStEventOriginalRejectsContradictoryReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		event := testStOriginalEvent(t)
		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{event})
		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{event})
		changed := *event
		changed.DataJson = `{"synthetic":false}`
		recovered := server.HandleError(func() { UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{&changed}) })
		cause, ok := recovered.(error)
		if !ok || !errors.Is(cause, ErrStEventOriginalIntegrity) {
			t.Fatalf("contradictory event: %v", recovered)
		}
		rows := GetStEvents(ctx, testStDeploymentKey, 41, 41)
		if len(rows) != 1 || !bytes.Equal(rows[0].OriginalLog, event.OriginalLog) || rows[0].DataJson != event.DataJson {
			t.Fatal("original log changed after refused replay")
		}
		if err := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM st_event WHERE deployment_key=$1`, string(testStDeploymentKey)))
			})
		}); err == nil {
			t.Fatal("event original delete succeeded")
		}
	})
}

// Claimed index/hash/removal fields must match the original returned log.
func TestStEventOriginalBindsEveryRoutingField(t *testing.T) {
	original := testStOriginalEvent(t)
	if err := validateStEventOriginal(original); err != nil {
		t.Fatal(err)
	}
	edits := []func(*StChainEvent){
		func(event *StChainEvent) { event.BlockNumber++ },
		func(event *StChainEvent) { event.LogIndex++ },
		func(event *StChainEvent) { event.BlockHash = common.Hash{9}.Hex() },
		func(event *StChainEvent) { event.TxHash = common.Hash{10}.Hex() },
		func(event *StChainEvent) { event.OriginalLog = []byte(`{}`) },
	}
	for index, edit := range edits {
		changed := *original
		edit(&changed)
		if err := validateStEventOriginal(&changed); !errors.Is(err, ErrStEventOriginalIntegrity) {
			t.Fatalf("routing case%d: %v", index, err)
		}
	}
}

// A historical missing log remains missing after a new reader replays the block.
func TestStEventOriginalDoesNotBackfillHistoricalCustody(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		event := testStOriginalEvent(t)
		historical := *event
		historical.OriginalLog = nil
		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{&historical})
		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{event})
		if rows := GetStEvents(ctx, testStDeploymentKey, 41, 41); len(rows) != 1 || len(rows[0].OriginalLog) != 0 {
			t.Fatal("later RPC read manufactured original custody")
		}
	})
}

// Exact challenge bytes persist prospectively; legacy unsigned history stays nil.
func TestStProviderWalletOriginalRetainsChallenge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId, networkId := server.NewId(), server.NewId()
		message, signature := "synthetic original challenge\nexact bytes", "synthetic-original-signature"
		SetStWalletOriginal(ctx, networkId, &clientId, "synthetic-coldkey", [32]byte{1}, &message, &signature)
		row := GetStProviderWalletsAt(ctx, server.NowUtc().Add(time.Second))[clientId]
		if row == nil || row.OriginalMessage == nil || *row.OriginalMessage != message || row.OriginalSignature == nil || *row.OriginalSignature != signature {
			t.Fatalf("wallet original missing: %+v", row)
		}
		networkRows := GetStProviderWalletsForNetwork(ctx, networkId)
		if len(networkRows) != 1 || networkRows[0].OriginalSignature == nil || *networkRows[0].OriginalSignature != signature {
			t.Fatal("network reader dropped original")
		}
		legacyId := server.NewId()
		SetStProviderWallet(ctx, legacyId, networkId, "synthetic-legacy-coldkey", [32]byte{2})
		legacy := GetStProviderWalletsAt(ctx, server.NowUtc().Add(time.Second))[legacyId]
		if legacy == nil || legacy.OriginalMessage != nil || legacy.OriginalSignature != nil {
			t.Fatal("legacy wallet fabricated original")
		}
		if err := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `UPDATE st_provider_wallet_history SET original_signature='changed' WHERE client_id=$1`, clientId))
			})
		}); err == nil {
			t.Fatal("wallet original mutation succeeded")
		}
	})
}

// A failed network projection rolls back the provider original in the same tx.
func TestStWalletOriginalPublicationIsAtomic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId, networkId := server.NewId(), server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION st_test_refuse_wallet() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic wallet storage refusal'; END $$; CREATE TRIGGER st_test_refuse_wallet BEFORE INSERT ON st_wallet FOR EACH ROW EXECUTE FUNCTION st_test_refuse_wallet()`))
		})
		defer server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER IF EXISTS st_test_refuse_wallet ON st_wallet; DROP FUNCTION IF EXISTS st_test_refuse_wallet()`))
		})
		message, signature := "synthetic challenge", "synthetic signature"
		if err := server.HandleError(func() {
			SetStWalletOriginal(ctx, networkId, &clientId, "synthetic-coldkey", [32]byte{1}, &message, &signature)
		}); err == nil {
			t.Fatal("synthetic wallet refusal not reached")
		}
		if row := GetStWallet(ctx, networkId); row != nil {
			t.Fatal("failed wallet changed network projection")
		}
		if row := GetStProviderWalletsAt(ctx, server.NowUtc().Add(time.Second))[clientId]; row != nil {
			t.Fatal("failed wallet left orphan original")
		}
	})
}

// Replacing a mutable binding head never erases either original consent.
func TestStBindingOriginalRetainsEachAcceptedConsent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		signature := &StFleetBindingSignature{DeploymentKey: testStDeploymentKey, ClientId: server.NewId(), NetworkId: server.NewId(), Generation: 1, Hotkey: [32]byte{1}, Digest: [32]byte{2}, BindingJson: `{"synthetic":1}`, ClientSignature: []byte{3}, CreateTime: server.NowUtc()}
		SetStFleetBindingSignature(ctx, signature)
		signature.CreateTime = signature.CreateTime.Add(time.Second)
		SetStFleetBindingSignature(ctx, signature)
		if rows := ListStFleetBindingOriginals(ctx, testStDeploymentKey, signature.ClientId, 100); len(rows) != 1 {
			t.Fatalf("exact consent retry count=%d", len(rows))
		}
		signature.BindingJson = `{"synthetic":2}`
		signature.Digest = [32]byte{4}
		signature.ClientSignature = []byte{5}
		SetStFleetBindingSignature(ctx, signature)
		rows := ListStFleetBindingOriginals(ctx, testStDeploymentKey, signature.ClientId, 100)
		if len(rows) != 2 {
			t.Fatalf("accepted original histories=%d", len(rows))
		}
		seen := map[string]bool{}
		for _, encoded := range rows {
			var original StFleetBindingSignature
			if err := json.Unmarshal(encoded, &original); err != nil {
				t.Fatal(err)
			}
			seen[original.BindingJson] = true
		}
		if !seen[`{"synthetic":1}`] || !seen[`{"synthetic":2}`] {
			t.Fatal("binding head replacement erased an original")
		}
		if err := server.HandleError(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM st_fleet_binding_original WHERE client_id=$1`, signature.ClientId))
			})
		}); err == nil {
			t.Fatal("binding original delete succeeded")
		}
	})
}
