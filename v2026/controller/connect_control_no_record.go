package controller

// A client's answer that it holds no record of contracts the platform asked it
// to close (connect's ContractsNoRecord, TransferContractsNoRecord = 38 in
// frame.proto). A provider answers so for a contract it lost before its first
// checkpoint, to a hard kill, an OOM, a crash or a power loss. Before it, the
// provider answered with a final of zero bytes, which settled at the mean of
// the two counts (half the sender's) or opened a dispute.
//
// The answer carries no count and changes no contract: the client stays what
// it was, silent if it never reported, and the expiry sweep settles the
// contract on the other party's count with no SN25 credit for the bytes only
// one side confirmed (model/contract_no_record.go). A platform that predates
// the message refuses its unknown type and applies nothing, which is the same.
//
// The frame is read by its number with the parser below, before the generic
// decode, so the server builds with a connect that predates the message and
// keeps the behavior once connect decodes it. connect's
// TestContractsNoRecordWireFormat and TestParseContractsNoRecordWireFormat here
// pin the same bytes.
//
// urnetwork_contract_close_no_record_total counts each contract id by the
// answering client's party and what the platform holds of its reports; party
// "none" and report "none" are lost contract state that settles on the other
// party's count. One log line per answer that names a lost contract carries the
// client id, the counts and one contract id; ids are not secrets. Answers that
// name only unknown or already reported contracts log at V(1).

import (
	"context"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/glog/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The wire code point of connect's TransferContractsNoRecord.
const contractsNoRecordMessageType = protocol.MessageType(38)

// The most contract ids read from one answer. A client sends at most 1024
// (connect ContractsNoRecordMaxCount); ids past this bound are counted as
// dropped and read for nothing.
const contractsNoRecordMaxCount = 4096

// The control frame label of the answer: the generic frame metrics have fixed
// cells and count it under "other" (controlFrameMessageLabel).
type contractsNoRecordControlMessage struct{}

var errContractsNoRecordInvalid = errors.New("invalid contracts no record")

var contractNoRecordCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "urnetwork_contract_close_no_record_total",
		Help: "Contract ids a client answered a close request for with no record, by the client's party and what the platform holds of its reports. report=none is lost contract state, settled on the other party's count with no SN25 credit.",
	},
	[]string{"party", "report"},
)

var contractNoRecordParties = []string{model.ContractPartySource, model.ContractPartyDestination, "none"}
var contractNoRecordReports = []string{
	model.ContractNoRecordReportNone,
	model.ContractNoRecordReportCheckpoint,
	model.ContractNoRecordReportFinal,
	model.ContractNoRecordReportClosed,
	model.ContractNoRecordReportUnknown,
	"dropped",
}

func init() {
	// materialize every bounded cell, so an explicit zero is distinguishable
	// from a server that predates the answer
	for _, party := range contractNoRecordParties {
		for _, report := range contractNoRecordReports {
			contractNoRecordCounter.WithLabelValues(party, report)
		}
	}
	prometheus.MustRegister(contractNoRecordCounter)
}

// The contract ids of a marshaled ContractsNoRecord: field 1, repeated bytes,
// each a 16-byte id. Unknown fields are skipped, as protobuf decoding does.
// Returns at most `contractsNoRecordMaxCount` distinct ids and how many more
// the answer named.
func parseContractsNoRecord(messageBytes []byte) (contractIds []server.Id, droppedCount int, returnErr error) {
	seenContractIds := map[server.Id]bool{}
	for 0 < len(messageBytes) {
		number, wireType, n := protowire.ConsumeTag(messageBytes)
		if n < 0 {
			return nil, 0, fmt.Errorf("%w: %v", errContractsNoRecordInvalid, protowire.ParseError(n))
		}
		messageBytes = messageBytes[n:]
		if number != 1 {
			n = protowire.ConsumeFieldValue(number, wireType, messageBytes)
			if n < 0 {
				return nil, 0, fmt.Errorf("%w: %v", errContractsNoRecordInvalid, protowire.ParseError(n))
			}
			messageBytes = messageBytes[n:]
			continue
		}
		if wireType != protowire.BytesType {
			return nil, 0, fmt.Errorf("%w: contract id wire type %d", errContractsNoRecordInvalid, wireType)
		}
		value, n := protowire.ConsumeBytes(messageBytes)
		if n < 0 {
			return nil, 0, fmt.Errorf("%w: %v", errContractsNoRecordInvalid, protowire.ParseError(n))
		}
		messageBytes = messageBytes[n:]
		contractId, err := server.IdFromBytes(value)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %v", errContractsNoRecordInvalid, err)
		}
		if seenContractIds[contractId] {
			continue
		}
		seenContractIds[contractId] = true
		if contractsNoRecordMaxCount <= len(contractIds) {
			droppedCount += 1
			continue
		}
		contractIds = append(contractIds, contractId)
	}
	return contractIds, droppedCount, nil
}

// Counts and logs a client's no-record answer. It writes nothing: each named
// contract stays as the client's reports left it, for the expiry sweep.
func ContractsNoRecord(ctx context.Context, clientId server.Id, messageBytes []byte) error {
	contractIds, droppedCount, err := parseContractsNoRecord(messageBytes)
	if err != nil {
		return err
	}
	if 0 < droppedCount {
		contractNoRecordCounter.WithLabelValues("none", "dropped").Add(float64(droppedCount))
	}
	states := model.GetContractNoRecordStates(ctx, clientId, contractIds)
	reportCounts := map[model.ContractNoRecordReport]int{}
	var lostContractId *server.Id
	for _, contractId := range contractIds {
		state := states[contractId]
		party := state.Party
		if party == "" {
			party = "none"
		}
		contractNoRecordCounter.WithLabelValues(party, state.Report).Inc()
		reportCounts[state.Report] += 1
		if state.Report == model.ContractNoRecordReportNone && lostContractId == nil {
			lostContractId = &contractId
		}
	}
	if lostContractId != nil {
		glog.Infof(
			"[contract]no record client=%s lost=%d checkpoint=%d final=%d closed=%d unknown=%d dropped=%d first_lost=%s\n",
			clientId,
			reportCounts[model.ContractNoRecordReportNone],
			reportCounts[model.ContractNoRecordReportCheckpoint],
			reportCounts[model.ContractNoRecordReportFinal],
			reportCounts[model.ContractNoRecordReportClosed],
			reportCounts[model.ContractNoRecordReportUnknown],
			droppedCount,
			*lostContractId,
		)
	} else if glog.V(1) {
		glog.Infof(
			"[contract]no record client=%s lost=0 checkpoint=%d final=%d closed=%d unknown=%d dropped=%d\n",
			clientId,
			reportCounts[model.ContractNoRecordReportCheckpoint],
			reportCounts[model.ContractNoRecordReportFinal],
			reportCounts[model.ContractNoRecordReportClosed],
			reportCounts[model.ContractNoRecordReportUnknown],
			droppedCount,
		)
	}
	return nil
}
