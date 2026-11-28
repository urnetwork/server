package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func parseBalanceGrantArgs(args []string) (docopt.Opts, error) {
	return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
}

// The command is a dry run unless --commit is given, and --commit needs the
// dry run's digest.
func TestBalanceGrantCommandLine(t *testing.T) {
	networkId := server.NewId().String()
	grant := []string{"balance", "grant", "--network_id=" + networkId, "--balance=500GiB", "--end=2026-11-26T01:00:00Z"}
	for _, args := range [][]string{
		grant,
		append(slices.Clone(grant), "--commit", "--expect=digest"),
	} {
		opts, err := parseBalanceGrantArgs(args)
		if err != nil {
			t.Fatal(args, err)
		}
		balance, _ := opts.Bool("balance")
		grantCommand, _ := opts.Bool("grant")
		moveRedeemed, _ := opts.Bool("move-redeemed")
		networkIdValue, _ := opts.String("--network_id")
		balanceValue, _ := opts.String("--balance")
		endValue, _ := opts.String("--end")
		commit, _ := opts.Bool("--commit")
		expect, _ := opts.String("--expect")
		if !balance || !grantCommand || moveRedeemed || networkIdValue != networkId || balanceValue != "500GiB" ||
			endValue != "2026-11-26T01:00:00Z" || commit != (len(args) == 7) || (expect == "digest") != commit {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"balance", "grant"},
		{"balance", "grant", "--network_id=" + networkId, "--balance=500GiB"},
		{"balance", "grant", "--network_id=" + networkId, "--end=2026-11-26T01:00:00Z"},
		{"balance", "grant", "--balance=500GiB", "--end=2026-11-26T01:00:00Z"},
		append(slices.Clone(grant), "--commit"),
		append(slices.Clone(grant), "--expect=digest"),
		append(slices.Clone(grant), "--dry-run"),
	} {
		if _, err := parseBalanceGrantArgs(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}
	// the commands that share its options are unchanged
	opts, err := parseBalanceGrantArgs([]string{"balance-code", "create", "--duration=24h", "--balance=10GiB", "--cost=1", "--email=synthetic@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if balance, _ := opts.Bool("balance"); balance {
		t.Fatal("balance-code create parsed as balance")
	}
	if balanceValue, _ := opts.String("--balance"); balanceValue != "10GiB" {
		t.Fatal("balance-code create --balance", balanceValue)
	}
	if _, err := parseBalanceGrantArgs([]string{"stats", "rollup-transfer", "--start=2026-11-01", "--end=2026-11-02"}); err != nil {
		t.Fatal(err)
	}
}

// An argument that does not parse runs nothing and exits 2.
func TestBalanceGrantRefusesUnparsedArguments(t *testing.T) {
	networkId := server.NewId().String()
	for _, c := range []struct {
		networkId string
		balance   string
		end       string
		option    string
	}{
		{networkId: "not-an-id", balance: "500GiB", end: "2026-11-26T01:00:00Z", option: "--network_id"},
		{networkId: networkId, balance: "lots", end: "2026-11-26T01:00:00Z", option: "--balance"},
		{networkId: networkId, balance: "500GiB", end: "2026-11-26", option: "--end"},
	} {
		var stdout, stderr bytes.Buffer
		exit := runBalanceGrant(context.Background(), &stdout, &stderr, c.networkId, c.balance, c.end, false, "")
		if exit != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), c.option) {
			t.Fatal("an unparsed argument", c, exit, stdout.String(), stderr.String())
		}
	}
}

// The digest binds the network, the grant and every active balance's durable
// identity, and not the bytes left, which the payer's traffic moves.
func TestBalanceGrantDigestBindsTheGrantAndTheActiveBalances(t *testing.T) {
	networkId := server.NewId()
	endTime := time.Date(2026, 11, 26, 1, 0, 0, 0, time.UTC)
	daily := &balanceGrantActiveBalance{
		BalanceId:             server.NewId(),
		StartTime:             time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC),
		EndTime:               endTime,
		StartBalanceByteCount: 30 * model.Gib,
		BalanceByteCount:      30 * model.Gib,
		GrantKind:             model.GrantKindFree,
	}
	digest := func(networkId server.Id, balanceByteCount model.ByteCount, endTime time.Time, activeBalances ...*balanceGrantActiveBalance) string {
		return balanceGrantDigest(networkId, balanceByteCount, endTime, activeBalances)
	}
	base := digest(networkId, 500*model.Gib, endTime, daily)
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(base) {
		t.Fatal("digest", base)
	}

	spent := *daily
	spent.BalanceByteCount = 1 * model.Gib
	if digest(networkId, 500*model.Gib, endTime, &spent) != base {
		t.Fatal("the bytes left changed the digest")
	}

	changed := func(change func(balance *balanceGrantActiveBalance)) *balanceGrantActiveBalance {
		balance := *daily
		change(&balance)
		return &balance
	}
	other := &balanceGrantActiveBalance{
		BalanceId:             server.NewId(),
		StartTime:             daily.StartTime,
		EndTime:               daily.EndTime,
		StartBalanceByteCount: model.Gib,
		BalanceByteCount:      model.Gib,
		GrantKind:             model.GrantKindReferral,
	}
	digests := map[string]string{base: "base"}
	for name, variant := range map[string]string{
		"network":       digest(server.NewId(), 500*model.Gib, endTime, daily),
		"bytes":         digest(networkId, 400*model.Gib, endTime, daily),
		"end":           digest(networkId, 500*model.Gib, endTime.Add(time.Microsecond), daily),
		"no balance":    digest(networkId, 500*model.Gib, endTime),
		"a new balance": digest(networkId, 500*model.Gib, endTime, daily, other),
		"balance id":    digest(networkId, 500*model.Gib, endTime, changed(func(balance *balanceGrantActiveBalance) { balance.BalanceId = server.NewId() })),
		"balance start": digest(networkId, 500*model.Gib, endTime, changed(func(balance *balanceGrantActiveBalance) { balance.StartTime = balance.StartTime.Add(time.Second) })),
		"balance end":   digest(networkId, 500*model.Gib, endTime, changed(func(balance *balanceGrantActiveBalance) { balance.EndTime = balance.EndTime.Add(time.Second) })),
		"balance size":  digest(networkId, 500*model.Gib, endTime, changed(func(balance *balanceGrantActiveBalance) { balance.StartBalanceByteCount += 1 })),
		"balance paid":  digest(networkId, 500*model.Gib, endTime, changed(func(balance *balanceGrantActiveBalance) { balance.Paid = true })),
		"balance pro":   digest(networkId, 500*model.Gib, endTime, changed(func(balance *balanceGrantActiveBalance) { balance.Pro = true })),
		"balance kind":  digest(networkId, 500*model.Gib, endTime, changed(func(balance *balanceGrantActiveBalance) { balance.GrantKind = model.GrantKindNone })),
	} {
		if previous, ok := digests[variant]; ok {
			t.Fatalf("a changed %s has the digest of %s", name, previous)
		}
		digests[variant] = name
	}
}

// Every transfer_balance row of the network, as the grant writes them.
type balanceGrantTestRow struct {
	balanceId             server.Id
	startTime             time.Time
	endTime               time.Time
	startBalanceByteCount model.ByteCount
	balanceByteCount      model.ByteCount
	netRevenue            model.NanoCents
	subsidyNetRevenue     model.NanoCents
	paid                  bool
	pro                   bool
	active                bool
	// empty when NULL
	grantKind        string
	hasPurchaseToken bool
}

func balanceGrantTestRows(ctx context.Context, networkId server.Id) map[server.Id]balanceGrantTestRow {
	rows := map[server.Id]balanceGrantTestRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					balance_id,
					start_time,
					end_time,
					start_balance_byte_count,
					balance_byte_count,
					net_revenue_nano_cents,
					subsidy_net_revenue_nano_cents,
					paid,
					pro,
					active,
					COALESCE(grant_kind, ''),
					purchase_token IS NOT NULL
				FROM transfer_balance
				WHERE network_id = $1
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var row balanceGrantTestRow
				server.Raise(result.Scan(
					&row.balanceId,
					&row.startTime,
					&row.endTime,
					&row.startBalanceByteCount,
					&row.balanceByteCount,
					&row.netRevenue,
					&row.subsidyNetRevenue,
					&row.paid,
					&row.pro,
					&row.active,
					&row.grantKind,
					&row.hasPurchaseToken,
				))
				rows[row.balanceId] = row
			}
		})
	})
	return rows
}

// The grant audit events of the network, by event id.
func balanceGrantTestAuditEvents(ctx context.Context, networkId server.Id) map[server.Id]string {
	events := map[server.Id]string{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT event_id, event_details FROM audit_network_event WHERE network_id = $1 AND event_type = $2`,
			networkId,
			auditEventTypeBalanceGranted,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var eventId server.Id
				var details string
				server.Raise(result.Scan(&eventId, &details))
				events[eventId] = details
			}
		})
	})
	return events
}

// Adds one grant of the kind with the grant writer, as the daily free grant
// and the referral bonus do.
func balanceGrantTestAddGrant(ctx context.Context, networkId server.Id, grantKind model.GrantKind, byteCount model.ByteCount, startTime time.Time, endTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.Raise(model.AddGrantTransferBalanceInTx(tx, ctx, networkId, grantKind, byteCount, startTime, endTime))
	})
}

// A synthetic network with one active client and the day's free grant.
func balanceGrantTestNetwork(ctx context.Context, freeByteCount model.ByteCount) (networkId server.Id, clientId server.Id) {
	networkId = server.NewId()
	clientId = server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "synthetic-grant-"+networkId.String(), server.NewId())
	model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "synthetic-grant-device", "synthetic")
	startTime, endTime := model.FreeGrantWindow(server.NowUtc())
	balanceGrantTestAddGrant(ctx, networkId, model.GrantKindFree, freeByteCount, startTime, endTime)
	return
}

var balanceGrantCommitLine = regexp.MustCompile(`(?m)^to grant exactly this: bringyourctl balance grant --network_id=(\S+) --balance=(\S+) --end=(\S+) --commit --expect=([0-9a-f]{64})$`)

// Runs the command as the process would, with the dry run in a read-only
// process, and returns its exit code and output.
func balanceGrantTestRun(t testing.TB, args []string) (exit int, stdout string, stderr string) {
	t.Helper()
	opts, err := parseBalanceGrantArgs(args)
	if err != nil {
		t.Fatal(args, err)
	}
	commit, _ := opts.Bool("--commit")
	if !commit {
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		defer server.Testing_ReleasePgReadOnly()
	}
	var stdoutBuffer, stderrBuffer bytes.Buffer
	exit = balanceGrantCommand(opts, &stdoutBuffer, &stderrBuffer)
	return exit, stdoutBuffer.String(), stderrBuffer.String()
}

// Runs the dry run and returns the digest its commit line names.
func balanceGrantTestDryRun(t testing.TB, args []string) string {
	t.Helper()
	exit, stdout, stderr := balanceGrantTestRun(t, args)
	match := balanceGrantCommitLine.FindStringSubmatch(stdout)
	if exit != 0 || stderr != "" || match == nil {
		t.Fatal("dry run", exit, stdout, stderr)
	}
	return match[4]
}

func balanceGrantTestArgs(networkId server.Id, balance string, endTime time.Time) []string {
	return []string{"balance", "grant", "--network_id=" + networkId.String(), "--balance=" + balance, "--end=" + endTime.UTC().Format(time.RFC3339)}
}

// The dry run writes nothing: its process refuses writes, and the network's
// balances and audit events are what they were. It prints the active balances,
// what the balance reads show, the row a commit inserts and the command that
// inserts exactly that.
func TestBalanceGrantDryRunWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, _ := balanceGrantTestNetwork(ctx, 30*model.Gib)
		rowsBefore := balanceGrantTestRows(ctx, networkId)
		if len(rowsBefore) != 1 {
			t.Fatal("fixture balances", len(rowsBefore))
		}
		endTime := server.NowUtc().Add(2 * time.Hour).Truncate(time.Second)
		args := balanceGrantTestArgs(networkId, "500GiB", endTime)

		opts, err := parseBalanceGrantArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		var stdout, stderr bytes.Buffer
		exit := balanceGrantCommand(opts, &stdout, &stderr)
		writeRefused := func() (refused bool) {
			defer func() {
				refused = recover() != nil
			}()
			balanceGrantTestAddGrant(ctx, networkId, model.GrantKindFree, model.Gib, server.NowUtc(), endTime)
			return false
		}()
		server.Testing_ReleasePgReadOnly()
		if exit != 0 || stderr.Len() != 0 || !writeRefused {
			t.Fatal("dry run", exit, stdout.String(), stderr.String(), writeRefused)
		}

		output := stdout.String()
		var dailyBalanceId server.Id
		for balanceId := range rowsBefore {
			dailyBalanceId = balanceId
		}
		match := balanceGrantCommitLine.FindStringSubmatch(output)
		if match == nil || match[1] != networkId.String() || match[2] != "500GiB" || match[3] != endTime.Format(time.RFC3339) {
			t.Fatal("the dry run printed no commit line for its arguments", output)
		}
		for _, want := range []string{
			"balance grant: dry run (read only)",
			"active balances: 1\n",
			"balance " + dailyBalanceId.String() + " kind free pro false paid false",
			"available 32212254720 (30gib), open escrow 0 (0b)",
			"bytes 536870912000 (500gib)",
			"grant_kind free, pro false, paid false, net revenue 0, subsidy net revenue 0",
			"state grant\n",
			"digest " + match[4] + "\n",
		} {
			if !strings.Contains(output, want) {
				t.Fatalf("the dry run did not print %q:\n%s", want, output)
			}
		}

		if rowsAfter := balanceGrantTestRows(ctx, networkId); len(rowsAfter) != 1 || rowsAfter[dailyBalanceId] != rowsBefore[dailyBalanceId] {
			t.Fatal("the dry run changed the network's balances", rowsAfter)
		}
		if events := balanceGrantTestAuditEvents(ctx, networkId); len(events) != 0 {
			t.Fatal("the dry run wrote an audit event", events)
		}

		// a refused dry run prints no digest and no commit line
		for _, refusedArgs := range [][]string{
			balanceGrantTestArgs(server.NewId(), "500GiB", endTime),
			balanceGrantTestArgs(networkId, "0GiB", endTime),
			balanceGrantTestArgs(networkId, "500GiB", server.NowUtc().Add(-time.Minute)),
		} {
			exit, stdout, stderr := balanceGrantTestRun(t, refusedArgs)
			if exit != 1 || !strings.Contains(stdout, "state refused") || strings.Contains(stdout, "digest ") ||
				strings.Contains(stdout, "to grant exactly") || !strings.Contains(stderr, errBalanceGrantRefused.Error()) {
				t.Fatal("refused dry run", refusedArgs, exit, stdout, stderr)
			}
		}
		if rowsAfter := balanceGrantTestRows(ctx, networkId); len(rowsAfter) != 1 {
			t.Fatal("a refused dry run changed the network's balances", rowsAfter)
		}
	})
}

// A commit with the dry run's digest writes exactly one transfer_balance row:
// the network, from the commit's time to the end, the bytes, no revenue, not
// paid, not Pro, a free grant; and one audit event. The network's balance
// reads include it at once.
func TestBalanceGrantCommitWritesOneGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, _ := balanceGrantTestNetwork(ctx, 30*model.Gib)
		rowsBefore := balanceGrantTestRows(ctx, networkId)
		summaryBefore := model.GetTransferBalanceSummary(ctx, networkId)
		endTime := server.NowUtc().Add(2 * time.Hour).Truncate(time.Second)
		args := balanceGrantTestArgs(networkId, "500GiB", endTime)
		digest := balanceGrantTestDryRun(t, args)

		commitStart := server.NowUtc()
		exit, stdout, stderr := balanceGrantTestRun(t, append(args, "--commit", "--expect="+digest))
		commitEnd := server.NowUtc()
		if exit != 0 || stderr != "" {
			t.Fatal("commit", exit, stdout, stderr)
		}

		rowsAfter := balanceGrantTestRows(ctx, networkId)
		newRows := []balanceGrantTestRow{}
		for balanceId, row := range rowsAfter {
			if previous, ok := rowsBefore[balanceId]; !ok {
				newRows = append(newRows, row)
			} else if previous != row {
				t.Fatal("the commit changed an existing balance", previous, row)
			}
		}
		if len(newRows) != 1 {
			t.Fatal("the commit wrote rows", len(newRows))
		}
		grant := newRows[0]
		if grant.startTime.Before(commitStart) || commitEnd.Before(grant.startTime) || !grant.endTime.Equal(endTime) ||
			grant.startBalanceByteCount != 500*model.Gib || grant.balanceByteCount != 500*model.Gib ||
			grant.netRevenue != 0 || grant.subsidyNetRevenue != 0 || grant.paid || grant.pro || !grant.active ||
			grant.grantKind != model.GrantKindFree || grant.hasPurchaseToken {
			t.Fatalf("the grant row %+v", grant)
		}

		events := balanceGrantTestAuditEvents(ctx, networkId)
		if len(events) != 1 {
			t.Fatal("audit events", events)
		}
		for eventId, details := range events {
			audit := &balanceGrantAudit{}
			if err := json.Unmarshal([]byte(details), audit); err != nil {
				t.Fatal(err)
			}
			if audit.BalanceId != grant.balanceId || audit.Digest != digest || audit.BalanceByteCount != 500*model.Gib ||
				!audit.EndTime.Equal(endTime) || audit.GrantKind != model.GrantKindFree {
				t.Fatal("audit details", details)
			}
			auditLine := "audit balance_granted event_id=" + eventId.String() + " network_id=" + networkId.String() +
				" balance_id=" + grant.balanceId.String() + " balance_byte_count=536870912000"
			if !strings.Contains(stdout, auditLine) || !strings.HasSuffix(stdout, " digest="+digest+"\n") {
				t.Fatal("commit output", stdout)
			}
		}
		if !strings.Contains(stdout, "granted: balance "+grant.balanceId.String()) ||
			!strings.Contains(stdout, "balance reads now: grant listed true, available 569083166720 (530gib)") {
			t.Fatal("commit output", stdout)
		}

		// the balance reads list it and count it
		listed := false
		for _, transferBalance := range model.GetActiveTransferBalances(ctx, networkId) {
			if transferBalance.BalanceId == grant.balanceId {
				listed = true
				if transferBalance.BalanceByteCount != 500*model.Gib || transferBalance.StartBalanceByteCount != 500*model.Gib ||
					transferBalance.Pro || transferBalance.Paid || transferBalance.GrantKind != model.GrantKindFree {
					t.Fatalf("the balance read %+v", transferBalance)
				}
			}
		}
		if !listed {
			t.Fatal("the balance reads do not list the grant")
		}
		summaryAfter := model.GetTransferBalanceSummary(ctx, networkId)
		if summaryAfter.BalanceByteCount != summaryBefore.BalanceByteCount+500*model.Gib ||
			summaryAfter.StartBalanceByteCount != summaryBefore.StartBalanceByteCount+500*model.Gib {
			t.Fatal("the balance summary", summaryBefore, summaryAfter)
		}
		if model.GetActiveTransferBalanceByteCount(ctx, networkId) != 530*model.Gib {
			t.Fatal("the active byte count", model.GetActiveTransferBalanceByteCount(ctx, networkId))
		}
		if model.IsProNetworkFresh(ctx, networkId) {
			t.Fatal("the grant conferred Pro")
		}
	})
}

// A commit whose digest is not the dry run's, or whose dry run is stale
// because the network's active balances changed since, writes nothing. A
// commit run again after it granted is stale too: its own grant changed the
// active balances.
func TestBalanceGrantCommitRefusesAStaleOrWrongDigest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, _ := balanceGrantTestNetwork(ctx, 30*model.Gib)
		endTime := server.NowUtc().Add(2 * time.Hour).Truncate(time.Second)
		args := balanceGrantTestArgs(networkId, "500GiB", endTime)
		digest := balanceGrantTestDryRun(t, args)
		refused := func(expect string, wantRowCount int) {
			t.Helper()
			exit, stdout, stderr := balanceGrantTestRun(t, append(slices.Clone(args), "--commit", "--expect="+expect))
			if exit != 1 || !strings.Contains(stderr, errBalanceGrantDigestMismatch.Error()) ||
				!strings.HasSuffix(stdout, "nothing was written\n") || strings.Contains(stdout, "audit balance_granted") {
				t.Fatal("refused commit", exit, stdout, stderr)
			}
			if rowCount := len(balanceGrantTestRows(ctx, networkId)); rowCount != wantRowCount {
				t.Fatal("a refused commit wrote a balance", rowCount, wantRowCount)
			}
		}

		// a wrong digest, and the right digest for other arguments
		refused(strings.Repeat("0", 64), 1)
		otherDigest := balanceGrantTestDryRun(t, balanceGrantTestArgs(networkId, "400GiB", endTime))
		refused(otherDigest, 1)
		if events := balanceGrantTestAuditEvents(ctx, networkId); len(events) != 0 {
			t.Fatal("a refused commit wrote an audit event", events)
		}

		// a stale digest: a balance started after the dry run
		balanceGrantTestAddGrant(ctx, networkId, model.GrantKindReferral, model.Gib, server.NowUtc(), endTime)
		refused(digest, 2)

		// a fresh dry run grants once, and its commit run again is stale
		digest = balanceGrantTestDryRun(t, args)
		if exit, stdout, stderr := balanceGrantTestRun(t, append(slices.Clone(args), "--commit", "--expect="+digest)); exit != 0 || stderr != "" {
			t.Fatal("commit", exit, stdout, stderr)
		}
		refused(digest, 3)
		if events := balanceGrantTestAuditEvents(ctx, networkId); len(events) != 1 {
			t.Fatal("audit events", events)
		}
		grantCount := 0
		for _, row := range balanceGrantTestRows(ctx, networkId) {
			if row.startBalanceByteCount == 500*model.Gib {
				grantCount += 1
			}
		}
		if grantCount != 1 {
			t.Fatal("grants", grantCount)
		}
	})
}

// The incident's shape: open contracts hold the payer's free grant in full, so
// a companion billed to it is refused for balance. After the grant, the same
// companion is admitted at once, in the process that just refused it, and its
// escrow is drawn from the grant.
func TestBalanceGrantFundsACompanionRefusedForBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// the payer's free grant is 1 MiB, and its origin contract holds all of it
		payerNetworkId, payerId := balanceGrantTestNetwork(ctx, model.Mib)
		otherNetworkId, otherId := balanceGrantTestNetwork(ctx, model.Gib)
		origin, err := model.CreateTransferEscrow(ctx, payerNetworkId, payerId, otherNetworkId, otherId, model.Mib)
		if err != nil {
			t.Fatal(err)
		}
		if available := model.GetActiveTransferBalanceByteCount(ctx, payerNetworkId); available != 0 {
			t.Fatal("the origin did not hold the payer's grant", available)
		}

		// the destination pays a companion: the payer
		_, err = model.CreateCompanionTransferEscrow(ctx, otherNetworkId, otherId, payerNetworkId, payerId, model.Mib, time.Hour)
		if !errors.Is(err, model.ErrContractBalanceExhausted) {
			t.Fatal("the companion of an exhausted payer was not refused for balance", err)
		}

		endTime := server.NowUtc().Add(time.Hour).Truncate(time.Second)
		args := balanceGrantTestArgs(payerNetworkId, "1GiB", endTime)
		digest := balanceGrantTestDryRun(t, args)
		exit, stdout, stderr := balanceGrantTestRun(t, append(args, "--commit", "--expect="+digest))
		if exit != 0 || stderr != "" {
			t.Fatal("commit", exit, stdout, stderr)
		}
		var grantBalanceId server.Id
		for _, row := range balanceGrantTestRows(ctx, payerNetworkId) {
			if row.startBalanceByteCount == model.Gib {
				grantBalanceId = row.balanceId
			}
		}
		if grantBalanceId == (server.Id{}) {
			t.Fatal("no grant")
		}

		companion, err := model.CreateCompanionTransferEscrow(ctx, otherNetworkId, otherId, payerNetworkId, payerId, model.Mib, time.Hour)
		if err != nil {
			t.Fatal("the companion was refused after the grant", err)
		}
		if companion.CompanionContractId == nil || *companion.CompanionContractId != origin.ContractId {
			t.Fatal("the companion's origin", companion.CompanionContractId)
		}
		escrow := model.GetTransferEscrow(ctx, companion.ContractId)
		if escrow == nil || len(escrow.Balances) != 1 || escrow.Balances[0].BalanceId != grantBalanceId ||
			escrow.Balances[0].BalanceByteCount != model.Mib {
			t.Fatalf("the companion's escrow %+v", escrow)
		}
		if available := model.GetActiveTransferBalanceByteCount(ctx, payerNetworkId); available != model.Gib-model.Mib {
			t.Fatal("the payer's available bytes", available)
		}
	})
}
