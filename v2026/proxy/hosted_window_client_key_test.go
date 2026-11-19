package proxy

// A hosted window client's identity key has no reader, so a hosted device keeps
// it local. Each candidate a discovery window builds is a new client with a new
// key, and each published key was a stored signed registration: on 2026-11-02
// hosted window clients were 70-94% of st_client_key_history's inserts, and the
// rows were kept whether or not the candidate ever got a contract.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The hosted-device policy asks for it, whatever else the settings say.
func TestProxyDeviceKeepsWindowClientKeysLocal(t *testing.T) {
	settings := DefaultProxyDeviceSettings()
	settings.DisableWindowIdentityPersistence = true
	local := newProxyDeviceLocalSettings(context.Background(), &model.ProxyDeviceConfig{}, settings)
	if !local.KeepWindowClientKeysLocal {
		t.Fatal("a hosted device's window clients publish keys nothing reads")
	}
}

// Over the full harness: a hosted device's window clients carry traffic through
// a real provider, and the platform holds no key for any of them, signed or
// legacy.
func TestProxyHostedWindowClientsCarryTrafficWithoutPublishingAKey(t *testing.T) {
	if testing.Short() {
		return
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		h := setupProxyTargetTest(t)
		defer h.close(t)

		var identities []*model.ProxyWindowClientIdentity
		waitFor(t, 30*time.Second, "window identity snapshot persisted", func() bool {
			identities = model.GetProxyWindowIdentities(h.ctx, h.proxyId)
			return 0 < len(identities)
		})

		// the clients have registered and carry traffic. A published key would
		// already be committed: it is sent as soon as the client is ready,
		// before its first contract.
		testProxyHttps(t, h)

		for _, identity := range identities {
			publicKey, err := model.GetClientPublicKey(h.ctx, identity.ClientId)
			if err != nil {
				t.Fatalf("read window client key: %v", err)
			}
			if 0 < len(publicKey) {
				t.Fatal("a hosted window client published a key nothing reads")
			}
			var rows int
			server.Db(h.ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(h.ctx, `SELECT COUNT(*) FROM st_client_key_history WHERE client_id = $1`, identity.ClientId).Scan(&rows))
			})
			if rows != 0 {
				t.Fatalf("a hosted window client wrote %d signed key registrations", rows)
			}
		}
	})
}
