package connect

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Track A drain behavior (CONNECTDRAIN2.md §3.1-§3.4): excuse markers are
// written exactly once per drained resident, a draining exchange refuses new
// work (nominate declines and the connect handler 503s), and the drain
// enforces its deadline against a stuck connection while a well-behaved
// connection drains out.
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestExchangeDrainTrackA(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		settings := DefaultExchangeSettings()
		settings.KeyEventDelivery.Enabled = false
		settings.EnableDrainExcuse = true
		settings.EnableDrainCoordination = true
		settings.DrainAllTimeout = 2 * time.Second
		settings.DrainStragglerSweepTimeout = 500 * time.Millisecond
		settings.DrainOneTimeout = 10 * time.Millisecond

		exchange := NewExchange(
			ctx,
			"host0",
			"test",
			"test",
			map[int]int{5080: 5080},
			map[string]string{"host0": "127.0.0.1"},
			settings,
		)
		defer exchange.Close()

		// markers are written exactly once per resident
		clientId := server.NewId()
		residentId := server.NewId()
		resident := &Resident{
			clientId:   clientId,
			residentId: residentId,
		}
		exchange.markDrained(resident)
		taken := model.TakeDrainExcuse(ctx, clientId)
		connect.AssertEqual(t, true, taken != nil)
		connect.AssertEqual(t, residentId, *taken)
		// a second mark does not rewrite the consumed marker
		exchange.markDrained(resident)
		connect.AssertEqual(t, true, model.TakeDrainExcuse(ctx, clientId) == nil)

		// a stuck connection that never unregisters forces the drain deadline;
		// a well-behaved connection unregisters on cancel and drains out
		stuckClientId := server.NewId()
		exchange.registerConnection(stuckClientId, server.NewId(), func() {})
		goneClientId := server.NewId()
		goneConnectionId := server.NewId()
		exchange.registerConnection(goneClientId, goneConnectionId, func() {
			exchange.unregisterConnection(goneClientId, goneConnectionId)
		})

		drainStartTime := time.Now()
		exchange.Drain()
		drainDuration := time.Since(drainStartTime)
		connect.AssertEqual(t, true, exchange.IsDraining())
		// bounded by DrainAllTimeout (with scheduling margin), not the
		// unbounded pre-CONNECTDRAIN2 walk
		connect.AssertEqual(t, true, drainDuration < 10*time.Second)

		// the well-behaved connection drained out; the stuck one remains
		func() {
			exchange.stateLock.Lock()
			defer exchange.stateLock.Unlock()
			_, stuck := exchange.connections[stuckClientId]
			_, gone := exchange.connections[goneClientId]
			connect.AssertEqual(t, true, stuck)
			connect.AssertEqual(t, false, gone)
		}()

		// draining refuses new nominations before any model work
		connect.AssertEqual(t, false, exchange.NominateLocalResident(server.NewId(), server.NewId(), nil))

		// draining refuses new connections at the handler
		handler := NewConnectHandler(ctx, server.NewId(), exchange, DefaultConnectHandlerSettings())
		recorder := httptest.NewRecorder()
		handler.Connect(recorder, httptest.NewRequest("GET", "/", nil))
		connect.AssertEqual(t, http.StatusServiceUnavailable, recorder.Code)
	})
}

// The resident's first hop sync is sent as a full-snapshot StreamReset, so a
// reconcile-style client keeps its streams across a resident migration
// (CONNECTDRAIN2.md §3.3)
func TestStreamHopsToReset(t *testing.T) {
	sourceId := server.NewId()
	destinationId := server.NewId()
	streamId1 := server.NewId()
	streamId2 := server.NewId()

	// an endpoint hop (destination only) and an intermediary hop (both)
	endpointHop := model.NewStreamHop(nil, &destinationId, streamId1)
	intermediaryHop := model.NewStreamHop(&sourceId, &destinationId, streamId2)

	streamReset := streamHopsToReset([]model.StreamHop{endpointHop, intermediaryHop})
	connect.AssertEqual(t, 2, len(streamReset.Streams))

	connect.AssertEqual(t, true, streamReset.Streams[0].SourceId == nil)
	connect.AssertEqual(t, destinationId.Bytes(), streamReset.Streams[0].DestinationId)
	connect.AssertEqual(t, streamId1.Bytes(), streamReset.Streams[0].StreamId)

	connect.AssertEqual(t, sourceId.Bytes(), streamReset.Streams[1].SourceId)
	connect.AssertEqual(t, destinationId.Bytes(), streamReset.Streams[1].DestinationId)
	connect.AssertEqual(t, streamId2.Bytes(), streamReset.Streams[1].StreamId)

	// an empty hop set is the legacy clear-all reset
	connect.AssertEqual(t, 0, len(streamHopsToReset(nil).Streams))
}
