package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/task"
	"github.com/urnetwork/server/v2026/taskworker/work"
)

func requireOpenClosureScanStatus(t testing.TB, output *bytes.Buffer, expected string) {
	t.Helper()
	var result struct {
		Schema   int    `json:"schema"`
		Kind     string `json:"kind"`
		Status   string `json:"status"`
		PageSize int    `json:"page_size"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || result.Schema != 1 || result.Kind != "contract-open-closure-scan-v1" ||
		result.Status != expected || result.PageSize != 1024 {
		t.Fatal("direct scan command emitted the wrong completion status", err)
	}
}

// The real command shape passes one fresh local session to the direct scanner.
// A caller deadline is retained exactly; the command creates no task timeout.
func TestScheduleOpenClosuresCommandUsesDirectContext(t *testing.T) {
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"contracts", "schedule-open-closures"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	selected, _ := opts.Bool("schedule-open-closures")
	queued, _ := opts.Bool("queue-expiry")
	if !selected || queued {
		t.Fatal("direct scanner command did not parse independently of the queue command")
	}
	for _, withDeadline := range []bool{false, true} {
		var parent context.Context
		var cancel context.CancelFunc
		if withDeadline {
			parent, cancel = context.WithTimeout(context.Background(), 5*time.Minute)
		} else {
			parent, cancel = context.WithCancel(context.Background())
		}
		t.Cleanup(cancel)
		expectedDeadline, expectedBound := parent.Deadline()
		before := server.NowUtc().Truncate(time.Microsecond)
		var output bytes.Buffer
		var captured *session.ClientSession
		called := 0
		code := runScheduleOpenContractClosures(parent, &output, func(args *work.ScheduleOpenContractClosuresArgs, client *session.ClientSession) (*work.ScheduleOpenContractClosuresResult, error) {
			called++
			captured = client
			deadline, bound := client.Ctx.Deadline()
			if bound != expectedBound || bound && !deadline.Equal(expectedDeadline) || client.Ctx.Err() != nil ||
				client.ByJwt != nil || client.ClientAddress != "0.0.0.0:0" || args.PageSize != 1024 || args.StartedAt.IsZero() ||
				args.StartedAt.Before(before) || args.StartedAt.After(server.NowUtc()) {
				t.Fatal("direct scan changed operator context, session or fixed startup arguments")
			}
			return &work.ScheduleOpenContractClosuresResult{}, nil
		})
		if code != 0 || called != 1 || captured == nil || !errors.Is(captured.Ctx.Err(), context.Canceled) {
			t.Fatal("direct scanner was not called exactly once or its local session leaked")
		}
		cancel()
		requireOpenClosureScanStatus(t, &output, "scan_completed")
	}
}

func TestScheduleOpenClosuresCommandFailureDoesNotClaimCompletion(t *testing.T) {
	for _, failure := range []string{"error", "panic", "missing-result"} {
		var output bytes.Buffer
		code := runScheduleOpenContractClosures(t.Context(), &output, func(*work.ScheduleOpenContractClosuresArgs, *session.ClientSession) (*work.ScheduleOpenContractClosuresResult, error) {
			if failure == "panic" {
				panic("synthetic-sensitive-database-error")
			}
			if failure == "missing-result" {
				return nil, nil
			}
			return nil, errors.New("synthetic-sensitive-database-error")
		})
		if code != 1 || strings.Contains(output.String(), "synthetic-sensitive") {
			t.Fatal("failed direct scan claimed completion or printed raw database details")
		}
		requireOpenClosureScanStatus(t, &output, "scan_failed")
	}
}

func TestScheduleOpenClosuresCommandHonorsCancellation(t *testing.T) {
	for _, cancelBefore := range []bool{true, false} {
		ctx, cancel := context.WithCancel(t.Context())
		if cancelBefore {
			cancel()
		}
		called := false
		var output bytes.Buffer
		code := runScheduleOpenContractClosures(ctx, &output, func(_ *work.ScheduleOpenContractClosuresArgs, client *session.ClientSession) (*work.ScheduleOpenContractClosuresResult, error) {
			called = true
			cancel()
			if !errors.Is(client.Ctx.Err(), context.Canceled) {
				t.Fatal("operator cancellation did not reach the direct scanner session")
			}
			return &work.ScheduleOpenContractClosuresResult{}, nil
		})
		cancel()
		if code != 1 || called == cancelBefore {
			t.Fatal("canceled direct scan entered work early or claimed completion")
		}
		requireOpenClosureScanStatus(t, &output, "canceled")
	}
}

// The actual CLI adapter runs the real two-page scanner without a TaskWorker or
// enclosing transaction. Only ordinary child tasks are committed, and a rerun
// coalesces their exact identities and deadlines.
func TestScheduleOpenClosuresCommandPublishesChildrenDirectly(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := model.WithProviderWorkSessionSource(t.Context(), nil)
		networkId, sourceId, destinationId := server.NewId(), server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "synthetic-direct-close-scan", server.NewId())
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), sourceId, "synthetic-source", "synthetic")
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), destinationId, "synthetic-destination", "synthetic")
		ids := make([]server.Id, 1025)
		for index := range ids {
			ids[index] = server.NewId()
		}
		stamp := server.NowUtc().Truncate(time.Microsecond)
		past, future := stamp.Add(-time.Minute), stamp.Add(3*time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract (
				contract_id,source_network_id,source_id,destination_network_id,destination_id,
				transfer_byte_count,usage_origin_is_source,create_time,expiration_time)
				SELECT contract_id,$2,$3,$2,$4,100,true,$5,NULL FROM unnest($1::uuid[]) AS candidate(contract_id)`,
				ids, networkId, sourceId, destinationId, stamp))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[0], past))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, ids[1], future))
		})
		type queuedChild struct {
			id    server.Id
			args  string
			runAt time.Time
		}
		readChildren := func() map[server.Id]queuedChild {
			children := map[server.Id]queuedChild{}
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT task_id,function_name,run_once_key,args_json,run_at FROM pending_task`)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						var child queuedChild
						var function, key string
						server.Raise(rows.Scan(&child.id, &function, &key, &child.args, &child.runAt))
						var args work.CloseScheduledContractArgs
						if json.Unmarshal([]byte(child.args), &args) != nil || !args.Private ||
							function != work.NewScheduledContractClosureTaskTarget().TargetFunctionName() ||
							key != task.RunOnce("close_scheduled_contract", args.ContractId).String() || !child.runAt.Equal(args.Deadline) {
							t.Fatal("direct CLI queued a scanner or changed an ordinary child's deadline authority")
						}
						children[args.ContractId] = child
					}
				})
			})
			return children
		}
		before := server.NowUtc().Truncate(time.Microsecond)
		var output bytes.Buffer
		if code := runScheduleOpenContractClosures(ctx, &output, work.ScheduleOpenContractClosures); code != 0 {
			t.Fatal("actual direct scanner did not finish its committed child publication")
		}
		after := server.NowUtc()
		requireOpenClosureScanStatus(t, &output, "scan_completed")
		children := readChildren()
		if len(children) != len(ids) || !children[ids[0]].runAt.Equal(past) || !children[ids[1]].runAt.Equal(future) {
			t.Fatal("direct scan lost a page or replaced an explicit expiration")
		}
		fallback := children[ids[2]].runAt
		if fallback.Before(before.Add(model.DefaultContractExpiration)) || fallback.After(after.Add(model.DefaultContractExpiration)) {
			t.Fatal("direct scan did not capture the current default lifetime")
		}
		for _, id := range ids[2:] {
			child, found := children[id]
			if !found || !child.runAt.Equal(fallback) {
				t.Fatal("direct scan omitted a contract or restarted its fallback timestamp")
			}
		}
		output.Reset()
		if code := runScheduleOpenContractClosures(ctx, &output, work.ScheduleOpenContractClosures); code != 0 {
			t.Fatal("direct scan replay failed")
		}
		requireOpenClosureScanStatus(t, &output, "scan_completed")
		replayed := readChildren()
		if len(replayed) != len(children) {
			t.Fatal("direct replay duplicated or omitted child work")
		}
		for id, before := range children {
			after := replayed[id]
			if before.id != after.id || before.args != after.args || !before.runAt.Equal(after.runAt) {
				t.Fatal("direct replay changed a stable child or postponed its wake")
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var finished, untouched int
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM finished_task`).Scan(&finished))
			server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_contract WHERE outcome IS NULL
				AND NOT usage_unverified AND provider_usage IS NULL`).Scan(&untouched))
			if finished != 0 || untouched != len(ids) {
				t.Fatal("direct scan executed through a task claim or mutated close proofs")
			}
		})
	})
}
