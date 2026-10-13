// Owner decision B (2026-10-10), driven through the resident control
// dispatcher: a receiver checkpoint that follows the sender's final close
// settles the contract in that receiver report when the receiver's retained
// count covers the sender's final. A receiver report that predates the
// sender's final never settles, so a sender cannot report less after the
// receiver's last count and settle before the receiver answers. A forward
// contract settled this way, like any closed forward contract, anchors the
// provider's companions until its original deadline, on its stream.
package controller

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
)

// Dispatch one close report exactly as the resident path does and return the
// dispatcher's error. A nil identity sends an older client's id-less report.
func (self *twoPartyCloseFixture) report(t testing.TB, clientId server.Id, contractId server.Id, ackedByteCount model.ByteCount, checkpoint bool, reportId []byte) error {
	t.Helper()
	frame, err := connect.ToFrame(&protocol.CloseContract{
		ContractId:     contractId.Bytes(),
		AckedByteCount: uint64(ackedByteCount),
		Checkpoint:     checkpoint,
		ReportId:       reportId,
	}, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	defer returnConnectControlFrames([]*protocol.Frame{frame})
	resultFrames, err := ConnectControlFrames(self.ctx, clientId, []*protocol.Frame{frame}, self.settings)
	returnConnectControlFrames(resultFrames)
	return err
}

// Like report, for a report that must be accepted.
func (self *twoPartyCloseFixture) mustReport(t testing.TB, clientId server.Id, contractId server.Id, ackedByteCount model.ByteCount, checkpoint bool, reportId []byte) {
	t.Helper()
	if err := self.report(t, clientId, contractId, ackedByteCount, checkpoint, reportId); err != nil {
		t.Fatalf("close report from %s failed: %v", clientId, err)
	}
}

// Create through the real contract control message, returning the result's
// contract id or its refusal.
func (self *twoPartyCloseFixture) requestContract(t testing.TB, clientId server.Id, createContract *protocol.CreateContract) (*server.Id, *protocol.ContractError) {
	t.Helper()
	resultFrames := self.control(t, clientId, createContract)
	defer returnConnectControlFrames(resultFrames)
	if len(resultFrames) != 1 {
		t.Fatalf("create contract returned %d frames, want 1", len(resultFrames))
	}
	message, err := connect.FromFrame(resultFrames[0])
	if err != nil {
		t.Fatal(err)
	}
	result, ok := message.(*protocol.CreateContractResult)
	if !ok {
		t.Fatalf("create contract returned %T", message)
	}
	if result.Error != nil || result.Contract == nil {
		return nil, result.Error
	}
	storedContract := &protocol.StoredContract{}
	if err := proto.Unmarshal(result.Contract.StoredContractBytes, storedContract); err != nil {
		t.Fatal(err)
	}
	contractId, err := server.IdFromBytes(storedContract.ContractId)
	if err != nil {
		t.Fatal(err)
	}
	return &contractId, nil
}

// One contract's settlement facts after the dispatcher returned. Missing
// outcome, stream and companion origin read as their zero values.
type coveredCheckpointState struct {
	outcome                          model.ContractOutcome
	destinationFound                 bool
	destinationCheckpoint            bool
	destinationUsedTransferByteCount model.ByteCount
	debitByteCount                   model.ByteCount
	receiptCount                     int
	evidenceCount                    int
	streamId                         server.Id
	companionContractId              server.Id
}

func (self *twoPartyCloseFixture) coveredState(t testing.TB, contractId server.Id) coveredCheckpointState {
	t.Helper()
	var state coveredCheckpointState
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT COALESCE(outcome,''),
			COALESCE(stream_id,'00000000-0000-0000-0000-000000000000'::uuid),
			COALESCE(companion_contract_id,'00000000-0000-0000-0000-000000000000'::uuid),
			(SELECT COALESCE(sum(debit_byte_count),0)::bigint FROM transfer_debit_journal WHERE contract_id=$1),
			(SELECT count(*) FROM contract_close_report WHERE contract_id=$1),
			(SELECT count(*) FROM contract_close_report_evidence WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(
			&state.outcome, &state.streamId, &state.companionContractId,
			&state.debitByteCount, &state.receiptCount, &state.evidenceCount))
		rows, err := conn.Query(self.ctx, `SELECT checkpoint,used_transfer_byte_count FROM contract_close WHERE contract_id=$1 AND party=$2`, contractId, model.ContractPartyDestination)
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				state.destinationFound = true
				server.Raise(rows.Scan(&state.destinationCheckpoint, &state.destinationUsedTransferByteCount))
			}
		})
	})
	return state
}

// The contract stays open and the receiver's checkpoint stays resumable.
func (self *twoPartyCloseFixture) assertCheckpointWaits(t testing.TB, contractId server.Id, stage string) {
	t.Helper()
	state := self.coveredState(t, contractId)
	if state.outcome != "" {
		t.Fatalf("%s: contract settled with outcome %s before its receiver reported after the sender's final", stage, state.outcome)
	}
	if !state.destinationFound || !state.destinationCheckpoint {
		t.Fatalf("%s: receiver checkpoint not retained as a checkpoint: found=%t checkpoint=%t", stage, state.destinationFound, state.destinationCheckpoint)
	}
	if state.debitByteCount != 0 {
		t.Fatalf("%s: unsettled contract debited %d bytes", stage, state.debitByteCount)
	}
}

// The receiver's report settled the contract inline: its checkpoint is
// completed as a final with every retained byte, and an escrowed contract
// debits the mean of the two parties' counts.
func (self *twoPartyCloseFixture) assertCheckpointSettled(t testing.TB, contractId server.Id, receiverByteCount model.ByteCount, escrowed bool, debitByteCount model.ByteCount) {
	t.Helper()
	state := self.coveredState(t, contractId)
	if state.outcome != model.ContractOutcomeSettled {
		t.Fatalf("contract did not settle in the receiver's report after the sender's final: outcome=%q", state.outcome)
	}
	if !state.destinationFound || state.destinationCheckpoint || state.destinationUsedTransferByteCount != receiverByteCount {
		t.Fatalf("receiver checkpoint not completed as a final of %d bytes: found=%t checkpoint=%t bytes=%d",
			receiverByteCount, state.destinationFound, state.destinationCheckpoint, state.destinationUsedTransferByteCount)
	}
	if escrowed && state.debitByteCount != debitByteCount {
		t.Fatalf("settlement debited %d bytes, want %d", state.debitByteCount, debitByteCount)
	}
}

// A contract kind with its sender and receiver for one contract.
type coveredCheckpointContract struct {
	f          *twoPartyCloseFixture
	contractId server.Id
	senderId   server.Id
	receiverId server.Id
	escrowed   bool
}

// A Redis-reserved origin from the window to the provider.
func newCoveredCheckpointOrigin(t testing.TB) coveredCheckpointContract {
	t.Helper()
	f := newTwoPartyCloseFixture(t, false)
	contractId := f.createContract(t, f.windowId, &protocol.CreateContract{
		DestinationId:     f.providerId.Bytes(),
		TransferByteCount: uint64(MinContractTransferByteCount),
	})
	if escrowCount, redisReserved := f.redisReserved(t, contractId); escrowCount == 0 || !redisReserved {
		t.Fatalf("origin escrow is not Redis reserved: escrows=%d redis=%t", escrowCount, redisReserved)
	}
	return coveredCheckpointContract{f: f, contractId: contractId, senderId: f.windowId, receiverId: f.providerId, escrowed: true}
}

// The provider's companion, which it sends and the window receives and pays.
func newCoveredCheckpointCompanion(t testing.TB) coveredCheckpointContract {
	t.Helper()
	f := newTwoPartyCloseFixture(t, false)
	model.SetProvide(f.ctx, f.windowId, map[model.ProvideMode][]byte{
		model.ProvideModeStream: []byte("synthetic-covered-stream-key0000"),
	})
	f.createContract(t, f.windowId, &protocol.CreateContract{
		DestinationId:     f.providerId.Bytes(),
		TransferByteCount: uint64(MinContractTransferByteCount),
	})
	streamVersion := uint32(connect.DefaultStreamVersion)
	contractId := f.createContract(t, f.providerId, &protocol.CreateContract{
		DestinationId:     f.windowId.Bytes(),
		TransferByteCount: uint64(MinContractTransferByteCount),
		Companion:         true,
		StreamVersion:     &streamVersion,
	})
	if state := f.coveredState(t, contractId); state.companionContractId == (server.Id{}) {
		t.Fatal("reply contract was not created as a companion")
	}
	return coveredCheckpointContract{f: f, contractId: contractId, senderId: f.providerId, receiverId: f.windowId, escrowed: true}
}

// A same-network contract, which holds no escrow.
func newCoveredCheckpointNetwork(t testing.TB) coveredCheckpointContract {
	t.Helper()
	f := newTwoPartyCloseFixture(t, true)
	contractId := f.createContract(t, f.windowId, &protocol.CreateContract{
		DestinationId:     f.providerId.Bytes(),
		TransferByteCount: uint64(MinContractTransferByteCount),
	})
	if escrowCount, _ := f.redisReserved(t, contractId); escrowCount != 0 {
		t.Fatalf("network contract unexpectedly holds %d escrows", escrowCount)
	}
	return coveredCheckpointContract{f: f, contractId: contractId, senderId: f.windowId, receiverId: f.providerId}
}

// The receiver checkpoints at receive-sequence exit after the sender's final;
// that report settles every contract kind without the deadline closer.
func TestCoveredCheckpointAfterSenderFinalSettles(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, kind := range []struct {
			name   string
			create func(testing.TB) coveredCheckpointContract
		}{
			{name: "redis origin", create: newCoveredCheckpointOrigin},
			{name: "companion", create: newCoveredCheckpointCompanion},
			{name: "network", create: newCoveredCheckpointNetwork},
		} {
			c := kind.create(t)
			const senderByteCount = model.ByteCount(48 * 1024)
			const receiverByteCount = model.ByteCount(50 * 1024)
			c.f.mustReport(t, c.senderId, c.contractId, senderByteCount, false, server.NewId().Bytes())
			c.f.assertOpen(t, c.contractId, kind.name+" sender final")
			c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, server.NewId().Bytes())
			c.f.assertCheckpointSettled(t, c.contractId, receiverByteCount, c.escrowed, (senderByteCount+receiverByteCount)/2)
		}
	})
}

// A receiver checkpoint that predates the sender's final does not settle in
// the sender's final, whether that final covers it or reports zero. The
// receiver's next report, after the final, settles at the mean of both.
func TestReceiverCheckpointBeforeSenderFinalWaitsForReceiverReport(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		for _, senderByteCount := range []model.ByteCount{32 * 1024, 0} {
			c := newCoveredCheckpointOrigin(t)
			const receiverByteCount = model.ByteCount(32 * 1024)
			c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, server.NewId().Bytes())
			c.f.mustReport(t, c.senderId, c.contractId, senderByteCount, false, server.NewId().Bytes())
			c.f.assertCheckpointWaits(t, c.contractId, "sender final after the receiver checkpoint")
			c.f.mustReport(t, c.receiverId, c.contractId, 0, true, server.NewId().Bytes())
			c.f.assertCheckpointSettled(t, c.contractId, receiverByteCount, true, (senderByteCount+receiverByteCount)/2)
		}
	})
}

// A sender that reports zero after the receiver's checkpoint cannot settle
// the contract by itself: neither its final, nor a retransmission of that
// final, nor an older client's id-less final settles before the receiver
// reports again.
func TestLyingSenderCannotSettleBeforeReceiverReportsAgain(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		c := newCoveredCheckpointOrigin(t)
		const receiverByteCount = model.ByteCount(40 * 1024)
		c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, server.NewId().Bytes())
		senderReportId := server.NewId().Bytes()
		c.f.mustReport(t, c.senderId, c.contractId, 0, false, senderReportId)
		c.f.assertCheckpointWaits(t, c.contractId, "zero sender final")
		c.f.mustReport(t, c.senderId, c.contractId, 0, false, senderReportId)
		c.f.assertCheckpointWaits(t, c.contractId, "retransmitted zero sender final")
		c.f.mustReport(t, c.senderId, c.contractId, 0, false, nil)
		c.f.assertCheckpointWaits(t, c.contractId, "id-less zero sender final")
		c.f.mustReport(t, c.receiverId, c.contractId, 0, true, server.NewId().Bytes())
		c.f.assertCheckpointSettled(t, c.contractId, receiverByteCount, true, receiverByteCount/2)
	})
}

// The stale checkpoint order: the receiver checkpoints at its receive
// timeout, the sender sends more on the same contract and then closes. A
// receiver report after that final that still undercounts the final waits;
// the report that brings the receiver's total to the final settles in full.
func TestUndercountingReceiverCheckpointWaitsForCoverage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		c := newCoveredCheckpointOrigin(t)
		const senderByteCount = model.ByteCount(64 * 1024)
		c.f.mustReport(t, c.receiverId, c.contractId, 24*1024, true, server.NewId().Bytes())
		c.f.mustReport(t, c.senderId, c.contractId, senderByteCount, false, server.NewId().Bytes())
		c.f.mustReport(t, c.receiverId, c.contractId, 24*1024, true, server.NewId().Bytes())
		c.f.assertCheckpointWaits(t, c.contractId, "undercounting receiver report after the sender final")
		c.f.mustReport(t, c.receiverId, c.contractId, 16*1024, true, server.NewId().Bytes())
		c.f.assertCheckpointSettled(t, c.contractId, senderByteCount, true, senderByteCount)
	})
}

// Older clients send id-less reports, and the server keeps only the largest
// id-less receiver checkpoint as a lower bound of the receiver's total. A
// bound that covers the sender's final settles. A later checkpoint that
// raises the bound follows the sender's final, but when the bound stays below
// the final it cannot be verified and waits, even though the receiver's two
// checkpoints sum to the final.
func TestIdlessReceiverCheckpointSettlesOnlyWithVerifiableCoverage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const senderByteCount = model.ByteCount(48 * 1024)
		covered := newCoveredCheckpointOrigin(t)
		covered.f.mustReport(t, covered.senderId, covered.contractId, senderByteCount, false, nil)
		covered.f.mustReport(t, covered.receiverId, covered.contractId, senderByteCount, true, nil)
		covered.f.assertCheckpointSettled(t, covered.contractId, senderByteCount, true, senderByteCount)

		unverifiable := newCoveredCheckpointOrigin(t)
		const firstByteCount = model.ByteCount(20 * 1024)
		const raisedByteCount = senderByteCount - firstByteCount
		unverifiable.f.mustReport(t, unverifiable.receiverId, unverifiable.contractId, firstByteCount, true, nil)
		unverifiable.f.mustReport(t, unverifiable.senderId, unverifiable.contractId, senderByteCount, false, nil)
		unverifiable.f.mustReport(t, unverifiable.receiverId, unverifiable.contractId, raisedByteCount, true, nil)
		unverifiable.f.assertCheckpointWaits(t, unverifiable.contractId, "id-less receiver lower bound below the sender final")
		if state := unverifiable.f.coveredState(t, unverifiable.contractId); state.destinationUsedTransferByteCount != raisedByteCount {
			t.Fatalf("id-less receiver bound is %d bytes, want %d", state.destinationUsedTransferByteCount, raisedByteCount)
		}
	})
}

// An id-less report carries no identity, so the server cannot tell a new one
// from a retransmission of one the receiver sent before the sender's final.
// After a sender reports zero, a retransmission of the receiver's earlier
// id-less checkpoint adds nothing to the receiver's retained count and does
// not settle. A later id-less report that raises that count was never applied
// before, so it follows the sender's final and settles.
func TestIdlessReceiverRetransmissionCannotSettleAfterSenderFinal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		c := newCoveredCheckpointOrigin(t)
		const receiverByteCount = model.ByteCount(40 * 1024)
		c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, nil)
		c.f.mustReport(t, c.senderId, c.contractId, 0, false, server.NewId().Bytes())
		c.f.assertCheckpointWaits(t, c.contractId, "zero sender final")
		c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, nil)
		c.f.assertCheckpointWaits(t, c.contractId, "id-less receiver retransmission after the zero sender final")
		const raisedByteCount = model.ByteCount(48 * 1024)
		c.f.mustReport(t, c.receiverId, c.contractId, raisedByteCount, true, nil)
		c.f.assertCheckpointSettled(t, c.contractId, raisedByteCount, true, raisedByteCount/2)
	})
}

// A late receiver report after a settlement in its checkpoint is handled
// exactly like a late report after a two-final settlement: an exact
// retransmission is acknowledged without change, and a new identified or
// id-less report is refused as already settled, with no new receipt,
// evidence, report bytes or debit.
func TestLateReceiverReportAfterCheckpointSettlementMatchesTwoFinals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const byteCount = model.ByteCount(32 * 1024)
		for _, receiverCheckpoint := range []bool{true, false} {
			c := newCoveredCheckpointOrigin(t)
			c.f.mustReport(t, c.senderId, c.contractId, byteCount, false, server.NewId().Bytes())
			receiverReportId := server.NewId().Bytes()
			c.f.mustReport(t, c.receiverId, c.contractId, byteCount, receiverCheckpoint, receiverReportId)
			c.f.assertCheckpointSettled(t, c.contractId, byteCount, true, byteCount)
			settled := c.f.coveredState(t, c.contractId)

			if err := c.f.report(t, c.receiverId, c.contractId, byteCount, receiverCheckpoint, receiverReportId); err != nil {
				t.Fatalf("checkpoint=%t exact receiver retransmission after settlement failed: %v", receiverCheckpoint, err)
			}
			for _, reportId := range [][]byte{server.NewId().Bytes(), nil} {
				err := c.f.report(t, c.receiverId, c.contractId, 8*1024, true, reportId)
				if err == nil || !strings.Contains(err.Error(), "already closed with outcome settled") {
					t.Fatalf("checkpoint=%t identified=%t late receiver report was not refused as settled: %v", receiverCheckpoint, reportId != nil, err)
				}
			}
			if after := c.f.coveredState(t, c.contractId); after != settled {
				t.Fatalf("checkpoint=%t late receiver reports changed the settled contract: before=%+v after=%+v", receiverCheckpoint, settled, after)
			}
		}
	})
}

// Moves a contract's whole timeline into the past, as if it had been created,
// reported and settled that long ago. Terminal close times are otherwise
// immutable, so the usage guard is suspended in this test database only.
func (self *twoPartyCloseFixture) agoContract(t testing.TB, contractId server.Id, ago time.Duration) {
	t.Helper()
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `ALTER TABLE transfer_contract DISABLE TRIGGER transfer_contract_usage_guard`))
		server.RaisePgResult(tx.Exec(self.ctx, `UPDATE transfer_contract SET create_time=create_time-$2*interval '1 second',
			expiration_time=expiration_time-$2*interval '1 second', close_time=close_time-$2*interval '1 second' WHERE contract_id=$1`, contractId, ago.Seconds()))
		server.RaisePgResult(tx.Exec(self.ctx, `UPDATE contract_close SET close_time=close_time-$2*interval '1 second' WHERE contract_id=$1`, contractId, ago.Seconds()))
		server.RaisePgResult(tx.Exec(self.ctx, `ALTER TABLE transfer_contract ENABLE TRIGGER transfer_contract_usage_guard`))
	})
}

// A closed forward contract anchors the provider's companions until its
// original deadline, not only for the client library's 300 second linger
// after its close, whether it settled in its receiver's checkpoint or with two
// finals. Past that deadline no companion opens from it.
func TestCompanionOriginLastsUntilOriginalDeadline(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		streamVersion := uint32(connect.DefaultStreamVersion)
		// Two finals first: that closure needs no settlement change, so it
		// isolates the origin window from the checkpoint settlement.
		for _, receiverCheckpoint := range []bool{false, true} {
			f := newTwoPartyCloseFixture(t, false)
			model.SetProvide(f.ctx, f.windowId, map[model.ProvideMode][]byte{
				model.ProvideModeStream: []byte("synthetic-origin-stream-key00000"),
			})
			originId := f.createContract(t, f.windowId, &protocol.CreateContract{
				DestinationId:     f.providerId.Bytes(),
				TransferByteCount: uint64(MinContractTransferByteCount),
			})
			const byteCount = model.ByteCount(16 * 1024)
			f.mustReport(t, f.windowId, originId, byteCount, false, server.NewId().Bytes())
			f.mustReport(t, f.providerId, originId, byteCount, receiverCheckpoint, server.NewId().Bytes())
			if state := f.coveredState(t, originId); state.outcome != model.ContractOutcomeSettled {
				t.Fatalf("checkpoint=%t forward contract did not settle: outcome=%q", receiverCheckpoint, state.outcome)
			}

			f.agoContract(t, originId, 10*time.Minute)
			companionId, contractErr := f.requestContract(t, f.providerId, &protocol.CreateContract{
				DestinationId:     f.windowId.Bytes(),
				TransferByteCount: uint64(MinContractTransferByteCount),
				Companion:         true,
				StreamVersion:     &streamVersion,
			})
			if contractErr != nil || companionId == nil {
				t.Fatalf("checkpoint=%t companion refused 10 minutes after its origin closed, before its deadline: %v", receiverCheckpoint, contractErr)
			}
			if state := f.coveredState(t, *companionId); state.companionContractId != originId {
				t.Fatalf("checkpoint=%t companion anchored on %s, want the closed origin %s", receiverCheckpoint, state.companionContractId, originId)
			}

			server.Tx(f.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(f.ctx, `UPDATE transfer_contract SET expiration_time=(clock_timestamp() AT TIME ZONE 'UTC')-interval '1 second' WHERE contract_id=$1`, originId))
			})
			if companionId, contractErr := f.requestContract(t, f.providerId, &protocol.CreateContract{
				DestinationId:     f.windowId.Bytes(),
				TransferByteCount: uint64(MinContractTransferByteCount),
				Companion:         true,
				StreamVersion:     &streamVersion,
			}); contractErr == nil || companionId != nil {
				t.Fatalf("checkpoint=%t companion opened from an origin past its original deadline", receiverCheckpoint)
			}
		}
	})
}

// A forward contract on a stream that settles in its receiver's checkpoint
// stays in that stream, so a later companion still joins the origin's stream
// while the origin anchors companions.
func TestCheckpointSettledOriginKeepsStreamForCompanions(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newTwoPartyCloseFixture(t, false)
		model.SetProvide(f.ctx, f.windowId, map[model.ProvideMode][]byte{
			model.ProvideModeStream: []byte("synthetic-kept-stream-key0000000"),
		})
		forceStream := true
		streamVersion := uint32(connect.DefaultStreamVersion)
		originId := f.createContract(t, f.windowId, &protocol.CreateContract{
			DestinationId:     f.providerId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
			ForceStream:       &forceStream,
			StreamVersion:     &streamVersion,
		})
		origin := f.coveredState(t, originId)
		if origin.streamId == (server.Id{}) {
			t.Fatal("forced stream origin has no stream")
		}
		const byteCount = model.ByteCount(16 * 1024)
		f.mustReport(t, f.windowId, originId, byteCount, false, server.NewId().Bytes())
		f.mustReport(t, f.providerId, originId, byteCount, true, server.NewId().Bytes())
		f.assertCheckpointSettled(t, originId, byteCount, true, byteCount)

		companionId, contractErr := f.requestContract(t, f.providerId, &protocol.CreateContract{
			DestinationId:     f.windowId.Bytes(),
			TransferByteCount: uint64(MinContractTransferByteCount),
			Companion:         true,
			StreamVersion:     &streamVersion,
		})
		if contractErr != nil || companionId == nil {
			t.Fatalf("companion refused after its origin settled in a checkpoint: %v", contractErr)
		}
		if companion := f.coveredState(t, *companionId); companion.streamId != origin.streamId {
			t.Fatalf("companion stream %s, want the settled origin's stream %s", companion.streamId, origin.streamId)
		}
	})
}
