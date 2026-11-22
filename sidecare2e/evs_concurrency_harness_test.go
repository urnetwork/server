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
//     orphan, container and pack it judges must be a day old first
//   - a hold: api mirrors each hold as hold/<network>/<case>/_ on the first metadata cluster, which evs's purge and
//     drop job read (§21.9); the world runs no abuse staff route, and abuse.yml's holds_enabled stays off until counsel
//     confirms holds (M5), so the hold is placed as api writes its object, through model's AbuseHoldStore on api's own
//     write budget, and released as AbuseHoldPass ends one, its object deleted
//   - the write budget's reports: every process reports into {nvb_<set>} under a 15 s ttl, so a sampler reads every
//     metadata cluster's reports for the case's life, keeping each process's newest, and a run chaos stopped stays in
//     the ledger after its report lapses
//   - the chaos at evs's origin: a hook in front of the world's lb for the evs block (worldLb) that, once armed, takes
//     the first push it matches, lets the block answer it in full, restarts the block on its stores, and answers the
//     pusher 502 with no evs body, as an lb whose upstream went away before the answer reached it: the pusher sends the
//     same push id and bytes again (QD-758), and the restarted block must answer the push as committed (§11.8's 5a),
//     so a restart lands exactly between a commit and its answer
//
// Safe for concurrent use.

package sidecare2e

import (
	"bytes"
	"context"
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
	"time"

	"google.golang.org/protobuf/proto"

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
	cancel context.CancelFunc
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
	host.Budget = budget
	return &evsConcurrencyWorker{world: world, host: host, budget: budget, cancel: cancel}, nil
}

// The process id the stand-in's budget reports under.
func (self *evsConcurrencyWorker) processId() string {
	return self.budget.ProcessId()
}

// Ends the stand-in's budget and gates.
func (self *evsConcurrencyWorker) close() {
	self.budget.Close()
	self.cancel()
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
	if budget := world.T3().ApiWriteBudget(); budget != nil {
		priceTable, err := evsservice.DefaultPriceTable(evsservice.DefaultEvsSettings().PriceBlockByteCount)
		if err != nil {
			return nil, err
		}
		store.SetWriteBudget(budget, func(objectByteCount int64) int64 {
			priced, err := priceTable.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: objectByteCount, Count: 1})
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
	return &evsConcurrencyHold{store: store, record: record}, nil
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

// Waits wait, or until ctx ends, whose error it answers.
func evsConcurrencyPause(ctx context.Context, wait time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}
