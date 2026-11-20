package evshost

// The versionless purge as the block runs it (EVSB-M10-01; purge_wiring.go), end to end on T2's fakes: an evs world
// whose one instance, evs1, is this package's block (fault.SetEvsTier naming NewHost), so the world's device pushes
// through NewHost's engine and each commit the engine finds certain reaches the host's Purger. The Net's clock moves
// only when a test moves it, so the readers' 60 s allowance is the test's to pass, and the purge's DELETEs go unpaced.
// A test waits for the purge through Host.Drain, the block's own drain, which has the purge delete what has fallen
// due and the usage producer send what it freed, never on the purge's timer.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// The series the purge exports, as evsservice names them.
const (
	purgeWiringPurgedMetricName   = "urnetwork_evs_versionless_purged_bytes_total"
	purgeWiringQueueMetricName    = "urnetwork_evs_versionless_purge_queue"
	purgeWiringObservedMetricName = "urnetwork_evs_versionless_observed_total"
)

// The block's purge settings in these worlds: §11.13's, with the DELETEs unpaced, since a pace would wait on a clock
// that moves only when a test moves it.
func purgeWiringPurgerSettings() *evsservice.PurgerSettings {
	settings := evsservice.DefaultPurgerSettings()
	settings.DeleteRate = 2 * int(time.Second)
	return settings
}

// A world on a T2 Net whose evs instance, evs1, is this package's block, with one device, admin, writing format 2,
// and §19.11's format2 gate admitting the world's network with the purge in the given mode.
type purgeWiringWorld struct {
	t     testing.TB
	net   *sidecartest.Net
	world *fault.EvsWorld
}

func newPurgeWiringWorld(t testing.TB, purgeMode string) *purgeWiringWorld {
	t.Helper()
	net := sidecartest.NewNet(t.Context(), nil)
	t.Cleanup(net.Close)
	fault.SetEvsTier(net, &fault.EvsTier{
		NewHost: func(ctx context.Context, host *sidecar.ServiceHost, evsSettings *evsservice.EvsSettings, deps *fault.EvsTierDeps) (fault.EvsTierHost, error) {
			// the world's format reaches the block through its config alone, as server's T3 tier hands it on
			blockSettings := *evsSettings
			blockSettings.InstanceFormat = 0
			hostSettings := DefaultHostSettings()
			hostSettings.Evs = &blockSettings
			hostSettings.Hdd = deps.Hdd
			hostSettings.Purger = purgeWiringPurgerSettings()
			var evsHost *Host
			var err error
			Testing_WithBlockConfig(fmt.Sprintf("instance_format: %d\n", evsSettings.InstanceFormat), func() {
				evsHost, err = NewHost(ctx, host, hostSettings, &HostDeps{
					AuthState:   deps.AuthState,
					GateViews:   deps.GateViews,
					ReceiptKeys: deps.ReceiptKeys,
				})
			})
			if err != nil {
				return nil, err
			}
			return evsHost, nil
		},
	})
	settings := fault.DefaultEvsWorldSettings()
	settings.Instances = []string{"evs1"}
	settings.Devices = []*fault.EvsWorldDeviceSettings{
		{Name: "admin", Instances: []string{"evs1"}, Writer: true, Admin: true, Format2: true},
	}
	settings.Format2 = &fault.EvsWorldFormat2Settings{Purge: purgeMode, Drop: "observe"}
	settings.InstanceFormats = map[string]uint32{"evs1": sidecar.EvsRepoFormat2}
	return &purgeWiringWorld{t: t, net: net, world: fault.NewEvsWorld(net, settings)}
}

// Runs the world's Setup and then steps, failing the test with the run's trace when one fails.
func (self *purgeWiringWorld) run(id string, steps ...*fault.Step) {
	self.t.Helper()
	schedule := &fault.Schedule{
		Id:    id,
		Steps: append([]*fault.Step{{Actor: "admin", Action: self.world.Setup}}, steps...),
		Tiers: []fault.Tier{fault.TierT2},
	}
	if result := fault.Run(self.t.Context(), schedule, self.net); result.Err != nil {
		self.t.Fatalf("%s", result)
	}
}

// A step: admin writes the ordinal-th content of p.bin, 20 KiB, so a versionless version's is a kind-5 segment
// uploaded to seg/. versioning is §18.5's setting, "" for the path's.
func (self *purgeWiringWorld) write(ordinal int, versioning string) *fault.Step {
	content := fault.EvsWorldContent(fmt.Sprintf("purge wiring p.bin %d", ordinal), 20*1024)
	return &fault.Step{Actor: "admin", Action: self.world.WriteVersioning("admin", "p.bin", content, versioning)}
}

// A step: check, as the check actor, once the steps before it have settled.
func (self *purgeWiringWorld) check(check func(ctx context.Context) error) *fault.Step {
	return &fault.Step{Actor: "check", Action: func(ctx context.Context, net *sidecartest.Net) error {
		return check(ctx)
	}}
}

// The block evs1 runs, and its purge.
func (self *purgeWiringWorld) host() (*Host, *evsservice.Purger, error) {
	evsHost, ok := self.net.Process("evs1").(*Host)
	if !ok {
		return nil, nil, errors.New("evs1 does not run this package's block")
	}
	if evsHost.Purger() == nil {
		return nil, nil, fmt.Errorf("the block built no purge: missing %v", evsHost.Missing())
	}
	return evsHost, evsHost.Purger(), nil
}

// What the segment cluster holds of p.bin's ordinal-th committed version, from 1: how many segments the version
// names, and how many of them are stored with their stored bytes.
type purgeWiringVersion struct {
	idCount         int
	storedCount     int
	storedByteCount int64
}

// p.bin's ordinal-th committed version, from 1, as the segment cluster holds it now.
func (self *purgeWiringWorld) version(ctx context.Context, ordinal int) (*purgeWiringVersion, error) {
	entries, err := self.world.CommittedEntries("p.bin")
	if err != nil {
		return nil, err
	}
	if len(entries) < ordinal {
		return nil, fmt.Errorf("p.bin has %d committed versions, not %d", len(entries), ordinal)
	}
	ids := entries[ordinal-1].SegmentIds()
	if len(ids) == 0 {
		return nil, fmt.Errorf("p.bin's version %d names no segment", ordinal)
	}
	cluster, err := self.net.Host().Blobs.SegmentCluster()
	if err != nil {
		return nil, err
	}
	version := &purgeWiringVersion{idCount: len(ids)}
	for _, id := range ids {
		info, err := cluster.Head(sidecartest.InspectContext(ctx), evsservice.SegmentKey(self.world.RepoId, [32]byte(id)))
		switch {
		case errors.Is(err, sidecar.ErrBlobNotFound):
		case err != nil:
			return nil, err
		default:
			version.storedCount += 1
			version.storedByteCount += info.ByteCount
		}
	}
	return version, nil
}

// The stored-byte deltas the world's network was reported, once the block's usage producer has sent its open
// window: everything counted so far.
func (self *purgeWiringWorld) storedDelta(ctx context.Context, evsHost *Host) (int64, error) {
	if err := evsHost.usage.Drain(ctx); err != nil {
		return 0, err
	}
	return self.net.Fakes().Quota.StoredDelta(connect.Id(self.world.NetworkId)), nil
}

// The host's engine hands the Purger each commit it finds certain: a first versionless version replaces nothing and
// queues nothing, and its replacement queues the replaced version's segments, deleting nothing at the certainty.
func TestPurgeWiringHandsACertainCommitToThePurger(t *testing.T) {
	world := newPurgeWiringWorld(t, "on")
	world.run("purge wiring: a certain commit's candidates",
		world.write(1, sidecar.EvsVersioningOff),
		world.check(func(ctx context.Context) error {
			_, purger, err := world.host()
			if err != nil {
				return err
			}
			if queued := purger.QueuedIdCount(); queued != 0 {
				return fmt.Errorf("a first version queued %d ids", queued)
			}
			return nil
		}),
		world.write(2, ""),
		world.check(func(ctx context.Context) error {
			_, purger, err := world.host()
			if err != nil {
				return err
			}
			replaced, err := world.version(ctx, 1)
			if err != nil {
				return err
			}
			stats := purger.Stats()
			if queued := purger.QueuedIdCount(); queued != replaced.idCount || stats.QueuedIdCount != replaced.idCount {
				return fmt.Errorf("the replacement queued %d ids (%d in all), not the replaced version's %d", queued, stats.QueuedIdCount, replaced.idCount)
			}
			if gauge := world.net.Fakes().Metrics.Value(purgeWiringQueueMetricName); gauge != float64(replaced.idCount) {
				return fmt.Errorf("the queue gauge reads %v", gauge)
			}
			if replaced.storedCount != replaced.idCount || stats.DeletedCount != 0 {
				return fmt.Errorf("the replaced version lost segments at the certainty: %+v, %+v", replaced, stats)
			}
			return nil
		}),
	)
}

// With format2.purge on, the replaced version's kind-5 segment stays through the readers' allowance and is deleted
// once it has passed on the test clock; the newest version's stays, the purge counts the freed bytes as the commit's,
// and the usage producer the engine counts with takes them as a negative stored-byte delta.
func TestPurgeWiringDeletesTheReplacedContentAfterTheAllowance(t *testing.T) {
	world := newPurgeWiringWorld(t, "on")
	var replacedByteCount int64
	var storedBefore int64
	world.run("purge wiring: on",
		world.write(1, sidecar.EvsVersioningOff),
		world.write(2, ""),
		world.check(func(ctx context.Context) error {
			evsHost, _, err := world.host()
			if err != nil {
				return err
			}
			replaced, err := world.version(ctx, 1)
			if err != nil {
				return err
			}
			replacedByteCount = replaced.storedByteCount
			storedBefore, err = world.storedDelta(ctx, evsHost)
			return err
		}),
		&fault.Step{Advance: evsservice.DefaultPurgerSettings().Allowance - time.Second},
		world.check(func(ctx context.Context) error {
			_, purger, err := world.host()
			if err != nil {
				return err
			}
			replaced, err := world.version(ctx, 1)
			if err != nil {
				return err
			}
			if replaced.storedCount != replaced.idCount || purger.Stats().DeletedCount != 0 {
				return fmt.Errorf("deleted inside the allowance: %+v, %+v", replaced, purger.Stats())
			}
			return nil
		}),
		&fault.Step{Advance: time.Second},
		world.check(func(ctx context.Context) error {
			evsHost, purger, err := world.host()
			if err != nil {
				return err
			}
			evsHost.Drain()
			replaced, err := world.version(ctx, 1)
			if err != nil {
				return err
			}
			newest, err := world.version(ctx, 2)
			if err != nil {
				return err
			}
			stats := purger.Stats()
			switch {
			case replaced.storedCount != 0:
				return fmt.Errorf("the replaced version keeps %d of its %d segments: %+v", replaced.storedCount, replaced.idCount, stats)
			case newest.storedCount != newest.idCount:
				return fmt.Errorf("the newest version lost segments: %+v", newest)
			case stats.DeletedCount != replaced.idCount || stats.FreedByteCount != replacedByteCount || stats.SweepCount != 0:
				return fmt.Errorf("the purge's counts: %+v, the replaced version's %d segments of %d bytes", stats, replaced.idCount, replacedByteCount)
			}
			if purged := world.net.Fakes().Metrics.Value(purgeWiringPurgedMetricName, "commit"); purged != float64(replacedByteCount) {
				return fmt.Errorf("purged_bytes_total{by=commit} reads %v, not %d", purged, replacedByteCount)
			}
			// Drain sent the producer's open window; nothing but the purge moved stored bytes since storedBefore
			storedAfter := world.net.Fakes().Quota.StoredDelta(connect.Id(world.world.NetworkId))
			if storedAfter-storedBefore != -replacedByteCount {
				return fmt.Errorf("the network's stored bytes moved %d across the purge, not %d", storedAfter-storedBefore, -replacedByteCount)
			}
			return nil
		}),
	)
}

// With format2.purge at observe, the purge deletes nothing once the allowance has passed: it counts the candidate's
// ids in urnetwork_evs_versionless_observed_total and reports nothing to usage.
func TestPurgeWiringObserveDeletesNothing(t *testing.T) {
	world := newPurgeWiringWorld(t, "observe")
	var storedBefore int64
	world.run("purge wiring: observe",
		world.write(1, sidecar.EvsVersioningOff),
		world.write(2, ""),
		world.check(func(ctx context.Context) error {
			evsHost, _, err := world.host()
			if err != nil {
				return err
			}
			storedBefore, err = world.storedDelta(ctx, evsHost)
			return err
		}),
		&fault.Step{Advance: evsservice.DefaultPurgerSettings().Allowance},
		world.check(func(ctx context.Context) error {
			evsHost, purger, err := world.host()
			if err != nil {
				return err
			}
			evsHost.Drain()
			replaced, err := world.version(ctx, 1)
			if err != nil {
				return err
			}
			stats := purger.Stats()
			switch {
			case replaced.storedCount != replaced.idCount:
				return fmt.Errorf("observe deleted %d of the replaced version's %d segments", replaced.idCount-replaced.storedCount, replaced.idCount)
			case stats.DeletedCount != 0 || stats.FreedByteCount != 0 || stats.ObservedCount != replaced.idCount:
				return fmt.Errorf("the purge's counts under observe: %+v, the replaced version's %d segments", stats, replaced.idCount)
			}
			if observed := world.net.Fakes().Metrics.Value(purgeWiringObservedMetricName); observed != float64(replaced.idCount) {
				return fmt.Errorf("observed_total reads %v, not %d", observed, replaced.idCount)
			}
			if purged := world.net.Fakes().Metrics.Value(purgeWiringPurgedMetricName, "commit"); purged != 0 {
				return fmt.Errorf("purged_bytes_total{by=commit} reads %v under observe", purged)
			}
			if storedAfter := world.net.Fakes().Quota.StoredDelta(connect.Id(world.world.NetworkId)); storedAfter != storedBefore {
				return fmt.Errorf("observe moved the network's stored bytes by %d", storedAfter-storedBefore)
			}
			return nil
		}),
	)
}

// A hold covering the repo, placed after the replaced version's segment was uploaded, keeps it past the allowance
// with format2.purge on: nothing is deleted, and since only the hold keeps it, it leaves the network's stored count.
func TestPurgeWiringHoldKeepsTheReplacedContent(t *testing.T) {
	world := newPurgeWiringWorld(t, "on")
	var replacedByteCount int64
	var storedBefore int64
	world.run("purge wiring: a hold",
		world.write(1, sidecar.EvsVersioningOff),
		&fault.Step{Advance: time.Second},
		&fault.Step{Actor: "abuse", Action: func(ctx context.Context, net *sidecartest.Net) error {
			placeTime := net.Clock().Now()
			return sidecartest.PlaceHold(sidecartest.InspectContext(ctx), net.Host().Blobs, &sidecar.HoldRecord{
				NetworkId:  world.world.NetworkId,
				CaseId:     sidecar.NewId(),
				Kind:       sidecar.HoldKindPreserve2703f,
				Scope:      sidecar.HoldScopeRepos(world.world.RepoId),
				PlaceTime:  placeTime,
				ExpireTime: placeTime.Add(90 * 24 * time.Hour),
			})
		}},
		world.write(2, ""),
		world.check(func(ctx context.Context) error {
			evsHost, _, err := world.host()
			if err != nil {
				return err
			}
			replaced, err := world.version(ctx, 1)
			if err != nil {
				return err
			}
			replacedByteCount = replaced.storedByteCount
			storedBefore, err = world.storedDelta(ctx, evsHost)
			return err
		}),
		&fault.Step{Advance: evsservice.DefaultPurgerSettings().Allowance},
		world.check(func(ctx context.Context) error {
			evsHost, purger, err := world.host()
			if err != nil {
				return err
			}
			evsHost.Drain()
			replaced, err := world.version(ctx, 1)
			if err != nil {
				return err
			}
			stats := purger.Stats()
			switch {
			case replaced.storedCount != replaced.idCount:
				return fmt.Errorf("the hold kept %d of the replaced version's %d segments", replaced.storedCount, replaced.idCount)
			case stats.DeletedCount != 0 || stats.HeldCount != replaced.idCount:
				return fmt.Errorf("the purge's counts under the hold: %+v, the replaced version's %d segments", stats, replaced.idCount)
			}
			if purged := world.net.Fakes().Metrics.Value(purgeWiringPurgedMetricName, "commit"); purged != 0 {
				return fmt.Errorf("purged_bytes_total{by=commit} reads %v for content the hold kept", purged)
			}
			storedAfter := world.net.Fakes().Quota.StoredDelta(connect.Id(world.world.NetworkId))
			if storedAfter-storedBefore != -replacedByteCount {
				return fmt.Errorf("the network's stored bytes moved %d for what only the hold keeps, not %d", storedAfter-storedBefore, -replacedByteCount)
			}
			return nil
		}),
	)
}

// One certain commit's list naming a single segment, as step 7 hands it.
func purgeWiringTestCommit() *evsservice.PurgeCommit {
	return &evsservice.PurgeCommit{
		ClusterId: "meta-1",
		RepoId:    sidecar.NewId(),
		RefId:     sidecar.NewId(),
		NetworkId: sidecar.NewId(),
		PushId:    sidecar.NewId(),
		Version:   2,
		PathEpoch: 1,
		List: &protocol.EvsPurgeList{Candidates: []*protocol.EvsPurgeCandidate{
			{ReplacedVersion: 1, SegmentIds: [][]byte{bytes.Repeat([]byte{0x5a}, 32)}},
		}},
	}
}

// The host runs its purge with its own lifecycle: built with every seam, it takes commits while the block runs;
// the host's Drain ends it, so a commit certain after the drain is the sweep's; and so does Close.
func TestHostRunsItsPurgeWithItsLifecycle(t *testing.T) {
	drained := newHostTestHost(t, nil)
	purger := drained.Purger()
	if purger == nil {
		t.Fatalf("no purge with every seam given: missing %v", drained.Missing())
	}
	purger.Certain(purgeWiringTestCommit())
	if purger.QueuedIdCount() != 1 {
		t.Fatalf("a running block's purge queued %d ids", purger.QueuedIdCount())
	}
	// the commit is not due until the allowance has passed, so the drain waits on it: pass it
	drained.clock.(*sidecartest.FakeClock).Advance(evsservice.DefaultPurgerSettings().Allowance)
	drained.Drain()
	// the host gives no gates, so the purge reads observe: the drain took the due commit through it, deleting nothing
	if stats := purger.Stats(); purger.QueuedIdCount() != 0 || stats.ObservedCount != 1 || stats.DeletedCount != 0 {
		t.Fatalf("the drain left %d ids queued: %+v", purger.QueuedIdCount(), stats)
	}
	purger.Certain(purgeWiringTestCommit())
	if purger.QueuedIdCount() != 0 || purger.Stats().SweepCount == 0 {
		t.Fatalf("a drained block's purge took a commit: %+v", purger.Stats())
	}

	closed := newHostTestHost(t, nil)
	closed.Close()
	closed.Purger().Certain(purgeWiringTestCommit())
	if stats := closed.Purger().Stats(); closed.Purger().QueuedIdCount() != 0 || stats.SweepCount != 1 {
		t.Fatalf("a closed block's purge took a commit: %+v", stats)
	}
}

// A host with no Quota has no usage producer for the purge to report to, so it builds none, and the block stays not
// ready, saying so, rather than serving with every purge left to the sweep.
func TestHostWithoutAUsageProducerBuildsNoPurge(t *testing.T) {
	evsHost := newHostTestHost(t, func(host *sidecar.ServiceHost, deps *HostDeps) { host.Quota = nil })
	if evsHost.Purger() != nil {
		t.Fatal("a purge was built with no usage producer")
	}
	if evsHost.Ready() {
		t.Fatal("ready with no purge")
	}
}

// Purge settings past §11.13's floors are an error, as the engine's own are: the block does not start.
func TestHostRefusesPurgeSettingsPastTheFloors(t *testing.T) {
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	host := sidecartest.NewFakeHost(t.Context(), clock, nil, nil, nil).ServiceHost()
	settings := DefaultHostSettings()
	settings.Purger.Allowance = settings.Purger.Allowance - time.Second
	evsHost, err := NewHost(t.Context(), host, settings, &HostDeps{
		AuthState:   hostTestAuthState{},
		GateViews:   hostTestGateViews{},
		ReceiptKeys: hostTestReceiptKeys(t, clock.Now()),
	})
	if err == nil {
		evsHost.Close()
		t.Fatal("a block started with an allowance under §11.13's 60 s")
	}
	if !errors.Is(err, evsservice.ErrPurgerSettings) {
		t.Fatalf("refused with %v", err)
	}
}
