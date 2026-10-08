// Combined custody remains byte-exact, deterministic and independently bounded
// before ordinary and open originals reach the public artifact verifier.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	snprotocol "github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026"
)

// These opaque transport samples assert no receipt authority. Only the actual
// public producer tests supply canonical source signatures to the next layer.
func TestProviderWorkOriginalPoolRetainsExactUniqueSortedBytes(t *testing.T) {
	first, second := []byte{2, 3}, []byte{1, 4}
	result, err := mergeProviderWorkOriginalPools(t.Context(), [][]byte{first}, [][]byte{second, first})
	if err != nil || len(result) != 2 || !bytes.Equal(result[0], second) || !bytes.Equal(result[1], first) {
		t.Fatal("combined originals were selected or repeated by arrival order", result, err)
	}
	first[0], second[0] = 9, 9
	if result[0][0] != 1 || result[1][0] != 2 {
		t.Fatal("caller mutation changed retained original pool")
	}
}

// Each input owner may fit alone while their union exceeds the shared byte cap.
func TestProviderWorkOriginalPoolBoundsCombinedCustody(t *testing.T) {
	first := bytes.Repeat([]byte{1}, 4*1024*1024)
	second := bytes.Repeat([]byte{2}, 4*1024*1024)
	if result, err := mergeProviderWorkOriginalPools(t.Context(), [][]byte{first}, [][]byte{second, {3}}); result != nil || !errors.Is(err, payoutartifact.ErrClosedWorkCapacity) {
		t.Fatal("combined original owners exceeded their shared custody cap", err)
	}
	records := make([][]byte, 32769)
	for index := range records {
		records[index] = []byte{byte(index), byte(index >> 8), byte(index >> 16)}
	}
	if result, err := mergeProviderWorkOriginalPools(t.Context(), records[:16384], records[16384:]); result != nil || !errors.Is(err, payoutartifact.ErrClosedWorkCapacity) {
		t.Fatal("combined original owners exceeded their shared record cap", err)
	}
}

// A genuine roster cannot authorize observation with an unproved raw clock.
// The actual attachment owner refuses before invoking the live model producer.
func TestProviderWorkObservationRequiresAuthenticatedOriginalClock(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{221}, 32))
		f.authority.WorkSources = []snprotocol.ProviderWorkSourceAuthority{{DomainHash: f.domain, SourceId: server.Id{222}.String(), Generation: server.Id{223}.String(), PublicKey: [32]byte(key.Public().(ed25519.PublicKey)), FromUnixMicro: f.epoch.StartTime.UnixMicro(), ThroughUnixMicro: f.epoch.EndTime.UnixMicro() + 1, MaxEndpointEvents: 4, MaxCohortMembers: 4, DirectoryPublicKeys: [][32]byte{}}}
		f.retainAuthority(t)
		root := crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey)
		expected := payoutartifact.WholeWorkExpectation{AuthoritySigner: root, AttributionSigner: root, ClientKeyRootSigner: root}
		clock := &payoutartifact.ClosedWorkWindowClock{Start: f.authority.Start, End: f.authority.End, StartTime: f.epoch.StartTime, EndTime: f.epoch.EndTime, StartHeader: bytes.Clone(f.epoch.StartHeader), EndHeader: bytes.Clone(f.epoch.EndHeader)}
		clock.EndHeader[len(clock.EndHeader)-1] ^= 1
		inventory := &payoutartifact.WholeWorkInventory{Clock: clock, Owners: []payoutartifact.WholeWorkOwnerCuts{}, Window: &payoutartifact.ClosedWorkWindow{Records: []payoutartifact.ClosedWorkWindowRecord{{ContractId: server.Id{224}.String()}}}}
		if err := attachProviderWorkOriginals(t.Context(), f.authority, expected, inventory); !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) || inventory.AttributionOriginals != nil {
			t.Fatal("unproved original clock reached live observation or attachment", err)
		}
	})
}

// A canceled owner cannot acquire a partially merged authoritative collection.
func TestProviderWorkOriginalPoolCancellationReturnsNoPartialPool(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if result, err := mergeProviderWorkOriginalPools(ctx, [][]byte{{1}}); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled original pool returned partial authority", result, err)
	}
}
