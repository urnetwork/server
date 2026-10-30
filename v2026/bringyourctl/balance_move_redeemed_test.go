package main

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func parseBalanceMoveRedeemedArgs(args []string) (docopt.Opts, error) {
	return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
}

// The command is a dry run unless --commit is given, and --commit needs the
// dry run's digest.
func TestBalanceMoveRedeemedCommandLine(t *testing.T) {
	balanceId := server.NewId().String()
	networkId := server.NewId().String()
	for _, args := range [][]string{
		{"balance", "move-redeemed", "--balance_id=" + balanceId, "--to_network_id=" + networkId},
		{"balance", "move-redeemed", "--balance_id=" + balanceId, "--to_network_id=" + networkId, "--commit", "--expect=digest"},
	} {
		opts, err := parseBalanceMoveRedeemedArgs(args)
		if err != nil {
			t.Fatal(args, err)
		}
		balance, _ := opts.Bool("balance")
		moveRedeemed, _ := opts.Bool("move-redeemed")
		balanceCode, _ := opts.Bool("balance-code")
		balanceIdValue, _ := opts.String("--balance_id")
		networkIdValue, _ := opts.String("--to_network_id")
		commit, _ := opts.Bool("--commit")
		expect, _ := opts.String("--expect")
		if !balance || !moveRedeemed || balanceCode || balanceIdValue != balanceId || networkIdValue != networkId ||
			commit != (len(args) == 6) || (expect == "digest") != commit {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"balance"},
		{"balance", "move-redeemed"},
		{"balance", "move-redeemed", "--balance_id=" + balanceId},
		{"balance", "move-redeemed", "--to_network_id=" + networkId},
		{"balance", "move-redeemed", "--balance_id=" + balanceId, "--to_network_id=" + networkId, "--commit"},
		{"balance", "move-redeemed", "--balance_id=" + balanceId, "--to_network_id=" + networkId, "--expect=digest"},
		{"balance", "move-redeemed", "--balance_id=" + balanceId, "--to_network_id=" + networkId, "--dry-run"},
	} {
		if _, err := parseBalanceMoveRedeemedArgs(args); err == nil {
			t.Fatal("an invalid command line was accepted", args)
		}
	}
	// the balance-code commands are unchanged
	if opts, err := parseBalanceMoveRedeemedArgs([]string{"balance-code", "check", "--secret=code"}); err != nil {
		t.Fatal(err)
	} else if balance, _ := opts.Bool("balance"); balance {
		t.Fatal("balance-code check parsed as balance")
	}
}

func testRedeemedBalanceMovePlan(state string) *model.RedeemedBalanceMovePlan {
	balanceId := server.NewId()
	toNetworkId := server.NewId()
	start := time.Date(2026, 3, 21, 21, 25, 33, 73780000, time.UTC)
	move := &model.RedeemedBalanceMove{
		BalanceId:             balanceId,
		BalanceCodeId:         server.NewId(),
		FromNetworkId:         server.NewId(),
		ToNetworkId:           toNetworkId,
		RedeemTime:            start,
		StartTime:             start,
		EndTime:               start.Add(36500 * 24 * time.Hour),
		StartBalanceByteCount: 10 * model.Tib,
		BalanceByteCount:      10 * model.Tib,
		NetRevenue:            model.UsdToNanoCents(40),
		Pro:                   true,
	}
	plan := &model.RedeemedBalanceMovePlan{
		Time:         time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC),
		BalanceId:    balanceId,
		ToNetworkId:  toNetworkId,
		State:        state,
		Move:         move,
		BalanceFound: true,
		CodeCount:    1,
		ToNetwork: &model.RedeemedBalanceMoveNetwork{
			NetworkId:   toNetworkId,
			NetworkName: "synthetic-target",
			AdminUserId: server.NewId(),
			SignIns:     []string{"password synthetic@example.com"},
		},
	}
	if state != model.RedeemedBalanceMoveStateRefused {
		plan.Digest = move.Digest()
	}
	return plan
}

// The report prints before any error. A dry run that can move ends with the
// exact commit command; a commit prints the audit line; a refusal or a
// failure exits 1; an id that does not parse exits 2 and runs nothing.
func TestRunBalanceMoveRedeemedOutput(t *testing.T) {
	run := func(commit bool, plan *model.RedeemedBalanceMovePlan, err error, balanceIdValue string, toNetworkIdValue string) (int, string, string, bool) {
		var stdout, stderr bytes.Buffer
		ran := false
		exit := runBalanceMoveRedeemed(&stdout, &stderr, balanceIdValue, toNetworkIdValue, commit, func(balanceId server.Id, toNetworkId server.Id) (*model.RedeemedBalanceMovePlan, error) {
			ran = true
			if balanceId.String() != balanceIdValue || toNetworkId.String() != toNetworkIdValue {
				t.Fatal("ids", balanceId, toNetworkId)
			}
			return plan, err
		})
		return exit, stdout.String(), stderr.String(), ran
	}

	// ids that do not parse
	plan := testRedeemedBalanceMovePlan(model.RedeemedBalanceMoveStateMove)
	if exit, _, stderr, ran := run(false, plan, nil, "not-an-id", plan.ToNetworkId.String()); exit != 2 || ran || !strings.Contains(stderr, "--balance_id") {
		t.Fatal("a bad balance id", exit, ran, stderr)
	}
	if exit, _, stderr, ran := run(false, plan, nil, plan.BalanceId.String(), "not-an-id"); exit != 2 || ran || !strings.Contains(stderr, "--to_network_id") {
		t.Fatal("a bad network id", exit, ran, stderr)
	}

	// a dry run that can move
	exit, stdout, stderr, _ := run(false, plan, nil, plan.BalanceId.String(), plan.ToNetworkId.String())
	commitLine := "to move exactly this: bringyourctl balance move-redeemed --balance_id=" + plan.BalanceId.String() +
		" --to_network_id=" + plan.ToNetworkId.String() + " --commit --expect=" + plan.Digest + "\n"
	if exit != 0 || stderr != "" || !strings.Contains(stdout, "digest "+plan.Digest) || !strings.HasSuffix(stdout, commitLine) ||
		!strings.Contains(stdout, "dry run (read only)") || !strings.Contains(stdout, "pro true") ||
		!strings.Contains(stdout, "password synthetic@example.com") || !strings.Contains(stdout, plan.Move.BalanceCodeId.String()) ||
		!strings.Contains(stdout, "from network "+plan.Move.FromNetworkId.String()+" (deleted)") {
		t.Fatal("dry run", exit, stdout, stderr)
	}

	// a refusal
	refused := testRedeemedBalanceMovePlan(model.RedeemedBalanceMoveStateRefused)
	refused.Refusals = []string{"the balance is used: synthetic"}
	exit, stdout, stderr, _ = run(false, refused, nil, refused.BalanceId.String(), refused.ToNetworkId.String())
	if exit != 1 || !strings.Contains(stdout, "refused: the balance is used: synthetic") || strings.Contains(stdout, "to move exactly") ||
		strings.Contains(stdout, "digest ") || !strings.Contains(stderr, model.ErrRedeemedBalanceMoveRefused.Error()) {
		t.Fatal("refusal", exit, stdout, stderr)
	}

	// a commit that moved
	committed := testRedeemedBalanceMovePlan(model.RedeemedBalanceMoveStateMove)
	committed.Committed = true
	eventId := model.RedeemedBalanceMoveEventId(committed.BalanceId, committed.ToNetworkId)
	committed.AuditEventId = &eventId
	proAfter := true
	committed.ToNetworkProAfter = &proAfter
	exit, stdout, stderr, _ = run(true, committed, nil, committed.BalanceId.String(), committed.ToNetworkId.String())
	auditLine := "audit redeemed_balance_moved event_id=" + eventId.String() + " balance_id=" + committed.BalanceId.String() +
		" balance_code_id=" + committed.Move.BalanceCodeId.String() + " from_network_id=" + committed.Move.FromNetworkId.String() +
		" to_network_id=" + committed.ToNetworkId.String() + " pro=true digest=" + committed.Digest + "\n"
	if exit != 0 || stderr != "" || !strings.HasSuffix(stdout, auditLine) || !strings.Contains(stdout, "moved: the balance and its code are on network") ||
		!strings.Contains(stdout, "pro after: true (cache refreshed)") || strings.Contains(stdout, "to move exactly") {
		t.Fatal("commit", exit, stdout, stderr)
	}

	// a commit run again
	moved := testRedeemedBalanceMovePlan(model.RedeemedBalanceMoveStateMoved)
	movedTime := time.Date(2026, 10, 29, 6, 5, 0, 0, time.UTC)
	movedEventId := model.RedeemedBalanceMoveEventId(moved.BalanceId, moved.ToNetworkId)
	moved.MovedTime = &movedTime
	moved.MovedEventId = &movedEventId
	exit, stdout, stderr, _ = run(true, moved, nil, moved.BalanceId.String(), moved.ToNetworkId.String())
	if exit != 0 || stderr != "" || !strings.Contains(stdout, "already moved at 2026-10-29T06:05:00Z") ||
		!strings.HasSuffix(stdout, "nothing was written\n") || strings.Contains(stdout, "audit redeemed_balance_moved") {
		t.Fatal("rerun", exit, stdout, stderr)
	}

	// a commit with another digest
	exit, stdout, stderr, _ = run(true, plan, model.ErrRedeemedBalanceMoveDigestMismatch, plan.BalanceId.String(), plan.ToNetworkId.String())
	if exit != 1 || !strings.Contains(stdout, "digest "+plan.Digest) || !strings.Contains(stderr, model.ErrRedeemedBalanceMoveDigestMismatch.Error()) {
		t.Fatal("mismatch", exit, stdout, stderr)
	}
	// a failure without a plan
	exit, _, stderr, _ = run(true, nil, errors.New("synthetic failure"), plan.BalanceId.String(), plan.ToNetworkId.String())
	if exit != 1 || !strings.Contains(stderr, "synthetic failure") {
		t.Fatal("failure", exit, stderr)
	}
}

// The command end to end on the test database: the dry run in a read-only
// process, the commit with the digest it printed, and the same commit again.
func TestBalanceMoveRedeemedCommandMovesTheBalance(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		exec := func(sql string, args ...any) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, sql, args...))
			})
		}
		balanceNetwork := func(balanceId server.Id) (networkId server.Id) {
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT network_id FROM transfer_balance WHERE balance_id = $1`, balanceId).Scan(&networkId))
			})
			return
		}

		// a code redeemed on a network that was deleted afterwards
		sourceNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, sourceNetworkId, "synthetic-command-source", server.NewId())
		targetNetworkId := server.NewId()
		model.Testing_CreateNetwork(ctx, targetNetworkId, "synthetic-command-target", server.NewId())
		code, err := model.CreateBalanceCode(ctx, 10*model.Tib, 36500*24*time.Hour, model.UsdToNanoCents(40), "synthetic-command-"+server.NewId().String(), "", "")
		if err != nil {
			t.Fatal(err)
		}
		redeemed, err := model.RedeemBalanceCode(&model.RedeemBalanceCodeArgs{Secret: code.Secret, NetworkId: sourceNetworkId}, ctx)
		if err != nil || redeemed.Error != nil {
			t.Fatal(err, redeemed)
		}
		balanceId := redeemed.TransferBalance.TransferBalanceId
		// grandfathered, as before July 2026
		exec(`UPDATE transfer_balance SET pro = true WHERE balance_id = $1`, balanceId)
		exec(`DELETE FROM network WHERE network_id = $1`, sourceNetworkId)

		args := []string{"balance", "move-redeemed", "--balance_id=" + balanceId.String(), "--to_network_id=" + targetNetworkId.String()}

		// the dry run: its process is read only
		opts, err := parseBalanceMoveRedeemedArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(server.Testing_ReleasePgReadOnly)
		var stdout, stderr bytes.Buffer
		exit := balanceMoveRedeemedCommand(opts, &stdout, &stderr)
		writeRefused := func() (refused bool) {
			defer func() {
				refused = recover() != nil
			}()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO audit_network_event (event_id, network_id, event_type) VALUES ($1, $2, 'synthetic_write_probe')`,
					server.NewId(),
					targetNetworkId,
				))
			}, server.OptNoRetry())
			return false
		}()
		server.Testing_ReleasePgReadOnly()
		if exit != 0 || stderr.Len() != 0 || !writeRefused {
			t.Fatal("dry run", exit, stdout.String(), stderr.String(), writeRefused)
		}
		match := regexp.MustCompile(`(?m)^to move exactly this: bringyourctl balance move-redeemed --balance_id=\S+ --to_network_id=\S+ --commit --expect=([0-9a-f]{64})$`).FindStringSubmatch(stdout.String())
		if match == nil {
			t.Fatal("the dry run printed no commit line", stdout.String())
		}
		digest := match[1]
		if balanceNetwork(balanceId) != sourceNetworkId {
			t.Fatal("the dry run moved the balance")
		}

		// the commit
		commitOpts, err := parseBalanceMoveRedeemedArgs(append(args, "--commit", "--expect="+digest))
		if err != nil {
			t.Fatal(err)
		}
		stdout.Reset()
		stderr.Reset()
		if exit := balanceMoveRedeemedCommand(commitOpts, &stdout, &stderr); exit != 0 || stderr.Len() != 0 {
			t.Fatal("commit", exit, stdout.String(), stderr.String())
		}
		eventId := model.RedeemedBalanceMoveEventId(balanceId, targetNetworkId)
		if !strings.Contains(stdout.String(), "audit redeemed_balance_moved event_id="+eventId.String()) ||
			!strings.Contains(stdout.String(), "pro after: true") {
			t.Fatal("commit output", stdout.String())
		}
		if balanceNetwork(balanceId) != targetNetworkId || !model.IsProNetwork(ctx, targetNetworkId) {
			t.Fatal("the commit did not move the balance")
		}

		// the same commit again writes nothing
		stdout.Reset()
		stderr.Reset()
		if exit := balanceMoveRedeemedCommand(commitOpts, &stdout, &stderr); exit != 0 || stderr.Len() != 0 ||
			!strings.Contains(stdout.String(), "already moved at") || !strings.HasSuffix(stdout.String(), "nothing was written\n") {
			t.Fatal("rerun", exit, stdout.String(), stderr.String())
		}
	})
}
