//go:build sidecar_pending

package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// §5.15's routes through the controller, on bindings sidecar signs for real.

// A network with one client, and that client's session.
func newSidecarBindTestClient(t testing.TB, ctx context.Context, label string) (*session.ClientSession, server.Id) {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, label, userId)
	userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: label})
	authResult, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, userSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, authResult)
	}
	byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return session.Testing_CreateClientSession(ctx, byJwt), userId
}

// Fresh keys for one sidecar.
type sidecarBindTestKeys struct {
	identitySeed []byte
	sidecarId    server.Id
}

func newSidecarBindTestKeys(t testing.TB) *sidecarBindTestKeys {
	t.Helper()
	identitySeed := make([]byte, 32)
	if _, err := rand.Read(identitySeed); err != nil {
		t.Fatal(err)
	}
	return &sidecarBindTestKeys{identitySeed: identitySeed, sidecarId: server.NewId()}
}

// A binding of generation for the session's client, signed by both keys.
func (self *sidecarBindTestKeys) bind(t testing.TB, clientSession *session.ClientSession, generation int64) []byte {
	t.Helper()
	clientKeySeed := make([]byte, 32)
	if _, err := rand.Read(clientKeySeed); err != nil {
		t.Fatal(err)
	}
	return self.bindWithClientKey(t, clientSession, generation, clientKeySeed)
}

// Auth-key enrollment commits the new client's public key before its dual-signed binding arrives.
func (self *sidecarBindTestKeys) bindWithClientKey(t testing.TB, clientSession *session.ClientSession, generation int64, clientKeySeed []byte) []byte {
	t.Helper()
	signedBinding, err := sidecar.NewSidecarIdentityBinding(&sidecar.SidecarIdentityBinding{
		NetworkId:  sidecar.Id(clientSession.ByJwt.NetworkId),
		SidecarId:  sidecar.Id(self.sidecarId),
		ClientId:   sidecar.Id(*clientSession.ByJwt.ClientId),
		Generation: generation,
		IssuedAt:   time.Date(2026, time.March, 4, 5, 6, 7, 0, time.UTC),
	}, self.identitySeed, clientKeySeed)
	if err != nil {
		t.Fatal(err)
	}
	return signedBinding
}

// The code a refusal carries, or "".
func sidecarRefusalCode(err error) (int, string) {
	refusal := &sidecarIdentityRefusal{}
	if !errors.As(err, &refusal) {
		return 0, ""
	}
	return refusal.status, refusal.code
}

// A real binding appends, its retry answers the same generation, and the next
// generation's client appends under the same identity.
func TestSidecarBindAppendsAndRetries(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-bind")
		keys := newSidecarBindTestKeys(t)
		signedBinding := keys.bind(t, clientSession, 1)
		for range 2 {
			result, err := SidecarBind(&SidecarBindArgs{Binding: signedBinding}, clientSession)
			if err != nil || result.Generation != 1 {
				t.Fatalf("bind: %v %+v", err, result)
			}
		}
		// the same client binding other bytes is refused
		_, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, clientSession, 1)}, clientSession)
		if status, code := sidecarRefusalCode(err); status != 409 || code != "client_bound" {
			t.Fatalf("other bytes: %v", err)
		}
	})
}

// The bearer must be the binding's client in its network, and a binding that
// does not verify is refused, each as binding_invalid with its coded body.
func TestBindRefusesOtherClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-bind-own")
		otherSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-bind-other")
		keys := newSidecarBindTestKeys(t)
		signedBinding := keys.bind(t, otherSession, 1)
		_, err := SidecarBind(&SidecarBindArgs{Binding: signedBinding}, clientSession)
		if status, code := sidecarRefusalCode(err); status != 400 || code != "binding_invalid" {
			t.Fatalf("another client's binding: %v", err)
		}
		tampered := append([]byte{}, keys.bind(t, clientSession, 1)...)
		tampered[len(tampered)-1] ^= 0x01
		_, err = SidecarBind(&SidecarBindArgs{Binding: tampered}, clientSession)
		if status, code := sidecarRefusalCode(err); status != 400 || code != "binding_invalid" {
			t.Fatalf("a tampered binding: %v", err)
		}
		_, err = SidecarBind(&SidecarBindArgs{Binding: nil}, clientSession)
		if status, code := sidecarRefusalCode(err); status != 400 || code != "binding_invalid" {
			t.Fatalf("no binding: %v", err)
		}
		// the refusal's body is the server's coded shape, and repeats no binding
		refusal := &sidecarIdentityRefusal{}
		errors.As(err, &refusal)
		body, _ := json.Marshal(refusal.HttpErrorResultBody())
		if !strings.Contains(string(body), `"code":"binding_invalid"`) || !strings.HasPrefix(err.Error(), "400 ") {
			t.Fatalf("body %s, error %v", body, err)
		}
		if model.GetSidecarIdentity(ctx, clientSession.ByJwt.NetworkId, keys.sidecarId, 0) != nil {
			t.Fatal("a refused bind created an identity")
		}
	})
}

// GET /sidecar/identity answers ids, keys, status and bindings, and never the
// member's user_id.
func TestIdentityNeverReturnsUserId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, userId := newSidecarBindTestClient(t, ctx, "sidecar-identity")
		keys := newSidecarBindTestKeys(t)
		signedBinding := keys.bind(t, clientSession, 1)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: signedBinding}, clientSession); err != nil {
			t.Fatal(err)
		}
		for _, lookup := range []*GetSidecarIdentityArgs{{SidecarId: &keys.sidecarId}, {ClientId: clientSession.ByJwt.ClientId}} {
			result, err := GetSidecarIdentity(lookup, clientSession)
			if err != nil || result.SidecarId != keys.sidecarId || string(result.Binding) != string(signedBinding) {
				t.Fatalf("lookup: %v %+v", err, result)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "user_id") || strings.Contains(string(encoded), userId.String()) {
				t.Fatalf("the answer names the user: %s", encoded)
			}
		}
		otherSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-identity-other")
		if _, err := GetSidecarIdentity(&GetSidecarIdentityArgs{SidecarId: &keys.sidecarId}, otherSession); err == nil || !strings.HasPrefix(err.Error(), "404 ") {
			t.Fatalf("another network: %v", err)
		}
	})
}

func TestOwnBoundBrowserAdmissionIdentity(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, ownerId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "browser-identity", ownerId)
		t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)))
		parent := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: ownerId, NetworkName: "browser-identity"})
		bind := func(keys *sidecarBindTestKeys, generation int64, ephemeral bool) (*session.ClientSession, []byte) {
			t.Helper()
			mint, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "sidecar", DeviceSpec: "synthetic",
				AdminRefused: true, StrictExp: true, DeclaresNoEgress: true, Ephemeral: ephemeral}, parent)
			if err != nil || mint == nil || mint.Error != nil || mint.ByClientJwt == nil {
				t.Fatalf("mint: %v %+v", err, mint)
			}
			jwt, err := session.ParseByJwt(ctx, *mint.ByClientJwt)
			if err != nil {
				t.Fatal(err)
			}
			device := session.Testing_CreateClientSession(ctx, jwt)
			binding := keys.bind(t, device, generation)
			if _, err := SidecarBind(&SidecarBindArgs{Binding: binding}, device); err != nil {
				t.Fatal(err)
			}
			return device, binding
		}
		signer, page, burner := newSidecarBindTestKeys(t), newSidecarBindTestKeys(t), newSidecarBindTestKeys(t)
		consumer, _ := bind(signer, 1, false)
		_, pageBinding := bind(page, 1, false)
		_, burnerBinding := bind(burner, 1, true)
		check := func(who *session.ClientSession, target *sidecarBindTestKeys, mode string, before int64, want []byte) {
			t.Helper()
			args := &GetSidecarIdentityArgs{SidecarId: &target.sidecarId, BeforeGeneration: before}
			if mode != "" {
				args.BrowserAdmission = &mode
			}
			reply, err := GetSidecarIdentity(args, who)
			if want == nil {
				if reply != nil || err == nil || !strings.HasPrefix(err.Error(), "404 ") {
					t.Fatalf("%s before %d: %+v %v", mode, before, reply, err)
				}
				return
			}
			if err != nil || reply == nil || reply.BrowserAdmission != mode || !bytes.Equal(reply.Binding, want) {
				t.Fatalf("%s binding: %+v %v", mode, reply, err)
			}
			encoded, err := json.Marshal(reply)
			if err != nil || strings.Contains(string(encoded), ownerId.String()) || (mode == "") != !strings.Contains(string(encoded), "browser_admission") {
				t.Fatalf("identity response fields: %v %s", err, encoded)
			}
		}
		check(consumer, page, "page", 0, pageBinding)
		check(consumer, burner, "burner", 0, burnerBinding)
		check(consumer, page, "burner", 0, nil)
		check(consumer, burner, "page", 0, nil)
		check(consumer, page, "page", 1, nil)
		check(consumer, page, "unknown", 0, nil)
		check(parent, page, "page", 0, nil)
		other, _ := newSidecarBindTestClient(t, ctx, "browser-other-member")
		check(other, page, "page", 0, nil)
		check(consumer, page, "", 0, pageBinding)
		current, _ := bind(signer, 2, false)
		check(consumer, page, "page", 0, nil)
		check(current, page, "page", 0, pageBinding)
	})
}

func TestTeamEnrollmentCarriesItsCurrentMembershipIntoBrowserApproval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		f := newEnrollFixture(t, true)
		creatorId := server.NewId()
		model.Testing_CreateNetwork(f.ctx, server.NewId(), "browser-team-member", creatorId)
		var member *model.NetworkMember
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `INSERT INTO network_team(network_id) VALUES($1)`, f.networkId))
			var err error
			member, err = model.AddNetworkMemberInTx(f.ctx, tx, &model.AddNetworkMemberArgs{NetworkId: f.networkId, UserId: creatorId, Role: model.NetworkMemberRoleAdmin})
			server.Raise(err)
		})
		signIn := model.ResolveNetworkSignIn(f.ctx, creatorId, f.networkId, server.NowUtc())
		if signIn == nil || signIn.MemberId == nil || *signIn.MemberId != member.MemberId {
			t.Fatal("team member missing")
		}
		f.root = session.Testing_CreateClientSession(f.ctx, model.NewNetworkSignInByJwt(signIn, false))
		bearer, _ := f.authKey(t, &model.CreateAuthKeyArgs{})
		clientPub, clientKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		enrolled, err := f.enroll(&model.NetworkEnrollArgs{AuthKey: bearer, DeviceSpec: "native member device", ClientPublicKey: clientPub, DeclaresNoEgress: true})
		if err != nil || enrolled == nil {
			t.Fatalf("enroll: %v", err)
		}
		jwt, err := session.ParseByJwt(f.ctx, enrolled.ByClientJwt)
		if err != nil || jwt.MemberId == nil || *jwt.MemberId != member.MemberId {
			t.Fatalf("team device membership: %v", err)
		}
		row := readEnrollOwnerRow(t, f.ctx, enrolled.ClientId)
		if row == nil || row.memberId == nil || *row.memberId != member.MemberId || row.ownerUserId == nil || *row.ownerUserId != creatorId {
			t.Fatalf("team enrollment owner: %+v", row)
		}
		signer := session.Testing_CreateClientSession(f.ctx, jwt)
		keys := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bindWithClientKey(t, signer, 1, clientKey.Seed())}, signer); err != nil {
			t.Fatal(err)
		}
		mint, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{DeviceSpec: "browser", Description: "browser", AdminRefused: true, StrictExp: true, DeclaresNoEgress: true}, f.root)
		if err != nil || mint == nil || mint.ByClientJwt == nil {
			t.Fatalf("page mint: %v %+v", err, mint)
		}
		jwt, err = session.ParseByJwt(f.ctx, *mint.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		page := session.Testing_CreateClientSession(f.ctx, jwt)
		candidate := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: candidate.bind(t, page, 1)}, page); err != nil {
			t.Fatal(err)
		}
		mode := "page"
		lookup := &GetSidecarIdentityArgs{SidecarId: &candidate.sidecarId, BrowserAdmission: &mode}
		if reply, err := GetSidecarIdentity(lookup, signer); err != nil || reply == nil || reply.BrowserAdmission != "page" {
			t.Fatalf("eligible team page: %v %+v", err, reply)
		}
		// A returning user has a new member_id. The old signer and old page still name the prior membership.
		server.Tx(f.ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(f.ctx, `DELETE FROM network_member WHERE member_id=$1`, member.MemberId))
			_, err := model.AddNetworkMemberInTx(f.ctx, tx, &model.AddNetworkMemberArgs{NetworkId: f.networkId, UserId: creatorId, Role: model.NetworkMemberRoleAdmin})
			server.Raise(err)
		})
		if reply, err := GetSidecarIdentity(lookup, signer); reply != nil || err == nil || !strings.HasPrefix(err.Error(), "404 ") {
			t.Fatalf("old team membership: %v %+v", err, reply)
		}
	})
}

// A strict_exp client api holds no binding for refreshes to restore_required,
// which is no refusal, and refreshes as usual once it has bound (§5.6, §19.10).
func TestRefreshAnswersRestoreRequired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-restore")
		strictByJwt := clientSession.ByJwt.Renew()
		strictByJwt.SetStrictExp(true)
		strictExpSession := session.Testing_CreateClientSession(ctx, strictByJwt)

		result, err := RefreshToken(strictExpSession)
		if err != nil || result.Error == nil || result.Error.Code != RefreshTokenCodeRestoreRequired || result.ByJwt != "" {
			t.Fatalf("no binding: %v %+v", err, result)
		}
		// a client without the mark refreshes as ever
		result, err = RefreshToken(clientSession)
		if err != nil || result.Error != nil || result.ByJwt == "" {
			t.Fatalf("an unmarked client: %v %+v", err, result)
		}

		keys := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, clientSession, 1)}, clientSession); err != nil {
			t.Fatal(err)
		}
		result, err = RefreshToken(strictExpSession)
		if err != nil || (result.Error != nil && result.Error.Code == RefreshTokenCodeRestoreRequired) {
			t.Fatalf("a bound client: %v %+v", err, result)
		}
	})
}

// Across an active §8.11 share (CORE-M6-04) the lookups answer the other
// network's identity as they answer the caller's own, ids, keys, status and
// bindings, and still never a user_id; once the share's row goes they answer
// 404 again.
func TestIdentityAcrossShareNeverReturnsUserId(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		restore := model.Testing_SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		})
		defer restore()
		hostSession, hostUserId := newSidecarBindTestClient(t, ctx, "sidecar-identity-host")
		keys := newSidecarBindTestKeys(t)
		signedBinding := keys.bind(t, hostSession, 1)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: signedBinding}, hostSession); err != nil {
			t.Fatal(err)
		}
		routerSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-identity-router")
		lookups := []*GetSidecarIdentityArgs{{SidecarId: &keys.sidecarId}, {ClientId: hostSession.ByJwt.ClientId}}
		for _, lookup := range lookups {
			if _, err := GetSidecarIdentity(lookup, routerSession); err == nil || !strings.HasPrefix(err.Error(), "404 ") {
				t.Fatalf("no share: %v", err)
			}
		}

		// the host's network shares, and the router's accepts
		shareId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := model.CreateNetworkBridgeShareInTx(ctx, tx, shareId, hostSession.ByJwt.NetworkId, []byte("targets sealed under K"), 1); err != nil {
				t.Fatalf("create: %v", err)
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			if err := model.AcceptNetworkBridgeShareInTx(ctx, tx, shareId, routerSession.ByJwt.NetworkId, []byte("aliases sealed under K"), 1, model.NetworkBridgeShareAcceptStatusActive); err != nil {
				t.Fatalf("accept: %v", err)
			}
		})
		for _, lookup := range lookups {
			result, err := GetSidecarIdentity(lookup, routerSession)
			if err != nil || result.SidecarId != keys.sidecarId || string(result.Binding) != string(signedBinding) {
				t.Fatalf("across the share: %v %+v", err, result)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "user_id") || strings.Contains(string(encoded), hostUserId.String()) {
				t.Fatalf("the answer names the user: %s", encoded)
			}
		}

		if !model.RemoveNetworkBridgeShare(ctx, shareId) {
			t.Fatal("the share's row did not go")
		}
		for _, lookup := range lookups {
			if _, err := GetSidecarIdentity(lookup, routerSession); err == nil || !strings.HasPrefix(err.Error(), "404 ") {
				t.Fatalf("a revoked share: %v", err)
			}
		}
	})
}
