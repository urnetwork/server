// SIGNALS.md §2.1a. These controls use the local channel semaphore and an injected
// source. The independent wire decoder also runs against the original detector.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The ordinal distinguishes simultaneous commands from the same scheduled signal.
type pgHostPermitWireGrant struct {
	Ordinal uint64 `json:"grant_ordinal"`
	Key     string `json:"signal_key"`
}

// A missing holder list keeps partial bookkeeping distinct from an empty address.
type pgHostPermitWireSnapshot struct {
	Status  string                  `json:"status"`
	Active  int                     `json:"active"`
	Holders []pgHostPermitWireGrant `json:"holders"`
}

// Decode only the promised private observation, independently of its producer.
type pgHostPermitWire struct {
	Schema       int                      `json:"schema"`
	AtRequest    pgHostPermitWireSnapshot `json:"at_request"`
	AtReturn     pgHostPermitWireSnapshot `json:"at_return"`
	ReleaseCount *uint64                  `json:"release_count"`
	Releases     []pgHostPermitWireGrant  `json:"releases"`
	Attribution  string                   `json:"attribution"`
}

// Accept the transport object from the actual immutable receipt or a local refusal.
func readPgHostPermitWire(t *testing.T, raw []byte) *pgHostPermitWire {
	t.Helper()
	var envelope struct {
		Transport struct {
			Holders *pgHostPermitWire `json:"host_permit_holders"`
		} `json:"transport_observation"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal("invalid host permit receipt")
	}
	return envelope.Transport.Holders
}

// Only the existing private context binding supplies a candidate catalog key.
func holdPgHostPermit(t *testing.T, limiter *hostCommandLimiter, address, key string) func() {
	t.Helper()
	release, err := limiter.acquire(context.WithValue(t.Context(), runSlotSignalContextKey{}, key), address)
	if err != nil {
		t.Fatal("synthetic command failed to acquire its own permit")
	}
	return release
}

// The second of two same-key commands returns first. Its exact release must
// survive later reuse and a different address's intervening release.
func TestPgHostPermitHoldersRecordExactReleaseInSamplerReceipt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pg, calls := pgRunSlotTimingFixture(t)
		limiter := pg.env.runner.(*runner).remoteCommands
		address := pg.env.cfg.hostByRole("pg-primary").addr(pg.env.cfg.addressMode)
		key := NewReindexDebrisSignal().Key()
		first := holdPgHostPermit(t, limiter, address, key)
		defer first()
		second := holdPgHostPermit(t, limiter, address, key)
		defer second()

		done := make(chan error, 1)
		go func() {
			_, err := runSignalInSlot(t.Context(), newRunSlotPool(runLoopMaxConcurrentSignals), pg, syntheticSettings(nil))
			done <- err
		}()
		synctest.Wait()
		if *calls != 0 || len(limiter.hostSlots(address)) != 2 {
			t.Fatal("scheduled sampler did not wait at the saturated local host")
		}
		holdPgHostPermit(t, limiter, "198.51.100.17", NewHostLoadSignal().Key())()
		time.Sleep(4 * time.Second)
		second()
		second()
		if err := <-done; err != nil || *calls != 1 || len(limiter.hostSlots(address)) != 1 {
			t.Fatal("host permit observation changed source completion or release ownership")
		}
		control, raw := pgTransportReadControlReceipt(t, pg.env)
		if !control.Complete || control.Samples != 12 || control.TransportObservation.Joined {
			t.Fatal("synthetic source lost its complete sample or acquired native authority")
		}
		first()
		wire := readPgHostPermitWire(t, raw)
		if wire == nil {
			t.Fatal("PG receipt omitted the actual host permit holders and release")
		}
		if wire.Schema != 1 || wire.Attribution != "unique_release" || wire.ReleaseCount == nil || *wire.ReleaseCount != 1 || len(wire.Releases) != 1 {
			t.Fatal("PG receipt did not identify the sole release from a full address")
		}
		before, after := wire.AtRequest, wire.AtReturn
		if before.Status != "ok" || before.Active != 2 || len(before.Holders) != 2 || after.Status != "ok" || after.Active != 1 || len(after.Holders) != 1 {
			t.Fatal("host permit snapshots lost a live grant or counted the sampler itself")
		}
		a, b := before.Holders[0], before.Holders[1]
		if a.Key != key || b.Key != key || a.Ordinal == 0 || a.Ordinal >= b.Ordinal || wire.Releases[0] != b || after.Holders[0] != a {
			t.Fatal("host permit release was attributed by signal key or channel FIFO instead of grant ownership")
		}
		for _, forbidden := range []string{address, "198.51.100.17", "secret-test-value", "transport_nonce"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatal("private command input entered the sampler receipt")
			}
		}
	})
}

// A canceled host wait leaves both original grants intact and provides no handoff.
func TestPgHostPermitHoldersCanceledWaitKeepsItsSourceBoundary(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pg, calls := pgRunSlotTimingFixture(t)
		limiter := pg.env.runner.(*runner).remoteCommands
		address := pg.env.cfg.hostByRole("pg-primary").addr(pg.env.cfg.addressMode)
		for _, key := range []string{"private-unlisted-host-holder", strings.Repeat("private-oversized", 20)} {
			release := holdPgHostPermit(t, limiter, address, key)
			defer release()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := runSignalInSlot(ctx, newRunSlotPool(runLoopMaxConcurrentSignals), pg, syntheticSettings(nil))
			done <- err
		}()
		synctest.Wait()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal("canceled sampler returned an unrelated error")
		}
		control, raw := pgTransportReadControlReceipt(t, pg.env)
		if *calls != 0 || control.Complete || control.TransportObservation.Spawn != "not_attempted" || len(limiter.hostSlots(address)) != 2 {
			t.Fatal("canceled host wait entered the source or released another grant")
		}
		wire := readPgHostPermitWire(t, raw)
		if wire == nil || wire.Attribution != "not_granted" || wire.ReleaseCount == nil || *wire.ReleaseCount != 0 || len(wire.Releases) != 0 || len(wire.AtRequest.Holders) != 2 || len(wire.AtReturn.Holders) != 2 {
			t.Fatal("canceled host request lost its two owners or claimed a release")
		}
		for i, holder := range wire.AtRequest.Holders {
			if holder.Key != "unknown" || holder != wire.AtReturn.Holders[i] {
				t.Fatal("private caller key became a catalog key or changed ownership")
			}
		}
		if strings.Contains(string(raw), "private-unlisted") || strings.Contains(string(raw), "private-oversized") {
			t.Fatal("untrusted holder identity entered the immutable receipt")
		}
	})
}

// Done is evaluated as part of the existing select. A barrier here forces
// releases inside its observed interval without depending on scheduler order.
type pgHostSelectBarrier struct {
	context.Context
	entered chan struct{}
	resume  chan struct{}
}

// This context is used only by one direct acquire. Its parent stays live.
func (self *pgHostSelectBarrier) Done() <-chan struct{} {
	close(self.entered)
	<-self.resume
	return self.Context.Done()
}

// More than two releases retain only the last two distinct release events.
// A final release timestamp or the last matching key cannot select a cause.
func TestPgHostPermitHoldersMultipleReleasesRemainAmbiguous(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newHostCommandLimiter(2)
		address := "203.0.113.17"
		first := holdPgHostPermit(t, limiter, address, NewReindexDebrisSignal().Key())
		second := holdPgHostPermit(t, limiter, address, NewHostPressureSignal().Key())
		ctx, observation := observePgSampleTransport(t.Context())
		barrier := &pgHostSelectBarrier{Context: ctx, entered: make(chan struct{}), resume: make(chan struct{})}
		done := make(chan error, 1)
		go func() {
			release, err := limiter.acquire(barrier, address)
			if err == nil {
				release()
			}
			done <- err
		}()
		<-barrier.entered
		second()
		holdPgHostPermit(t, limiter, address, NewHostLoadSignal().Key())()
		first()
		close(barrier.resume)
		if err := <-done; err != nil || len(limiter.hostSlots(address)) != 0 {
			t.Fatal("multiple-release control changed permit reuse")
		}
		raw, err := json.Marshal(map[string]any{"transport_observation": observation.receipt})
		if err != nil {
			t.Fatal("host permit observation did not encode")
		}
		wire := readPgHostPermitWire(t, raw)
		if wire == nil || wire.Attribution != "multiple_releases" || wire.ReleaseCount == nil || *wire.ReleaseCount != 3 || len(wire.Releases) != 2 || len(wire.AtRequest.Holders) != 2 || wire.AtReturn.Active != 0 {
			t.Fatal("multiple local releases became one exact releaser or unbounded history")
		}
		if wire.Releases[0].Key != NewHostLoadSignal().Key() || wire.Releases[1] != wire.AtRequest.Holders[0] {
			t.Fatal("bounded release tail lost chronological grant ownership")
		}
	})
}

// The channel can grant before that caller publishes bookkeeping. A sample
// seeing such an occupied slot must keep the unidentified owner explicit.
func TestPgHostPermitHoldersUnpublishedGrantRemainsUnavailable(t *testing.T) {
	limiter := newHostCommandLimiter(2)
	address := "192.0.2.41"
	semaphore := limiter.hostSlots(address)
	semaphore <- struct{}{}
	defer func() { <-semaphore }()
	ctx, observation := observePgSampleTransport(t.Context())
	release, err := limiter.acquire(ctx, address)
	if err != nil {
		t.Fatal("unknown ownership affected available capacity")
	}
	release()
	raw, err := json.Marshal(map[string]any{"transport_observation": observation.receipt})
	if err != nil {
		t.Fatal("host permit observation did not encode")
	}
	wire := readPgHostPermitWire(t, raw)
	if wire == nil || wire.Attribution != "unavailable" || wire.AtRequest.Active != 1 || wire.AtRequest.Status != "unavailable" || wire.AtRequest.Holders != nil || wire.AtReturn.Active != 1 || wire.AtReturn.Status != "unavailable" || wire.AtReturn.Holders != nil {
		t.Fatal("unpublished host grant became a complete empty or attributed snapshot")
	}
}

// A healthy, uncontended turn must preserve its original complete source and cadence.
func TestPgHostPermitHoldersImmediateGrantHasNoReleaser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pg, calls := pgRunSlotTimingFixture(t)
		if _, err := runSignalInSlot(t.Context(), newRunSlotPool(runLoopMaxConcurrentSignals), pg, syntheticSettings(nil)); err != nil {
			t.Fatal("uncontended sampler failed")
		}
		control, raw := pgTransportReadControlReceipt(t, pg.env)
		wire := readPgHostPermitWire(t, raw)
		if *calls != 1 || !control.Complete || control.Samples != 12 || wire == nil || wire.Attribution != "no_release_required" || wire.ReleaseCount == nil || *wire.ReleaseCount != 0 || wire.AtRequest.Status != "ok" || wire.AtReturn.Status != "ok" || len(wire.AtRequest.Holders) != 0 || len(wire.AtReturn.Holders) != 0 {
			t.Fatal("uncontended grant fabricated a releasing command or lost its source receipt")
		}
	})
}
