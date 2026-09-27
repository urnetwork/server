package model

// st_model_db_test.go — pg/redis-backed integration tests for the st
// settlement model (the serviceless pure tests live in st_model_test.go).
// Run with the local services from test.sh:
//
//	WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test \
//	WARP_VERSION=0.0.0 BRINGYOUR_POSTGRES_HOSTNAME=local-pg.bringyour.com \
//	BRINGYOUR_REDIS_HOSTNAME=local-redis.bringyour.com go test ./model -run TestSt

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

const testStDeploymentKey StDeploymentKey = "945:0x2000000000000000000000000000000000000002"

// A replacement coordinator commonly starts its epoch counter near values
// already mirrored by a prior testnet attempt. Prove every collision-prone
// state surface can retain both histories while exposing only the requested
// chain/coordinator identity.
func TestStDeploymentStateIsIsolatedAcrossCoordinatorReplacements(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		keyA := StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		keyB := StDeploymentKey("945:0x2000000000000000000000000000000000000002")

		UpsertStEpoch(ctx, keyA, &StEpoch{Epoch: 7, StartBlock: 100, Status: StEpochStatusOpen})
		UpsertStEpoch(ctx, keyB, &StEpoch{Epoch: 7, StartBlock: 900, Status: StEpochStatusOpen})
		SetStEpochStatus(ctx, keyA, 7, StEpochStatusCommitted)
		if a, b := GetStEpoch(ctx, keyA, 7), GetStEpoch(ctx, keyB, 7); a == nil || b == nil || a.StartBlock != 100 || a.Status != StEpochStatusCommitted || b.StartBlock != 900 || b.Status != StEpochStatusOpen {
			t.Fatalf("isolated epochs A/B=%+v/%+v", a, b)
		}

		eventA := &StChainEvent{BlockNumber: 101, BlockHash: "0xaaa", LogIndex: 0, TxHash: "0x1", Kind: "Fixture", DataJson: `{"deployment":"a"}`}
		eventB := &StChainEvent{BlockNumber: 101, BlockHash: "0xbbb", LogIndex: 0, TxHash: "0x2", Kind: "Fixture", DataJson: `{"deployment":"b"}`}
		UpsertStEvents(ctx, keyA, []*StChainEvent{eventA})
		UpsertStEvents(ctx, keyB, []*StChainEvent{eventB})
		if a, b := GetStEvents(ctx, keyA, 101, 101), GetStEvents(ctx, keyB, 101, 101); len(a) != 1 || len(b) != 1 || a[0].DataJson == b[0].DataJson {
			t.Fatalf("isolated events A/B=%+v/%+v", a, b)
		}

		SetStChainCheckpoint(ctx, keyA, StChainCheckpoint{NextBlock: 110, BlockHash: "0xaaa"})
		SetStChainCheckpoint(ctx, keyB, StChainCheckpoint{NextBlock: 910, BlockHash: "0xbbb"})
		if a, b := GetStChainCheckpoint(ctx, keyA), GetStChainCheckpoint(ctx, keyB); a.NextBlock != 110 || b.NextBlock != 910 || a.BlockHash == b.BlockHash {
			t.Fatalf("isolated checkpoints A/B=%+v/%+v", a, b)
		}

		networkA, networkB := server.NewId(), server.NewId()
		SetStPayoutLeaves(ctx, keyA, 7, 1, []*StPayoutLeaf{{Epoch: 7, NoId: 1, NetworkId: networkA, Coldkey: testStCkey(1), ShareBps: 10000}})
		SetStPayoutLeaves(ctx, keyB, 7, 1, []*StPayoutLeaf{{Epoch: 7, NoId: 1, NetworkId: networkB, Coldkey: testStCkey(2), ShareBps: 10000}})
		if a, b := GetStPayoutLeaves(ctx, keyA, 7, 1), GetStPayoutLeaves(ctx, keyB, 7, 1); len(a) != 1 || len(b) != 1 || a[0].Coldkey == b[0].Coldkey {
			t.Fatalf("isolated leaves A/B=%+v/%+v", a, b)
		}

		now := time.Now().UTC()
		AddStPayoutArtifact(ctx, keyA, &StPayoutArtifact{Epoch: 7, NoId: 1, ContentHash: "sha256:" + strings.Repeat("a", 64), ContentKey: "a", HistoryKey: "history-a", PayoutRoot: testStCkey(3), CreateTime: now})
		AddStPayoutArtifact(ctx, keyB, &StPayoutArtifact{Epoch: 7, NoId: 1, ContentHash: "sha256:" + strings.Repeat("b", 64), ContentKey: "b", HistoryKey: "history-b", PayoutRoot: testStCkey(4), CreateTime: now})
		if a, b := GetStPayoutArtifact(ctx, keyA, 7, 1), GetStPayoutArtifact(ctx, keyB, 7, 1); a == nil || b == nil || a.ContentHash == b.ContentHash {
			t.Fatalf("isolated artifacts A/B=%+v/%+v", a, b)
		}

		AddStPublish(ctx, keyA, 7, StPublishKindCommit)
		AddStPublish(ctx, keyB, 7, StPublishKindDeposit)
		if a, b := GetStPublishes(ctx, keyA, 7), GetStPublishes(ctx, keyB, 7); len(a) != 1 || len(b) != 1 || a[0].Kind != StPublishKindCommit || b[0].Kind != StPublishKindDeposit {
			t.Fatalf("isolated publishes A/B=%+v/%+v", a, b)
		}

		SetStEpochSummaryCache(ctx, keyA, &StEpochSummary{Epoch: 7, StartBlock: 100}, time.Minute)
		SetStEpochSummaryCache(ctx, keyB, &StEpochSummary{Epoch: 7, StartBlock: 900}, time.Minute)
		if a, b := GetStEpochSummaryCache(ctx, keyA), GetStEpochSummaryCache(ctx, keyB); a == nil || b == nil || a.StartBlock != 100 || b.StartBlock != 900 {
			t.Fatalf("isolated summaries A/B=%+v/%+v", a, b)
		}

		// The earnings APIs were developed on a branch which predated deployment
		// scoping. Equal epoch and generation numbers must remain independent too;
		// otherwise a replacement coordinator can read old shares/consent or have
		// its notification suppressed by the prior deployment.
		SetStEpochStatus(ctx, keyA, 7, StEpochStatusFinalized)
		SetStEpochStatus(ctx, keyB, 7, StEpochStatusFinalized)
		if a, b := GetFinalizedStEpochs(ctx, keyA, 1), GetFinalizedStEpochs(ctx, keyB, 1); len(a) != 1 || len(b) != 1 || a[0].StartBlock != 100 || b[0].StartBlock != 900 {
			t.Fatalf("isolated finalized earnings epochs A/B=%+v/%+v", a, b)
		}
		if shares := GetStPayoutNetworkShares(ctx, keyA, 7, 1); len(shares) != 1 || shares[networkA] != 10000 {
			t.Fatalf("deployment A payout shares crossed deployments: %+v", shares)
		}
		if shares := GetStPayoutShareBpsForNetwork(ctx, keyB, networkB, 1, []uint64{7}); len(shares) != 1 || shares[7] != 10000 {
			t.Fatalf("deployment B account shares crossed deployments: %+v", shares)
		}

		ckey := testStCkey(5)
		hotkeyA, hotkeyB := testStCkey(6), testStCkey(7)
		UpsertStHeadBinding(ctx, keyA, ckey, hotkeyA, 11, true, 100)
		UpsertStHeadBinding(ctx, keyB, ckey, hotkeyB, 22, true, 100)
		if a, b := GetActiveStHeadBindingsForCkeys(ctx, keyA, [][32]byte{ckey})[ckey], GetActiveStHeadBindingsForCkeys(ctx, keyB, [][32]byte{ckey})[ckey]; a == nil || b == nil || a.Hotkey != hotkeyA || b.Hotkey != hotkeyB {
			t.Fatalf("isolated active head bindings A/B=%+v/%+v", a, b)
		}

		clientId := server.NewId()
		SetStFleetBindingSignature(ctx, &StFleetBindingSignature{DeploymentKey: keyA, ClientId: clientId, NetworkId: networkA, Generation: 1, Hotkey: hotkeyA, Digest: testStCkey(8), BindingJson: "a", ClientSignature: []byte{1}, CreateTime: now})
		SetStFleetBindingSignature(ctx, &StFleetBindingSignature{DeploymentKey: keyB, ClientId: clientId, NetworkId: networkB, Generation: 1, Hotkey: hotkeyB, Digest: testStCkey(9), BindingJson: "b", ClientSignature: []byte{2}, CreateTime: now})
		if a, b := GetStFleetBindingSignature(ctx, keyA, clientId, 1), GetStFleetBindingSignature(ctx, keyB, clientId, 1); a == nil || b == nil || a.BindingJson != "a" || b.BindingJson != "b" {
			t.Fatalf("isolated fleet binding signatures A/B=%+v/%+v", a, b)
		}

		if !ClaimStEpochNotification(ctx, keyA, 7) || !ClaimStEpochNotification(ctx, keyB, 7) {
			t.Fatal("equal epochs in distinct deployments did not receive independent notification claims")
		}
		if ClaimStEpochNotification(ctx, keyA, 7) || ClaimStEpochNotification(ctx, keyB, 7) {
			t.Fatal("notification claim was not once per deployment and epoch")
		}
	})
}

// Nonce ownership crosses human profiles and coordinator replacements while
// logical replay remains isolated to one exact chain/coordinator operation.
func TestStTransactionIntentReservationUsesChainAccountNonceScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		deploymentIdA := server.NewId().String()
		deploymentIdB := server.NewId().String()
		deploymentKeyA := StDeploymentKey("945:0x1000000000000000000000000000000000000001")
		deploymentKeyB := StDeploymentKey("945:0x2000000000000000000000000000000000000002")
		fromAddress := "0x" + strings.Repeat("1", 40)
		otherFromAddress := "0x" + strings.Repeat("2", 40)
		toAddress := "0x" + strings.Repeat("3", 40)
		calldataHash := "0x" + strings.Repeat("4", 64)
		calldata := []byte{1, 2, 3}
		genesisHash := "0x" + strings.Repeat("a", 64)

		first := ReserveStTransactionIntent(
			ctx, "test-intent-first", "testnet", deploymentIdA, deploymentKeyA, 945,
			genesisHash,
			fromAddress, toAddress, calldataHash, calldata, 7,
		)
		if first == nil || first.Nonce != 7 {
			t.Fatalf("first reservation = %+v, want nonce 7", first)
		}
		replayed := ReserveStTransactionIntent(
			ctx, "test-intent-first", "renamed-profile", deploymentIdB, deploymentKeyA, 945,
			genesisHash,
			fromAddress, toAddress, calldataHash, calldata, 99,
		)
		if replayed == nil || replayed.IntentId != first.IntentId || replayed.Nonce != first.Nonce {
			t.Fatalf("idempotent replay = %+v, want intent %s nonce %d", replayed, first.IntentId, first.Nonce)
		}
		replacement := ReserveStTransactionIntent(
			ctx, "test-intent-replacement", "another-profile", deploymentIdB, deploymentKeyB, 945,
			genesisHash,
			fromAddress, toAddress, calldataHash, []byte{4, 5, 6}, 1,
		)
		if replacement == nil || replacement.Nonce != 8 {
			t.Fatalf("replacement reservation = %+v, want global account nonce 8", replacement)
		}
		other := ReserveStTransactionIntent(
			ctx, "test-intent-other-account", "testnet", deploymentIdA, deploymentKeyA, 945,
			genesisHash,
			otherFromAddress, toAddress, calldataHash, calldata, 3,
		)
		if other == nil || other.Nonce != 3 {
			t.Fatalf("other-account reservation = %+v, want nonce 3", other)
		}
		otherChain := ReserveStTransactionIntent(
			ctx, "test-intent-other-chain", "mainnet", deploymentIdA,
			StDeploymentKey("964:0x1000000000000000000000000000000000000001"), 964,
			"0x"+strings.Repeat("b", 64),
			fromAddress, toAddress, calldataHash, calldata, 4,
		)
		if otherChain == nil || otherChain.Nonce != 4 {
			t.Fatalf("other-chain reservation = %+v, want nonce 4", otherChain)
		}
		resetChain := ReserveStTransactionIntent(
			ctx, "test-intent-reset-chain", "testnet", deploymentIdA, deploymentKeyA, 945,
			"0x"+strings.Repeat("e", 64), fromAddress, toAddress, calldataHash, calldata, 7,
		)
		if resetChain == nil || resetChain.Nonce != 7 {
			t.Fatalf("reset-chain reservation = %+v, want independently reusable nonce 7", resetChain)
		}
	})
}

// A canonical revert consumes the account nonce but leaves contract state
// unchanged. Concurrent, byte-identical retries get one successor generation;
// immutable drift still fails closed instead of selecting another operation.
func TestStTransactionRevertRetryCreatesOneImmutableSuccessor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		deploymentId := server.NewId().String()
		genesisHash := "0x" + strings.Repeat("f", 64)
		fromAddress := "0x" + strings.Repeat("a", 40)
		toAddress := "0x" + strings.Repeat("b", 40)
		calldataHash := "0x" + strings.Repeat("c", 64)
		calldata := []byte{1, 2, 3}
		reserve := func(dataHash string, data []byte) *StTransactionIntent {
			return ReserveStTransactionIntent(
				ctx, "test-revert-generation", "testnet", deploymentId, testStDeploymentKey,
				945, genesisHash, fromAddress, toAddress, dataHash, data, 23,
			)
		}
		intent := reserve(calldataHash, calldata)
		price := "1"
		attempt := AddStTransactionAttempt(ctx, &StTransactionAttempt{
			IntentId: intent.IntentId, Attempt: 1, Kind: StTxAttemptExecution, TxHash: "0x" + strings.Repeat("d", 64),
			RawTransaction: []byte{1}, GasLimit: 21_000, GasPrice: &price,
		})
		MarkStTransactionReverted(ctx, intent.IntentId, attempt.Attempt, errors.New("canonical revert"))

		start := make(chan struct{})
		results := make(chan *StTransactionIntent, 2)
		for range 2 {
			go func() {
				<-start
				results <- reserve(calldataHash, calldata)
			}()
		}
		close(start)
		first, second := <-results, <-results
		if first == nil || second == nil || first.IntentId != second.IntentId || first.Generation != 1 || first.Nonce != 24 {
			t.Fatalf("revert retries did not converge: %+v / %+v", first, second)
		}

		panicked := func() (panicked bool) {
			defer func() {
				panicked = recover() != nil
			}()
			reserve("0x"+strings.Repeat("e", 64), []byte{9})
			return false
		}()
		if !panicked {
			t.Fatal("changed calldata reused an active logical operation")
		}
	})
}

// Two signers racing from the same stale snapshot must converge on the exact
// bytes persisted by the database for both initial and replacement attempts.
func TestStTransactionAttemptCandidatesConvergeOnOneWinner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		genesisHash := "0x" + strings.Repeat("c", 64)
		fromAddress := "0x" + strings.Repeat("5", 40)
		intent := ReserveStTransactionIntent(
			ctx, "test-attempt-race", "testnet", server.NewId().String(), testStDeploymentKey,
			945, genesisHash, fromAddress, "0x"+strings.Repeat("6", 40),
			"0x"+strings.Repeat("7", 64), []byte{1, 2, 3}, 11,
		)
		price := "1"
		candidate := func(attempt int, marker byte) *StTransactionAttempt {
			return &StTransactionAttempt{
				IntentId: intent.IntentId, Attempt: attempt, Kind: StTxAttemptExecution,
				TxHash:         "0x" + strings.Repeat(fmt.Sprintf("%x", marker), 64),
				RawTransaction: []byte{marker}, GasLimit: 21_000, GasPrice: &price,
			}
		}

		start := make(chan struct{})
		results := make(chan *StTransactionAttempt, 2)
		for _, attemptCandidate := range []*StTransactionAttempt{candidate(1, 1), candidate(1, 2)} {
			go func(candidate *StTransactionAttempt) {
				<-start
				results <- AddStTransactionAttempt(ctx, candidate)
			}(attemptCandidate)
		}
		close(start)
		first, second := <-results, <-results
		if first == nil || second == nil || first.TxHash != second.TxHash || first.Attempt != 1 || second.Attempt != 1 {
			t.Fatalf("initial candidates did not converge: %+v / %+v", first, second)
		}

		MarkStTransactionUncertain(ctx, intent.IntentId, 1, errors.New("replace now"))
		start = make(chan struct{})
		results = make(chan *StTransactionAttempt, 2)
		for _, attemptCandidate := range []*StTransactionAttempt{candidate(2, 3), candidate(2, 4)} {
			go func(candidate *StTransactionAttempt) {
				<-start
				results <- AddStTransactionAttempt(ctx, candidate)
			}(attemptCandidate)
		}
		close(start)
		first, second = <-results, <-results
		if first == nil || second == nil || first.TxHash != second.TxHash || first.Attempt != 2 || second.Attempt != 2 {
			t.Fatalf("replacement candidates did not converge: %+v / %+v", first, second)
		}
		attempts := GetStTransactionAttempts(ctx, intent.IntentId)
		if len(attempts) != 2 || attempts[0].Attempt != 2 || attempts[0].Kind != StTxAttemptExecution || attempts[1].Attempt != 1 || attempts[1].Status != StTxAttemptReplaced {
			t.Fatalf("durable attempts = %+v", attempts)
		}
	})
}

// A finalized self-cancellation is an absorbing nonce outcome; late evidence
// from the obsolete business attempt cannot revive or finalize that intent.
func TestStTransactionCancellationCannotRegress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		intent := ReserveStTransactionIntent(
			ctx, "test-cancel-state", "testnet", server.NewId().String(), testStDeploymentKey,
			945, "0x"+strings.Repeat("1", 64), "0x"+strings.Repeat("c", 40),
			"0x"+strings.Repeat("d", 40), "0x"+strings.Repeat("2", 64), []byte{1}, 31,
		)
		price := "1"
		execution := AddStTransactionAttempt(ctx, &StTransactionAttempt{
			IntentId: intent.IntentId, Attempt: 1, Kind: StTxAttemptExecution,
			TxHash: "0x" + strings.Repeat("3", 64), RawTransaction: []byte{1}, GasLimit: 21_000, GasPrice: &price,
		})
		MarkStTransactionUncertain(ctx, intent.IntentId, 1, errors.New("stale deployment"))
		cancellation := AddStTransactionAttempt(ctx, &StTransactionAttempt{
			IntentId: intent.IntentId, Attempt: 2, Kind: StTxAttemptCancellation,
			TxHash: "0x" + strings.Repeat("4", 64), RawTransaction: []byte{2}, GasLimit: 21_000, GasPrice: &price,
		})
		MarkStTransactionCanceled(ctx, intent.IntentId, 2, cancellation.TxHash, 120, "0xfinal120", nil)

		MarkStTransactionMined(ctx, intent.IntentId, 1, execution.TxHash, 119, "0xlate119")
		MarkStTransactionFinalized(ctx, intent.IntentId, 1, execution.TxHash, 121, "0xfinal121")
		MarkStTransactionReverted(ctx, intent.IntentId, 1, errors.New("late revert"))

		stored := GetStTransactionIntent(ctx, intent.LogicalKey)
		if stored == nil || stored.Status != StTxCanceled || stored.CurrentTxHash == nil || *stored.CurrentTxHash != cancellation.TxHash {
			t.Fatalf("canceled intent regressed: %+v", stored)
		}
		attempts := GetStTransactionAttempts(ctx, intent.IntentId)
		if len(attempts) != 2 || attempts[0].Kind != StTxAttemptCancellation || attempts[0].Status != StTxCanceled || attempts[0].FinalizedBlock == nil || *attempts[0].FinalizedBlock != 120 {
			t.Fatalf("cancellation evidence regressed: %+v", attempts)
		}
		if unresolved := GetUnresolvedStTransactionIntents(ctx, intent.ChainId, intent.GenesisHash, intent.FromAddress); len(unresolved) != 0 {
			t.Fatalf("terminal cancellation remained unresolved: %+v", unresolved)
		}
	})
}

// Canonical finality is absorbing even when stale broadcasters, receipt
// readers, or timeout handlers report weaker observations afterward.
func TestStTransactionFinalizedAttemptCannotRegress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		intent := ReserveStTransactionIntent(
			ctx, "test-terminal-state", "testnet", server.NewId().String(), testStDeploymentKey,
			945, "0x"+strings.Repeat("d", 64), "0x"+strings.Repeat("8", 40),
			"0x"+strings.Repeat("9", 40), "0x"+strings.Repeat("a", 64), []byte{1}, 17,
		)
		price := "1"
		attemptOne := AddStTransactionAttempt(ctx, &StTransactionAttempt{
			IntentId: intent.IntentId, Attempt: 1, Kind: StTxAttemptExecution, TxHash: "0x" + strings.Repeat("1", 64),
			RawTransaction: []byte{1}, GasLimit: 21_000, GasPrice: &price,
		})
		MarkStTransactionUncertain(ctx, intent.IntentId, 1, errors.New("replacement eligible"))
		attemptTwo := AddStTransactionAttempt(ctx, &StTransactionAttempt{
			IntentId: intent.IntentId, Attempt: 2, Kind: StTxAttemptExecution, TxHash: "0x" + strings.Repeat("2", 64),
			RawTransaction: []byte{2}, GasLimit: 21_000, GasPrice: &price,
		})
		MarkStTransactionMined(ctx, intent.IntentId, 1, attemptOne.TxHash, 100, "0xblock100")
		MarkStTransactionFinalized(ctx, intent.IntentId, 1, attemptOne.TxHash, 110, "0xfinal110")

		MarkStTransactionBroadcast(ctx, intent.IntentId, 2)
		MarkStTransactionUncertain(ctx, intent.IntentId, 2, errors.New("late timeout"))
		MarkStTransactionMined(ctx, intent.IntentId, 2, attemptTwo.TxHash, 101, "0xblock101")
		MarkStTransactionReverted(ctx, intent.IntentId, 2, errors.New("late revert"))

		stored := GetStTransactionIntent(ctx, intent.LogicalKey)
		if stored == nil || stored.Status != StTxFinalized || stored.CurrentTxHash == nil || *stored.CurrentTxHash != attemptOne.TxHash || stored.Error != nil {
			t.Fatalf("terminal intent regressed: %+v", stored)
		}
		attempts := GetStTransactionAttempts(ctx, intent.IntentId)
		if len(attempts) != 2 || attempts[1].Status != StTxFinalized || attempts[1].FinalizedBlock == nil || *attempts[1].FinalizedBlock != 110 {
			t.Fatalf("winning attempt regressed: %+v", attempts)
		}
	})
}

func TestStWalletRoundtrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()

		if GetStWallet(ctx, networkId) != nil {
			t.Fatal("unset wallet must be nil")
		}

		coldkeyA := testStCkey(1)
		SetStWallet(ctx, networkId, "5A...a", coldkeyA)
		wallet := GetStWallet(ctx, networkId)
		if wallet == nil || wallet.ColdkeyPubkey != coldkeyA || wallet.ColdkeySs58 != "5A...a" {
			t.Fatalf("wallet = %+v", wallet)
		}

		// upsert replaces (one wallet per network)
		coldkeyB := testStCkey(2)
		SetStWallet(ctx, networkId, "5B...b", coldkeyB)
		all := GetAllStWalletColdkeys(ctx)
		if len(all) != 1 || all[networkId] != coldkeyB {
			t.Fatalf("coldkeys = %v, want only the replacement", all)
		}
	})
}

func TestStEpochLifecycleMonotonic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		UpsertStEpoch(ctx, testStDeploymentKey, &StEpoch{
			Epoch: 7, StartBlock: 100, CommitDeadlineBlock: 210,
			TrailsDeadlineBlock: 260, FinalizeBlock: 300, Status: StEpochStatusOpen,
		})
		SetStEpochStatus(ctx, testStDeploymentKey, 7, StEpochStatusCommitted)

		// a late window refresh must update the blocks but never regress status
		UpsertStEpoch(ctx, testStDeploymentKey, &StEpoch{
			Epoch: 7, StartBlock: 101, CommitDeadlineBlock: 211,
			TrailsDeadlineBlock: 261, FinalizeBlock: 301, Status: StEpochStatusOpen,
		})
		row := GetStEpoch(ctx, testStDeploymentKey, 7)
		if row.Status != StEpochStatusCommitted || row.StartBlock != 101 {
			t.Fatalf("after refresh: %+v (status must hold, blocks must update)", row)
		}

		// direct status regress is a no-op
		SetStEpochStatus(ctx, testStDeploymentKey, 7, StEpochStatusClosed)
		if row = GetStEpoch(ctx, testStDeploymentKey, 7); row.Status != StEpochStatusCommitted {
			t.Fatalf("status regressed to %s", row.Status)
		}

		// finalize records finalized_time exactly once
		SetStEpochStatus(ctx, testStDeploymentKey, 7, StEpochStatusFinalized)
		row = GetStEpoch(ctx, testStDeploymentKey, 7)
		if row.Status != StEpochStatusFinalized || row.FinalizedTime == nil {
			t.Fatalf("finalize: %+v", row)
		}
		finalizedTime := *row.FinalizedTime
		SetStEpochStatus(ctx, testStDeploymentKey, 7, StEpochStatusFinalized)
		if row = GetStEpoch(ctx, testStDeploymentKey, 7); !row.FinalizedTime.Equal(finalizedTime) {
			t.Fatal("finalized_time must be set once")
		}

		UpsertStEpoch(ctx, testStDeploymentKey, &StEpoch{Epoch: 9, Status: StEpochStatusOpen})
		if latest := GetLatestStEpoch(ctx, testStDeploymentKey); latest == nil || latest.Epoch != 9 {
			t.Fatalf("latest = %+v", latest)
		}
		if finalized := GetLatestFinalizedStEpoch(ctx, testStDeploymentKey); finalized == nil || finalized.Epoch != 7 {
			t.Fatalf("latest finalized = %+v", finalized)
		}
		open := GetStEpochsWithStatus(ctx, testStDeploymentKey, StEpochStatusOpen)
		if len(open) != 1 || open[0].Epoch != 9 {
			t.Fatalf("open epochs = %+v", open)
		}
	})
}

func TestStPayoutLeavesReplaceAndLookup(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const epoch, noId = uint64(3), uint64(1)

		nA, nB := server.NewId(), server.NewId()
		SetStPayoutLeaves(ctx, testStDeploymentKey, epoch, noId, []*StPayoutLeaf{
			{Epoch: epoch, NoId: noId, NetworkId: nA, Coldkey: testStCkey(1), ShareBps: 4000, LeafIndex: 0},
			{Epoch: epoch, NoId: noId, NetworkId: nB, Coldkey: testStCkey(2), ShareBps: 6000, LeafIndex: 1},
		})

		// idempotent recompute before the on-chain commit: full replace
		SetStPayoutLeaves(ctx, testStDeploymentKey, epoch, noId, []*StPayoutLeaf{
			{Epoch: epoch, NoId: noId, NetworkId: nB, Coldkey: testStCkey(2), ShareBps: 10000, LeafIndex: 0},
		})

		leaves := GetStPayoutLeaves(ctx, testStDeploymentKey, epoch, noId)
		if len(leaves) != 1 || leaves[0].Coldkey != testStCkey(2) || leaves[0].ShareBps != 10000 {
			t.Fatalf("leaves = %+v, want the replacement set only", leaves)
		}

		if leaf := GetStPayoutLeafForColdkey(ctx, testStDeploymentKey, epoch, noId, testStCkey(2)); leaf == nil || leaf.ShareBps != 10000 {
			t.Fatalf("coldkey leaf = %+v", leaf)
		}
		if leaf := GetStPayoutLeafForColdkey(ctx, testStDeploymentKey, epoch, noId, testStCkey(1)); leaf != nil {
			t.Fatalf("replaced coldkey still has a leaf: %+v", leaf)
		}
	})
}

func TestStPublishLifecycle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		publishId := AddStPublish(ctx, testStDeploymentKey, 4, StPublishKindCommit)
		txHash := "0xabc"
		UpdateStPublish(ctx, publishId, StPublishStatusConfirmed, &txHash, nil)
		AddStPublish(ctx, testStDeploymentKey, 4, StPublishKindDeposit)

		publishes := GetStPublishes(ctx, testStDeploymentKey, 4)
		if len(publishes) != 2 {
			t.Fatalf("publishes = %+v", publishes)
		}
		if publishes[0].Kind != StPublishKindCommit || publishes[0].Status != StPublishStatusConfirmed ||
			publishes[0].TxHash == nil || *publishes[0].TxHash != txHash {
			t.Fatalf("resolved publish = %+v", publishes[0])
		}
		if publishes[1].Status != StPublishStatusPending {
			t.Fatalf("second publish = %+v", publishes[1])
		}
	})
}

func TestStEventsDedupOrderAndHighWater(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{
			{BlockNumber: 5, LogIndex: 2, TxHash: "0x1", Kind: "HeadBound", DataJson: `{"a":1}`},
			{BlockNumber: 5, LogIndex: 0, TxHash: "0x1", Kind: "HeadUnbound", DataJson: `{}`},
			{BlockNumber: 3, LogIndex: 7, TxHash: "0x0", Kind: "OperatorCommitted", DataJson: `{}`},
		})
		// conservative re-scan: the duplicate (5, 2) must be ignored, first write wins
		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{
			{BlockNumber: 5, LogIndex: 2, TxHash: "0x1", Kind: "HeadBound", DataJson: `{"a":2}`},
		})

		events := GetStEvents(ctx, testStDeploymentKey, 0, 10)
		if len(events) != 3 {
			t.Fatalf("events = %+v", events)
		}
		// ordered by (block, log)
		if events[0].BlockNumber != 3 || events[1].LogIndex != 0 || events[2].LogIndex != 2 {
			t.Fatalf("order = %+v", events)
		}
		if events[2].DataJson != `{"a":1}` {
			t.Fatalf("dedup must keep the first write, got %s", events[2].DataJson)
		}

		SetStHighWaterBlock(ctx, testStDeploymentKey, 100)
		SetStHighWaterBlock(ctx, testStDeploymentKey, 50) // never moves backward
		if block := GetStHighWaterBlock(ctx, testStDeploymentKey); block != 100 {
			t.Fatalf("high water = %d, want 100", block)
		}
		SetStHighWaterBlock(ctx, testStDeploymentKey, 150)
		if block := GetStHighWaterBlock(ctx, testStDeploymentKey); block != 150 {
			t.Fatalf("high water = %d, want 150", block)
		}

		SetStChainCheckpoint(ctx, testStDeploymentKey, StChainCheckpoint{NextBlock: 200, BlockHash: "0xabc"})
		SetStHighWaterBlock(ctx, testStDeploymentKey, 175)
		checkpoint := GetStChainCheckpoint(ctx, testStDeploymentKey)
		if checkpoint.NextBlock != 200 || checkpoint.BlockHash != "0xabc" {
			t.Fatalf("stale high water changed canonical checkpoint: %+v", checkpoint)
		}

		SetStHighWaterBlock(ctx, testStDeploymentKey, 250)
		checkpoint = GetStChainCheckpoint(ctx, testStDeploymentKey)
		if checkpoint.NextBlock != 250 || checkpoint.BlockHash != "" {
			t.Fatalf("advanced hashless checkpoint = %+v", checkpoint)
		}
	})
}

// SumStDepositedRao sums the `st_event` Deposited log for one (epoch, no),
// filtering `kind = 'Deposited'` in SQL (served by st_event_kind_block) and the
// epoch/no in Go from data_json. Guards that the kind filter excludes
// non-Deposited events and that only the matching (epoch, no) deposits sum.
func TestSumStDepositedRao(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{
			// two deposits for (epoch 7, no 3) -> summed (100 + 250 = 350)
			{BlockNumber: 10, LogIndex: 0, TxHash: "0x1", Kind: "Deposited", DataJson: `{"e":"7","no_id":"3","amount":"100"}`},
			{BlockNumber: 11, LogIndex: 0, TxHash: "0x2", Kind: "Deposited", DataJson: `{"e":"7","no_id":"3","amount":"250"}`},
			// different no, different epoch -> excluded by the Go epoch/no match
			{BlockNumber: 12, LogIndex: 0, TxHash: "0x3", Kind: "Deposited", DataJson: `{"e":"7","no_id":"9","amount":"999"}`},
			{BlockNumber: 13, LogIndex: 0, TxHash: "0x4", Kind: "Deposited", DataJson: `{"e":"8","no_id":"3","amount":"500"}`},
			// a non-Deposited event with a matching-looking payload -> must be
			// excluded by the `kind = 'Deposited'` filter, not summed
			{BlockNumber: 14, LogIndex: 0, TxHash: "0x5", Kind: "HeadBound", DataJson: `{"e":"7","no_id":"3","amount":"1000000"}`},
		})

		if total := SumStDepositedRao(ctx, testStDeploymentKey, 7, 3); total.Cmp(big.NewInt(350)) != 0 {
			t.Fatalf("SumStDepositedRao(7, 3) = %s, want 350", total.String())
		}
		// no deposits for this (epoch, no) -> zero
		if total := SumStDepositedRao(ctx, testStDeploymentKey, 7, 100); total.Sign() != 0 {
			t.Fatalf("SumStDepositedRao(7, 100) = %s, want 0", total.String())
		}
	})
}

// SumStMinerClaimedInBlockRange windows MinerClaimed events by chain block
// (the public stats collector maps the subnet block clock to a chain block
// range) and dedupes the claiming coldkeys. Guards the half-open block
// range, the kind filter, the coldkey dedupe, and malformed-row skipping.
func TestSumStMinerClaimedInBlockRange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{
			// in range: two claims by the same coldkey and one by another
			{BlockNumber: 100, LogIndex: 0, TxHash: "0x1", Kind: "MinerClaimed", DataJson: `{"e":"7","no_id":"3","coldkey":"0xaa","share_bps":"5000","amount":"100","caller":"0x0"}`},
			{BlockNumber: 150, LogIndex: 0, TxHash: "0x2", Kind: "MinerClaimed", DataJson: `{"e":"7","no_id":"4","coldkey":"0xaa","share_bps":"5000","amount":"250","caller":"0x0"}`},
			{BlockNumber: 199, LogIndex: 0, TxHash: "0x3", Kind: "MinerClaimed", DataJson: `{"e":"7","no_id":"3","coldkey":"0xbb","share_bps":"5000","amount":"1000","caller":"0x0"}`},
			// the range is half-open: block 200 belongs to the next window
			{BlockNumber: 200, LogIndex: 0, TxHash: "0x4", Kind: "MinerClaimed", DataJson: `{"e":"8","no_id":"3","coldkey":"0xcc","share_bps":"5000","amount":"5000","caller":"0x0"}`},
			// before the range
			{BlockNumber: 99, LogIndex: 0, TxHash: "0x5", Kind: "MinerClaimed", DataJson: `{"e":"6","no_id":"3","coldkey":"0xdd","share_bps":"5000","amount":"7000","caller":"0x0"}`},
			// a different kind with a claim-shaped payload must be excluded by
			// the kind filter
			{BlockNumber: 120, LogIndex: 0, TxHash: "0x6", Kind: "Deposited", DataJson: `{"e":"7","no_id":"3","coldkey":"0xee","amount":"9999"}`},
			// malformed rows are skipped, not counted
			{BlockNumber: 130, LogIndex: 0, TxHash: "0x7", Kind: "MinerClaimed", DataJson: `{"coldkey":"0xff","amount":"not-a-number"}`},
			{BlockNumber: 131, LogIndex: 0, TxHash: "0x8", Kind: "MinerClaimed", DataJson: `not json`},
		})

		amount, miners := SumStMinerClaimedInBlockRange(ctx, testStDeploymentKey, 100, 200)
		if amount.Cmp(big.NewInt(1350)) != 0 {
			t.Fatalf("SumStMinerClaimedInBlockRange(100, 200) amount = %s, want 1350", amount.String())
		}
		if miners != 2 {
			t.Fatalf("SumStMinerClaimedInBlockRange(100, 200) miners = %d, want 2 (0xaa claimed twice)", miners)
		}

		// the next window sees only its own claim
		amount, miners = SumStMinerClaimedInBlockRange(ctx, testStDeploymentKey, 200, 300)
		if amount.Cmp(big.NewInt(5000)) != 0 || miners != 1 {
			t.Fatalf("SumStMinerClaimedInBlockRange(200, 300) = %s, %d, want 5000, 1", amount.String(), miners)
		}

		// an empty window is zero, not an error
		amount, miners = SumStMinerClaimedInBlockRange(ctx, testStDeploymentKey, 1000, 2000)
		if amount.Sign() != 0 || miners != 0 {
			t.Fatalf("SumStMinerClaimedInBlockRange(1000, 2000) = %s, %d, want 0, 0", amount.String(), miners)
		}
	})
}

// testStHeadBindingRow reads the st_head_binding mirror row directly (the
// mirror is ops/debug only — production reads replay st_event instead).
func testStHeadBindingRow(ctx context.Context, deploymentKey StDeploymentKey, ckey [32]byte) (active bool, updateBlock uint64, exists bool) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT active, update_block FROM st_head_binding WHERE deployment_key = $1 AND ckey = $2`,
			string(deploymentKey), ckey[:],
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var block int64
				server.Raise(result.Scan(&active, &block))
				updateBlock = uint64(block)
				exists = true
			}
		})
	})
	return
}

func TestStHeadBindingUpsertGuard(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ckey, hotkey := testStCkey(1), testStCkey(9)

		UpsertStHeadBinding(ctx, testStDeploymentKey, ckey, hotkey, 5, true, 10)
		if active, block, ok := testStHeadBindingRow(ctx, testStDeploymentKey, ckey); !ok || !active || block != 10 {
			t.Fatalf("bound row = %v %d %v", active, block, ok)
		}

		// an older event never regresses a newer state
		UpsertStHeadBinding(ctx, testStDeploymentKey, ckey, hotkey, 5, false, 5)
		if active, block, _ := testStHeadBindingRow(ctx, testStDeploymentKey, ckey); !active || block != 10 {
			t.Fatalf("older event applied: %v %d", active, block)
		}

		// same-block later log wins (the sync applies in (block, log) order)
		UpsertStHeadBinding(ctx, testStDeploymentKey, ckey, hotkey, 5, false, 10)
		if active, _, _ := testStHeadBindingRow(ctx, testStDeploymentKey, ckey); active {
			t.Fatal("same-block later event must win")
		}

		UpsertStHeadBinding(ctx, testStDeploymentKey, ckey, hotkey, 6, true, 20)
		if active, block, _ := testStHeadBindingRow(ctx, testStDeploymentKey, ckey); !active || block != 20 {
			t.Fatalf("newer event not applied: %v %d", active, block)
		}
	})
}

// testStHeadEventJson builds the data_json shape the event decoder writes for
// HeadBound/HeadUnbound (st_controller.go stEventDecoders).
func testStHeadEventJson(ckey [32]byte) string {
	hotkey := testStCkey(0xee)
	return fmt.Sprintf(
		`{"ckey":"0x%s","hotkey":"0x%s","uid":"7","registrant":"0x0"}`,
		hex.EncodeToString(ckey[:]),
		hex.EncodeToString(hotkey[:]),
	)
}

// TestGetHeadBoundCkeysInEpochFromEventLog is the SQL leg of the HF-1 fix
// (the replay core is unit-tested in st_model_test.go): kind filter,
// block <= close filter, and (block, log_index) ordering — including
// same-block sequences whose OUTCOME depends on log order.
func TestGetHeadBoundCkeysInEpochFromEventLog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		const startBlock, closeBlock = uint64(500), uint64(1000)

		boundAll := testStCkey(1)       // bound pre-window, never unbound → excluded
		dodger := testStCkey(2)         // HF-1: unbind at close-1 → still excluded
		preWindow := testStCkey(3)      // bound+unbound before the window → not excluded
		bindThenUnbind := testStCkey(4) // same pre-window block: log0 Bound, log1 Unbound → NOT excluded iff log order applied
		unbindThenBind := testStCkey(5) // same pre-window block: log0 Unbound (no-op), log1 Bound → excluded iff log order applied
		postClose := testStCkey(6)      // bound only after closeBlock → filtered by the query
		malformed := testStCkey(7)      // malformed data_json row → skipped

		UpsertStEvents(ctx, testStDeploymentKey, []*StChainEvent{
			{BlockNumber: 100, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(boundAll)},
			{BlockNumber: 100, LogIndex: 1, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(dodger)},
			{BlockNumber: closeBlock - 1, LogIndex: 0, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(dodger)},
			{BlockNumber: 100, LogIndex: 2, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(preWindow)},
			{BlockNumber: 400, LogIndex: 0, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(preWindow)},
			{BlockNumber: 300, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(bindThenUnbind)},
			{BlockNumber: 300, LogIndex: 1, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(bindThenUnbind)},
			{BlockNumber: 300, LogIndex: 2, TxHash: "0x", Kind: "HeadUnbound", DataJson: testStHeadEventJson(unbindThenBind)},
			{BlockNumber: 300, LogIndex: 3, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(unbindThenBind)},
			{BlockNumber: closeBlock + 1, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: testStHeadEventJson(postClose)},
			{BlockNumber: 600, LogIndex: 0, TxHash: "0x", Kind: "HeadBound", DataJson: `{"ckey":"0x1234"}`},
			// a same-window non-head event kind must be ignored entirely
			{BlockNumber: 600, LogIndex: 1, TxHash: "0x", Kind: "OperatorCommitted", DataJson: testStHeadEventJson(malformed)},
		})

		got := GetHeadBoundCkeysInEpoch(ctx, testStDeploymentKey, startBlock, closeBlock)

		want := map[[32]byte]bool{boundAll: true, dodger: true, unbindThenBind: true}
		for ckey, in := range want {
			if got[ckey] != in {
				t.Fatalf("ckey %x: excluded=%v, want %v (full set %v)", ckey[0], got[ckey], in, got)
			}
		}
		if len(got) != len(want) {
			t.Fatalf("excluded set = %v, want exactly %d ckeys", got, len(want))
		}
	})
}

func TestStEpochSummaryCache(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		if GetStEpochSummaryCache(ctx, testStDeploymentKey) != nil {
			t.Fatal("fresh cache must miss")
		}
		summary := &StEpochSummary{
			Epoch: 12, StartBlock: 1200, CommitDeadlineBlock: 1310,
			TrailsDeadlineBlock: 1360, FinalizeBlock: 1400,
			TEpochBlocks: 100, ChainId: 964, ContractAddress: "0xdead",
		}
		SetStEpochSummaryCache(ctx, testStDeploymentKey, summary, time.Minute)
		got := GetStEpochSummaryCache(ctx, testStDeploymentKey)
		if got == nil || *got != *summary {
			t.Fatalf("cache roundtrip = %+v", got)
		}
	})
}

func TestGetStContributingClientCkeys(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		withKey, shortKey, noKey := server.NewId(), server.NewId(), server.NewId()
		ckey := testStCkey(5)
		SetClientPublicKey(ctx, withKey, ckey[:])
		SetClientPublicKey(ctx, shortKey, []byte("not-32-bytes"))

		got := GetStContributingClientCkeys(ctx, []server.Id{withKey, shortKey, noKey})
		if len(got) != 1 || got[withKey] != ckey {
			t.Fatalf("ckeys = %v, want only the 32-byte published key", got)
		}
		if len(GetStContributingClientCkeys(ctx, nil)) != 0 {
			t.Fatal("empty input must return empty")
		}
	})
}

func TestGetStEpochNetworkUsageWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		end := start.Add(time.Hour)

		nA, nB := server.NewId(), server.NewId()
		insertSweep := func(networkId server.Id, byteCount int64, sweepTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
	                    INSERT INTO transfer_escrow_sweep (
	                        contract_id, balance_id, network_id,
	                        payout_byte_count, payout_net_revenue_nano_cents, sweep_time
	                    )
	                    VALUES ($1, $2, $3, $4, 0, $5)
	                `,
					server.NewId(), server.NewId(), networkId, byteCount, sweepTime,
				))
			})
		}
		insertSweep(nA, 600, start)                     // inclusive start
		insertSweep(nA, 400, start.Add(30*time.Minute)) // summed per network
		insertSweep(nB, 250, end.Add(-time.Second))
		insertSweep(nB, 999, end)                     // exclusive end
		insertSweep(nB, 999, start.Add(-time.Second)) // before the window

		got := map[server.Id]int64{}
		for _, usage := range GetStEpochNetworkUsage(ctx, start, end) {
			got[usage.NetworkId] = usage.PayoutByteCount
		}
		if len(got) != 2 || got[nA] != 1000 || got[nB] != 250 {
			t.Fatalf("usage = %v, want {A:1000, B:250}", got)
		}
	})
}

func TestGetStEpochClientReliabilityJoinAndWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId, userId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("st-rel-%s", networkId), userId)
		clientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "d", "spec")
		orphanClientId := server.NewId() // no network_client row → dropped by the join

		p1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
		p2 := p1.Add(15 * time.Minute)
		period := func(clientId server.Id, start time.Time, a int64, c int64) *VerifyProviderStatsRow {
			return &VerifyProviderStatsRow{
				PeriodStart: start, PeriodEnd: start.Add(15 * time.Minute),
				ClientId: clientId, Assignments: a, Confirmations: c,
			}
		}
		UpsertVerifyProviderStats(ctx, []*VerifyProviderStatsRow{
			period(clientId, p1, 10, 8),
			period(clientId, p2, 4, 4),
			period(orphanClientId, p1, 99, 99),
		})

		// window spanning both periods sums them
		rows := GetStEpochClientReliability(ctx, p1, p2.Add(15*time.Minute))
		if len(rows) != 1 {
			t.Fatalf("reliability rows = %+v, want the joined client only", rows)
		}
		if rows[0].ClientId != clientId || rows[0].NetworkId != networkId ||
			rows[0].Assignments != 14 || rows[0].Confirmations != 12 {
			t.Fatalf("summed row = %+v", rows[0])
		}

		// strict overlap: a period ending exactly at the window start is out
		if rows := GetStEpochClientReliability(ctx, p1.Add(15*time.Minute), p2.Add(15*time.Minute)); len(rows) != 1 || rows[0].Assignments != 4 {
			t.Fatalf("second-period-only rows = %+v", rows)
		}
	})
}
