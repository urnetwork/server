package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The admin_refused claim (§7.4; owner decision 291): the mint sets it from the
// client's owner row, every derivation and signing keeps it, and a token whose
// client has no mark carries none. Pure: tokens are signed with the local vault
// key, and nothing here reads a store.

// A signed token's claims as a holder that did not mint it reads them: the
// payload, unverified.
func adminRefusedTestClaims(t testing.TB, signed string) map[string]any {
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

// Requires the signed token to carry admin_refused as a JSON true and parse
// back with the mark, or to carry no such claim at all.
func requireAdminRefusedClaim(t testing.TB, signed string, adminRefused bool) {
	t.Helper()
	claim, present := adminRefusedTestClaims(t, signed)["admin_refused"]
	if adminRefused && claim != true || !adminRefused && present {
		t.Fatalf("admin_refused claim %v (present %t), want %t", claim, present, adminRefused)
	}
	parsed, err := ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceApi)
	if err != nil {
		t.Fatal(err)
	}
	connect.AssertEqual(t, parsed.AdminRefused, adminRefused)
}

// A marked credential keeps the claim through a refresh's Renew, a child's
// Client, and signing.
// The mark sets no lifetime: an app device client is admin_refused and keeps
// today's 30 days (owner decision 131), and a sidecar's has strict_exp beside
// it, whose 7 days it keeps.
func TestAdminRefusedClaimIsKeptByEveryDerivation(t *testing.T) {
	network := NewByJwt(server.NewId(), server.NewId(), "admin-refused-test", false, false)
	device := network.Client(server.NewId(), server.NewId())
	device.AdminRefused = true
	requireExpiryCloseTo(t, device, 30*24*time.Hour)
	requireAdminRefusedClaim(t, device.Testing_Sign(), true)

	renewed := device.Renew()
	connect.AssertEqual(t, renewed.AdminRefused, true)
	requireExpiryCloseTo(t, renewed, 30*24*time.Hour)
	requireAdminRefusedClaim(t, renewed.Testing_Sign(), true)

	child := device.Client(server.NewId(), server.NewId())
	connect.AssertEqual(t, child.AdminRefused, true)
	requireAdminRefusedClaim(t, child.Testing_Sign(), true)

	sidecarClient := network.Client(server.NewId(), server.NewId())
	sidecarClient.SetStrictExp(true)
	sidecarClient.AdminRefused = true
	requireExpiryCloseTo(t, sidecarClient, 7*24*time.Hour)
	signed := sidecarClient.Testing_Sign()
	requireAdminRefusedClaim(t, signed, true)
	connect.AssertEqual(t, ByJwtStrictExpUnverified(signed), true)
	renewedSidecar := sidecarClient.Renew()
	connect.AssertEqual(t, renewedSidecar.AdminRefused && renewedSidecar.StrictExp, true)
	requireExpiryCloseTo(t, renewedSidecar, 7*24*time.Hour)
}

// A client with no mark carries no claim at all, not a false one, and no
// derivation adds it: the network's own token, its client and that client's
// refresh and child.
func TestUnmarkedTokenCarriesNoAdminRefusedClaim(t *testing.T) {
	network := NewByJwt(server.NewId(), server.NewId(), "admin-refused-test", false, false)
	requireAdminRefusedClaim(t, network.Testing_Sign(), false)
	client := network.Client(server.NewId(), server.NewId())
	requireAdminRefusedClaim(t, client.Testing_Sign(), false)
	requireAdminRefusedClaim(t, client.Renew().Testing_Sign(), false)
	requireAdminRefusedClaim(t, client.Client(server.NewId(), server.NewId()).Testing_Sign(), false)

	// strict_exp alone is not the claim: the mint sets each from the row
	client.SetStrictExp(true)
	requireAdminRefusedClaim(t, client.Testing_Sign(), false)
}
