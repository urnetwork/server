//go:build sidecar_pending

package model

// A team's tagged or pre-approved auth key reaches what its creator's role allows at enrollment, not what it allowed
// when the key was made (QD-1199; §7.5, §7.8, D-TEAM-3, R-TEAM-6), on Postgres with MG0, MG1 and MG6 and T2's
// FakeBlobs for the team's log. The roles are the log's: a MemberAdmit and an AdminSet the team's Owner's first device
// signs make the key's creator an Admin, a later AdminSet demotes it to Member and another promotes it back, and the
// effects pass mirrors each into network_member as api's effects task does once an append lands (TEAM-M6-03). A
// demoted Admin's tagged and pre-approved keys answer 401 auth_key_invalid and spend, mint and post nothing; its
// untagged key still enrolls its own device; promoted back, its keys enroll again. A demotion that lands after the
// enrollment read the creator's sign-in, or inside the mint's transaction just before the spend, refuses the mint
// there.

import (
	"context"
	"crypto/sha256"
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// newTeamEnrollFixture's team, its creator an Admin by the team's log: an Owner whose first device signed the
// genesis, and the creator admitted and set Admin by statements that device signed, their effects applied.
type creatorRoleTeam struct {
	*teamEnrollFixture
	owner     *NetworkMember
	world     *sidecartest.ConfigLogSuiteWorld
	sequencer *ConfigLogSequencer
	// the admissions an enrollment posted, through the test's append
	appendedCount int
}

func newCreatorRoleTeam(t testing.TB) *creatorRoleTeam {
	t.Helper()
	fixture := newTeamEnrollFixture(t, NetworkMemberRoleAdmin)
	owner := addTestSignInMember(t, fixture.ctx, fixture.networkId, server.NewId(), NetworkMemberRoleOwner, false)
	cluster := testAdmissionFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorldFor(fixture.ctx, cluster, sidecar.Id(fixture.networkId), sidecar.Id(owner.MemberId), sidecar.NewId())
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	if err := world.Start(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
		t.Fatal(err)
	}
	self := &creatorRoleTeam{
		teamEnrollFixture: fixture,
		owner:             owner,
		world:             world,
		sequencer:         sequencer,
	}
	// an admission's append stands in for the log's verdict on it, which these
	// tests do not reach: only whether the enrollment posted one
	t.Cleanup(Testing_SetNetworkEnrollAppendAdmission(func(ctx context.Context, networkId server.Id, signedBytes []byte) (*bridge.AuthPin, error) {
		self.appendedCount += 1
		return &bridge.AuthPin{Version: uint64(1 + self.appendedCount), Chain: make([]byte, sha256.Size)}, nil
	}))
	self.land(t, bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		Header:   world.Header(),
		MemberId: sidecar.Id(fixture.creator.MemberId).Bytes(),
		Role:     bridge.MemberRole_MEMBER_ROLE_MEMBER,
	})
	self.setCreatorAdmin(t, true)
	return self
}

// Lands a statement of the Owner's first device in the team's log and applies its effects, which mirror the roles it
// set into network_member.
func (self *creatorRoleTeam) land(t testing.TB, signedType bridge.SignedType, body proto.Message) {
	t.Helper()
	pin, err := self.sequencer.AppendStatement(self.ctx, self.networkId, self.world.SignAsOwner(signedType, body))
	if err != nil {
		t.Fatalf("the %s: %v", signedType, err)
	}
	self.world.SetTip(pin)
	if _, err := ApplyConfigLogEffects(self.ctx, self.sequencer, self.networkId, DefaultConfigLogEffectsSettings()); err != nil {
		t.Fatalf("the %s's effects: %v", signedType, err)
	}
}

// Lands the Owner's AdminSet naming the Owner and, when admin, the creator an Admin; one that leaves the creator out
// demotes it to Member (§7.9's AdminSet). The creator's role network_member mirrors then follows.
func (self *creatorRoleTeam) setCreatorAdmin(t testing.TB, admin bool) {
	t.Helper()
	adminSet := &protocol.AdminSet{
		Header: self.world.Header(),
		Owners: [][]byte{sidecar.Id(self.owner.MemberId).Bytes()},
	}
	want := NetworkMemberRoleMember
	if admin {
		adminSet.Admins = [][]byte{sidecar.Id(self.creator.MemberId).Bytes()}
		want = NetworkMemberRoleAdmin
	}
	self.land(t, bridge.SignedType_SIGNED_ADMIN_SET, adminSet)
	if role, ok := MemberRole(self.ctx, self.networkId, self.creator.MemberId); !ok || role != want {
		t.Fatalf("the creator's mirrored role is %q (%t), want %q", role, ok, want)
	}
}

// A pre-approved key's admission as its device signs it with the key's Delegation: a Signed SidecarAdmit into the
// team, of the shape the enrollment parses, its verdict the test's append's.
func newCreatorRoleAdmission(t testing.TB, networkId server.Id) []byte {
	t.Helper()
	admitBytes, err := proto.Marshal(&protocol.SidecarAdmit{
		NetworkId: sidecar.Id(networkId).Bytes(),
		SidecarId: sidecar.NewId().Bytes(),
	})
	if err != nil {
		t.Fatal(err)
	}
	delegationId := sha256.Sum256([]byte("synthetic delegation"))
	keyId := sha256.Sum256([]byte("synthetic delegation key"))
	signedBytes, err := proto.Marshal(&bridge.Signed{
		Type:     uint32(bridge.SignedType_SIGNED_SIDECAR_ADMIT),
		SignerId: delegationId[:],
		KeyId:    keyId[:],
		Body:     admitBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return signedBytes
}

// Enrolls a device from the bearer, with admission when it is not nil.
func (self *creatorRoleTeam) enroll(t testing.TB, bearer string, admission []byte) (*NetworkEnrollResult, error) {
	t.Helper()
	args := teamEnrollArgs(t, bearer)
	args.Admission = admission
	return EnrollNetworkClient(self.ctx, args, "192.0.2.9:1")
}

// Only a role that holds the keys permission, an Owner's or an Admin's, still holds a tagged or pre-approved key at
// enrollment, and a role this build does not know holds none; every role still holds an untagged key, ephemeral or
// not. A key needs the permission when it is tagged or pre-approved.
func TestEnrollmentAsksTheCreatorsRoleForTheKeysPermission(t *testing.T) {
	roles := map[NetworkMemberRole]bool{
		NetworkMemberRoleOwner:                    true,
		NetworkMemberRoleAdmin:                    true,
		NetworkMemberRoleNetworkAdmin:             false,
		NetworkMemberRoleItAdmin:                  false,
		NetworkMemberRoleBillingAdmin:             false,
		NetworkMemberRoleAuditor:                  false,
		NetworkMemberRoleMember:                   false,
		NetworkMemberRole("synthetic-later-role"): false,
		NetworkMemberRole(""):                     false,
	}
	for role, keeps := range roles {
		if networkEnrollCreatorRolePermits(true, role) != keeps {
			t.Errorf("a %q creator keeps its tagged or pre-approved key: %t, want %t", role, !keeps, keeps)
		}
		if !networkEnrollCreatorRolePermits(false, role) {
			t.Errorf("a %q creator lost its untagged key", role)
		}
	}

	keys := []struct {
		key   *networkEnrollKey
		needs bool
	}{
		{key: &networkEnrollKey{}, needs: false},
		{key: &networkEnrollKey{ephemeral: true}, needs: false},
		{key: &networkEnrollKey{tagged: true}, needs: true},
		{key: &networkEnrollKey{preApproved: true}, needs: true},
		{key: &networkEnrollKey{preApproved: true, ephemeral: true}, needs: true},
		{key: &networkEnrollKey{tagged: true, preApproved: true, ephemeral: true}, needs: true},
	}
	for _, c := range keys {
		if c.key.needsKeysPermission() != c.needs {
			t.Errorf("a key %+v needs the keys permission: %t, want %t", c.key, !c.needs, c.needs)
		}
	}
}

// An Admin's tagged key enrolls a tagged client; once a signed AdminSet demotes the Admin to Member, the key answers
// 401 auth_key_invalid, spending nothing and minting nothing; once the next AdminSet promotes it back, the key enrolls
// again, its client on the team's own identity as before.
func TestADemotedAdminsTaggedKeyIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})

		first, err := self.enroll(t, bearer, nil)
		if err != nil {
			t.Fatalf("the Admin's tagged enrollment: %v", err)
		}
		firstJwt := requireTeamEnrollCredential(t, self.ctx, first)
		connect.AssertEqual(t, firstJwt.UserId, self.ownerOfRecordId)
		connect.AssertEqual(t, readTeamEnrollOwnerRow(t, self.ctx, first.ClientId).tagged, true)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 1)
		clientCount := countTeamEnrollClients(t, self.ctx, self.networkId)

		self.setCreatorAdmin(t, false)
		_, err = self.enroll(t, bearer, nil)
		requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 1)
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), clientCount)
		// the client the key minted while its creator was an Admin stays: the
		// network's, as past its sponsor's removal (owner decision 83)
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, first.ClientId), true)

		self.setCreatorAdmin(t, true)
		again, err := self.enroll(t, bearer, nil)
		if err != nil {
			t.Fatalf("the promoted Admin's tagged enrollment: %v", err)
		}
		againJwt := requireTeamEnrollCredential(t, self.ctx, again)
		connect.AssertEqual(t, againJwt.UserId, self.ownerOfRecordId)
		connect.AssertEqual(t, againJwt.MemberId == nil, true)
		row := readTeamEnrollOwnerRow(t, self.ctx, again.ClientId)
		connect.AssertEqual(t, row.tagged, true)
		connect.AssertEqual(t, row.sponsorUserId != nil && *row.sponsorUserId == self.creatorUserId, true)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 2)
	})
}

// An Admin's pre-approved key enrolls with its device's admission, which it posts, and answers approved; once a
// signed AdminSet demotes the Admin to Member, the key answers 401 auth_key_invalid with its admission or without one,
// posting no admission, spending nothing and minting nothing; promoted back, it enrolls and posts again.
func TestADemotedAdminsPreApprovedKeyIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{PreApproved: true})

		first, err := self.enroll(t, bearer, newCreatorRoleAdmission(t, self.networkId))
		if err != nil {
			t.Fatalf("the Admin's pre-approved enrollment: %v", err)
		}
		connect.AssertEqual(t, first.ApprovalState, NetworkEnrollApprovalStateApproved)
		connect.AssertEqual(t, self.appendedCount, 1)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 1)
		clientCount := countTeamEnrollClients(t, self.ctx, self.networkId)

		self.setCreatorAdmin(t, false)
		for _, admission := range [][]byte{newCreatorRoleAdmission(t, self.networkId), nil} {
			_, err := self.enroll(t, bearer, admission)
			requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		}
		connect.AssertEqual(t, self.appendedCount, 1)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 1)
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), clientCount)

		self.setCreatorAdmin(t, true)
		again, err := self.enroll(t, bearer, newCreatorRoleAdmission(t, self.networkId))
		if err != nil {
			t.Fatalf("the promoted Admin's pre-approved enrollment: %v", err)
		}
		connect.AssertEqual(t, again.ApprovalState, NetworkEnrollApprovalStateApproved)
		connect.AssertEqual(t, self.appendedCount, 2)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 2)
	})
}

// An untagged key, which every role makes for its own devices, keeps enrolling past its creator's demotion: the
// device is the demoted member's own, its owner row and its token naming the membership, untagged and unsponsored.
// An untagged ephemeral key the same.
func TestADemotedAdminsUntaggedKeyStillEnrollsItsOwnDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{})
		ephemeralBearer, ephemeralKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Ephemeral: true})

		self.setCreatorAdmin(t, false)
		for _, c := range []struct {
			bearer    string
			authKeyId server.Id
			ephemeral bool
		}{
			{bearer: bearer, authKeyId: authKeyId, ephemeral: false},
			{bearer: ephemeralBearer, authKeyId: ephemeralKeyId, ephemeral: true},
		} {
			result, err := self.enroll(t, c.bearer, nil)
			if err != nil {
				t.Fatalf("the demoted member's untagged enrollment (ephemeral %t): %v", c.ephemeral, err)
			}
			byJwt := requireTeamEnrollCredential(t, self.ctx, result)
			if byJwt.MemberId == nil || *byJwt.MemberId != self.creator.MemberId {
				t.Fatalf("the own device's credential names member %v, not the creator's %s", byJwt.MemberId, self.creator.MemberId)
			}
			row := readTeamEnrollOwnerRow(t, self.ctx, result.ClientId)
			connect.AssertEqual(t, row.tagged, false)
			connect.AssertEqual(t, row.ephemeral, c.ephemeral)
			connect.AssertEqual(t, row.ownerUserId != nil && *row.ownerUserId == self.creatorUserId, true)
			connect.AssertEqual(t, row.memberId != nil && *row.memberId == self.creator.MemberId, true)
			connect.AssertEqual(t, row.sponsorUserId == nil, true)
			connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, c.authKeyId), 1)
		}
	})
}

// A demotion that lands after the enrollment read the creator's sign-in, its role still Admin's there, is met in the
// mint's transaction, which reads the role under the membership's lock before the spend: the tagged key answers 401
// auth_key_invalid, and nothing is spent or minted.
func TestADemotionAfterTheSignInRefusesTheMint(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})

		demoted := false
		t.Cleanup(func() { Testing_EnrollAfterSignIn = nil })
		Testing_EnrollAfterSignIn = func() {
			if !demoted {
				demoted = true
				self.setCreatorAdmin(t, false)
			}
		}
		_, err := self.enroll(t, bearer, nil)
		requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		connect.AssertEqual(t, demoted, true)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 0)
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), 0)
	})
}

// The spend's linearization point: a demotion that commits inside the mint's transaction, at the barrier just before
// the spend, is met by the role read there, which locks the membership and reads its committed role, so the tagged
// key answers 401 auth_key_invalid, spending nothing and minting nothing. Promoted back, the same barrier passes and
// the key mints.
func TestADemotionInsideTheMintRefusesTheSpend(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newCreatorRoleTeam(t)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{teamEnrollTagHex}})

		barrierCount := 0
		demote := true
		t.Cleanup(func() { Testing_EnrollBeforeConsume = nil })
		Testing_EnrollBeforeConsume = func() {
			barrierCount += 1
			if demote {
				demote = false
				self.setCreatorAdmin(t, false)
			}
		}
		_, err := self.enroll(t, bearer, nil)
		requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		if barrierCount < 1 || demote {
			t.Fatalf("the barrier ran %d times, demoting %t", barrierCount, !demote)
		}
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 0)
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), 0)

		self.setCreatorAdmin(t, true)
		result, err := self.enroll(t, bearer, nil)
		if err != nil {
			t.Fatalf("the promoted Admin's tagged enrollment past the barrier: %v", err)
		}
		requireTeamEnrollCredential(t, self.ctx, result)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 1)
	})
}
