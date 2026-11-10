//go:build sidecar_pending

package model

// §20.7's config_admissions at api's verifier and an enrollment's admission
// (TEAM-M2-16, §22.7), on Postgres for the tier and the limit rows, and T2's
// FakeBlobs for the log.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// A first metadata cluster on T2's fake Blobs.
func testAdmissionFakeCluster(t testing.TB) sidecar.BlobCluster {
	t.Helper()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

// A network of the owner's in Postgres, and its log at base 1 through a
// sequencer whose verifier counts admissions: the owner's first device is
// admitted by the genesis.
type testAdmissionLog struct {
	ctx         context.Context
	networkId   server.Id
	ownerUserId server.Id
	cluster     sidecar.BlobCluster
	world       *sidecartest.ConfigLogSuiteWorld
	sequencer   *ConfigLogSequencer
}

func newTestAdmissionLog(t testing.TB) *testAdmissionLog {
	t.Helper()
	ctx := t.Context()
	networkId := server.NewId()
	ownerUserId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "admission-"+networkId.String(), ownerUserId)
	cluster := testAdmissionFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorldFor(ctx, cluster, sidecar.Id(networkId), sidecar.Id(ownerUserId), sidecar.NewId())
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	settings.AdmissionCap = CheckConfigAdmissionCap
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	if err := world.Start(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
		t.Fatal(err)
	}
	return &testAdmissionLog{ctx: ctx, networkId: networkId, ownerUserId: ownerUserId, cluster: cluster, world: world, sequencer: sequencer}
}

// A new Ed25519 identity key.
func testAdmissionIdentityKey(t testing.TB) ed25519.PublicKey {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey
}

// The owner's admission of another device of its own member, as the owner's
// first device signs it.
func (self *testAdmissionLog) ownerDeviceAdmit(t testing.TB, sidecarId sidecar.Id) []byte {
	t.Helper()
	return self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header:            self.world.Header(),
		NetworkId:         sidecar.Id(self.networkId).Bytes(),
		SidecarId:         sidecarId.Bytes(),
		IdentityPublicKey: testAdmissionIdentityKey(t),
		IdentityAlg:       "ed25519",
		MemberId:          sidecar.Id(self.ownerUserId).Bytes(),
	})
}

// The owner's removal of a device.
func (self *testAdmissionLog) ownerRevoke(sidecarId sidecar.Id) []byte {
	return self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
		Header:    self.world.Header(),
		SidecarId: sidecarId.Bytes(),
		Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
	})
}

// Appends signed and moves the world's tip to it.
func (self *testAdmissionLog) post(signed []byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendStatement(self.ctx, self.networkId, signed)
	if err == nil {
		self.world.SetTip(pin)
	}
	return pin, err
}

// §22.7: the cap refuses an admission at the tier's limit with QuotaRefusal,
// 402 while a higher tier lifts it, and writes nothing; a removal frees its
// room, so the refused device lands next; dark limits refuse nothing.
func TestConfigAdmissionsCapAtTheTiersLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		t.Cleanup(Testing_SetEnforceSidecarLimits(true))
		t.Cleanup(Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{ConfigAdmissionsLimitKey: 2}))
		t.Cleanup(Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{ConfigAdmissionsLimitKey: 500}))
		self := newTestAdmissionLog(t)

		connect.AssertEqual(t, ConfigAdmissionCount(self.sequencerState(t), self.world.Clock().Now().UnixMilli()), int64(1))
		second := sidecar.NewId()
		if _, err := self.post(self.ownerDeviceAdmit(t, second)); err != nil {
			t.Fatalf("the second admission: %v", err)
		}
		tip, err := self.sequencer.Tip(self.ctx, self.networkId)
		connect.AssertEqual(t, err, nil)

		third := sidecar.NewId()
		thirdAdmit := self.ownerDeviceAdmit(t, third)
		_, err = self.sequencer.AppendStatement(self.ctx, self.networkId, thirdAdmit)
		quotaRefusal := &QuotaRefusalError{}
		if !errors.As(err, &quotaRefusal) {
			t.Fatalf("past the cap: %v", err)
		}
		connect.AssertEqual(t, quotaRefusal.Refusal.LimitKey, ConfigAdmissionsLimitKey)
		connect.AssertEqual(t, quotaRefusal.Refusal.Limit, int64(2))
		connect.AssertEqual(t, quotaRefusal.Refusal.Used, int64(2))
		connect.AssertEqual(t, quotaRefusal.Refusal.Upgrade, true)
		connect.AssertEqual(t, quotaRefusal.HttpStatus(), http.StatusPaymentRequired)
		after, err := self.sequencer.Tip(self.ctx, self.networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, after.GetVersion(), tip.GetVersion())

		// a removal is no admission and frees its room
		if _, err := self.post(self.ownerRevoke(second)); err != nil {
			t.Fatalf("the removal: %v", err)
		}
		if _, err := self.post(self.ownerDeviceAdmit(t, third)); err != nil {
			t.Fatalf("the third admission after the removal: %v", err)
		}
		connect.AssertEqual(t, ConfigAdmissionCount(self.sequencerState(t), self.world.Clock().Now().UnixMilli()), int64(2))

		// dark limits count nothing
		Testing_SetEnforceSidecarLimits(false)
		if _, err := self.post(self.ownerDeviceAdmit(t, sidecar.NewId())); err != nil {
			t.Fatalf("an admission with the limits dark: %v", err)
		}
	})
}

// The verifier's state at the tip.
func (self *testAdmissionLog) sequencerState(t testing.TB) *sidecar.ConfigState {
	t.Helper()
	network := self.sequencer.network(self.networkId)
	network.stateLock.Lock()
	defer network.stateLock.Unlock()
	if err := network.loadWithLock(self.ctx, self.sequencer, nil); err != nil {
		t.Fatal(err)
	}
	return network.log.State()
}

// A row past its not_after and a revoked tombstone count for nothing; a parked
// row would still count, since a park is no removal.
func TestConfigAdmissionCountSkipsTombstonesAndLapsedRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestAdmissionLog(t)
		ephemeral := sidecar.NewId()
		notAfter := self.world.Clock().Now().Add(10 * time.Minute).UnixMilli()
		signed := self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
			Header:            self.world.Header(),
			NetworkId:         sidecar.Id(self.networkId).Bytes(),
			SidecarId:         ephemeral.Bytes(),
			IdentityPublicKey: testAdmissionIdentityKey(t),
			IdentityAlg:       "ed25519",
			MemberId:          sidecar.Id(self.ownerUserId).Bytes(),
			Ephemeral:         true,
			NotAfter:          notAfter,
		})
		if _, err := self.post(signed); err != nil {
			t.Fatalf("an ephemeral admission: %v", err)
		}
		state := self.sequencerState(t)
		connect.AssertEqual(t, ConfigAdmissionCount(state, notAfter-1), int64(2))
		connect.AssertEqual(t, ConfigAdmissionCount(state, notAfter), int64(1))

		removed := sidecar.NewId()
		if _, err := self.post(self.ownerDeviceAdmit(t, removed)); err != nil {
			t.Fatal(err)
		}
		if _, err := self.post(self.ownerRevoke(removed)); err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, ConfigAdmissionCount(self.sequencerState(t), notAfter-1), int64(2))
	})
}

// A completed proposal of a SidecarAdmit takes an admission where it lands,
// and a refused statement takes none.
func TestConfigStatementAdmitsReadsTheVerdict(t *testing.T) {
	admit := &protocol.SidecarAdmit{SidecarId: sidecar.NewId().Bytes()}
	admitBytes, _ := proto.Marshal(admit)
	proposal := &protocol.AuthorityProposal{StatementType: uint32(bridge.SignedType_SIGNED_SIDECAR_ADMIT), Statement: admitBytes}
	proposalBytes, _ := proto.Marshal(proposal)
	signedProposal, _ := proto.Marshal(&bridge.Signed{Type: uint32(bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL), Body: proposalBytes})
	other := &protocol.AuthorityProposal{StatementType: uint32(bridge.SignedType_SIGNED_MEMBER_ADMIT)}
	otherBytes, _ := proto.Marshal(other)
	signedOther, _ := proto.Marshal(&bridge.Signed{Type: uint32(bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL), Body: otherBytes})

	connect.AssertEqual(t, ConfigStatementAdmits(nil, nil, sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_SIDECAR_ADMIT}), true)
	connect.AssertEqual(t, ConfigStatementAdmits(nil, nil, sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_SIDECAR_ADMIT, Refusal: sidecar.ErrConfigRule}), false)
	connect.AssertEqual(t, ConfigStatementAdmits(nil, nil, sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_MEMBER_ADMIT}), false)
	completed := []byte{1}
	connect.AssertEqual(t, ConfigStatementAdmits(nil, signedProposal, sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL, CompletedProposalId: completed}), true)
	connect.AssertEqual(t, ConfigStatementAdmits(nil, signedProposal, sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL}), false)
	connect.AssertEqual(t, ConfigStatementAdmits(nil, signedOther, sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL, CompletedProposalId: completed}), false)
}

// An enrollment's admission is parsed for its shape alone: a Delegation's
// SidecarAdmit, refusing another type, a sidecar's signature and bytes that
// are no Signed; and it must fit its key's network, pre-approval and ephemeral
// flag.
func TestNetworkEnrollAdmissionShape(t *testing.T) {
	networkId := server.NewId()
	admit := &protocol.SidecarAdmit{NetworkId: sidecar.Id(networkId).Bytes(), SidecarId: sidecar.NewId().Bytes()}
	admitBytes, _ := proto.Marshal(admit)
	delegationId := sha256.Sum256([]byte("synthetic delegation"))
	keyId := sha256.Sum256([]byte("synthetic delegation key"))
	signed := func(signedType bridge.SignedType, signerId []byte, keyId []byte, body []byte) []byte {
		signedBytes, _ := proto.Marshal(&bridge.Signed{Type: uint32(signedType), SignerId: signerId, KeyId: keyId, Body: body})
		return signedBytes
	}

	admission, err := parseNetworkEnrollAdmission(signed(bridge.SignedType_SIGNED_SIDECAR_ADMIT, delegationId[:], keyId[:], admitBytes))
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, string(admission.delegationId), string(delegationId[:]))
	for _, refused := range [][]byte{
		[]byte("not a signed statement"),
		signed(bridge.SignedType_SIGNED_MEMBER_ADMIT, delegationId[:], keyId[:], admitBytes),
		signed(bridge.SignedType_SIGNED_SIDECAR_ADMIT, sidecar.NewId().Bytes(), nil, admitBytes),
	} {
		if _, err := parseNetworkEnrollAdmission(refused); err == nil {
			t.Errorf("an admission of the wrong shape parsed")
		}
	}

	fitting := &networkEnrollKey{networkId: networkId, preApproved: true}
	connect.AssertEqual(t, admission.fits(fitting), nil)
	for _, key := range []*networkEnrollKey{
		{networkId: server.NewId(), preApproved: true},
		{networkId: networkId},
		{networkId: networkId, preApproved: true, ephemeral: true},
	} {
		if admission.fits(key) == nil {
			t.Errorf("an admission fit a key it does not fit: %+v", key)
		}
	}
}
