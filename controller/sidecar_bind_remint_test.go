//go:build sidecar_pending

package controller

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// POST /sidecar/bind's re-mint (plan-SOCK-client-auth, QD-040) on bindings sidecar signs for real: the claim
// names the binding's sidecar, refresh keeps it, and no child inherits it.

// The token a bind answered, parsed as api parses a bearer.
func parseSidecarRemint(t testing.TB, ctx context.Context, result *SidecarBindResult) *session.ByJwt {
	t.Helper()
	if result == nil || result.ByClientJwt == "" {
		t.Fatalf("no re-minted token: %+v", result)
	}
	byJwt, err := session.ParseByJwt(ctx, result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return byJwt
}

// A bind answers the bearer's credential with the binding's sidecar as its sidecar_id, every other claim as
// presented, its lifetime included, and only a new jti; its byte-identical retry answers the same.
func TestSidecarBindRemintsTheBearerWithItsSidecar(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-bind-remint")
		presented := clientSession.ByJwt
		if presented.SidecarId != nil {
			t.Fatalf("a client token carries a sidecar before its bind: %s", presented.SidecarId)
		}
		keys := newSidecarBindTestKeys(t)
		signedBinding := keys.bind(t, clientSession, 1)
		for range 2 {
			result, err := SidecarBind(&SidecarBindArgs{Binding: signedBinding}, clientSession)
			if err != nil || result.Generation != 1 {
				t.Fatalf("bind: %v %+v", err, result)
			}
			reminted := parseSidecarRemint(t, ctx, result)
			if reminted.SidecarId == nil || *reminted.SidecarId != keys.sidecarId {
				t.Fatalf("sidecar_id %v, want %s", reminted.SidecarId, keys.sidecarId)
			}
			if reminted.NetworkId != presented.NetworkId || reminted.UserId != presented.UserId ||
				*reminted.ClientId != *presented.ClientId || *reminted.DeviceId != *presented.DeviceId ||
				reminted.RootClientId == nil || *reminted.RootClientId != *presented.RootClientId ||
				reminted.StrictExp != presented.StrictExp || !reminted.CreateTime.Equal(presented.CreateTime) {
				t.Fatalf("identity %+v, presented %+v", reminted, presented)
			}
			if !reminted.ExpiresAt.Equal(presented.ExpiresAt.Time) || !reminted.IssuedAt.Equal(presented.IssuedAt.Time) ||
				!reminted.NotBefore.Equal(presented.NotBefore.Time) {
				t.Fatalf("a re-mint moved the lifetime: exp %v iat %v nbf %v, presented %v %v %v",
					reminted.ExpiresAt, reminted.IssuedAt, reminted.NotBefore, presented.ExpiresAt, presented.IssuedAt, presented.NotBefore)
			}
			if reminted.ID == "" || reminted.ID == presented.ID {
				t.Fatalf("jti %q, presented %q", reminted.ID, presented.ID)
			}
		}
	})
}

// A refresh of the re-minted credential keeps its sidecar_id, and a client the sidecar's credential creates
// carries none: it is another client, which acts as no sidecar.
func TestSidecarClaimKeptByRefreshAndNotByAChild(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, _ := newSidecarBindTestClient(t, ctx, "sidecar-claim-refresh")
		keys := newSidecarBindTestKeys(t)
		result, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, clientSession, 1)}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		reminted := parseSidecarRemint(t, ctx, result)
		remintedSession := session.Testing_CreateClientSession(ctx, reminted)

		refreshed, err := RefreshToken(remintedSession)
		if err != nil || refreshed.Error != nil {
			t.Fatalf("refresh: %v %+v", err, refreshed)
		}
		refreshedByJwt, err := session.ParseByJwt(ctx, refreshed.ByJwt)
		if err != nil {
			t.Fatal(err)
		}
		if refreshedByJwt.SidecarId == nil || *refreshedByJwt.SidecarId != keys.sidecarId {
			t.Fatalf("a refresh dropped or changed sidecar_id: %v", refreshedByJwt.SidecarId)
		}

		// a client token creates only children of its own client
		child, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{
			SourceClientId: reminted.ClientId,
			Description:    "synthetic",
			DeviceSpec:     "synthetic",
		}, remintedSession)
		if err != nil || child.Error != nil || child.ByClientJwt == nil {
			t.Fatalf("child: %v %+v %+v", err, child, child.Error)
		}
		childByJwt, err := session.ParseByJwt(ctx, *child.ByClientJwt)
		if err != nil {
			t.Fatal(err)
		}
		if childByJwt.SidecarId != nil {
			t.Fatalf("a child inherited its parent's sidecar_id %s", childByJwt.SidecarId)
		}
	})
}
