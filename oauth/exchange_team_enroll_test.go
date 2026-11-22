//go:build sidecar_pending

package oauth

// A team's enrolled devices at the exchange (§7.5, §7.8, §8.6): each enrolls
// through model.EnrollNetworkClient from a key a team member made, on Postgres
// with MG0, MG1 and MG6, so its token names the member's membership, and
// exchanges with that token and its own ClientKey's proof against the team's
// real log (exchangeScopesWorld), whose genesis names the member. Only the
// client's sidecar binding is the test's, since no identity is registered.

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

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
