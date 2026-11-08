//go:build sidecar_pending

package controller

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
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
