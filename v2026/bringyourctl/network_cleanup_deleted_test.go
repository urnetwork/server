package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Removes the fixture's network, user and sign-ins as every deletion did
// before 2026-11-16, with nothing queued after it.
func removeNetworkAsBefore(t testing.TB, fixture *model.Testing_DeletedNetworkFixture) {
	t.Helper()
	ctx := t.Context()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_user WHERE user_id = $1`, fixture.UserId))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network WHERE network_id = $1`, fixture.NetworkId))
	})
}

// `bringyourctl network cleanup-deleted`: the dry run finds the networks that
// deletions before 2026-11-16 left behind, writes their sorted list, counts
// what they left and changes nothing; the commit refuses a digest that does
// not name the list, and with the dry run's digest cleans each listed network,
// queuing the taskworker for the clients it deactivated. A network that exists
// is never listed or touched.
func TestNetworkCleanupDeletedDryRunThenCommit(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		deletedA := model.Testing_CreateDeletedNetworkFixture(ctx, "synthetic-a-"+server.NewId().String()[24:])
		deletedB := model.Testing_CreateDeletedNetworkFixture(ctx, "synthetic-b-"+server.NewId().String()[24:])
		live := model.Testing_CreateDeletedNetworkFixture(ctx, "synthetic-live-"+server.NewId().String()[24:])
		removeNetworkAsBefore(t, deletedA)
		removeNetworkAsBefore(t, deletedB)
		// a deleted network that left rows but no client or device
		rowsOnlyNetworkId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO account_preferences (network_id, product_updates) VALUES ($1, true)`, rowsOnlyNetworkId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_referral_code (network_id, referral_code) VALUES ($1, $2)`, rowsOnlyNetworkId, "SYN"+server.NewId().String()[24:]))
		})
		rowsOnlyCount := func() (n int64) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT (SELECT count(*) FROM account_preferences WHERE network_id = $1) + (SELECT count(*) FROM network_referral_code WHERE network_id = $1)`,
					rowsOnlyNetworkId,
				).Scan(&n))
			})
			return
		}
		rowsBefore := map[server.Id]map[string]int64{}
		for _, fixture := range []*model.Testing_DeletedNetworkFixture{deletedA, deletedB, live} {
			rowsBefore[fixture.NetworkId] = model.Testing_CountDeletedNetworkFixtureRows(ctx, fixture)
		}

		idsFile := filepath.Join(t.TempDir(), "deleted-network-ids.txt")
		parse := func(args ...string) *networkCleanupDeletedOptions {
			opts, err := (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, append([]string{"network", "cleanup-deleted"}, args...), "test")
			if err != nil {
				t.Fatal(err)
			}
			options, err := parseNetworkCleanupDeletedOptions(opts)
			if err != nil {
				t.Fatal(err)
			}
			return options
		}

		var stdout, stderr bytes.Buffer
		options := parse("--ids_file="+idsFile, "--page=1", "--count_chunk=1")
		if code := runNetworkCleanupDeleted(ctx, &stdout, &stderr, options); code != 0 {
			t.Fatalf("dry run exited %d: %s %s", code, stdout.String(), stderr.String())
		}
		wantIds := sortedUniqueNetworkIds([]server.Id{deletedA.NetworkId, deletedB.NetworkId, rowsOnlyNetworkId})
		listed, err := readNetworkIdsFile(idsFile)
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, listed, wantIds)
		digest := networkCleanupDeletedDigest(wantIds)
		out := stdout.String()
		if !strings.Contains(out, "deleted networks: 3\n") || !strings.Contains(out, "digest: "+digest+"\n") || !strings.Contains(out, "clients: 4 active") {
			t.Fatalf("dry run output = %q", out)
		}
		connect.AssertEqual(t, rowsOnlyCount(), int64(2))
		for _, fixture := range []*model.Testing_DeletedNetworkFixture{deletedA, deletedB, live} {
			connect.AssertEqual(t, model.Testing_CountDeletedNetworkFixtureRows(ctx, fixture), rowsBefore[fixture.NetworkId])
		}

		// a digest that does not name the list changes nothing
		stdout.Reset()
		stderr.Reset()
		options = parse("--ids_file="+idsFile, "--commit", "--expect=0000000000000000")
		if code := runNetworkCleanupDeleted(ctx, &stdout, &stderr, options); code != 1 {
			t.Fatalf("a commit with another digest exited %d", code)
		}
		for _, fixture := range []*model.Testing_DeletedNetworkFixture{deletedA, deletedB, live} {
			connect.AssertEqual(t, model.Testing_CountDeletedNetworkFixtureRows(ctx, fixture), rowsBefore[fixture.NetworkId])
		}

		// the commit: everything but the clients, which wait, deactivated
		stdout.Reset()
		stderr.Reset()
		options = parse("--ids_file="+idsFile, "--network_pause=0s", "--count_chunk=2", "--commit", "--expect="+digest)
		if code := runNetworkCleanupDeleted(ctx, &stdout, &stderr, options); code != 0 {
			t.Fatalf("commit exited %d: %s %s", code, stdout.String(), stderr.String())
		}
		out = stdout.String()
		if strings.Count(out, "queued for the taskworker") != 2 || !strings.Contains(out, "1 networks without clients or devices: account_preferences=1 network_referral_code=1\n") || !strings.Contains(out, "done: 3 networks\n") {
			t.Fatalf("commit output = %q", out)
		}
		connect.AssertEqual(t, rowsOnlyCount(), int64(0))
		for _, fixture := range []*model.Testing_DeletedNetworkFixture{deletedA, deletedB} {
			rows := model.Testing_CountDeletedNetworkFixtureRows(ctx, fixture)
			connect.AssertEqual(t, rows["network_client"], int64(2))
			connect.AssertEqual(t, rows["network_client_active"], int64(0))
			for _, table := range []string{"account_preferences", "account_api_key", "network_referral_code", "auth_code", "user_auth_reset"} {
				if 0 < rows[table] {
					t.Fatalf("%s still has %d rows after the commit", table, rows[table])
				}
			}
			var queued int
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(
					ctx,
					`SELECT count(*) FROM pending_task WHERE args_json::jsonb->'deleted_network'->>'network_id' = $1`,
					fixture.NetworkId.String(),
				).Scan(&queued))
			})
			connect.AssertEqual(t, queued, 1)
		}
		connect.AssertEqual(t, model.Testing_CountDeletedNetworkFixtureRows(ctx, live), rowsBefore[live.NetworkId])

		// --start_after resumes after a listed network
		stdout.Reset()
		options = parse("--ids_file="+idsFile, "--start_after="+wantIds[0].String(), "--commit", "--expect="+digest)
		if code := runNetworkCleanupDeleted(ctx, &stdout, &stderr, options); code != 0 || !strings.Contains(stdout.String(), "done: 2 networks\n") {
			t.Fatalf("a resumed commit exited %d: %s", code, stdout.String())
		}
		if slices.Contains(wantIds, live.NetworkId) {
			t.Fatal("the live network was listed")
		}
		if _, err := os.Stat(idsFile); err != nil {
			t.Fatal(err)
		}
	})
}

// The command's usage parses in both forms, with every option, and a commit
// needs the list the dry run wrote.
func TestNetworkCleanupDeletedUsageParses(t *testing.T) {
	parse := func(args ...string) (*networkCleanupDeletedOptions, error) {
		opts, err := (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, append([]string{"network", "cleanup-deleted"}, args...), "test")
		if err != nil {
			t.Fatal(err)
		}
		return parseNetworkCleanupDeletedOptions(opts)
	}
	options, err := parse()
	if err != nil || options.commit || options.pageCount != 10000 || options.countChunk != 200 || options.cleanup.ClientBatchCount != 200 {
		t.Fatalf("defaults = %+v, %v", options, err)
	}
	options, err = parse(
		"--ids_file=ids.txt", "--page=5", "--count_chunk=7", "--client_batch=3", "--row_batch=11",
		"--network_pause=1s", "--start_after=00000000-0000-0000-0000-000000000001", "--max_networks=9",
		"--commit", "--expect=0123456789abcdef",
	)
	if err != nil || !options.commit || options.expectDigest != "0123456789abcdef" || options.idsFile != "ids.txt" ||
		options.pageCount != 5 || options.countChunk != 7 || options.cleanup.ClientBatchCount != 3 ||
		options.cleanup.RowBatchCount != 11 || options.pause != time.Second || options.startAfter == nil || options.maxNetworks != 9 {
		t.Fatalf("options = %+v, %v", options, err)
	}
	if _, err := parse("--commit", "--expect=0123456789abcdef"); err == nil {
		t.Fatal("a commit without the dry run's list parsed")
	}
	if _, err := parse("--count_chunk=0"); err == nil {
		t.Fatal("a zero chunk parsed")
	}
	// the digest names the exact list, in order
	a, b := server.NewId(), server.NewId()
	if networkCleanupDeletedDigest([]server.Id{a, b}) == networkCleanupDeletedDigest([]server.Id{a}) ||
		networkCleanupDeletedDigest(sortedUniqueNetworkIds([]server.Id{b, a, b})) != networkCleanupDeletedDigest(sortedUniqueNetworkIds([]server.Id{a, b})) {
		t.Fatal("the digest does not name the sorted list")
	}
}
