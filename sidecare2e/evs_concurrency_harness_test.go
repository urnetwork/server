//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The evs concurrency case's server-side parts (evs_concurrency_test.go), gathered in one file as the e2e conventions
// ask, since QD-1028's layout move will move what they reach:
//
//   - taskworker's part: the world runs no taskworker, so what evs's jobs do for the case runs here, on a host of its
//     own over the world's clusters and Redis, composed as taskworker's evs jobs compose theirs
//     (taskworker/evs_jobs.go): server's gates from sidecar.yml, a write budget of its own that reports beside every
//     block's, no task queue, and a clock check that answers sure, as the world's evs block's does. It runs a drop's
//     dry run, which taskworker's evs.drop takes at the {evsd_} hint (evsservice.DropPreviews), and the stored count,
//     which evs.nightly lists (evsservice.NightlyPass); in a world minutes old the pass deletes nothing, since every
//     orphan, container and pack it judges must be a day old first. Its jobs spend through a count of what each issue
//     and grant took, beside the budget that spends it, so the report its process writes as its budget closes is held
//     to that count to the byte: the one process of the case whose every write the harness sees
//   - a hold: api mirrors each hold as hold/<network>/<case>/_ on the first metadata cluster, which evs's purge and
//     drop job read (§21.9); the world runs no abuse staff route, and abuse.yml's holds_enabled stays off until counsel
//     confirms holds (M5), so the hold is placed as api writes its object, through model's AbuseHoldStore on api's own
//     write budget, read back to price it as it stood, and released as AbuseHoldPass ends one, its object deleted
//   - api's write budget against what api wrote, exactly: every P0 byte api's process reported against the first
//     metadata cluster is an object it holds under §7.9's and §7.10's prefixes priced as created, a rev/ raise priced
//     as its object, or the hold's object priced as it stood, as controlPlaneCheckApiBudget counts api's objects (its
//     helpers, control_plane_concurrency_harness_test.go), read where the objects stand still across the report
//   - the write budget's reports: every process reports into {nvb_<set>} under a 15 s ttl, so a sampler reads every
//     metadata cluster's reports for the case's life, keeping each process's newest, and a run chaos stopped stays in
//     the ledger after its report lapses; the report a process writes as its budget closes is its newest
//   - the chaos at evs's origin: a hook in front of the world's lb for the evs block (worldLb) that, once armed, takes
//     the first push it matches, lets the block answer it in full, restarts the block on its stores, and answers the
//     pusher 502 with no evs body, as an lb whose upstream went away before the answer reached it: the pusher sends the
//     same push id and bytes again (QD-758), and the restarted block must answer the push as committed (§11.8's 5a),
//     so a restart lands exactly between a commit and its answer
//   - a recorder at gkm's origin, in front of the world's lb for the gkm block: every 401 or 403 gkm answers, with the
//     bearer's claims read unverified (its log position, scopes and sidecar, never the token) and what the case says of
//     the caller's device at that moment, so a refusal reads as what it refused: since QD-1179 a refusal of a batch
//     whose role on the call's repo is stronger than gkm's state gives is the client's to try again under the call's
//     own scope, and one the retry does not cure fails its write
//
// Safe for concurrent use.

package sidecare2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The stand-in for taskworker's evs jobs (the file's header).
type evsConcurrencyWorker struct {
	world  *World
	host   *sidecar.ServiceHost
	budget *evsservice.WriteBudgetClient
	// the budget the stand-in's jobs spend through, counting what each was granted
	counted *evsConcurrencyCountedBudget
	cancel  context.CancelFunc
}

// The stand-in's host and budget, until close.
func newEvsConcurrencyWorker(world *World) (*evsConcurrencyWorker, error) {
	ctx, cancel := context.WithCancel(context.Background())
	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{
		Blobs:         world.T3().Host().Blobs,
		ClockCheck:    sidecartest.NewFakeClockCheck(nil),
		ServiceTokens: world.T3().ServiceTokens(),
	})
	budget, err := evsservice.NewWriteBudget(ctx, host, nil, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("the stand-in's write budget: %w", err)
	}
	counted := newEvsConcurrencyCountedBudget(budget)
	host.Budget = counted
	return &evsConcurrencyWorker{world: world, host: host, budget: budget, counted: counted, cancel: cancel}, nil
}

// The process id the stand-in's budget reports under.
func (self *evsConcurrencyWorker) processId() string {
	return self.budget.ProcessId()
}

// Ends the stand-in's budget, which writes its last report as it closes, and its gates. More than once is safe.
func (self *evsConcurrencyWorker) close() {
	self.budget.Close()
	self.cancel()
}

// A write budget that counts what every grant it passes on spent, beside the budget that spends it: the stand-in's
// own count of its jobs' writes, which its process's report must hold to the byte (the file's header). Safe for
// concurrent use.
type evsConcurrencyCountedBudget struct {
	sidecar.WriteBudget

	stateLock sync.Mutex
	// P0 by kind, P1's immediate and deferred parts, and P2, as each issue or grant took them
	p0ByteCounts    map[sidecar.WriteP0Kind]int64
	p1ByteCount     int64
	creditByteCount int64
	p2ByteCount     int64
}

func newEvsConcurrencyCountedBudget(budget sidecar.WriteBudget) *evsConcurrencyCountedBudget {
	return &evsConcurrencyCountedBudget{WriteBudget: budget, p0ByteCounts: map[sidecar.WriteP0Kind]int64{}}
}

func (self *evsConcurrencyCountedBudget) IssueP0(ctx context.Context, clusterId string, kind sidecar.WriteP0Kind, byteCount int64) context.Context {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.p0ByteCounts[kind] += byteCount
	}()
	return self.WriteBudget.IssueP0(ctx, clusterId, kind, byteCount)
}

func (self *evsConcurrencyCountedBudget) AdmitP1(ctx context.Context, clusterId string, networkId connect.Id, cost sidecar.WriteCost) (sidecar.WriteGrant, error) {
	grant, err := self.WriteBudget.AdmitP1(ctx, clusterId, networkId, cost)
	if err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.p1ByteCount += cost.ImmediateByteCount
		self.creditByteCount += cost.DeferredByteCount
	}
	return grant, err
}

func (self *evsConcurrencyCountedBudget) AcquireP2(ctx context.Context, clusterId string, work *sidecar.WriteP2Work) (sidecar.WriteGrant, error) {
	grant, err := self.WriteBudget.AcquireP2(ctx, clusterId, work)
	if err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.p2ByteCount += work.ByteCount
	}
	return grant, err
}

func (self *evsConcurrencyCountedBudget) TryAcquireP2(ctx context.Context, clusterId string, work *sidecar.WriteP2Work) (sidecar.WriteGrant, bool, error) {
	grant, ok, err := self.WriteBudget.TryAcquireP2(ctx, clusterId, work)
	if ok && err == nil {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.p2ByteCount += work.ByteCount
	}
	return grant, ok, err
}

// What the grants spent so far, as a report would carry it, P2 funded and unfunded together under UnfundedByteCount.
func (self *evsConcurrencyCountedBudget) counts() *evsservice.WriteBudgetReport {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	p0ByteCounts := map[sidecar.WriteP0Kind]int64{}
	for kind, byteCount := range self.p0ByteCounts {
		if byteCount != 0 {
			p0ByteCounts[kind] = byteCount
		}
	}
	return &evsservice.WriteBudgetReport{
		P0ByteCounts:      p0ByteCounts,
		P1ByteCount:       self.p1ByteCount,
		CreditByteCount:   self.creditByteCount,
		UnfundedByteCount: self.p2ByteCount,
	}
}

// The metadata cluster repoId is placed on, as place/<repo_id> on the first cluster names it.
func (self *evsConcurrencyWorker) repoClusterId(ctx context.Context, repoId sidecar.Id) (string, error) {
	first, err := self.host.Blobs.FirstCluster()
	if err != nil {
		return "", err
	}
	place, err := sidecar.ReadEvsRepoPlace(ctx, first, repoId)
	if err != nil {
		return "", fmt.Errorf("the repo's placement: %w", err)
	}
	return place.ClusterId, nil
}

// Runs repoId's queued dry runs as taskworker's evs.drop does at the {evsd_} hint, every interval until done closes or
// ctx ends: each run takes the oldest queued dry run, if any, resolves and marks it at its snapshot and stores its
// results. Answers how many dry runs it finished and the first error a run gave.
func (self *evsConcurrencyWorker) runDryRuns(ctx context.Context, repoId sidecar.Id, interval time.Duration, done <-chan struct{}) (int, error) {
	clusterId, err := self.repoClusterId(ctx, repoId)
	if err != nil {
		return 0, err
	}
	evsSettings := evsservice.DefaultEvsSettings()
	resolver, err := evsservice.NewDropResolve(self.host, &evsservice.DropResolveDeps{EvsSettings: evsSettings})
	if err != nil {
		return 0, err
	}
	marker, err := evsservice.NewDropMark(self.host, &evsservice.DropMarkDeps{EvsSettings: evsSettings})
	if err != nil {
		return 0, err
	}
	previews, err := evsservice.NewDropPreviews(self.host, &evsservice.DropPreviewDeps{Resolver: resolver, Marker: marker, EvsSettings: evsSettings})
	if err != nil {
		return 0, err
	}
	finished := 0
	for {
		result, err := previews.Run(ctx, clusterId, repoId)
		if err != nil {
			return finished, fmt.Errorf("a dry run of the repo: %w", err)
		}
		switch result.State {
		case protocol.EvsDropPreviewState_EVS_DROP_PREVIEW_DONE:
			finished += 1
		case protocol.EvsDropPreviewState_EVS_DROP_PREVIEW_FAILED:
			return finished, fmt.Errorf("the dry run %s ended failed", sidecar.EvsKeyId(result.PreviewId))
		}
		select {
		case <-done:
			return finished, nil
		case <-ctx.Done():
			return finished, ctx.Err()
		case <-time.After(interval):
		}
	}
}

// Each network's stored count as evs.nightly lists it (§11.13's bytes row): its repos' seg/, pack/ and hp/ objects'
// sizes, plus the record lengths its head windows and log pages give for versions still in batches, over every
// metadata cluster. An error for a cluster the pass did not count whole.
func (self *evsConcurrencyWorker) storedByteCounts(ctx context.Context) (map[sidecar.Id]int64, error) {
	pass, err := evsservice.NewNightlyPass(self.host, &evsservice.NightlyPassDeps{})
	if err != nil {
		return nil, err
	}
	counts := map[sidecar.Id]int64{}
	for _, clusterId := range self.host.Blobs.MetadataClusterIds() {
		for _, shard := range pass.Settings().Shards() {
			result, err := pass.Run(ctx, clusterId, shard, "")
			if err != nil {
				return nil, fmt.Errorf("the nightly pass of %s: %w", clusterId, err)
			}
			if !result.Complete || result.More {
				return nil, fmt.Errorf("the nightly pass of %s did not count it whole: complete %t, more %t", clusterId, result.Complete, result.More)
			}
			for networkId, bytes := range result.NetworkBytes {
				counts[networkId] += bytes.StoredByteCount()
			}
		}
	}
	return counts, nil
}

// One hold the case placed, as api mirrors it.
type evsConcurrencyHold struct {
	store  *model.AbuseHoldStore
	record *sidecar.HoldRecord
	// what its object, read back as it stood, prices at as created: the P0 of kind lifecycle api's budget spent on it,
	// 0 where api runs no budget
	pricedByteCount int64
}

// A preserve_2703f hold on networkId's repoIds, placed at now as api writes hold/<network>/<case>/_ on the first
// metadata cluster, its write P0 of kind lifecycle on api's own budget, priced as the evs block prices an object's
// create. It binds evs's purge and drop job once their hold caches reread hold/, within 5 s (§21.9).
func placeEvsConcurrencyHold(ctx context.Context, world *World, networkId server.Id, repoIds []sidecar.Id, life time.Duration) (*evsConcurrencyHold, error) {
	first, err := world.T3().Host().Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	store := model.NewAbuseHoldStore(first)
	priceTable, err := evsservice.DefaultPriceTable(evsservice.DefaultEvsSettings().PriceBlockByteCount)
	if err != nil {
		return nil, err
	}
	createPrice := func(objectByteCount int64) (int64, error) {
		return priceTable.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: objectByteCount, Count: 1})
	}
	budget := world.T3().ApiWriteBudget()
	if budget != nil {
		store.SetWriteBudget(budget, func(objectByteCount int64) int64 {
			priced, err := createPrice(objectByteCount)
			if err != nil {
				return objectByteCount
			}
			return priced
		})
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	record := &sidecar.HoldRecord{
		NetworkId:  sidecar.Id(networkId),
		CaseId:     sidecar.NewId(),
		Kind:       sidecar.HoldKindPreserve2703f,
		Scope:      sidecar.HoldScopeRepos(repoIds...),
		PlaceTime:  now,
		ExpireTime: now.Add(life),
	}
	stored, err := store.Create(ctx, record)
	if err != nil {
		return nil, fmt.Errorf("the hold's object: %w", err)
	}
	if !stored.PlaceTime.Equal(record.PlaceTime) {
		return nil, fmt.Errorf("the hold's object holds another placement, at %s", stored.PlaceTime)
	}
	hold := &evsConcurrencyHold{store: store, record: record}
	if budget != nil {
		object, found, err := store.Read(ctx, networkId, server.Id(record.CaseId))
		if err != nil || !found {
			return nil, fmt.Errorf("the hold's object read back: found %t: %v", found, err)
		}
		if hold.pricedByteCount, err = createPrice(int64(len(object.Body))); err != nil {
			return nil, err
		}
	}
	return hold, nil
}

// Ends the hold as AbuseHoldPass ends one: its object deleted.
func (self *evsConcurrencyHold) release(ctx context.Context) error {
	return self.store.Delete(ctx, server.Id(self.record.NetworkId), server.Id(self.record.CaseId))
}

// Whether the hold's object stands on the first metadata cluster.
func (self *evsConcurrencyHold) stands(ctx context.Context) (bool, error) {
	_, found, err := self.store.Read(ctx, server.Id(self.record.NetworkId), server.Id(self.record.CaseId))
	return found, err
}

// Every metadata cluster's write budget reports, read every interval for the case's life, each process's newest kept
// (the file's header). Safe for concurrent use.
type evsConcurrencyBudgetSampler struct {
	world    *World
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}

	stateLock sync.Mutex
	// by cluster, then process id: its newest report, and how many reads found it
	clusterReports map[string]map[string]*evsservice.WriteBudgetReport
	sampleCount    int
	errs           []error
}

// Starts the sampler, reading at once and every interval until stop.
func startEvsConcurrencyBudgetSampler(world *World, interval time.Duration) *evsConcurrencyBudgetSampler {
	ctx, cancel := context.WithCancel(context.Background())
	self := &evsConcurrencyBudgetSampler{
		world:          world,
		interval:       interval,
		cancel:         cancel,
		done:           make(chan struct{}),
		clusterReports: map[string]map[string]*evsservice.WriteBudgetReport{},
	}
	go self.run(ctx)
	return self
}

func (self *evsConcurrencyBudgetSampler) run(ctx context.Context) {
	defer close(self.done)
	for {
		self.sample(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(self.interval):
		}
	}
}

// One read of every cluster's reports, each process's kept where it is newer than the one held.
func (self *evsConcurrencyBudgetSampler) sample(ctx context.Context) {
	host := self.world.T3().Host()
	for _, clusterId := range host.Blobs.MetadataClusterIds() {
		reports, err := evsservice.ReadWriteBudgetReports(ctx, host.Redis, clusterId)
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if err != nil {
				if ctx.Err() == nil {
					self.errs = append(self.errs, fmt.Errorf("%s's reports: %w", clusterId, err))
				}
				return
			}
			processReports := self.clusterReports[clusterId]
			if processReports == nil {
				processReports = map[string]*evsservice.WriteBudgetReport{}
				self.clusterReports[clusterId] = processReports
			}
			for _, report := range reports {
				if held := processReports[report.ProcessId]; held == nil || held.TimeMs <= report.TimeMs {
					processReports[report.ProcessId] = report
				}
			}
			self.sampleCount += 1
		}()
	}
}

// Reads once more and stops.
func (self *evsConcurrencyBudgetSampler) stop() {
	self.sample(context.Background())
	self.cancel()
	<-self.done
}

// Each cluster's newest report of each process the sampler saw, and the reads that failed.
func (self *evsConcurrencyBudgetSampler) reports() (map[string]map[string]*evsservice.WriteBudgetReport, []error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	clusterReports := map[string]map[string]*evsservice.WriteBudgetReport{}
	for clusterId, processReports := range self.clusterReports {
		clusterReports[clusterId] = maps.Clone(processReports)
	}
	return clusterReports, slices.Clone(self.errs)
}

// api's write budget against what api wrote, exactly (the file's header): the P0 bytes api's process reported by kind
// equal what the objects it holds under proj/, projrows/ and rev/ price at as created, each rev/ raise since
// revWritesAtStart at its object's price, and the hold's object at its price as it stood (holdByteCount, P0 of kind
// lifecycle; its release deleted it, which spends nothing); the rev/ writer created exactly the rev/ objects the store
// holds; and api reported nothing else. Read where api's objects and the rev/ writer's count stand still across the
// report, at most tryCount times, each LIST asking pageCount objects, as controlPlaneCheckApiBudget reads them.
func evsConcurrencyCheckApiBudget(t testing.TB, ctx context.Context, world *World, tryCount int, pageCount int, revWritesAtStart *controlPlaneRevWrites, holdByteCount int64) {
	t.Helper()
	cluster, err := world.T3().Host().Blobs.FirstCluster()
	if err != nil {
		t.Errorf("the first metadata cluster: %v", err)
		return
	}
	prices := evsservice.DefaultPriceTable4KiB()
	kinds := controlPlaneApiObjectKinds()
	disagreements := []string{}
	for range tryCount {
		writesBefore, err := controlPlaneRevWriteCounts()
		if err != nil {
			t.Errorf("the rev/ writer's writes: %v", err)
			return
		}
		before, err := controlPlaneApiObjects(ctx, world, pageCount)
		if err != nil {
			t.Errorf("api's objects: %v", err)
			return
		}
		ledgers, err := world.ReadLedgers(ctx, time.Now())
		if err != nil {
			t.Errorf("the ledgers: %v", err)
			return
		}
		after, err := controlPlaneApiObjects(ctx, world, pageCount)
		if err != nil {
			t.Errorf("api's objects: %v", err)
			return
		}
		writesAfter, err := controlPlaneRevWriteCounts()
		if err != nil {
			t.Errorf("the rev/ writer's writes: %v", err)
			return
		}
		if !maps.Equal(before, after) || *writesBefore != *writesAfter {
			disagreements = append(disagreements, fmt.Sprintf("%d objects, then %d; rev/ writes %+v, then %+v", len(before), len(after), *writesBefore, *writesAfter))
			continue
		}
		want := map[sidecar.WriteP0Kind]int64{}
		objectCounts := map[sidecar.WriteP0Kind]int{}
		revPrices := map[int64]bool{}
		for key, byteCount := range after {
			for prefix, kind := range kinds {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				price, err := prices.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: byteCount, Count: 1})
				if err != nil {
					t.Errorf("the price of %s's %d bytes: %v", key, byteCount, err)
					return
				}
				want[kind] += price
				objectCounts[kind] += 1
				if kind == sidecar.WriteP0KindRevocation {
					revPrices[price] = true
				}
			}
		}
		createCount := int64(writesAfter.createdCount - revWritesAtStart.createdCount)
		if createCount != int64(objectCounts[sidecar.WriteP0KindRevocation]) {
			t.Errorf("the rev/ writer created %d objects since the world began, and api holds %d under rev/", createCount, objectCounts[sidecar.WriteP0KindRevocation])
		}
		raiseCount := int64(writesAfter.raisedCount - revWritesAtStart.raisedCount)
		if 0 < raiseCount {
			if len(revPrices) != 1 {
				t.Errorf("api raised rev/ objects %d times, and its rev/ objects price at %v, not one price a raise takes", raiseCount, slices.Sorted(maps.Keys(revPrices)))
				return
			}
			for price := range revPrices {
				want[sidecar.WriteP0KindRevocation] += raiseCount * price
			}
		}
		if 0 < holdByteCount {
			want[sidecar.WriteP0KindLifecycle] += holdByteCount
		}
		var report *evsservice.WriteBudgetReport
		for _, candidate := range ledgers.BudgetReports[cluster.ClusterId()] {
			if candidate.ProcessId == ledgers.ApiProcessId {
				report = candidate
			}
		}
		if ledgers.ApiProcessId == "" || report == nil {
			t.Errorf("api's process %q holds no budget report of %s", ledgers.ApiProcessId, cluster.ClusterId())
			return
		}
		got := map[sidecar.WriteP0Kind]int64{}
		for kind, byteCount := range report.P0ByteCounts {
			if byteCount != 0 {
				got[kind] = byteCount
			}
		}
		if !maps.Equal(got, want) {
			t.Errorf("api's process reported P0 %v against %s, and the objects it holds there price at %v as created with %d rev/ raises and the hold's object at %d B (%v objects by kind): a write spent and never made, or made and never spent", got, cluster.ClusterId(), want, raiseCount, holdByteCount, objectCounts)
		}
		if report.P1ByteCount != 0 || report.UnfundedByteCount != 0 || report.FundedByteCount != 0 || report.CreditByteCount != 0 {
			t.Errorf("api's process reported P1 %d, P2 %d unfunded and %d funded, and credit %d, where all it writes is P0", report.P1ByteCount, report.UnfundedByteCount, report.FundedByteCount, report.CreditByteCount)
		}
		t.Logf("api's write budget: P0 %v against %s for %v objects, %d rev/ raises and the hold's object", got, cluster.ClusterId(), objectCounts, raiseCount)
		return
	}
	t.Errorf("api's objects never stood still across a budget report: %v", disagreements)
}

// The priced bytes a report says its process issued: P0 by kind, P1 and both P2s.
func evsConcurrencyReportByteCount(report *evsservice.WriteBudgetReport) int64 {
	total := report.P1ByteCount + report.UnfundedByteCount + report.FundedByteCount
	for _, byteCount := range report.P0ByteCounts {
		total += byteCount
	}
	return total
}

// Whether err is an answer that refuses nothing, a 503 or a dropped connection such as a block's restart gives, whose
// read a caller tries again.
func evsConcurrencyUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if evsError, ok := sidecar.EvsErrorOf(err); ok {
		return evsError.RefusesNothing() || evsError.OutcomeUnknown
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// The evs block's origin with the chaos hook in front of the world's lb (the file's header). Safe for concurrent use.
type evsConcurrencyOrigin struct {
	lb http.Handler

	stateLock sync.Mutex
	armed     *evsConcurrencyPushChaos
}

// One push chaos: the push it takes and what it does then, and once taken, the push and what the block answered it.
type evsConcurrencyPushChaos struct {
	match func(push *protocol.EvsPush) bool
	chaos func() error
	// closed once a push was taken and the chaos ran; the fields below are set before
	taken    chan struct{}
	pushId   sidecar.Id
	status   int
	version  uint64
	chaosErr error
}

// Serves the evs block's origin through the hook from now on, in place of the lb the world's first run of the block
// put there.
func installEvsConcurrencyOrigin(world *World) (*evsConcurrencyOrigin, error) {
	block := world.Block(WorldBlockEvs)
	if block == nil || block.service == "" {
		return nil, errors.New("the world serves no evs origin")
	}
	origin := &evsConcurrencyOrigin{lb: &worldLb{block: block}}
	api := world.T3().Api()
	if err := api.ServeService(block.service, nil); err != nil {
		return nil, err
	}
	if err := api.ServeService(block.service, origin); err != nil {
		return nil, err
	}
	return origin, nil
}

// Arms a chaos for the next push match takes; one armed at a time, a later arm replacing an earlier one not taken.
func (self *evsConcurrencyOrigin) arm(match func(push *protocol.EvsPush) bool, chaos func() error) *evsConcurrencyPushChaos {
	armed := &evsConcurrencyPushChaos{match: match, chaos: chaos, taken: make(chan struct{})}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = armed
	return armed
}

// Disarms armed, where no push took it.
func (self *evsConcurrencyOrigin) disarm(armed *evsConcurrencyPushChaos) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.armed == armed {
		self.armed = nil
	}
}

func (self *evsConcurrencyOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	chaos := self.take(r)
	if chaos == nil {
		self.lb.ServeHTTP(w, r)
		return
	}
	answer := httptest.NewRecorder()
	self.lb.ServeHTTP(answer, r)
	chaos.status = answer.Code
	if answer.Code == http.StatusOK {
		reply := &protocol.EvsPushReply{}
		if err := proto.Unmarshal(answer.Body.Bytes(), reply); err == nil {
			chaos.version = reply.GetVersion()
		}
	}
	chaos.chaosErr = chaos.chaos()
	close(chaos.taken)
	http.Error(w, "the upstream went away", http.StatusBadGateway)
}

// The armed chaos where r is a push it matches, which this takes; nil for every other request. A push's body is read
// to judge it and put back for the lb.
func (self *evsConcurrencyOrigin) take(r *http.Request) *evsConcurrencyPushChaos {
	self.stateLock.Lock()
	armed := self.armed
	self.stateLock.Unlock()
	if armed == nil || r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/push") {
		return nil
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return nil
	}
	request := &protocol.EvsPushRequest{}
	if err := proto.Unmarshal(body, request); err != nil {
		return nil
	}
	statement, err := sidecar.ParseEvsStatementFormat2(request.GetStatement())
	if err != nil {
		if statement, err = sidecar.ParseEvsStatement(request.GetStatement()); err != nil {
			return nil
		}
	}
	if !armed.match(statement.Push) {
		return nil
	}
	pushId, err := sidecar.IdFromBytes(statement.Push.GetPushId())
	if err != nil {
		return nil
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.armed != armed {
		// another push took it first
		return nil
	}
	self.armed = nil
	armed.pushId = pushId
	return armed
}

// gkm's origin with the recorder in front of the world's lb (the file's header). Safe for concurrent use.
type evsConcurrencyGkmOrigin struct {
	lb http.Handler

	stateLock sync.Mutex
	// what the case says of the device a sidecar id names, given the scopes its token holds; nil until set
	describe func(sidecarId sidecar.Id, scopes []string) string
	refusals []*evsConcurrencyGkmRefusal
	// how many refusals the case has logged
	logged int
}

// One refusal gkm answered.
type evsConcurrencyGkmRefusal struct {
	time   time.Time
	status int
	// the method and path, its ids and numbers as {id} and {n}, with no query
	route string
	// the bearer's claims, read unverified; claimed is false where the bearer held none that parse
	claimed           bool
	projectionVersion uint64
	scopes            []string
	sidecarId         sidecar.Id
	// what describe said of the device at the answer, "" with no describe or no sidecar
	device string
}

// What a service token claims that the recorder reads (server/oauth's ServiceTokenClaims).
type evsConcurrencyServiceTokenClaims struct {
	Scope       string `json:"scope"`
	ProjVersion uint64 `json:"proj_v"`
	SidecarId   string `json:"ur_sidecar_id"`
}

// Serves the gkm block's origin through the recorder from now on, in place of the lb the world's first run of the block
// put there.
func installEvsConcurrencyGkmOrigin(world *World) (*evsConcurrencyGkmOrigin, error) {
	block := world.Block(WorldBlockGkm)
	if block == nil || block.service == "" {
		return nil, errors.New("the world serves no gkm origin")
	}
	origin := &evsConcurrencyGkmOrigin{lb: &worldLb{block: block}}
	api := world.T3().Api()
	if err := api.ServeService(block.service, nil); err != nil {
		return nil, err
	}
	if err := api.ServeService(block.service, origin); err != nil {
		return nil, err
	}
	return origin, nil
}

// Sets what the recorder asks of a refused caller's device from now on.
func (self *evsConcurrencyGkmOrigin) setDescribe(describe func(sidecarId sidecar.Id, scopes []string) string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.describe = describe
}

func (self *evsConcurrencyGkmOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	status := &evsConcurrencyStatusWriter{ResponseWriter: w}
	self.lb.ServeHTTP(status, r)
	if status.status != http.StatusUnauthorized && status.status != http.StatusForbidden {
		return
	}
	refusal := &evsConcurrencyGkmRefusal{time: time.Now(), status: status.status, route: r.Method + " " + evsConcurrencyRoute(r.URL.Path)}
	if claims := evsConcurrencyBearerClaims(r); claims != nil {
		refusal.claimed = true
		refusal.projectionVersion = claims.ProjVersion
		refusal.scopes = strings.Fields(claims.Scope)
		refusal.sidecarId, _ = sidecar.ParseId(claims.SidecarId)
	}
	self.stateLock.Lock()
	describe := self.describe
	self.stateLock.Unlock()
	if describe != nil && refusal.sidecarId != (sidecar.Id{}) {
		refusal.device = describe(refusal.sidecarId, refusal.scopes)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.refusals = append(self.refusals, refusal)
}

// The refusals recorded since the last call, oldest first.
func (self *evsConcurrencyGkmOrigin) takeRefusals() []*evsConcurrencyGkmRefusal {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	refusals := slices.Clone(self.refusals[self.logged:])
	self.logged = len(self.refusals)
	return refusals
}

// A path as a route: each 32-character hex segment {id}, each number {n}.
func evsConcurrencyRoute(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		if segment == "" {
			continue
		}
		hex, digits := len(segment) == 32, true
		for _, c := range segment {
			if c < '0' || '9' < c {
				digits = false
				if c < 'a' || 'f' < c {
					hex = false
				}
			}
		}
		switch {
		case hex:
			segments[i] = "{id}"
		case digits:
			segments[i] = "{n}"
		}
	}
	return strings.Join(segments, "/")
}

// The claims of r's bearer, read without verifying it, nil where it holds none that parse.
func evsConcurrencyBearerClaims(r *http.Request) *evsConcurrencyServiceTokenClaims {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return nil
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	claims := &evsConcurrencyServiceTokenClaims{}
	if err := json.Unmarshal(payload, claims); err != nil {
		return nil
	}
	return claims
}

// A response writer that keeps the status it was answered with.
type evsConcurrencyStatusWriter struct {
	http.ResponseWriter
	status int
}

func (self *evsConcurrencyStatusWriter) WriteHeader(status int) {
	if self.status == 0 {
		self.status = status
	}
	self.ResponseWriter.WriteHeader(status)
}

func (self *evsConcurrencyStatusWriter) Write(b []byte) (int, error) {
	if self.status == 0 {
		self.status = http.StatusOK
	}
	return self.ResponseWriter.Write(b)
}

// What http.ResponseController reaches through, the lb's flushes among it.
func (self *evsConcurrencyStatusWriter) Unwrap() http.ResponseWriter {
	return self.ResponseWriter
}

// Waits wait, or until ctx ends, whose error it answers.
func evsConcurrencyPause(ctx context.Context, wait time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}
