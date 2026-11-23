//go:build sidecar_pending

package model

// §7.5's auth key reach past the keys permission (R-TEAM-2, D-TEAM-3; QD-1199), on Postgres with MG0, MG1 and MG6.
//
// Keys for owned tags, a Network admin's: on T2's FakeBlobs for the team's log, whose roles are the log's (the Owner's
// first device signs a MemberAdmit and an AdminSet, the effects pass mirroring each into network_member) and whose tag
// owners are its newest base's rows, which these tests fold as a saved document's tagOwners compile them (§8.8's
// atoms). The creator makes a tagged key, pre-approved or not, for a tag it owns and for no other, and the key enrolls
// a tagged client while the creator still owns the tag and holds the role, each asked again at enrollment. A Member
// named in tagOwners makes none, and a log that cannot say who owns a tag refuses.
//
// Listing and revoking: every member lists and revokes the keys it created and sees no one else's, an Owner and an
// Admin every key.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// newCreatorRoleTeam's team with its log installed as this process's, so the auth key model reads its tag owners
// (configTipState), its creator a Network admin by the Owner's AdminSet, and two tags: ownedTagId, whose tagOwners
// name the creator, and otherTagId, whose name the Owner alone.
type tagOwnerTeam struct {
	*creatorRoleTeam
	ownedTagId sidecar.Id
	otherTagId sidecar.Id
}

func newTagOwnerTeam(t testing.TB) *tagOwnerTeam {
	t.Helper()
	team := newCreatorRoleTeam(t)
	SetConfigLogSequencer(team.sequencer)
	t.Cleanup(func() { SetConfigLogSequencer(nil) })
	self := &tagOwnerTeam{
		creatorRoleTeam: team,
		ownedTagId:      sidecar.NewId(),
		otherTagId:      sidecar.NewId(),
	}
	self.setCreatorRole(t, NetworkMemberRoleNetworkAdmin)
	self.foldTagOwners(t, map[sidecar.Id][]server.Id{
		self.ownedTagId: {self.creator.MemberId},
		self.otherTagId: {self.owner.MemberId},
	})
	return self
}

// Lands the Owner's AdminSet naming the Owner and, for role Network admin, the creator one; for role Member, no one
// else, which drops the creator to Member (§7.9's AdminSet). The creator's role network_member mirrors then follows.
func (self *tagOwnerTeam) setCreatorRole(t testing.TB, role NetworkMemberRole) {
	t.Helper()
	adminSet := &protocol.AdminSet{
		Header: self.world.Header(),
		Owners: [][]byte{sidecar.Id(self.owner.MemberId).Bytes()},
	}
	switch role {
	case NetworkMemberRoleNetworkAdmin:
		adminSet.NetworkAdmins = [][]byte{sidecar.Id(self.creator.MemberId).Bytes()}
	case NetworkMemberRoleMember:
	default:
		t.Fatalf("no AdminSet here sets a %s", role)
	}
	self.land(t, bridge.SignedType_SIGNED_ADMIN_SET, adminSet)
	if mirrored, ok := MemberRole(self.ctx, self.networkId, self.creator.MemberId); !ok || mirrored != role {
		t.Fatalf("the creator's mirrored role is %q (%t), want %q", mirrored, ok, role)
	}
}

// Folds a base through the tip, signed by the Owner's first device, whose rows are the tip state's with each tag of
// tagOwners holding exactly the members it names as its owners, sorted, as compile writes a tag atom's
// tag_owner_member_ids (§8.8's atoms); its effects are applied as a statement's are.
func (self *tagOwnerTeam) foldTagOwners(t testing.TB, tagOwners map[sidecar.Id][]server.Id) {
	t.Helper()
	state, err := self.sequencer.CaughtUpState(self.ctx, self.networkId)
	if err != nil || state == nil {
		t.Fatalf("the tip state: %v", err)
	}
	previous := state.Base()
	tip := state.Pin()
	rows := state.Rows()
	acl := &protocol.AclRows{}
	if err := proto.Unmarshal(rows.GetAcl(), acl); err != nil {
		t.Fatal(err)
	}
	acl.Atoms = slices.DeleteFunc(acl.Atoms, func(atom *protocol.AclAtom) bool {
		for tagId := range tagOwners {
			if bytes.Equal(atom.GetAtom(), tagId.Bytes()) {
				return true
			}
		}
		return false
	})
	for tagId, ownerIds := range tagOwners {
		atom := &protocol.AclAtom{Atom: tagId.Bytes()}
		for _, ownerId := range ownerIds {
			atom.TagOwnerMemberIds = append(atom.TagOwnerMemberIds, sidecar.Id(ownerId).Bytes())
		}
		slices.SortFunc(atom.TagOwnerMemberIds, bytes.Compare)
		acl.Atoms = append(acl.Atoms, atom)
	}
	slices.SortFunc(acl.Atoms, func(a *protocol.AclAtom, b *protocol.AclAtom) int {
		return bytes.Compare(a.GetAtom(), b.GetAtom())
	})
	aclBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(acl)
	if err != nil {
		t.Fatal(err)
	}
	rows.Acl = aclBytes
	rowsBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	rowsSha256 := sha256.Sum256(rowsBytes)
	signedHeader := self.world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, &bridge.ProjectionHeader{
		NetworkId:        sidecar.Id(self.networkId).Bytes(),
		Version:          tip.GetVersion() + 1,
		PrevChain:        tip.GetChain(),
		FoldedThrough:    tip.GetVersion(),
		ConfigVersion:    previous.GetConfigVersion(),
		ConfigHeadSha256: previous.GetConfigHeadSha256(),
		RowsSha256:       rowsSha256[:],
		RowsByteCount:    uint32(len(rowsBytes)),
		Threshold:        previous.GetThreshold(),
	})
	pin, err := self.sequencer.AppendBase(self.ctx, self.networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rowsBytes})
	if err != nil {
		t.Fatalf("the base with the tag owners: %v", err)
	}
	self.world.SetTip(pin)
	if _, err := ApplyConfigLogEffects(self.ctx, self.sequencer, self.networkId, DefaultConfigLogEffectsSettings()); err != nil {
		t.Fatalf("the base's effects: %v", err)
	}
}

// A tag id as POST /network/auth-keys takes it.
func testAuthKeyTagHex(tagId sidecar.Id) string {
	return hex.EncodeToString(tagId.Bytes())
}

// Creates a key with the creator's session, an hour and ten uses unless args say otherwise.
func (self *tagOwnerTeam) createKey(args *CreateAuthKeyArgs) (*CreateAuthKeyResult, error) {
	if args.DurationSeconds == 0 {
		args.DurationSeconds = 3600
	}
	if args.MaxUses == 0 {
		args.MaxUses = 10
	}
	return CreateAuthKey(args, self.creatorSession)
}

// A Network admin makes a tagged key for the tag the network's tagOwners name it an owner of, pre-approved or not,
// ephemeral or not, and keys for its own devices; it makes none for a tag only another member owns, for that tag
// beside its own, for a tag no row holds, nor a pre-approved key with no tag, each refused 403 with nothing created.
// The keys it made carry only its tag.
func TestANetworkAdminMakesTaggedKeysOnlyForTheTagsItOwns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTagOwnerTeam(t)
		owned, other := testAuthKeyTagHex(self.ownedTagId), testAuthKeyTagHex(self.otherTagId)
		if reach := authKeyReachOf(self.creatorSession); reach != authKeyReachOwnedTags {
			t.Fatalf("a Network admin's reach is %d, want owned tags", reach)
		}

		made := []*CreateAuthKeyArgs{
			{Tags: []string{owned}},
			{Tags: []string{owned}, PreApproved: true},
			{Tags: []string{owned}, Ephemeral: true, PreApproved: true},
			{},
			{Ephemeral: true},
		}
		for _, args := range made {
			result, err := self.createKey(args)
			if err != nil || result.Error != nil || result.AuthKeyId == nil {
				t.Fatalf("the Network admin's key %+v: %v %+v", args, err, result)
			}
		}
		for _, args := range []*CreateAuthKeyArgs{
			{Tags: []string{other}},
			{Tags: []string{owned, other}},
			{Tags: []string{other}, PreApproved: true},
			{Tags: []string{testAuthKeyTagHex(sidecar.NewId())}},
			{PreApproved: true},
		} {
			result, err := self.createKey(args)
			if !authKeyRoleRefusal(err, authKeyOwnedTagsRefusedMessage) || result != nil {
				t.Errorf("the Network admin's key %+v answered %v %+v, not the owned tags refusal", args, err, result)
			}
		}

		listed, err := GetNetworkAuthKeys(self.creatorSession)
		if err != nil || len(listed.AuthKeys) != len(made) {
			t.Fatalf("the Network admin lists %+v (%v), want its %d keys", listed, err, len(made))
		}
		for _, authKey := range listed.AuthKeys {
			if authKey.Creator != self.creatorUserId || 1 < len(authKey.Tags) || len(authKey.Tags) == 1 && authKey.Tags[0] != owned {
				t.Errorf("a key the Network admin made: %+v", authKey)
			}
		}
		connect.AssertEqual(t, countTestAuthKeysOf(t, self.ctx, self.networkId, self.creatorUserId), len(made))

		// owning every tag of none, and at no state, owns nothing
		state, err := self.sequencer.CaughtUpState(self.ctx, self.networkId)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, configStateOwnsTags(state, self.creator.MemberId, [][]byte{self.ownedTagId.Bytes()}), true)
		connect.AssertEqual(t, configStateOwnsTags(state, self.creator.MemberId, nil), false)
		connect.AssertEqual(t, configStateOwnsTags(state, self.owner.MemberId, [][]byte{self.ownedTagId.Bytes()}), false)
		connect.AssertEqual(t, configStateOwnsTags(nil, self.creator.MemberId, [][]byte{self.ownedTagId.Bytes()}), false)
	})
}

// A Network admin's key for its own tag enrolls a tagged client of the network, on the team's owner of record and
// sponsored by the creator, and its pre-approved one posts its admission and answers approved. Once a base takes the
// creator off the tag's tagOwners, both answer 401 auth_key_invalid, spending, minting and posting nothing; once a base
// names it again, both enroll again; once an AdminSet drops it to Member, both are refused though it still owns the
// tag.
func TestANetworkAdminsOwnedTagKeyEnrollsWhileItOwnsTheTag(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTagOwnerTeam(t)
		owned := testAuthKeyTagHex(self.ownedTagId)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{owned}})
		preApprovedBearer, preApprovedKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{owned}, PreApproved: true})

		first, err := self.enroll(t, bearer, nil)
		if err != nil {
			t.Fatalf("the Network admin's tagged enrollment: %v", err)
		}
		firstJwt := requireTeamEnrollCredential(t, self.ctx, first)
		connect.AssertEqual(t, firstJwt.UserId, self.ownerOfRecordId)
		connect.AssertEqual(t, firstJwt.MemberId == nil, true)
		row := readTeamEnrollOwnerRow(t, self.ctx, first.ClientId)
		connect.AssertEqual(t, row.tagged, true)
		connect.AssertEqual(t, row.ownerUserId == nil, true)
		connect.AssertEqual(t, row.sponsorUserId != nil && *row.sponsorUserId == self.creatorUserId, true)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 1)
		approved, err := self.enroll(t, preApprovedBearer, newCreatorRoleAdmission(t, self.networkId))
		if err != nil {
			t.Fatalf("the Network admin's pre-approved enrollment: %v", err)
		}
		connect.AssertEqual(t, approved.ApprovalState, NetworkEnrollApprovalStateApproved)
		connect.AssertEqual(t, self.appendedCount, 1)
		clientCount := countTeamEnrollClients(t, self.ctx, self.networkId)

		refused := func(useCount int) {
			t.Helper()
			_, err := self.enroll(t, bearer, nil)
			requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
			_, err = self.enroll(t, preApprovedBearer, newCreatorRoleAdmission(t, self.networkId))
			requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
			connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), useCount)
			connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, preApprovedKeyId), useCount)
			connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), clientCount)
		}

		// a save takes the creator off the tag's owners
		self.foldTagOwners(t, map[sidecar.Id][]server.Id{self.ownedTagId: {self.owner.MemberId}})
		refused(1)
		connect.AssertEqual(t, self.appendedCount, 1)
		// the client the key minted stays the network's
		connect.AssertEqual(t, readTeamEnrollClientActive(t, self.ctx, first.ClientId), true)

		// and a save names it again
		self.foldTagOwners(t, map[sidecar.Id][]server.Id{self.ownedTagId: {self.creator.MemberId, self.owner.MemberId}})
		again, err := self.enroll(t, bearer, nil)
		if err != nil {
			t.Fatalf("the tag owner's enrollment again: %v", err)
		}
		requireTeamEnrollCredential(t, self.ctx, again)
		if _, err := self.enroll(t, preApprovedBearer, newCreatorRoleAdmission(t, self.networkId)); err != nil {
			t.Fatalf("the tag owner's pre-approved enrollment again: %v", err)
		}
		connect.AssertEqual(t, self.appendedCount, 2)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 2)
		clientCount = countTeamEnrollClients(t, self.ctx, self.networkId)

		// an AdminSet drops it to Member, the tag still its own
		self.setCreatorRole(t, NetworkMemberRoleMember)
		refused(2)
		connect.AssertEqual(t, self.appendedCount, 2)
	})
}

// The spend's linearization point for a key for owned tags: a demotion to Member that commits inside the mint's
// transaction, at the barrier just before the spend, is met by the role read there under the membership's lock, so the
// key answers 401 auth_key_invalid, spending and minting nothing, though the log's owners were read before the mint.
func TestADemotionInsideTheMintRefusesAnOwnedTagKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTagOwnerTeam(t)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{testAuthKeyTagHex(self.ownedTagId)}})

		barrierCount := 0
		demote := true
		t.Cleanup(func() { Testing_EnrollBeforeConsume = nil })
		Testing_EnrollBeforeConsume = func() {
			barrierCount += 1
			if demote {
				demote = false
				self.setCreatorRole(t, NetworkMemberRoleMember)
			}
		}
		_, err := self.enroll(t, bearer, nil)
		requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		if barrierCount < 1 || demote {
			t.Fatalf("the barrier ran %d times, demoting %t", barrierCount, !demote)
		}
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 0)
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), 0)
	})
}

// §7.5 gives keys for owned tags to the Network admin: a Member whom the network's tagOwners name an owner of a tag
// still makes keys for its own devices alone, its tagged key for that tag refused 403 with nothing created.
func TestAMemberNamedInTagOwnersMakesNoTaggedKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTagOwnerTeam(t)
		self.setCreatorRole(t, NetworkMemberRoleMember)
		if reach := authKeyReachOf(self.creatorSession); reach != authKeyReachOwnDevices {
			t.Fatalf("a Member's reach is %d, want own devices", reach)
		}
		owned := testAuthKeyTagHex(self.ownedTagId)
		for _, args := range []*CreateAuthKeyArgs{
			{Tags: []string{owned}},
			{Tags: []string{owned}, PreApproved: true},
		} {
			result, err := self.createKey(args)
			if !authKeyRoleRefusal(err, authKeyOwnDevicesRefusedMessage) || result != nil {
				t.Errorf("the Member's key %+v answered %v %+v, not the own devices refusal", args, err, result)
			}
		}
		if result, err := self.createKey(&CreateAuthKeyArgs{}); err != nil || result.Error != nil {
			t.Fatalf("the Member's own key: %v %+v", err, result)
		}
		connect.AssertEqual(t, countTestAuthKeysOf(t, self.ctx, self.networkId, self.creatorUserId), 1)
	})
}

// Whose a tag is fails closed: while the log cannot be read, a Network admin's tagged key answers 503 at its creation,
// creating nothing, and its key made before answers 503 at enrollment, spending and minting nothing, while its key for
// its own devices, which asks no log, is made and enrolls; before the log's base 1 no tag has an owner, so the tagged
// key is refused 403.
func TestOwnedTagKeysFailClosedWithoutTheLog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newTagOwnerTeam(t)
		owned := testAuthKeyTagHex(self.ownedTagId)
		bearer, authKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{Tags: []string{owned}})

		previous := authKeyTagOwnerState
		t.Cleanup(func() { authKeyTagOwnerState = previous })
		authKeyTagOwnerState = func(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
			return nil, errors.New("synthetic unreadable log")
		}
		result, err := self.createKey(&CreateAuthKeyArgs{Tags: []string{owned}})
		if err == nil || err.Error() != "503 "+authKeyTagOwnersUnavailableMessage || result != nil {
			t.Fatalf("a tagged key with the log unreadable answered %v %+v", err, result)
		}
		connect.AssertEqual(t, countTestAuthKeysOf(t, self.ctx, self.networkId, self.creatorUserId), 1)
		_, err = self.enroll(t, bearer, nil)
		refusal := &NetworkEnrollRefusal{}
		if !errors.As(err, &refusal) || refusal.Status != http.StatusServiceUnavailable || refusal.Code != NetworkEnrollCodeUnavailable {
			t.Fatalf("an enrollment with the log unreadable answered %v", err)
		}
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 0)
		connect.AssertEqual(t, countTeamEnrollClients(t, self.ctx, self.networkId), 0)

		ownBearer, ownKeyId := newTeamEnrollAuthKey(t, self.creatorSession, &CreateAuthKeyArgs{})
		if _, err := self.enroll(t, ownBearer, nil); err != nil {
			t.Fatalf("the Network admin's own device with the log unreadable: %v", err)
		}
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, ownKeyId), 1)

		authKeyTagOwnerState = func(ctx context.Context, networkId server.Id) (*sidecar.ConfigState, error) {
			return nil, nil
		}
		result, err = self.createKey(&CreateAuthKeyArgs{Tags: []string{owned}})
		if !authKeyRoleRefusal(err, authKeyOwnedTagsRefusedMessage) || result != nil {
			t.Fatalf("a tagged key before base 1 answered %v %+v", err, result)
		}
		_, err = self.enroll(t, bearer, nil)
		requireTeamEnrollRefusal(t, err, http.StatusUnauthorized, NetworkEnrollCodeAuthKeyInvalid)
		connect.AssertEqual(t, readTeamEnrollKeyUseCount(t, self.ctx, authKeyId), 0)
	})
}

// The ids of the keys a listing holds, in its order.
func testListedAuthKeyIds(t testing.TB, clientSession *session.ClientSession) []server.Id {
	t.Helper()
	listed, err := GetNetworkAuthKeys(clientSession)
	if err != nil {
		t.Fatalf("list the keys: %v", err)
	}
	authKeyIds := []server.Id{}
	for _, authKey := range listed.AuthKeys {
		authKeyIds = append(authKeyIds, authKey.AuthKeyId)
	}
	return authKeyIds
}

// The ids sorted, as a listing orders them.
func testSortedAuthKeyIds(authKeyIds ...server.Id) []server.Id {
	sorted := slices.Clone(authKeyIds)
	slices.SortFunc(sorted, func(a server.Id, b server.Id) int {
		return a.Cmp(b)
	})
	return sorted
}

// A Member lists the keys it created and no one else's, and revokes its own, while another's key, a Member's or an
// Admin's, answers as no such key and stays; an Admin lists and revokes every key, an expired one listed by none.
func TestAMemberListsAndRevokesOnlyTheKeysItCreated(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		admin := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAdmin, false)
		member := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleMember, false)
		other := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleMember, false)
		adminSession := testAuthKeyMemberSession(ctx, teamId, admin)
		memberSession := testAuthKeyMemberSession(ctx, teamId, member)
		otherSession := testAuthKeyMemberSession(ctx, teamId, other)
		now := server.NowUtc()

		create := func(clientSession *session.ClientSession, args *CreateAuthKeyArgs) server.Id {
			return *createTestAuthKey(t, clientSession, args, now).AuthKeyId
		}
		memberKeyIds := []server.Id{
			create(memberSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}),
			create(memberSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 5, Ephemeral: true}),
		}
		otherKeyId := create(otherSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1})
		adminKeyId := create(adminSession, &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1, Tags: []string{teamEnrollTagHex}, PreApproved: true})
		// a key of the member's made an hour ago for five minutes, expired, which
		// no listing holds
		createTestAuthKey(t, memberSession, &CreateAuthKeyArgs{DurationSeconds: 300, MaxUses: 1}, now.Add(-time.Hour))

		connect.AssertEqual(t, testListedAuthKeyIds(t, memberSession), testSortedAuthKeyIds(memberKeyIds...))
		connect.AssertEqual(t, testListedAuthKeyIds(t, otherSession), []server.Id{otherKeyId})
		connect.AssertEqual(t, testListedAuthKeyIds(t, adminSession), testSortedAuthKeyIds(memberKeyIds[0], memberKeyIds[1], otherKeyId, adminKeyId))
		listed, err := GetNetworkAuthKeys(memberSession)
		if err != nil {
			t.Fatal(err)
		}
		for _, authKey := range listed.AuthKeys {
			connect.AssertEqual(t, authKey.Creator, member.UserId)
		}

		// another's key is no key to the member, and stays
		for _, authKeyId := range []server.Id{otherKeyId, adminKeyId} {
			result, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: authKeyId}, memberSession)
			if err != nil || result.Error == nil || result.Error.Message != "No such auth key." || result.BundleStillAdmits {
				t.Fatalf("the member's revoke of another's key %s answered %v %+v", authKeyId, err, result)
			}
		}
		connect.AssertEqual(t, testListedAuthKeyIds(t, otherSession), []server.Id{otherKeyId})
		connect.AssertEqual(t, len(testListedAuthKeyIds(t, adminSession)), 4)

		// its own key goes
		result, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: memberKeyIds[0]}, memberSession)
		if err != nil || result.Error != nil || result.BundleStillAdmits {
			t.Fatalf("the member's revoke of its own key answered %v %+v", err, result)
		}
		connect.AssertEqual(t, testListedAuthKeyIds(t, memberSession), []server.Id{memberKeyIds[1]})

		// the Admin revokes anyone's, a pre-approved key's answer saying its bundle still admits
		result, err = RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: otherKeyId}, adminSession)
		if err != nil || result.Error != nil {
			t.Fatalf("the Admin's revoke of a Member's key answered %v %+v", err, result)
		}
		connect.AssertEqual(t, testListedAuthKeyIds(t, otherSession), []server.Id{})
		result, err = RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: adminKeyId}, adminSession)
		if err != nil || result.Error != nil || !result.BundleStillAdmits || result.Note != revokeAuthKeyBundleNote {
			t.Fatalf("the Admin's revoke of its pre-approved key answered %v %+v", err, result)
		}
		connect.AssertEqual(t, testListedAuthKeyIds(t, adminSession), []server.Id{memberKeyIds[1]})
	})
}

// Whose keys each credential lists and revokes, its scopes cutting its role as at creation: a Member's, a Network
// admin's and an Auditor's token and a Member's auth_keys or all API key the keys its user created; an Admin's token,
// its auth_keys API key and OAuth client token every key, as a personal network's owner; a devices:core API key, a
// pending member and another team's member none, each refused 403 at both routes, nothing revoked.
func TestAuthKeyListingReachOfEachCredential(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		teamId, _ := newTestSignInTeam(t, ctx, false)
		otherTeamId, _ := newTestSignInTeam(t, ctx, false)
		member := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleMember, false)
		networkAdmin := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleNetworkAdmin, false)
		auditor := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAuditor, false)
		admin := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAdmin, false)
		pending := addTestSignInMember(t, ctx, teamId, server.NewId(), NetworkMemberRoleAdmin, true)
		elsewhere := addTestSignInMember(t, ctx, otherTeamId, server.NewId(), NetworkMemberRoleOwner, false)
		personalUserId, personalNetworkId := server.NewId(), server.NewId()
		Testing_CreateNetwork(ctx, personalNetworkId, "personal-"+personalNetworkId.String(), personalUserId)

		withApiKey := func(clientSession *session.ClientSession, scopes []string) *session.ClientSession {
			apiKeyId := server.NewId()
			clientSession.ApiKeyAuthenticated = true
			clientSession.ApiKeyId = &apiKeyId
			clientSession.ApiKeyScopes = scopes
			return clientSession
		}
		withOauthClient := func(clientSession *session.ClientSession, scopes []string) *session.ClientSession {
			clientSession.UnsignedIdentity = true
			clientSession.OauthClient = &session.OauthClientUse{
				NetworkId:     clientSession.ByJwt.NetworkId,
				MemberId:      *clientSession.ByJwt.MemberId,
				OauthClientId: server.NewId(),
				Scopes:        scopes,
				ExpireTime:    server.NowUtc().Add(time.Hour),
			}
			return clientSession
		}
		personal := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: personalUserId})

		cases := []struct {
			name          string
			clientSession *session.ClientSession
			// nil for every key
			creatorUserId *server.Id
			refused       bool
		}{
			{"a Member's token", testAuthKeyMemberSession(ctx, teamId, member), &member.UserId, false},
			{"a Member's auth_keys API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeAuthKeys}), &member.UserId, false},
			{"a Member's all API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeAll}), &member.UserId, false},
			{"a Member's auth_keys OAuth client token", withOauthClient(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeAuthKeys}), &member.UserId, false},
			{"a Network admin's token", testAuthKeyMemberSession(ctx, teamId, networkAdmin), &networkAdmin.UserId, false},
			{"an Auditor's token", testAuthKeyMemberSession(ctx, teamId, auditor), &auditor.UserId, false},
			{"an Admin's token", testAuthKeyMemberSession(ctx, teamId, admin), nil, false},
			{"an Admin's auth_keys API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, admin), []string{ApiKeyScopeAuthKeys}), nil, false},
			{"an Admin's auth_keys OAuth client token", withOauthClient(testAuthKeyMemberSession(ctx, teamId, admin), []string{ApiKeyScopeAuthKeys}), nil, false},
			{"a personal owner's token", personal, nil, false},
			{"a Member's devices:core API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, member), []string{ApiKeyScopeDevicesCore}), nil, true},
			{"an Admin's devices:core API key", withApiKey(testAuthKeyMemberSession(ctx, teamId, admin), []string{ApiKeyScopeDevicesCore}), nil, true},
			{"a pending member's token", testAuthKeyMemberSession(ctx, teamId, pending), nil, true},
			{"another team's member", testAuthKeyMemberSession(ctx, teamId, elsewhere), nil, true},
		}
		now := server.NowUtc()
		memberKeyId := *createTestAuthKey(t, testAuthKeyMemberSession(ctx, teamId, member), &CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1}, now).AuthKeyId
		for _, c := range cases {
			creatorUserId, ok := authKeyCreatorFilterOf(c.clientSession)
			switch {
			case ok == c.refused:
				t.Errorf("%s lists keys: %t, want %t", c.name, ok, !c.refused)
			case c.refused:
			case (creatorUserId == nil) != (c.creatorUserId == nil) || creatorUserId != nil && *creatorUserId != *c.creatorUserId:
				t.Errorf("%s lists the keys of %v, want %v", c.name, creatorUserId, c.creatorUserId)
			}
			if !c.refused {
				continue
			}
			listed, err := GetNetworkAuthKeys(c.clientSession)
			if !authKeyRoleRefusal(err, authKeyRoleRefusedMessage) || listed != nil {
				t.Errorf("%s's listing answered %v %+v", c.name, err, listed)
			}
			result, err := RevokeAuthKey(&RevokeAuthKeyArgs{AuthKeyId: memberKeyId}, c.clientSession)
			if !authKeyRoleRefusal(err, authKeyRoleRefusedMessage) || result != nil {
				t.Errorf("%s's revoke answered %v %+v", c.name, err, result)
			}
		}
		connect.AssertEqual(t, testListedAuthKeyIds(t, testAuthKeyMemberSession(ctx, teamId, member)), []server.Id{memberKeyId})
	})
}
