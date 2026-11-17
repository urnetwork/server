//go:build sidecar_pending

package controller

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The by_jwt's admin_refused claim through api's own mint, bind and refresh (§7.4; owner decision 291, QD-907): a
// child that an app mints under its device client with both marks carries the claim from its owner row, bind's
// re-mint and a refresh keep it, and every one of those tokens passes the daemon's pairing check
// (sidecar.DaemonPairNetwork, which Sidecar.Pair's check shares); a client whose owner row has no admin_refused mark
// carries no claim, whatever its caller's token says, and fails that check.

// A signed token's claims as a holder that did not mint it reads them: the payload, unverified.
func adminRefusedClaimsOf(t testing.TB, signed string) map[string]any {
	t.Helper()
	parts := strings.Split(signed, ".")
	if len(parts) != 3 {
		t.Fatalf("a token of %d parts", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]any{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	return claims
}

// Requires signed to carry admin_refused as a JSON true and parse back with the mark, or to carry no such claim at
// all; answers the token as api parses it.
func requireAdminRefusedClaim(t testing.TB, ctx context.Context, signed string, adminRefused bool) *session.ByJwt {
	t.Helper()
	claim, present := adminRefusedClaimsOf(t, signed)["admin_refused"]
	if adminRefused && claim != true || !adminRefused && present {
		t.Fatalf("admin_refused claim %v (present %t), want %t", claim, present, adminRefused)
	}
	byJwt, err := session.ParseByJwt(ctx, signed)
	if err != nil {
		t.Fatal(err)
	}
	if byJwt.AdminRefused != adminRefused {
		t.Fatalf("parsed admin_refused %t, want %t", byJwt.AdminRefused, adminRefused)
	}
	return byJwt
}

// Requires the daemon's pairing check to take signed as a child of networkId, or to refuse it.
func requirePairable(t testing.TB, signed string, networkId server.Id, pairable bool) {
	t.Helper()
	named, ok := sidecar.DaemonPairNetwork(signed)
	if ok != pairable || pairable && named != sidecar.Id(networkId) {
		t.Fatalf("the pairing check names %s, %t; want %t", named, ok, pairable)
	}
}

// A network in sidecar.yml's preview_networks, so it may hold strict_exp clients (D-TRUST-3), and its owner's
// network credential as a session; byJwt's other fields are set on that credential.
func newAdminRefusedClaimNetwork(t testing.TB, ctx context.Context, label string, byJwt session.ByJwt) (server.Id, *session.ClientSession) {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, label, userId)
	t.Cleanup(server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId)))
	byJwt.NetworkId = networkId
	byJwt.UserId = userId
	byJwt.NetworkName = label
	return networkId, session.Testing_CreateClientSession(ctx, &byJwt)
}

// Mints args with caller, requiring that it succeeds, and answers the result with a session of its token.
func mintAdminRefusedClaimClient(t testing.TB, ctx context.Context, caller *session.ClientSession, args *model.AuthNetworkClientArgs) (*model.AuthNetworkClientResult, *session.ClientSession) {
	t.Helper()
	result, err := model.AuthNetworkClient(args, caller)
	if err != nil || result.Error != nil || result.ByClientJwt == nil {
		t.Fatalf("auth-client: %v %+v", err, result)
	}
	byJwt, err := session.ParseByJwt(ctx, *result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return result, session.Testing_CreateClientSession(ctx, byJwt)
}

// §5.8's pair on api's side: the app's device client D, admin_refused as §18.7 mints it, carries the claim and no
// strict_exp, so its own token does not pair; the child C it mints with both marks carries both claims, and so do
// bind's re-mint of C's token, the re-mint's refresh and a reissue of C, each of which pairs.
func TestPairChildCarriesAdminRefusedThroughBindAndRefresh(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, root := newAdminRefusedClaimNetwork(t, ctx, "admin-refused-claim", session.ByJwt{})

		device, deviceSession := mintAdminRefusedClaimClient(t, ctx, root, &model.AuthNetworkClientArgs{
			Description: "device", DeviceSpec: "synthetic", AdminRefused: true,
		})
		if deviceByJwt := requireAdminRefusedClaim(t, ctx, *device.ByClientJwt, true); deviceByJwt.StrictExp {
			t.Fatal("an app's device client carries strict_exp")
		}
		requirePairable(t, *device.ByClientJwt, networkId, false)

		child, childSession := mintAdminRefusedClaimClient(t, ctx, deviceSession, &model.AuthNetworkClientArgs{
			SourceClientId: device.ClientId, Description: "sidecar", DeviceSpec: "synthetic", AdminRefused: true, StrictExp: true,
		})
		if !child.AdminRefused || !child.StrictExp {
			t.Fatalf("the child's reply marks: %+v", child)
		}
		if childByJwt := requireAdminRefusedClaim(t, ctx, *child.ByClientJwt, true); !childByJwt.StrictExp {
			t.Fatal("the child carries no strict_exp")
		}
		requirePairable(t, *child.ByClientJwt, networkId, true)

		keys := newSidecarBindTestKeys(t)
		bound, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, childSession, 1)}, childSession)
		if err != nil || bound.Generation != 1 {
			t.Fatalf("bind: %v %+v", err, bound)
		}
		reminted := requireAdminRefusedClaim(t, ctx, bound.ByClientJwt, true)
		if reminted.SidecarId == nil || *reminted.SidecarId != keys.sidecarId || !reminted.StrictExp {
			t.Fatalf("bind's re-mint: %+v", reminted)
		}
		requirePairable(t, bound.ByClientJwt, networkId, true)

		refreshed, err := RefreshToken(session.Testing_CreateClientSession(ctx, reminted))
		if err != nil || refreshed.Error != nil {
			t.Fatalf("refresh: %v %+v", err, refreshed)
		}
		if refreshedByJwt := requireAdminRefusedClaim(t, ctx, refreshed.ByJwt, true); !refreshedByJwt.StrictExp {
			t.Fatal("the refresh dropped strict_exp")
		}
		requirePairable(t, refreshed.ByJwt, networkId, true)

		// a reissue reads C's row again, whoever asks: here the network credential
		reissued, _ := mintAdminRefusedClaimClient(t, ctx, root, &model.AuthNetworkClientArgs{
			ClientId: child.ClientId, Description: "sidecar", DeviceSpec: "synthetic",
		})
		requireAdminRefusedClaim(t, ctx, *reissued.ByClientJwt, true)
		requirePairable(t, *reissued.ByClientJwt, networkId, true)
	})
}

// A client whose owner row has no admin_refused mark carries no claim, not a false one: a legacy device, the child it
// mints with no marks, and that child's refresh, none of which pairs. The claim is the row's alone: a caller whose own
// token says admin_refused mints a client with no mark, and that client's token carries none.
func TestNonRefusedClientTokenOmitsAdminRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, root := newAdminRefusedClaimNetwork(t, ctx, "admin-refused-omitted", session.ByJwt{})

		device, deviceSession := mintAdminRefusedClaimClient(t, ctx, root, &model.AuthNetworkClientArgs{
			Description: "device", DeviceSpec: "synthetic",
		})
		requireAdminRefusedClaim(t, ctx, *device.ByClientJwt, false)

		child, childSession := mintAdminRefusedClaimClient(t, ctx, deviceSession, &model.AuthNetworkClientArgs{
			SourceClientId: device.ClientId, Description: "window", DeviceSpec: "synthetic",
		})
		if child.AdminRefused {
			t.Fatal("an unmarked child's reply confirms admin_refused")
		}
		requireAdminRefusedClaim(t, ctx, *child.ByClientJwt, false)
		requirePairable(t, *child.ByClientJwt, networkId, false)

		refreshed, err := RefreshToken(childSession)
		if err != nil || refreshed.Error != nil {
			t.Fatalf("refresh: %v %+v", err, refreshed)
		}
		requireAdminRefusedClaim(t, ctx, refreshed.ByJwt, false)
		requirePairable(t, refreshed.ByJwt, networkId, false)

		// a credential claiming the mark, which api mints for no network credential, mints a client the row leaves
		// unmarked, and the mint answers the row
		_, markedRoot := newAdminRefusedClaimNetwork(t, ctx, "admin-refused-caller", session.ByJwt{AdminRefused: true})
		unmarked, _ := mintAdminRefusedClaimClient(t, ctx, markedRoot, &model.AuthNetworkClientArgs{
			Description: "device", DeviceSpec: "synthetic",
		})
		requireAdminRefusedClaim(t, ctx, *unmarked.ByClientJwt, false)
	})
}
