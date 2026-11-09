//go:build sidecar_localstack

// T3's Nets on the private test stores and the local stack's MinIO (sidecar/local), which these tests
// fail rather than skip without: sidecar-t3.sh's nightly cadence runs them, and its suites cadence when
// the stack answers.

package sidecart3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server"
)

// The private stores, a database of the test's own, and no migrations: nothing here reads server's
// schema.
func testEnv() *server.TestEnv {
	return &server.TestEnv{ApplyDbMigrations: false}
}

// A key no other run shares.
func runKey(prefix string) string {
	return prefix + strings.ToLower(sidecar.NewId().String())
}

// A toy row, one builder for both tiers: an actor creates an object, is refused a second create by its
// condition, reads the object back, and round-trips a Redis key.
func toyBuilder(net *sidecartest.Net) *fault.Schedule {
	key := runKey("toy/")
	redisKey := "{" + runKey("t3toy_") + "}k"
	return &fault.Schedule{
		Id:    "toy",
		Tiers: []fault.Tier{fault.TierT2, fault.TierT3},
		Steps: []*fault.Step{
			{Actor: "writerA", Action: func(ctx context.Context, net *sidecartest.Net) error {
				cluster, err := net.Host().Blobs.FirstCluster()
				if err != nil {
					return err
				}
				if _, err := cluster.PutIfNoneMatch(ctx, key, []byte("toy"), nil); err != nil {
					return err
				}
				if _, err := cluster.PutIfNoneMatch(ctx, key, []byte("toy"), nil); !errors.Is(err, sidecar.ErrBlobPrecondition) {
					return fmt.Errorf("a second create answered %v", err)
				}
				object, err := cluster.Get(ctx, key)
				if err != nil {
					return err
				}
				defer object.Body.Close()
				body, err := io.ReadAll(object.Body)
				if err != nil {
					return err
				}
				if string(body) != "toy" {
					return fmt.Errorf("read back %q", body)
				}
				redis := net.Host().Redis
				if err := redis.Set(ctx, redisKey, []byte("v"), time.Minute); err != nil {
					return err
				}
				value, found, err := redis.Get(ctx, redisKey)
				if err != nil || !found || string(value) != "v" {
					return fmt.Errorf("Redis read back %q, %t, %v", value, found, err)
				}
				return nil
			}},
		},
	}
}

// One builder passes on T2's fakes and on T3's stores (Q-TEST-1's check, passing).
func TestT3OneBuilderPassesOnBothTiers(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		divergence := Diverge(t, toyBuilder, nil)
		if divergence.Diverged() || divergence.Err() != nil {
			t.Errorf("%v", divergence.Err())
		}
	})
}

// A row that reads an object's age against the schedule's clock passes on T2, whose fake stamps objects
// with that clock, and fails on T3, whose MinIO keeps real time until TEST-M2-07 translates it: the
// divergence check names it a divergence.
func TestT3ARowFailingOnlyAtT3FailsTheDivergenceCheck(t *testing.T) {
	builder := func(net *sidecartest.Net) *fault.Schedule {
		key := runKey("age/")
		return &fault.Schedule{
			Id:    "age",
			Tiers: []fault.Tier{fault.TierT2, fault.TierT3},
			Steps: []*fault.Step{
				{Actor: "writerA", Action: func(ctx context.Context, net *sidecartest.Net) error {
					cluster, err := net.Host().Blobs.FirstCluster()
					if err != nil {
						return err
					}
					if _, err := cluster.PutIfNoneMatch(ctx, key, []byte("age"), nil); err != nil {
						return err
					}
					info, err := cluster.Head(ctx, key)
					if err != nil {
						return err
					}
					if !info.LastModified.Equal(net.Clock().Now()) {
						return fmt.Errorf("LastModified %s is not the schedule's %s", info.LastModified, net.Clock().Now())
					}
					return nil
				}},
			},
		}
	}
	testEnv().Run(t, func(tb testing.TB) {
		var divergence *Divergence
		recorder := record(t, func(tb testing.TB) {
			divergence = Diverge(tb, builder, nil)
		})
		if divergence == nil || !divergence.Diverged() {
			t.Fatalf("no divergence: %s", recorder.text())
		}
		if !strings.Contains(recorder.text(), "diverged: it passed on T2's fakes and failed on T3's stores") {
			t.Errorf("the check said %s", recorder.text())
		}
	})
}

// §22.8's unknown rule on a real store: ApplyThenDrop sends the PUT and loses its answer, so the object
// is there and its writer holds an unknown outcome; FailBeforeApply sends nothing.
func TestT3ApplyThenDropOnARealPutLeavesTheObject(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		cluster, err := net.Host().Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		ctx := net.Actors().Actor(net.Context(), "writerA").Context()
		dropped := runKey("dropped/")
		fired, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: dropped, Result: sidecartest.ApplyThenDrop})
		if err != nil {
			t.Fatal(err)
		}
		var unknown *sidecar.BlobUnknownError
		if _, err := cluster.PutIfNoneMatch(ctx, dropped, []byte("applied"), nil); !errors.As(err, &unknown) {
			t.Fatalf("the writer saw %v", err)
		}
		select {
		case <-fired:
		default:
			t.Error("the plan did not fire")
		}
		object, err := cluster.Get(sidecartest.InspectContext(ctx), dropped)
		if err != nil {
			t.Fatalf("the dropped PUT left no object: %v", err)
		}
		body, err := io.ReadAll(object.Body)
		object.Body.Close()
		if err != nil || string(body) != "applied" {
			t.Errorf("the object holds %q, %v", body, err)
		}

		failed := runKey("failed/")
		if _, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: failed, Result: sidecartest.FailBeforeApply}); err != nil {
			t.Fatal(err)
		}
		if _, err := cluster.PutIfNoneMatch(ctx, failed, []byte("never"), nil); !errors.As(err, &unknown) {
			t.Errorf("the writer saw %v", err)
		}
		if _, err := cluster.Head(sidecartest.InspectContext(ctx), failed); !errors.Is(err, sidecar.ErrBlobNotFound) {
			t.Errorf("a PUT that never left reads %v", err)
		}
		if unfired := net.Faults().Unfired(); len(unfired) != 0 {
			t.Errorf("never fired: %v", unfired)
		}
	})
}

// §22.8's pg-commit fault on server.Tx: a lost commit answer leaves the row committed while its caller
// sees an error, and a commit that never left leaves none. A commit outside any actor takes no plan.
func TestT3PgCommitDropLeavesACommittedRow(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `CREATE TABLE sidecart3_commit_probe (probe text PRIMARY KEY)`))
		})
		if _, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPgCommit, Result: sidecartest.ApplyThenDrop}); err != nil {
			t.Fatal(err)
		}
		if _, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPgCommit, Nth: 2, Result: sidecartest.FailBeforeApply}); err != nil {
			t.Fatal(err)
		}
		actorCtx := net.Actors().Actor(net.Context(), "writerA").Context()
		insert := func(probe string) (raised any) {
			defer func() {
				raised = recover()
			}()
			server.Tx(actorCtx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(actorCtx, `INSERT INTO sidecart3_commit_probe (probe) VALUES ($1)`, probe))
			})
			return nil
		}
		if raised := insert("dropped"); raised == nil {
			t.Error("the lost commit answer reached its caller as a success")
		} else if err, ok := raised.(error); !ok || !errors.Is(err, errTransport) {
			t.Errorf("the caller saw %v", raised)
		}
		if raised := insert("failed"); raised == nil {
			t.Error("the failed commit reached its caller as a success")
		}
		probes := map[string]bool{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT probe FROM sidecart3_commit_probe`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var probe string
					server.Raise(result.Scan(&probe))
					probes[probe] = true
				}
			})
		})
		if !probes["dropped"] {
			t.Error("the dropped commit left no row")
		}
		if probes["failed"] {
			t.Error("the commit that never left left a row")
		}
		if unfired := net.Faults().Unfired(); len(unfired) != 0 {
			t.Errorf("never fired: %v", unfired)
		}
	})
}

// §22.8's redis plans reach server's Redis: a lost answer leaves the write, a failed call leaves none.
func TestT3RedisPlansReachTheRealStore(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		redis := net.Host().Redis
		ctx := net.Actors().Actor(net.Context(), "writerA").Context()
		dropped := "{" + runKey("t3drop_") + "}k"
		failed := "{" + runKey("t3fail_") + "}k"
		for _, plan := range []*sidecartest.FaultPlan{
			{Op: sidecartest.FaultOpRedis, KeyPrefix: dropped, Result: sidecartest.ApplyThenDrop},
			{Op: sidecartest.FaultOpRedis, KeyPrefix: failed, Result: sidecartest.FailBeforeApply},
		} {
			if _, err := net.Faults().Add(plan); err != nil {
				t.Fatal(err)
			}
		}
		if err := redis.Set(ctx, dropped, []byte("v"), time.Minute); !errors.Is(err, errTransport) {
			t.Errorf("a lost answer read %v", err)
		}
		if err := redis.Set(ctx, failed, []byte("v"), time.Minute); !errors.Is(err, errTransport) {
			t.Errorf("a failed call read %v", err)
		}
		inspect := sidecartest.InspectContext(ctx)
		if _, found, err := redis.Get(inspect, dropped); err != nil || !found {
			t.Errorf("the lost answer's write is not there: %t, %v", found, err)
		}
		if _, found, err := redis.Get(inspect, failed); err != nil || found {
			t.Errorf("the failed call wrote: %t, %v", found, err)
		}
	})
}

// §22.8's http plans on a client of server's api: a lost answer leaves the handler run, a failed request
// reaches no handler.
func TestT3HttpPlansReachTheApi(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		net := NewNet(t, nil)
		defer net.Close()
		served := atomic.Int64{}
		api := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			served.Add(1)
			writer.Write([]byte("ok"))
		}))
		defer api.Close()
		client := &http.Client{Transport: FaultTransport(net, nil)}
		for _, plan := range []*sidecartest.FaultPlan{
			{Op: sidecartest.FaultOpHttp, KeyPrefix: "/toy/drop", Result: sidecartest.ApplyThenDrop},
			{Op: sidecartest.FaultOpHttp, KeyPrefix: "/toy/fail", Result: sidecartest.FailBeforeApply},
		} {
			if _, err := net.Faults().Add(plan); err != nil {
				t.Fatal(err)
			}
		}
		post := func(path string) error {
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, api.URL+path, strings.NewReader("body"))
			if err != nil {
				return err
			}
			response, err := client.Do(request)
			if err != nil {
				return err
			}
			io.Copy(io.Discard, response.Body)
			return response.Body.Close()
		}
		if err := post("/toy/drop"); !errors.Is(err, errTransport) {
			t.Errorf("a lost answer read %v", err)
		}
		if served.Load() != 1 {
			t.Errorf("the dropped request was served %d times", served.Load())
		}
		if err := post("/toy/fail"); !errors.Is(err, errTransport) {
			t.Errorf("a failed request read %v", err)
		}
		if err := post("/toy/pass"); err != nil {
			t.Errorf("an unplanned request: %v", err)
		}
		if served.Load() != 2 {
			t.Errorf("served %d requests, want the dropped and the unplanned", served.Load())
		}
	})
}

// What T3's stores cannot do is refused, not faked: a Delay at the schedule's Add, and a row on the
// fake contract issuer and ControlId sink, until server's exchange carries T3's clients.
func TestT3RefusesWhatItsStoresCannotDo(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		func() {
			net := NewNet(t, nil)
			defer net.Close()
			if _, err := net.Faults().Add(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, Result: sidecartest.Delay}); !errors.Is(err, errPlanNeedsAFake) {
				t.Errorf("a Delay was armed on T3: %v", err)
			}
		}()
		withClient := func(net *sidecartest.Net) *fault.Schedule {
			return &fault.Schedule{
				Id:    "client",
				Tiers: []fault.Tier{fault.TierT2, fault.TierT3},
				Steps: []*fault.Step{
					{Actor: "daemonM", Action: func(ctx context.Context, net *sidecartest.Net) error {
						net.AddClient("m", nil)
						return nil
					}},
				},
			}
		}
		result := runOn(t, withClient, NetSettings(t, nil))
		if result.Err == nil || !strings.Contains(result.Err.Error(), refusePlatformFakes) {
			t.Errorf("a row on the platform fakes ran on T3: %s", result)
		}
	})
}

// A failed attempt fails once: nothing builds or runs it again (R-TEST-9).
func TestT3AFailedAttemptIsNotRetried(t *testing.T) {
	testEnv().Run(t, func(tb testing.TB) {
		builds := atomic.Int64{}
		failing := func(net *sidecartest.Net) *fault.Schedule {
			builds.Add(1)
			return &fault.Schedule{
				Id:    "fails",
				Tiers: []fault.Tier{fault.TierT2, fault.TierT3},
				Steps: []*fault.Step{
					{Actor: "writerA", Action: func(ctx context.Context, net *sidecartest.Net) error {
						return errors.New("the toy row fails")
					}},
				},
			}
		}
		recorder := record(t, func(tb testing.TB) {
			Run(tb, failing, nil)
		})
		if !strings.Contains(recorder.text(), "the toy row fails") {
			t.Errorf("the failure read %s", recorder.text())
		}
		if builds.Load() != 1 {
			t.Errorf("the row was built %d times", builds.Load())
		}
	})
}

// Every FS row registered on T3, under §22.8's rules with the nightly seeds (sidecar-t3.sh's rows).
func TestT3Rows(t *testing.T) {
	registrations := []*fault.Registration{}
	for _, registration := range fault.Registered() {
		for _, tier := range registration.Tiers {
			if tier == fault.TierT3 {
				registrations = append(registrations, registration)
			}
		}
	}
	if len(registrations) == 0 {
		t.Log("no FS row registers a T3 builder yet: each lands with its row's task")
		return
	}
	testEnv().Run(t, func(tb testing.TB) {
		for _, registration := range registrations {
			RunRow(t, registration.Row, registration.Case, nil)
		}
	})
}
