// Both hosted control routes preserve the same authenticated stable report identity.
package controller

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"google.golang.org/protobuf/proto"
)

// The actual HTTP adapter takes its owner from a synthetic authenticated client session.
type closeReportControlFixture struct {
	ctx             context.Context
	clientSession   *session.ClientSession
	sourceId        server.Id
	contractId      server.Id
	otherContractId server.Id
}

// Real no-escrow contracts keep transport-path tests independent of subscription sizing.
func newCloseReportControlFixture(t testing.TB) *closeReportControlFixture {
	t.Helper()
	ctx := t.Context()
	networkId, userId, deviceId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "synthetic close reports", userId)
	model.Testing_CreateDevice(ctx, networkId, deviceId, sourceId, "synthetic source", "source")
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic destination", "destination")
	clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "synthetic close reports", false, false).Client(deviceId, sourceId))
	t.Cleanup(clientSession.Cancel)
	first, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 1000)
	if err != nil {
		t.Fatal(err)
	}
	second, err := model.CreateContractNoEscrow(ctx, networkId, sourceId, networkId, destinationId, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return &closeReportControlFixture{ctx: ctx, clientSession: clientSession, sourceId: sourceId, contractId: first, otherContractId: second}
}

// Serialization exercises the real protobuf ReportId field consumed by both routes.
func closeReportControlFrame(t testing.TB, report *protocol.CloseContract) *protocol.Frame {
	t.Helper()
	frame, err := connect.ToFrame(report, connect.DefaultProtocolVersion)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

// The original serialized pack is retained for retries, matching ControlSync ownership.
func (self *closeReportControlFixture) http(t testing.TB, report *protocol.CloseContract) error {
	t.Helper()
	frame := closeReportControlFrame(t, report)
	defer returnConnectControlFrames([]*protocol.Frame{frame})
	wire, err := proto.Marshal(&protocol.Pack{Frames: []*protocol.Frame{frame}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ConnectControl(&ConnectControlArgs{Pack: base64.StdEncoding.EncodeToString(wire)}, self.clientSession)
	if err != nil {
		return err
	}
	if result.Error != nil {
		return errors.New(result.Error.Message)
	}
	return nil
}

// Durable facts, not metrics, are the accounting assertion; metrics additionally count once.
func assertControlCloseReportCensus(t testing.TB, f *closeReportControlFixture, contractId server.Id, reports int, bytes int64) {
	t.Helper()
	var gotReports int
	var gotBytes int64
	server.Db(f.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(f.ctx, `SELECT count(*) FROM contract_close_report_evidence WHERE contract_id=$1`, contractId).Scan(&gotReports))
		server.Raise(conn.QueryRow(f.ctx, `SELECT COALESCE(sum(used_transfer_byte_count),0) FROM contract_close WHERE contract_id=$1`, contractId).Scan(&gotBytes))
	})
	if reports != gotReports || bytes != gotBytes {
		t.Fatal("hosted original report/byte census changed", gotReports, gotBytes, reports, bytes)
	}
}

// Hosted HTTP and resident/shared frame dispatch must deduplicate the same serialized report.
func TestCloseReportHostedAndFrameRetryCountOnce(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		report := &protocol.CloseContract{ContractId: f.contractId.Bytes(), ReportId: server.NewId().Bytes(), AckedByteCount: 20, UnackedByteCount: 7, Checkpoint: true}
		before := testutil.ToFloat64(transferByteCounter)
		if err := f.http(t, report); err != nil {
			t.Fatal(err)
		}
		frame := closeReportControlFrame(t, report)
		defer returnConnectControlFrames([]*protocol.Frame{frame})
		result, err := ConnectControlFrames(f.ctx, f.sourceId, []*protocol.Frame{frame}, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(result)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.http(t, report); err != nil {
			t.Fatal(err)
		}
		assertControlCloseReportCensus(t, f, f.contractId, 1, 20)
		if delta := testutil.ToFloat64(transferByteCounter) - before; delta != 20 {
			t.Fatal("hosted retry counted the original twice", delta)
		}
	})
}

// A conflicting original is scoped to its frame; a later healthy report still commits.
func TestCloseReportConflictKeepsHealthyBatchSibling(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		original := &protocol.CloseContract{ContractId: f.contractId.Bytes(), ReportId: server.NewId().Bytes(), AckedByteCount: 20, Checkpoint: true}
		if err := CloseContract(f.ctx, f.sourceId, original); err != nil {
			t.Fatal(err)
		}
		poison := proto.Clone(original).(*protocol.CloseContract)
		poison.AckedByteCount++
		healthy := &protocol.CloseContract{ContractId: f.otherContractId.Bytes(), ReportId: server.NewId().Bytes(), AckedByteCount: 30, Checkpoint: true}
		frames := []*protocol.Frame{closeReportControlFrame(t, poison), closeReportControlFrame(t, healthy)}
		defer returnConnectControlFrames(frames)
		result, err := ConnectControlFrames(f.ctx, f.sourceId, frames, connect.DefaultContractManagerSettings())
		defer returnConnectControlFrames(result)
		if !errors.Is(err, model.ErrContractCloseReportConflict) {
			t.Fatal("report identity conflict was hidden", err)
		}
		assertControlCloseReportCensus(t, f, f.contractId, 1, 20)
		assertControlCloseReportCensus(t, f, f.otherContractId, 1, 30)
	})
}

// Empty ids retain legacy incremental reports even when an identified peer shares the contract.
func TestCloseReportLegacyEmptyIdRemainsIncremental(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		report := &protocol.CloseContract{ContractId: f.contractId.Bytes(), AckedByteCount: 20, UnackedByteCount: ^uint64(0), Checkpoint: true}
		for range 2 {
			if err := f.http(t, report); err != nil {
				t.Fatal(err)
			}
		}
		assertControlCloseReportCensus(t, f, f.contractId, 0, 40)
		report.ReportId = server.NewId().Bytes()
		for range 2 {
			if err := f.http(t, report); err != nil {
				t.Fatal(err)
			}
		}
		assertControlCloseReportCensus(t, f, f.contractId, 1, 60)
	})
}

// Present but malformed ids may not be silently demoted to legacy accumulation.
func TestCloseReportMalformedIdentityHasNoAccountingEffect(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newCloseReportControlFixture(t)
		for _, reportId := range [][]byte{{1}, make([]byte, 16), make([]byte, 17)} {
			err := CloseContract(f.ctx, f.sourceId, &protocol.CloseContract{ContractId: f.contractId.Bytes(), ReportId: reportId, AckedByteCount: 20, Checkpoint: true})
			if !errors.Is(err, model.ErrContractCloseReportInvalid) {
				t.Fatal("malformed report became legacy work", err)
			}
		}
		assertControlCloseReportCensus(t, f, f.contractId, 0, 0)
	})
}
