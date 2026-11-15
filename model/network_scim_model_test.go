//go:build sidecar_pending

package model

// §7.6's SCIM queue (network_scim_model.go; TEAM-M6-07), which needs MG6: who reads GET /network/scim-changes, its
// pages, the tier, a one-command admin token read once, and the queue's 7 days, with the team tier stubbed on; and who
// reads GET /sidecar/scim-changes and what it answers (QD-829), judged by the rows of a network of the admission suite
// on server's own sequencer.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A team with an Owner and a Member, each with a credential, and three queued changes.
type scimChangesFixture struct {
	ctx           context.Context
	teamNetworkId server.Id
	owner         *session.ClientSession
	member        *session.ClientSession
	memberUserId  server.Id
	now           time.Time
	changes       *networkScimChanges
}

func newScimChangesFixture(t testing.TB) *scimChangesFixture {
	t.Helper()
	ctx := context.Background()
	_, restore := Testing_InMemoryNetworkNameSearch(ctx)
	t.Cleanup(restore)
	_, ownerUserId, _, personal := testTeamPersonalNetwork(t, ctx, "scim-queue-owner")
	created := createTestTeam(t, testNetworkTeams(server.NowUtc), personal, testTeamName("scim-queue-team"))
	self := &scimChangesFixture{ctx: ctx, teamNetworkId: *created.NetworkId, now: server.NowUtc()}
	ownerMemberId := *created.MemberId
	self.owner = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: ownerUserId, MemberId: &ownerMemberId})
	self.memberUserId = server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		member, err := AddNetworkMemberInTx(ctx, tx, &AddNetworkMemberArgs{NetworkId: self.teamNetworkId, UserId: self.memberUserId, Role: NetworkMemberRoleMember})
		server.Raise(err)
		memberId := member.MemberId
		self.member = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: self.memberUserId, MemberId: &memberId})
		for _, kind := range []NetworkScimChangeKind{NetworkScimChangeKindMemberRevoke, NetworkScimChangeKindUserCreate, NetworkScimChangeKindGroup} {
			_, err := AddNetworkScimChangeInTx(ctx, tx, self.teamNetworkId, nil, kind, map[string]string{"kind": string(kind)}, self.now)
			server.Raise(err)
		}
	})
	self.changes = &networkScimChanges{
		settings: DefaultNetworkScimSettings(),
		now:      func() time.Time { return self.now },
		tierOf:   func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierTeam },
	}
	return self
}

// The Owner reads the queue, a page at a time; a Member, a client token and an API key are refused; and a network
// outside the team tier reads nothing.
func TestScimChangesReadByOwnersAndAdmins(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimChangesFixture(t)
		result, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Changes), 3)
		connect.AssertEqual(t, result.More, false)

		self.changes.settings = &NetworkScimSettings{ChangeRetention: 7 * 24 * time.Hour, ChangeReadLimit: 2}
		first, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(first.Changes), 2)
		connect.AssertEqual(t, first.More, true)
		after := first.Changes[1].ChangeId
		second, err := self.changes.get(&GetNetworkScimChangesArgs{After: &after}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(second.Changes), 1)
		connect.AssertEqual(t, second.More, false)

		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.member); err == nil {
			t.Error("a Member read the queue")
		}
		clientId := server.NewId()
		client := session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.teamNetworkId, UserId: self.memberUserId, ClientId: &clientId})
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, client); err == nil {
			t.Error("a client token read the queue")
		}
		apiKey := session.Testing_CreateClientSession(self.ctx, self.owner.ByJwt)
		apiKey.ApiKeyAuthenticated = true
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, apiKey); err == nil {
			t.Error("an API key read the queue")
		}
		self.changes.tierOf = func(ctx context.Context, networkId server.Id) NetworkTier { return NetworkTierFree }
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner); err == nil {
			t.Error("a network outside the team tier read the queue")
		}
	})
}

// A one-command admin token with the all scope reads once: its replay answers the first read; a token without all is
// refused.
func TestScimChangesByAdminTokenOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimChangesFixture(t)
		tokenSession := func(scopes []string, jti string) *session.ClientSession {
			clientSession := session.Testing_CreateClientSession(self.ctx, self.owner.ByJwt)
			clientSession.UnsignedIdentity = true
			clientSession.AdminCommand = &session.AdminCommandUse{
				NetworkId: self.teamNetworkId, MemberId: *self.owner.ByJwt.MemberId, Jti: jti, Cmd: "scim-changes-test-cmd",
				Scopes: scopes, ExpireTime: self.now.Add(time.Hour),
			}
			return clientSession
		}
		if _, err := self.changes.get(&GetNetworkScimChangesArgs{}, tokenSession([]string{"auth_keys"}, server.NewId().String())); err == nil {
			t.Error("a token without all read the queue")
		}
		jti := server.NewId().String()
		first, err := self.changes.get(&GetNetworkScimChangesArgs{}, tokenSession([]string{"all"}, jti))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(first.Changes), 3)
		server.Tx(self.ctx, func(tx server.PgTx) {
			_, err := AddNetworkScimChangeInTx(self.ctx, tx, self.teamNetworkId, nil, NetworkScimChangeKindGroup, nil, self.now)
			server.Raise(err)
		})
		replay, err := self.changes.get(&GetNetworkScimChangesArgs{}, tokenSession([]string{"all"}, jti))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(replay.Changes), 3)
	})
}

// A change lasts ChangeRetention: past it reads leave it out, and the sweep deletes it.
func TestScimChangesExpireAtRetention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newScimChangesFixture(t)
		self.now = self.now.Add(DefaultNetworkScimSettings().ChangeRetention + time.Second)
		result, err := self.changes.get(&GetNetworkScimChangesArgs{}, self.owner)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(result.Changes), 0)
		removed := RemoveExpiredNetworkScimChanges(self.ctx, self.now.Add(-DefaultNetworkScimSettings().ChangeRetention))
		connect.AssertEqual(t, 3 <= removed, true)
	})
}

// A network of the admission suite on server's sequencer for GET /sidecar/scim-changes: members outside the AdminSet
// (two to remove, one removed already), an Admin, the indexer ServiceSettings.scim_signer names and another member's
// device, both the invited first devices of members outside the AdminSet; SCIM's queue holding a removal of each of
// them, of a member no row names, a second removal of the first member, a user_create and a group change; and a client
// of the indexer and one of the other device.
type sidecarScimFixture struct {
	t         testing.TB
	ctx       context.Context
	net       *sidecartest.AdmissionNet
	sequencer *ConfigLogSequencer
	networkId server.Id

	leaver      sidecar.Id
	second      sidecar.Id
	gone        sidecar.Id
	adminMember sidecar.Id
	stranger    sidecar.Id
	indexer     *sidecartest.AdmissionDevice
	other       *sidecartest.AdmissionDevice
	// the member_revoke changes the indexer reads, in change_id order
	listed []server.Id

	indexerClientId server.Id
	otherClientId   server.Id
}

func newSidecarScimFixture(t testing.TB) *sidecarScimFixture {
	t.Helper()
	self := &sidecarScimFixture{t: t, ctx: context.Background()}
	suiteSettings := sidecartest.DefaultAdmissionSuiteSettings()
	suiteSettings.ForgedAppends = false
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	tier := &sidecartest.AdmissionTier{
		Cluster:  cluster,
		Settings: suiteSettings,
		NewApi: func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
			settings := DefaultConfigLogSettings()
			settings.Clock = clock
			self.sequencer = NewConfigLogSequencer(cluster, settings, nil)
			return testConfigLogAdmissionApi{testConfigLogSuiteSequencer{sequencer: self.sequencer}}
		},
	}
	self.net = sidecartest.NewAdmissionNet(t, tier)
	t.Cleanup(self.net.Close)
	self.networkId = server.Id(self.net.NetworkId())
	owner := self.net.Owner()
	self.leaver, _ = self.net.ConsentedMember("scim route leaver")
	self.second, _ = self.net.ConsentedMember("scim route second")
	self.gone, _ = self.net.ConsentedMember("scim route gone")
	self.adminMember = self.net.AdmitMember(owner, "scim route admin", bridge.MemberRole_MEMBER_ROLE_MEMBER)
	self.net.MustLand(self.net.Sign(owner, bridge.SignedType_SIGNED_ADMIN_SET, self.net.AdminSet([]sidecar.Id{owner.MemberId}, []sidecar.Id{self.adminMember}, 0)))
	_, self.indexer = self.net.ConsentedMember("scim route indexer")
	_, self.other = self.net.ConsentedMember("scim route other")
	self.stranger = self.net.Id("a member no row names")
	self.net.SetServiceSettings(&protocol.ServiceSettings{ScimSigner: self.indexer.SidecarId.Bytes()})

	now := server.NowUtc()
	server.Tx(self.ctx, func(tx server.PgTx) {
		queue := func(memberId *sidecar.Id, kind NetworkScimChangeKind) server.Id {
			var queuedId *server.Id
			if memberId != nil {
				id := server.Id(*memberId)
				queuedId = &id
			}
			change, err := AddNetworkScimChangeInTx(self.ctx, tx, self.networkId, queuedId, kind, map[string]string{"email": "member@scim.example"}, now)
			server.Raise(err)
			return change.ChangeId
		}
		self.listed = append(self.listed, queue(&self.leaver, NetworkScimChangeKindMemberRevoke))
		queue(&self.adminMember, NetworkScimChangeKindMemberRevoke)
		queue(&self.gone, NetworkScimChangeKindMemberRevoke)
		queue(&self.stranger, NetworkScimChangeKindMemberRevoke)
		queue(nil, NetworkScimChangeKindUserCreate)
		self.listed = append(self.listed, queue(&self.second, NetworkScimChangeKindMemberRevoke))
		queue(nil, NetworkScimChangeKindGroup)
		self.listed = append(self.listed, queue(&self.leaver, NetworkScimChangeKindMemberRevoke))
	})
	// an admin device removed this one already: its queued change stays, and the rows no longer hold it
	self.net.MustLand(self.net.Sign(owner, bridge.SignedType_SIGNED_MEMBER_REVOKE, self.net.MemberRevoke(self.gone)))
	self.indexerClientId, self.otherClientId = server.NewId(), server.NewId()
	return self
}

// A session of clientId's token in the network.
func (self *sidecarScimFixture) client(clientId server.Id) *session.ClientSession {
	return session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.networkId, UserId: server.NewId(), ClientId: &clientId})
}

// The route as api serves it, but for the client lookup: the indexer's client and the other device's are bound, any
// other client is no sidecar's, and the rows are the sequencer's at its tip.
func (self *sidecarScimFixture) read(args *GetSidecarScimChangesArgs, clientSession *session.ClientSession) (*GetNetworkScimChangesResult, error) {
	return Testing_GetSidecarScimChanges(args, clientSession, server.NowUtc,
		func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool) {
			switch {
			case networkId != self.networkId:
				return server.Id{}, false
			case clientId == self.indexerClientId:
				return server.Id(self.indexer.SidecarId), true
			case clientId == self.otherClientId:
				return server.Id(self.other.SidecarId), true
			}
			return server.Id{}, false
		},
		func(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
			return self.sequencer.CaughtUpState(ctx, networkId)
		},
	)
}

// The change ids of a page.
func sidecarScimChangeIds(result *GetNetworkScimChangesResult) []server.Id {
	changeIds := []server.Id{}
	for _, change := range result.Changes {
		changeIds = append(changeIds, change.ChangeId)
	}
	return changeIds
}

// A refusal's status, by the "<code> message" convention the router answers.
func sidecarScimRefusalStatus(err error) string {
	if err == nil {
		return ""
	}
	status, _, _ := strings.Cut(err.Error(), " ")
	return status
}

// The designated indexer's client reads the queued removals of members outside the AdminSet the rows hold, every one
// of them, in change_id order and GET /network/scim-changes's form, a page at a time: not an Admin's, not the removal
// of a member already removed or of one no row names, and no user_create or group change.
func TestSidecarScimChangesAnswerTheScimSignerTheRemovalsOutsideTheAdminSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newSidecarScimFixture(t)
		result, err := self.read(&GetSidecarScimChangesArgs{}, self.client(self.indexerClientId))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(sidecarScimChangeIds(result), self.listed) || result.More {
			t.Fatalf("read %v, want %v", sidecarScimChangeIds(result), self.listed)
		}
		for _, change := range result.Changes {
			if change.Kind != NetworkScimChangeKindMemberRevoke || change.MemberId == nil || change.NetworkId != self.networkId ||
				!strings.Contains(string(change.Detail), "member@scim.example") {
				t.Errorf("a change not in the console's form: %+v", change)
			}
			if memberId := sidecar.Id(*change.MemberId); memberId != self.leaver && memberId != self.second {
				t.Errorf("listed the removal of %s", memberId)
			}
		}

		first, err := self.read(&GetSidecarScimChangesArgs{Limit: 2}, self.client(self.indexerClientId))
		if err != nil || !slices.Equal(sidecarScimChangeIds(first), self.listed[:2]) || !first.More {
			t.Fatalf("a first page of 2: %v %v", sidecarScimChangeIds(first), err)
		}
		after := first.Changes[1].ChangeId
		rest, err := self.read(&GetSidecarScimChangesArgs{After: &after, Limit: 2}, self.client(self.indexerClientId))
		if err != nil || !slices.Equal(sidecarScimChangeIds(rest), self.listed[2:]) || rest.More {
			t.Fatalf("the page after it: %v %v", sidecarScimChangeIds(rest), err)
		}

		// signed: the member leaves the rows, and its removals with it
		self.net.MustLand(self.net.Sign(self.indexer, bridge.SignedType_SIGNED_MEMBER_REVOKE, self.net.MemberRevoke(self.leaver)))
		result, err = self.read(&GetSidecarScimChangesArgs{}, self.client(self.indexerClientId))
		if err != nil || !slices.Equal(sidecarScimChangeIds(result), self.listed[1:2]) {
			t.Fatalf("after the indexer's MemberRevoke: %v %v", sidecarScimChangeIds(result), err)
		}
	})
}

// Every caller but the designated indexer's client is refused 403: another sidecar's client token, a client no
// sidecar binds, a network credential, an API key and an admin token; the indexer itself once it no longer stands
// (suspended) or the setting names another sidecar; and a log api cannot read answers 503, never the queue.
func TestSidecarScimChangesRefuseEveryOtherCaller(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newSidecarScimFixture(t)
		refused := func(label string, clientSession *session.ClientSession) {
			t.Helper()
			result, err := self.read(&GetSidecarScimChangesArgs{}, clientSession)
			if sidecarScimRefusalStatus(err) != "403" || result != nil {
				t.Errorf("%s: %v %v", label, result, err)
			}
		}
		refused("another sidecar's client", self.client(self.otherClientId))
		refused("a client no sidecar binds", self.client(server.NewId()))
		refused("the network's credential", session.Testing_CreateClientSession(self.ctx, &session.ByJwt{NetworkId: self.networkId, UserId: server.NewId()}))
		apiKey := self.client(self.indexerClientId)
		apiKey.ApiKeyAuthenticated = true
		refused("an API key", apiKey)
		adminToken := self.client(self.indexerClientId)
		adminToken.UnsignedIdentity = true
		refused("an unsigned identity", adminToken)

		unavailable, err := Testing_GetSidecarScimChanges(&GetSidecarScimChangesArgs{}, self.client(self.indexerClientId), server.NowUtc,
			func(ctx context.Context, networkId server.Id, clientId server.Id) (server.Id, bool) {
				return server.Id(self.indexer.SidecarId), true
			},
			func(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
				return nil, errors.New("the first metadata cluster is down")
			},
		)
		if sidecarScimRefusalStatus(err) != "503" || unavailable != nil || strings.Contains(err.Error(), "cluster") {
			t.Errorf("an unreadable log: %v %v", unavailable, err)
		}

		owner := self.net.Owner()
		suspendId := self.net.Id("scim route suspension")
		self.net.MustLand(self.net.Sign(owner, bridge.SignedType_SIGNED_SUSPEND, &bridge.Suspend{Header: self.net.Header(), Subject: self.indexer.SidecarId.Bytes(), SuspendId: suspendId.Bytes()}))
		refused("the suspended indexer", self.client(self.indexerClientId))
		self.net.MustLand(self.net.Sign(owner, bridge.SignedType_SIGNED_UNSUSPEND, &bridge.Unsuspend{Header: self.net.Header(), Subject: self.indexer.SidecarId.Bytes(), SuspendId: suspendId.Bytes()}))
		if _, err := self.read(&GetSidecarScimChangesArgs{}, self.client(self.indexerClientId)); err != nil {
			t.Fatalf("the unsuspended indexer: %v", err)
		}
		self.net.SetServiceSettings(&protocol.ServiceSettings{ScimSigner: self.other.SidecarId.Bytes()})
		refused("the indexer the setting no longer names", self.client(self.indexerClientId))
		if result, err := self.read(&GetSidecarScimChangesArgs{}, self.client(self.otherClientId)); err != nil || len(result.Changes) != len(self.listed) {
			t.Errorf("the newly named indexer: %v %v", result, err)
		}
	})
}

// The route judges by the store's tip, not by what this instance's verifier last saw: a second api instance whose
// verifier loaded earlier catches up to the entries the first landed since, the designation among them.
func TestSidecarScimChangesReadTheTipAnotherInstanceLanded(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newSidecarScimFixture(t)
		settings := DefaultConfigLogSettings()
		settings.Clock = self.net.Clock()
		second := NewConfigLogSequencer(self.sequencer.cluster, settings, nil)
		state, err := second.CaughtUpState(self.ctx, self.networkId)
		if err != nil || state == nil || state.Pin().GetVersion() != self.net.Tip().GetVersion() {
			t.Fatalf("the second instance's first read: %v", err)
		}
		self.net.SetServiceSettings(&protocol.ServiceSettings{ScimSigner: self.other.SidecarId.Bytes()})
		state, err = second.CaughtUpState(self.ctx, self.networkId)
		if err != nil {
			t.Fatal(err)
		}
		if state.Pin().GetVersion() != self.net.Tip().GetVersion() || !state.ScimSignerStanding(self.other.SidecarId, server.NowUtc()) ||
			state.ScimSignerStanding(self.indexer.SidecarId, server.NowUtc()) {
			t.Fatalf("the second instance stands at %d of %d", state.Pin().GetVersion(), self.net.Tip().GetVersion())
		}
		if empty, err := second.CaughtUpState(self.ctx, server.NewId()); err != nil || empty != nil {
			t.Errorf("a network with no log: %v %v", empty, err)
		}
	})
}
