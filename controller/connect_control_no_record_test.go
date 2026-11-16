// A provider that lost a contract before its first checkpoint answers the
// platform's close request. Before connect's ContractsNoRecord it answered with
// a zero-byte final, and the contract settled at the mean of the two counts:
// half the sender's. These tests drive both answers through
// ConnectControlFrames, the entry point of the resident, api and alt control
// routes, against a real escrowed contract and the real expiry sweep.
package controller

import (
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// connect's TestContractsNoRecordWireFormat pins the same bytes.
const contractsNoRecordWireHex = "0a100102030405060708090a0b0c0d0e0f10" + "0a10f1f2f3f4f5f6f7f8f9fafbfcfdfeff00"

// The answer a provider sends for contracts it has no record of, encoded as
// connect encodes ContractsNoRecord. Built from the wire format rather than
// the generated type, as the server reads it.
func contractsNoRecordTestFrame(messageType protocol.MessageType, contractIds ...server.Id) *protocol.Frame {
	var messageBytes []byte
	for _, contractId := range contractIds {
		messageBytes = protowire.AppendTag(messageBytes, 1, protowire.BytesType)
		messageBytes = protowire.AppendBytes(messageBytes, contractId.Bytes())
	}
	return &protocol.Frame{MessageType: messageType, MessageBytes: messageBytes}
}

// The counter's value for one cell, read by name from the default registry.
func contractNoRecordTestCount(t testing.TB, party string, report string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_contract_close_no_record_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["party"] == party && labels["report"] == report {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func TestParseContractsNoRecordWireFormat(t *testing.T) {
	messageBytes, err := hex.DecodeString(contractsNoRecordWireHex)
	if err != nil {
		t.Fatal(err)
	}
	want := []server.Id{
		{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e, 0x0f, 0x10},
		{0xf1, 0xf2, 0xf3, 0xf4, 0xf5, 0xf6, 0xf7, 0xf8, 0xf9, 0xfa, 0xfb, 0xfc, 0xfd, 0xfe, 0xff, 0x00},
	}
	// a field a later connect may add is skipped; a repeated id is read once
	extended := protowire.AppendTag(slices.Clone(messageBytes), 2, protowire.VarintType)
	extended = protowire.AppendVarint(extended, 7)
	extended = append(extended, messageBytes[:18]...)
	for _, encoded := range [][]byte{messageBytes, extended} {
		contractIds, droppedCount, err := parseContractsNoRecord(encoded)
		if err != nil || droppedCount != 0 || !slices.Equal(contractIds, want) {
			t.Fatalf("parse = %v, %d, %v; want %v", contractIds, droppedCount, err, want)
		}
	}
	shortId := protowire.AppendBytes(protowire.AppendTag(nil, 1, protowire.BytesType), []byte{1, 2, 3})
	varintId := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), 1)
	for _, invalid := range [][]byte{shortId, varintId, messageBytes[:10]} {
		if _, _, err := parseContractsNoRecord(invalid); !errors.Is(err, errContractsNoRecordInvalid) {
			t.Fatalf("parse(%x) = %v, want invalid", invalid, err)
		}
	}
	var many []server.Id
	for range contractsNoRecordMaxCount + 3 {
		many = append(many, server.NewId())
	}
	contractIds, droppedCount, err := parseContractsNoRecord(contractsNoRecordTestFrame(contractsNoRecordMessageType, many...).MessageBytes)
	if err != nil || len(contractIds) != contractsNoRecordMaxCount || droppedCount != 3 {
		t.Fatalf("bounded parse = %d ids, %d dropped, %v", len(contractIds), droppedCount, err)
	}
}

// No connect decoder, one that predates the message or one that knows it,
// turns the answer into a close report: it has its own message type.
func TestContractsNoRecordFrameIsNoCloseReport(t *testing.T) {
	frame := contractsNoRecordTestFrame(contractsNoRecordMessageType, server.NewId())
	if frame.MessageType == protocol.MessageType_TransferCloseContract {
		t.Fatal("the no-record answer shares the close report's message type")
	}
	message, err := connect.FromFrame(frame)
	if err == nil {
		if _, ok := message.(*protocol.CloseContract); ok {
			t.Fatal("the no-record answer decoded as a close report")
		}
	}
}

// One sender final on an escrowed contract to a provider that never reported.
type lostContractTestFixture struct {
	sourceId      server.Id
	destinationId server.Id
	contractId    server.Id
	balanceId     server.Id
}

// The sender's count, under the 16 MiB tolerance so that a zero final settles
// at the mean rather than in dispute.
const lostContractTestSenderByteCount = 200

func newLostContractTestFixture(t testing.TB) *lostContractTestFixture {
	t.Helper()
	ctx := t.Context()
	sourceNetworkId, sourceId := server.NewId(), server.NewId()
	destinationNetworkId, destinationId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, sourceNetworkId, "lost-contract-source-"+sourceNetworkId.String(), server.NewId())
	model.Testing_CreateDevice(ctx, sourceNetworkId, server.NewId(), sourceId, "source", "fixture")
	model.Testing_CreateNetwork(ctx, destinationNetworkId, "lost-contract-provider-"+destinationNetworkId.String(), server.NewId())
	model.Testing_CreateDevice(ctx, destinationNetworkId, server.NewId(), destinationId, "provider", "fixture")
	server.Raise(model.AddBasicTransferBalance(ctx, sourceNetworkId, 10000, server.NowUtc(), server.NowUtc().Add(time.Hour)))
	balances := model.GetActiveTransferBalances(ctx, sourceNetworkId)
	if len(balances) != 1 {
		t.Fatal("fixture does not own one funding grant")
	}
	escrow, err := model.CreateTransferEscrow(ctx, sourceNetworkId, sourceId, destinationNetworkId, destinationId, 1000)
	server.Raise(err)
	f := &lostContractTestFixture{
		sourceId:      sourceId,
		destinationId: destinationId,
		contractId:    escrow.ContractId,
		balanceId:     balances[0].BalanceId,
	}
	if err := CloseContract(ctx, sourceId, &protocol.CloseContract{
		ContractId:     f.contractId.Bytes(),
		ReportId:       server.NewId().Bytes(),
		AckedByteCount: lostContractTestSenderByteCount,
	}); err != nil {
		t.Fatal(err)
	}
	// the sender's final asks the provider for its own (CLIENTCLOSE.md section 3)
	requests := model.TakeClientCloseRequests(ctx, destinationId, 1024)
	if !slices.Equal(requests.ContractIds, []server.Id{f.contractId}) {
		t.Fatalf("close requests = %v, want the contract", requests.ContractIds)
	}
	return f
}

// The contract's outcome, the provider's report and the money and SN25 usage
// its settlement wrote.
type lostContractTestState struct {
	settled             bool
	destinationReported bool
	payoutByteCount     int64
	usageByteCount      int64
	usageExcludedReason string
}

func (self *lostContractTestFixture) state(t testing.TB) lostContractTestState {
	t.Helper()
	ctx := t.Context()
	var state lostContractTestState
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `
			SELECT
				outcome IS NOT NULL,
				EXISTS(SELECT 1 FROM contract_close WHERE contract_id = $1 AND party = 'destination'),
				COALESCE((SELECT sum(payout_byte_count) FROM transfer_escrow_sweep WHERE contract_id = $1), 0),
				COALESCE((provider_usage->>'byte_count')::bigint, -1),
				COALESCE(provider_usage->>'excluded_reason', '')
			FROM transfer_contract
			WHERE contract_id = $1
		`, self.contractId).Scan(
			&state.settled,
			&state.destinationReported,
			&state.payoutByteCount,
			&state.usageByteCount,
			&state.usageExcludedReason,
		))
	})
	return state
}

// Closes every contract past its quiet period, as the expiry task does.
func (self *lostContractTestFixture) sweep(t testing.TB) {
	t.Helper()
	count, err := model.ForceCloseOpenContractIds(t.Context(), server.NowUtc().Add(time.Hour), 10, 1, 0, 0)
	if err != nil || count != 1 {
		t.Fatalf("expiry = %d, %v; want the contract", count, err)
	}
}

// The defect: the zero-byte final a provider sent for a contract it no longer
// held settles at the mean of the two counts, half the sender's.
func TestLostContractZeroFinalSettlesAtHalfTheSenderCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newLostContractTestFixture(t)
		zeroFinal, err := connect.ToFrame(&protocol.CloseContract{
			ContractId: f.contractId.Bytes(),
			ReportId:   server.NewId().Bytes(),
		}, connect.DefaultProtocolVersion)
		if err != nil {
			t.Fatal(err)
		}
		defer returnConnectControlFrames([]*protocol.Frame{zeroFinal})
		out, err := ConnectControlFrames(ctx, f.destinationId, []*protocol.Frame{zeroFinal}, connect.DefaultContractManagerSettings())
		returnConnectControlFrames(out)
		if err != nil {
			t.Fatal(err)
		}
		state := f.state(t)
		if !state.settled || state.payoutByteCount != lostContractTestSenderByteCount/2 {
			t.Fatalf("zero final: %+v, want settled at %d", state, lostContractTestSenderByteCount/2)
		}
	})
}

// The fix: the provider answers with no record. The platform counts it and
// writes nothing, so the provider stays silent, and the expiry sweep settles
// on the sender's count with no SN25 credit for the bytes only the sender
// counted, as for a provider that never answers.
func TestLostContractNoRecordSettlesOnTheSenderCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newLostContractTestFixture(t)
		lostBefore := contractNoRecordTestCount(t, "destination", "none")
		unknownBefore := contractNoRecordTestCount(t, "none", "unknown")

		noRecord := contractsNoRecordTestFrame(protocol.MessageType(38), f.contractId, server.NewId())
		out, err := ConnectControlFrames(ctx, f.destinationId, []*protocol.Frame{noRecord}, connect.DefaultContractManagerSettings())
		returnConnectControlFrames(out)
		if err != nil {
			t.Fatal(err)
		}
		if delta := contractNoRecordTestCount(t, "destination", "none") - lostBefore; delta != 1 {
			t.Fatalf("lost contract count moved %v, want 1", delta)
		}
		if delta := contractNoRecordTestCount(t, "none", "unknown") - unknownBefore; delta != 1 {
			t.Fatalf("unknown contract count moved %v, want 1", delta)
		}
		if state := f.state(t); state.settled || state.destinationReported {
			t.Fatalf("no record changed the contract: %+v", state)
		}

		f.sweep(t)
		state := f.state(t)
		if !state.settled || state.payoutByteCount != lostContractTestSenderByteCount {
			t.Fatalf("no record: %+v, want settled at %d", state, lostContractTestSenderByteCount)
		}
		if state.usageByteCount != 0 || state.usageExcludedReason != "expired_unconfirmed" {
			t.Fatalf("no record: SN25 usage %d (%q), want none credited", state.usageByteCount, state.usageExcludedReason)
		}
	})
}

// A platform that predates the answer has no case for its message type: it
// refuses the frame as unknown, applies nothing, and the sweep settles as for
// a provider that never answered. The frame takes the same unknown-type path
// here under a number this platform does not know.
func TestLostContractNoRecordOnOlderPlatformSettlesOnTheSenderCount(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f := newLostContractTestFixture(t)
		older := contractsNoRecordTestFrame(contractsNoRecordMessageType+100, f.contractId)
		out, err := ConnectControlFrames(ctx, f.destinationId, []*protocol.Frame{older}, connect.DefaultContractManagerSettings())
		returnConnectControlFrames(out)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "unknown message type") {
			t.Fatalf("older platform: %v, want the unknown message type refused", err)
		}
		if state := f.state(t); state.settled || state.destinationReported {
			t.Fatalf("an older platform changed the contract: %+v", state)
		}

		f.sweep(t)
		state := f.state(t)
		if !state.settled || state.payoutByteCount != lostContractTestSenderByteCount ||
			state.usageByteCount != 0 || state.usageExcludedReason != "expired_unconfirmed" {
			t.Fatalf("older platform: %+v, want settled at %d with no SN25 credit", state, lostContractTestSenderByteCount)
		}
	})
}
