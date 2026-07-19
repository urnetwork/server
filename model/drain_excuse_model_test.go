package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Drain excuse markers are consumed exactly once (getdel) and expire on the
// ttl, so a marker excuses at most one reconnect within one excuse window
// (CONNECTDRAIN2.md §3.1).
func TestDrainExcuse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		clientId := server.NewId()
		residentId := server.NewId()

		// no marker: no excuse
		connect.AssertEqual(t, true, TakeDrainExcuse(ctx, clientId) == nil)

		// no drain: no provide-change excuse window
		connect.AssertEqual(t, false, HasDrainProvideChangeExcuse(ctx, clientId))

		// set then take returns the drained resident id
		SetDrainExcuse(ctx, clientId, residentId, 1*time.Minute)
		// the write armed the provide-change excuse window, observable by any
		// announce of the client from drain time on
		connect.AssertEqual(t, true, HasDrainProvideChangeExcuse(ctx, clientId))
		taken := TakeDrainExcuse(ctx, clientId)
		connect.AssertEqual(t, true, taken != nil)
		connect.AssertEqual(t, residentId, *taken)

		// consumed exactly once; the provide-change window remains (ttl-bounded)
		connect.AssertEqual(t, true, TakeDrainExcuse(ctx, clientId) == nil)
		connect.AssertEqual(t, true, HasDrainProvideChangeExcuse(ctx, clientId))

		// a second set overwrites the first
		otherResidentId := server.NewId()
		SetDrainExcuse(ctx, clientId, residentId, 1*time.Minute)
		SetDrainExcuse(ctx, clientId, otherResidentId, 1*time.Minute)
		taken = TakeDrainExcuse(ctx, clientId)
		connect.AssertEqual(t, true, taken != nil)
		connect.AssertEqual(t, otherResidentId, *taken)

		// the ttl bounds a stale marker
		SetDrainExcuse(ctx, clientId, residentId, 50*time.Millisecond)
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
		connect.AssertEqual(t, true, TakeDrainExcuse(ctx, clientId) == nil)
	})
}
