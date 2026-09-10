package model

import (
	"context"
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
