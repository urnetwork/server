// Owner decision B (2026-10-10), driven through the resident control
// dispatcher: a receiver checkpoint that follows the sender's final close
// settles the contract in that receiver report when the receiver's retained
// count covers the sender's final. A receiver checkpoint that predates the
// sender's final settles in that final instead when the two counts agree
// within model.AcceptableTransfersByteDifference either way, at the floor mean
// of the two (owner, 2026-10-28). A sender can then report less after the
// receiver's last count and settle before the receiver answers, but only
// within the tolerance, so the receiver loses at most half of it. Beyond the
// tolerance the checkpoint waits for the receiver's next report, which
// settles only under decision B, or for the deadline closer. A forward
// contract settled either way, like any closed forward contract, anchors the
// provider's companions until its original deadline, on its stream.
package controller

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
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
	dispute                          bool
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
		server.Raise(conn.QueryRow(self.ctx, `SELECT COALESCE(outcome,''), dispute,
			COALESCE(stream_id,'00000000-0000-0000-0000-000000000000'::uuid),
			COALESCE(companion_contract_id,'00000000-0000-0000-0000-000000000000'::uuid),
			(SELECT COALESCE(sum(debit_byte_count),0)::bigint FROM transfer_debit_journal WHERE contract_id=$1),
			(SELECT count(*) FROM contract_close_report WHERE contract_id=$1),
			(SELECT count(*) FROM contract_close_report_evidence WHERE contract_id=$1)
			FROM transfer_contract WHERE contract_id=$1`, contractId).Scan(
			&state.outcome, &state.dispute, &state.streamId, &state.companionContractId,
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
	if state.dispute {
		t.Fatalf("%s: contract disputed before its receiver's final", stage)
	}
	if !state.destinationFound || !state.destinationCheckpoint {
		t.Fatalf("%s: receiver checkpoint not retained as a checkpoint: found=%t checkpoint=%t", stage, state.destinationFound, state.destinationCheckpoint)
	}
	if state.debitByteCount != 0 {
		t.Fatalf("%s: unsettled contract debited %d bytes", stage, state.debitByteCount)
	}
}

// A report settled the contract inline, the receiver's after the sender's
// final (decision B) or the sender's final after the receiver's checkpoint
// (owner, 2026-10-28): the receiver's checkpoint is completed as a final with
// every retained byte, and an escrowed contract debits the floor mean of the
// two parties' counts.
func (self *twoPartyCloseFixture) assertCheckpointSettled(t testing.TB, contractId server.Id, receiverByteCount model.ByteCount, escrowed bool, debitByteCount model.ByteCount) {
	t.Helper()
	state := self.coveredState(t, contractId)
	if state.outcome != model.ContractOutcomeSettled {
		t.Fatalf("contract did not settle in the report that completes the receiver's checkpoint: outcome=%q", state.outcome)
	}
	if !state.destinationFound || state.destinationCheckpoint || state.destinationUsedTransferByteCount != receiverByteCount {
		t.Fatalf("receiver checkpoint not completed as a final of %d bytes: found=%t checkpoint=%t bytes=%d",
			receiverByteCount, state.destinationFound, state.destinationCheckpoint, state.destinationUsedTransferByteCount)
	}
	if escrowed && state.debitByteCount != debitByteCount {
		t.Fatalf("settlement debited %d bytes, want %d", state.debitByteCount, debitByteCount)
	}
}

// A report after the contract settled gets the benign refusal every late
// report gets once its contract is settled, and changes nothing.
func (self *twoPartyCloseFixture) assertLateReportRefused(t testing.TB, clientId server.Id, contractId server.Id, ackedByteCount model.ByteCount, checkpoint bool, reportId []byte, stage string) {
	t.Helper()
	settled := self.coveredState(t, contractId)
	err := self.report(t, clientId, contractId, ackedByteCount, checkpoint, reportId)
	if err == nil || !strings.Contains(err.Error(), "already closed with outcome settled") {
		t.Fatalf("%s: late report was not refused as settled: %v", stage, err)
	}
	if after := self.coveredState(t, contractId); after != settled {
		t.Fatalf("%s: late report changed the settled contract: before=%+v after=%+v", stage, settled, after)
	}
}

// Two finals that disagree beyond the tolerance dispute the contract: it has
// no outcome and no debit, and the receiver's final keeps every retained byte.
func (self *twoPartyCloseFixture) assertDisputed(t testing.TB, contractId server.Id, receiverByteCount model.ByteCount, stage string) {
	t.Helper()
	state := self.coveredState(t, contractId)
	if state.outcome != "" || !state.dispute {
		t.Fatalf("%s: contract not in dispute: outcome=%q dispute=%t", stage, state.outcome, state.dispute)
	}
	if !state.destinationFound || state.destinationCheckpoint || state.destinationUsedTransferByteCount != receiverByteCount {
		t.Fatalf("%s: receiver final not recorded as a final of %d bytes: found=%t checkpoint=%t bytes=%d",
			stage, receiverByteCount, state.destinationFound, state.destinationCheckpoint, state.destinationUsedTransferByteCount)
	}
	if state.debitByteCount != 0 {
		t.Fatalf("%s: disputed contract debited %d bytes", stage, state.debitByteCount)
	}
}

// A settlement at the edge of the tolerance debits at least half of it, more
// than a minimum contract escrows, so the tolerance tests use origins that
// escrow twice the tolerance.
const coveredCheckpointToleranceTransferByteCount = 2 * model.AcceptableTransfersByteDifference

// A Redis-reserved origin from the window to the provider that escrows at
// least transferByteCount.
func (self *twoPartyCloseFixture) createOrigin(t testing.TB, transferByteCount model.ByteCount) server.Id {
	t.Helper()
	contractId := self.createContract(t, self.windowId, &protocol.CreateContract{
		DestinationId:     self.providerId.Bytes(),
		TransferByteCount: uint64(transferByteCount),
	})
	if escrowCount, redisReserved := self.redisReserved(t, contractId); escrowCount == 0 || !redisReserved {
		t.Fatalf("origin escrow is not Redis reserved: escrows=%d redis=%t", escrowCount, redisReserved)
	}
	var escrowByteCount model.ByteCount
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, `SELECT COALESCE(sum(balance_byte_count),0)::bigint FROM transfer_escrow WHERE contract_id=$1`, contractId).Scan(&escrowByteCount))
	})
	if escrowByteCount < transferByteCount {
		t.Fatalf("origin escrows %d bytes, fewer than the %d it requested", escrowByteCount, transferByteCount)
	}
	return contractId
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
	return newCoveredCheckpointOriginOfSize(t, MinContractTransferByteCount)
}

// An origin that can settle at either edge of the tolerance.
func newCoveredCheckpointToleranceOrigin(t testing.TB) coveredCheckpointContract {
	t.Helper()
	return newCoveredCheckpointOriginOfSize(t, coveredCheckpointToleranceTransferByteCount)
}

func newCoveredCheckpointOriginOfSize(t testing.TB, transferByteCount model.ByteCount) coveredCheckpointContract {
	t.Helper()
	f := newTwoPartyCloseFixture(t, false)
	contractId := f.createOrigin(t, transferByteCount)
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

// A receiver checkpoint that predates the sender's final settles in that
// final when the two counts agree within the tolerance (owner, 2026-10-28):
// whether the final covers the checkpoint, reports zero, or exceeds it by the
// whole tolerance, the checkpoint is completed as a final of its retained
// count, the escrow debits the floor mean of both, and the receiver's next
// report is refused as already settled. A final that exceeds the checkpoint
// by more than the tolerance leaves the checkpoint waiting, and the
// receiver's next report, after the final, settles at the mean of both once
// it covers the final (decision B).
func TestReceiverCheckpointBeforeSenderFinalSettlesWithinTolerance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const receiverByteCount = model.ByteCount(32 * 1024)
		for _, senderByteCount := range []model.ByteCount{
			receiverByteCount,
			0,
			receiverByteCount + model.AcceptableTransfersByteDifference,
		} {
			c := newCoveredCheckpointToleranceOrigin(t)
			c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, server.NewId().Bytes())
			c.f.mustReport(t, c.senderId, c.contractId, senderByteCount, false, server.NewId().Bytes())
			c.f.assertCheckpointSettled(t, c.contractId, receiverByteCount, true, (senderByteCount+receiverByteCount)/2)
			c.f.assertLateReportRefused(t, c.receiverId, c.contractId, 0, true, server.NewId().Bytes(), "receiver report after the sender final settled")
		}

		c := newCoveredCheckpointToleranceOrigin(t)
		const senderByteCount = receiverByteCount + model.AcceptableTransfersByteDifference + 1
		c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, server.NewId().Bytes())
		c.f.mustReport(t, c.senderId, c.contractId, senderByteCount, false, server.NewId().Bytes())
		c.f.assertCheckpointWaits(t, c.contractId, "sender final beyond the tolerance after the receiver checkpoint")
		const coveringByteCount = senderByteCount + 16*1024
		c.f.mustReport(t, c.receiverId, c.contractId, coveringByteCount-receiverByteCount, true, server.NewId().Bytes())
		c.f.assertCheckpointSettled(t, c.contractId, coveringByteCount, true, (senderByteCount+coveringByteCount)/2)
	})
}

// A sender that reports less than the receiver's checkpoint settles the
// contract by itself only within the tolerance (owner, 2026-10-28). There its
// final settles at once at the floor mean, so the receiver loses at most half
// the tolerance, and later reports are refused as already settled. Beyond the
// tolerance it cannot settle by itself: neither its final, nor a
// retransmission of that final, nor an older client's id-less final settles,
// and neither does the receiver's next checkpoint, which does not cover the
// final within the tolerance either (decision B). The receiver's final then
// disputes the contract.
func TestLyingSenderSettlesAloneOnlyWithinTolerance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		within := newCoveredCheckpointToleranceOrigin(t)
		const withinByteCount = model.AcceptableTransfersByteDifference
		within.f.mustReport(t, within.receiverId, within.contractId, withinByteCount, true, server.NewId().Bytes())
		withinReportId := server.NewId().Bytes()
		within.f.mustReport(t, within.senderId, within.contractId, 0, false, withinReportId)
		within.f.assertCheckpointSettled(t, within.contractId, withinByteCount, true, withinByteCount/2)
		settled := within.f.coveredState(t, within.contractId)
		within.f.mustReport(t, within.senderId, within.contractId, 0, false, withinReportId)
		if after := within.f.coveredState(t, within.contractId); after != settled {
			t.Fatalf("retransmitted zero sender final changed the settled contract: before=%+v after=%+v", settled, after)
		}
		within.f.assertLateReportRefused(t, within.senderId, within.contractId, 0, false, nil, "id-less zero sender final after the settlement")
		within.f.assertLateReportRefused(t, within.receiverId, within.contractId, 0, true, server.NewId().Bytes(), "receiver report after the zero sender final settled")

		beyond := newCoveredCheckpointToleranceOrigin(t)
		const beyondByteCount = model.AcceptableTransfersByteDifference + 1
		beyond.f.mustReport(t, beyond.receiverId, beyond.contractId, beyondByteCount, true, server.NewId().Bytes())
		beyondReportId := server.NewId().Bytes()
		beyond.f.mustReport(t, beyond.senderId, beyond.contractId, 0, false, beyondReportId)
		beyond.f.assertCheckpointWaits(t, beyond.contractId, "zero sender final")
		beyond.f.mustReport(t, beyond.senderId, beyond.contractId, 0, false, beyondReportId)
		beyond.f.assertCheckpointWaits(t, beyond.contractId, "retransmitted zero sender final")
		beyond.f.mustReport(t, beyond.senderId, beyond.contractId, 0, false, nil)
		beyond.f.assertCheckpointWaits(t, beyond.contractId, "id-less zero sender final")
		beyond.f.mustReport(t, beyond.receiverId, beyond.contractId, 0, true, server.NewId().Bytes())
		beyond.f.assertCheckpointWaits(t, beyond.contractId, "receiver checkpoint after the zero sender final")
		beyond.f.mustReport(t, beyond.receiverId, beyond.contractId, 0, false, server.NewId().Bytes())
		beyond.f.assertDisputed(t, beyond.contractId, beyondByteCount, "receiver final after the zero sender final")
	})
}

// The stale checkpoint order: the receiver checkpoints at its receive
// timeout, the sender sends more on the same contract and then closes. A
// final that exceeds the stale checkpoint by at most the tolerance settles at
// once at the floor mean (owner, 2026-10-28), and the receiver's report of
// the rest is refused as already settled. A final that exceeds it by more
// leaves the checkpoint waiting: a receiver report after that final that
// still undercounts the final waits, and the report that brings the
// receiver's total to the final settles in full.
func TestUndercountingReceiverCheckpointBeyondToleranceWaitsForCoverage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const staleByteCount = model.ByteCount(24 * 1024)

		within := newCoveredCheckpointToleranceOrigin(t)
		const withinSenderByteCount = staleByteCount + model.AcceptableTransfersByteDifference
		within.f.mustReport(t, within.receiverId, within.contractId, staleByteCount, true, server.NewId().Bytes())
		within.f.mustReport(t, within.senderId, within.contractId, withinSenderByteCount, false, server.NewId().Bytes())
		within.f.assertCheckpointSettled(t, within.contractId, staleByteCount, true, (staleByteCount+withinSenderByteCount)/2)
		within.f.assertLateReportRefused(t, within.receiverId, within.contractId, withinSenderByteCount-staleByteCount, true, server.NewId().Bytes(), "receiver report of the rest after the sender final settled")

		c := newCoveredCheckpointToleranceOrigin(t)
		const senderByteCount = staleByteCount + model.AcceptableTransfersByteDifference + 1
		c.f.mustReport(t, c.receiverId, c.contractId, staleByteCount, true, server.NewId().Bytes())
		c.f.mustReport(t, c.senderId, c.contractId, senderByteCount, false, server.NewId().Bytes())
		c.f.assertCheckpointWaits(t, c.contractId, "sender final beyond the tolerance after the stale receiver checkpoint")
		c.f.mustReport(t, c.receiverId, c.contractId, 24*1024, true, server.NewId().Bytes())
		c.f.assertCheckpointWaits(t, c.contractId, "undercounting receiver report after the sender final")
		c.f.mustReport(t, c.receiverId, c.contractId, senderByteCount-2*staleByteCount, true, server.NewId().Bytes())
		c.f.assertCheckpointSettled(t, c.contractId, senderByteCount, true, senderByteCount)
	})
}

// Older clients send id-less reports, and the server keeps only the largest
// id-less receiver checkpoint as a lower bound of the receiver's total. A
// receiver checkpoint after the sender's final whose bound covers that final
// settles. A sender's final after the bound settles at once at the floor mean
// when the two agree within the tolerance, with the bound as the receiver's
// final (owner, 2026-10-28). Beyond the tolerance the bound waits, and a
// later checkpoint that raises it follows the sender's final, but when the
// bound stays below the final it cannot be verified and waits, even though
// the receiver's two checkpoints sum to the final.
func TestIdlessReceiverCheckpointAfterSenderFinalSettlesOnlyWithVerifiableCoverage(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const senderByteCount = model.ByteCount(48 * 1024)
		covered := newCoveredCheckpointOrigin(t)
		covered.f.mustReport(t, covered.senderId, covered.contractId, senderByteCount, false, nil)
		covered.f.mustReport(t, covered.receiverId, covered.contractId, senderByteCount, true, nil)
		covered.f.assertCheckpointSettled(t, covered.contractId, senderByteCount, true, senderByteCount)

		const firstByteCount = model.ByteCount(20 * 1024)
		within := newCoveredCheckpointToleranceOrigin(t)
		const withinSenderByteCount = firstByteCount + model.AcceptableTransfersByteDifference
		within.f.mustReport(t, within.receiverId, within.contractId, firstByteCount, true, nil)
		within.f.mustReport(t, within.senderId, within.contractId, withinSenderByteCount, false, nil)
		within.f.assertCheckpointSettled(t, within.contractId, firstByteCount, true, (firstByteCount+withinSenderByteCount)/2)
		within.f.assertLateReportRefused(t, within.receiverId, within.contractId, withinSenderByteCount-firstByteCount, true, nil, "id-less receiver checkpoint after the id-less sender final settled")

		unverifiable := newCoveredCheckpointToleranceOrigin(t)
		const unverifiableSenderByteCount = firstByteCount + model.AcceptableTransfersByteDifference + 1
		const raisedByteCount = unverifiableSenderByteCount - firstByteCount
		unverifiable.f.mustReport(t, unverifiable.receiverId, unverifiable.contractId, firstByteCount, true, nil)
		unverifiable.f.mustReport(t, unverifiable.senderId, unverifiable.contractId, unverifiableSenderByteCount, false, nil)
		unverifiable.f.assertCheckpointWaits(t, unverifiable.contractId, "id-less sender final beyond the tolerance after the receiver lower bound")
		unverifiable.f.mustReport(t, unverifiable.receiverId, unverifiable.contractId, raisedByteCount, true, nil)
		unverifiable.f.assertCheckpointWaits(t, unverifiable.contractId, "id-less receiver lower bound below the sender final")
		if state := unverifiable.f.coveredState(t, unverifiable.contractId); state.destinationUsedTransferByteCount != raisedByteCount {
			t.Fatalf("id-less receiver bound is %d bytes, want %d", state.destinationUsedTransferByteCount, raisedByteCount)
		}
	})
}

// An id-less report carries no identity, so the server cannot tell a new one
// from a retransmission of one the receiver sent before the sender's final.
// Within the tolerance the sender's final settles that earlier checkpoint at
// once (owner, 2026-10-28), so a retransmission of it is refused as already
// settled and changes nothing. Beyond the tolerance the checkpoint waits: a
// retransmission adds nothing to the receiver's retained count and does not
// settle, while a later id-less report that raises that count was never
// applied before, so it follows the sender's final and settles once it covers
// the final.
func TestIdlessReceiverRetransmissionCannotSettleAfterSenderFinal(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		const receiverByteCount = model.ByteCount(40 * 1024)

		within := newCoveredCheckpointOrigin(t)
		within.f.mustReport(t, within.receiverId, within.contractId, receiverByteCount, true, nil)
		within.f.mustReport(t, within.senderId, within.contractId, 0, false, server.NewId().Bytes())
		within.f.assertCheckpointSettled(t, within.contractId, receiverByteCount, true, receiverByteCount/2)
		within.f.assertLateReportRefused(t, within.receiverId, within.contractId, receiverByteCount, true, nil, "id-less receiver retransmission after the zero sender final settled")

		c := newCoveredCheckpointToleranceOrigin(t)
		const senderByteCount = receiverByteCount + model.AcceptableTransfersByteDifference + 1
		c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, nil)
		c.f.mustReport(t, c.senderId, c.contractId, senderByteCount, false, server.NewId().Bytes())
		c.f.assertCheckpointWaits(t, c.contractId, "sender final beyond the tolerance")
		c.f.mustReport(t, c.receiverId, c.contractId, receiverByteCount, true, nil)
		c.f.assertCheckpointWaits(t, c.contractId, "id-less receiver retransmission after the sender final")
		if state := c.f.coveredState(t, c.contractId); state.destinationUsedTransferByteCount != receiverByteCount {
			t.Fatalf("id-less receiver retransmission changed its retained count to %d bytes, want %d", state.destinationUsedTransferByteCount, receiverByteCount)
		}
		const raisedByteCount = senderByteCount + 8*1024
		c.f.mustReport(t, c.receiverId, c.contractId, raisedByteCount, true, nil)
		c.f.assertCheckpointSettled(t, c.contractId, raisedByteCount, true, (senderByteCount+raisedByteCount)/2)
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
