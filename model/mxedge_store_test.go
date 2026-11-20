package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"

	"github.com/urnetwork/server"
)

// mxedge's store as its engine calls it, from its probe and its record batches, which run under no server handler:
// whatever a call raises answers as an error (QD-1090), and the probe reads on the main pool (QD-1133).

// Whatever a store call raises comes back from mxEdgeStoreRun as an error and never leaves it, since its callers are
// mxedge's own goroutines, with no server handler to catch a raise (QD-1090): server.Db's done error at a context
// that ended under the call, which it raised again before, answers as an error that is still the context's, as does
// connect's "Done"; an ordinary error answers as itself and any other value as its text, as before; and a call that
// raised nothing answers nil.
func TestMxEdgeStoreRunAnswersEveryRaiseAsAnError(t *testing.T) {
	storeErr := errors.New("synthetic store failure")
	// what server.Db raises when its context ends under it: the done sentinel joined to the acquire's and the
	// context's errors
	doneErr := errors.Join(server.DbContextDoneError, context.Canceled, context.Canceled)
	if !server.IsDoneError(doneErr) {
		t.Fatal("the synthetic stop is not a done error, so this test would prove nothing of one")
	}
	for _, c := range []struct {
		raised any
		// the answer's text, and an error it must wrap where there is one
		text string
		is   error
	}{
		{raised: doneErr, text: doneErr.Error(), is: context.Canceled},
		{raised: "Done", text: "Done"},
		{raised: storeErr, text: storeErr.Error(), is: storeErr},
		{raised: 42, text: "42"},
	} {
		reraised, err := func() (reraised any, err error) {
			defer func() {
				reraised = recover()
			}()
			return nil, mxEdgeStoreRun(func() {
				panic(c.raised)
			})
		}()
		if reraised != nil {
			t.Errorf("a raise of %v left the store's run: %v", c.raised, reraised)
		} else if err == nil || err.Error() != c.text || (c.is != nil && !errors.Is(err, c.is)) {
			t.Errorf("a raise of %v answered %v", c.raised, err)
		}
	}
	if err := mxEdgeStoreRun(func() {}); err != nil {
		t.Errorf("a call that raised nothing answered %v", err)
	}
}

// Every call whose context ended, as the engine's StoreTimeout or its close ends one on a slow Postgres, answers the
// context's error, a context past its deadline the deadline's, and never raises into its caller, mxedge's probe or
// its record batches, with no server handler to catch it (QD-1090); the store answers again once a context lives,
// the probe on the main pool. Before QD-1090 each call raised server.Db's done error at a canceled context, which
// ended the mxedge process.
func TestMxEdgeStoreAnswersAnEndedContextWithAnError(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg7Linked() {
			return
		}
		ctx := context.Background()
		store := NewMxEdgeStore(nil)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		expired, expire := context.WithDeadline(ctx, server.NowUtc().Add(-time.Minute))
		defer expire()
		// a record a row can hold, of an hour neither retired nor ahead, so the write reaches Postgres
		record := &mxedgeservice.MxConnectionRecord{
			NetworkId: sidecar.NewId(),
			StartTime: server.NowUtc(),
			ByteCount: 2048,
			Result:    "relayed",
		}
		calls := map[string]func(ctx context.Context) error{
			"Ping": store.Ping,
			"InsertConnectionRecords": func(ctx context.Context) error {
				return store.InsertConnectionRecords(ctx, []*mxedgeservice.MxConnectionRecord{record})
			},
		}
		for _, ended := range []struct {
			ctx context.Context
			err error
		}{
			{ctx: canceled, err: context.Canceled},
			{ctx: expired, err: context.DeadlineExceeded},
		} {
			for name, call := range calls {
				raised, err := func() (raised any, err error) {
					defer func() {
						raised = recover()
					}()
					return nil, call(ended.ctx)
				}()
				if raised != nil {
					t.Errorf("%s on a context ended by %v raised %v", name, ended.err, raised)
				} else if !errors.Is(err, ended.err) {
					t.Errorf("%s on a context ended by %v answered %v", name, ended.err, err)
				}
			}
		}
		if err := store.Ping(ctx); err != nil {
			t.Fatalf("the probe after the ended calls: %v", err)
		}
	})
}
