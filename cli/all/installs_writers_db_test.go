//go:build sidecar_pending

package main

// all's process runs api and taskworker together, and each installs its own first metadata cluster's writers (§19.4's
// writers that share a process; owner decisions 785, 788, amended by QD-1420), each block on its own MinIO user over
// the one bucket: api's parts, as all's main installs them (operatorInstallParts), put in api's signing rev/ store,
// api's hold/ writer and api's sequencer under api's log settings, on api's user, which reads and writes proj/,
// projrows/, rev/ and hold/ and deletes nothing (allWritersApiBlobs, as xops' policy-api.json.j2 renders §19.4's api
// row); and taskworker's runtime, beside evs's jobs, its evs-job writers (taskworker.Testing_InstallEvsJobWriters), on
// taskworker's user, which deletes: a rev/ sweep store, its hold/ writer and the worker's sequencer for retention,
// effects and the tick tasks. Here both run in one process over one fake first metadata cluster, api's on all's
// process host and api's budget, taskworker's on a fake budget of its own, against the database and Redis with MG1 to
// MG9, each statement signed on the clock both sequencers read:
//
//	rev/       a removal's revocation is published, signed, as its transaction commits, through api's store, and
//	           the sweep deletes it through taskworker's sweep store, which reads real time, as in production,
//	           spending the worker's budget at §11.13's delete row, api's user sent no DELETE
//	appends    a network's log, three bases with statements between them, appends through api's sequencer, whose
//	           entry cache INCRs {acl_<networkId>}v once an entry: the worker's settings write no cache
//	retention  a pass past the newest base's 30 days runs through the worker's sequencer: the entries before the
//	           base it retires to are gone, entry 0 and the rest stay, and every DELETE spends the worker's budget,
//	           P0 of kind log at §11.13's delete row, which api's MinIO user could not have sent
//	ticks      the worker's tick and activation tasks hand their runs to the issuer, and the issuer's handoffs append
//	           through api's sequencer rather than handing off again
//	holds      a hold the worker placed, past its expiry, ends on AbuseHoldPass through taskworker's hold/ writer, the
//	           current one, its object's DELETE spending the worker's budget at the delete row, api's user sent no
//	           DELETE and no hold/ PUT
//	removal    the worker's removal leaves api's store, writer and sequencer in place
//
// A second test installs taskworker's writers first and api's parts after them, as when api's mount comes later than
// taskworker's start (its switch retrying a build that failed), and the sweep and the hold pass still delete through
// taskworker's. Before the places, the worker's install replaced api's in each one-pointer slot: the revocation stayed
// owed and unpublished, api's appends ran on the worker's sequencer without the entry cache, and each handoff handed
// off again. Before QD-1420 the sweep read api's store, and api's hold/ writer, installed later, became the pass's, so
// both DELETEs went to api's user, which refuses them: the sweep failed every pass and ended holds kept their objects.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/taskworker"
	"github.com/urnetwork/server/taskworker/work"
)

// api's sequencer as the admission world's api: its three appends and its read, each refusal as the world reads one,
// and no unverified append.
type allWritersAdmissionApi struct {
	sequencer *model.ConfigLogSequencer
}

func allWritersRefusal(err error) error {
	configLogError := &model.ConfigLogError{}
	if errors.As(err, &configLogError) {
		return &sidecartest.ConfigLogRefusal{Status: configLogError.Status, Tip: configLogError.Tip, Err: configLogError.Err}
	}
	return err
}

func (self allWritersAdmissionApi) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
	return pin, allWritersRefusal(err)
}

func (self allWritersAdmissionApi) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendBase(ctx, server.Id(networkId), &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	return pin, allWritersRefusal(err)
}

func (self allWritersAdmissionApi) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
	return pin, allWritersRefusal(err)
}

func (self allWritersAdmissionApi) ReadConfigProjection(ctx context.Context, networkId sidecar.Id, after uint64) (*sidecar.ConfigProjection, error) {
	read, err := self.sequencer.Read(ctx, server.Id(networkId), after)
	if err != nil {
		return nil, allWritersRefusal(err)
	}
	return &sidecar.ConfigProjection{SignedGenesis: read.SignedGenesis, Entries: read.Entries, Rows: read.Rows, Tip: read.Tip, More: read.More}, nil
}

func (self allWritersAdmissionApi) SequenceAnyway(ctx context.Context, networkId sidecar.Id, kind uint32, body []byte) (*bridge.AuthPin, error) {
	return nil, errors.New("api's sequencer appends nothing its check refuses")
}

// What the store answers a request its user's policy does not allow.
var errAllWritersAccessDenied = errors.New("AccessDenied: access denied")

// The first metadata cluster as one block's MinIO user reaches it, over the bucket every block's user shares (§19.4's
// own credentials; xops' templates/file-clusters/policy-api.json.j2 and policy-taskworker.json.j2): api's user reads
// and writes proj/, projrows/, rev/ and hold/, lists under them and reads sentinel/_, and deletes nothing, while
// taskworker's reaches every object. The store answers a request the policy refuses with AccessDenied, which this
// reports as server's FileCluster reads it: a read or a LIST failed, a PUT or a DELETE an unknown outcome. Counts the
// PUTs and DELETEs the user sent.
type allWritersClusterUser struct {
	sidecar.BlobCluster
	// the prefixes the user reads, writes and lists under; nil for every key
	prefixes []string
	// whether the user deletes
	deletes bool

	stateLock sync.Mutex
	// PUTs the policy let through, by key prefix up to its first slash
	prefixPutCounts map[string]int
	// DELETEs the policy let through, and those it refused
	deleteCount        int
	refusedDeleteCount int
}

// api's user on cluster (§19.4's api row).
func newAllWritersApiUser(cluster sidecar.BlobCluster) *allWritersClusterUser {
	return &allWritersClusterUser{
		BlobCluster:     cluster,
		prefixes:        []string{"proj/", "projrows/", "rev/", "hold/"},
		prefixPutCounts: map[string]int{},
	}
}

// taskworker's user on cluster (§19.4's taskworker row).
func newAllWritersWorkerUser(cluster sidecar.BlobCluster) *allWritersClusterUser {
	return &allWritersClusterUser{BlobCluster: cluster, deletes: true, prefixPutCounts: map[string]int{}}
}

// Whether the policy lets the user write key, or list under it.
func (self *allWritersClusterUser) reaches(key string) bool {
	if self.prefixes == nil {
		return true
	}
	for _, prefix := range self.prefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Whether the policy lets the user read key.
func (self *allWritersClusterUser) reads(key string) bool {
	return self.reaches(key) || key == "sentinel/_"
}

// A PUT the policy let through, counted, or its refusal.
func (self *allWritersClusterUser) put(key string) error {
	if !self.reaches(key) {
		return &sidecar.BlobUnknownError{Err: errAllWritersAccessDenied}
	}
	prefix, _, _ := strings.Cut(key, "/")
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.prefixPutCounts[prefix+"/"] += 1
	}()
	return nil
}

func (self *allWritersClusterUser) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if err := self.put(key); err != nil {
		return "", err
	}
	return self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
}

func (self *allWritersClusterUser) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	if err := self.put(key); err != nil {
		return "", err
	}
	return self.BlobCluster.PutIfMatch(ctx, key, etag, body, options)
}

func (self *allWritersClusterUser) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (string, error) {
	if err := self.put(key); err != nil {
		return "", err
	}
	return self.BlobCluster.PutStream(ctx, key, body, byteCount, checksumSha256)
}

func (self *allWritersClusterUser) Get(ctx context.Context, key string) (*sidecar.BlobObject, error) {
	if !self.reads(key) {
		return nil, fmt.Errorf("blob read failed: %w", errAllWritersAccessDenied)
	}
	return self.BlobCluster.Get(ctx, key)
}

func (self *allWritersClusterUser) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (*sidecar.BlobObject, error) {
	if !self.reads(key) {
		return nil, fmt.Errorf("blob read failed: %w", errAllWritersAccessDenied)
	}
	return self.BlobCluster.GetRange(ctx, key, offset, byteCount)
}

func (self *allWritersClusterUser) Head(ctx context.Context, key string) (*sidecar.BlobInfo, error) {
	if !self.reads(key) {
		return nil, fmt.Errorf("blob read failed: %w", errAllWritersAccessDenied)
	}
	return self.BlobCluster.Head(ctx, key)
}

func (self *allWritersClusterUser) List(ctx context.Context, keyPrefix string, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	if !self.reaches(keyPrefix) {
		return nil, false, fmt.Errorf("blob list failed: %w", errAllWritersAccessDenied)
	}
	return self.BlobCluster.List(ctx, keyPrefix, startAfter, limit)
}

func (self *allWritersClusterUser) Delete(ctx context.Context, key string) error {
	refused := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.deletes {
			self.refusedDeleteCount += 1
			return true
		}
		self.deleteCount += 1
		return false
	}()
	if refused {
		return &sidecar.BlobUnknownError{Err: errAllWritersAccessDenied}
	}
	return self.BlobCluster.Delete(ctx, key)
}

// The DELETEs the user sent: those the policy let through, and those it refused.
func (self *allWritersClusterUser) deleteCounts() (deleted int, refused int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.deleteCount, self.refusedDeleteCount
}

// The PUTs the policy let through under prefix, such as "hold/".
func (self *allWritersClusterUser) putCount(prefix string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.prefixPutCounts[prefix]
}

// The clusters api's block reaches through file-clusters-api.yml (§19.4's api row): the first metadata cluster alone,
// as api's MinIO user.
type allWritersApiBlobs struct {
	first *allWritersClusterUser
}

func (self *allWritersApiBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	if clusterId != self.first.ClusterId() {
		return nil, fmt.Errorf("%w: %s", sidecar.ErrBlobClusterUnknown, clusterId)
	}
	return self.first, nil
}

func (self *allWritersApiBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	return self.first, nil
}

func (self *allWritersApiBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return nil, fmt.Errorf("%w: no segment cluster", sidecar.ErrBlobClusterUnknown)
}

func (self *allWritersApiBlobs) MetadataClusterIds() []string {
	return []string{self.first.ClusterId()}
}

func (self *allWritersApiBlobs) NewRepoClusterId() string {
	return self.first.ClusterId()
}

// What both tests run on: the issuer's service key and the verifier of its rev/ objects, the sidecar limits off, one
// fake first metadata cluster on a clock an hour before real time, so what the worker's sweep, which reads real time,
// finds is past its 20 minutes, api's user and the worker's on it, the worker's budget, holds turned on, and the hold
// pass's sweeps queued in memory.
type allWritersWorld struct {
	tb           testing.TB
	ctx          context.Context
	clock        *sidecartest.FakeClock
	blobs        *sidecartest.FakeBlobs
	first        sidecar.BlobCluster
	apiUser      *allWritersClusterUser
	workerUser   *allWritersClusterUser
	workerBudget *sidecartest.FakeWriteBudget
	verifier     *oauth.ServiceTokenVerifier
	deletePrice  int64
	closes       []func()
}

func newAllWritersWorld(tb testing.TB, ctx context.Context) *allWritersWorld {
	tb.Helper()
	self := &allWritersWorld{tb: tb, ctx: ctx}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		tb.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
	if err != nil {
		tb.Fatal(err)
	}
	self.closes = append(self.closes, oauth.Testing_SetSignerKeys(&oauth.SignerKey{
		Kid: oauth.SignerKeySetService.KidPrefix() + thumbprint, Alg: "ES256", Set: oauth.SignerKeySetService, PrivateKey: privateKey,
	}))
	self.verifier, err = oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: "https://issuer.allwriters.example", Keys: oauth.PublicJwks(oauth.SignerKeySetService).Keys,
	})
	if err != nil {
		tb.Fatal(err)
	}
	// the world's three bases come within a minute of each other, past pro.yml's one a minute
	for _, tier := range []model.NetworkTier{model.NetworkTierFree, model.NetworkTierPro, model.NetworkTierTeam} {
		self.closes = append(self.closes, model.Testing_SetSidecarLimits(tier, map[string]int64{
			"config_statements_per_min": -1,
			"config_statements_per_day": -1,
			"config_bases_per_min":      -1,
			"config_bases_per_day":      -1,
		}))
	}
	self.closes = append(self.closes, server.Testing_PushAbuseConfig("holds_enabled: true\n"))

	// one first metadata cluster in place of vault's file-clusters-api.yml and file-clusters-taskworker.yml
	self.clock = sidecartest.NewFakeClock(server.NowUtc().Add(-time.Hour))
	self.blobs = sidecartest.NewFakeBlobs(self.clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	self.first, err = self.blobs.FirstCluster()
	if err != nil {
		tb.Fatal(err)
	}
	self.apiUser = newAllWritersApiUser(self.first)
	self.workerUser = newAllWritersWorkerUser(self.first)
	self.workerBudget = sidecartest.NewFakeWriteBudget(nil)
	self.closes = append(self.closes, work.Testing_SetAbuseHoldSweepTasks(sidecartest.NewFakeTasks(self.clock, self.blobs.Faults(), nil)))
	self.deletePrice, err = evsservice.DefaultWriteBudgetSettings().PriceTable.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationDelete, Count: 1})
	if err != nil {
		tb.Fatal(err)
	}
	model.SetSidecarRevStore(nil)
	model.SetConfigLogSequencer(nil)
	model.SetAbuseHoldStore(nil)
	return self
}

// Puts back what the world set.
func (self *allWritersWorld) close() {
	for _, closeOne := range self.closes {
		defer closeOne()
	}
}

// api's parts, as all's main installs them, over the host it builds, on api's user. Answers what removes them.
func (self *allWritersWorld) installApiParts() func() {
	processHost := newProcessHost(self.ctx)
	processHost.Clock = self.clock
	apiBlobs := &allWritersApiBlobs{first: self.apiUser}
	return controller.InstallProcessParts(self.ctx, operatorInstallParts(processHost, commandOptions{}, &controller.ApiRoutesInstallDeps{
		Clusters: func(ctx context.Context) (sidecar.BlobStore, func(), error) {
			return apiBlobs, func() {}, nil
		},
	}))
}

// taskworker's writers, as its runtime installs them beside evs's jobs, on the worker's user and budget. Answers
// what removes them.
func (self *allWritersWorld) installWorkerWriters() func() {
	return taskworker.Testing_InstallEvsJobWriters(&sidecar.ServiceHost{Blobs: self.blobs, Budget: self.workerBudget, Clock: self.clock}, self.workerUser)
}

// What the worker's budget spent of kind.
func (self *allWritersWorld) workerSpent(kind sidecar.WriteP0Kind) int64 {
	if violations := self.workerBudget.Violations(); len(violations) != 0 {
		self.tb.Errorf("the worker's budget's violations: %v", violations)
	}
	return self.workerBudget.P0ByteCount(self.first.ClusterId(), kind)
}

// Fails the test if api's user was sent a DELETE, which it refuses.
func (self *allWritersWorld) requireApiSentNoDelete(what string) {
	self.tb.Helper()
	if deleted, refused := self.apiUser.deleteCounts(); deleted != 0 || refused != 0 {
		self.tb.Fatalf("%s: api's user was sent %d DELETEs, %d of them refused", what, deleted+refused, refused)
	}
}

// A removal's revocation, owed in its transaction, which publishes it as it commits.
func (self *allWritersWorld) oweRevocation() *model.SidecarRevocation {
	revocation := &model.SidecarRevocation{
		NetworkId: server.NewId(),
		Kind:      sidecar.RevocationKindClient,
		SubjectId: server.NewId(),
		NotBefore: sidecar.RevocationNotBefore(self.clock.Now()),
		Reason:    sidecar.RevocationReasonRemoved,
	}
	server.Tx(self.ctx, func(tx server.PgTx) {
		model.WriteRevocation(self.ctx, tx, revocation)
	})
	return revocation
}

// Whether revocation's object stands, signed by the issuer.
func (self *allWritersWorld) published(revocation *model.SidecarRevocation) bool {
	record, err := sidecar.ReadRevocation(self.ctx, self.first, self.verifier.VerifyRevocation, sidecar.Id(revocation.NetworkId), revocation.Kind, sidecar.Id(revocation.SubjectId))
	if err != nil {
		self.tb.Fatal(err)
	}
	return record != nil && record.NotBefore.Equal(revocation.NotBefore)
}

// rev/: a removal's revocation is published through api's store, and the sweep deletes it through the worker's,
// spending the worker's budget at §11.13's delete row and sending api's user no DELETE.
func (self *allWritersWorld) requireRevThroughEachRole() {
	self.tb.Helper()
	revocation := self.oweRevocation()
	if !self.published(revocation) {
		self.tb.Fatal("beside the worker's writers the committed removal published no signed revocation")
	}
	if self.apiUser.putCount("rev/") == 0 || self.workerUser.putCount("rev/") != 0 {
		self.tb.Fatalf("api's user sent %d rev/ PUTs and the worker's %d", self.apiUser.putCount("rev/"), self.workerUser.putCount("rev/"))
	}
	spentBefore := self.workerSpent(sidecar.WriteP0KindRevocation)
	if deletedCount, err := model.SweepSidecarRevocations(self.ctx); err != nil || deletedCount != 1 {
		self.tb.Fatalf("the sweep deleted %d: %v", deletedCount, err)
	}
	if spent := self.workerSpent(sidecar.WriteP0KindRevocation) - spentBefore; spent != self.deletePrice {
		self.tb.Errorf("the sweep spent %d of the worker's budget, and §11.13's delete row prices its DELETE at %d", spent, self.deletePrice)
	}
	self.requireApiSentNoDelete("the sweep")
}

// holds: a hold the worker placed 91 days ago, as its placement task places one, past its 90 days, ends on
// AbuseHoldPass through the worker's writer: its object's DELETE spends the worker's budget at §11.13's delete row,
// its rows go, and api's user is sent no DELETE and no hold/ PUT.
func (self *allWritersWorld) requireHoldPassThroughTheWorkers() {
	self.tb.Helper()
	networkId := server.NewId()
	model.Testing_CreateNetwork(self.ctx, networkId, "hold-"+networkId.String()[24:], server.NewId())
	var caseId server.Id
	var err error
	server.Tx(self.ctx, func(tx server.PgTx) {
		caseId, _, err = model.OpenAbuseCaseInTx(self.ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      model.AbuseCategoryOther,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: networkId.String()},
			ReporterClass: model.AbuseReporterClassLawEnforcement,
			JoinKind:      model.AbuseJoinKindNetwork,
			JoinKey:       networkId.String(),
		}, server.NowUtc(), model.DefaultAbuseSettings())
	})
	if err != nil {
		self.tb.Fatal(err)
	}
	placeTime := server.NowUtc().AddDate(0, 0, -(model.LegalHoldPreserveDays + 1))
	hold, inserted, err := model.PlaceLegalHoldWithClock(self.ctx, &model.LegalHoldArgs{
		NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f,
	}, func() time.Time { return placeTime }, nil)
	if err != nil || !inserted {
		self.tb.Fatalf("the worker's placement: %t, %v", inserted, err)
	}
	key := sidecar.HoldObjectKey(sidecar.Id(networkId), sidecar.Id(caseId))
	if _, err := self.first.Head(self.ctx, key); err != nil {
		self.tb.Fatalf("the placed hold has no object: %v", err)
	}
	if !server.NowUtc().After(hold.ExpireTime) {
		self.tb.Fatalf("the hold ends at %s, after now", hold.ExpireTime)
	}

	spentBefore := self.workerSpent(sidecar.WriteP0KindLifecycle)
	clientSession := session.NewLocalClientSession(self.ctx, "", nil)
	defer clientSession.Cancel()
	result, err := work.AbuseHoldPass(&work.AbuseHoldPassArgs{}, clientSession)
	if err != nil || result.EndedCount != 1 || result.FailedCount != 0 {
		self.tb.Fatalf("the hold pass: %+v, %v", result, err)
	}
	if _, err := self.first.Head(self.ctx, key); !errors.Is(err, sidecar.ErrBlobNotFound) {
		self.tb.Fatalf("the ended hold's object stands: %v", err)
	}
	if model.GetLegalHold(self.ctx, networkId, caseId) != nil {
		self.tb.Fatal("the ended hold's row stands")
	}
	if spent := self.workerSpent(sidecar.WriteP0KindLifecycle) - spentBefore; spent != self.deletePrice {
		self.tb.Errorf("the pass spent %d of the worker's budget, and §11.13's delete row prices its DELETE at %d", spent, self.deletePrice)
	}
	self.requireApiSentNoDelete("the hold pass")
	if puts := self.apiUser.putCount("hold/"); puts != 0 {
		self.tb.Fatalf("api's user was sent %d hold/ PUTs", puts)
	}
}

func TestAllRunsApisAndTheWorkersWritersSideBySide(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newAllWritersWorld(tb, ctx)
		defer world.close()
		clock := world.clock
		first := world.first

		// api's parts first, as all's main installs them before taskworker starts
		undoParts := world.installApiParts()
		defer undoParts()
		apiSequencer, err := model.CurrentConfigLogSequencer()
		if err != nil {
			tb.Fatalf("all's parts installed no sequencer of api's: %v", err)
		}
		removeWriters := world.installWorkerWriters()
		removed := false
		defer func() {
			if !removed {
				removeWriters()
			}
		}()

		world.requireRevThroughEachRole()
		// the rest at the time the appends always ran at, 21 minutes past real time
		clock.Advance(time.Hour + model.DefaultSidecarRevSettings().DeleteAfter + time.Minute)

		// appends: a network's log through api's sequencer, three bases with statements between them
		suiteSettings := sidecartest.DefaultAdmissionSuiteSettings()
		suiteSettings.ForgedAppends = false
		net := sidecartest.NewAdmissionNet(tb, &sidecartest.AdmissionTier{
			Cluster:  first,
			Settings: suiteSettings,
			Clock:    clock,
			NewApi: func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
				return allWritersAdmissionApi{sequencer: apiSequencer}
			},
		})
		defer net.Close()
		net.AdmitMember(net.Owner(), "first", bridge.MemberRole_MEMBER_ROLE_MEMBER)
		net.AdmitMember(net.Owner(), "second", bridge.MemberRole_MEMBER_ROLE_MEMBER)
		retiredTo := net.Fold(true).GetVersion()
		net.AdmitMember(net.Owner(), "third", bridge.MemberRole_MEMBER_ROLE_MEMBER)
		net.Fold(true)
		tip := net.Tip().GetVersion()
		networkId := net.NetworkId()
		var cacheVersion int64
		server.Redis(ctx, func(r server.RedisClient) {
			cacheVersion, err = r.Get(ctx, sidecar.AclVersionKey(networkId)).Int64()
		})
		if err != nil || cacheVersion != int64(tip) {
			tb.Fatalf("api's entry cache counted %d of the %d entries: %v", cacheVersion, tip, err)
		}
		if spent := world.workerSpent(sidecar.WriteP0KindLog); spent != 0 {
			tb.Fatalf("the worker's budget spent %d of the log before retention", spent)
		}

		// retention, past the newest base's 30 days, through the worker's sequencer
		if retiredTo <= 2 {
			tb.Fatalf("the second base at %d leaves retention nothing to delete", retiredTo)
		}
		retentionSettings := model.DefaultConfigLogRetentionSettings()
		retention, err := model.RetainConfigLogs(ctx, clock.Now().Add(retentionSettings.KeepAfterNewerBase+24*time.Hour), nil, retentionSettings)
		if err != nil || retention.NetworkCount != 1 || retention.DeletedCount < int(retiredTo)-1 {
			tb.Fatalf("retention: %+v, %v; it should delete entries 1 to %d", retention, err, retiredTo-1)
		}
		entryHeld := func(n uint64) bool {
			_, err := first.Head(ctx, sidecar.ConfigEntryKey(networkId, n))
			return err == nil
		}
		for n := uint64(0); n <= tip; n += 1 {
			if want := n == 0 || retiredTo <= n; entryHeld(n) != want {
				tb.Errorf("entry %d of %d held %t after retention to %d", n, tip, !want, retiredTo)
			}
		}
		if spent, want := world.workerSpent(sidecar.WriteP0KindLog), int64(retention.DeletedCount)*world.deletePrice; spent != want {
			tb.Errorf("retention's %d DELETEs spent %d of the worker's budget, and §11.13's delete row prices them at %d", retention.DeletedCount, spent, want)
		}
		world.requireApiSentNoDelete("retention")

		// ticks: the worker's tasks hand their runs to the issuer, whose handoffs append through api's sequencer
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		tickArgs := &model.AppendConfigLogTickArgs{NetworkId: server.Id(networkId)}
		if tick, err := model.AppendConfigLogTickTask(tickArgs, clientSession); err != nil || !tick.PublishAtIssuer {
			tb.Fatalf("the worker's tick task: %+v, %v", tick, err)
		}
		if tick, err := model.PublishConfigLogTickTask(tickArgs, clientSession); err != nil || tick.PublishAtIssuer {
			tb.Fatalf("the issuer's tick handoff: %+v, %v", tick, err)
		}
		activationArgs := &model.AppendConfigLogActivationArgs{NetworkId: server.Id(networkId), ProposalId: make([]byte, 32)}
		if activation, err := model.AppendConfigLogActivationTask(activationArgs, clientSession); err != nil || !activation.PublishAtIssuer {
			tb.Fatalf("the worker's activation task: %+v, %v", activation, err)
		}
		if activation, err := model.PublishConfigLogActivationTask(activationArgs, clientSession); err != nil || activation.PublishAtIssuer {
			tb.Fatalf("the issuer's activation handoff: %+v, %v", activation, err)
		}

		world.requireHoldPassThroughTheWorkers()

		// removal: the worker's writers' removal leaves api's store, writer and sequencer
		removeWriters()
		removed = true
		if sequencer, err := model.CurrentConfigLogSequencer(); err != nil || sequencer != apiSequencer {
			tb.Fatalf("the worker's removal took away api's sequencer: %v", err)
		}
		if _, err := model.CurrentAbuseHoldStore(); err != nil {
			tb.Fatalf("the worker's removal took away api's hold/ writer: %v", err)
		}
		if revocation := world.oweRevocation(); !world.published(revocation) {
			tb.Fatal("after the worker's removal the committed removal published no signed revocation")
		}
	})
}

// taskworker's writers first and api's parts after them, as when api's mount comes later than taskworker's start: every
// rev/ write still goes through api's signing store, and the sweep and the hold pass still delete through taskworker's
// sweep store and hold/ writer, api's, installed later, standing in for neither; api's removal leaves them.
func TestAllDeletesThroughTheWorkersWritersWhenApisMountComesLater(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		world := newAllWritersWorld(tb, ctx)
		defer world.close()

		removeWriters := world.installWorkerWriters()
		defer removeWriters()
		undoParts := world.installApiParts()
		undone := false
		defer func() {
			if !undone {
				undoParts()
			}
		}()
		if _, err := model.CurrentConfigLogSequencer(); err != nil {
			tb.Fatalf("api's later mount installed no sequencer of api's: %v", err)
		}

		world.requireRevThroughEachRole()
		world.requireHoldPassThroughTheWorkers()

		// api's removal leaves the worker's sweep store and hold/ writer; the object may stand again, since api's issuer
		// worker may have run the revocation's durable write task after the sweep, so the count is not held here
		undone = true
		undoParts()
		if _, err := model.CurrentAbuseHoldStore(); err != nil {
			tb.Fatalf("api's removal took away the worker's hold/ writer: %v", err)
		}
		if deletedCount, err := model.SweepSidecarRevocations(ctx); err != nil {
			tb.Fatalf("after api's removal the worker's sweep, %d deleted: %v", deletedCount, err)
		}
		world.requireApiSentNoDelete("the sweep after api's removal")
	})
}
