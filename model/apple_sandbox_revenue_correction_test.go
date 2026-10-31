// The operator correction for sandbox App Store revenue
// (local/apple-sandbox-revenue-correction.sql) and its read-only dry run run
// against synthetic rows covering each predicate edge. The dry run's counts
// are what the correction must change; any other count, scope or isolation
// rolls back the whole correction. Entitlements are never touched.
package model

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/urnetwork/server"
)

type appleSandboxCorrectionCounts struct {
	renewals       int64
	creditBalances int64
	grantBalances  int64
	renewalUsd     float64
	creditUsd      float64
	grantUsd       float64
}

func appleSandboxCorrectionScript(t testing.TB, name string) string {
	t.Helper()
	script, err := os.ReadFile("../local/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(script)
}

// appleSandboxCorrectionDryRun runs the dry run in a read-only transaction.
func appleSandboxCorrectionDryRun(t testing.TB, ctx context.Context, includePreLedger string) appleSandboxCorrectionCounts {
	t.Helper()
	script := appleSandboxCorrectionScript(t, "apple-sandbox-revenue-correction-dry-run.sql")
	counts := appleSandboxCorrectionCounts{}
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		server.Raise(err)
		defer tx.Rollback(ctx)
		_, err = tx.Exec(ctx, `SELECT set_config('urnetwork.apple_sandbox_include_pre_ledger', $1, true)`, includePreLedger)
		server.Raise(err)
		var scope string
		var networks, openBalances int64
		err = tx.QueryRow(ctx, script).Scan(
			&scope,
			&counts.renewals,
			&counts.renewalUsd,
			&networks,
			&counts.creditBalances,
			&counts.creditUsd,
			&counts.grantBalances,
			&counts.grantUsd,
			&openBalances,
		)
		server.Raise(err)
		if scope != includePreLedger {
			t.Fatalf("the dry run reported scope %q, want %q", scope, includePreLedger)
		}
	})
	return counts
}

type appleSandboxCorrectionRun struct {
	includePreLedger string
	counts           appleSandboxCorrectionCounts
	isoLevel         pgx.TxIsoLevel
	// leaves the expected counts unset
	omitCounts bool
}

// apply runs the correction script as an operator would and commits.
func (self *appleSandboxCorrectionRun) apply(t testing.TB, ctx context.Context) (receipt string, returnErr error) {
	t.Helper()
	script := appleSandboxCorrectionScript(t, "apple-sandbox-revenue-correction.sql")
	server.Db(ctx, func(conn server.PgConn) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: self.isoLevel})
		if err != nil {
			returnErr = err
			return
		}
		defer tx.Rollback(ctx)
		if self.omitCounts {
			_, err = tx.Exec(ctx, `SELECT set_config('urnetwork.apple_sandbox_include_pre_ledger', $1, true)`, self.includePreLedger)
		} else {
			_, err = tx.Exec(
				ctx,
				`
				SELECT set_config('urnetwork.apple_sandbox_include_pre_ledger', $1, true),
				       set_config('urnetwork.apple_sandbox_expected_renewals', $2, true),
				       set_config('urnetwork.apple_sandbox_expected_credit_balances', $3, true),
				       set_config('urnetwork.apple_sandbox_expected_grant_balances', $4, true)
				`,
				self.includePreLedger,
				strconv.FormatInt(self.counts.renewals, 10),
				strconv.FormatInt(self.counts.creditBalances, 10),
				strconv.FormatInt(self.counts.grantBalances, 10),
			)
		}
		if err == nil {
			_, err = tx.Exec(ctx, script)
		}
		if err == nil {
			err = tx.QueryRow(ctx, `SELECT current_setting('urnetwork.apple_sandbox_revenue_correction_receipt')`).Scan(&receipt)
		}
		if err == nil {
			err = tx.Commit(ctx)
		}
		returnErr = err
	})
	return
}

// appleSandboxCorrectionSeed writes synthetic renewals, ledger rows and
// balances in the shapes the credit paths write them.
type appleSandboxCorrectionSeed struct {
	ctx context.Context
	tx  server.PgTx
}

func (self *appleSandboxCorrectionSeed) addRenewal(
	networkId server.Id,
	transactionId string,
	startTime time.Time,
	endTime time.Time,
	netRevenue NanoCents,
	ledger bool,
) {
	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
		INSERT INTO subscription_renewal (
			network_id, subscription_type, start_time, end_time,
			net_revenue_nano_cents, market, transaction_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		`,
		networkId,
		SubscriptionTypeSupporter,
		startTime,
		endTime,
		netRevenue,
		SubscriptionMarketApple,
		transactionId,
	))
	if ledger {
		server.RaisePgResult(self.tx.Exec(
			self.ctx,
			`
			INSERT INTO apple_subscription_transaction (transaction_id, notification_uuid, network_id, product_id, price_milliunits, currency)
			VALUES ($1, $2, $3, 'supporter_monthly_26', 4990, 'USD')
			`,
			transactionId,
			server.NewId(),
			networkId,
		))
	}
}

func (self *appleSandboxCorrectionSeed) addBalance(
	networkId server.Id,
	startTime time.Time,
	endTime time.Time,
	subsidyNetRevenue NanoCents,
	pro bool,
	grantKind *string,
) server.Id {
	balanceId := server.NewId()
	server.RaisePgResult(self.tx.Exec(
		self.ctx,
		`
		INSERT INTO transfer_balance (
			balance_id, network_id, start_time, end_time, start_balance_byte_count,
			net_revenue_nano_cents, subsidy_net_revenue_nano_cents, balance_byte_count, pro, grant_kind
		)
		VALUES ($1, $2, $3, $4, $5, 0, $6, $5, $7, $8)
		`,
		balanceId,
		networkId,
		startTime,
		endTime,
		ByteCount(1024*1024),
		subsidyNetRevenue,
		pro,
		grantKind,
	))
	return balanceId
}

type appleSandboxCorrectionBalance struct {
	subsidy          NanoCents
	paid             bool
	pro              bool
	startTime        time.Time
	endTime          time.Time
	balanceByteCount ByteCount
}

func readAppleSandboxCorrectionBalance(ctx context.Context, balanceId server.Id) appleSandboxCorrectionBalance {
	balance := appleSandboxCorrectionBalance{}
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(
			ctx,
			`
			SELECT subsidy_net_revenue_nano_cents, paid, pro, start_time, end_time, balance_byte_count
			FROM transfer_balance WHERE balance_id = $1
			`,
			balanceId,
		).Scan(&balance.subsidy, &balance.paid, &balance.pro, &balance.startTime, &balance.endTime, &balance.balanceByteCount)
		server.Raise(err)
	})
	return balance
}

func readAppleSandboxCorrectionRenewalRevenue(ctx context.Context, networkId server.Id, transactionId string) NanoCents {
	var netRevenue NanoCents
	server.Db(ctx, func(conn server.PgConn) {
		err := conn.QueryRow(
			ctx,
			`
			SELECT net_revenue_nano_cents FROM subscription_renewal
			WHERE network_id = $1 AND market = $2 AND transaction_id = $3
			`,
			networkId,
			SubscriptionMarketApple,
			transactionId,
		).Scan(&netRevenue)
		server.Raise(err)
	})
	return netRevenue
}

func TestAppleSandboxRevenueCorrectionZeroesOnlySandboxRevenue(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc().Truncate(time.Second)
		day := 24 * time.Hour
		price := UsdToNanoCents(3.99)
		marker := UsdToNanoCents(0.01)
		proGrant := "pro"
		freeGrant := "free"

		// testflight: a TestFlight-only network; production: a subscriber;
		// mixed: both at once; traps: ids or periods with one signal only;
		// preLedger: TestFlight rows from the handler before the ledger
		testflight, production, mixed, traps, preLedger, testflightGrant := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		for i, networkId := range []server.Id{testflight, production, mixed, traps, preLedger, testflightGrant} {
			Testing_CreateNetwork(ctx, networkId, "applesandboxcorrection"+strconv.Itoa(i), server.NewId())
		}

		type expectedBalance struct {
			name        string
			balanceId   server.Id
			subsidy     NanoCents
			zeroed      bool
			zeroedByPre bool
		}
		balances := []*expectedBalance{}
		type expectedRenewal struct {
			networkId     server.Id
			transactionId string
			netRevenue    NanoCents
			zeroed        bool
			zeroedByPre   bool
		}
		renewals := []*expectedRenewal{}

		server.Tx(ctx, func(tx server.PgTx) {
			seed := &appleSandboxCorrectionSeed{ctx: ctx, tx: tx}
			addRenewal := func(networkId server.Id, transactionId string, start time.Time, end time.Time, netRevenue NanoCents, ledger bool, zeroed bool, zeroedByPre bool) {
				seed.addRenewal(networkId, transactionId, start, end, netRevenue, ledger)
				renewals = append(renewals, &expectedRenewal{networkId, transactionId, netRevenue, zeroed, zeroedByPre})
			}
			addBalance := func(name string, networkId server.Id, start time.Time, end time.Time, subsidy NanoCents, pro bool, grantKind *string, zeroed bool, zeroedByPre bool) {
				balanceId := seed.addBalance(networkId, start, end, subsidy, pro, grantKind)
				balances = append(balances, &expectedBalance{name, balanceId, subsidy, zeroed, zeroedByPre})
			}

			// a TestFlight day: a sandbox id and a 24 hour period, its credit
			// balance, and the monthly grant pro-rated from it alone
			addRenewal(testflight, "2000000000002001", now.Add(-time.Hour), now.Add(47*time.Hour), price, true, true, false)
			addBalance("testflight credit", testflight, now.Add(-time.Hour), now.Add(47*time.Hour), price, true, nil, true, false)
			addBalance("testflight grant", testflight, now.Add(-30*time.Minute), now.Add(31*day), UsdToNanoCents(63.84), true, nil, true, false)

			// the same grant shape with the current grant kind; a data balance
			// of the network is never touched
			addRenewal(testflightGrant, "2000000000002004", now.Add(-2*time.Hour), now.Add(46*time.Hour), price, true, true, false)
			addBalance("testflight pro-kind grant", testflightGrant, now.Add(-time.Hour), now.Add(31*day), UsdToNanoCents(63.84), true, &proGrant, true, false)
			addBalance("testflight data balance", testflightGrant, now.Add(-time.Hour), now.Add(day), price, false, &freeGrant, false, false)

			// a production subscriber: renewal, credit and grant all stay
			addRenewal(production, "150000000002001", now.Add(-day), now.Add(31*day), price, true, false, false)
			addBalance("production credit", production, now.Add(-day), now.Add(31*day), price, true, nil, false, false)
			addBalance("production grant", production, now.Add(-30*time.Minute), now.Add(31*day), UsdToNanoCents(2.45), true, &proGrant, false, false)

			// mixed: the sandbox renewal and its credit are corrected; the
			// grant shared a production renewal and stays
			addRenewal(mixed, "2000000000002002", now.Add(-time.Hour), now.Add(47*time.Hour), price, true, true, false)
			addBalance("mixed sandbox credit", mixed, now.Add(-time.Hour), now.Add(47*time.Hour), price, true, nil, true, false)
			addRenewal(mixed, "150000000002002", now.Add(-2*day), now.Add(28*day), price, true, false, false)
			addBalance("mixed grant", mixed, now.Add(-30*time.Minute), now.Add(31*day), UsdToNanoCents(66.29), true, &proGrant, false, false)

			// one signal is not enough: a sandbox-family id with a month, a
			// production id with a day, and pre-ledger rows of a year and of a
			// malformed negative period
			addRenewal(traps, "2000000000002003", now.Add(-day), now.Add(30*day), price, true, false, false)
			addRenewal(traps, "150000000002003", now.Add(-time.Hour), now.Add(47*time.Hour), price, true, false, false)
			addBalance("production day credit", traps, now.Add(-time.Hour), now.Add(47*time.Hour), price, true, nil, false, false)
			addRenewal(traps, "704200000000002003", now.Add(-100*day), now.Add(266*day), price, false, false, false)
			addRenewal(traps, "704200000000002004", now.Add(-5*day), now.Add(-5*day+10*time.Hour), price, false, false, false)

			// pre-ledger TestFlight rows: corrected only in the pre-ledger scope
			// (start at processing, end at expiry plus the day of grace)
			addRenewal(preLedger, "704200000000002001", now.Add(-10*day), now.Add(-10*day+48*time.Hour+time.Minute), price, false, false, true)
			addBalance("pre-ledger credit", preLedger, now.Add(-10*day), now.Add(-10*day+48*time.Hour+time.Minute), marker, true, nil, false, true)
			addRenewal(preLedger, "704200000000002002", now.Add(-20*day), now.Add(-20*day+day+5*time.Minute), marker, false, false, true)
			addBalance("pre-ledger grant", preLedger, now.Add(-10*day+time.Hour), now.Add(21*day), UsdToNanoCents(31.92), true, &proGrant, false, true)
		})

		before := map[server.Id]appleSandboxCorrectionBalance{}
		for _, balance := range balances {
			before[balance.balanceId] = readAppleSandboxCorrectionBalance(ctx, balance.balanceId)
		}
		// requireState checks every seeded row against the scopes applied so
		// far: none, the ledger scope, or both scopes
		const (
			appliedNone = iota
			appliedLedger
			appliedPreLedger
		)
		requireState := func(applied int) {
			t.Helper()
			for _, renewal := range renewals {
				want := renewal.netRevenue
				if (appliedLedger <= applied && renewal.zeroed) || (appliedPreLedger <= applied && renewal.zeroedByPre) {
					want = 0
				}
				if got := readAppleSandboxCorrectionRenewalRevenue(ctx, renewal.networkId, renewal.transactionId); got != want {
					t.Fatalf("renewal %s revenue %d, want %d", renewal.transactionId, got, want)
				}
			}
			for _, balance := range balances {
				got := readAppleSandboxCorrectionBalance(ctx, balance.balanceId)
				original := before[balance.balanceId]
				want := balance.subsidy
				if (appliedLedger <= applied && balance.zeroed) || (appliedPreLedger <= applied && balance.zeroedByPre) {
					want = 0
				}
				if got.subsidy != want || got.paid != (0 < want) {
					t.Fatalf("%s subsidy %d paid %t, want %d paid %t", balance.name, got.subsidy, got.paid, want, 0 < want)
				}
				// the entitlement is never touched
				if got.pro != original.pro || !got.startTime.Equal(original.startTime) || !got.endTime.Equal(original.endTime) || got.balanceByteCount != original.balanceByteCount {
					t.Fatalf("%s entitlement changed: %+v, was %+v", balance.name, got, original)
				}
			}
		}

		counts := appleSandboxCorrectionDryRun(t, ctx, "false")
		if counts.renewals != 3 || counts.creditBalances != 2 || counts.grantBalances != 2 {
			t.Fatalf("ledger-scope dry run counted %+v, want 3 renewals, 2 credit and 2 grant balances", counts)
		}
		if counts.renewalUsd != 11.97 || counts.creditUsd != 7.98 || counts.grantUsd != 127.68 {
			t.Fatalf("ledger-scope dry run sums %+v, want 11.97, 7.98 and 127.68 USD", counts)
		}

		// refusals: each rolls the whole correction back
		wrong := counts
		wrong.renewals = 2
		for _, refused := range []struct {
			name string
			run  *appleSandboxCorrectionRun
			want string
		}{
			{name: "count differs", run: &appleSandboxCorrectionRun{includePreLedger: "false", counts: wrong, isoLevel: pgx.Serializable}, want: "would change 3 renewals, the dry run reported 2"},
			{name: "not serializable", run: &appleSandboxCorrectionRun{includePreLedger: "false", counts: counts, isoLevel: pgx.ReadCommitted}, want: "requires a serializable transaction"},
			{name: "unknown scope", run: &appleSandboxCorrectionRun{includePreLedger: "yes", counts: counts, isoLevel: pgx.Serializable}, want: "must be true or false"},
			{name: "no counts", run: &appleSandboxCorrectionRun{includePreLedger: "false", isoLevel: pgx.Serializable, omitCounts: true}, want: "needs the dry run counts"},
		} {
			_, err := refused.run.apply(t, ctx)
			if err == nil || !strings.Contains(err.Error(), refused.want) {
				t.Fatalf("%s: correction error %v, want %q", refused.name, err, refused.want)
			}
			requireState(appliedNone)
		}

		receipt, err := (&appleSandboxCorrectionRun{includePreLedger: "false", counts: counts, isoLevel: pgx.Serializable}).apply(t, ctx)
		if err != nil {
			t.Fatalf("ledger-scope correction failed: %v", err)
		}
		if receipt != "include_pre_ledger=false renewals=3 credit_balances=2 grant_balances=2" {
			t.Fatalf("ledger-scope receipt %q", receipt)
		}
		requireState(appliedLedger)

		// idempotent: nothing is left, and a second run changes nothing
		if again := appleSandboxCorrectionDryRun(t, ctx, "false"); again.renewals != 0 || again.creditBalances != 0 || again.grantBalances != 0 {
			t.Fatalf("ledger-scope dry run after the correction counted %+v, want nothing", again)
		}
		if _, err := (&appleSandboxCorrectionRun{includePreLedger: "false", isoLevel: pgx.Serializable}).apply(t, ctx); err != nil {
			t.Fatalf("a repeated ledger-scope correction failed: %v", err)
		}
		requireState(appliedLedger)

		// the pre-ledger scope adds the pre-ledger TestFlight rows and their grant
		preCounts := appleSandboxCorrectionDryRun(t, ctx, "true")
		if preCounts.renewals != 2 || preCounts.creditBalances != 1 || preCounts.grantBalances != 1 {
			t.Fatalf("pre-ledger dry run counted %+v, want 2 renewals, 1 credit and 1 grant balance", preCounts)
		}
		if _, err := (&appleSandboxCorrectionRun{includePreLedger: "true", counts: preCounts, isoLevel: pgx.Serializable}).apply(t, ctx); err != nil {
			t.Fatalf("pre-ledger correction failed: %v", err)
		}
		requireState(appliedPreLedger)
		if again := appleSandboxCorrectionDryRun(t, ctx, "true"); again.renewals != 0 || again.creditBalances != 0 || again.grantBalances != 0 {
			t.Fatalf("pre-ledger dry run after the correction counted %+v, want nothing", again)
		}
	})
}
