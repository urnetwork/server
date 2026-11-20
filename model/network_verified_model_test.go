package model

// network_verified (§21.7, owner decision 96, plan-ABUSE-small-readings #19; ABUSE-M5-07) on the private stack, on
// both legs, since it reads only tables that exist today: each source true and false, an expired paid balance false
// with no writer once the two windows pass, guest, wallet and seed phrase sign-ins never counting, the version order
// in both tiers, and the writers' refresh.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A network whose admin user has no sign-in, created a month before now: its id and its admin's.
func testVerifiedNetwork(t testing.TB, ctx context.Context, now time.Time) (networkId server.Id, adminUserId server.Id) {
	t.Helper()
	networkId = server.NewId()
	adminUserId = server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_user (user_id, user_name, auth_type) VALUES ($1, $2, $3)`,
			adminUserId,
			"verified",
			AuthTypeGuest,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network (network_id, network_name, admin_user_id, create_time)
				VALUES ($1, $2, $3, $4)
			`,
			networkId,
			"verified-"+networkId.String(),
			adminUserId,
			now.Add(-30*24*time.Hour).UTC(),
		))
	})
	return
}

// A balance for the network in [startTime, endTime), carrying revenue when paid.
func testAddPaidTransferBalance(t testing.TB, ctx context.Context, networkId server.Id, startTime time.Time, endTime time.Time, paid bool) {
	t.Helper()
	netRevenue := NanoCents(0)
	if paid {
		netRevenue = UsdToNanoCents(5)
	}
	AddTransferBalance(ctx, &TransferBalance{
		NetworkId:             networkId,
		StartTime:             startTime.UTC(),
		EndTime:               endTime.UTC(),
		StartBalanceByteCount: 1024 * 1024 * 1024,
		BalanceByteCount:      1024 * 1024 * 1024,
		NetRevenue:            netRevenue,
	})
}

// A password sign-in of authType for the user, verified or not.
func testAddPasswordAuth(t testing.TB, ctx context.Context, userId server.Id, authType UserAuthType, verified bool) {
	t.Helper()
	userAuth := server.NewId().String() + "@verified.example"
	if authType == UserAuthTypePhone {
		// a synthetic phone sign-in: nothing here parses it
		userAuth = "phone-" + server.NewId().String()
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user_auth_password (user_id, user_auth, auth_type, verified)
				VALUES ($1, $2, $3, $4)
			`,
			userId,
			userAuth,
			string(authType),
			verified,
		))
	})
}

// An sso sign-in of authType for the user.
func testAddSsoAuth(t testing.TB, ctx context.Context, userId server.Id, authType AuthType) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO network_user_auth_sso (user_id, auth_type, auth_jwt, user_auth)
				VALUES ($1, $2, $3, $4)
			`,
			userId,
			authType,
			"synthetic",
			server.NewId().String()+"@sso.example",
		))
	})
}

// Each source of network_verified, true and false, from the source of truth and through the cache.
func TestNetworkVerifiedSources(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range []struct {
			name     string
			setup    func(networkId server.Id, adminUserId server.Id, now time.Time)
			verified bool
		}{
			{name: "no sign-in", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {}, verified: false},
			{name: "a verified email", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)
			}, verified: true},
			{name: "an unverified email", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, false)
			}, verified: false},
			{name: "a verified phone", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypePhone, true)
			}, verified: true},
			{name: "an unverified phone", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypePhone, false)
			}, verified: false},
			{name: "an Apple sign-in", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddSsoAuth(t, ctx, adminUserId, AuthTypeApple)
			}, verified: true},
			{name: "a Google sign-in", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddSsoAuth(t, ctx, adminUserId, AuthTypeGoogle)
			}, verified: true},
			{name: "a wallet", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(
						ctx,
						`INSERT INTO network_user_auth_wallet (user_id, wallet_address, blockchain) VALUES ($1, $2, $3)`,
						adminUserId,
						server.NewId().String(),
						AuthTypeSolana,
					))
				})
			}, verified: false},
			{name: "a seed phrase", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(CreateSeedphraseAuthInTx(tx, ctx, adminUserId, "synthetic seed "+server.NewId().String()))
				})
			}, verified: false},
			{name: "a paid balance in window", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPaidTransferBalance(t, ctx, networkId, now.Add(-time.Hour), now.Add(time.Hour), true)
			}, verified: true},
			{name: "a paid balance ended", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPaidTransferBalance(t, ctx, networkId, now.Add(-2*time.Hour), now.Add(-time.Hour), true)
			}, verified: false},
			{name: "a paid balance not yet begun", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPaidTransferBalance(t, ctx, networkId, now.Add(time.Hour), now.Add(2*time.Hour), true)
			}, verified: false},
			{name: "an unpaid balance in window", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPaidTransferBalance(t, ctx, networkId, now.Add(-time.Hour), now.Add(time.Hour), false)
			}, verified: false},
			{name: "another user's verified email", setup: func(networkId server.Id, adminUserId server.Id, now time.Time) {
				testAddPasswordAuth(t, ctx, server.NewId(), UserAuthTypeEmail, true)
			}, verified: false},
		} {
			now := server.NowUtc()
			networkId, adminUserId := testVerifiedNetwork(t, ctx, now)
			c.setup(networkId, adminUserId, now)
			var loaded networkVerifiedEntitlement
			server.Db(ctx, func(conn server.PgConn) {
				loaded = loadNetworkVerifiedWithConn(ctx, conn, networkId, now)
			})
			if loaded.verified != c.verified || loaded.version == 0 {
				t.Errorf("%s: %+v", c.name, loaded)
			}
			if verified := IsNetworkVerified(ctx, networkId); verified != c.verified {
				t.Errorf("%s through the cache: %t", c.name, verified)
			}
		}
		if verified := IsNetworkVerified(ctx, server.NewId()); verified {
			t.Error("a network that does not exist is verified")
		}
	})
}

// A team's admin_user_id is a synthetic owner of record with no sign-in, so a team verifies by payment or a live
// Owner's verified sign-in (§21.7: M6 names a team's Owner; §7.5): an Owner's verified email or Apple sign-in
// verifies it; a JIT pending Owner's, an Admin's, or an Owner's unverified email never does. The cache's load and
// the gate's statement read it alike.
func TestNetworkVerifiedCountsATeamsLiveOwner(t *testing.T) {
	if !server.SidecarMg6Linked() {
		t.Skip("network_member is MG6's: run with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		for _, c := range []struct {
			name     string
			role     NetworkMemberRole
			pending  bool
			signIn   func(userId server.Id)
			verified bool
		}{
			{name: "an Owner's verified email", role: NetworkMemberRoleOwner, signIn: func(userId server.Id) {
				testAddPasswordAuth(t, ctx, userId, UserAuthTypeEmail, true)
			}, verified: true},
			{name: "an Owner's Apple sign-in", role: NetworkMemberRoleOwner, signIn: func(userId server.Id) {
				testAddSsoAuth(t, ctx, userId, AuthTypeApple)
			}, verified: true},
			{name: "an Owner's unverified email", role: NetworkMemberRoleOwner, signIn: func(userId server.Id) {
				testAddPasswordAuth(t, ctx, userId, UserAuthTypeEmail, false)
			}, verified: false},
			{name: "a pending Owner's verified email", role: NetworkMemberRoleOwner, pending: true, signIn: func(userId server.Id) {
				testAddPasswordAuth(t, ctx, userId, UserAuthTypeEmail, true)
			}, verified: false},
			{name: "an Admin's verified email", role: NetworkMemberRoleAdmin, signIn: func(userId server.Id) {
				testAddPasswordAuth(t, ctx, userId, UserAuthTypeEmail, true)
			}, verified: false},
		} {
			now := server.NowUtc()
			// the team's owner of record: no sign-in
			networkId, _ := testVerifiedNetwork(t, ctx, now)
			memberUserId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_team (network_id) VALUES ($1)`, networkId))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_user (user_id, user_name, auth_type) VALUES ($1, $2, $3)`,
					memberUserId,
					"member",
					AuthTypePassword,
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_member (member_id, network_id, user_id, role, pending) VALUES ($1, $2, $3, $4, $5)`,
					server.NewId(),
					networkId,
					memberUserId,
					string(c.role),
					c.pending,
				))
			})
			c.signIn(memberUserId)
			var loaded networkVerifiedEntitlement
			var networkIdFacts map[server.Id]*networkAbuseFacts
			server.Db(ctx, func(conn server.PgConn) {
				loaded = loadNetworkVerifiedWithConn(ctx, conn, networkId, now)
				networkIdFacts = loadNetworkAbuseFactsWithConn(ctx, conn, []server.Id{networkId}, now, true)
			})
			if loaded.verified != c.verified {
				t.Errorf("%s: the cache's load read %t", c.name, loaded.verified)
			}
			if facts := networkIdFacts[networkId]; facts == nil || facts.verified != c.verified {
				t.Errorf("%s: the gate's statement read %+v", c.name, facts)
			}
		}
	})
}

// A paid balance's window ends with no writer: past it the source reads false, the in-process entry is gone after
// LocalCacheTtl, and the Redis entry lives at most CacheTtl, past which a read loads false.
func TestNetworkVerifiedExpiredBalanceWithNoWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		clock := &testAbuseClock{now: start}
		settings := DefaultNetworkVerifiedSettings()
		cache := newNetworkVerifiedCache(settings, clock.Now)
		networkId, _ := testVerifiedNetwork(t, ctx, start)
		testAddPaidTransferBalance(t, ctx, networkId, start.Add(-time.Hour), start.Add(time.Hour), true)

		if !cache.isVerified(ctx, networkId) {
			t.Fatal("a paid balance in window is not verified")
		}
		var pttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			pttl, err = r.PTTL(ctx, networkVerifiedKey(networkId)).Result()
			server.Raise(err)
		})
		if pttl <= 0 || settings.CacheTtl < pttl {
			t.Fatalf("the Redis entry lives %s", pttl)
		}

		// the window ends, and nothing writes
		clock.Advance(2 * time.Hour)
		var loaded networkVerifiedEntitlement
		server.Db(ctx, func(conn server.PgConn) {
			loaded = loadNetworkVerifiedWithConn(ctx, conn, networkId, clock.Now())
		})
		if loaded.verified {
			t.Fatal("an ended balance still verifies")
		}
		if _, ok := cache.getLocal(networkId); ok {
			t.Fatal("the in-process entry outlived LocalCacheTtl")
		}
		// what the Redis entry's ttl does on its own, at most CacheTtl from now
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, networkVerifiedKey(networkId)).Err())
		})
		if cache.isVerified(ctx, networkId) {
			t.Fatal("past both windows an ended balance read verified")
		}
	})
}

// Neither tier takes an older read over a newer one.
func TestNetworkVerifiedVersionOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cache := newNetworkVerifiedCache(nil, nil)
		networkId := server.NewId()
		newer := networkVerifiedEntitlement{verified: true, version: 2000}
		older := networkVerifiedEntitlement{verified: false, version: 1000}

		cache.store(ctx, networkId, newer)
		cache.store(ctx, networkId, older)
		if local, ok := cache.getLocal(networkId); !ok || local != newer {
			t.Fatalf("in process: %+v %t", local, ok)
		}
		if cached, ok := cache.getCached(ctx, networkId); !ok || cached != newer {
			t.Fatalf("in Redis: %+v %t", cached, ok)
		}
		// a newer read replaces both
		newest := networkVerifiedEntitlement{verified: false, version: 3000}
		cache.store(ctx, networkId, newest)
		if local, _ := cache.getLocal(networkId); local != newest {
			t.Fatalf("in process: %+v", local)
		}
		if cached, _ := cache.getCached(ctx, networkId); cached != newest {
			t.Fatalf("in Redis: %+v", cached)
		}
		for _, value := range []string{"1", "2:5", "1:x", "true:5", ""} {
			if _, ok := parseNetworkVerifiedEntitlement(value); ok {
				t.Errorf("%q parsed", value)
			}
		}
	})
}

// UpdateNetworkVerified refreshes both tiers at once, where a read through the cache keeps its old answer until then.
func TestUpdateNetworkVerifiedRefreshesBothTiers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, adminUserId := testVerifiedNetwork(t, ctx, server.NowUtc())
		if IsNetworkVerified(ctx, networkId) {
			t.Fatal("verified with no sign-in")
		}
		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)
		if IsNetworkVerified(ctx, networkId) {
			t.Fatal("the cache answered before its writer")
		}
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("the writer's refresh read unverified")
		}
		localVerified, localOk, cachedVerified, cachedOk := Testing_NetworkVerifiedCacheEntries(ctx, networkId)
		if !localVerified || !localOk || !cachedVerified || !cachedOk {
			t.Fatalf("tiers: %t %t %t %t", localVerified, localOk, cachedVerified, cachedOk)
		}
		if !IsNetworkVerified(ctx, networkId) {
			t.Fatal("a read after the refresh")
		}
	})
}

// UpdateProNetwork, which every payment path and every sign-in's IsProFresh runs after its commit, refreshes
// network_verified where MG5 is linked, and leaves it alone before M5, when nothing reads it.
func TestUpdateProNetworkRefreshesNetworkVerified(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, adminUserId := testVerifiedNetwork(t, ctx, server.NowUtc())
		if IsNetworkVerified(ctx, networkId) {
			t.Fatal("verified with no sign-in")
		}
		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)
		UpdateProNetwork(ctx, networkId)
		localVerified, localOk, cachedVerified, cachedOk := Testing_NetworkVerifiedCacheEntries(ctx, networkId)
		if server.SidecarMg5Linked() {
			if !localVerified || !localOk || !cachedVerified || !cachedOk {
				t.Fatalf("with MG5: %t %t %t %t", localVerified, localOk, cachedVerified, cachedOk)
			}
		} else if localVerified || cachedVerified {
			t.Fatalf("without MG5 the hook ran: %t %t", localVerified, cachedVerified)
		}
	})
}

// The gate's one statement for many networks, on both legs: each network's creation, no row while none is written
// (and none can be, without MG5), and network_verified beside them; a network that does not exist has no entry. The
// standing it derives is probation or standard by age alone.
func TestNetworkAbuseFactsLoadOnEveryLeg(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		verifiedId, verifiedAdminId := testVerifiedNetwork(t, ctx, now)
		testAddPasswordAuth(t, ctx, verifiedAdminId, UserAuthTypeEmail, true)
		unverifiedId, _ := testVerifiedNetwork(t, ctx, now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET create_time = $2 WHERE network_id = $1`, unverifiedId, now.Add(-2*24*time.Hour)))
		})
		missingId := server.NewId()

		var networkIdFacts map[server.Id]*networkAbuseFacts
		server.Db(ctx, func(conn server.PgConn) {
			networkIdFacts = loadNetworkAbuseFactsWithConn(ctx, conn, []server.Id{verifiedId, unverifiedId, missingId}, now, true)
		})
		if len(networkIdFacts) != 2 {
			t.Fatalf("%d networks loaded", len(networkIdFacts))
		}
		if facts := networkIdFacts[verifiedId]; facts == nil || !facts.verified || facts.row != nil || facts.version == 0 ||
			!facts.createTime.Equal(now.Add(-30*24*time.Hour)) {
			t.Fatalf("the verified network: %+v", facts)
		}
		if facts := networkIdFacts[unverifiedId]; facts == nil || facts.verified || facts.row != nil {
			t.Fatalf("the unverified network: %+v", facts)
		}
		if standing, found := GetNetworkAbuseStanding(ctx, verifiedId, now); !found || standing.Standing != AbuseStandingStandard {
			t.Fatalf("a month old: %+v %t", standing, found)
		}
		if standing, found := GetNetworkAbuseStanding(ctx, unverifiedId, now); !found || standing.Standing != AbuseStandingProbation {
			t.Fatalf("two days old: %+v %t", standing, found)
		}
		if _, found := GetNetworkAbuseStanding(ctx, missingId, now); found {
			t.Fatal("a network that does not exist was found")
		}
	})
}
