package server

// §22.9's file-cluster suite (EVSB-M2-17; X13, D-OPS-14, D-OPS-20, FS-40, FS-63, FS-114, DESIGN-DETAILS 2.6 and 2.7): one
// runnable suite over a cluster's BlobCluster, so a new cluster and each build and upgrade run the same code
// (server/cli/filecluster-suite), each case reported pass, fail or not run, and the suite passed only when every case
// passed. What plan-EVSB-settle-gate records, settle_passed_build, is the one build every host ran through a passed
// suite (FileClusterSuiteReport.SettlePassedBuild); a report with any case failed or not run names none, so no
// collection window opens, and no purge or drop deletes, on a build the suite did not pass.
//
// The cases, by what they need of the cluster:
//
//	any cluster      one pool (owner decision 145, FS-63), checked before any write, so a cluster with two pools fails
//	                 the suite with nothing written; every host on one build, read from admin info, which is the
//	                 build the report names; no lifecycle rule on the bucket (QD-254); and the store cases the caller
//	                 adds (StoreCases: server/cli/filecluster-suite adds sidecartest/blobsuite's, the settle cases
//	                 through a path that sends a late request's x-ur-deadline-ms and never cancels at it)
//	barrier hooks    a settle_testhooks build of the same commit, with each host's control port (FS-114): a head
//	                 PUT's drive commits held after their checks past the drive timeout, on one host and on two, the
//	                 request cancelled; two DELETEs of one key, each with commits held past their deadline; a create
//	                 whose last drive commit is held before its check across an acknowledged DELETE of the key
//	host control     each host stopped and started (T3's containers): a LIST answered whole or 503 by how many of a
//	                 set's drives are offline; stale conditions refused with a host lost (§11.3)
//
// A case whose need the target lacks is not run, and a suite with a case not run is not passed, so a production
// cluster, which carries no hooks, is passed only on the strength of the same commit's hooked build passing on T3:
// the operator runs both and records the build only when both pass.
//
// It reports, beside the cases, the longest lag any host saw from a cancel to a write's last drive answer, which no
// margin relies on (§22.9), and how long a failed DELETE's key stayed held past its request's end (DESIGN-DETAILS 2.7).
//
// The suite runs on the store's own clock: a held commit outlasts the drive timeout in real time, as the store
// counts it, and a wait for the store polls it, bounded by WaitLimit (QD-264: FS-114 and this suite never use T3's
// clock wrapper). It writes only under a fresh prefix and deletes what it wrote. Not safe for concurrent use; one run
// at a time per cluster.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/sidecar"
)

// The fork's barrier points (minio/cmd/settle-testhooks.go), which FS-114 holds commits at.
const (
	FileClusterSuiteCommitArrived = "commit-arrived"
	FileClusterSuiteCommitChecked = "commit-checked"
)

// A case's outcome.
type FileClusterSuiteOutcome string

const (
	FileClusterSuitePass   FileClusterSuiteOutcome = "pass"
	FileClusterSuiteFail   FileClusterSuiteOutcome = "fail"
	FileClusterSuiteNotRun FileClusterSuiteOutcome = "not run"
)

// What the suite holds the cluster to and waits for.
type FileClusterSuiteSettings struct {
	// The cluster's drive timeout, which a held commit outlasts by a quarter: production's 30 s, a T3 cluster's
	// MINIO_DRIVE_MAX_TIMEOUT. Positive.
	DriveTimeout time.Duration
	// The deadline a held write carries, 10 s, and one a request expected to wait on a held key carries, so it fails
	// rather than waits out the hold, 1.5 s. Positive.
	HeldRequestDeadline  time.Duration
	ShortRequestDeadline time.Duration
	// The most any wait on the store takes before its case fails: 5 min, past a settle and a host's restart.
	// Positive.
	WaitLimit time.Duration
	// How often a wait asks the store again: 50 ms. Positive.
	PollInterval time.Duration
	// The bound on one control-port call: 10 s, and a barrier's wait for its commits, WaitLimit. Positive.
	HookTimeout time.Duration
	// How many keys the LIST case writes, 16, and how many hosts it stops at most, 2: one host's drives are inside the
	// line a LIST is whole at on a 4 x 2 set and two are past it. Positive.
	ListKeyCount     int
	ListStoppedHosts int
	// The most drives of a set offline at which the fork answers a LIST whole, 2: three or more, or one beside one
	// healing, answer 503 (§19.6's list row). The store's rule, which this only reads. Positive.
	ListWholeOfflineDrives int
	// FS-114's twin's head writes, 20. Positive.
	TwinRoundCount int
	// How much past its own deadline a request that must not wait on a held commit may take, for its round trips: 1 s.
	// Positive.
	RoundTripSlack time.Duration
	// The most of a control-port reply read: 64 KiB. Positive.
	HookReplyByteCount int64
	// A cleanup's LISTs of what a case wrote, 100, each of at most 1,000 keys. Positive.
	CleanupListCount int
	CleanupListLimit int
}

// Production's drive timeout and the waits a run on it takes.
func DefaultFileClusterSuiteSettings() *FileClusterSuiteSettings {
	return &FileClusterSuiteSettings{
		DriveTimeout:           30 * time.Second,
		HeldRequestDeadline:    10 * time.Second,
		ShortRequestDeadline:   1500 * time.Millisecond,
		WaitLimit:              5 * time.Minute,
		PollInterval:           50 * time.Millisecond,
		HookTimeout:            10 * time.Second,
		ListKeyCount:           16,
		ListStoppedHosts:       2,
		ListWholeOfflineDrives: 2,
		TwinRoundCount:         20,
		RoundTripSlack:         time.Second,
		HookReplyByteCount:     64 * 1024,
		CleanupListCount:       100,
		CleanupListLimit:       1000,
	}
}

// One host of the cluster under test.
type FileClusterSuiteHost struct {
	// Its S3 endpoint, host:port, one of the cluster's.
	Endpoint string
	// Its barrier control port, host:port, on a settle_testhooks build; "" for none.
	HookAddress string
	// Stops and starts it, for the host-control cases; nil for none.
	Control FileClusterSuiteHostControl
}

// Takes a host down as a lost host is, and back.
type FileClusterSuiteHostControl interface {
	Stop(ctx context.Context) error
	Start(ctx context.Context) error
}

// A case the caller adds, run on the whole cluster after the suite's own checks: the store conformance cases.
type FileClusterSuiteStoreCases func(ctx context.Context, target *FileClusterSuiteStoreTarget) []*FileClusterSuiteCase

// What the caller's store cases run on.
type FileClusterSuiteStoreTarget struct {
	// The cluster through every host, as a block reaches it.
	Cluster sidecar.BlobCluster
	// The same cluster through a path that sends each request's x-ur-deadline-ms and never cancels at it, as a request
	// the network held past its deadline reaches the store (FS-114).
	Delayed sidecar.BlobCluster
	// The store's clock as this process reads it.
	Now func() time.Time
}

// The cluster a run tests.
type FileClusterSuiteTarget struct {
	// The cluster as the vault file names it.
	Config *FileClusterConfig
	// Each host, in Config.Endpoints' order.
	Hosts []*FileClusterSuiteHost
	// Each host's drives, which every case's quorum arithmetic reads: 2 at T3's 4 x 2 EC:4, 4 at its 4 x 4 EC:8, 8 for a
	// one-process cluster.
	DrivesPerHost int
	// The caller's store cases, nil for none.
	StoreCases FileClusterSuiteStoreCases
	// nil takes the defaults.
	Settings *FileClusterSuiteSettings

	// every host's base transport, nil for the real one; a test's fake store sets it
	newBaseTransport fileClusterBaseTransportFunction
}

// One case's outcome.
type FileClusterSuiteCase struct {
	Name    string
	Outcome FileClusterSuiteOutcome
	// Why it failed or did not run.
	Detail  string
	Elapsed time.Duration
}

// What a run found.
type FileClusterSuiteReport struct {
	ClusterId string
	// Each host's build as admin info gives it, in the target's order; "" for one unread.
	HostBuilds []string
	// The one build every host ran, "" when they differ or one was not read.
	Build string
	Cases []*FileClusterSuiteCase
	// The longest lag any host reported from a cancel to a write's last drive answer, which no margin relies on.
	CancelLag time.Duration
	// How long a failed DELETE's key stayed held past its request's end.
	DeleteHeldPastEnd time.Duration
	Started           time.Time
	Elapsed           time.Duration
}

// Whether every case ran and passed.
func (self *FileClusterSuiteReport) Passed() bool {
	if len(self.Cases) == 0 {
		return false
	}
	for _, c := range self.Cases {
		if c.Outcome != FileClusterSuitePass {
			return false
		}
	}
	return true
}

// The build sidecar.yml's settle_passed_build may name for this cluster (plan-EVSB-settle-gate): the one every host
// ran through a run where every case passed; false for any other run.
func (self *FileClusterSuiteReport) SettlePassedBuild() (string, bool) {
	if !self.Passed() || self.Build == "" {
		return "", false
	}
	return self.Build, true
}

// The report as a person reads it, one line a case.
func (self *FileClusterSuiteReport) String() string {
	builder := &strings.Builder{}
	fmt.Fprintf(builder, "cluster %s: build %q, hosts %v\n", self.ClusterId, self.Build, self.HostBuilds)
	for _, c := range self.Cases {
		fmt.Fprintf(builder, "  %-8s %s (%s)", c.Outcome, c.Name, c.Elapsed.Round(time.Millisecond))
		if c.Detail != "" {
			fmt.Fprintf(builder, ": %s", c.Detail)
		}
		builder.WriteString("\n")
	}
	fmt.Fprintf(builder, "cancel lag %s; a failed DELETE's key held %s past its request's end\n", self.CancelLag, self.DeleteHeldPastEnd)
	if build, ok := self.SettlePassedBuild(); ok {
		fmt.Fprintf(builder, "passed: settle_passed_build: %s\n", build)
	} else {
		builder.WriteString("not passed: record no settle_passed_build for this build\n")
	}
	return builder.String()
}

// What a case needs of the target.
type fileClusterSuiteNeed int

const (
	fileClusterSuiteNeedsNothing fileClusterSuiteNeed = iota
	fileClusterSuiteNeedsHooks
	fileClusterSuiteNeedsControl
)

// One of the suite's own cases: its name, what it needs and how it runs under a prefix of its own.
type fileClusterSuiteCaseSpec struct {
	name  string
	needs fileClusterSuiteNeed
	run   func(ctx context.Context, run *fileClusterSuiteRun, prefix string) error
}

// The suite's own cases after the cluster checks, in order.
func fileClusterSuiteCases() []*fileClusterSuiteCaseSpec {
	return []*fileClusterSuiteCaseSpec{
		{name: "fs114-twin", needs: fileClusterSuiteNeedsHooks, run: fileClusterSuiteTwin},
		{name: "fs114-head-one-host-held", needs: fileClusterSuiteNeedsHooks, run: fileClusterSuiteHeadOneHostHeld},
		{name: "fs114-head-two-hosts-held", needs: fileClusterSuiteNeedsHooks, run: fileClusterSuiteHeadTwoHostsHeld},
		{name: "fs114-two-deletes-held", needs: fileClusterSuiteNeedsHooks, run: fileClusterSuiteTwoDeletesHeld},
		{name: "fs114-create-held-across-a-delete", needs: fileClusterSuiteNeedsHooks, run: fileClusterSuiteCreateAcrossDelete},
		{name: "list-whole-or-503", needs: fileClusterSuiteNeedsControl, run: fileClusterSuiteListWhole},
		{name: "stale-conditions-refused-with-a-host-lost", needs: fileClusterSuiteNeedsControl, run: fileClusterSuiteStaleWithHostLost},
	}
}

// One run's clients and what it measures.
type fileClusterSuiteRun struct {
	target   *FileClusterSuiteTarget
	settings *FileClusterSuiteSettings
	clusters *FileClusters
	cluster  *FileCluster
	// a cluster per host, each through that host alone
	hostClusters []*FileCluster
	hostStores   []*FileClusters
	hooks        []*fileClusterSuiteHook
	report       *FileClusterSuiteReport
}

// Runs the suite on target and reports each case. An error is a target the suite cannot start on: a config no client
// builds from. A cluster that answers wrong, or not at all, is a failed case.
func RunFileClusterSuite(ctx context.Context, target *FileClusterSuiteTarget) (*FileClusterSuiteReport, error) {
	settings := target.Settings
	if settings == nil {
		settings = DefaultFileClusterSuiteSettings()
	}
	if target.Config == nil || len(target.Hosts) != len(target.Config.Endpoints) || target.DrivesPerHost <= 0 {
		return nil, fmt.Errorf("%w: a suite target names each host of its config and its drives", ErrFileClusterConfig)
	}
	for index, host := range target.Hosts {
		if host.Endpoint != target.Config.Endpoints[index] {
			return nil, fmt.Errorf("%w: host %d is %q, the config's %q", ErrFileClusterConfig, index, host.Endpoint, target.Config.Endpoints[index])
		}
	}
	clusterSettings := DefaultFileClusterSettings()
	newBaseTransport := target.newBaseTransport
	if newBaseTransport == nil {
		newBaseTransport = newFileClusterBaseTransport(clusterSettings)
	}
	clusters, err := newFileClusterSuiteStore(ctx, target.Config, target.Config.Endpoints, clusterSettings, newBaseTransport)
	if err != nil {
		return nil, err
	}
	defer clusters.Close()
	run := &fileClusterSuiteRun{
		target:   target,
		settings: settings,
		clusters: clusters,
		cluster:  clusters.clusterIdClusters[target.Config.ClusterId],
		report:   &FileClusterSuiteReport{ClusterId: target.Config.ClusterId, Started: time.Now()},
	}
	defer func() {
		for _, store := range run.hostStores {
			store.Close()
		}
	}()
	for _, host := range target.Hosts {
		hostStore, err := newFileClusterSuiteStore(ctx, target.Config, []string{host.Endpoint}, clusterSettings, newBaseTransport)
		if err != nil {
			return nil, err
		}
		run.hostStores = append(run.hostStores, hostStore)
		run.hostClusters = append(run.hostClusters, hostStore.clusterIdClusters[target.Config.ClusterId])
		var hook *fileClusterSuiteHook
		if host.HookAddress != "" {
			hook = &fileClusterSuiteHook{address: host.HookAddress, client: &http.Client{Timeout: settings.WaitLimit + settings.HookTimeout}, settings: settings}
		}
		run.hooks = append(run.hooks, hook)
	}
	run.runAll(ctx)
	run.report.Elapsed = time.Since(run.report.Started)
	return run.report, nil
}

// A store of config's cluster alone, reached through endpoints. It stands as its own first metadata cluster, since the
// suite reaches one cluster and never reads place/ or proj/ through it, so a segment cluster or a later metadata
// cluster runs the same way as the first.
func newFileClusterSuiteStore(ctx context.Context, config *FileClusterConfig, endpoints []string, settings *FileClusterSettings,
	newBaseTransport fileClusterBaseTransportFunction) (*FileClusters, error) {
	suiteConfig := *config
	suiteConfig.Kind = FileClusterKindMetadata
	suiteConfig.First = true
	suiteConfig.Endpoints = slices.Clone(endpoints)
	return newFileClusters(ctx, []*FileClusterConfig{&suiteConfig}, settings, newBaseTransport, CurrentSidecarConfig)
}

// Records a case that ran: a pass, or a failure with err's words.
func (self *fileClusterSuiteRun) add(name string, started time.Time, err error) {
	c := &FileClusterSuiteCase{Name: name, Outcome: FileClusterSuitePass, Elapsed: time.Since(started)}
	if err != nil {
		c.Outcome = FileClusterSuiteFail
		c.Detail = err.Error()
	}
	self.report.Cases = append(self.report.Cases, c)
}

// Records a case that did not run, and why.
func (self *fileClusterSuiteRun) notRun(name string, detail string) {
	self.report.Cases = append(self.report.Cases, &FileClusterSuiteCase{Name: name, Outcome: FileClusterSuiteNotRun, Detail: detail})
}

// The cluster checks, the caller's store cases, then the suite's own, each the target can run.
func (self *fileClusterSuiteRun) runAll(ctx context.Context) {
	// one pool, before any write: a cluster that has not shown one takes nothing (FS-63)
	started := time.Now()
	self.clusters.Check(ctx)
	info := self.cluster.LastAdminInfo()
	poolErr := func() error {
		switch {
		case info == nil:
			return errors.New("admin info was not read, so no write is sent")
		case info.PoolCount != 1:
			return fmt.Errorf("%d pools: a cluster grows by whole clusters, never by pools (owner decision 145), and takes no write", info.PoolCount)
		}
		return nil
	}()
	self.add("one-pool", started, poolErr)
	started = time.Now()
	self.add("one-build", started, self.readBuilds(info))
	if poolErr != nil {
		self.notRunRest("the cluster took no write: " + poolErr.Error())
		return
	}
	for _, hostStore := range self.hostStores {
		hostStore.Check(ctx)
	}
	started = time.Now()
	self.add("no-lifecycle-rule", started, self.checkNoLifecycle(ctx))
	if self.target.StoreCases != nil {
		started := time.Now()
		delayed, err := newFileClusterSuiteDelayedCluster(self.cluster)
		if err != nil {
			self.add("store-cases", started, err)
		} else {
			self.report.Cases = append(self.report.Cases, self.target.StoreCases(ctx, &FileClusterSuiteStoreTarget{Cluster: self.cluster, Delayed: delayed, Now: time.Now})...)
		}
	}
	runId := fileClusterSuiteRunId()
	for _, spec := range fileClusterSuiteCases() {
		if detail := self.lacks(spec.needs); detail != "" {
			self.notRun(spec.name, detail)
			continue
		}
		prefix := "filecluster-suite/" + runId + "/" + spec.name + "/"
		started := time.Now()
		err := spec.run(ctx, self, prefix)
		self.add(spec.name, started, err)
		self.cleanup(ctx, prefix)
	}
	self.readLag(ctx)
}

// Every case past the pool check, not run.
func (self *fileClusterSuiteRun) notRunRest(detail string) {
	self.notRun("no-lifecycle-rule", detail)
	for _, spec := range fileClusterSuiteCases() {
		self.notRun(spec.name, detail)
	}
}

// Why the target cannot run a case of needs, "" when it can.
func (self *fileClusterSuiteRun) lacks(needs fileClusterSuiteNeed) string {
	switch needs {
	case fileClusterSuiteNeedsHooks:
		if len(self.hooks) < 2 {
			return "a held commit needs a second host"
		}
		for index, hook := range self.hooks {
			if hook == nil {
				return fmt.Sprintf("host %d has no barrier control port: run this commit's settle_testhooks build on T3", index)
			}
		}
	case fileClusterSuiteNeedsControl:
		if len(self.target.Hosts) < 2 {
			return "a host lost needs a second host"
		}
		for index, host := range self.target.Hosts {
			if host.Control == nil {
				return fmt.Sprintf("host %d cannot be stopped from here", index)
			}
		}
	}
	return ""
}

// Each host's build from admin info, in its order, and the one build they share. Admin info names hosts by the
// addresses they reach each other on, which on T3 are not the ones the suite dials, so hosts are counted, not matched.
func (self *fileClusterSuiteRun) readBuilds(info *FileClusterAdminInfo) error {
	if info == nil {
		return errors.New("admin info was not read")
	}
	builds := []string{}
	for _, hostInfo := range info.Hosts {
		builds = append(builds, hostInfo.CommitId)
	}
	self.report.HostBuilds = builds
	if len(builds) != len(self.target.Hosts) {
		return fmt.Errorf("admin info lists %d hosts of the target's %d", len(builds), len(self.target.Hosts))
	}
	for index, build := range builds {
		if !fileClusterSuiteCommitId(build) {
			return fmt.Errorf("host %d runs %q, no stamped commit: settle_passed_build names a commit (buildscripts/sidecar-release.sh)", index, build)
		}
		if build != builds[0] {
			return fmt.Errorf("host %d runs %q and host 0 %q: a mixed cluster agrees on no commit", index, build, builds[0])
		}
	}
	self.report.Build = builds[0]
	return nil
}

// Whether build is a commit as the fork's release stamps it into admin info: 40 lowercase hex digits.
func fileClusterSuiteCommitId(build string) bool {
	decoded, err := hex.DecodeString(build)
	return err == nil && len(decoded) == 20 && hex.EncodeToString(decoded) == build
}

// No lifecycle rule on the bucket (QD-254): nothing but evs's jobs deletes.
func (self *fileClusterSuiteRun) checkNoLifecycle(ctx context.Context) error {
	host := self.cluster.picker.pick()
	config, err := host.core.GetBucketLifecycle(ctx, self.target.Config.Bucket)
	if err != nil {
		if response, ok := fileClusterErrorResponse(err); ok && response.Code == "NoSuchLifecycleConfiguration" {
			return nil
		}
		return fmt.Errorf("the bucket's lifecycle could not be read: %w", err)
	}
	if config != nil && 0 < len(config.Rules) {
		return fmt.Errorf("the bucket holds %d lifecycle rules", len(config.Rules))
	}
	return nil
}

// Best effort: what a failed case leaves stays under its run's prefix.
func (self *fileClusterSuiteRun) cleanup(ctx context.Context, prefix string) {
	for range self.settings.CleanupListCount {
		objects, more, err := self.cluster.List(ctx, prefix, "", self.settings.CleanupListLimit)
		if err != nil {
			return
		}
		for _, object := range objects {
			if err := self.cluster.Delete(ctx, object.Key); err != nil {
				return
			}
		}
		if !more {
			return
		}
	}
}

// The longest cancel lag any host's control port reports.
func (self *fileClusterSuiteRun) readLag(ctx context.Context) {
	for _, hook := range self.hooks {
		if hook == nil {
			continue
		}
		state, err := hook.state(ctx)
		if err != nil {
			continue
		}
		if lag := time.Duration(state.LagMs) * time.Millisecond; self.report.CancelLag < lag {
			self.report.CancelLag = lag
		}
	}
}

// A run's prefix id: 16 random hex digits.
func fileClusterSuiteRunId() string {
	var random [8]byte
	// never fails: since Go 1.24 a broken system generator crashes the program instead
	rand.Read(random[:])
	return hex.EncodeToString(random[:])
}

// One host's barrier control (minio/cmd/settle-testhooks_on.go): arm, wait, release and state over HTTP.
type fileClusterSuiteHook struct {
	address  string
	client   *http.Client
	settings *FileClusterSuiteSettings
}

// What a host tells of its writes and clock.
type fileClusterSuiteHostState struct {
	Online    int   `json:"online"`
	Refusals  int64 `json:"refusals"`
	Held      int   `json:"held"`
	HeldTotal int64 `json:"heldTotal"`
	Live      int   `json:"live"`
	OldestMs  int64 `json:"oldestMs"`
	LagMs     int64 `json:"lagMs"`
	ClockSure bool  `json:"clockSure"`
}

// One POST to the control port, bounded by timeout.
func (self *fileClusterSuiteHook) call(ctx context.Context, action string, values url.Values, timeout time.Duration) (int, []byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(callCtx, http.MethodPost, "http://"+self.address+"/"+action, strings.NewReader(values.Encode()))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := self.client.Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("the control port %s: %w", action, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, self.settings.HookReplyByteCount))
	return response.StatusCode, body, err
}

// Holds commits reaching point on a key containing object, every one or, with limit, the first limit.
func (self *fileClusterSuiteHook) arm(ctx context.Context, point string, object string, limit int) error {
	values := url.Values{"point": {point}, "object": {object}}
	if 0 < limit {
		values.Set("limit", strconv.Itoa(limit))
	}
	_, _, err := self.call(ctx, "arm", values, self.settings.HookTimeout)
	return err
}

// Waits for count commits held at point.
func (self *fileClusterSuiteHook) wait(ctx context.Context, point string, count int) error {
	status, _, err := self.call(ctx, "wait", url.Values{"point": {point}, "count": {strconv.Itoa(count)}, "timeout": {self.settings.WaitLimit.String()}},
		self.settings.WaitLimit+self.settings.HookTimeout)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%d commits never reached %s at %s", count, point, self.address)
	}
	return nil
}

// Lets go of every commit held at point.
func (self *fileClusterSuiteHook) release(ctx context.Context, point string) error {
	_, _, err := self.call(ctx, "release", url.Values{"point": {point}}, self.settings.HookTimeout)
	return err
}

// What the host tells of its writes, keys held and clock now.
func (self *fileClusterSuiteHook) state(ctx context.Context) (*fileClusterSuiteHostState, error) {
	_, body, err := self.call(ctx, "state", url.Values{}, self.settings.HookTimeout)
	if err != nil {
		return nil, err
	}
	state := &fileClusterSuiteHostState{}
	if err := json.Unmarshal(body, state); err != nil {
		return nil, fmt.Errorf("the control port's state: %w", err)
	}
	return state, nil
}

// Polls condition until it holds, failing with what past WaitLimit.
func (self *fileClusterSuiteRun) waitFor(ctx context.Context, what string, condition func() bool) error {
	deadline := time.Now().Add(self.settings.WaitLimit)
	for !condition() {
		if deadline.Before(time.Now()) {
			return fmt.Errorf("timed out waiting for %s", what)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(self.settings.PollInterval):
		}
	}
	return nil
}

// Waits until no host holds a key or runs a write: every settle done.
func (self *fileClusterSuiteRun) quiesce(ctx context.Context) error {
	return self.waitFor(ctx, "every host to settle", func() bool {
		for _, hook := range self.hooks {
			if hook == nil {
				continue
			}
			state, err := hook.state(ctx)
			if err != nil || 0 < state.Held || 0 < state.Live {
				return false
			}
		}
		return true
	})
}

// How many keys the hosts hold now.
func (self *fileClusterSuiteRun) heldNow(ctx context.Context) (int, error) {
	held := 0
	for _, hook := range self.hooks {
		state, err := hook.state(ctx)
		if err != nil {
			return 0, err
		}
		held += state.Held
	}
	return held, nil
}

// Lets a held commit outlast the drive timeout, as the deployed build counts it.
func (self *fileClusterSuiteRun) pastDriveTimeout(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(self.settings.DriveTimeout + self.settings.DriveTimeout/4):
		return nil
	}
}

// The body every host serves for key.
func (self *fileClusterSuiteRun) expectBody(ctx context.Context, key string, want []byte) error {
	for index, cluster := range self.hostClusters {
		object, err := cluster.Get(ctx, key)
		if err != nil {
			return fmt.Errorf("host %d: get %s: %w", index, key, err)
		}
		body, err := io.ReadAll(object.Body)
		object.Body.Close()
		if err != nil {
			return fmt.Errorf("host %d: read %s: %w", index, key, err)
		}
		if string(body) != string(want) {
			return fmt.Errorf("host %d: %s reads %q, want %q", index, key, body, want)
		}
	}
	return nil
}

// Every host answers 404 for key.
func (self *fileClusterSuiteRun) expectAbsent(ctx context.Context, key string) error {
	for index, cluster := range self.hostClusters {
		if _, err := cluster.Head(ctx, key); !errors.Is(err, sidecar.ErrBlobNotFound) {
			return fmt.Errorf("host %d: %s after its DELETE: %v", index, key, err)
		}
	}
	return nil
}

// A write whose answer is still to come.
type fileClusterSuiteHeld struct {
	cancel context.CancelFunc
	done   chan error
}

// Sends write, returning at once; its ctx carries deadline.
func (self *fileClusterSuiteRun) start(ctx context.Context, deadline time.Duration, write func(ctx context.Context) error) *fileClusterSuiteHeld {
	writeCtx, cancel := context.WithCancel(sidecar.WithBlobDeadline(ctx, time.Now().Add(deadline)))
	held := &fileClusterSuiteHeld{cancel: cancel, done: make(chan error, 1)}
	go func() {
		held.done <- write(writeCtx)
	}()
	return held
}

// The current ETag of key through host index.
func (self *fileClusterSuiteRun) etagOf(ctx context.Context, index int, key string) (string, error) {
	info, err := self.hostClusters[index].Head(ctx, key)
	if err != nil {
		return "", err
	}
	return info.Etag, nil
}

// FS-114's twin: with no barrier every write releases at its write quorum. Twenty head writes through each host in
// turn, each read through the next host, and no host ever holds the key.
func fileClusterSuiteTwin(ctx context.Context, run *fileClusterSuiteRun, prefix string) error {
	key := prefix + "twin/_"
	heldBefore := []int64{}
	for _, hook := range run.hooks {
		state, err := hook.state(ctx)
		if err != nil {
			return err
		}
		heldBefore = append(heldBefore, state.HeldTotal)
	}
	etag := ""
	for round := range run.settings.TwinRoundCount {
		body := []byte(fmt.Sprintf("twin %d", round))
		cluster := run.hostClusters[round%len(run.hostClusters)]
		var err error
		if round == 0 {
			etag, err = cluster.PutIfNoneMatch(ctx, key, body, nil)
		} else {
			etag, err = cluster.PutIfMatch(ctx, key, etag, body, nil)
		}
		if err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		object, err := run.hostClusters[(round+1)%len(run.hostClusters)].Get(ctx, key)
		if err != nil {
			return fmt.Errorf("round %d's read: %w", round, err)
		}
		read, err := io.ReadAll(object.Body)
		object.Body.Close()
		if err != nil || string(read) != string(body) {
			return fmt.Errorf("round %d reads %q, %v", round, read, err)
		}
	}
	if err := run.quiesce(ctx); err != nil {
		return err
	}
	for index, hook := range run.hooks {
		state, err := hook.state(ctx)
		if err != nil {
			return err
		}
		if state.HeldTotal != heldBefore[index] {
			return fmt.Errorf("host %d held a key %d times with no barrier", index, state.HeldTotal-heldBefore[index])
		}
	}
	return nil
}

// FS-114's head case on one host: a head If-Match PUT's commits held after their checks on one host past the drive
// timeout, the request cancelled. The write already holds a write quorum, so a newer PUT and a GET go through at
// once, and once the held commits are released the newer head stays, read through every host.
func fileClusterSuiteHeadOneHostHeld(ctx context.Context, run *fileClusterSuiteRun, prefix string) error {
	key := prefix + "head/_"
	last := len(run.hostClusters) - 1
	oldEtag, err := run.hostClusters[0].PutIfNoneMatch(ctx, key, []byte("old head"), nil)
	if err != nil {
		return err
	}
	if err := run.quiesce(ctx); err != nil {
		return err
	}
	held := run.hooks[last]
	if err := held.arm(ctx, FileClusterSuiteCommitChecked, key, 0); err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			held.release(context.WithoutCancel(ctx), FileClusterSuiteCommitChecked)
		}
	}()
	put := run.start(ctx, run.settings.HeldRequestDeadline, func(writeCtx context.Context) error {
		_, err := run.hostClusters[0].PutIfMatch(writeCtx, key, oldEtag, []byte("held write"), nil)
		return err
	})
	if err := held.wait(ctx, FileClusterSuiteCommitChecked, run.target.DrivesPerHost); err != nil {
		return err
	}
	put.cancel()
	if err := run.pastDriveTimeout(ctx); err != nil {
		return err
	}
	if err := run.expectBody(ctx, key, []byte("held write")); err != nil {
		return fmt.Errorf("the write with a quorum: %w", err)
	}
	etag, err := run.etagOf(ctx, 1, key)
	if err != nil {
		return err
	}
	started := time.Now()
	newerCtx := sidecar.WithBlobDeadline(ctx, time.Now().Add(run.settings.ShortRequestDeadline))
	if _, err := run.hostClusters[1].PutIfMatch(newerCtx, key, etag, []byte("newer head"), nil); err != nil {
		return fmt.Errorf("the newer PUT: %w", err)
	}
	if err := run.expectBody(ctx, key, []byte("newer head")); err != nil {
		return err
	}
	if elapsed := time.Since(started); run.settings.ShortRequestDeadline+run.settings.RoundTripSlack < elapsed {
		return fmt.Errorf("the newer PUT and its reads took %s", elapsed)
	}
	released = true
	if err := held.release(ctx, FileClusterSuiteCommitChecked); err != nil {
		return err
	}
	if err := run.quiesce(ctx); err != nil {
		return err
	}
	return run.expectBody(ctx, key, []byte("newer head"))
}

// FS-114's head case on two hosts: the write has no quorum and could still reach a read quorum, so its key is held:
// a newer PUT and a GET fail 503 until the held commits are released and the settle reverts them, the write never
// read.
func fileClusterSuiteHeadTwoHostsHeld(ctx context.Context, run *fileClusterSuiteRun, prefix string) error {
	key := prefix + "head/_"
	count := len(run.hostClusters)
	oldEtag, err := run.hostClusters[0].PutIfNoneMatch(ctx, key, []byte("old head"), nil)
	if err != nil {
		return err
	}
	if err := run.quiesce(ctx); err != nil {
		return err
	}
	heldHooks := []*fileClusterSuiteHook{run.hooks[count-2], run.hooks[count-1]}
	for _, hook := range heldHooks {
		if err := hook.arm(ctx, FileClusterSuiteCommitChecked, key, 0); err != nil {
			return err
		}
	}
	released := false
	release := func(ctx context.Context) error {
		released = true
		for _, hook := range heldHooks {
			if err := hook.release(ctx, FileClusterSuiteCommitChecked); err != nil {
				return err
			}
		}
		return nil
	}
	defer func() {
		if !released {
			release(context.WithoutCancel(ctx))
		}
	}()
	put := run.start(ctx, run.settings.ShortRequestDeadline, func(writeCtx context.Context) error {
		_, err := run.hostClusters[0].PutIfMatch(writeCtx, key, oldEtag, []byte("held write"), nil)
		return err
	})
	for _, hook := range heldHooks {
		if err := hook.wait(ctx, FileClusterSuiteCommitChecked, run.target.DrivesPerHost); err != nil {
			return err
		}
	}
	put.cancel()
	<-put.done
	if err := run.pastDriveTimeout(ctx); err != nil {
		return err
	}
	shortCtx := sidecar.WithBlobDeadline(ctx, time.Now().Add(run.settings.ShortRequestDeadline))
	var unknown *sidecar.BlobUnknownError
	if _, err := run.hostClusters[1].PutIfMatch(shortCtx, key, oldEtag, []byte("refused"), nil); !errors.As(err, &unknown) {
		return fmt.Errorf("a PUT on the held key: %v, want 503's unknown outcome", err)
	}
	if _, err := run.hostClusters[1].Get(sidecar.WithBlobDeadline(ctx, time.Now().Add(run.settings.ShortRequestDeadline)), key); err == nil {
		return errors.New("a GET on the held key was served")
	}
	if held, err := run.heldNow(ctx); err != nil || held != 1 {
		return fmt.Errorf("%d keys held, want the one: %v", held, err)
	}
	if err := release(ctx); err != nil {
		return err
	}
	if err := run.quiesce(ctx); err != nil {
		return err
	}
	if err := run.expectBody(ctx, key, []byte("old head")); err != nil {
		return fmt.Errorf("after the settle reverted the write: %w", err)
	}
	if _, err := run.hostClusters[1].PutIfMatch(ctx, key, oldEtag, []byte("newer head"), nil); err != nil {
		return fmt.Errorf("a PUT after the settle: %w", err)
	}
	return run.expectBody(ctx, key, []byte("newer head"))
}

// DESIGN-DETAILS 2.1's B1 on the cluster: two DELETEs of one key, each short of its quorum with its commits held on
// two hosts past its deadline. Each holds the key until its held commits are released and reverted, so the two
// failed DELETEs' leavings never add up to a read quorum: the object reads after each, through every host. The time
// from a DELETE's release to its key reading again is what the report gives as a failed DELETE's hold past its end.
func fileClusterSuiteTwoDeletesHeld(ctx context.Context, run *fileClusterSuiteRun, prefix string) error {
	key := prefix + "marker/_"
	count := len(run.hostClusters)
	body := []byte("the object both DELETEs fail to remove")
	if _, err := run.hostClusters[0].PutIfNoneMatch(ctx, key, body, nil); err != nil {
		return err
	}
	if err := run.quiesce(ctx); err != nil {
		return err
	}
	for round := range 2 {
		heldHooks := []*fileClusterSuiteHook{run.hooks[count-2], run.hooks[count-1]}
		for _, hook := range heldHooks {
			if err := hook.arm(ctx, FileClusterSuiteCommitChecked, key, run.target.DrivesPerHost); err != nil {
				return err
			}
		}
		del := run.start(ctx, run.settings.ShortRequestDeadline, func(deleteCtx context.Context) error {
			return run.hostClusters[round%2].Delete(deleteCtx, key)
		})
		for _, hook := range heldHooks {
			if err := hook.wait(ctx, FileClusterSuiteCommitChecked, run.target.DrivesPerHost); err != nil {
				return err
			}
		}
		deleteErr := <-del.done
		var unknown *sidecar.BlobUnknownError
		if !errors.As(deleteErr, &unknown) {
			return fmt.Errorf("round %d: a DELETE short of its quorum answered %v", round, deleteErr)
		}
		if err := run.pastDriveTimeout(ctx); err != nil {
			return err
		}
		if held, err := run.heldNow(ctx); err != nil || held != 1 {
			return fmt.Errorf("round %d: %d keys held with the DELETE's commits unanswered, want the one: %v", round, held, err)
		}
		for _, hook := range heldHooks {
			if err := hook.release(ctx, FileClusterSuiteCommitChecked); err != nil {
				return err
			}
		}
		released := time.Now()
		if err := run.waitFor(ctx, "the failed DELETE's key to read", func() bool {
			_, err := run.hostClusters[0].Head(sidecar.WithBlobDeadline(ctx, time.Now().Add(run.settings.ShortRequestDeadline)), key)
			return err == nil
		}); err != nil {
			return fmt.Errorf("round %d: %w", round, err)
		}
		if heldPastEnd := time.Since(released); run.report.DeleteHeldPastEnd < heldPastEnd {
			run.report.DeleteHeldPastEnd = heldPastEnd
		}
		if err := run.quiesce(ctx); err != nil {
			return err
		}
		if err := run.expectBody(ctx, key, body); err != nil {
			return fmt.Errorf("round %d: after the failed DELETE: %w", round, err)
		}
	}
	return nil
}

// DESIGN-DETAILS 2.1's C on the cluster: a create acknowledged with its last host's commits held before their check,
// then an acknowledged DELETE of the key; released, those commits find the drive's stamp moved and answer stale, so
// the key reads 404 through every host.
func fileClusterSuiteCreateAcrossDelete(ctx context.Context, run *fileClusterSuiteRun, prefix string) error {
	key := prefix + "created/_"
	last := len(run.hostClusters) - 1
	held := run.hooks[last]
	if err := held.arm(ctx, FileClusterSuiteCommitArrived, key, run.target.DrivesPerHost); err != nil {
		return err
	}
	released := false
	defer func() {
		if !released {
			held.release(context.WithoutCancel(ctx), FileClusterSuiteCommitArrived)
		}
	}()
	create := run.start(ctx, run.settings.HeldRequestDeadline, func(writeCtx context.Context) error {
		_, err := run.hostClusters[0].PutIfNoneMatch(writeCtx, key, []byte("created"), nil)
		return err
	})
	if err := held.wait(ctx, FileClusterSuiteCommitArrived, run.target.DrivesPerHost); err != nil {
		return err
	}
	if err := <-create.done; err != nil {
		return fmt.Errorf("the create with a quorum without its last host: %w", err)
	}
	if err := run.hostClusters[1].Delete(ctx, key); err != nil {
		return fmt.Errorf("the DELETE: %w", err)
	}
	released = true
	if err := held.release(ctx, FileClusterSuiteCommitArrived); err != nil {
		return err
	}
	if err := run.quiesce(ctx); err != nil {
		return err
	}
	// the held host's own drives answer too: a commit that landed there would be read through it
	return run.expectAbsent(ctx, key)
}

// The hosts stopped for a case, started again whatever the case found.
func (self *fileClusterSuiteRun) stopHosts(ctx context.Context, indexes ...int) (func() error, error) {
	stopped := []int{}
	restart := func() error {
		var first error
		for _, index := range stopped {
			if err := self.target.Hosts[index].Control.Start(context.WithoutCancel(ctx)); err != nil && first == nil {
				first = err
			}
		}
		if first != nil {
			return first
		}
		return self.waitFor(ctx, "every host to serve again", func() bool {
			for _, cluster := range self.hostClusters {
				if _, _, err := cluster.List(ctx, "filecluster-suite/", "", 1); err != nil {
					return false
				}
			}
			return true
		})
	}
	for _, index := range indexes {
		if err := self.target.Hosts[index].Control.Stop(ctx); err != nil {
			return restart, err
		}
		stopped = append(stopped, index)
	}
	return restart, nil
}

// §19.6's list row: a LIST is answered whole while a set has at most two drives offline, every acknowledged key
// listed, and 503 past that, never a listing that drops a key a read would return.
func fileClusterSuiteListWhole(ctx context.Context, run *fileClusterSuiteRun, prefix string) error {
	keys := []string{}
	for index := range run.settings.ListKeyCount {
		key := fmt.Sprintf("%slisted/%04d/_", prefix, index)
		if _, err := run.cluster.PutIfNoneMatch(ctx, key, []byte("listed"), nil); err != nil {
			return err
		}
		keys = append(keys, key)
	}
	listedThrough := func(index int) ([]string, error) {
		objects, _, err := run.hostClusters[index].List(ctx, prefix+"listed/", "", run.settings.ListKeyCount+1)
		if err != nil {
			return nil, err
		}
		listed := []string{}
		for _, object := range objects {
			listed = append(listed, object.Key)
		}
		return listed, nil
	}
	for stoppedCount := 1; stoppedCount <= run.settings.ListStoppedHosts && stoppedCount < len(run.target.Hosts); stoppedCount += 1 {
		indexes := []int{}
		for index := range stoppedCount {
			indexes = append(indexes, len(run.target.Hosts)-1-index)
		}
		restart, err := run.stopHosts(ctx, indexes...)
		if err != nil {
			restart()
			return err
		}
		offline := stoppedCount * run.target.DrivesPerHost
		var caseErr error
		err = run.waitFor(ctx, "the stopped hosts' drives to read offline", func() bool {
			listed, err := listedThrough(0)
			if offline <= run.settings.ListWholeOfflineDrives {
				caseErr = err
				if err != nil {
					return false
				}
				if !slices.Equal(listed, keys) {
					caseErr = fmt.Errorf("with %d drives offline the LIST named %d of %d keys", offline, len(listed), len(keys))
					return false
				}
				return true
			}
			// past the line: 503, never a short listing
			if err == nil && !slices.Equal(listed, keys) {
				caseErr = fmt.Errorf("with %d drives offline the LIST named %d of %d keys and did not answer 503", offline, len(listed), len(keys))
				return true
			}
			caseErr = nil
			return err != nil
		})
		if restartErr := restart(); restartErr != nil && err == nil {
			err = restartErr
		}
		if caseErr != nil {
			return caseErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// §11.3 with a host lost: a stale If-Match and a create on an existing key are each refused, and nothing is written.
func fileClusterSuiteStaleWithHostLost(ctx context.Context, run *fileClusterSuiteRun, prefix string) error {
	key := prefix + "head/_"
	etag, err := run.cluster.PutIfNoneMatch(ctx, key, []byte("v1"), nil)
	if err != nil {
		return err
	}
	if _, err := run.cluster.PutIfMatch(ctx, key, etag, []byte("v2"), nil); err != nil {
		return err
	}
	restart, err := run.stopHosts(ctx, len(run.target.Hosts)-1)
	if err != nil {
		restart()
		return err
	}
	caseErr := func() error {
		// the cluster takes writes again once it has seen the host go: until then a request may wait on the lost
		// host's connections and answer nothing definite, which says nothing of conditions
		attempt := 0
		if err := run.waitFor(ctx, "a write with the host lost", func() bool {
			attempt += 1
			_, err := run.hostClusters[0].PutIfNoneMatch(ctx, fmt.Sprintf("%sprobe/%d/_", prefix, attempt), []byte("probe"), nil)
			return err == nil
		}); err != nil {
			return err
		}
		// each refusal is definite: an unknown outcome is asked again, and a stale condition never takes
		refused := func(what string, write func() error) error {
			var last error
			if err := run.waitFor(ctx, what+" answered", func() bool {
				last = write()
				var unknown *sidecar.BlobUnknownError
				return !errors.As(last, &unknown)
			}); err != nil {
				return fmt.Errorf("%s: %w: %v", what, err, last)
			}
			if !errors.Is(last, sidecar.ErrBlobPrecondition) {
				return fmt.Errorf("%s with a host lost: %v", what, last)
			}
			return nil
		}
		if err := refused("a stale If-Match", func() error {
			_, err := run.hostClusters[0].PutIfMatch(ctx, key, etag, []byte("stale"), nil)
			return err
		}); err != nil {
			return err
		}
		if err := refused("a create on an existing key", func() error {
			_, err := run.hostClusters[0].PutIfNoneMatch(ctx, key, []byte("again"), nil)
			return err
		}); err != nil {
			return err
		}
		object, err := run.hostClusters[0].Get(ctx, key)
		if err != nil {
			return err
		}
		body, _ := io.ReadAll(object.Body)
		object.Body.Close()
		if string(body) != "v2" {
			return fmt.Errorf("the key reads %q after refused writes", body)
		}
		return nil
	}()
	if err := restart(); err != nil && caseErr == nil {
		caseErr = err
	}
	return caseErr
}

// The cluster through a path that sends each request's x-ur-deadline-ms and the named DELETE's header, and never
// cancels at the deadline, as a request the network held past its deadline reaches the store: what blobsuite's settle
// cases take as Settle.Delayed. It shares cluster's admin info, so its pool check reads what the cluster read.
func newFileClusterSuiteDelayedCluster(cluster *FileCluster) (*FileCluster, error) {
	config := cluster.config
	settings := cluster.clusters.settings
	hosts := []*fileClusterHost{}
	for _, endpoint := range config.Endpoints {
		base, err := newFileClusterBaseTransport(settings)(config.Tls)
		if err != nil {
			return nil, err
		}
		transport := &fileClusterSuiteDelayedTransport{base: base, clock: cluster.clusters.clock}
		core, err := minio.NewCore(endpoint, &minio.Options{
			Creds:        credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
			Secure:       config.Tls,
			Transport:    transport,
			Region:       config.Region,
			BucketLookup: minio.BucketLookupPath,
			MaxRetries:   1,
		})
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, &fileClusterHost{endpoint: endpoint, core: core, transport: transport})
	}
	delayed := &FileCluster{
		clusters: cluster.clusters,
		config:   config,
		picker:   &fileClusterHostPicker{hosts: hosts, holdoff: settings.HostFailureHoldoff, clock: cluster.clusters.clock},
	}
	cluster.stateLock.Lock()
	delayed.adminInfo = cluster.adminInfo
	delayed.sentinelTime = cluster.sentinelTime
	cluster.stateLock.Unlock()
	return delayed, nil
}

// A transport that adds the deadline header as fileClusterTransport does and never cancels at it.
type fileClusterSuiteDelayedTransport struct {
	base  http.RoundTripper
	clock sidecar.Clock
}

// The request with its headers, on a context its caller's cancel does not reach.
func (self *fileClusterSuiteDelayedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	requestTimeout, ok := request.Context().Value(fileClusterDeadlineKey{}).(time.Duration)
	if !ok {
		return self.base.RoundTrip(request)
	}
	deadline := sidecar.BlobRequestDeadline(request.Context(), self.clock.Now(), requestTimeout)
	request = request.Clone(context.WithoutCancel(request.Context()))
	request.Header.Set(fileClusterDeadlineHeader, strconv.FormatInt(deadline.UnixMilli(), 10))
	if request.Method == http.MethodDelete {
		if lastModified, ok := sidecar.BlobUnmodified(request.Context()); ok {
			request.Header.Set(fileClusterUnmodifiedHeader, strconv.FormatInt(lastModified.UnixMilli(), 10))
		}
	}
	return self.base.RoundTrip(request)
}

// A docker container as a host the suite stops and starts: T3's file-cluster containers (local/file-cluster-t3).
type FileClusterSuiteDockerHost struct {
	Container string
	// The docker binary, "docker" when empty.
	Docker string
}

// One docker command, its output in the error when it fails.
func (self *FileClusterSuiteDockerHost) docker(ctx context.Context, args ...string) error {
	docker := self.Docker
	if docker == "" {
		docker = "docker"
	}
	output, err := exec.CommandContext(ctx, docker, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Kills the container at once, as a host lost.
func (self *FileClusterSuiteDockerHost) Stop(ctx context.Context) error {
	return self.docker(ctx, "kill", self.Container)
}

// Starts the container again with its drives.
func (self *FileClusterSuiteDockerHost) Start(ctx context.Context) error {
	return self.docker(ctx, "start", self.Container)
}

// The clusters a vault file's text names (file-clusters-<service>.yml), checked as LoadFileClusters checks them: what
// filecluster-suite reads from a path the operator gives it, since the suite runs where no vault is mounted.
func ParseFileClusterConfigs(data []byte) ([]*FileClusterConfig, error) {
	y := &fileClustersYaml{}
	if err := yaml.Unmarshal(data, y); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFileClusterConfig, err)
	}
	return parseFileClusterConfigs(y)
}

// What filecluster-suite's hosts file says of each cluster beyond its vault file: the drives each host holds, the
// drive timeout it runs with, and, on T3, each host's barrier control port and container, in endpoint order.
//
//	clusters:
//	  meta-1:
//	    drives_per_host: 2
//	    drive_timeout: 30s
//	    hooks: [127.0.0.1:51151, ...]
//	    containers: [sidecar-test-fc-meta-1-a, ...]
type FileClusterSuiteHostsFile struct {
	Clusters map[string]*FileClusterSuiteHostsEntry `yaml:"clusters"`
}

// One cluster's entry in the hosts file.
type FileClusterSuiteHostsEntry struct {
	DrivesPerHost int      `yaml:"drives_per_host"`
	DriveTimeout  string   `yaml:"drive_timeout"`
	Hooks         []string `yaml:"hooks"`
	Containers    []string `yaml:"containers"`
}

// The hosts file's text.
func ParseFileClusterSuiteHostsFile(data []byte) (*FileClusterSuiteHostsFile, error) {
	hostsFile := &FileClusterSuiteHostsFile{}
	if err := yaml.Unmarshal(data, hostsFile); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrFileClusterConfig, err)
	}
	return hostsFile, nil
}

// The suite's target for config, from its hosts file entry: an entry's hooks and containers name every host or none.
func NewFileClusterSuiteTarget(config *FileClusterConfig, entry *FileClusterSuiteHostsEntry, settings *FileClusterSuiteSettings) (*FileClusterSuiteTarget, error) {
	if entry == nil || entry.DrivesPerHost <= 0 {
		return nil, fmt.Errorf("%w: %s: the hosts file gives no drives per host", ErrFileClusterConfig, config.ClusterId)
	}
	if (0 < len(entry.Hooks) && len(entry.Hooks) != len(config.Endpoints)) || (0 < len(entry.Containers) && len(entry.Containers) != len(config.Endpoints)) {
		return nil, fmt.Errorf("%w: %s: hooks and containers name each of its %d hosts or none", ErrFileClusterConfig, config.ClusterId, len(config.Endpoints))
	}
	if settings == nil {
		settings = DefaultFileClusterSuiteSettings()
	}
	if entry.DriveTimeout != "" {
		driveTimeout, err := time.ParseDuration(entry.DriveTimeout)
		if err != nil || driveTimeout <= 0 {
			return nil, fmt.Errorf("%w: %s: a drive timeout of %q", ErrFileClusterConfig, config.ClusterId, entry.DriveTimeout)
		}
		settings.DriveTimeout = driveTimeout
	}
	target := &FileClusterSuiteTarget{Config: config, DrivesPerHost: entry.DrivesPerHost, Settings: settings}
	for index, endpoint := range config.Endpoints {
		host := &FileClusterSuiteHost{Endpoint: endpoint}
		if 0 < len(entry.Hooks) {
			host.HookAddress = entry.Hooks[index]
		}
		if 0 < len(entry.Containers) {
			host.Control = &FileClusterSuiteDockerHost{Container: entry.Containers[index]}
		}
		target.Hosts = append(target.Hosts, host)
	}
	return target, nil
}
