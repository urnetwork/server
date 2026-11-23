//go:build sidecar_pending

package oauth

// A team's enrolled devices at the exchange (§7.5, §7.8, §8.6): each enrolls
// through model.EnrollNetworkClient from a key a team member made, on Postgres
// with MG0, MG1 and MG6, so an untagged device's token names the member's
// membership and a tagged one's the team's owner of record (QD-1194), and
// exchanges with that token and its own ClientKey's proof against the team's
// real log (exchangeScopesWorld), whose genesis names the member. Only the
// client's sidecar binding is the test's, since no identity is registered.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A base folding through the tip whose rows add tagId's atom with ownerMemberId
// its tag owner and a rule giving the tag read of readRepoId, as a saved
// document's tagOwners and grant compile them (§8.4), so the owner's device may
// admit a sidecar carrying the tag (AclState.CanApplyTag) and the tag's
// sidecars read the repo.
func (self *exchangeScopesWorld) foldWithTag(tagId sidecar.Id, ownerMemberId server.Id, readRepoId server.Id) {
	self.t.Helper()
	snapshot := self.load()
	state := snapshot.Config
	previous := state.Base()
	rows := state.Rows()
	acl := &protocol.AclRows{}
	if err := proto.Unmarshal(rows.GetAcl(), acl); err != nil {
		self.t.Fatal(err)
	}
	acl.Atoms = append(acl.Atoms, &protocol.AclAtom{Atom: tagId.Bytes(), TagOwnerMemberIds: [][]byte{sidecar.Id(ownerMemberId).Bytes()}})
	slices.SortFunc(acl.Atoms, func(a *protocol.AclAtom, b *protocol.AclAtom) int {
		return bytes.Compare(a.GetAtom(), b.GetAtom())
	})
	acl.Rules = append(acl.Rules, &protocol.AclRule{
		RuleId:   sidecar.NewId().Bytes(),
		SrcAtoms: [][]byte{tagId.Bytes()},
		Targets:  []*protocol.AclTarget{{RepoId: sidecar.Id(readRepoId).Bytes()}},
		Actions:  []uint32{uint32(protocol.AclAction_ACL_ACTION_READ)},
	})
	aclBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(acl)
	if err != nil {
		self.t.Fatal(err)
	}
	rows.Acl = aclBytes
	rowsBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(rows)
	if err != nil {
		self.t.Fatal(err)
	}
	rowsSha256 := sha256.Sum256(rowsBytes)
	signedHeader := self.world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, &bridge.ProjectionHeader{
		NetworkId:        sidecar.Id(self.networkId).Bytes(),
		Version:          self.tip.GetVersion() + 1,
		PrevChain:        self.tip.GetChain(),
		FoldedThrough:    self.tip.GetVersion(),
		ConfigVersion:    previous.GetConfigVersion(),
		ConfigHeadSha256: previous.GetConfigHeadSha256(),
		RowsSha256:       rowsSha256[:],
		RowsByteCount:    uint32(len(rowsBytes)),
		Threshold:        previous.GetThreshold(),
	})
	pin, err := self.sequencer.AppendBase(self.ctx, self.networkId, &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rowsBytes})
	if err != nil {
		self.t.Fatalf("the base with the tag: %v", err)
	}
	self.world.SetTip(pin)
	self.tip = pin
}

// A team's tagged device holds its tag's rights and outlives its sponsor at the
// exchange (owner decision 83; QD-1194): enrolled from a tagged key the team's
// Owner made, its token names the team's owner of record with no member_id,
// whose credential change time lies past every token. Admitted in the team's
// log carrying its one tag, which the Owner's device applies as the tag's
// owner, its token and proof buy the config's read and the general repo a rule
// gives the tag, never another general repo, the service token naming its
// sidecar and no member, and still do once api removes its sponsor, the key's
// creator. Its own removal at api ends the exchange with invalid_grant.
func TestExchangeTakesATeamsTaggedDevicePastItsSponsor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		installTestSignerKeys(t)
		t.Setenv("WARP_DOMAIN", "exchange.example")
		ctx := t.Context()

		// the team as team-create leaves it, its owner of record refusing every
		// token by its change time, and its creator's membership
		networkId, ownerOfRecordId, creatorUserId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "team-"+networkId.String(), ownerOfRecordId)
		model.Testing_CreateNetwork(ctx, server.NewId(), "creator-"+creatorUserId.String(), creatorUserId)
		var creator *model.NetworkMember
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id, sso_required) VALUES ($1, false)`, networkId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`, ownerOfRecordId, time.Date(9999, time.January, 1, 0, 0, 0, 0, time.UTC)))
			var err error
			creator, err = model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: networkId, UserId: creatorUserId, Role: model.NetworkMemberRoleOwner})
			server.Raise(err)
		})
		signIn := model.ResolveNetworkSignIn(ctx, creatorUserId, networkId, server.NowUtc())
		if signIn == nil || signIn.MemberId == nil {
			t.Fatalf("the creator's sign-in: %+v", signIn)
		}
		creatorSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", model.NewNetworkSignInByJwt(signIn, false))
		defer creatorSession.Cancel()
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)))
		t.Cleanup(model.Testing_SetNetworkEnrollHasGenesis(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		}))

		tagId := sidecar.NewId()
		key, err := model.CreateAuthKey(&model.CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1, Tags: []string{hex.EncodeToString(tagId.Bytes())}}, creatorSession)
		if err != nil || key.Error != nil {
			t.Fatalf("the Owner's tagged key: %v %+v", err, key)
		}
		publicKey, privateKey, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		enrolled, err := model.EnrollNetworkClient(ctx, &model.NetworkEnrollArgs{AuthKey: key.AuthKey, ClientPublicKey: publicKey, DeviceSpec: "team agent"}, "192.0.2.9:1")
		if err != nil {
			t.Fatalf("the team's tagged enrollment: %v", err)
		}
		byJwt, err := session.ParseByJwt(ctx, enrolled.ByClientJwt)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, byJwt.UserId, ownerOfRecordId)
		connect.AssertEqual(t, byJwt.MemberId == nil, true)

		self := newExchangeScopesWorldFor(t, networkId, creator.MemberId, server.NewId())
		defer SetExchangeScopeSource(self.source())()
		taggedSidecarId := server.NewId()
		previousSidecarId := exchangeClientSidecarId
		exchangeClientSidecarId = func(ctx context.Context, networkId server.Id, clientId server.Id) *server.Id {
			if clientId == enrolled.ClientId {
				return &taggedSidecarId
			}
			return nil
		}
		t.Cleanup(func() {
			exchangeClientSidecarId = previousSidecarId
		})
		jti := 0
		exchange := func(audience string, scopes ...string) (*ExchangeResult, error) {
			jti += 1
			proof := session.Testing_SignDpopProof(privateKey, "POST", TokenEndpoint(), enrolled.ByClientJwt, fmt.Sprintf("team-tagged-jti-%d-%s", jti, server.NewId()), server.NowUtc())
			return Exchange(ctx, &ExchangeArgs{
				SubjectToken:     enrolled.ByClientJwt,
				SubjectTokenType: ExchangeSubjectTokenType,
				Resources:        []string{"https://" + audience + ".exchange.example"},
				Scope:            strings.Join(scopes, " "),
				Proofs:           []string{proof},
			})
		}

		// a document giving the tag read of one general repo of two, then the
		// sidecar admitted carrying its one tag, by the Owner's device as the
		// tag's owner
		taggedRepoId := self.createGeneralRepo()
		otherRepoId := self.createGeneralRepo()
		self.foldWithTag(tagId, creator.MemberId, taggedRepoId)
		self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
			Header:            self.world.Header(),
			NetworkId:         sidecar.Id(networkId).Bytes(),
			SidecarId:         sidecar.Id(taggedSidecarId).Bytes(),
			IdentityPublicKey: exchangeScopesIdentityKey(t),
			IdentityAlg:       "ed25519",
			TagIds:            [][]byte{tagId.Bytes()},
			NotAfter:          self.world.Clock().Now().Add(24 * time.Hour).UnixMilli(),
		}))
		snapshot := self.load()
		admitted := snapshot.Config.Sidecar(sidecar.Id(taggedSidecarId))
		if admitted == nil || len(admitted.TagIds) != 1 || admitted.TagIds[0] != tagId {
			t.Fatalf("the tagged sidecar's admission: %+v", admitted)
		}
		configRead := repoScope(self.configRepoId(snapshot), "r")
		taggedRead := repoScope(taggedRepoId, "r")
		otherRead := repoScope(otherRepoId, "r")
		verifier := newTestServiceTokenVerifier(t)
		requireTagRights := func(name string) {
			result, err := exchange("evs", configRead, taggedRead, otherRead)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			connect.AssertEqual(t, strings.Fields(result.Scope), []string{configRead, taggedRead})
			claims, err := verifier.Verify(result.AccessToken, "evs")
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, claims.ClientId, enrolled.ClientId)
			connect.AssertEqual(t, claims.SidecarId != nil && *claims.SidecarId == taggedSidecarId, true)
			connect.AssertEqual(t, claims.MemberId == nil, true)
			_, err = exchange("evs", otherRead)
			requireExchangeRefused(t, name+", the other repo alone", err, ErrInvalidScope)
		}
		requireTagRights("the admitted tagged device's tag rights")

		// its sponsor removed at api: the client stays, and so does its exchange
		server.Tx(ctx, func(tx server.PgTx) {
			memberId := creator.MemberId
			_, err := model.DenyNetworkMemberInTx(ctx, tx, &model.NetworkMemberDenial{
				NetworkId: networkId,
				MemberId:  memberId,
				Time:      server.NowUtc(),
				Event: &model.AdminEvent{
					NetworkId:  networkId,
					ActorKind:  model.AdminEventActorSystem,
					Action:     model.AdminEventActionMemberRemove,
					TargetKind: model.AdminEventTargetMember,
					TargetId:   &memberId,
					Result:     model.AdminEventResultOk,
				},
			})
			server.Raise(err)
		})
		requireTagRights("the tagged device's tag rights past its sponsor's removal")

		// its own removal at api ends it
		enrolledSession := session.NewLocalClientSession(ctx, "192.0.2.9:1", byJwt)
		defer enrolledSession.Cancel()
		removed, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: enrolled.ClientId}, enrolledSession)
		if err != nil || removed.Error != nil {
			t.Fatalf("the tagged device's removal: %v %+v", err, removed)
		}
		_, err = exchange("evs", configRead)
		requireExchangeRefused(t, "the removed tagged device's exchange", err, ErrInvalidGrant)
	})
}

// A team member's ephemeral device holds no more than §8.6's ceiling: before
// its admission the exchange refuses it its member's recovery scope, which a
// durable device enrolled from the same member's key takes, since its owner
// row marks it ephemeral; once its member's device admits it ephemeral, its
// token buys the config's read alone at evs, neither a general repo nor the
// config's admin, which the durable device holds as a config writer (QD-894).
func TestExchangeGivesATeamsEnrolledEphemeralDeviceTheCeilingAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		installTestSignerKeys(t)
		t.Setenv("WARP_DOMAIN", "exchange.example")
		ctx := t.Context()

		// the team as team-create leaves it, and its creator's membership
		networkId, ownerOfRecordId, creatorUserId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "team-"+networkId.String(), ownerOfRecordId)
		model.Testing_CreateNetwork(ctx, server.NewId(), "creator-"+creatorUserId.String(), creatorUserId)
		var creator *model.NetworkMember
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id, sso_required) VALUES ($1, false)`, networkId))
			var err error
			creator, err = model.AddNetworkMemberInTx(ctx, tx, &model.AddNetworkMemberArgs{NetworkId: networkId, UserId: creatorUserId, Role: model.NetworkMemberRoleOwner})
			server.Raise(err)
		})
		signIn := model.ResolveNetworkSignIn(ctx, creatorUserId, networkId, server.NowUtc())
		if signIn == nil || signIn.MemberId == nil {
			t.Fatalf("the creator's sign-in: %+v", signIn)
		}
		creatorSession := session.NewLocalClientSession(ctx, "192.0.2.1:1", model.NewNetworkSignInByJwt(signIn, false))
		defer creatorSession.Cancel()
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)))
		t.Cleanup(model.Testing_SetNetworkEnrollHasGenesis(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		}))

		type enrolledDevice struct {
			clientId   server.Id
			token      string
			privateKey ed25519.PrivateKey
		}
		enroll := func(ephemeral bool) *enrolledDevice {
			key, err := model.CreateAuthKey(&model.CreateAuthKeyArgs{DurationSeconds: 3600, MaxUses: 1, Ephemeral: ephemeral}, creatorSession)
			if err != nil || key.Error != nil {
				t.Fatalf("the creator's key: %v %+v", err, key)
			}
			publicKey, privateKey, err := ed25519.GenerateKey(nil)
			if err != nil {
				t.Fatal(err)
			}
			result, err := model.EnrollNetworkClient(ctx, &model.NetworkEnrollArgs{AuthKey: key.AuthKey, ClientPublicKey: publicKey, DeviceSpec: "team device"}, "192.0.2.9:1")
			if err != nil {
				t.Fatalf("the team enrollment (ephemeral %t): %v", ephemeral, err)
			}
			return &enrolledDevice{clientId: result.ClientId, token: result.ByClientJwt, privateKey: privateKey}
		}
		ephemeral := enroll(true)
		durable := enroll(false)

		self := newExchangeScopesWorldFor(t, networkId, creator.MemberId, server.NewId())
		defer SetExchangeScopeSource(self.source())()
		ephemeralSidecarId, durableSidecarId := server.NewId(), server.NewId()
		previousSidecarId := exchangeClientSidecarId
		exchangeClientSidecarId = func(ctx context.Context, networkId server.Id, clientId server.Id) *server.Id {
			switch clientId {
			case ephemeral.clientId:
				return &ephemeralSidecarId
			case durable.clientId:
				return &durableSidecarId
			}
			return nil
		}
		t.Cleanup(func() {
			exchangeClientSidecarId = previousSidecarId
		})
		jti := 0
		exchange := func(device *enrolledDevice, audience string, scopes ...string) (*ExchangeResult, error) {
			jti += 1
			proof := session.Testing_SignDpopProof(device.privateKey, "POST", TokenEndpoint(), device.token, fmt.Sprintf("team-enroll-jti-%d-%s", jti, server.NewId()), server.NowUtc())
			return Exchange(ctx, &ExchangeArgs{
				SubjectToken:     device.token,
				SubjectTokenType: ExchangeSubjectTokenType,
				Resources:        []string{"https://" + audience + ".exchange.example"},
				Scope:            strings.Join(scopes, " "),
				Proofs:           []string{proof},
			})
		}

		// unadmitted: the member's recovery scope goes to its durable device alone
		self.load()
		recovery := recoveryScope(creator.MemberId)
		_, err := exchange(ephemeral, "gkm", recovery)
		requireExchangeRefused(t, "the unadmitted ephemeral device's recovery scope", err, ErrInvalidGrant)
		result, err := exchange(durable, "gkm", recovery)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Scope, recovery)

		// admitted, ephemeral and durable, and a general repo the member's first device files
		self.admitOwnerDevice(ephemeralSidecarId, true, self.world.Clock().Now().Add(24*time.Hour).UnixMilli(), 0)
		self.admitOwnerDevice(durableSidecarId, false, 0, 0)
		generalRepoId := self.createGeneralRepo()
		snapshot := self.load()
		configRead := repoScope(self.configRepoId(snapshot), "r")
		configAdmin := repoScope(self.configRepoId(snapshot), "admin")
		generalRead := repoScope(generalRepoId, "r")

		result, err = exchange(ephemeral, "evs", configRead, generalRead, configAdmin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Scope, configRead)
		claims, err := newTestServiceTokenVerifier(t).Verify(result.AccessToken, "evs")
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.ClientId, ephemeral.clientId)
		connect.AssertEqual(t, claims.SidecarId != nil && *claims.SidecarId == ephemeralSidecarId, true)
		_, err = exchange(ephemeral, "evs", generalRead)
		requireExchangeRefused(t, "the ephemeral device's general repo read", err, ErrInvalidScope)

		// the member's durable device, a config writer, takes the config's admin, which the ceiling keeps from the
		// ephemeral one
		result, err = exchange(durable, "evs", configRead, configAdmin)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, strings.Fields(result.Scope), []string{configRead, configAdmin})
	})
}
