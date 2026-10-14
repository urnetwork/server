package model

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Tests of the lag.dev guest networks (lag.dev/PLAN.md A1, A2) and the lag
// rate limits.

// A lag guest with its one client, as the guest session creates it.
func testCreateLagGuestWithClient(t testing.TB, ctx context.Context, createdAt time.Time, expiresAt time.Time) (*LagGuest, *session.ByJwt) {
	t.Helper()
	guest := CreateLagGuestNetwork(ctx, createdAt, expiresAt)
	byJwt := session.NewByJwt(guest.NetworkId, guest.UserId, guest.NetworkName, false, false)
	authClient, err := AuthNetworkClient(
		&AuthNetworkClientArgs{Description: "lag.dev", DeviceSpec: "lag.dev"},
		session.Testing_CreateClientSession(ctx, byJwt),
	)
	connect.AssertEqual(t, err, nil)
	if authClient.Error != nil {
		t.Fatalf("the guest's client was refused: %s", authClient.Error.Message)
	}
	clientByJwt, err := session.ParseByJwt(ctx, *authClient.ByClientJwt)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, *clientByJwt.ClientId, *authClient.ClientId)
	return guest, clientByJwt
}

func testNetworkExists(t testing.TB, ctx context.Context, networkId server.Id) (networkExists bool, adminUserExists bool) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
			SELECT
				EXISTS (SELECT 1 FROM network WHERE network_id = $1),
				EXISTS (
					SELECT 1
					FROM network_user
					WHERE user_id = (SELECT admin_user_id FROM network WHERE network_id = $1)
				)
			`,
			networkId,
		).Scan(&networkExists, &adminUserExists))
	})
	return
}

func testActiveClientCount(t testing.TB, ctx context.Context, networkId server.Id) (active int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT COUNT(*) FROM network_client WHERE network_id = $1 AND active`,
			networkId,
		).Scan(&active))
	})
	return
}

// A lag guest is a guest-shaped network with its expiry, and its client's
// token is an ordinary client token while the guest lives.
func TestLagGuestNetworkCreate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		createdAt := server.NowUtc().Truncate(time.Second)
		expiresAt := createdAt.Add(24 * time.Hour)
		guest, clientByJwt := testCreateLagGuestWithClient(t, ctx, createdAt, expiresAt)

		connect.AssertEqual(t, guest.NetworkName, "lag-"+guest.NetworkId.String())
		stored := GetLagGuest(ctx, guest.NetworkId)
		connect.AssertEqual(t, stored.UserId, guest.UserId)
		connect.AssertEqual(t, stored.NetworkName, guest.NetworkName)
		connect.AssertEqual(t, stored.CreatedAt.Equal(createdAt), true)
		connect.AssertEqual(t, stored.ExpiresAt.Equal(expiresAt), true)

		var authType string
		var authMethods bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT auth_type FROM network_user WHERE user_id = $1`, guest.UserId).Scan(&authType))
		})
		authMethods = HasAnyAuthMethod(ctx, guest.UserId)
		connect.AssertEqual(t, authType, string(AuthTypeGuest))
		connect.AssertEqual(t, authMethods, false)

		connect.AssertEqual(t, session.ValidateByJwtState(ctx, clientByJwt, true), nil)
		connect.AssertEqual(t, testActiveClientCount(t, ctx, guest.NetworkId), 1)

		// an unmarked network is no lag guest
		connect.AssertEqual(t, GetLagGuest(ctx, server.NewId()), (*LagGuest)(nil))
	})
}

// The sweep removes a lag guest once its expiry passes, with its clients, so
// its token fails ValidateByJwtState; the apps' guest networks are never
// touched, and a guest not yet expired stays.
func TestRemoveExpiredLagGuests(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		expired, expiredClient := testCreateLagGuestWithClient(t, ctx, now, now.Add(24*time.Hour))
		later, laterClient := testCreateLagGuestWithClient(t, ctx, now, now.Add(25*time.Hour))

		appNetworkId := server.NewId()
		appUserId := server.NewId()
		Testing_CreateGuestNetwork(ctx, appNetworkId, "app-guest-"+appNetworkId.String()[:8], appUserId)
		legacyNetworkId := server.NewId()
		Testing_CreateLegacyGuestNetwork(ctx, legacyNetworkId, server.NewId())

		// nothing is due a second before the expiry
		result := RemoveExpiredLagGuests(ctx, now.Add(24*time.Hour-time.Second), 100)
		connect.AssertEqual(t, result, RemoveExpiredLagGuestsResult{})

		result = RemoveExpiredLagGuests(ctx, now.Add(24*time.Hour), 100)
		connect.AssertEqual(t, result, RemoveExpiredLagGuestsResult{Removed: 1})

		networkExists, adminUserExists := testNetworkExists(t, ctx, expired.NetworkId)
		connect.AssertEqual(t, networkExists, false)
		connect.AssertEqual(t, adminUserExists, false)
		var userExists bool
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM network_user WHERE user_id = $1)`, expired.UserId).Scan(&userExists))
		})
		connect.AssertEqual(t, userExists, false)
		connect.AssertEqual(t, testActiveClientCount(t, ctx, expired.NetworkId), 0)
		connect.AssertEqual(t, GetLagGuest(ctx, expired.NetworkId), (*LagGuest)(nil))
		connect.AssertNotEqual(t, session.ValidateByJwtState(ctx, expiredClient, true), nil)

		// the guest that expires later still works
		connect.AssertEqual(t, session.ValidateByJwtState(ctx, laterClient, true), nil)
		connect.AssertNotEqual(t, GetLagGuest(ctx, later.NetworkId), (*LagGuest)(nil))

		// long past every expiry, the apps' guests are still there
		result = RemoveExpiredLagGuests(ctx, now.Add(365*24*time.Hour), 100)
		connect.AssertEqual(t, result, RemoveExpiredLagGuestsResult{Removed: 1})
		for _, networkId := range []server.Id{appNetworkId, legacyNetworkId} {
			networkExists, adminUserExists := testNetworkExists(t, ctx, networkId)
			connect.AssertEqual(t, networkExists, true)
			connect.AssertEqual(t, adminUserExists, true)
		}
		connect.AssertNotEqual(t, session.ValidateByJwtState(ctx, laterClient, true), nil)
	})
}

// A batch removes at most its limit, oldest expiry first, and a guest whose
// network was already deleted leaves only its mark, which the sweep removes.
func TestRemoveExpiredLagGuestsBatches(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		guests := []*LagGuest{}
		for i := range 3 {
			guests = append(guests, CreateLagGuestNetwork(ctx, now, now.Add(time.Duration(i+1)*time.Minute)))
		}
		// the newest guest deleted its own account
		removed, _ := RemoveNetwork(ctx, guests[2].NetworkId, &guests[2].UserId)
		connect.AssertEqual(t, removed, true)

		connect.AssertEqual(t, RemoveExpiredLagGuests(ctx, now.Add(time.Hour), 2), RemoveExpiredLagGuestsResult{Removed: 2})
		connect.AssertEqual(t, GetLagGuest(ctx, guests[0].NetworkId), (*LagGuest)(nil))
		connect.AssertEqual(t, GetLagGuest(ctx, guests[1].NetworkId), (*LagGuest)(nil))
		connect.AssertNotEqual(t, GetLagGuest(ctx, guests[2].NetworkId), (*LagGuest)(nil))

		connect.AssertEqual(t, RemoveExpiredLagGuests(ctx, now.Add(time.Hour), 2), RemoveExpiredLagGuestsResult{Removed: 1})
		connect.AssertEqual(t, GetLagGuest(ctx, guests[2].NetworkId), (*LagGuest)(nil))
	})
}

// A lag guest someone paid for is kept and stops being a lag guest, as an
// account deletion refuses an active renewal.
func TestRemoveExpiredLagGuestsKeepsAPaidGuest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		paid, paidClient := testCreateLagGuestWithClient(t, ctx, now, now.Add(time.Minute))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO transfer_balance (
					balance_id,
					network_id,
					start_time,
					end_time,
					start_balance_byte_count,
					net_revenue_nano_cents,
					balance_byte_count
				)
				VALUES ($1, $2, $3, $4, 1024, 1000000, 1024)
				`,
				server.NewId(),
				paid.NetworkId,
				now,
				now.Add(30*24*time.Hour),
			))
		})

		result := RemoveExpiredLagGuests(ctx, now.Add(time.Hour), 100)
		connect.AssertEqual(t, result, RemoveExpiredLagGuestsResult{Kept: 1})
		connect.AssertEqual(t, GetLagGuest(ctx, paid.NetworkId), (*LagGuest)(nil))
		networkExists, _ := testNetworkExists(t, ctx, paid.NetworkId)
		connect.AssertEqual(t, networkExists, true)
		connect.AssertEqual(t, session.ValidateByJwtState(ctx, paidClient, true), nil)
	})
}

// The lag limits count a whole /24 (/48), separately per name, and refuse
// past the limit with the rest of the window.
func TestLagRateLimits(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		window := time.Hour
		name := "test" + server.NewId().String()[:8]

		for i := range 3 {
			// two addresses of one /24 share the budget
			addr := netip.MustParseAddr([]string{"198.51.100.1", "198.51.100.200", "198.51.100.1"}[i])
			limit, err := CheckLagPrefixRateLimit(ctx, name, addr, 3, window)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, limit.Allowed, true)
		}
		limit, err := CheckLagPrefixRateLimit(ctx, name, netip.MustParseAddr("198.51.100.99"), 3, window)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, limit.Allowed, false)
		if limit.RetryAfter <= 0 || window < limit.RetryAfter {
			t.Fatalf("retry after %s, want within the window", limit.RetryAfter)
		}

		// another /24, and another name, have their own
		limit, err = CheckLagPrefixRateLimit(ctx, name, netip.MustParseAddr("198.51.101.1"), 3, window)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, limit.Allowed, true)
		limit, err = CheckLagPrefixRateLimit(ctx, name+"x", netip.MustParseAddr("198.51.100.1"), 3, window)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, limit.Allowed, true)

		// a /48 is one caller
		for i := range 2 {
			addr := netip.MustParseAddr([]string{"2001:db8:aa:1::1", "2001:db8:aa:ffff::2"}[i])
			limit, err := CheckLagPrefixRateLimit(ctx, name, addr, 2, window)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, limit.Allowed, true)
		}
		limit, err = CheckLagPrefixRateLimit(ctx, name, netip.MustParseAddr("2001:db8:aa::3"), 2, window)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, limit.Allowed, false)

		// per client
		clientId := server.NewId()
		for range 2 {
			limit, err := CheckLagClientRateLimit(ctx, name, clientId, 2, window)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, limit.Allowed, true)
		}
		limit, err = CheckLagClientRateLimit(ctx, name, clientId, 2, window)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, limit.Allowed, false)
		limit, err = CheckLagClientRateLimit(ctx, name, server.NewId(), 2, window)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, limit.Allowed, true)

		// a prefix counter is keyed by the peppered hash alone and is gone a
		// window after its last count (amendment 9)
		key := lagPrefixRateLimitKey(name, netip.MustParseAddr("198.51.100.1"))
		for _, text := range []string{"198.51.100", "c63364"} {
			if strings.Contains(key, text) {
				t.Fatalf("the counter key %q names the address as %q", key, text)
			}
		}
		server.Redis(ctx, func(r server.RedisClient) {
			physicalKeys, err := r.Keys(ctx, key+"_*").Result()
			connect.AssertEqual(t, err, nil)
			if len(physicalKeys) == 0 {
				t.Fatalf("no counter under %q", key)
			}
			for _, physicalKey := range physicalKeys {
				if ttl := r.TTL(ctx, physicalKey).Val(); ttl <= 0 || window < ttl {
					t.Fatalf("counter %q expires in %s, want within %s", physicalKey, ttl, window)
				}
			}
		})
	})
}
