// The operator command acknowledges verified model results, preserves caller
// cancellation, and reports partial failures without claiming a completed scan.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The old empty-string check confused an explicitly empty bound with omission.
func TestSynchronousClosureLimitRejectsExplicitlyInvalidBounds(t *testing.T) {
	for _, sample := range []struct {
		args []string
		want int
		bad  bool
	}{
		{},
		{args: []string{"--limit="}, bad: true},
		{args: []string{"--limit=-1"}, bad: true},
		{args: []string{"--limit=abc"}, bad: true},
		{args: []string{"--limit=0"}},
		{args: []string{"--limit=10"}, want: 10},
	} {
		opts, err := docopt.ParseArgs(bringyourctlUsage, append([]string{"contracts", "schedule-open-closures"}, sample.args...), "synthetic")
		if err != nil {
			t.Fatal(err)
		}
		limit, err := synchronousContractClosureLimit(opts)
		if (err != nil) != sample.bad || !sample.bad && limit != sample.want {
			t.Fatal("invalid or empty bound became an unbounded scan", sample.args, limit, err)
		}
	}
}

// Read the actual JSON stream, including every per-contract acknowledgement.
func readSynchronousClosureOutput(t testing.TB, output *bytes.Buffer) []map[string]any {
	t.Helper()
	decoder := json.NewDecoder(output)
	rows := []map[string]any{}
	for {
		var row map[string]any
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			return rows
		}
		if err != nil {
			t.Fatal("invalid closure output", err)
		}
		rows = append(rows, row)
	}
}

// The existing command selects the synchronous adapter with a bounded option;
// no task session or task timeout may replace the operator's context.
func TestScheduleOpenClosuresCommandUsesSynchronousContext(t *testing.T) {
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"contracts", "schedule-open-closures", "--limit=10"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	selected, _ := opts.Bool("schedule-open-closures")
	limit, _ := opts.Int("--limit")
	if !selected || limit != 10 {
		t.Fatal("bounded synchronous command did not parse", opts)
	}
	for _, withDeadline := range []bool{false, true} {
		parent, cancel := context.WithCancel(t.Context())
		if withDeadline {
			cancel()
			parent, cancel = context.WithTimeout(t.Context(), time.Minute)
		}
		before := server.NowUtc()
		var captured context.Context
		var output bytes.Buffer
		code := runSynchronousContractClosures(parent, &output, limit, func(ctx context.Context, options model.ContractClosureScanOptions, observe func(*model.ContractDeadlineReconciliation, error) error) (*model.ContractClosureScanResult, error) {
			captured = ctx
			deadline, bound := ctx.Deadline()
			expected, expectedBound := parent.Deadline()
			if bound != expectedBound || bound && !deadline.Equal(expected) || options.Limit != 10 || options.PageSize != 1024 || options.At.Before(before) || options.At.After(server.NowUtc()) {
				t.Fatal("adapter changed caller deadline or scan boundary", options)
			}
			if err := observe(&model.ContractDeadlineReconciliation{ContractId: server.NewId(), Outcome: model.ContractOutcomeSettled}, nil); err != nil {
				t.Fatal(err)
			}
			return &model.ContractClosureScanResult{Visited: 1, Closed: 1}, nil
		})
		cancel()
		rows := readSynchronousClosureOutput(t, &output)
		if code != 0 || captured == nil || captured.Err() == nil || len(rows) != 2 || rows[0]["status"] != "closed" || rows[1]["status"] != "scan_completed" {
			t.Fatal("synchronous result or context custody lost", code, rows)
		}
	}
}

// Neither a returned error, panic, missing result nor partial result may be
// mislabeled as a completed scan. A failed close has its own explicit record.
func TestSynchronousClosureCommandFailureAcknowledgement(t *testing.T) {
	for _, failure := range []string{"error", "panic", "missing", "partial", "missing-close"} {
		var output bytes.Buffer
		code := runSynchronousContractClosures(t.Context(), &output, 10, func(_ context.Context, _ model.ContractClosureScanOptions, observe func(*model.ContractDeadlineReconciliation, error) error) (*model.ContractClosureScanResult, error) {
			switch failure {
			case "panic":
				panic("synthetic failure")
			case "missing":
				return nil, nil
			case "partial":
				if err := observe(&model.ContractDeadlineReconciliation{ContractId: server.NewId()}, errors.New("synthetic close failure")); err != nil {
					t.Fatal(err)
				}
				return &model.ContractClosureScanResult{Visited: 1, Failed: 1}, nil
			case "missing-close":
				return nil, observe(nil, nil)
			default:
				return nil, errors.New("synthetic scan failure")
			}
		})
		rows := readSynchronousClosureOutput(t, &output)
		if code != 1 || len(rows) == 0 || rows[len(rows)-1]["status"] != "scan_failed" {
			t.Fatal("failed scan claimed completion", failure, code, rows)
		}
		if failure == "partial" && (len(rows) != 2 || rows[0]["status"] != "failed" || rows[0]["error"] != "synthetic close failure") {
			t.Fatal("partial failure lost its cause", rows)
		}
	}
}

// Cancellation reaches the model and a partial scan retains its closed count.
func TestSynchronousClosureCommandCancellation(t *testing.T) {
	for _, before := range []bool{true, false} {
		ctx, cancel := context.WithCancel(t.Context())
		if before {
			cancel()
		}
		called := false
		var output bytes.Buffer
		code := runSynchronousContractClosures(ctx, &output, 10, func(ctx context.Context, _ model.ContractClosureScanOptions, _ func(*model.ContractDeadlineReconciliation, error) error) (*model.ContractClosureScanResult, error) {
			called = true
			cancel()
			return &model.ContractClosureScanResult{Visited: 1, Closed: 1}, ctx.Err()
		})
		cancel()
		rows := readSynchronousClosureOutput(t, &output)
		if code != 1 || called == before || len(rows) != 1 || rows[0]["status"] != "canceled" {
			t.Fatal("canceled scan continued or claimed completion", code, rows)
		}
		if !before && rows[0]["result"].(map[string]any)["closed"] != float64(1) {
			t.Fatal("cancellation discarded committed progress", rows)
		}
	}
}

// The real CLI adapter and model commit closure before output without creating
// any task, and leave an explicit future expiration untouched.
func TestSynchronousClosureCommandCommitsWithoutTasks(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-synchronous-close", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
		ids := []server.Id{server.NewId(), server.NewId(), server.NewId()}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
				contract_id,source_network_id,source_id,destination_network_id,destination_id,
				transfer_byte_count,usage_origin_is_source,expiration_time)
				SELECT contract_id,$2,$3,$2,$4,100,true,NULL FROM unnest($1::uuid[]) AS candidate(contract_id)`, ids, networkId, sourceId, destinationId))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[1], server.NowUtc().Add(time.Hour)))
		})
		var output bytes.Buffer
		if code := runSynchronousContractClosures(ctx, &output, 10, model.CloseOpenContractsSynchronously); code != 0 {
			t.Fatal("synchronous command failed", code, output.String())
		}
		rows := readSynchronousClosureOutput(t, &output)
		if len(rows) != 3 || rows[0]["status"] != "closed" || rows[1]["status"] != "closed" || rows[2]["status"] != "scan_completed" {
			t.Fatal("unverified or deferred contract acknowledged as closed", rows)
		}
		server.Db(ctx, func(conn server.PgConn) {
			var closed, open, queued int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FILTER (WHERE outcome IS NOT NULL),
				count(*) FILTER (WHERE outcome IS NULL) FROM transfer_contract WHERE contract_id=ANY($1)`, ids).Scan(&closed, &open))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM pending_task`).Scan(&queued))
			if closed != 2 || open != 1 || queued != 0 {
				t.Fatal("command deferred settlement to a task", closed, open, queued)
			}
		})
	})
}
