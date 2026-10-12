// A streamed contract's insert writes its stream id, so joining the stream
// after commit does not write the contract row again. The stream id is
// written later only when it genuinely changed after the read that precedes
// the creation transaction.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// With a binding, the insert writes the stream the contract joins, both for a
// stream that the contract starts and for a live stream it joins, on the
// no-escrow, escrow and companion inserts. The association after commit then
// writes no row version, and still records the stream's participants.
func TestContractStreamBoundAtInsertIsNotWrittenAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractRowWriteCount := countContractRowWrites(t, ctx)
		originNetwork, providerNetwork := server.NewId(), server.NewId()
		origin, first, egress := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: originNetwork, first: providerNetwork, egress: providerNetwork})
		addContractPayoutTestBalance(ctx, originNetwork, 1024)
		addContractPayoutTestBalance(ctx, providerNetwork, 1024)
		intermediaryIds := []server.Id{first}

		requireBoundAndUnwritten := func(name string, contractId server.Id, streamId server.Id) {
			t.Helper()
			if written := readContractStreamId(ctx, contractId); written == nil || *written != streamId {
				t.Fatal(name, "insert did not write the joined stream id", written, streamId)
			}
			if writeCount := contractRowWriteCount(contractId); writeCount != 0 {
				t.Fatal(name, "stream association wrote the contract row again", writeCount)
			}
		}

		// the first contract starts the stream with the id its insert bound
		startCtx := WithAddStreamBinding(ctx, origin, egress, intermediaryIds)
		started, err := CreateTransferEscrow(startCtx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		inserted := readContractStreamId(ctx, started.ContractId)
		streamId := AddToStream(startCtx, started.ContractId, origin, egress, intermediaryIds)
		if inserted == nil || *inserted != streamId {
			t.Fatal("the insert did not bind the id of the stream the contract started", inserted, streamId)
		}
		if err := SetContractStream(startCtx, started.ContractId, streamId, intermediaryIds); err != nil {
			t.Fatal(err)
		}
		requireBoundAndUnwritten("started stream:", started.ContractId, streamId)
		var participantCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM contract_participant WHERE stream_id = $1 AND client_id = $2`, streamId, first).Scan(&participantCount))
		})
		if participantCount != 1 {
			t.Fatal("bound stream association did not record its participant", participantCount)
		}

		// a no-escrow contract on the same path joins the live stream
		joinCtx := WithAddStreamBinding(ctx, origin, egress, intermediaryIds)
		joinedId, err := CreateContractNoEscrow(joinCtx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		if joined := AddToStream(joinCtx, joinedId, origin, egress, intermediaryIds); joined != streamId {
			t.Fatal("a contract on the same path did not join the live stream", joined, streamId)
		}
		if err := SetContractStream(joinCtx, joinedId, streamId, intermediaryIds); err != nil {
			t.Fatal(err)
		}
		requireBoundAndUnwritten("joined stream:", joinedId, streamId)

		// the companion's insert binds its origin's stream
		companionCtx := WithCompanionStreamBinding(ctx, egress, origin)
		companion, err := CreateCompanionTransferEscrow(companionCtx, providerNetwork, egress, originNetwork, origin, 121, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		joined, ok := AddCompanionContractToStream(companionCtx, companion.ContractId, *companion.CompanionContractId, egress, origin)
		if !ok || joined != streamId {
			t.Fatal("the companion did not join its origin's stream", ok, joined, streamId)
		}
		if err := SetContractStream(companionCtx, companion.ContractId, joined, nil); err != nil {
			t.Fatal(err)
		}
		requireBoundAndUnwritten("companion:", companion.ContractId, streamId)
	})
}

// The stream the insert bound ended before the contract joined, so the
// contract started a new stream. The association replaces the bound id once.
func TestContractStreamBindingReplacesAnEndedStreamOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractRowWriteCount := countContractRowWrites(t, ctx)
		networkId := server.NewId()
		origin, egress := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: networkId, egress: networkId})

		previousId, err := CreateContractNoEscrow(ctx, networkId, origin, networkId, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		endedStreamId := AddToStream(ctx, previousId, origin, egress, nil)

		boundCtx := WithAddStreamBinding(ctx, origin, egress, nil)
		contractId, err := CreateContractNoEscrow(boundCtx, networkId, origin, networkId, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		if bound := readContractStreamId(ctx, contractId); bound == nil || *bound != endedStreamId {
			t.Fatal("the insert did not bind the live stream", bound, endedStreamId)
		}
		RemoveFromStream(ctx, previousId)
		streamId := AddToStream(boundCtx, contractId, origin, egress, nil)
		if streamId == endedStreamId {
			t.Fatal("the contract rejoined a stream that ended")
		}
		if err := SetContractStream(boundCtx, contractId, streamId, nil); err != nil {
			t.Fatal(err)
		}
		if written := readContractStreamId(ctx, contractId); written == nil || *written != streamId {
			t.Fatal("the association did not replace the ended stream", written, streamId)
		}
		if writeCount := contractRowWriteCount(contractId); writeCount != 1 {
			t.Fatal("replacing the bound stream did not write the row exactly once", writeCount)
		}
	})
}

// The pair stream the insert bound ended before the contract could join it,
// so the contract carries no stream and the bound id is cleared once.
func TestContractStreamBindingClearedWhenTheJoinFindsNoStream(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractRowWriteCount := countContractRowWrites(t, ctx)
		networkId := server.NewId()
		origin, egress := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: networkId, egress: networkId})

		forwardId, err := CreateContractNoEscrow(ctx, networkId, origin, networkId, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		pairStreamId := AddToStream(ctx, forwardId, origin, egress, nil)

		boundCtx := WithPairStreamBinding(ctx, egress, origin, false)
		replyId, err := CreateContractNoEscrow(boundCtx, networkId, egress, networkId, origin, 121)
		if err != nil {
			t.Fatal(err)
		}
		if bound := readContractStreamId(ctx, replyId); bound == nil || *bound != pairStreamId {
			t.Fatal("the insert did not bind the pair's live stream", bound, pairStreamId)
		}
		RemoveFromStream(ctx, forwardId)
		if _, ok := AddContractToPairStream(boundCtx, replyId, egress, origin); ok {
			t.Fatal("the reply joined a stream that ended")
		}
		ClearContractStreamBinding(boundCtx, replyId)
		if cleared := readContractStreamId(ctx, replyId); cleared != nil {
			t.Fatal("a contract outside any stream kept the bound stream id", *cleared)
		}
		if writeCount := contractRowWriteCount(replyId); writeCount != 1 {
			t.Fatal("clearing the bound stream did not write the row exactly once", writeCount)
		}
	})
}

// Two requests read the path before either started its stream, so each insert
// bound its own proposal. The first join starts the stream with its proposal
// and the second contract joins that stream: the second association replaces
// its bound id once, and the first contract is not written again.
func TestContractStreamBindingReplacesAStreamAnotherRequestStarted(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractRowWriteCount := countContractRowWrites(t, ctx)
		networkId := server.NewId()
		origin, egress := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: networkId, egress: networkId})

		firstCtx := WithAddStreamBinding(ctx, origin, egress, nil)
		firstId, err := CreateContractNoEscrow(firstCtx, networkId, origin, networkId, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		secondCtx := WithAddStreamBinding(ctx, origin, egress, nil)
		secondId, err := CreateContractNoEscrow(secondCtx, networkId, origin, networkId, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		firstBound, secondBound := readContractStreamId(ctx, firstId), readContractStreamId(ctx, secondId)
		if firstBound == nil || secondBound == nil || *firstBound == *secondBound {
			t.Fatal("each insert did not bind its own proposal", firstBound, secondBound)
		}

		streamId := AddToStream(firstCtx, firstId, origin, egress, nil)
		if streamId != *firstBound {
			t.Fatal("the first join did not start the stream with its bound id", streamId, *firstBound)
		}
		if err := SetContractStream(firstCtx, firstId, streamId, nil); err != nil {
			t.Fatal(err)
		}
		if joined := AddToStream(secondCtx, secondId, origin, egress, nil); joined != streamId {
			t.Fatal("the second contract did not join the stream the first started", joined, streamId)
		}
		if err := SetContractStream(secondCtx, secondId, streamId, nil); err != nil {
			t.Fatal(err)
		}

		for _, contractId := range []server.Id{firstId, secondId} {
			if written := readContractStreamId(ctx, contractId); written == nil || *written != streamId {
				t.Fatal("a contract does not carry the stream it joined", contractId, written, streamId)
			}
		}
		if writeCount := contractRowWriteCount(firstId); writeCount != 0 {
			t.Fatal("the contract that started the stream was written again", writeCount)
		}
		if writeCount := contractRowWriteCount(secondId); writeCount != 1 {
			t.Fatal("replacing the second proposal did not write the row exactly once", writeCount)
		}
	})
}

// The companion's insert bound its origin's stream, which ended before the
// companion joined, and the pair started another stream meanwhile. The
// companion joins the pair's other stream, and its association replaces the
// bound id once.
func TestCompanionStreamBindingReplacesAnEndedOriginStreamOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractRowWriteCount := countContractRowWrites(t, ctx)
		originNetwork, providerNetwork := server.NewId(), server.NewId()
		origin, egress := contractPayoutTestId(1), contractPayoutTestId(2)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: originNetwork, egress: providerNetwork})
		addContractPayoutTestBalance(ctx, originNetwork, 1024)

		forward, err := CreateTransferEscrow(ctx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		originStreamId := AddToStream(ctx, forward.ContractId, origin, egress, nil)

		companionCtx := WithCompanionStreamBinding(ctx, egress, origin)
		companion, err := CreateCompanionTransferEscrow(companionCtx, providerNetwork, egress, originNetwork, origin, 121, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if companion.CompanionContractId == nil || *companion.CompanionContractId != forward.ContractId {
			t.Fatal("the companion did not answer the forward contract", companion.CompanionContractId)
		}
		if bound := readContractStreamId(ctx, companion.ContractId); bound == nil || *bound != originStreamId {
			t.Fatal("the insert did not bind the origin's stream", bound, originStreamId)
		}

		RemoveFromStream(ctx, forward.ContractId)
		next, err := CreateTransferEscrow(ctx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		nextStreamId := AddToStream(ctx, next.ContractId, origin, egress, nil)
		if nextStreamId == originStreamId {
			t.Fatal("the pair restarted a stream that ended")
		}

		joined, ok := AddCompanionContractToStream(companionCtx, companion.ContractId, forward.ContractId, egress, origin)
		if !ok || joined != nextStreamId {
			t.Fatal("the companion did not join the pair's other stream", ok, joined, nextStreamId)
		}
		if err := SetContractStream(companionCtx, companion.ContractId, joined, nil); err != nil {
			t.Fatal(err)
		}
		if written := readContractStreamId(ctx, companion.ContractId); written == nil || *written != nextStreamId {
			t.Fatal("the association did not replace the ended origin stream", written, nextStreamId)
		}
		if writeCount := contractRowWriteCount(companion.ContractId); writeCount != 1 {
			t.Fatal("replacing the bound stream did not write the row exactly once", writeCount)
		}
	})
}

// The pair carries two live streams, so the companion's insert bound the pair
// stream its read found first, which need not be its origin's. The companion
// joins its origin's stream and carries it after at most one write, none when
// the read found the origin's stream.
func TestCompanionStreamBindingOfAPairWithTwoStreams(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		contractRowWriteCount := countContractRowWrites(t, ctx)
		originNetwork, providerNetwork := server.NewId(), server.NewId()
		origin, first, egress := contractPayoutTestId(1), contractPayoutTestId(2), contractPayoutTestId(3)
		addContractPayoutTestClients(ctx, map[server.Id]server.Id{origin: originNetwork, first: providerNetwork, egress: providerNetwork})
		addContractPayoutTestBalance(ctx, originNetwork, 1024)

		forward, err := CreateTransferEscrow(ctx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		originStreamId := AddToStream(ctx, forward.ContractId, origin, egress, nil)
		multihop, err := CreateTransferEscrow(ctx, originNetwork, origin, providerNetwork, egress, 121)
		if err != nil {
			t.Fatal(err)
		}
		multihopStreamId := AddToStream(ctx, multihop.ContractId, origin, egress, []server.Id{first})
		if multihopStreamId == originStreamId {
			t.Fatal("the two paths share one stream")
		}

		companionCtx := WithCompanionStreamBinding(ctx, egress, origin)
		companion, err := CreateCompanionTransferEscrow(companionCtx, providerNetwork, egress, originNetwork, origin, 121, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if companion.CompanionContractId == nil || *companion.CompanionContractId != forward.ContractId {
			t.Fatal("the companion did not answer the earliest forward contract", companion.CompanionContractId)
		}
		bound := readContractStreamId(ctx, companion.ContractId)
		if bound == nil || (*bound != originStreamId && *bound != multihopStreamId) {
			t.Fatal("the insert did not bind a stream of the pair", bound)
		}

		joined, ok := AddCompanionContractToStream(companionCtx, companion.ContractId, forward.ContractId, egress, origin)
		if !ok || joined != originStreamId {
			t.Fatal("the companion did not join its origin's stream", ok, joined, originStreamId)
		}
		if err := SetContractStream(companionCtx, companion.ContractId, joined, nil); err != nil {
			t.Fatal(err)
		}
		if written := readContractStreamId(ctx, companion.ContractId); written == nil || *written != originStreamId {
			t.Fatal("the companion does not carry its origin's stream", written, originStreamId)
		}
		wantWriteCount := int64(1)
		if *bound == originStreamId {
			wantWriteCount = 0
		}
		if writeCount := contractRowWriteCount(companion.ContractId); writeCount != wantWriteCount {
			t.Fatal("the association wrote the row", writeCount, "times, want", wantWriteCount)
		}
	})
}
