// Finite publication boundaries keep independently approved provenance separate
// from a byte-valid SDK inventory or the ordinary artifact signing authority.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"google.golang.org/protobuf/proto"
)

// This is a parser/collection fixture only. It grants no window, participant or
// payment completeness; the public-path root obtains cuts from actual SDKs.
func providerWorkCandidateCut(t testing.TB, ids ...server.Id) []byte {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{182}, 32))
	cut := protocol.OriginalWorkCut{DomainHash: [32]byte{183}, ClientId: [16]byte{1}, Generation: [16]byte{184}, Epoch: 7, Block: 20, BlockHash: [32]byte{185}, Complete: true, Contracts: []protocol.OriginalWorkContract{}}
	for _, id := range ids {
		stored, err := proto.Marshal(&protocol.StoredContract{ContractId: id.Bytes(), SourceId: server.Id{1}.Bytes(), DestinationId: server.Id{2}.Bytes(), TransferByteCount: 121})
		if err != nil {
			t.Fatal(err)
		}
		cut.Contracts = append(cut.Contracts, protocol.OriginalWorkContract{ContractId: [16]byte(id), StoredContract: stored})
	}
	cut, err := protocol.SignOriginalWorkCut(t.Context(), cut, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cut.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Both cuts can contain contracts absent from this window, including late
// traffic. Their original reservation/outcome receipts must still be acquired.
func TestProviderWorkCandidatesIncludeBothCutsAndWindow(t *testing.T) {
	a, b, c := server.Id{3}, server.Id{4}, server.Id{5}
	inventory := &payoutartifact.WholeWorkInventory{Window: &payoutartifact.ClosedWorkWindow{Records: []payoutartifact.ClosedWorkWindowRecord{{ContractId: a.String()}}}, Owners: []payoutartifact.WholeWorkOwnerCuts{{Start: providerWorkCandidateCut(t, b), End: providerWorkCandidateCut(t, b, c)}}}
	ids, err := providerWorkCandidateIds(t.Context(), inventory)
	if err != nil || len(ids) != 3 || ids[0] != a || ids[1] != b || ids[2] != c {
		t.Fatal("cut-only candidates escaped original receipt acquisition", ids, err)
	}
	inventory.Window.Records[0].ContractId = "invalid"
	if _, err := providerWorkCandidateIds(t.Context(), inventory); !errors.Is(err, payoutartifact.ErrClosedWorkIntegrity) {
		t.Fatal("invalid original candidate admitted", err)
	}
}

// Configuration must independently authorize the attribution purpose. The
// artifact signer and the authority's body cannot add that permission.
func TestProviderWorkAttributionRequiresExplicitPolicyPin(t *testing.T) {
	base := fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nauthority_signer: %s\nclient_key_root_signer: %s\n", ProviderWorkPolicySchema, [32]byte{186}, [32]byte{187}, common.Address{188}.Hex(), common.Address{189}.Hex())
	pop := server.Vault.PushSimpleResource("provider_work.yml", []byte(base))
	_, _, expected, err := LoadProviderWorkAuthorityPolicy()
	pop()
	if err != nil || expected.AttributionSigner != (common.Address{}) {
		t.Fatal("missing independent purpose became an approval", err)
	}
	pop = server.Vault.PushSimpleResource("provider_work.yml", []byte(base+"attribution_signer: "+common.Address{188}.Hex()+"\n"))
	_, _, expected, err = LoadProviderWorkAuthorityPolicy()
	pop()
	if err != nil || expected.AttributionSigner != expected.AuthoritySigner {
		t.Fatal("approved purpose was lost", err)
	}
	pop = server.Vault.PushSimpleResource("provider_work.yml", []byte(base+"attribution_signer: "+common.Address{190}.Hex()+"\n"))
	_, _, _, err = LoadProviderWorkAuthorityPolicy()
	pop()
	if err == nil {
		t.Fatal("foreign signer became roster attribution authority")
	}
}

// A valid whole-work component is insufficient for an opted-in publication
// while async participant originals are missing. This tests only the gate.
func TestProviderWorkPublicationWaitsForApprovedAttribution(t *testing.T) {
	key, err := crypto.HexToECDSA(strings.Repeat("14", 32))
	if err != nil {
		t.Fatal(err)
	}
	root := crypto.PubkeyToAddress(key.PublicKey)
	domain := snprotocol.ClientKeyHistoryDomain{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 521, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: [32]byte{4}, PolicyHash: [32]byte{5}, NoID: 1}
	digest, err := domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	source := snprotocol.ProviderWorkSourceAuthority{DomainHash: digest, SourceId: server.Id{191}.String(), Generation: server.Id{192}.String(), PublicKey: [32]byte{193}, FromUnixMicro: 1, ThroughUnixMicro: 2, MaxEndpointEvents: 4, MaxCohortMembers: 4, DirectoryPublicKeys: [][32]byte{}}
	authority, err := payoutartifact.SignWholeWorkAuthority(t.Context(), payoutartifact.WholeWorkAuthority{Domain: domain, Epoch: 7, Start: payoutartifact.Boundary{Number: 20, Hash: common.Hash{6}.Hex()}, End: payoutartifact.Boundary{Number: 30, Hash: common.Hash{7}.Hex()}, RequestPublicKey: [32]byte{8}, Owners: []payoutartifact.WholeWorkOwner{}, ExpectedProviders: []payoutartifact.WholeWorkExpectedProvider{}, PriorContracts: []payoutartifact.WholeWorkPriorContract{}, WorkSources: []snprotocol.ProviderWorkSourceAuthority{source}}, key)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := authority.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	inventory := &payoutartifact.WholeWorkInventory{Authority: raw}
	expected := payoutartifact.WholeWorkExpectation{AuthoritySigner: root, ClientKeyRootSigner: root, AttributionSigner: root}
	verified := &payoutartifact.VerifiedWholeWorkInventory{Complete: true}
	if err := requireProviderWorkPublication(t.Context(), inventory, expected, verified); !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) {
		t.Fatal("missing async originals froze publication", err)
	}
	verified.AttributionComplete = true
	if err := requireProviderWorkPublication(t.Context(), inventory, expected, verified); err != nil {
		t.Fatal("complete approved component remained blocked", err)
	}
	expected.AttributionSigner = common.Address{}
	if err := requireProviderWorkPublication(t.Context(), inventory, expected, verified); !errors.Is(err, payoutartifact.ErrClosedWorkUnavailable) {
		t.Fatal("component supplied its own purpose authority", err)
	}
}

// Real cancellation and I/O causes survive alongside any authenticated hard
// contradiction, without converting a missing read into an integrity failure.
func TestProviderWorkOriginalFailurePreservesCauses(t *testing.T) {
	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(syscall.EIO)
	soft := errors.Join(ctx.Err(), context.Cause(ctx))
	got := providerWorkOriginalFailure(soft)
	if !errors.Is(got, context.Canceled) || !errors.Is(got, syscall.EIO) || errors.Is(got, payoutartifact.ErrClosedWorkIntegrity) {
		t.Fatal("source read became integrity", got)
	}
	got = providerWorkOriginalFailure(errors.Join(soft, snprotocol.ErrProviderWorkIntegrity))
	if !errors.Is(got, context.Canceled) || !errors.Is(got, syscall.EIO) || !errors.Is(got, payoutartifact.ErrClosedWorkIntegrity) {
		t.Fatal("actual mixed contradiction lost cause", got)
	}
}
