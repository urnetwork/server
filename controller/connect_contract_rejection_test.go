package controller

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestContractRejectionCauseSchemaAndErrorClasses(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{err: context.Canceled, want: "canceled"},
		{err: context.DeadlineExceeded, want: "deadline"},
		{err: model.ErrActiveClientNotFound, want: "source_inactive"},
		{err: errors.New("private error text must not become a label"), want: "other"},
	} {
		if got := contractRejectionFailureClass(test.err); got != test.want {
			t.Fatalf("fixed error class = %q, want %q", got, test.want)
		}
	}
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_connect_contract_rejections_total" {
			continue
		}
		if len(family.Metric) != 40 {
			t.Fatalf("rejection collector has %d cells, want 40 initialized cells", len(family.Metric))
		}
		for _, sample := range family.Metric {
			if len(sample.Label) != 3 {
				t.Fatal("rejection collector gained an unreviewed label")
			}
		}
		return
	}
	t.Fatal("rejection collector is absent")
}

func rejectionCount(t testing.TB, ingress, cause, companion string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "urnetwork_connect_contract_rejections_total" {
			continue
		}
		for _, sample := range family.Metric {
			labels := map[string]string{}
			for _, label := range sample.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["ingress"] == ingress && labels["cause"] == cause && labels["companion"] == companion {
				return sample.GetCounter().GetValue()
			}
		}
	}
	// A baseline without this collector must execute the real controller
	// branch before failing its expected increment below.
	return 0
}

// These real controller paths all return a protocol error with nil Go error.
// Neither missing mode nor missing key reaches financial allocation, so a
// protocol-reject total cannot stand in for exhausted grant credit.
func TestCreateContractRejectionCauseIncludesPreAllocationFailures(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network, peerNetwork, source, destination := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, n := range []server.Id{network, peerNetwork} {
			model.Testing_CreateNetwork(ctx, n, "rejection-fixture-"+n.String(), server.NewId())
		}
		model.Testing_CreateDevice(ctx, network, server.NewId(), source, "source", "fixture")
		model.Testing_CreateDevice(ctx, peerNetwork, server.NewId(), destination, "destination", "fixture")
		server.Raise(model.AddBasicTransferBalance(ctx, network, 4*1024*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
		for _, test := range []struct {
			cause     string
			companion bool
			modes     map[model.ProvideMode][]byte
			wire      protocol.ContractError
		}{
			{cause: "provide_mode_unavailable", modes: map[model.ProvideMode][]byte{}, wire: protocol.ContractError_NoPermission},
			{cause: "provide_secret_unavailable", companion: true, modes: map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{42}, 32)}, wire: protocol.ContractError_NoPermission},
			{cause: "missing_companion_origin", companion: true, modes: map[model.ProvideMode][]byte{model.ProvideModeStream: bytes.Repeat([]byte{42}, 32)}, wire: protocol.ContractError_Reliability},
		} {
			model.SetProvide(ctx, destination, test.modes)
			companion := "false"
			if test.companion {
				companion = "true"
			}
			before := rejectionCount(t, "internal", test.cause, companion)
			frames, err := CreateContract(ctx, source, &protocol.CreateContract{DestinationId: destination.Bytes(), TransferByteCount: 1024 * 1024, Companion: test.companion}, connect.DefaultContractManagerSettings())
			if err != nil || len(frames) != 1 {
				t.Fatalf("%s changed transport outcome: %v", test.cause, err)
			}
			message, err := connect.FromFrame(frames[0])
			returnConnectControlFrames(frames)
			if err != nil {
				t.Fatal(err)
			}
			result := message.(*protocol.CreateContractResult)
			if result.Contract != nil || result.Error == nil || *result.Error != test.wire {
				t.Fatalf("%s changed protocol refusal", test.cause)
			}
			if got := rejectionCount(t, "internal", test.cause, companion); got != before+1 {
				t.Fatalf("%s did not count its actual rejection: %v -> %v", test.cause, before, got)
			}
			if model.GetOpenTransferByteCount(ctx, network) != 0 {
				t.Fatal("pre-allocation refusal changed reservations")
			}
		}
	})
}

// The completed disposition and narrower cause cells preserve an actual
// healthy signed allocation and a separate insufficient-credit refusal.
func TestCreateContractRejectionCausePreservesHealthyAccounting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		network, peerNetwork, source, destination := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, n := range []server.Id{network, peerNetwork} {
			model.Testing_CreateNetwork(ctx, n, "rejection-accounting-"+n.String(), server.NewId())
		}
		model.Testing_CreateDevice(ctx, network, server.NewId(), source, "source", "fixture")
		model.Testing_CreateDevice(ctx, peerNetwork, server.NewId(), destination, "destination", "fixture")
		// The 512 KiB left after the 1 MiB contract is below the shrink-to-fit
		// floor, so the larger request is still refused.
		server.Raise(model.AddBasicTransferBalance(ctx, network, 1024*1024+512*1024, server.NowUtc(), server.NowUtc().Add(time.Hour)))
		model.SetProvide(ctx, destination, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{42}, 32)})
		httpCtx := context.WithValue(ctx, controlHttpIngressKey{}, true)
		before := rejectionCount(t, "http", "insufficient_balance", "false")
		for _, amount := range []int{1024 * 1024, 8 * 1024 * 1024} {
			frames, err := CreateContract(httpCtx, source, &protocol.CreateContract{DestinationId: destination.Bytes(), TransferByteCount: uint64(amount)}, connect.DefaultContractManagerSettings())
			if err != nil || len(frames) != 1 {
				t.Fatal("actual contract request failed", err)
			}
			message, err := connect.FromFrame(frames[0])
			returnConnectControlFrames(frames)
			if err != nil {
				t.Fatal(err)
			}
			result := message.(*protocol.CreateContractResult)
			if amount == 1024*1024 {
				if result.Contract == nil || result.Error != nil || rejectionCount(t, "http", "insufficient_balance", "false") != before {
					t.Fatal("healthy reply became a rejection")
				}
			} else if result.Contract != nil || result.Error == nil || *result.Error != protocol.ContractError_InsufficientBalance || rejectionCount(t, "http", "insufficient_balance", "false") != before+1 {
				t.Fatal("insufficient balance lost its distinct wire/cause outcome")
			}
		}
		if model.GetOpenTransferByteCount(ctx, network) != 1024*1024 {
			t.Fatal("observation changed reservation accounting")
		}
	})
}

// A grant census that stops at its selection bound has not shown that the
// payer is short (the model reports that funding is unknown), so the refusal
// must not tell the user they are out of data. This payer holds more than the
// request in total, spread over more grants than one request may select.
func TestCreateContractIncompleteGrantCensusIsNotInsufficientBalance(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
		defer cancel()
		networkId, peerNetworkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for _, fixtureNetworkId := range []server.Id{networkId, peerNetworkId} {
			model.Testing_CreateNetwork(ctx, fixtureNetworkId, "census-fixture-"+fixtureNetworkId.String(), server.NewId())
		}
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "source", "fixture")
		model.Testing_CreateDevice(ctx, peerNetworkId, server.NewId(), destinationId, "destination", "fixture")
		// 400 grants of 3 KiB hold 1200 KiB, more than the 1 MiB request. The
		// default selection bound of 256 grants stops the census at 768 KiB.
		startTime, endTime := server.NowUtc(), server.NowUtc().Add(time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			for range 400 {
				server.Raise(model.AddBasicTransferBalanceInTx(tx, ctx, networkId, 3*1024, startTime, endTime))
			}
		})
		model.SetProvide(ctx, destinationId, map[model.ProvideMode][]byte{model.ProvideModePublic: bytes.Repeat([]byte{42}, 32)})

		failureCount := func(cause string) float64 {
			return testutil.ToFloat64(contractFailureCounter.WithLabelValues(cause, "false"))
		}
		beforeOther, beforeBalance := failureCount("other"), failureCount("insufficient_balance")
		beforeRejection := rejectionCount(t, "internal", "other", "false")
		frames, err := CreateContract(ctx, sourceId, &protocol.CreateContract{DestinationId: destinationId.Bytes(), TransferByteCount: 1024 * 1024}, connect.DefaultContractManagerSettings())
		if err != nil || len(frames) != 1 {
			t.Fatalf("census refusal changed transport outcome: %v", err)
		}
		message, err := connect.FromFrame(frames[0])
		returnConnectControlFrames(frames)
		if err != nil {
			t.Fatal(err)
		}
		result := message.(*protocol.CreateContractResult)
		if result.Contract != nil || result.Error == nil {
			t.Fatal("an incomplete grant census must refuse the contract")
		}
		if *result.Error != protocol.ContractError_Setup {
			t.Fatalf("incomplete grant census returned %s, want %s", *result.Error, protocol.ContractError_Setup)
		}
		if failureCount("other") != beforeOther+1 || failureCount("insufficient_balance") != beforeBalance {
			t.Fatal("an incomplete grant census must count as cause other, not insufficient_balance")
		}
		if got := rejectionCount(t, "internal", "other", "false"); got != beforeRejection+1 {
			t.Fatalf("rejection cause other = %v, want %v", got, beforeRejection+1)
		}
		if model.GetOpenTransferByteCount(ctx, networkId) != 0 {
			t.Fatal("refusal changed reservation accounting")
		}
	})
}
