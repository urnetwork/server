// Production payout assembly consumes a separately signed roster and original
// cuts. A known-empty window is explicit, and old missing originals stay unknown.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/urfoundation/sn/payoutartifact"
	snprotocol "github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/startifact"
)

// The independently provisioned fixture owns no live key or network authority.
type providerWorkWindowFixture struct {
	blobRoot  string
	cfg       *StConfig
	epoch     *StPayoutEpochAuthority
	authority payoutartifact.WholeWorkAuthority
	domain    [32]byte
	approver  ed25519.PrivateKey
	window    *payoutartifact.ClosedWorkWindow
}

// Original RLP headers fix the complete UTC window; the signed roster starts
// empty and may be populated by an individual test before its immutable intake.
func newProviderWorkWindowFixture(t testing.TB) *providerWorkWindowFixture {
	t.Helper()
	base, _, cfg := newStClientKeyHistoryControllerFixtureWithoutBlobStore(t)
	cfg.ReliabilityAMin = 8
	f := &providerWorkWindowFixture{blobRoot: controllerUseLocalBlobStore(t), cfg: cfg, approver: ed25519.NewKeyFromSeed(bytes.Repeat([]byte{101}, 32))}
	startTime := time.Unix(1_800_000_000, 0).UTC()
	endTime := startTime.Add(time.Hour)
	startHeader := &types.Header{Number: big.NewInt(20), Time: uint64(startTime.Unix()), Difficulty: big.NewInt(0), GasLimit: 1, Extra: []byte{1}}
	endHeader := &types.Header{Number: big.NewInt(30), Time: uint64(endTime.Unix()), Difficulty: big.NewInt(0), GasLimit: 1, Extra: []byte{2}}
	startRaw, err := rlp.EncodeToBytes(startHeader)
	if err != nil {
		t.Fatal(err)
	}
	endRaw, err := rlp.EncodeToBytes(endHeader)
	if err != nil {
		t.Fatal(err)
	}
	f.epoch = &StPayoutEpochAuthority{Epoch: 7, PolicyHash: cfg.PolicyHash, Start: snprotocol.ClientKeyEffectiveBoundary{Block: 20, Hash: [32]byte(startHeader.Hash())}, End: snprotocol.ClientKeyEffectiveBoundary{Block: 30, Hash: [32]byte(endHeader.Hash())}, StartTime: startTime, EndTime: endTime, StartHeader: startRaw, EndHeader: endRaw}
	f.domain, err = base.domain.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f.authority = payoutartifact.WholeWorkAuthority{Domain: base.domain, Epoch: f.epoch.Epoch, Start: payoutartifact.Boundary{Number: 20, Hash: startHeader.Hash().Hex()}, End: payoutartifact.Boundary{Number: 30, Hash: endHeader.Hash().Hex()}, RequestPublicKey: [32]byte(f.approver.Public().(ed25519.PublicKey)), Owners: []payoutartifact.WholeWorkOwner{}, PriorContracts: []payoutartifact.WholeWorkPriorContract{}, ExpectedProviders: []payoutartifact.WholeWorkExpectedProvider{}}
	f.window = &payoutartifact.ClosedWorkWindow{Schema: payoutartifact.ClosedWorkWindowSchema, Start: startTime.Format(time.RFC3339Nano), End: endTime.Format(time.RFC3339Nano), Records: []payoutartifact.ClosedWorkWindowRecord{}}
	t.Cleanup(server.Vault.PushSimpleResource("provider_work.yml", []byte(fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nauthority_signer: %s\nclient_key_root_signer: %s\n", ProviderWorkPolicySchema, f.domain, f.authority.RequestPublicKey, crypto.PubkeyToAddress(cfg.RootKey.PublicKey).Hex(), crypto.PubkeyToAddress(cfg.RootKey.PublicKey).Hex()))))
	return f
}

// Intake calls the same immutable signature checker as the public route.
func (self *providerWorkWindowFixture) retainAuthority(t testing.TB) []byte {
	t.Helper()
	authority, err := payoutartifact.SignWholeWorkAuthority(t.Context(), self.authority, self.cfg.RootKey)
	if err != nil {
		t.Fatal(err)
	}
	self.authority = authority
	raw, err := authority.Bytes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.RetainProviderWorkAuthority(t.Context(), raw, self.domain, authority.RequestPublicKey, crypto.PubkeyToAddress(self.cfg.RootKey.PublicKey)); err != nil {
		t.Fatal(err)
	}
	return raw
}

// This invokes the actual production payout producer and immutable blob path.
// The companion survives a repeated producer call which owns no fresh inputs.
func TestProviderWorkProductionPayoutSignsAndRetainsKnownEmptyWindow(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		original := f.retainAuthority(t)
		client := newStubStClient(&StEpochState{})
		if _, _, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, f.epoch.StartTime, f.epoch.EndTime, f.epoch.Start.Block, f.epoch.End.Block, f.epoch); err != nil {
			t.Fatal("actual payout failed", err)
		}
		record := model.GetStPayoutArtifact(t.Context(), f.cfg.DeploymentKey(), f.epoch.Epoch, f.cfg.NoId)
		if record == nil {
			t.Fatal("payout did not retain artifact")
		}
		store, ok := server.LoadBlobStore()
		if !ok {
			t.Fatal("fixture blob owner absent")
		}
		artifact, raw, err := startifact.Read(t.Context(), store, record.ContentHash)
		if err != nil {
			t.Fatal(err)
		}
		if artifact.ClosedWork == nil || artifact.ClosedWork.WholeInventory == nil || len(artifact.ClosedWork.WholeInventory.Window.Records) != 0 || !bytes.Equal(artifact.ClosedWork.WholeInventory.Authority, original) {
			t.Fatal("signed payout omitted independently complete empty window")
		}
		_, _, expected, err := LoadProviderWorkAuthorityPolicy()
		if err != nil {
			t.Fatal(err)
		}
		verified, err := payoutartifact.VerifyWholeWorkInventory(t.Context(), artifact, expected)
		if err != nil || !verified.Complete || verified.Contracts != 0 {
			t.Fatal("actual empty payout did not verify", err)
		}
		digest, err := providerWorkPolicyHash(record.ContentHash[len("sha256:"):])
		if err != nil {
			t.Fatal(err)
		}
		first, err := ProviderWorkWindow(t.Context(), f.domain, f.epoch.Epoch, digest, [32]byte{})
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := stComputeReleasePayout(t.Context(), f.cfg, client, f.epoch.Epoch, time.Time{}, time.Time{}, 0, 0, nil); err != nil {
			t.Fatal("immutable payout restart requested fresh authority", err)
		}
		_, after, err := startifact.Read(t.Context(), store, record.ContentHash)
		if err != nil || !bytes.Equal(raw, after) {
			t.Fatal("restart changed signed artifact", err)
		}
		second, err := ProviderWorkWindow(t.Context(), f.domain, f.epoch.Epoch, digest, sha256.Sum256(original))
		if err != nil || !bytes.Equal(first, second) {
			t.Fatal("companion changed on exact retrieval", err)
		}
		if _, err := ProviderWorkWindow(t.Context(), f.domain, f.epoch.Epoch, digest, [32]byte{102}); !errors.Is(err, model.ErrProviderWorkConflict) {
			t.Fatal("foreign authority selector was accepted", err)
		}
	})
}

// Two exact original boundaries are joined in the independently signed owner
// order. Missing one cannot be guessed from an empty query or current registry.
func TestProviderWorkWindowJoinsOnlyApprovedOwnerOriginals(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		sdk := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{103}, 32))
		owner := payoutartifact.WholeWorkOwner{ClientId: [16]byte(server.NewId()), NetworkId: [16]byte(server.NewId()), Generation: [16]byte(server.NewId()), PublicKey: [32]byte(sdk.Public().(ed25519.PublicKey))}
		f.authority.Owners = []payoutartifact.WholeWorkOwner{owner}
		f.retainAuthority(t)
		if got, _, err := stPrepareWholeWorkInventory(t.Context(), f.cfg, f.epoch, f.window); err != nil || got != nil {
			t.Fatal("missing owner originals became complete", got, err)
		}
		var originals []protocol.OriginalWorkCutSubmission
		for _, side := range []struct {
			kind     string
			boundary snprotocol.ClientKeyEffectiveBoundary
		}{{kind: "start", boundary: f.epoch.Start}, {kind: "end", boundary: f.epoch.End}} {
			request, err := protocol.SignOriginalWorkRequest(protocol.OriginalWorkRequest{RequestId: [16]byte(server.NewId()), DomainHash: f.domain, ClientId: owner.ClientId, Generation: owner.Generation, PublicKey: owner.PublicKey, Epoch: f.epoch.Epoch, Kind: side.kind, Block: side.boundary.Block, BlockHash: side.boundary.Hash, IssuedAtUnix: f.epoch.StartTime.Unix(), ExpiresAtUnix: f.epoch.StartTime.Unix() + 300}, f.approver)
			if err != nil {
				t.Fatal(err)
			}
			cut, err := protocol.SignOriginalWorkCut(t.Context(), protocol.OriginalWorkCut{DomainHash: f.domain, ClientId: owner.ClientId, Generation: owner.Generation, Epoch: f.epoch.Epoch, Block: side.boundary.Block, BlockHash: side.boundary.Hash, Complete: true, Contracts: []protocol.OriginalWorkContract{}}, sdk)
			if err != nil {
				t.Fatal(err)
			}
			requestRaw, err := request.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			cutRaw, err := cut.Bytes(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			submission := protocol.OriginalWorkCutSubmission{Request: requestRaw, Cut: cutRaw}
			if _, err := model.RetainProviderWorkRequest(t.Context(), requestRaw, f.authority.RequestPublicKey, f.domain, f.epoch.StartTime); err != nil {
				t.Fatal(err)
			}
			if _, err := model.RetainProviderWorkCut(t.Context(), submission, f.authority.RequestPublicKey, f.domain); err != nil {
				t.Fatal(err)
			}
			originals = append(originals, submission)
		}
		got, _, err := stPrepareWholeWorkInventory(t.Context(), f.cfg, f.epoch, f.window)
		if err != nil || got == nil || len(got.Owners) != 1 || !bytes.Equal(got.Owners[0].StartRequest, originals[0].Request) || !bytes.Equal(got.Owners[0].Start, originals[0].Cut) || !bytes.Equal(got.Owners[0].EndRequest, originals[1].Request) || !bytes.Equal(got.Owners[0].End, originals[1].Cut) {
			t.Fatal("approved boundary original join changed", err)
		}
	})
}

// An ordinary artifact signer cannot declare complete SDK coverage, and an
// independently signed different roster cannot replace a retained same epoch.
func TestProviderWorkAuthorityIsIndependentAndImmutable(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		f := newProviderWorkWindowFixture(t)
		original := f.retainAuthority(t)
		changed := f.authority
		changed.End.Number++
		signed, err := payoutartifact.SignWholeWorkAuthority(t.Context(), changed, f.cfg.RootKey)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := signed.Bytes(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.RetainProviderWorkAuthority(t.Context(), raw, f.domain, f.authority.RequestPublicKey, crypto.PubkeyToAddress(f.cfg.RootKey.PublicKey)); !errors.Is(err, model.ErrProviderWorkConflict) {
			t.Fatal("same epoch roster replaced retained approval", err)
		}
		if _, err := model.RetainProviderWorkAuthority(t.Context(), original, f.domain, f.authority.RequestPublicKey, crypto.PubkeyToAddress(f.cfg.ArtifactKey.PublicKey)); !errors.Is(err, model.ErrProviderWorkInvalid) {
			t.Fatal("payout publisher selected complete roster", err)
		}
		f.epoch.StartHeader = nil
		if inventory, _, err := stPrepareWholeWorkInventory(t.Context(), f.cfg, f.epoch, f.window); err != nil || inventory != nil {
			t.Fatal("missing historical header became a clock", err)
		}
	})
}

// Strict policy parsing never accepts a body key, extra document or ambiguous
// unknown field as a replacement for separately provisioned authority.
func TestProviderWorkPolicyRequiresExactIndependentPins(t *testing.T) {
	for _, raw := range []string{"schema: wrong\ndomain_hash: 00\nrequest_public_key: 00\n", fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\nextra: true\n", ProviderWorkPolicySchema, [32]byte{1}, [32]byte{2}), fmt.Sprintf("schema: %s\ndomain_hash: %x\nrequest_public_key: %x\n---\nschema: ignored\n", ProviderWorkPolicySchema, [32]byte{1}, [32]byte{2})} {
		pop := server.Vault.PushSimpleResource("provider_work.yml", []byte(raw))
		_, _, err := LoadProviderWorkPolicy()
		pop()
		if err == nil {
			t.Fatal("malformed authority config accepted")
		}
	}
}
