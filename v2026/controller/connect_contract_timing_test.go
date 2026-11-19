package controller

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func controllerContractTimingCount(t testing.TB, ingress, outcome string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() == "urnetwork_contract_creation_completed_total" {
			for _, m := range f.Metric {
				a, b := false, false
				for _, l := range m.Label {
					a = a || l.GetName() == "ingress" && l.GetValue() == ingress
					b = b || l.GetName() == "outcome" && l.GetValue() == outcome
				}
				if a && b {
					return m.Counter.GetValue()
				}
			}
		}
	}
	t.Fatal("missing finite disposition cell")
	return 0
}

// Real control replies, with complete financial posts, must distinguish an
// allocated signed contract from a normal nil-Go-error protocol rejection.
func TestCreateContractTimingActualReplyAndRejection(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network, peerNetwork, source, destination := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, n := range []server.Id{network, peerNetwork} {
			model.Testing_CreateNetwork(ctx, n, "timing-fixture-"+n.String(), server.NewId())
		}
		model.Testing_CreateDevice(ctx, network, server.NewId(), source, "timing-source", "fixture")
		model.Testing_CreateDevice(ctx, peerNetwork, server.NewId(), destination, "timing-destination", "fixture")
		// The 512 KiB left after the 1 MiB contract is below the shrink-to-fit
		// floor, so the larger request is still refused.
		server.Raise(model.AddBasicTransferBalance(ctx, network, 1024*1024+512*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
		model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{42}, 32)})
		before := controllerContractTimingCount(t, "internal", "contract_reply")
		request := &protocol.CreateContract{DestinationId: destination.Bytes(), TransferByteCount: 1024 * 1024}
		frames, err := CreateContract(ctx, source, request, connect.DefaultContractManagerSettings())
		if err != nil || len(frames) != 1 {
			t.Fatal("healthy contract failed")
		}
		defer returnConnectControlFrames(frames)
		message, err := connect.FromFrame(frames[0])
		if err != nil {
			t.Fatal(err)
		}
		result := message.(*protocol.CreateContractResult)
		if result.Contract == nil || result.Error != nil || controllerContractTimingCount(t, "internal", "contract_reply") != before+1 {
			t.Fatal("successful allocation not classified as reply")
		}
		if model.GetOpenTransferByteCount(ctx, network) != 1024*1024 {
			t.Fatal("diagnostic changed allocated bytes")
		}
		// The second request exceeds credit. Keep its old wire classification,
		// distinguish it from a signed reply and record server-owned HTTP ingress.
		request.TransferByteCount = 8 * 1024 * 1024
		httpCtx := context.WithValue(ctx, controlHttpIngressKey{}, true)
		before = controllerContractTimingCount(t, "http", "protocol_reject")
		frames, err = CreateContract(httpCtx, source, request, connect.DefaultContractManagerSettings())
		if err != nil || len(frames) != 1 {
			t.Fatal("protocol refusal changed transport result")
		}
		defer returnConnectControlFrames(frames)
		message, err = connect.FromFrame(frames[0])
		if err != nil {
			t.Fatal(err)
		}
		result = message.(*protocol.CreateContractResult)
		if result.Contract != nil || result.Error == nil || *result.Error != protocol.ContractError_InsufficientBalance || controllerContractTimingCount(t, "http", "protocol_reject") != before+1 {
			t.Fatal("refusal changed or counted as a contract")
		}
		if model.GetOpenTransferByteCount(ctx, network) != 1024*1024 {
			t.Fatal("rejected request changed ledger")
		}
	})
}
