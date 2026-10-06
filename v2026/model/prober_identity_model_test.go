package model

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// The prober bootstrap task re-arms itself every six hours, forever, with
// nobody watching. Every test in this file exists for that one reason: the
// damage from any of these behaviours regressing is not a failed pass, it is a
// slow accumulation -- a network, a client, or a balance grant per pass, four
// times a day -- that nothing surfaces until someone counts the rows.
//
// Each test gets its own database (server.DefaultTestEnv().Run drops it
// afterwards), which matters more here than usual: prober_identity is a
// singleton table, so tests sharing one database would contend for the single
// row.

// Row counts are the assertions these tests actually turn on. Status flags say
// what a pass BELIEVES it did; the row counts say what it did. countRows is the
// package-level helper from auth_model_test.go.

// proberTaskSession builds the session the taskworker actually passes in:
// UNAUTHENTICATED, ByJwt nil. Using an authenticated one here would hide a
// whole class of regression, since the model is required to build its own
// authenticated session from the stored row rather than read one from this.
func proberTaskSession(ctx context.Context) *session.ClientSession {
	return session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
}

// Grant admission must use its already-owned connection. Requiring another
// pool slot while holding the grant lock deadlocks a saturated worker pool.
func TestProberPreflightUsesOnePoolConnection(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()
		if _, err := BootstrapProberIdentity(clientSession); err != nil {
			t.Fatal(err)
		}
		pop := server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n"))
		server.PgReset()
		defer func() { pop(); server.PgReset() }()
		var granted bool
		var grantErr error
		if err := server.HandleError(func() {
			granted, grantErr = EnsureProberTransferBalance(ctx, 2*ProberTransferBalanceTopUp)
		}); err != nil {
			t.Fatalf("single-connection preflight failed: %v", err)
		}
		if grantErr != nil || !granted {
			t.Fatalf("single-connection preflight granted=%t error=%v", granted, grantErr)
		}
	})
}

// The waiter must read the grant committed by the previous lock owner, not
// the snapshot established when its SELECT FOR UPDATE began waiting.
func TestProberPreflightSeesPrecedingQueuedGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()
		if _, err := BootstrapProberIdentity(clientSession); err != nil {
			t.Fatal(err)
		}
		identity := GetProberIdentity(ctx)
		locked := make(chan uint32, 1)
		release := make(chan struct{})
		ownerDone := make(chan any, 1)
		go func() {
			ownerDone <- server.HandleError(func() {
				server.Tx(ctx, func(tx server.PgTx) {
					server.RaisePgResult(tx.Exec(ctx, `SELECT network_id FROM prober_identity WHERE singleton FOR UPDATE`))
					locked <- tx.Conn().PgConn().PID()
					select {
					case <-release:
					case <-ctx.Done():
						server.Raise(ctx.Err())
					}
					now := server.NowUtc()
					server.Raise(AddBasicTransferBalanceInTx(tx, ctx, *identity.NetworkId,
						ProberTransferBalanceTopUp, now, now.Add(ProberTransferBalanceDuration)))
				})
			})
		}()
		ownerPid := <-locked
		waiterDone := make(chan any, 1)
		go func() {
			waiterDone <- server.HandleError(func() {
				granted, err := EnsureProberTransferBalance(ctx, 2*ProberTransferBalanceTopUp)
				server.Raise(err)
				if granted {
					panic(fmt.Errorf("queued preflight duplicated preceding committed grant"))
				}
			})
		}()
		// A real PostgreSQL lock waiter establishes the interleaving. There is
		// no sleep-based assumption about when the second transaction started.
		waiting := false
		for !waiting && ctx.Err() == nil {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT EXISTS (
					SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
					AND $1::int = ANY(pg_blocking_pids(pid))
					AND query LIKE '%prober_identity%FOR UPDATE%'
				)`, ownerPid).Scan(&waiting))
			})
		}
		close(release)
		if err := <-ownerDone; err != nil {
			t.Fatalf("preceding grant: %v", err)
		}
		if err := <-waiterDone; err != nil {
			t.Fatalf("queued preflight: %v", err)
		}
		if !waiting {
			t.Fatal("did not establish queued singleton-lock waiter")
		}
		if got := countRows(ctx, `SELECT count(*) FROM transfer_balance WHERE network_id = $1`, *identity.NetworkId); got != 2 {
			t.Fatalf("balance grants=%d, want bootstrap and preceding owner only", got)
		}
	})
}

// A final grant near the integer boundary must not wrap the available sum
// and turn a one-byte deficit into an unbounded sequence of new grants.
func TestProberPreflightDoesNotOverflowAvailableBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()
		if _, err := BootstrapProberIdentity(clientSession); err != nil {
			t.Fatal(err)
		}
		identity := GetProberIdentity(ctx)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance
				SET start_balance_byte_count = $1, balance_byte_count = $1
				WHERE network_id = $2`, int64(math.MaxInt64-1), *identity.NetworkId))
		})
		if err := server.HandleError(func() {
			granted, err := EnsureProberTransferBalance(ctx, math.MaxInt64)
			server.Raise(err)
			if !granted {
				panic(fmt.Errorf("one-byte credit deficit was not granted"))
			}
		}); err != nil {
			t.Fatalf("near-boundary preflight failed: %v", err)
		}
		if got := GetActiveTransferBalanceByteCount(ctx, *identity.NetworkId); got != math.MaxInt64 {
			t.Fatalf("available credit=%d, want representable maximum", got)
		}
		granted, err := EnsureProberTransferBalance(ctx, math.MaxInt64)
		if err != nil || granted {
			t.Fatalf("healthy near-boundary repeat granted=%t error=%v", granted, err)
		}
	})
}

// Four shards may start on different Taskworkers at once. Their preflights
// must serialize against the one persisted internal account, not each grant
// against the same stale balance snapshot.
func TestProberShardPreflightSerializesSharedHeadroom(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()
		if _, err := BootstrapProberIdentity(clientSession); err != nil {
			t.Fatalf("bootstrap: %v", err)
		}
		identity := GetProberIdentity(ctx)
		if !identity.HasNetwork() {
			t.Fatal("bootstrap did not persist the internal prober network")
		}
		minimum := 4 * ProberShardTransferHeadroom
		const simultaneousShards = 8
		var wg sync.WaitGroup
		errs := make(chan error, simultaneousShards)
		grants := make(chan bool, simultaneousShards)
		for i := 0; i < simultaneousShards; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				granted, err := EnsureProberTransferBalance(ctx, minimum)
				grants <- granted
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		close(grants)
		for err := range errs {
			if err != nil {
				t.Fatalf("shard preflight: %v", err)
			}
		}
		grantedCount := 0
		for granted := range grants {
			if granted {
				grantedCount++
			}
		}
		if grantedCount != 1 {
			t.Errorf("%d preflights granted credit, want one", grantedCount)
		}
		if got := countRows(ctx, `SELECT count(*) FROM transfer_balance WHERE network_id = $1`, *identity.NetworkId); got != 2 {
			t.Errorf("%d balance rows after concurrent preflights, want bootstrap plus one grant", got)
		}
		if available := GetActiveTransferBalanceByteCount(ctx, *identity.NetworkId); available < minimum {
			t.Errorf("available credit %d below combined shard headroom %d", available, minimum)
		}
		granted, err := EnsureProberTransferBalance(ctx, minimum)
		if err != nil || granted {
			t.Errorf("healthy repeat preflight granted=%t error=%v", granted, err)
		}
		// Simulate the old cohort retaining both grants in Redis escrow. The
		// next shard must replenish the full combined floor, not stop after one
		// 512 GiB grant while most of its peers remain unfunded.
		balances := GetActiveTransferBalances(ctx, *identity.NetworkId)
		server.Redis(ctx, func(r server.RedisClient) {
			for _, balance := range balances {
				server.Raise(r.Set(ctx, netEscrowKey(balance.BalanceId),
					int64(balance.BalanceByteCount)-1, time.Hour).Err())
			}
		})
		defer server.Redis(ctx, func(r server.RedisClient) {
			for _, balance := range balances {
				server.Raise(r.Del(ctx, netEscrowKey(balance.BalanceId)).Err())
			}
		})
		granted, err = EnsureProberTransferBalance(ctx, minimum)
		if err != nil || !granted {
			t.Fatalf("reserved headroom was not replenished: granted=%t error=%v", granted, err)
		}
		if got := countRows(ctx, `SELECT count(*) FROM transfer_balance WHERE network_id = $1`, *identity.NetworkId); got != 4 {
			t.Errorf("%d grants after depleted shared headroom, want four total", got)
		}
		if available := GetActiveTransferBalanceByteCount(ctx, *identity.NetworkId); available < minimum {
			t.Errorf("replenished credit %d below combined shard headroom %d", available, minimum)
		}
	})
}

// A second pass must not create a second network.
//
// This is the highest-severity behaviour in the feature. The account is the one
// irreversible thing the task makes, and a repeat run that created another
// would do so every six hours forever, filling the deployment with orphan
// accounts that nothing has a name to look up -- the seedphrase branch of
// NetworkCreate discards the requested name, so prober_identity is the only
// record any of them exist.
//
// The `count(*) FROM network` assertion is the real one. status.NetworkCreated
// is the task's own opinion, and a regression that created a network while
// failing to record it would report exactly the same false.
func TestProberBootstrapSecondPassCreatesNoSecondNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()

		status1, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the first bootstrap pass failed: %s", err)
		}
		if !status1.NetworkCreated {
			t.Fatalf("the first pass did not create the network, so this test never reaches what it is testing: %+v", status1)
		}
		identity1 := GetProberIdentity(ctx)
		if !identity1.HasNetwork() {
			t.Fatalf("the first pass reported a create but stored no network")
		}
		networksAfterFirst := countRows(ctx, `SELECT count(*) FROM network`)

		status2, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the second bootstrap pass failed: %s", err)
		}

		if status2.NetworkCreated {
			t.Errorf("the second pass created a network again; this task runs every %s forever, "+
				"so a pass that creates is a new orphan account four times a day", "6h")
		}
		networksAfterSecond := countRows(ctx, `SELECT count(*) FROM network`)
		if networksAfterSecond != networksAfterFirst {
			t.Errorf("the second pass created another network: count went %d -> %d. "+
				"The claim in claimProberIdentityCreate is what must prevent this",
				networksAfterFirst, networksAfterSecond)
		}

		identity2 := GetProberIdentity(ctx)
		if !identity2.HasNetwork() || *identity2.NetworkId != *identity1.NetworkId {
			t.Errorf("the stored prober network changed across passes: %v -> %v. "+
				"The identity must be single-assignment; repointing it strands the previous account "+
				"and every credential minted against it", identity1.NetworkId, identity2.NetworkId)
		}

		// The pass above never reached the claim: BootstrapProberIdentity
		// short-circuits on HasNetwork() and returns before createProberNetwork.
		// That short-circuit is only the OUTER layer, and it is the one that can
		// legitimately be bypassed -- two workers entering a pass together both
		// read an empty identity, and a crash between NetworkCreate and
		// setProberIdentityNetwork leaves a later pass believing there is no
		// account. The claim is what has to hold then, so it is driven here
		// directly, exactly as a concurrent pass would reach it.
		//
		// `created` alone is too weak to assert: with the claim granted, the
		// create runs and setProberIdentityNetwork's own guard then rejects the
		// result, so this still returns false while a real, paid-for, orphaned
		// network exists in the table with nothing pointing at it. The count is
		// what sees that.
		raced, err := createProberNetwork(clientSession, &ProberBootstrapStatus{})
		if err != nil {
			t.Fatalf("a create attempt against an existing identity errored rather than declining: %s", err)
		}
		if raced {
			t.Errorf("createProberNetwork reported a create while the identity already had a network")
		}
		if networksAfterRace := countRows(ctx, `SELECT count(*) FROM network`); networksAfterRace != networksAfterFirst {
			t.Errorf("a create attempt that bypassed the HasNetwork short-circuit created an ORPHAN network: "+
				"count went %d -> %d. Nothing can find that account again -- the seedphrase branch of NetworkCreate "+
				"discards the requested name, so prober_identity is the only record any prober network exists",
				networksAfterFirst, networksAfterRace)
		}
	})
}

// Once network_id is set, the claim must return no row at all -- and must not
// burn an attempt doing it.
//
// The claim is the mechanism the whole no-second-network guarantee rests on, so
// it is tested directly rather than only through a full pass. The second
// assertion is the subtle one: the guard lives in the ON CONFLICT's
// `WHERE prober_identity.network_id IS NULL`, so a steady-state pass touches no
// row. A refactor that moved that guard to a check AFTER the update would still
// return claimed == false and still look correct here, while incrementing
// create_attempts on every pass -- so the counter would reach
// MaxProberBootstrapAttempts within about a day, and the identity would then be
// permanently unable to recreate its account if it ever needed to.
func TestProberIdentityClaimReturnsNothingOnceNetworkIsSet(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		createAttempts, claimed := claimProberIdentityCreate(ctx)
		if !claimed || createAttempts != 0 {
			t.Fatalf("the first claim on an empty table must be granted with 0 prior attempts, got attempts=%d claimed=%v",
				createAttempts, claimed)
		}

		// the account now exists, as it would after a successful create
		if !setProberIdentityNetwork(ctx, server.NewId(), server.NewId(), "prober-net") {
			t.Fatalf("could not record the network on a freshly claimed row")
		}
		attemptsAtClaim := GetProberIdentity(ctx).CreateAttempts

		_, claimedAgain := claimProberIdentityCreate(ctx)
		if claimedAgain {
			t.Errorf("the claim was granted again after network_id was set; every later pass would create another account")
		}

		attemptsAfter := GetProberIdentity(ctx).CreateAttempts
		if attemptsAfter != attemptsAtClaim {
			t.Errorf("a refused claim burned an attempt: create_attempts went %d -> %d. "+
				"In the steady state this runs every 6h forever, so the counter would reach "+
				"MaxProberBootstrapAttempts (%d) within days and the identity could never recreate its account",
				attemptsAtClaim, attemptsAfter, MaxProberBootstrapAttempts)
		}
	})
}

// The balance grant must be driven by the CURRENT balance, not by whether a
// grant has ever happened.
//
// Both directions are pinned here because the two regressions are opposites and
// each is invisible on its own:
//
//   - drop the `< ProberMinTransferBalance` condition and every pass stacks
//     another 32 GiB grant, four times a day forever
//   - replace it with a once-only flag and the prober silently runs out of
//     balance when its grant expires, which is exactly the silent stop this
//     whole feature exists to remove
func TestProberBootstrapDoesNotRegrantAHealthyBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()

		status1, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the first bootstrap pass failed: %s", err)
		}
		if !status1.BalanceGranted {
			t.Fatalf("the first pass granted no balance, so this test never reaches what it is testing: %+v", status1)
		}
		identity := GetProberIdentity(ctx)
		if !identity.HasNetwork() {
			t.Fatalf("the first pass stored no network")
		}
		networkId := *identity.NetworkId

		balanceRows := `SELECT count(*) FROM transfer_balance WHERE network_id = $1`
		rowsAfterFirst := countRows(ctx, balanceRows, networkId)
		if rowsAfterFirst != 1 {
			t.Fatalf("expected exactly one balance row after the first grant, got %d", rowsAfterFirst)
		}
		if active := GetActiveTransferBalanceByteCount(ctx, networkId); active < ProberMinTransferBalance {
			t.Fatalf("the granted balance %d is below ProberMinTransferBalance %d, so the second pass "+
				"would legitimately grant again and this test would prove nothing",
				active, ProberMinTransferBalance)
		}

		status2, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the second bootstrap pass failed: %s", err)
		}
		if status2.BalanceGranted {
			t.Errorf("the second pass granted balance again while the balance was already healthy")
		}
		if rows := countRows(ctx, balanceRows, networkId); rows != rowsAfterFirst {
			t.Errorf("a pass over a healthy balance wrote another transfer_balance row: %d -> %d. "+
				"At one pass every 6h that is four stacked grants a day, forever",
				rowsAfterFirst, rows)
		}

		// Now the other direction: expire the grant, exactly as it expires on its
		// own after ProberTransferBalanceDuration, and the next pass must top up.
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE transfer_balance SET end_time = $2 WHERE network_id = $1`,
				networkId,
				server.NowUtc().Add(-time.Hour),
			))
		})
		if active := GetActiveTransferBalanceByteCount(ctx, networkId); ProberMinTransferBalance <= active {
			t.Fatalf("expiring the balance left %d active, so the top-up half of this test is not exercised", active)
		}

		status3, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the third bootstrap pass failed: %s", err)
		}
		if !status3.BalanceGranted {
			t.Errorf("the balance had run out and the pass did not top it up. The grant must be conditional on the "+
				"CURRENT balance being below ProberMinTransferBalance (%d), not on whether a grant ever happened -- "+
				"a prober with no balance cannot open a contract and stops probing silently",
				ProberMinTransferBalance)
		}
	})
}

// A busy prober can have hundreds of GiB of genuine, short-lived escrow.
// Only this persisted internal identity may receive repeat grants; a Redis
// reservation must reduce available credit without changing the PG grant row.
func TestProberBootstrapReplenishesHeavyEscrow(t *testing.T) {
	if ProberMinTransferBalance < 256*Gib || ProberTransferBalanceTopUp < 512*Gib {
		t.Fatal("prober grant cannot cover the synthetic high-parallel reservation envelope")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()
		first, err := BootstrapProberIdentity(clientSession)
		if err != nil || !first.BalanceGranted {
			t.Fatalf("first prober grant: granted=%t err=%v", first.BalanceGranted, err)
		}
		identity := GetProberIdentity(ctx)
		if !identity.HasNetwork() {
			t.Fatal("prober identity has no network")
		}
		balances := GetActiveTransferBalances(ctx, *identity.NetworkId)
		if len(balances) != 1 {
			t.Fatalf("first pass made %d grants, want one", len(balances))
		}
		key := netEscrowKey(balances[0].BalanceId)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, key, int64(balances[0].BalanceByteCount-1), time.Hour).Err())
		})
		defer server.Redis(ctx, func(r server.RedisClient) { server.Raise(r.Del(ctx, key).Err()) })
		if available := GetActiveTransferBalanceByteCount(ctx, *identity.NetworkId); available != 1 {
			t.Fatalf("synthetic reservation left %d bytes, want one", available)
		}
		second, err := BootstrapProberIdentity(clientSession)
		if err != nil || !second.BalanceGranted {
			t.Fatalf("reserved prober credit was not replenished: granted=%t err=%v", second.BalanceGranted, err)
		}
		if got := len(GetActiveTransferBalances(ctx, *identity.NetworkId)); got != 2 {
			t.Fatalf("reservation refill made %d grant rows, want two total", got)
		}
		if available := GetActiveTransferBalanceByteCount(ctx, *identity.NetworkId); available < ProberMinTransferBalance {
			t.Fatalf("refill left only %d available bytes", available)
		}
		third, err := BootstrapProberIdentity(clientSession)
		if err != nil || third.BalanceGranted {
			t.Fatalf("healthy prober received a duplicate refill: granted=%t err=%v", third.BalanceGranted, err)
		}
	})
}

// A re-mint must re-auth the SAME client, not provision another one.
//
// The stored client_id is passed back into AuthNetworkClient for exactly this
// reason. Dropping it (minting against a nil client id) still produces a
// working credential on every pass, so nothing fails and nothing logs -- the
// only symptom is one more network_client and one more device row every six
// hours, forever, in a table the sweepers then have to walk.
//
// The client here is a genuine one from a real first mint. Seeding a fabricated
// id instead would make the re-auth fail and fire the recovery path
// (clearProberIdentityClient, then a new client), so client_id would change for
// a legitimate reason and the test would be asserting the opposite behaviour.
func TestProberRemintReusesTheSameClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()

		status1, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the first bootstrap pass failed: %s", err)
		}
		if !status1.ClientJwtMinted {
			t.Fatalf("the first pass minted no client jwt, so there is no client to re-mint for: %+v", status1)
		}
		identity1 := GetProberIdentity(ctx)
		if identity1.ClientId == nil {
			t.Fatalf("the first pass stored no client id")
		}
		firstClientId := *identity1.ClientId
		firstJwt := identity1.ByClientJwt

		clientRows := `SELECT count(*) FROM network_client WHERE network_id = $1`
		deviceRows := `SELECT count(*) FROM device WHERE network_id = $1`
		clientsAfterFirst := countRows(ctx, clientRows, *identity1.NetworkId)
		devicesAfterFirst := countRows(ctx, deviceRows, *identity1.NetworkId)

		// age the stored credential past ProberJwtRefreshAge so the next pass
		// re-mints rather than doing nothing
		setProberIdentityClient(ctx, firstClientId, firstJwt,
			server.NowUtc().Add(-ProberJwtRefreshAge-time.Minute))

		status2, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the re-mint pass failed: %s", err)
		}
		if !status2.ClientJwtMinted {
			t.Fatalf("a credential older than ProberJwtRefreshAge (%s) was not re-minted; "+
				"the prober's jwt would eventually expire with nothing renewing it", ProberJwtRefreshAge)
		}

		identity2 := GetProberIdentity(ctx)
		if identity2.ClientId == nil {
			t.Fatalf("the re-mint stored no client id")
		}
		if *identity2.ClientId != firstClientId {
			t.Errorf("the re-mint provisioned a NEW client (%s -> %s) instead of re-authing the stored one. "+
				"At one pass every 6h this accumulates a client and a device per pass forever",
				firstClientId, *identity2.ClientId)
		}
		if clients := countRows(ctx, clientRows, *identity1.NetworkId); clients != clientsAfterFirst {
			t.Errorf("the re-mint added a network_client row: %d -> %d", clientsAfterFirst, clients)
		}
		if devices := countRows(ctx, deviceRows, *identity1.NetworkId); devices != devicesAfterFirst {
			t.Errorf("the re-mint added a device row: %d -> %d", devicesAfterFirst, devices)
		}
		if identity2.ByClientJwt == firstJwt {
			t.Errorf("the re-mint stored the same jwt it started with, so nothing was actually refreshed " +
				"and the credential still ages out on the original deadline")
		}
	})
}

// Past MaxProberBootstrapAttempts the task must STOP creating.
//
// This bound is the backstop for the whole feature: if creation is failing for
// some reason that a retry cannot fix, an unbounded task creates a network on
// every pass, forever, four times a day. Failing loudly and stopping is
// recoverable; a thousand orphan accounts is not.
//
// The attempts are burned by calling the claim directly rather than by driving
// five real creates. claimProberIdentityCreate returns the number of attempts
// BEFORE the current one, so five prior claims leave create_attempts at 4 and
// the bootstrap's own claim -- the sixth -- returns 5, which is the bound.
func TestProberBootstrapStopsCreatingAtTheAttemptBound(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()

		for i := 0; i < MaxProberBootstrapAttempts; i++ {
			if _, claimed := claimProberIdentityCreate(ctx); !claimed {
				t.Fatalf("claim %d was refused while network_id is still NULL", i+1)
			}
		}

		status, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the bootstrap pass returned an error rather than giving up cleanly: %s", err)
		}

		if !status.CreateExhausted {
			t.Errorf("the pass did not report the attempt bound as spent after %d attempts: %+v",
				MaxProberBootstrapAttempts, status)
		}
		if status.NetworkCreated {
			t.Errorf("a network was created past the attempt bound of %d. Unbounded, this creates "+
				"an orphan account every 6h forever", MaxProberBootstrapAttempts)
		}
		if n := countRows(ctx, `SELECT count(*) FROM network`); n != 0 {
			t.Errorf("%d network(s) exist after a pass that should have refused to create", n)
		}
		if GetProberIdentity(ctx).HasNetwork() {
			t.Errorf("an identity was recorded by a pass that should have refused to create")
		}
	})
}

// The bound must not bite one attempt early.
//
// The pair with the test above brackets MaxProberBootstrapAttempts from both
// sides. On its own, either one passes under an off-by-one: a bound that
// refused a create too early would leave a deployment with NO prober account at
// all -- the exact silent no-probing state this feature was written to remove --
// and the exhaustion test alone would still be green.
func TestProberBootstrapStillCreatesOnTheLastAllowedAttempt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := proberTaskSession(ctx)
		defer clientSession.Cancel()

		for i := 0; i < MaxProberBootstrapAttempts-1; i++ {
			if _, claimed := claimProberIdentityCreate(ctx); !claimed {
				t.Fatalf("claim %d was refused while network_id is still NULL", i+1)
			}
		}

		status, err := BootstrapProberIdentity(clientSession)
		if err != nil {
			t.Fatalf("the bootstrap pass failed: %s", err)
		}

		if status.CreateExhausted {
			t.Errorf("creation was refused on the last attempt the bound still allows "+
				"(%d prior attempts, bound %d); a deployment would be left with no prober account and no probing",
				MaxProberBootstrapAttempts-1, MaxProberBootstrapAttempts)
		}
		if !status.NetworkCreated {
			t.Errorf("no network was created on the last allowed attempt: %+v", status)
		}
		if !GetProberIdentity(ctx).HasNetwork() {
			t.Errorf("no prober identity was recorded on the last allowed attempt")
		}
	})
}
