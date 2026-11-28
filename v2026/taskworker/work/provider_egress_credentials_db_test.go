package work

import (
	"context"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The real model, signature verification and DB lifecycle are the authority.
// An internal transport seam must not mint a different kind of client, refresh
// the root lineage, grant credit, or weaken network-scoped retirement.
func TestProviderEgressInternalCredentialsPreserveModelContract(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		bootstrap := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer bootstrap.Cancel()
		if _, err := model.BootstrapProberIdentity(bootstrap); err != nil {
			t.Fatal(err)
		}
		identity := model.GetProberIdentity(ctx)
		credentials, err := newProviderEgressCredentials(identity)
		if err != nil {
			t.Fatal(err)
		}
		parent, err := session.ParseByJwtForAudience(ctx, identity.ByClientJwt, session.ByJwtAudienceApi)
		if err != nil {
			t.Fatal(err)
		}
		defer session.Testing_SetRejectExpired(true)()
		for _, invalid := range []string{"signature", "audience", "expired"} {
			badIdentity := *identity
			badClaims := *parent
			switch invalid {
			case "signature":
				badIdentity.ByClientJwt += "synthetic-tamper"
			case "audience":
				badClaims.Audience = gojwt.ClaimStrings{"urnetwork:synthetic-wrong"}
				badIdentity.ByClientJwt = badClaims.Testing_Sign()
			case "expired":
				badClaims.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
				badIdentity.ByClientJwt = badClaims.Testing_Sign()
			}
			badCredentials, err := newProviderEgressCredentials(&badIdentity)
			if err != nil {
				t.Fatal(err)
			}
			source := connect.Id(*identity.ClientId)
			if _, err := badCredentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source}); err == nil {
				t.Fatalf("%s parent bypassed normal JWT policy", invalid)
			}
		}
		var unexpectedChildren int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_client WHERE source_client_id = $1`, *identity.ClientId).Scan(&unexpectedChildren))
		})
		if unexpectedChildren != 0 {
			t.Fatal("rejected parent credentials created derived identities")
		}
		var balanceBefore int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(SUM(balance_byte_count), 0) FROM transfer_balance WHERE network_id = $1`, *identity.NetworkId).Scan(&balanceBefore))
		})
		source := connect.Id(*identity.ClientId)
		minted, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source})
		if err != nil || minted == nil {
			t.Fatalf("internal model mint failed: %v", err)
		}
		child, err := session.ParseByJwtForAudience(ctx, minted.ByClientJwt, session.ByJwtAudienceConnect)
		if err != nil {
			t.Fatal("derived credential failed normal Connect signature/audience validation")
		}
		if child.ClientId == nil || *child.ClientId == *identity.ClientId || child.DeviceId == nil || *child.DeviceId != *parent.DeviceId ||
			child.NetworkId != parent.NetworkId || child.UserId != parent.UserId || !child.CreateTime.Equal(parent.CreateTime) ||
			child.Principal != parent.Principal || len(child.Roles) != len(parent.Roles) || child.ExpiresAt == nil || child.IssuedAt == nil ||
			child.ExpiresAt.Time.Sub(child.IssuedAt.Time) != parent.ExpiresAt.Time.Sub(parent.IssuedAt.Time) {
			t.Fatal("derived identity, device, lineage, roles, or normal token lifetime changed")
		}
		if _, err := session.ParseByJwtForAudience(ctx, minted.ByClientJwt, session.ByJwtAudienceApi); err != nil {
			t.Fatal("derived credential lost its ordinary API audience")
		}
		if err := session.ValidateByJwtState(ctx, child, true); err != nil {
			t.Fatal("minted child did not become a normal active network client")
		}
		var childSource, childNetwork, childDevice server.Id
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT source_client_id, network_id, device_id FROM network_client WHERE client_id = $1`, *child.ClientId).Scan(&childSource, &childNetwork, &childDevice))
		})
		if childSource != *identity.ClientId || childNetwork != *identity.NetworkId || childDevice != *parent.DeviceId {
			t.Fatal("model did not persist the original prober source/device/network")
		}
		foreign := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, description) VALUES ($1, $2, $3, 'synthetic foreign client')`, foreign, server.NewId(), server.NewId()))
		})
		if _, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(foreign)}); err == nil {
			t.Fatal("internal retirement revoked a foreign-network client")
		}
		if _, err := credentials.RemoveNetworkClient(ctx, &connect.RemoveNetworkClientArgs{ClientId: connect.Id(*child.ClientId)}); err != nil {
			t.Fatal(err)
		}
		if err := session.ValidateByJwtState(ctx, child, true); err == nil {
			t.Fatal("retired child credential remained valid")
		}
		if err := session.ValidateByJwtState(ctx, parent, true); err != nil {
			t.Fatal("derived retirement revoked the durable prober parent")
		}
		var balanceAfter int64
		var foreignActive bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COALESCE(SUM(balance_byte_count), 0) FROM transfer_balance WHERE network_id = $1`, *identity.NetworkId).Scan(&balanceAfter))
			server.Raise(conn.QueryRow(ctx, `SELECT active FROM network_client WHERE client_id = $1`, foreign).Scan(&foreignActive))
		})
		if balanceAfter != balanceBefore || !foreignActive {
			t.Fatal("credential transport changed credit or another network's lifecycle")
		}
		// The stored token must stop authorizing new children immediately when
		// normal parent state validation fails; internal ownership is no bypass.
		if _, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: *identity.ClientId}, &session.ClientSession{Ctx: ctx, ByJwt: parent}); err != nil {
			t.Fatal(err)
		}
		if _, err := credentials.AuthNetworkClient(ctx, &connect.AuthNetworkClientArgs{SourceClientId: &source}); err == nil {
			t.Fatal("revoked prober parent minted a new derived client")
		}
	})
}
