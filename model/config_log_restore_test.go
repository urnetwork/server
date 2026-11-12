//go:build sidecar_pending

package model

// §19.10's Postgres restore steps at api (config_log_restore.go; TEAM-M3-02), which need MG2's network_acl_head and
// MG1's identity tables: a restored head advances to the store's verified tip and never across a log that does not pass
// through it; the effects walk again from the start; and re-seed mode's admins are the restored rows' Owners'.

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// A restored head below the store's tip advances to the verified tip; a head that names another A_n at a retained
// position is reported and left where it is.
func TestAdvanceAclHeadsAfterRestore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		self := newReseedWorld(t, true)
		connect.AssertEqual(t, func() error {
			_, err := AdvanceNetworkAclHead(ctx, self.networkId, 1, self.chainAt(t, 1), false)
			return err
		}(), nil)

		offChain := newReseedWorld(t, false)
		other := bytes.Repeat([]byte{7}, 32)
		connect.AssertEqual(t, func() error { _, err := AdvanceNetworkAclHead(ctx, offChain.networkId, 2, other, false); return err }(), nil)

		// both networks' logs on one sequencer's store: a restore reads what the cluster holds
		outcomes, err := AdvanceAclHeadsAfterRestore(ctx, self.sequencer)
		connect.AssertEqual(t, err, nil)
		found := false
		for _, outcome := range outcomes {
			if outcome.NetworkId != self.networkId {
				continue
			}
			found = true
			connect.AssertEqual(t, outcome.Err, nil)
			connect.AssertEqual(t, outcome.From, uint64(1))
			connect.AssertEqual(t, outcome.To, self.tip.GetVersion())
		}
		connect.AssertEqual(t, found, true)
		head, err := GetNetworkAclHead(ctx, self.networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, head.Version, self.tip.GetVersion())
		connect.AssertEqual(t, bytes.Equal(head.Chain, self.tip.GetChain()), true)

		outcomes, err = AdvanceAclHeadsAfterRestore(ctx, offChain.sequencer)
		connect.AssertEqual(t, err, nil)
		for _, outcome := range outcomes {
			if outcome.NetworkId == offChain.networkId && !errors.Is(outcome.Err, ErrConfigLogRestoreOffChain) {
				t.Fatalf("an off-chain head: %+v", outcome)
			}
		}
		head, err = GetNetworkAclHead(ctx, offChain.networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, head.Version, uint64(2))
	})
}

// The chain value the world's log holds at n.
func (self *reseedWorld) chainAt(t testing.TB, n uint64) []byte {
	t.Helper()
	read, err := self.sequencer.Read(self.ctx, self.networkId, n-1)
	if err != nil || len(read.Entries) == 0 {
		t.Fatalf("no entry at %d: %v", n, err)
	}
	entry := &bridge.AuthEntry{}
	if err := proto.Unmarshal(read.Entries[0], entry); err != nil {
		t.Fatal(err)
	}
	chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
	if err != nil {
		t.Fatal(err)
	}
	return chain[:]
}

// Re-seed mode's admins: a personal network's owner's bound sidecar is one, with the owner's user as its member; a
// sidecar of a client another user owns is not, and neither is an unknown one.
func TestConfigReseedAdminSidecar(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner := newClientOwnerFixture(t)
		owner.pushPreview(t)
		sidecarClient := owner.mint(t, owner.root, &AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "test", StrictExp: true})
		sidecarId := server.NewId()
		identityKey, _, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		record := testSidecarBindingRecord(t, owner.networkId, sidecarId, identityKey, 1)
		record.ClientId = *sidecarClient.ClientId
		if outcome, _ := BindSidecarIdentity(owner.ctx, record, server.NowUtc()); outcome != SidecarBindAppended {
			t.Fatalf("the binding: %s", outcome)
		}
		memberId, admin, err := ConfigReseedAdminSidecar(owner.ctx, owner.networkId, sidecarId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, admin, true)
		connect.AssertEqual(t, *memberId, owner.userId)

		_, admin, err = ConfigReseedAdminSidecar(owner.ctx, owner.networkId, server.NewId())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, admin, false)
		_, admin, err = ConfigReseedAdminSidecar(owner.ctx, server.NewId(), sidecarId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, admin, false)
	})
}

// The rev step owes a rev/ object for a client deactivated within rev/'s DeleteAfter and none for one deactivated
// before it.
func TestRecreateRecentRevocations(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		owner := newClientOwnerFixture(t)
		recent := owner.mint(t, owner.root, &AuthNetworkClientArgs{Description: "recent", DeviceSpec: "test"})
		old := owner.mint(t, owner.root, &AuthNetworkClientArgs{Description: "old", DeviceSpec: "test"})
		now := server.NowUtc()
		settings := DefaultSidecarRevSettings()
		server.Tx(owner.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(owner.ctx, `UPDATE network_client SET active = false, deactivate_time = $2 WHERE client_id = $1`, *recent.ClientId, now.Add(-5*time.Minute)))
			server.RaisePgResult(tx.Exec(owner.ctx, `UPDATE network_client SET active = false, deactivate_time = $2 WHERE client_id = $1`, *old.ClientId, now.Add(-settings.DeleteAfter-time.Minute)))
		})
		count, err := RecreateRecentRevocations(owner.ctx, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, count, 1)
	})
}

// The effects step after a Postgres restore whose backup predates a SidecarRevoke's effects: this process's watermark
// says it applied the entry, so an ordinary run applies nothing, but the walk from the start applies it again, and a
// second walk finds nothing left.
func TestReapplyRemovalEffectsAfterARestore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installTestRevStore(t)
		self := newTestEffectsLog(t)
		sidecarId := self.removal.sidecarId
		pin, err := self.post(self.ownerRevoke(sidecar.Id(sidecarId)))
		if err != nil {
			t.Fatalf("the removal: %v", err)
		}
		settings := DefaultConfigLogEffectsSettings()
		first, err := ApplyConfigLogEffects(self.ctx, self.sequencer, self.networkId, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, first.AppliedCount, 1)

		// the restored backup: the client and the identity active, and no event naming the entry
		var chain [32]byte
		copy(chain[:], pin.GetChain())
		server.Tx(self.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(self.ctx, `UPDATE network_client SET active = true, deactivate_time = NULL WHERE client_id = $1`, self.removal.clientId))
			server.RaisePgResult(tx.Exec(self.ctx, `UPDATE sidecar_identity SET status = $2, revoke_time = NULL WHERE sidecar_id = $1`, sidecarId, SidecarIdentityStatusActive))
			server.RaisePgResult(tx.Exec(self.ctx, `DELETE FROM network_admin_event WHERE network_id = $1 AND request_id = $2`, self.networkId, configLogEffectRequestId(self.networkId, pin.GetVersion(), chain)))
		})
		ordinary, err := ApplyConfigLogEffects(self.ctx, self.sequencer, self.networkId, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ordinary.AppliedCount, 0)
		connect.AssertEqual(t, testEffectsClientActive(t, self.ctx, self.removal.clientId), true)

		count, err := ReapplyRemovalEffects(self.ctx, self.sequencer, self.networkId, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, count, 1)
		connect.AssertEqual(t, testEffectsClientActive(t, self.ctx, self.removal.clientId), false)
		connect.AssertEqual(t, testEffectsIdentityStatus(t, self.ctx, sidecarId), SidecarIdentityStatusRevoked)
		count, err = ReapplyRemovalEffects(self.ctx, self.sequencer, self.networkId, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, count, 0)
	})
}
