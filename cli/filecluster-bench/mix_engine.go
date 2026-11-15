package main

// The mix's in-process target: evsservice's engines as ProcessCount evs processes over the clusters under test, each
// with its own write budget client, quota gate and usage batcher; a budget loop per metadata cluster stepping the
// governor on the measured cluster's real drive counters, refilled at the run's refill; evacuation per metadata
// cluster each EvacuationInterval; and Networks x WritersPerNetwork writers, each a device of its own committing k
// entry writes a push through sidecar.EvsClient as built, on a ref of its own in its network's repo, every repo
// placed on the measured cluster. Requests reach a process by turns, as the lb's would.
//
// What it stands on is sidecartest's: an in-process Redis, an authenticator, gates, quota, gkm and an authorization
// state that admits every writer, so a run measures the store and the engines' writes, never api's, gkm's commit
// path or Redis. It drives pushes, the folds their page-outs cause and evacuation; parts, segment uploads, gkm
// commits, rotations, Adds, path-epoch copies, removed entries, projection writes, reconnects and full resyncs it
// does not, and says so, so the report never takes its phases as the whole mix. Its observer reads the engines'
// metrics and asks evacuation for the oldest batch at each sample.
//
// A restart swaps every process for a new one at once and then closes the old, so requests in flight on them fail
// and their resets stay incomplete. Safe for concurrent use.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The in-process target's shape. DefaultEngineMixSettings is the run's.
type EngineMixSettings struct {
	// evs processes: 8, near §11.13's ~8 processes a set near capacity sees, which sets records per batch.
	ProcessCount int
	// Networks, each one repo, and writers in each, every writer its own sidecar and ref: 64 and 8.
	NetworkCount      int
	WritersPerNetwork int
	// §11.13's k entry writes a push, each EntryByteCount bytes, over PathsPerWriter paths a writer rewrites in
	// turn, so its entries are overwritten and folded as a working ref's are: 3, 512 B and 64.
	EntryWritesPerPush int
	EntryByteCount     int
	PathsPerWriter     int
	// Each metadata cluster's evacuation runs this often, as taskworker's hourly run: 1 h.
	EvacuationInterval time.Duration
	// The tokens a device is issued last this long: 15 min, the exchange's.
	TokenLife time.Duration
	// The balance each network holds, so quota never refuses: 1 TiB of credit.
	NetworkBalance int64
	// How long before the run the upload receipts' one key was issued, so it is current throughout: 48 h, its
	// previous key's keep (§11.13's keys row).
	ReceiptKeyAge time.Duration
}

func DefaultEngineMixSettings() *EngineMixSettings {
	return &EngineMixSettings{
		ProcessCount:       8,
		NetworkCount:       64,
		WritersPerNetwork:  8,
		EntryWritesPerPush: 3,
		EntryByteCount:     512,
		PathsPerWriter:     64,
		EvacuationInterval: time.Hour,
		TokenLife:          15 * time.Minute,
		NetworkBalance:     1024 * 1024 * 1024 * 1024,
		ReceiptKeyAge:      48 * time.Hour,
	}
}

// One evs process: its engine and what it alone holds.
type engineProcess struct {
	engine  *evsservice.EvsEngine
	closers []func()
}

func (self *engineProcess) close() {
	for i := len(self.closers) - 1; 0 <= i; i -= 1 {
		self.closers[i]()
	}
}

// One writer: a device committing to its own ref, one push at a time.
type engineWriter struct {
	pushLock sync.Mutex
	device   *sidecar.EvsClient
	repoId   sidecar.Id
	refName  []byte
	pushes   int64
}

// See the file header.
type engineMixTarget struct {
	ctx         context.Context
	cancel      context.CancelFunc
	settings    *EngineMixSettings
	fakes       *sidecartest.FakeHost
	host        *sidecar.ServiceHost
	blobs       *timedBlobs
	metrics     *benchMetrics
	clusterId   string
	clusterIds  []string
	budget      *evsservice.WriteBudgetSettings
	engine      *evsservice.EvsSettings
	receiptKeys *evsservice.ReceiptKeys
	authState   *engineAuthState
	gateViews   *engineGateViews
	evacuator   *evsservice.Evacuator
	writers     []*engineWriter
	nextProcess atomic.Uint64
	backgrounds sync.WaitGroup
	closers     []func()

	stateLock sync.Mutex
	processes []*engineProcess
}

// The in-process target over store, every repo placed on clusterId, its budget loops reading counters, the budget
// refilled at refillDwpd. clockCheck may be nil for one that is always sure, which the run notes.
func newEngineMixTarget(
	ctx context.Context,
	store sidecar.BlobStore,
	clusterId string,
	counters DriveCounterReader,
	clockCheck sidecar.ClockCheck,
	refillDwpd float64,
	latencySampleCount int,
	settings *EngineMixSettings,
) (target *engineMixTarget, returnErr error) {
	if settings.ProcessCount <= 0 || settings.NetworkCount <= 0 || settings.WritersPerNetwork <= 0 || settings.EntryWritesPerPush <= 0 || settings.PathsPerWriter <= 0 {
		return nil, fmt.Errorf("an engine target of %d processes, %d networks of %d writers and %d entries a push", settings.ProcessCount, settings.NetworkCount, settings.WritersPerNetwork, settings.EntryWritesPerPush)
	}
	targetCtx, cancel := context.WithCancel(ctx)
	clock := sidecar.RealClock{}
	fakes := sidecartest.NewFakeHost(targetCtx, clock, nil, nil, nil)
	target = &engineMixTarget{
		ctx:       targetCtx,
		cancel:    cancel,
		settings:  settings,
		fakes:     fakes,
		blobs:     newTimedBlobs(store, latencySampleCount, clock),
		metrics:   newBenchMetrics(),
		clusterId: clusterId,
		authState: &engineAuthState{repoNetworkIds: map[sidecar.Id]sidecar.Id{}},
		gateViews: &engineGateViews{networkViews: map[sidecar.Id]sidecar.GkmAuthView{}},
	}
	defer func() {
		if returnErr != nil {
			target.Close()
		}
	}()
	sidecartest.RegisterEvsTwins(fakes.Redis)
	host := fakes.ServiceHost()
	host.Blobs = target.blobs
	host.Metrics = target.metrics
	if clockCheck != nil {
		host.ClockCheck = clockCheck
	}
	target.host = host
	target.clusterIds = store.MetadataClusterIds()

	target.budget = evsservice.DefaultWriteBudgetSettings()
	target.budget.RefillDwpd = refillDwpd
	target.engine = evsservice.DefaultEvsSettings()
	target.engine.InstanceFormat = sidecar.EvsRepoFormat1
	receiptKey := sha256.Sum256([]byte("filecluster-bench receipt key"))
	receiptKeys, err := evsservice.NewReceiptKeys([]*evsservice.ReceiptKey{
		{KeyId: 1, Key: receiptKey[:], IssueFrom: clock.Now().Add(-settings.ReceiptKeyAge)},
	}, nil)
	if err != nil {
		return nil, err
	}
	target.receiptKeys = receiptKeys

	// the budget loops, one a metadata cluster, on the measured cluster's counters
	loopSettings := evsservice.DefaultBudgetLoopSettings()
	loopSettings.Budget = target.budget
	for _, metadataClusterId := range target.clusterIds {
		loop, err := evsservice.NewBudgetLoop(host, metadataClusterId, &engineDriveCounters{counters: counters}, sidecar.NewNoopLogger(), loopSettings)
		if err != nil {
			return nil, fmt.Errorf("the budget loop on %s: %w", metadataClusterId, err)
		}
		target.backgrounds.Go(func() { loop.Run(targetCtx) })
	}

	// the networks, their repos and writers
	first, err := store.FirstCluster()
	if err != nil {
		return nil, err
	}
	placed, err := store.Cluster(clusterId)
	if err != nil {
		return nil, err
	}
	gkm := sidecartest.NewFakeGkm(clock, host.Blobs, nil)
	networkIds := []string{}
	type pendingWriter struct {
		signer    *sidecartest.FakeGkmSigner
		networkId sidecar.Id
		repoId    sidecar.Id
		view      sidecar.GkmAuthView
		index     int
	}
	pending := []*pendingWriter{}
	for range settings.NetworkCount {
		networkId := sidecar.NewId()
		repoId := sidecar.NewId()
		networkIds = append(networkIds, strconv.Quote(connect.Id(networkId).String()))
		fakes.Quota.SetBalance(connect.Id(networkId), settings.NetworkBalance)
		signers := []*sidecartest.FakeGkmSigner{}
		sidecarIds := []sidecar.Id{}
		identityKeys := map[sidecar.Id][]byte{}
		for range settings.WritersPerNetwork {
			signer := sidecartest.NewFakeGkmSigner()
			signers = append(signers, signer)
			sidecarIds = append(sidecarIds, signer.SidecarId)
			identityKeys[signer.SidecarId] = signer.PublicKey()
		}
		rows := sidecartest.FakeGkmAuthRows(&protocol.AclRepo{RepoId: repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL})
		sidecarRows, member := sidecartest.FakeGkmAuthMemberDevices(sidecar.NewId(), sidecarIds...)
		rows.Sidecars = sidecarRows
		rows.Members = []*bridge.MemberRow{member}
		view := sidecartest.NewFakeGkmAuthLog().Append(&sidecartest.FakeGkmAuthFixture{
			Rows:         rows,
			IdentityKeys: identityKeys,
			RepoReaders:  map[sidecar.Id][]sidecar.Id{repoId: sidecarIds},
			RepoWriters:  map[sidecar.Id][]sidecar.Id{repoId: sidecarIds},
		})
		target.authState.repoNetworkIds[repoId] = networkId
		target.gateViews.networkViews[networkId] = view

		placeBytes, err := (&sidecar.EvsRepoPlace{ClusterId: clusterId, CreatorSidecarId: signers[0].SidecarId}).Marshal()
		if err != nil {
			return nil, err
		}
		if _, err := first.PutIfNoneMatch(targetCtx, sidecar.EvsPlaceKey(repoId), placeBytes, nil); err != nil {
			return nil, fmt.Errorf("place/ for a repo: %w", err)
		}
		repoBytes, err := proto.Marshal(&protocol.EvsRepoObject{
			RepoId: repoId.Bytes(), Kind: protocol.RepoKind_REPO_KIND_GENERAL, State: protocol.EvsRepoState_EVS_REPO_ACTIVE,
			Format: sidecar.EvsRepoFormat1, NetworkId: networkId.Bytes(),
		})
		if err != nil {
			return nil, err
		}
		if _, err := placed.PutIfNoneMatch(targetCtx, sidecar.EvsRepoObjectKey(repoId), repoBytes, nil); err != nil {
			return nil, fmt.Errorf("repo/ for a repo: %w", err)
		}
		groupClient := gkm.Client(signers[0].SidecarId, signers[0].Sign)
		groupClient.AuthAdvanced(view)
		if _, err := groupClient.CreateGroup(targetCtx, &sidecar.GkmCreateGroupArgs{RepoId: repoId, Kind: protocol.RepoKind_REPO_KIND_GENERAL}); err != nil {
			return nil, fmt.Errorf("a repo's group: %w", err)
		}
		for i, signer := range signers {
			pending = append(pending, &pendingWriter{signer: signer, networkId: networkId, repoId: repoId, view: view, index: i})
		}
	}
	if err := fakes.Gates.Set(fmt.Sprintf("sidecar:\n  evs: {enabled: true, networks: [%s]}\n", strings.Join(networkIds, ", "))); err != nil {
		return nil, err
	}

	if err := target.startProcesses(); err != nil {
		return nil, err
	}

	for _, writer := range pending {
		deviceGkm := gkm.Client(writer.signer.SidecarId, writer.signer.Sign)
		deviceGkm.AuthAdvanced(writer.view)
		clientId := sidecar.NewId()
		identity := &sidecar.SidecarIdentity{
			NetworkId: writer.networkId, SidecarId: writer.signer.SidecarId, ClientId: clientId, Generation: 1,
			IdentityPublicKey: writer.signer.PublicKey(),
		}
		networkId := writer.networkId
		sidecarId := writer.signer.SidecarId
		view := writer.view
		device, err := sidecar.NewEvsClient(&sidecar.EvsClientArgs{
			NetworkId: networkId,
			EvsUrl:    "https://evs.example",
			Transport: target,
			ServiceToken: func(ctx context.Context, resource string, scopes []string) (string, error) {
				connectSidecarId := connect.Id(sidecarId)
				return fakes.Authenticator.Issue(&sidecar.Principal{
					NetworkId: connect.Id(networkId),
					ClientId:  connect.Id(clientId),
					SidecarId: &connectSidecarId,
					Audience:  evsservice.EvsAudience,
					Scopes:    scopes,
					IssuedAt:  clock.Now(),
					ExpiresAt: clock.Now().Add(settings.TokenLife),
				}), nil
			},
			Sign:     sidecar.EvsSignFunction(writer.signer.Sign),
			Identity: func() *sidecar.SidecarIdentity { return identity },
			Gkm:      deviceGkm,
			Store:    sidecartest.NewFakeStore().Root(),
			Config:   func() sidecar.GkmAuthView { return view },
			Clock:    clock,
			Log:      sidecar.NewNoopLogger(),
			Settings: sidecar.DefaultEvsSettings(),
		})
		if err != nil {
			return nil, err
		}
		target.closers = append(target.closers, device.Close)
		target.writers = append(target.writers, &engineWriter{
			device:  device,
			repoId:  writer.repoId,
			refName: []byte(fmt.Sprintf("load/%d", writer.index)),
		})
	}

	// evacuation, as taskworker's hourly run
	evacuator, err := evsservice.NewEvacuator(host, &evsservice.EvacuatorDeps{Log: sidecar.NewNoopLogger(), Format2: evsservice.NewFormat2Gate(host)})
	if err != nil {
		return nil, err
	}
	target.evacuator = evacuator
	target.backgrounds.Go(func() {
		for {
			select {
			case <-targetCtx.Done():
				return
			case <-clock.After(settings.EvacuationInterval):
			}
			for _, metadataClusterId := range target.clusterIds {
				// a failed run waits for the next, as taskworker's would
				evacuator.Run(targetCtx, metadataClusterId)
			}
		}
	})
	return target, nil
}

// A new process for every slot, then the old ones closed: see the file header.
func (self *engineMixTarget) startProcesses() error {
	processes := []*engineProcess{}
	for range self.settings.ProcessCount {
		process, err := self.newProcess()
		if err != nil {
			for _, started := range processes {
				started.close()
			}
			return err
		}
		processes = append(processes, process)
	}
	var old []*engineProcess
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		old = self.processes
		self.processes = processes
	}()
	for _, process := range old {
		process.close()
	}
	return nil
}

// One evs process: its own budget client, quota gate, usage batcher and engine, as a block of its own holds them.
func (self *engineMixTarget) newProcess() (returnProcess *engineProcess, returnErr error) {
	process := &engineProcess{}
	defer func() {
		if returnErr != nil {
			process.close()
		}
	}()
	processHost := *self.host
	budget, err := evsservice.NewWriteBudget(self.ctx, &processHost, nil, self.budget)
	if err != nil {
		return nil, fmt.Errorf("a process's write budget: %w", err)
	}
	process.closers = append(process.closers, budget.Close)
	processHost.Budget = budget
	usage, err := evsservice.NewUsageBatcher(self.ctx, &processHost, nil)
	if err != nil {
		return nil, err
	}
	process.closers = append(process.closers, usage.Close)
	quota, err := evsservice.NewQuotaGate(self.ctx, &processHost, nil)
	if err != nil {
		return nil, err
	}
	process.closers = append(process.closers, quota.Close)
	engine, err := evsservice.NewEvsEngine(self.ctx, &processHost, self.engine, &evsservice.EvsEngineDeps{
		AuthState: self.authState,
		Uploads:   &evsservice.EvsUploadDeps{Keys: self.receiptKeys, Quota: quota, Usage: usage},
		GateViews: self.gateViews,
	})
	if err != nil {
		return nil, err
	}
	process.closers = append(process.closers, engine.Close)
	process.engine = engine
	return process, nil
}

// The devices' transport: a process by turns.
func (self *engineMixTarget) RoundTrip(request *http.Request) (*http.Response, error) {
	process := func() *engineProcess {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if len(self.processes) == 0 {
			return nil
		}
		return self.processes[self.nextProcess.Add(1)%uint64(len(self.processes))]
	}()
	if process == nil {
		return nil, errors.New("no evs process runs")
	}
	recorder := httptest.NewRecorder()
	process.engine.Handler().ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func (self *engineMixTarget) Push(ctx context.Context, arrival int64) error {
	writer := self.writers[arrival%int64(len(self.writers))]
	writer.pushLock.Lock()
	defer writer.pushLock.Unlock()
	writes := []*sidecar.EvsPathWrite{}
	for i := range self.settings.EntryWritesPerPush {
		path := (writer.pushes*int64(self.settings.EntryWritesPerPush) + int64(i)) % int64(self.settings.PathsPerWriter)
		seed := []byte(fmt.Sprintf("%d.%d.%d;", arrival, path, i))
		content := bytes.Repeat(seed, self.settings.EntryByteCount/len(seed)+1)[:self.settings.EntryByteCount]
		writes = append(writes, &sidecar.EvsPathWrite{Path: fmt.Sprintf("p/%03d", path), Content: bytes.NewReader(content)})
	}
	writer.pushes += 1
	_, err := writer.device.Repo(writer.repoId).WritePaths(ctx, &sidecar.EvsWritePathsArgs{RefName: writer.refName, Writes: writes})
	return err
}

func (self *engineMixTarget) Restart(ctx context.Context) error {
	return self.startProcesses()
}

func (self *engineMixTarget) Events() ([]string, []string) {
	driven := []string{MixEventPush, MixEventFold, MixEventEvacuation}
	missing := []string{}
	for _, event := range MixEvents {
		if !slices.Contains(driven, event) {
			missing = append(missing, event)
		}
	}
	return driven, missing
}

func (self *engineMixTarget) TakeOperationLatencies() map[string][]time.Duration {
	return self.blobs.take()
}

func (self *engineMixTarget) MetadataClusterIds() []string {
	return []string{self.clusterId}
}

func (self *engineMixTarget) Close() {
	self.cancel()
	for i := len(self.closers) - 1; 0 <= i; i -= 1 {
		self.closers[i]()
	}
	self.closers = nil
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, process := range self.processes {
			process.close()
		}
		self.processes = nil
	}()
	self.backgrounds.Wait()
}

// The target's observer: the engines' and loops' metrics, and evacuation's oldest batch on each metadata cluster
// read at the sample, since evacuation sets its gauge only when it runs.
func (self *engineMixTarget) Observe(ctx context.Context) ([]*metricSample, error) {
	samples := self.metrics.samples()
	for _, clusterId := range self.clusterIds {
		age, ok, err := self.evacuator.OldestBatchAge(ctx, clusterId)
		if err != nil {
			return nil, fmt.Errorf("the oldest batch on %s: %w", clusterId, err)
		}
		if ok {
			samples = append(samples, &metricSample{Name: seriesOldestBatch, Labels: map[string]string{"cluster": clusterId}, Value: age.Seconds()})
		}
	}
	return samples, nil
}

// server's counters as the budget loop reads them, admin info's use known since a reading needs it fresh.
type engineDriveCounters struct {
	counters DriveCounterReader
}

func (self *engineDriveCounters) ReadDriveCounters(ctx context.Context, clusterId string) (*evsservice.DriveCounterReading, error) {
	if self.counters == nil {
		return nil, errors.New("no drive counter source")
	}
	reading, err := self.counters.ReadDriveCounters(ctx, clusterId)
	if err != nil {
		return nil, err
	}
	return convertDriveCounterReading(reading), nil
}

func convertDriveCounterReading(reading *server.FileClusterCounterReading) *evsservice.DriveCounterReading {
	converted := &evsservice.DriveCounterReading{
		UsageKnown:           true,
		RawUsageByteCount:    reading.RawUsageByteCount,
		RawCapacityByteCount: reading.RawCapacityByteCount,
		UsedInodeCount:       reading.UsedInodeCount,
		FreeInodeCount:       reading.FreeInodeCount,
	}
	for _, drive := range reading.Drives {
		converted.Drives = append(converted.Drives, &evsservice.DriveCounter{
			DriveId:          drive.DriveId,
			WrittenByteCount: drive.WrittenByteCount,
			WritesCompleted:  drive.WritesCompleted,
			ReadsCompleted:   drive.ReadsCompleted,
			Healing:          drive.Healing,
			Missing:          drive.Missing,
		})
	}
	return converted
}

// An authorization state that admits every writer of the target's networks to its own network's repo.
type engineAuthState struct {
	repoNetworkIds map[sidecar.Id]sidecar.Id
}

func (self *engineAuthState) CheckRevocation(ctx context.Context, principal *sidecar.Principal) error {
	return nil
}

func (self *engineAuthState) MatchBinding(principal *sidecar.Principal) evsservice.EvsAuthAnswer {
	return evsservice.EvsAuthAllowed
}

func (self *engineAuthState) HonorScope(principal *sidecar.Principal, repoId sidecar.Id, rights sidecar.AclEvsRights, now time.Time) evsservice.EvsAuthAnswer {
	if networkId, ok := self.repoNetworkIds[repoId]; !ok || connect.Id(networkId) != principal.NetworkId {
		return evsservice.EvsAuthDenied
	}
	return evsservice.EvsAuthAllowed
}

func (self *engineAuthState) Repo(principal *sidecar.Principal, repoId sidecar.Id) (*evsservice.EvsRepoFacts, evsservice.EvsAuthAnswer) {
	networkId, ok := self.repoNetworkIds[repoId]
	if !ok {
		return nil, evsservice.EvsAuthDenied
	}
	return &evsservice.EvsRepoFacts{Kind: protocol.RepoKind_REPO_KIND_GENERAL, NetworkId: networkId}, evsservice.EvsAuthAllowed
}

type engineGateViews struct {
	networkViews map[sidecar.Id]sidecar.GkmAuthView
}

func (self *engineGateViews) View(networkId sidecar.Id) (sidecar.GkmAuthView, evsservice.EvsAuthAnswer) {
	view, ok := self.networkViews[networkId]
	if !ok {
		return nil, evsservice.EvsAuthUnknown
	}
	return view, evsservice.EvsAuthAllowed
}

// sidecar.Metrics for the target's processes, read back as samples. Safe for concurrent use.
type benchMetrics struct {
	stateLock sync.Mutex
	// by name, then by label values joined
	values      map[string]map[string]float64
	labelNames  map[string][]string
	seriesLabel map[string]map[string][]string
}

func newBenchMetrics() *benchMetrics {
	return &benchMetrics{values: map[string]map[string]float64{}, labelNames: map[string][]string{}, seriesLabel: map[string]map[string][]string{}}
}

func (self *benchMetrics) declare(name string, labelNames []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.values[name]; !ok {
		self.values[name] = map[string]float64{}
		self.seriesLabel[name] = map[string][]string{}
		self.labelNames[name] = append([]string{}, labelNames...)
	}
}

func (self *benchMetrics) update(name string, labelValues []string, apply func(value float64) float64) {
	key := ""
	for _, value := range labelValues {
		key += value + "\x1f"
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	values, ok := self.values[name]
	if !ok {
		return
	}
	values[key] = apply(values[key])
	if _, ok := self.seriesLabel[name][key]; !ok {
		self.seriesLabel[name][key] = append([]string{}, labelValues...)
	}
}

func (self *benchMetrics) Counter(name string, help string, labelNames ...string) sidecar.MetricsCounter {
	self.declare(name, labelNames)
	return &benchMetricsSeries{metrics: self, name: name, add: true}
}

func (self *benchMetrics) Gauge(name string, help string, labelNames ...string) sidecar.MetricsGauge {
	self.declare(name, labelNames)
	return &benchMetricsSeries{metrics: self, name: name}
}

func (self *benchMetrics) Histogram(name string, help string, bucketUpperBounds []float64, labelNames ...string) sidecar.MetricsHistogram {
	self.declare(name, labelNames)
	return &benchMetricsSeries{metrics: self, name: name, add: true}
}

// Every series as a sample: a counter's total, a gauge's level, a histogram's sum.
func (self *benchMetrics) samples() []*metricSample {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	samples := []*metricSample{}
	for name, values := range self.values {
		labelNames := self.labelNames[name]
		for key, value := range values {
			labels := map[string]string{}
			for i, labelValue := range self.seriesLabel[name][key] {
				if i < len(labelNames) {
					labels[labelNames[i]] = labelValue
				}
			}
			samples = append(samples, &metricSample{Name: name, Labels: labels, Value: value})
		}
	}
	return samples
}

type benchMetricsSeries struct {
	metrics *benchMetrics
	name    string
	add     bool
}

func (self *benchMetricsSeries) Add(value float64, labelValues ...string) {
	self.metrics.update(self.name, labelValues, func(current float64) float64 { return current + value })
}

func (self *benchMetricsSeries) Set(value float64, labelValues ...string) {
	self.metrics.update(self.name, labelValues, func(current float64) float64 { return value })
}

func (self *benchMetricsSeries) Observe(value float64, labelValues ...string) {
	self.metrics.update(self.name, labelValues, func(current float64) float64 { return current + value })
}
