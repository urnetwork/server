package work

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026/model"
)

type urlFundingContractRequests struct {
	t      *testing.T
	amount chan model.ByteCount
}

func (self *urlFundingContractRequests) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	for _, frame := range frames {
		if frame.MessageType == protocol.MessageType_TransferCreateContract {
			request := &protocol.CreateContract{}
			if err := connect.ProtoUnmarshal(frame.MessageBytes, request); err != nil {
				self.t.Error(err)
			} else {
				self.amount <- model.ByteCount(request.TransferByteCount)
			}
		}
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	if callback != nil {
		callback(nil, nil)
	}
}

// Drive the real pinned ContractManager serializer through the complete ramp,
// then current, announced-ahead and prefetched standard contracts. Every request
// remains reserved in this ledger: no close callback or settlement gives credit
// back. Both the origin and companion sequences charge the shard's one payer.
func TestUrlProbeShardFundingCoversUnreclaimedContractRequestsTenfold(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	settings := connect.DefaultClientSettings()
	settings.Log = connect.NewNoopLogger()
	oob := &urlFundingContractRequests{t: t, amount: make(chan model.ByteCount, 1)}
	client := connect.NewClient(ctx, connect.NewId(), oob, settings)
	defer func() {
		if err := client.CloseAndWait(ctx); err != nil {
			t.Error(err)
		}
	}()
	var perGeneration model.ByteCount
	for _, companion := range []bool{false, true} {
		key := connect.ContractKey{Destination: connect.DestinationId(connect.NewId()), CompanionContract: companion}
		for sequence := uint64(0); sequence < settings.ContractManagerSettings.ContractTransferByteSeqScale+3; sequence++ {
			client.ContractManager().CreateContract(key, sequence, 1024)
			select {
			case amount := <-oob.amount:
				if amount <= 0 {
					t.Fatal("serialized request has no positive reservation")
				}
				perGeneration += amount
			case <-ctx.Done():
				t.Fatal("contract request did not complete")
			}
		}
	}
	for _, concurrency := range []int{1, 64, 5000} {
		for _, recreations := range []int{1, 2, 8} {
			args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("funding.example"), 0)
			args.UrlProbe.Concurrency = concurrency
			args.TunnelRecreateAttempts = recreations
			credit, err := providerUrlProbeShardCredit(args)
			if err != nil {
				t.Fatal(err)
			}
			anticipated := perGeneration * model.ByteCount(providerEgressFullSelectedLimit+concurrency) * model.ByteCount(recreations+1)
			if credit/10 < anticipated {
				t.Fatalf("concurrency=%d recreations=%d credit=%d below 10x unreclaimed requests=%d", concurrency, recreations, credit, anticipated)
			}
			// Adding unrelated shard accounts must not enlarge this allocation.
			args.ShardCount = 256
			other, err := providerUrlProbeShardCredit(args)
			if err != nil || other != credit {
				t.Fatalf("another shard changed private funding: %d %d %v", credit, other, err)
			}
		}
	}
}

// The old formula fits while the explicit tenfold full-pass requirement does
// not. Reject the geometry rather than wrapping or silently clipping funding.
func TestUrlProbeShardFundingRejectsHeadroomOverflow(t *testing.T) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("funding.example"), 0)
	args.ShardCount, args.UrlProbe.Concurrency = 1, 1
	args.TunnelRecreateAttempts = 1_000_000
	if _, err := providerUrlProbeShardCredit(args); err == nil {
		t.Fatal("tenfold pass exposure overflow accepted")
	}
	args.TunnelRecreateAttempts = 1
	args.UrlProbe.Concurrency = math.MaxInt
	if _, err := providerUrlProbeShardCredit(args); err == nil {
		t.Fatal("overflowing concurrency accepted")
	}
	if _, err := providerUrlProbeShardCredit(nil); err == nil {
		t.Fatal("missing funding owner accepted")
	}
}
