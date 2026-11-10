//go:build sidecar_pending

package model

// §7.10's effects of landed entries at api (TEAM-M2-16, §22.7), on Postgres
// with MG1, T2's FakeBlobs for the log and the rev/ store: a removal's effects
// run once after a kill between the append and the effect, a statement refused
// at its position changes no row, the removed client's token is refused, and
// sponsor_user_id follows the tip's sponsor.

import (
	"context"
	"crypto/sha256"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A network whose owner's second device S is bound to a client C, admitted in
// the log at base 1's next position; the log's sequencer schedules no effects,
// as a process killed after its appends would leave them.
type testEffectsLog struct {
	*testAdmissionLog
	removal *testRemovalNetwork
	// the client's token, as its transports present it
	clientByJwt *session.ByJwt
}

func newTestEffectsLog(t testing.TB) *testEffectsLog {
	t.Helper()
	ctx := t.Context()
	removal := newTestRemovalNetwork(t, ctx, false)
	// the client's token, then its binding to the sidecar, as the removal
	// fixture binds one
	authResult, err := AuthNetworkClient(&AuthNetworkClientArgs{ClientId: &removal.clientId, Description: "synthetic", DeviceSpec: "synthetic"}, removal.networkSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		t.Fatalf("client token: %v %+v", err, authResult)
	}
	clientByJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	record := testSidecarBindingRecord(t, removal.networkId, removal.sidecarId, testSidecarKey(t), 1)
	record.ClientId = removal.clientId
	if outcome, _ := BindSidecarIdentity(ctx, record, server.NowUtc()); outcome != SidecarBindAppended {
		t.Fatalf("bind: %s", outcome)
	}
	cluster := testAdmissionFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorldFor(ctx, cluster, sidecar.Id(removal.networkId), sidecar.Id(removal.networkSession.ByJwt.UserId), sidecar.NewId())
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	if err := world.Start(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
		t.Fatal(err)
	}
	self := &testEffectsLog{
		testAdmissionLog: &testAdmissionLog{
			ctx:         ctx,
			networkId:   removal.networkId,
			ownerUserId: removal.networkSession.ByJwt.UserId,
			cluster:     cluster,
			world:       world,
			sequencer:   sequencer,
		},
		removal:     removal,
		clientByJwt: clientByJwt,
	}
	if _, err := self.post(self.ownerDeviceAdmit(t, sidecar.Id(removal.sidecarId))); err != nil {
		t.Fatalf("the device's admission: %v", err)
	}
	return self
}

// A sequencer of a fresh process over the same cluster: its verifier loads the
// log from the store, and it has applied no effects.
func (self *testEffectsLog) freshSequencer() *ConfigLogSequencer {
	settings := DefaultConfigLogSettings()
	settings.Clock = self.world.Clock()
	return NewConfigLogSequencer(self.cluster, settings, nil)
}

// Whether the client is active.
func testEffectsClientActive(t testing.TB, ctx context.Context, clientId server.Id) (active bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId).Scan(&active))
	})
	return
}

// The identity's status.
func testEffectsIdentityStatus(t testing.TB, ctx context.Context, sidecarId server.Id) (status string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT status FROM sidecar_identity WHERE sidecar_id = $1`, sidecarId).Scan(&status))
	})
	return
}

// The network's admin events whose action starts with config.
func testEffectsEvents(t testing.TB, ctx context.Context, networkId server.Id) []*NetworkAdminEvent {
	t.Helper()
	events := []*NetworkAdminEvent{}
	for _, event := range GetNetworkAdminEvents(ctx, networkId, 0, 1000) {
		if len(event.Action) > len(AdminEventActionConfigPrefix) && event.Action[:len(AdminEventActionConfigPrefix)] == AdminEventActionConfigPrefix {
			events = append(events, event)
		}
	}
	return events
}

// §22.7: a SidecarRevoke that landed with the process killed before its
// effects: a fresh process's run deactivates the bound client, revokes the
// identity, owes both rev/ objects and records one admin event naming the
// entry; a second run, and a third process's, change nothing more.
func TestRemovalEffectsRunOnceAfterAKill(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		s := installTestRevStore(t)
		self := newTestEffectsLog(t)
		sidecarId := self.removal.sidecarId
		pin, err := self.post(self.ownerRevoke(sidecar.Id(sidecarId)))
		if err != nil {
			t.Fatalf("the removal: %v", err)
		}
		// nothing ran: the kill came between the append and the effect
		connect.AssertEqual(t, testEffectsClientActive(t, self.ctx, self.removal.clientId), true)
		connect.AssertEqual(t, testEffectsIdentityStatus(t, self.ctx, sidecarId), SidecarIdentityStatusActive)

		sequencer := self.freshSequencer()
		result, err := ApplyConfigLogEffects(self.ctx, sequencer, self.networkId, DefaultConfigLogEffectsSettings())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.AppliedCount, 1)
		connect.AssertEqual(t, result.Through, pin.GetVersion())
		connect.AssertEqual(t, testEffectsClientActive(t, self.ctx, self.removal.clientId), false)
		connect.AssertEqual(t, testEffectsIdentityStatus(t, self.ctx, sidecarId), SidecarIdentityStatusRevoked)
		if s.clientRecord(t, self.removal) == nil {
			t.Fatal("the deactivated bound client owes no rev/ object")
		}
		if s.held(t, &SidecarRevocation{NetworkId: self.networkId, Kind: sidecar.RevocationKindSidecar, SubjectId: sidecarId}) == nil {
			t.Fatal("the revoked sidecar owes no rev/ object")
		}
		events := testEffectsEvents(t, self.ctx, self.networkId)
		connect.AssertEqual(t, len(events), 1)
		connect.AssertEqual(t, events[0].Action, AdminEventActionConfigPrefix+"sidecar_revoke")
		connect.AssertEqual(t, events[0].TargetKind, AdminEventTargetSidecar)
		connect.AssertEqual(t, *events[0].TargetId, sidecarId)
		connect.AssertEqual(t, events[0].ActorKind, AdminEventActorSidecar)
		connect.AssertEqual(t, *events[0].ActorId, server.Id(self.world.OwnerSidecarId()))
		var chain [32]byte
		copy(chain[:], pin.GetChain())
		connect.AssertEqual(t, *events[0].RequestId, configLogEffectRequestId(self.networkId, pin.GetVersion(), chain))

		again, err := ApplyConfigLogEffects(self.ctx, sequencer, self.networkId, DefaultConfigLogEffectsSettings())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.AppliedCount, 0)
		third, err := ApplyConfigLogEffects(self.ctx, self.freshSequencer(), self.networkId, DefaultConfigLogEffectsSettings())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, third.AppliedCount, 0)
		connect.AssertEqual(t, len(testEffectsEvents(t, self.ctx, self.networkId)), 1)
	})
}

// A statement refused at its own position, which only a misbehaving appender
// could have written there, changes no row: the client stays active and no
// event is recorded.
func TestARefusedStatementChangesNoRow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestEffectsLog(t)
		tip, err := self.sequencer.Tip(self.ctx, self.networkId)
		if err != nil {
			t.Fatal(err)
		}
		// a revocation the owner signed at a tip that names another chain, so
		// its at is off the log's chain and every verifier refuses it there
		header := self.world.Header()
		header.At = &bridge.AuthPin{Version: tip.GetVersion(), Chain: make([]byte, 32)}
		signed := self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
			Header:    header,
			SidecarId: sidecar.Id(self.removal.sidecarId).Bytes(),
			Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
		})
		entryBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.AuthEntry{
			NetworkId: sidecar.Id(self.networkId).Bytes(),
			N:         tip.GetVersion() + 1,
			PrevChain: tip.GetChain(),
			Kind:      bridge.AuthEntryKindStatement,
			TimeMs:    self.world.Clock().Now().UnixMilli(),
			Body:      signed,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := self.cluster.PutIfNoneMatch(self.ctx, sidecar.ConfigEntryKey(sidecar.Id(self.networkId), tip.GetVersion()+1), entryBytes, nil); err != nil {
			t.Fatal(err)
		}

		result, err := ApplyConfigLogEffects(self.ctx, self.freshSequencer(), self.networkId, DefaultConfigLogEffectsSettings())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.AppliedCount, 0)
		connect.AssertEqual(t, result.Through, tip.GetVersion()+1)
		connect.AssertEqual(t, testEffectsClientActive(t, self.ctx, self.removal.clientId), true)
		connect.AssertEqual(t, testEffectsIdentityStatus(t, self.ctx, self.removal.sidecarId), SidecarIdentityStatusActive)
		connect.AssertEqual(t, len(testEffectsEvents(t, self.ctx, self.networkId)), 0)
	})
}

// The removed client's token is refused once the effects commit, a definitive
// answer the client's connect transports meet at their next authorization
// recheck, within the 90 s lease: so connect closes them in <= 90 s (§7.10).
func TestRemovedClientsTokenIsRefusedWithinTheLease(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installTestRevStore(t)
		self := newTestEffectsLog(t)
		byJwt := self.clientByJwt
		connect.AssertEqual(t, session.ValidateByJwtState(self.ctx, byJwt, true), nil)
		if _, err := self.post(self.ownerRevoke(sidecar.Id(self.removal.sidecarId))); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyConfigLogEffects(self.ctx, self.freshSequencer(), self.networkId, DefaultConfigLogEffectsSettings()); err != nil {
			t.Fatal(err)
		}
		if session.ValidateByJwtState(self.ctx, byJwt, true) == nil {
			t.Fatal("the removed client's token still validates")
		}
		// the lease's recheck comes within it, and the lease is §7.10's bound
		connect.AssertEqual(t, session.AuthorizationRecheckInterval <= session.AuthorizationLeaseDuration, true)
		connect.AssertEqual(t, session.AuthorizationLeaseDuration <= 90*time.Second, true)
	})
}

// sponsor_user_id follows the tip's sponsor: a SponsorSet's member's user, and
// NULL once that member is gone; an untagged client's row is not touched.
func TestSponsorUserIdFollowsTheSponsor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		removal := newTestRemovalNetwork(t, ctx, true)
		creatorUserId := removal.networkSession.ByJwt.UserId
		writeOwner := func(clientId server.Id, tagged bool) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_client_owner (client_id, sponsor_user_id, tagged)
						VALUES ($1, $2, $3)
						ON CONFLICT (client_id) DO UPDATE SET sponsor_user_id = $2, tagged = $3
					`,
					clientId,
					creatorUserId,
					tagged,
				))
			})
		}
		sponsorOf := func(clientId server.Id) (sponsorUserId *server.Id) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT sponsor_user_id FROM network_client_owner WHERE client_id = $1`, clientId).Scan(&sponsorUserId))
			})
			return
		}
		writeOwner(removal.clientId, true)

		newSponsor := server.NewId()
		reconcile := func(sponsors map[server.Id]*server.Id, memberIds []server.Id) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.Raise(reconcileConfigSponsorsInTx(ctx, tx, removal.networkId, sponsors, memberIds))
			})
		}
		// a personal network's member is its user
		reconcile(map[server.Id]*server.Id{removal.sidecarId: &newSponsor}, []server.Id{newSponsor})
		connect.AssertEqual(t, *sponsorOf(removal.clientId), newSponsor)
		// the member gone: autogroup:admin stands in
		reconcile(map[server.Id]*server.Id{removal.sidecarId: nil}, nil)
		if sponsorOf(removal.clientId) != nil {
			t.Fatal("a sponsor whose member is gone stays")
		}

		// an untagged client keeps its row whatever the sponsors say
		writeOwner(removal.clientId, false)
		reconcile(map[server.Id]*server.Id{removal.sidecarId: &newSponsor}, []server.Id{newSponsor})
		connect.AssertEqual(t, *sponsorOf(removal.clientId), creatorUserId)
	})
}

// §7.8: an AdminSet change carried out from the log owes the network's Owners
// and Admins the network authority notice, and a second run owes nothing more.
func TestAnAdminSetChangeOwesItsNoticeOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestEffectsLog(t)
		ownerMemberId := self.world.OwnerMemberId()
		adminMemberId := sidecar.NewId()
		if _, err := self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
			Header: self.world.Header(), MemberId: adminMemberId.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER,
		})); err != nil {
			t.Fatalf("the member: %v", err)
		}
		if _, err := self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_ADMIN_SET, &protocol.AdminSet{
			Header: self.world.Header(), Owners: [][]byte{ownerMemberId.Bytes()}, Admins: [][]byte{adminMemberId.Bytes()},
		})); err != nil {
			t.Fatalf("the AdminSet: %v", err)
		}
		if _, err := ApplyConfigLogEffects(self.ctx, self.freshSequencer(), self.networkId, DefaultConfigLogEffectsSettings()); err != nil {
			t.Fatal(err)
		}
		notices := unsentTestNotices(t, self.ctx, self.networkId, SecurityNetworkAuthorityTemplateName)
		if len(notices) != 1 {
			t.Fatalf("%d notices, want one", len(notices))
		}
		if changes := notices[0].changes(t); len(changes) != 1 || changes[0]["action"] != SecurityNetworkAuthorityAdminSet {
			t.Fatalf("the notice: %s", notices[0].templateJson)
		}
		if _, err := ApplyConfigLogEffects(self.ctx, self.freshSequencer(), self.networkId, DefaultConfigLogEffectsSettings()); err != nil {
			t.Fatal(err)
		}
		again := unsentTestNotices(t, self.ctx, self.networkId, SecurityNetworkAuthorityTemplateName)
		if len(again) != 1 || again[0].templateJson != notices[0].templateJson {
			t.Fatalf("a second run owed more: %d notices", len(again))
		}
	})
}

// §7.8: an entry that declared a Delegation owes the notice when its effects
// are applied, and applying the entry again owes nothing more; the notice
// names neither the Delegation nor its key.
func TestANewDelegationOwesItsNoticeOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTestEffectsLog(t)
		effect := &configLogEntryEffect{
			n:               7,
			chain:           sha256.Sum256([]byte("synthetic entry")),
			timeMs:          self.world.Clock().Now().UnixMilli(),
			action:          AdminEventActionConfigPrefix + "delegation",
			delegationCount: 1,
		}
		connect.AssertEqual(t, effect.empty(), false)
		for i := range 2 {
			server.Tx(self.ctx, func(tx server.PgTx) {
				applied, err := applyConfigLogEntryEffectInTx(self.ctx, tx, self.networkId, effect)
				server.Raise(err)
				connect.AssertEqual(t, applied, i == 0)
			})
			notices := unsentTestNotices(t, self.ctx, self.networkId, SecurityNetworkAuthorityTemplateName)
			if len(notices) != 1 {
				t.Fatalf("%d notices, want one", len(notices))
			}
			changes := notices[0].changes(t)
			if len(changes) != 1 || changes[0]["action"] != SecurityNetworkAuthorityDelegation || len(changes[0]) != 2 {
				t.Fatalf("the notice: %s", notices[0].templateJson)
			}
		}
	})
}
