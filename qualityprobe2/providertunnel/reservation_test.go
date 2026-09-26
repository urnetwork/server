// Contract-request tests pin the short-probe reservation budget while keeping
// full/bandwidth defaults and unrelated transfer behavior unchanged.
package providertunnel

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// Captures the actual serialized create request and returns every frame owner.
type reservationTestOob struct {
	testing *testing.T
	counts  chan connect.ByteCount
}

// No contract is returned: the test observes requested reservations only.
func (self *reservationTestOob) SendControl(frames []*protocol.Frame, callback connect.OobResultFunction) {
	for _, frame := range frames {
		if frame.MessageType == protocol.MessageType_TransferCreateContract {
			request := &protocol.CreateContract{}
			if err := connect.ProtoUnmarshal(frame.MessageBytes, request); err != nil {
				self.testing.Errorf("decode create contract: %v", err)
			} else {
				self.counts <- connect.ByteCount(request.TransferByteCount)
			}
		}
		connect.MessagePoolReturn(frame.MessageBytes)
	}
	if callback != nil {
		callback(nil, nil)
	}
}

// A first contract and its unused successor use the real CreateContract
// serializer. The default control reproduces the disproportionate successor.
func TestProviderTunnelContractReservationRequests(t *testing.T) {
	const opening connect.ByteCount = 1024 * 1024
	for _, test := range []struct {
		name   string
		target connect.ByteCount
		want   []connect.ByteCount
	}{
		{name: "short_probe", target: opening, want: []connect.ByteCount{opening, opening, opening, opening}},
		{name: "small_target", target: opening / 16, want: []connect.ByteCount{opening / 16, opening / 16, opening / 16, opening / 16}},
		{name: "full_default", want: []connect.ByteCount{opening, opening + (128*opening-opening)/4, 128 * opening, 128 * opening}},
		{name: "large_target_cannot_raise_defaults", target: math.MaxInt64, want: []connect.ByteCount{opening, opening + (128*opening-opening)/4, 128 * opening, 128 * opening}},
	} {
		func() {
			settings := providerTunnelClientSettings(test.target)
			settings.Log = connect.NewNoopLogger()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			oob := &reservationTestOob{testing: t, counts: make(chan connect.ByteCount, 8)}
			client := connect.NewClient(ctx, connect.NewId(), oob, settings)
			defer func() {
				if err := client.CloseAndWait(ctx); err != nil {
					t.Errorf("join request client: %v", err)
				}
			}()
			for _, networkPeer := range []bool{false, true} {
				key := connect.ContractKey{
					Destination: connect.DestinationId(connect.NewId()),
					NetworkPeer: networkPeer,
				}
				for index, sequence := range []uint64{0, 1, 4, 8} {
					client.ContractManager().CreateContract(key, sequence, 1024)
					select {
					case got := <-oob.counts:
						if got != test.want[index] {
							t.Errorf("%s network peer=%t sequence=%d request=%d, want %d", test.name, networkPeer, sequence, got, test.want[index])
						}
					case <-ctx.Done():
						t.Fatal("create request was not observed")
					}
				}
			}
			// It is a per-contract ramp target, not a quota. A legal message
			// floor larger than that target must not become unsendable.
			floor := connect.ByteCount(256 * 1024 * 1024)
			client.ContractManager().CreateContract(connect.ContractKey{
				Destination: connect.DestinationId(connect.NewId()),
			}, 0, floor)
			select {
			case got := <-oob.counts:
				if got != floor {
					t.Errorf("message floor request=%d, want %d", got, floor)
				}
			case <-ctx.Done():
				t.Fatal("message floor request was not observed")
			}
		}()
	}
}

// Budgeting may change only the contract ramp, not carrier, prefetch, crypto,
// or throughput options. A fresh settings tree prevents cross-probe mutation.
func TestProviderTunnelContractReservationSettingsIsolation(t *testing.T) {
	const opening connect.ByteCount = 1024 * 1024
	var constructed sync.WaitGroup
	var mutated sync.WaitGroup
	var done sync.WaitGroup
	constructed.Add(16)
	mutated.Add(16)
	done.Add(16)
	mutate := make(chan struct{})
	verify := make(chan struct{})
	for range 16 {
		go func() {
			defer done.Done()
			bounded := providerTunnelClientSettings(opening)
			defaults := providerTunnelClientSettings(0)
			fresh := providerTunnelClientSettings(opening)
			if !reflect.DeepEqual(defaults, connect.DefaultClientSettings()) {
				t.Error("unset reservation changed shared client defaults")
			}
			expected := connect.DefaultClientSettings()
			expected.ContractManagerSettings.StandardContractTransferByteCount = opening
			if !reflect.DeepEqual(bounded, expected) {
				t.Error("bounded settings differ beyond the contract reservation ramp")
			}
			constructed.Done()
			<-mutate
			bounded.ContractManagerSettings.StandardContractTransferByteCount = 1
			bounded.SendBufferSettings.PrewarmOpeningContract = false
			mutated.Done()
			<-verify
			if !reflect.DeepEqual(fresh, expected) || !reflect.DeepEqual(defaults, connect.DefaultClientSettings()) {
				t.Error("one generated client changed another client's settings")
			}
		}()
	}
	constructed.Wait()
	close(mutate)
	mutated.Wait()
	close(verify)
	done.Wait()
}

// Verifies the actual Open factory receives the selected budget, not just a
// helper that production might accidentally bypass.
func TestOpenUsesContractReservationSettings(t *testing.T) {
	cfg := dummyOpenConfig()
	cfg.ContractReservationByteCount = 1024 * 1024
	tunnel, err := Open(context.Background(), cfg, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer tunnel.Close()
	settings := tunnel.generator.NewClientSettings()
	if settings.ContractManagerSettings.StandardContractTransferByteCount != cfg.ContractReservationByteCount {
		t.Fatal("Open bypassed the configured contract reservation budget")
	}
}

// Invalid budgets must fail before constructing a Tun or opening any API work.
func TestOpenRejectsNegativeContractReservation(t *testing.T) {
	original := createTun
	called := false
	createTun = func(context.Context, *connect.DnsResolverSettings) (*connect.Tun, error) {
		called = true
		return nil, errors.New("unexpected tun construction")
	}
	t.Cleanup(func() { createTun = original })
	cfg := dummyOpenConfig()
	cfg.ContractReservationByteCount = -1
	tunnel, err := Open(context.Background(), cfg, connect.NewId())
	if !errors.Is(err, ErrContractReservation) || tunnel != nil || called {
		t.Fatalf("negative reservation accepted: error=%v tunnel=%t constructed=%t", err, tunnel != nil, called)
	}
}
