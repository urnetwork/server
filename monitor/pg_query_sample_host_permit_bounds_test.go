// Prove the finite host-permit projection yields to existing source evidence
// and keeps unsupported or ambiguous semaphore ownership explicit.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// The normal scheduled entry supplies the catalog key, and the normal runner
// holds its address permit until the shared reservation finishes cleanup.
type pgHostPermitScheduledCommand struct {
	Signal
	runner *runner
	target *host
}

// The runner's transport and shared store are both in-memory controls.
func (self *pgHostPermitScheduledCommand) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	_, err := self.runner.sshTimeout(ctx, self.target, "synthetic command", "synthetic input", time.Minute)
	return nil, err
}

// The source callback has already returned, but the owning command's release
// is still pending. Only joined shared cleanup opens the local permit.
func TestPgHostPermitHoldersIncludeSharedCleanupUntilActualRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pg, calls := pgRunSlotTimingFixture(t)
		cfg := *pg.env.cfg
		target := cfg.hostByRole("pg-primary")
		cleanup, finish := make(chan struct{}), make(chan struct{})
		cfg.sharedSshAdmission = &sshAdmissionPhaseFixture{acquireCommand: func(context.Context, string) (func() error, error) {
			return func() error { close(cleanup); <-finish; return nil }, nil
		}}
		command := &pgHostPermitScheduledCommand{Signal: NewReindexDebrisSignal(), runner: newRunner(&cfg), target: target}
		command.runner.runSSH = func(context.Context, []string, string) (string, string, error) { return "", "", nil }
		pool := newRunSlotPool(runLoopMaxConcurrentSignals)
		held := holdPgHostPermit(t, command.runner.remoteCommands, target.addr(cfg.addressMode), NewHostLoadSignal().Key())
		defer held()
		owner := make(chan error, 1)
		go func() { _, err := runSignalInSlot(t.Context(), pool, command, syntheticSettings(nil)); owner <- err }()
		<-cleanup
		sample := make(chan error, 1)
		go func() { _, err := runSignalInSlot(t.Context(), pool, pg, syntheticSettings(nil)); sample <- err }()
		synctest.Wait()
		if *calls != 0 {
			t.Fatal("sampler entered the source before the holder released shared admission")
		}
		close(finish)
		if err := <-owner; err != nil {
			t.Fatal("completed synthetic command lost its original result")
		}
		if err := <-sample; err != nil || *calls != 1 {
			t.Fatal("sampler did not complete its one admitted source call")
		}
		_, raw := pgTransportReadControlReceipt(t, pg.env)
		wire := readPgHostPermitWire(t, raw)
		if wire == nil || wire.Attribution != "unique_release" || len(wire.Releases) != 1 || wire.Releases[0].Key != command.Key() {
			t.Fatal("normal scheduled transport did not retain its exact shared-cleanup release")
		}
	})
}

// Size pressure must preserve the complete baseline body and every retained
// SQL representative. An absent optional diagnostic conveys no holder evidence.
func TestPgHostPermitHoldersAtReceiptCapPreserveOriginalEvidence(t *testing.T) {
	for _, private := range []bool{false, true} {
		receipt := pgQuerySampleReceipt{Schema: 1, Reason: "f", TransportObservation: &pgSampleTransportReceipt{}}
		if private {
			load := pgSampleLoad{Query: "q1", Family: "other"}
			receipt.Load = []pgSampleLoad{load}
			receipt.PrivateSql = &pgSampleSqlCoverage{Schema: 1}
			receipt.privateSqlRows = []pgSampleSqlRow{{pgSampleSqlGroup: pgSampleSqlGroupForLoad(load), Samples: []int{1}, Prefix: []byte("synthetic")}}
		}
		public, companion, err := pgSampleEncodeReceipts(&receipt)
		if err != nil {
			t.Fatal("synthetic source failed to fit before the size boundary")
		}
		receipt.Reason += strings.Repeat("x", pgSampleReceiptBytes-len(public)-len(companion))
		base, privateBase, err := pgSampleEncodeReceipts(&receipt)
		if err != nil || len(base)+len(privateBase) != pgSampleReceiptBytes || (private && receipt.PrivateSql.RetainedStatements != 1) {
			t.Fatal("control did not exactly fill the combined limit with its source evidence")
		}
		receipt.TransportObservation.HostPermitHolders = &pgSampleHostPermitHolders{Schema: 1, Attribution: "no_release_required"}
		observed, privateObserved, err := pgSampleEncodeReceipts(&receipt)
		if err != nil || !bytes.Equal(base, observed) || !bytes.Equal(privateBase, privateObserved) || readPgHostPermitWire(t, observed) != nil {
			t.Fatal("optional host permit data displaced the original bounded receipt or private representative")
		}
	}
}

// A nonstandard higher capacity continues to admit its last command. The
// production two-holder bound cannot present that address's census as complete.
func TestPgHostPermitHoldersUnsupportedCapacityPreservesGrant(t *testing.T) {
	limiter := newHostCommandLimiter(3)
	address := "203.0.113.39"
	for range 2 {
		release := holdPgHostPermit(t, limiter, address, NewHostLoadSignal().Key())
		defer release()
	}
	ctx, observation := observePgSampleTransport(t.Context())
	release, err := limiter.acquire(ctx, address)
	if err != nil || len(limiter.hostSlots(address)) != 3 {
		t.Fatal("diagnostic bound prevented the third permitted grant")
	}
	release()
	detail := observation.receipt.HostPermitHolders
	if detail == nil || detail.Attribution != "unavailable" || detail.AtRequest.Active != 2 || detail.AtReturn.Active != 2 || detail.AtRequest.Holders != nil || detail.AtReturn.Holders != nil || detail.ReleaseCount != nil || detail.Releases != nil {
		t.Fatal("unsupported capacity invented either a complete census or zero releases")
	}
}

// A returned command inside an initially open window is an exact release but
// cannot establish that the sampler needed to wait for it.
func TestPgHostPermitHoldersInitiallyAvailableReleaseIsUnattributed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newHostCommandLimiter(2)
		address := "198.51.100.61"
		held := holdPgHostPermit(t, limiter, address, NewHostLoadSignal().Key())
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
		held()
		close(barrier.resume)
		if err := <-done; err != nil {
			t.Fatal("available-slot control lost its grant")
		}
		detail := observation.receipt.HostPermitHolders
		if detail == nil || detail.Attribution != "not_full_at_request" || detail.ReleaseCount == nil || *detail.ReleaseCount != 1 || len(detail.Releases) != 1 || len(detail.AtRequest.Holders) != 1 || detail.Releases[0] != detail.AtRequest.Holders[0] {
			t.Fatal("a release observed from an open address acquired causal authority")
		}
	})
}

// Exhaustion must withhold the release measure, even if the current held set
// is complete. A legacy receipt has no authority for either measurement.
func TestPgHostPermitHoldersExhaustionAndLegacyKeepMissingEvidence(t *testing.T) {
	limiter := newHostCommandLimiter(2)
	address := "192.0.2.53"
	state := limiter.forHost(address)
	state.released = ^uint64(0)
	ctx, observation := observePgSampleTransport(t.Context())
	release, err := limiter.acquire(ctx, address)
	if err != nil {
		t.Fatal("exhausted observation counter affected admission")
	}
	release()
	detail := observation.receipt.HostPermitHolders
	if detail == nil || detail.Attribution != "unavailable" || detail.ReleaseCount != nil || detail.Releases != nil {
		t.Fatal("exhausted release sequence wrapped into a current observation")
	}
	var legacy pgSampleTransportReceipt
	if json.Unmarshal([]byte(`{"phase":"host_admission","spawn":"not_attempted"}`), &legacy) != nil || legacy.HostPermitHolders != nil {
		t.Fatal("historical receipt acquired a host permit holder")
	}
}
