package model

import (
	"context"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Removes the fixture's network as an account deletion does and returns what
// the deletion handed to its scheduler.
func removeDeletedNetworkFixture(t testing.TB, fixture *Testing_DeletedNetworkFixture) *DeletedNetwork {
	t.Helper()
	ctx := t.Context()
	var deletedNetwork *DeletedNetwork
	outcome, _ := RemoveNetworkWithRemovals(
		ctx,
		fixture.NetworkId,
		&fixture.UserId,
		&RemoveNetworkStoreSnapshot{},
		func(tx server.PgTx, scheduledNetwork *DeletedNetwork, userAuths map[string]bool) {
			deletedNetwork = scheduledNetwork
		},
	)
	if outcome != RemoveNetworkRemoved || deletedNetwork == nil {
		t.Fatalf("the deletion = %s with deleted network %v, want removed and handed to the scheduler", outcome, deletedNetwork)
	}
	return deletedNetwork
}

// The deactivate time the database stamped on the client.
func deletedNetworkTestDeactivateTime(t testing.TB, clientId server.Id) time.Time {
	t.Helper()
	ctx := t.Context()
	var deactivateTime time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT deactivate_time FROM network_client WHERE client_id = $1`, clientId).Scan(&deactivateTime))
	})
	return deactivateTime
}

// A deleted account leaves nothing but its financial records: the deletion
// hands its network and admin user to the cleanup in its own transaction; the
// first run deletes the network's and the user's own rows at once and
// deactivates the clients; the run after the client delay deletes the
// clients, every row keyed by them and the device. Before 2026-11-16 the
// deletion removed the network, its users and their sign-ins only, and every
// row below stayed. Another network's rows are never touched.
func TestDeletedNetworkCleanupRemovesEverythingButFinancialRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeletedNetworkCleanupSettings()
		fixture := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-deleted-"+server.NewId().String()[24:])
		live := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-live-"+server.NewId().String()[24:])
		liveRowsBefore := Testing_CountDeletedNetworkFixtureRows(ctx, live)
		keptBefore := Testing_CountDeletedNetworkFixtureKeptRows(ctx, fixture)
		rowsBefore := Testing_CountDeletedNetworkFixtureRows(ctx, fixture)
		// every kind of table holds a fixture row before the deletion
		for _, table := range []string{"network_client", "device", "account_preferences", "account_api_key", "network_referral_code", "network_referral", "network_referral_referee", "network_referral_app_day", "auth_code", "auth_code_role", "user_auth_reset", "network_client_connection", "network_client_location", "provide_key", "client_tls_certificate", "provider_egress_health", "search_provider_stats", "audit_device_state", "audit_device_event", "audit_provider_state", "lag_drift_test", "lag_drift_run", "lag_drift_network"} {
			if rowsBefore[table] == 0 {
				t.Fatalf("the fixture has no %s row", table)
			}
		}

		deletedNetwork := removeDeletedNetworkFixture(t, fixture)
		connect.AssertEqual(t, deletedNetwork.NetworkId, fixture.NetworkId)
		connect.AssertEqual(t, deletedNetwork.UserIds, []server.Id{fixture.UserId})
		// the deletion itself leaves the rows as it always did
		if rows := Testing_CountDeletedNetworkFixtureRows(ctx, fixture); rows["network_client_active"] != 2 || rows["account_preferences"] != 1 {
			t.Fatalf("after the deletion alone: %v", rows)
		}

		// the first run: the network's and the user's rows go, the clients go inactive
		now := server.NowUtc()
		result := RemoveDeletedNetworkData(ctx, deletedNetwork, now, settings)
		if result.NetworkExists || result.Complete || result.More || result.NextClientDueTime == nil {
			t.Fatalf("first run = %+v, want clients waiting for the client delay", result)
		}
		connect.AssertEqual(t, result.RowCounts[DeletedNetworkCleanupDeactivatedClients], int64(2))
		rows := Testing_CountDeletedNetworkFixtureRows(ctx, fixture)
		for table := range rows {
			switch table {
			case "network_client", "device":
			default:
				if slices.ContainsFunc(deletedClientHistoryTables, func(t *deletedNetworkTable) bool { return t.table == table }) ||
					slices.Contains(deletedClientTables, table) ||
					slices.ContainsFunc(deletedDeviceTables, func(t *deletedNetworkTable) bool { return t.table == table }) ||
					table == "network_client_connection" || table == "network_client_latency" || table == "network_client_speed" {
					// keyed by a client or device that waits
					continue
				}
				t.Fatalf("after the first run %s still has %d rows", table, rows[table])
			}
		}
		connect.AssertEqual(t, rows["network_client"], int64(2))
		connect.AssertEqual(t, rows["network_client_active"], int64(0))
		deactivateTime := deletedNetworkTestDeactivateTime(t, fixture.ClientIds[0])
		if !result.NextClientDueTime.Equal(deactivateTime.Add(settings.ClientReapAfterDeactivate)) {
			t.Fatalf("the clients are due at %s, want %s after their deactivation at %s", result.NextClientDueTime, settings.ClientReapAfterDeactivate, deactivateTime)
		}

		// before the delay nothing more goes
		result = RemoveDeletedNetworkData(ctx, deletedNetwork, deactivateTime.Add(settings.ClientReapAfterDeactivate-time.Minute), settings)
		if result.Complete || 0 < result.RowCounts["network_client"] {
			t.Fatalf("a run before the client delay = %+v", result)
		}

		// after the delay: the clients, their rows and the device
		result = RemoveDeletedNetworkData(ctx, deletedNetwork, deactivateTime.Add(settings.ClientReapAfterDeactivate+time.Minute), settings)
		if !result.Complete || result.More {
			t.Fatalf("the run after the client delay = %+v, want complete", result)
		}
		connect.AssertEqual(t, result.RowCounts["network_client"], int64(2))
		connect.AssertEqual(t, result.RowCounts["device"], int64(1))
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureRows(ctx, fixture), map[string]int64{})

		// the financial records stay, and nothing of the live network changed
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureKeptRows(ctx, fixture), keptBefore)
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureRows(ctx, live), liveRowsBefore)

		// a rerun finds nothing
		result = RemoveDeletedNetworkData(ctx, deletedNetwork, deactivateTime.Add(48*time.Hour), settings)
		if !result.Complete || 0 < len(result.RowCounts) {
			t.Fatalf("a rerun = %+v, want complete with nothing deleted", result)
		}
	})
}

// The cleanup touches nothing of a network that exists, nor the rows of a
// user that exists, whatever it is handed.
func TestDeletedNetworkCleanupTouchesNothingThatExists(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeletedNetworkCleanupSettings()
		live := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-live-"+server.NewId().String()[24:])
		rowsBefore := Testing_CountDeletedNetworkFixtureRows(ctx, live)

		result := RemoveDeletedNetworkData(ctx, &DeletedNetwork{NetworkId: live.NetworkId, UserIds: []server.Id{live.UserId}}, server.NowUtc().Add(48*time.Hour), settings)
		if !result.NetworkExists || result.Complete || 0 < len(result.RowCounts) {
			t.Fatalf("a cleanup of a network that exists = %+v", result)
		}
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureRows(ctx, live), rowsBefore)

		// a deleted network handed a user that exists: the user's rows stay
		deleted := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-deleted-"+server.NewId().String()[24:])
		deletedNetwork := removeDeletedNetworkFixture(t, deleted)
		deletedNetwork.UserIds = append(deletedNetwork.UserIds, live.UserId)
		RemoveDeletedNetworkData(ctx, deletedNetwork, server.NowUtc(), settings)
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureRows(ctx, live), rowsBefore)
	})
}

// Every run is safe to stop and repeat: a run that stops at once leaves
// everything for the next; a client another transaction holds is skipped, not
// waited for, and goes in a later run; batches smaller than the network still
// remove it whole; and a run after the last finds nothing.
func TestDeletedNetworkCleanupResumesWhereItStopped(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeletedNetworkCleanupSettings()
		settings.ClientBatchCount = 2
		settings.RowBatchCount = 1
		fixture := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-deleted-"+server.NewId().String()[24:])
		// more children on the device, so the clients take three batches
		server.Tx(ctx, func(tx server.PgTx) {
			for range 3 {
				childClientId := server.NewId()
				fixture.ClientIds = append(fixture.ClientIds, childClientId)
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id)
						VALUES ($1, $2, $3, 'synthetic child', now(), now(), $4)
					`,
					childClientId, fixture.NetworkId, fixture.DeviceIds[0], fixture.ClientIds[0],
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO provide_key_change (client_id, change_time) VALUES ($1, now()), ($1, now() + interval '1 second')`,
					childClientId,
				))
			}
		})
		deletedNetwork := removeDeletedNetworkFixture(t, fixture)
		rowsBefore := Testing_CountDeletedNetworkFixtureRows(ctx, fixture)

		// a run whose context is done stops before its first step
		stoppedCtx, cancel := context.WithCancel(ctx)
		cancel()
		result := RemoveDeletedNetworkData(stoppedCtx, deletedNetwork, server.NowUtc(), settings)
		if !result.More || result.Complete || 0 < len(result.RowCounts) {
			t.Fatalf("a stopped run = %+v, want more to do and nothing done", result)
		}
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureRows(ctx, fixture), rowsBefore)

		result = RemoveDeletedNetworkData(ctx, deletedNetwork, server.NowUtc(), settings)
		connect.AssertEqual(t, result.RowCounts[DeletedNetworkCleanupDeactivatedClients], int64(5))
		dueTime := deletedNetworkTestDeactivateTime(t, fixture.ClientIds[0]).Add(settings.ClientReapAfterDeactivate + time.Minute)

		// another transaction holds one client while the clients are due
		heldClientId := fixture.ClientIds[len(fixture.ClientIds)-1]
		locked := make(chan struct{})
		release := make(chan struct{})
		held := make(chan error, 1)
		go func() {
			held <- func() (returnErr error) {
				defer func() {
					if r := recover(); r != nil {
						returnErr = r.(error)
					}
				}()
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT 1 FROM network_client WHERE client_id = $1 FOR UPDATE`, heldClientId))
					close(locked)
					<-release
				})
				return nil
			}()
		}()
		<-locked
		result = RemoveDeletedNetworkData(ctx, deletedNetwork, dueTime, settings)
		close(release)
		if err := <-held; err != nil {
			t.Fatal(err)
		}
		if result.Complete || result.NextClientDueTime == nil {
			t.Fatalf("a run with a held client = %+v, want the held client left", result)
		}
		connect.AssertEqual(t, result.RowCounts["network_client"], int64(4))
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureRows(ctx, fixture)["network_client"], int64(1))

		result = RemoveDeletedNetworkData(ctx, deletedNetwork, dueTime, settings)
		if !result.Complete {
			t.Fatalf("the run after the hold = %+v, want complete", result)
		}
		connect.AssertEqual(t, result.RowCounts["network_client"], int64(1))
		connect.AssertEqual(t, Testing_CountDeletedNetworkFixtureRows(ctx, fixture), map[string]int64{})

		result = RemoveDeletedNetworkData(ctx, deletedNetwork, dueTime, settings)
		if !result.Complete || 0 < len(result.RowCounts) {
			t.Fatalf("a rerun = %+v, want complete with nothing deleted", result)
		}
	})
}

// A hosted proxy of a deleted network goes with its client: its device
// configuration, its rows on the proxy host and their change log.
func TestDeletedNetworkCleanupRemovesHostedProxies(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeletedNetworkCleanupSettings()
		// the proxy's network has no network row: it is a deleted one
		proxy := newHostedProxyReapTestProxy(t)
		proxyRows := func() (count int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`
						SELECT
							(SELECT count(*) FROM proxy_device_config WHERE proxy_id = $1) +
							(SELECT count(*) FROM proxy_client WHERE proxy_id = $1) +
							(SELECT count(*) FROM proxy_client_change WHERE proxy_id = $1)
					`,
					proxy.proxyId,
				).Scan(&count))
			})
			return
		}
		if proxyRows() < 2 {
			t.Fatal("the proxy fixture has no configuration and proxy client")
		}
		deletedNetwork := &DeletedNetwork{NetworkId: proxy.networkId}
		result := RemoveDeletedNetworkData(ctx, deletedNetwork, server.NowUtc(), settings)
		connect.AssertEqual(t, result.RowCounts[DeletedNetworkCleanupDeactivatedClients], int64(1))
		dueTime := deletedNetworkTestDeactivateTime(t, proxy.clientId).Add(settings.ClientReapAfterDeactivate + time.Minute)
		result = RemoveDeletedNetworkData(ctx, deletedNetwork, dueTime, settings)
		if !result.Complete {
			t.Fatalf("the run after the client delay = %+v, want complete", result)
		}
		connect.AssertEqual(t, proxyRows(), int64(0))
		connect.AssertEqual(t, retiredHostedTestClientExists(t, proxy.clientId), false)
	})
}

// The discovery of deleted networks finds every network id that a table the
// cleanup deletes from still names without a network row, through each kind
// of anchor (an indexed one paged a distinct id at a time, and a small one),
// and never a network that exists.
func TestDeletedNetworkDiscoveryFindsOnlyGoneNetworks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		live := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-live-"+server.NewId().String()[24:])
		deleted := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-deleted-"+server.NewId().String()[24:])
		removeDeletedNetworkFixture(t, deleted)
		// a network that left only a client, only preferences, only an
		// outbox message (a small anchor)
		clientOnlyNetworkId := server.NewId()
		Testing_CreateDevice(ctx, clientOnlyNetworkId, server.NewId(), server.NewId(), "synthetic", "synthetic")
		preferencesOnlyNetworkId := server.NewId()
		outboxOnlyNetworkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_preferences (network_id) VALUES ($1)`, preferencesOnlyNetworkId))
			messageId := server.NewId()
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO account_message_outbox (message_id, message_key, network_id, user_auth, template_name, template_json, create_time)
					VALUES ($1, $2, $3, 'synthetic@example.com', 'synthetic', '{}'::jsonb, now())
				`,
				messageId, messageId.String(), outboxOnlyNetworkId,
			))
		})

		want := []server.Id{deleted.NetworkId, clientOnlyNetworkId, preferencesOnlyNetworkId, outboxOnlyNetworkId}
		slices.SortFunc(want, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		for _, pageCount := range []int{1, 2, 10000} {
			found := Testing_FindAllDeletedNetworkIds(ctx, pageCount)
			if slices.Contains(found, live.NetworkId) {
				t.Fatalf("page %d: the discovery found a network that exists", pageCount)
			}
			connect.AssertEqual(t, found, want)
		}
	})
}

// The referral grant gives nothing to a deleted network, as referrer or as
// referee, and a deleted referee does not count toward its referrer's bonus.
// A referee keeps its own bonus when its referrer is deleted. Before
// 2026-11-16 the grant read every referral link, with no check that its
// networks exist, and granted the deleted ones each run.
func TestReferralGrantSkipsDeletedNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		create := func(name string) server.Id {
			networkId := server.NewId()
			Testing_CreateNetwork(ctx, networkId, name+"-"+networkId.String()[24:], server.NewId())
			return networkId
		}
		referrerId := create("referrer")
		refereeId := create("referee")
		// gone: deleted networks, whose links remain. The links are from
		// before the referral terms, which pay without app use.
		deletedRefereeId := server.NewId()
		deletedReferrerId := server.NewId()
		orphanRefereeId := create("orphan-referee")
		server.Tx(ctx, func(tx server.PgTx) {
			for _, link := range [][2]server.Id{
				{refereeId, referrerId},
				{deletedRefereeId, referrerId},
				{orphanRefereeId, deletedReferrerId},
			} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_referral (network_id, referral_network_id, existing_link) VALUES ($1, $2, true)`,
					link[0],
					link[1],
				))
			}
		})
		startTime, endTime := ReferralGrantWindow(now)
		added := AddReferralBonusesToAllNetworks(ctx, startTime, endTime, ByteCount(100), ByteCount(10))
		for _, networkId := range []server.Id{deletedRefereeId, deletedReferrerId} {
			if _, granted := added[networkId]; granted {
				t.Fatalf("a deleted network was granted %d", added[networkId])
			}
		}
		connect.AssertEqual(t, added[refereeId], ByteCount(10))
		connect.AssertEqual(t, added[orphanRefereeId], ByteCount(10))
		// paid for the referral that exists alone
		connect.AssertEqual(t, added[referrerId], ByteCount(100)*ByteCount(ReferralBonusCount(1)))

		var deletedBalances int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM transfer_balance WHERE network_id = ANY($1)`,
				idStrings([]server.Id{deletedRefereeId, deletedReferrerId}),
			).Scan(&deletedBalances))
		})
		connect.AssertEqual(t, deletedBalances, 0)
	})
}

// The lag.dev guest expiry hands each guest it deletes to the cleanup in the
// removal's own transaction, and a guest kept because it was paid for is not
// handed over.
func TestLagGuestExpiryHandsTheGuestToTheCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc()
		guest := CreateLagGuestNetwork(ctx, now, now.Add(time.Hour))
		scheduled := map[server.Id]*DeletedNetwork{}
		schedule := func(tx server.PgTx, deletedNetwork *DeletedNetwork, userAuths map[string]bool) {
			scheduled[deletedNetwork.NetworkId] = deletedNetwork
		}
		result := RemoveExpiredLagGuestsWithRemovals(ctx, now.Add(2*time.Hour), 100, schedule)
		connect.AssertEqual(t, result.Removed, 1)
		connect.AssertEqual(t, slices.Collect(maps.Keys(scheduled)), []server.Id{guest.NetworkId})
		connect.AssertEqual(t, scheduled[guest.NetworkId].UserIds, []server.Id{guest.UserId})
	})
}

// Every table that names a network, user, client or device is either deleted
// from when its account is deleted or kept for a stated reason, so a table
// added later cannot keep a deleted account's rows unnoticed. Every table the
// classification names exists.
func TestEveryAccountTableIsClassified(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		accountTables := map[string]bool{}
		allTables := map[string]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT
						c.relname,
						bool_or(a.attname ~ '(network_id|client_id|user_id|device_id)s?$')
					FROM pg_class c
					JOIN pg_namespace n ON n.oid = c.relnamespace
					JOIN pg_attribute a ON a.attrelid = c.oid AND 0 < a.attnum AND NOT a.attisdropped
					WHERE
						n.nspname = 'public' AND
						c.relkind IN ('r', 'p') AND
						NOT c.relispartition
					GROUP BY c.relname
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var table string
					var keyed bool
					server.Raise(result.Scan(&table, &keyed))
					allTables[table] = true
					if keyed {
						accountTables[table] = true
					}
				}
			})
		})
		if len(accountTables) < 100 {
			t.Fatalf("found only %d account tables", len(accountTables))
		}
		deletedTables := deletedNetworkDeletedTableNames()
		for table := range accountTables {
			_, kept := deletedNetworkKeptTables[table]
			switch {
			case kept && deletedTables[table]:
				t.Errorf("%s is both deleted from and kept", table)
			case !kept && !deletedTables[table]:
				t.Errorf("%s names a network, user, client or device but is neither deleted from nor kept on account deletion", table)
			}
		}
		for table := range deletedTables {
			if !allTables[table] {
				t.Errorf("the cleanup deletes from %s, which does not exist", table)
			}
		}
		for table := range deletedNetworkKeptTables {
			if !allTables[table] {
				t.Errorf("the kept table %s does not exist", table)
			}
		}
	})
}

// A deactivation batch takes a deleted network's newest clients first, so a
// hosted proxy's windows, minted after the proxy, go in batches before it, and
// no batch pulls the proxy's whole tree of windows into one transaction.
func TestDeletedNetworkCleanupDeactivatesInBoundedBatches(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultDeletedNetworkCleanupSettings()
		settings.ClientBatchCount = 2
		// the parent's id is the least, as a minted parent's is
		clientIds := []server.Id{}
		for range 6 {
			clientIds = append(clientIds, server.NewId())
		}
		slices.SortFunc(clientIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
		parentId := clientIds[0]
		networkId := server.NewId()
		deviceId := server.NewId()
		Testing_CreateDevice(ctx, networkId, deviceId, parentId, "synthetic proxy", "synthetic")
		server.Tx(ctx, func(tx server.PgTx) {
			for _, childId := range clientIds[1:] {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_client (client_id, network_id, device_id, description, create_time, auth_time, source_client_id)
						VALUES ($1, $2, $3, 'synthetic window', now(), now(), $4)
					`,
					childId, networkId, deviceId, parentId,
				))
			}
		})
		activeCount := func() (n int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM network_client WHERE network_id = $1 AND active`, networkId).Scan(&n))
			})
			return
		}

		batchCount, deactivatedCount := deactivateDeletedNetworkClients(ctx, networkId, settings)
		connect.AssertEqual(t, batchCount, int64(2))
		connect.AssertEqual(t, deactivatedCount, int64(2))
		connect.AssertEqual(t, activeCount(), int64(4))

		result := RemoveDeletedNetworkData(ctx, &DeletedNetwork{NetworkId: networkId}, server.NowUtc(), settings)
		connect.AssertEqual(t, result.RowCounts[DeletedNetworkCleanupDeactivatedClients], int64(4))
		connect.AssertEqual(t, activeCount(), int64(0))
	})
}

// The dry run's counts under CountStatementTimeout: a count that runs past the
// timeout is reported for its table and the counting goes on with the next
// table. Another session holds provide_key's table lock, so that one count
// waits past the timeout; every other count matches the unbounded baseline.
// Unbounded, the same count waits for the lock until LockTimeout and the whole
// count fails.
func TestCountDeletedNetworkDataReportsTimedOutCounts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := Testing_CreateDeletedNetworkFixture(ctx, "synthetic-deleted-"+server.NewId().String()[24:])
		removeDeletedNetworkFixture(t, fixture)
		networkIds := []server.Id{fixture.NetworkId}
		now := server.NowUtc()

		baseline := CountDeletedNetworkData(ctx, networkIds, nil, now, DefaultDeletedNetworkCleanupSettings())
		if baseline.RowCounts["provide_key"] == 0 || len(baseline.TimedOutCounts) != 0 {
			t.Fatalf("baseline = %+v, want provide_key rows and no timeouts", baseline)
		}

		locked := make(chan struct{})
		release := make(chan struct{})
		unlocked := make(chan struct{})
		go func() {
			defer close(unlocked)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `LOCK TABLE provide_key IN ACCESS EXCLUSIVE MODE`))
				close(locked)
				<-release
			})
		}()
		<-locked
		// the lock goes as soon as the count returns, and also if it panics,
		// so no other test waits behind it
		var releaseOnce sync.Once
		unlock := func() {
			releaseOnce.Do(func() { close(release) })
			<-unlocked
		}
		defer unlock()
		settings := DefaultDeletedNetworkCleanupSettings()
		settings.CountStatementTimeout = 200 * time.Millisecond
		start := time.Now()
		bounded := CountDeletedNetworkData(ctx, networkIds, nil, now, settings)
		elapsed := time.Since(start)
		unlock()

		connect.AssertEqual(t, bounded.TimedOutCounts, map[string]int{"provide_key": 1})
		wantRows := maps.Clone(baseline.RowCounts)
		delete(wantRows, "provide_key")
		connect.AssertEqual(t, bounded.RowCounts, wantRows)
		connect.AssertEqual(t, bounded.ActiveClientCount, baseline.ActiveClientCount)
		connect.AssertEqual(t, bounded.DueClientCount, baseline.DueClientCount)
		connect.AssertEqual(t, bounded.WaitingClientCount, baseline.WaitingClientCount)
		connect.AssertEqual(t, bounded.DeviceCount, baseline.DeviceCount)
		// the locked count gave up at the statement timeout, well before the lock timeout
		if settings.LockTimeout <= elapsed {
			t.Fatalf("the bounded count took %s, want under the lock timeout %s", elapsed, settings.LockTimeout)
		}
	})
}
