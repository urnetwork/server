//go:build sidecar_localstack && sidecar_testhooks && !windows

// FS-5 on T3 with stock clients (§22's FS-5, FI-9; §22.9's git row; EVS1-M2-43): evsgit's world with its evs on
// server/cli/evs's block (evshost.NewHost, through sidecart3's evs tier) over a T3 Net's stores, the private Redis and
// the local stack's MinIO, its devices presenting service tokens server's oauth mints, and stock git as every device's
// client through the local tier. FS-5's clauses (sidecartest/evsgit/fs5.go) run on one Net, each on its own branch:
// the same schedules sidecartest's TestEvsGitFs5* tests run on T2. The Net's clock follows real time, since stock git,
// the devices and their queues run on it and evs holds a statement's time_ms to its own clock (5b). The stack must be
// up (sidecar/local/stack.sh), and this fails rather than skips without it.
//
// Two worlds share the Net, each served by a block of its own on the Net's stores, as an evs world's instances share
// one (fault's evs worlds), with the gates open to both networks. Four clauses run on one whose devices take FS-5's
// settings (evsgit.Fs5ClientSettings), by which the refusals and journal clauses count attempts. The push in parts runs
// on the other, whose devices ride out a loaded store (fs5T3Settings.PartsClient): each part's step 4 reads every one
// of its changes' path entries from MinIO at once, and so does evs's answer to the plan's read of its 1,100 paths.
// Under load the stack's MinIO resets connections, and evs answers each reset 503 refusing nothing (push_step4.go's
// unavailableOn; §11.8's retry id row), more times running than FS-5's two push retries 10 ms apart, or a device's
// three read retries, outlast. A burst of a thousand dials is reset on nearly every attempt while the load lasts, so
// that world's block also reaches the Net's stores through a bound on its requests in flight (fs5T3BoundedBlobs),
// which keeps its reads on kept connections. TestFs5OnT3PushInPartsRidesOutRefusedParts holds the 503s' cause to both
// settings deterministically.

package evshost_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
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
	"github.com/urnetwork/sidecar/sidecartest/evsgit"
	"github.com/urnetwork/sidecar/sidecartest/fault"

	"github.com/urnetwork/server/sidecart3"
)

// What a T3 git world is built with: the Net clock's step behind real time, and how long the block may take to read
// ready.
type fs5T3Settings struct {
	ClockFollowInterval time.Duration
	ReadyTimeout        time.Duration
	// Each block's engine settings: evs's, its batch flushed at once, an unknown push read again at once and no repo
	// cache, as T2's world runs its engine.
	Engine *evsservice.EvsSettings
	// The files of the clause's push in parts: past PartMaxEntryWriteCount, 1,024 entry writes.
	PartsFileCount int
	// The evs settings of the parts clause's devices: FS-5's, but with a push's requests asked again as often and as
	// far apart as a device's defaults ask them, 40 times, each after evs's own retry_after (1 s for a 503), a read
	// asked again as often as those (a device's 3 is not enough: the plan's read of 1,100 paths meets the same resets),
	// and the receive path waiting a device's 3 min for the commit to reach the local ref. So a store that answers a
	// read, a part, a seal or a lease 503 for a while, or a replay of 1,100 files slowed by load, is ridden out.
	PartsClient *sidecar.EvsSettings
	// The most store requests the block of a world that pushes in parts has in flight at once (fs5T3BoundedBlobs): 16,
	// the idle connections minio-go's transport keeps for a host, so the block's reads reuse kept connections.
	// Unbounded, a part's step 4 and the plan's read each dial up to a thousand at once, a burst the stack's MinIO,
	// behind Docker's port forward, resets under load on nearly every attempt, which no retry rides out.
	PartsStoreRequestCount int
}

func defaultFs5T3Settings() *fs5T3Settings {
	engine := evsservice.DefaultEvsSettings()
	engine.BatchFlushDelay = 0
	engine.PushUnknownRereadInterval = 0
	engine.RepoCacheTtl = 0
	deviceDefaults := sidecar.DefaultEvsSettings()
	partsClient := evsgit.Fs5ClientSettings()
	partsClient.PushRetryCount = deviceDefaults.PushRetryCount
	partsClient.PushRetryMaxWait = deviceDefaults.PushRetryMaxWait
	partsClient.ReadRetryCount = deviceDefaults.PushRetryCount
	partsClient.ReceiveWaitTimeout = deviceDefaults.ReceiveWaitTimeout
	return &fs5T3Settings{
		ClockFollowInterval:    10 * time.Millisecond,
		ReadyTimeout:           time.Minute,
		Engine:                 engine,
		PartsFileCount:         1100,
		PartsClient:            partsClient,
		PartsStoreRequestCount: 16,
	}
}

// Not a test: the hook the world's mounts exec relays to its device's daemon (evsgit.HookHelper).
func TestEvsGitHookHelperProcess(t *testing.T) {
	evsgit.HookHelper()
}

func TestFs5OnT3(t *testing.T) {
	settings := defaultFs5T3Settings()
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		net := newFs5T3Net(tb, settings)
		world := net.newWorld(tb, &fs5T3WorldArgs{Client: evsgit.Fs5ClientSettings()})
		evsgit.Fs5Siblings(tb, world)
		evsgit.Fs5RefusalsAreNoNonFastForward(tb, world)
		partsWorld := net.newWorld(tb, &fs5T3WorldArgs{Client: settings.PartsClient, StoreRequestCount: settings.PartsStoreRequestCount})
		evsgit.Fs5PushInParts(tb, partsWorld, settings.PartsFileCount)
		evsgit.Fs5AtomicSecondRefConflicts(tb, world)
		evsgit.Fs5LostAnswerReconciles(tb, world)
	})
}

// What made TestFs5OnT3 fail under load, held to both settings: evs answers each part's first three posts 503
// UNAVAILABLE, refusing nothing, as it answers a part whose step 4 read met a reset of the stack's MinIO. A twin whose
// devices take the parts clause's settings but FS-5's retries of a push's requests, three attempts to a part, runs out
// on its parts and git hears busy; on the parts clause's own settings the clause lands, each part taken at its fourth
// post. The two differ in those retries alone, so a read the store refuses under load ends neither push early.
func TestFs5OnT3PushInPartsRidesOutRefusedParts(t *testing.T) {
	settings := defaultFs5T3Settings()
	fs5Settings := evsgit.Fs5ClientSettings()
	twinSettings := *settings.PartsClient
	twinSettings.PushRetryCount = fs5Settings.PushRetryCount
	twinSettings.PushRetryMaxWait = fs5Settings.PushRetryMaxWait
	// every attempt FS-5's settings give one request
	refusedCount := twinSettings.PushRetryCount + 1
	fsCommitTestEnv().Run(t, func(tb testing.TB) {
		net := newFs5T3Net(tb, settings)

		twinRefusals := newFs5T3PartRefusals(refusedCount, settings.Engine.UnavailableRetryAfter)
		twin := net.newWorld(tb, &fs5T3WorldArgs{
			Client: &twinSettings, StoreRequestCount: settings.PartsStoreRequestCount, WrapHandler: twinRefusals.wrap,
		}).Device("fs5-parts-twin", nil)
		const branch = "fs5-parts-twin"
		dir := twin.Clone()
		first := twin.Commit(dir, "first", map[string]*string{"first.txt": evsgit.Text("first\n")})
		if out, ok := twin.Push(dir, "origin", "HEAD:refs/heads/"+branch); !ok {
			tb.Fatalf("the twin's first push: %s", out)
		}
		files := map[string]*string{}
		for i := range settings.PartsFileCount {
			files[fmt.Sprintf("many/%02d/%04d.txt", i%32, i)] = evsgit.Text(fmt.Sprintf("file %d\n", i))
		}
		twin.Commit(dir, "many files", files)
		out, ok := twin.Push(dir, "origin", "HEAD:refs/heads/"+branch)
		if ok || !strings.Contains(out, "(busy)") {
			tb.Fatalf("a push in parts on FS-5's retries, each part refused %d times: %v %s", refusedCount, ok, out)
		}
		if twin.LocalRef("refs/heads/"+branch) != first {
			tb.Fatalf("the twin's refused push in parts moved its branch to %q from %s", twin.LocalRef("refs/heads/"+branch), first)
		}
		// the push ended on a part that ran out of attempts
		twinRefused := twinRefusals.refusedCounts()
		ranOut := false
		for _, count := range twinRefused {
			ranOut = ranOut || count == refusedCount
		}
		if !ranOut {
			tb.Fatalf("no part of the twin's push was refused on all %d attempts: %v", refusedCount, twinRefused)
		}

		partsRefusals := newFs5T3PartRefusals(refusedCount, settings.Engine.UnavailableRetryAfter)
		partsWorld := net.newWorld(tb, &fs5T3WorldArgs{
			Client: settings.PartsClient, StoreRequestCount: settings.PartsStoreRequestCount, WrapHandler: partsRefusals.wrap,
		})
		evsgit.Fs5PushInParts(tb, partsWorld, settings.PartsFileCount)
		partsRefused := partsRefusals.refusedCounts()
		if len(partsRefused) < 2 {
			tb.Fatalf("the push in parts posted %d parts: %v", len(partsRefused), partsRefused)
		}
		for path, count := range partsRefused {
			if count != refusedCount {
				tb.Fatalf("the part at %s was refused %d times, not %d: %v", path, count, refusedCount, partsRefused)
			}
		}
	})
}

// A part's route, POST .../push/<push_id>/<k>, its push id captured.
var fs5T3PartPath = regexp.MustCompile(`/push/([0-9a-f]{32})/[0-9]+$`)

// A block's handler wrapped so that each part's first refusedCount posts are answered 503 UNAVAILABLE, refusing
// nothing, as evs answers a part whose step 4 read the store did not answer (push_step4.go's unavailableOn): the
// part's push id, its code, and the Retry-After evs gives a store that did not answer. Every later post, and every
// other request, reaches the block. Safe for concurrent use.
type fs5T3PartRefusals struct {
	refusedCount int
	retryAfter   time.Duration

	stateLock sync.Mutex
	// the posts refused, by part path
	pathRefusedCounts map[string]int
}

// Refuses each part's first refusedCount posts, each answer naming retryAfter.
func newFs5T3PartRefusals(refusedCount int, retryAfter time.Duration) *fs5T3PartRefusals {
	return &fs5T3PartRefusals{refusedCount: refusedCount, retryAfter: retryAfter, pathRefusedCounts: map[string]int{}}
}

// handler with each part's first posts refused: see the type.
func (self *fs5T3PartRefusals) wrap(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		match := fs5T3PartPath.FindStringSubmatch(request.URL.Path)
		if request.Method != http.MethodPost || match == nil || !self.refuse(request.URL.Path) {
			handler.ServeHTTP(writer, request)
			return
		}
		// evs reads a part's body before its step 4
		io.Copy(io.Discard, request.Body)
		pushId, err := hex.DecodeString(match[1])
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(&protocol.EvsRefusalReply{
			PushId: pushId,
			Code:   protocol.EvsRefusalCode_EVS_REFUSAL_UNAVAILABLE,
		})
		if err != nil {
			http.Error(writer, err.Error(), http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/x-protobuf")
		writer.Header().Set("Retry-After", strconv.Itoa(max(1, int(math.Ceil(self.retryAfter.Seconds())))))
		writer.WriteHeader(http.StatusServiceUnavailable)
		writer.Write(body)
	})
}

// Whether the post to path is one of its first refusedCount, which it counts.
func (self *fs5T3PartRefusals) refuse(path string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.refusedCount <= self.pathRefusedCounts[path] {
		return false
	}
	self.pathRefusedCounts[path] += 1
	return true
}

// The posts refused so far, by part path.
func (self *fs5T3PartRefusals) refusedCounts() map[string]int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return maps.Clone(self.pathRefusedCounts)
}

// A T3 Net whose evs tier serves a block for each git world built on it: see the file header. Safe for concurrent use.
type fs5T3Net struct {
	net      *sidecartest.Net
	tier     *fault.EvsTier
	settings *fs5T3Settings

	stateLock sync.Mutex
	// the network of each world whose block has started, which the gates open evs to
	networks []string
}

// A Net on real time with no world yet: see the file header.
func newFs5T3Net(tb testing.TB, settings *fs5T3Settings) *fs5T3Net {
	tb.Helper()
	netSettings := sidecart3.DefaultSettings()
	netSettings.Net.ClockStart = time.Now()
	net := sidecart3.NewNet(tb, netSettings)
	tb.Cleanup(net.Close)
	fs5FollowRealTime(tb, net.Clock(), settings.ClockFollowInterval)
	tier := fault.EvsTierOf(net)
	if tier == nil || tier.NewHost == nil || tier.IssueToken == nil {
		tb.Fatal("a T3 Net with no evs tier")
	}
	return &fs5T3Net{net: net, tier: tier, settings: settings}
}

// How one git world on the Net is built.
type fs5T3WorldArgs struct {
	// Each device's evs settings.
	Client *sidecar.EvsSettings
	// The most store requests the world's block has in flight at once (fs5T3BoundedBlobs); non-positive leaves them
	// unbounded, as server's file clusters do.
	StoreRequestCount int
	// What the world reaches in place of the block's handler, given it; nil for the handler as it is.
	WrapHandler func(handler http.Handler) http.Handler
}

// A git world on the Net, served by a block of its own: see the file header.
func (self *fs5T3Net) newWorld(tb testing.TB, args *fs5T3WorldArgs) *evsgit.World {
	tb.Helper()
	binary, err := os.Executable()
	if err != nil {
		tb.Fatal(err)
	}
	worldSettings := evsgit.DefaultWorldSettings()
	worldSettings.HookCommand = []string{binary, "-test.run=^TestEvsGitHookHelperProcess$", "--"}
	worldSettings.ClientSettings = args.Client
	worldSettings.Evs = &evsgit.WorldEvs{
		NewHost: func(ctx context.Context, deps *evsgit.WorldEvsDeps) (*evsgit.WorldEvsHost, error) {
			fakes := self.net.Fakes()
			if err := fakes.Gates.Set(self.gatesWith(connect.Id(deps.NetworkId))); err != nil {
				return nil, err
			}
			fakes.Quota.SetBalance(connect.Id(deps.NetworkId), 1024*1024*1024*1024)
			// the block's own view of the Net's host, its stores bounded where the args bound them
			host := *self.net.Host()
			if 0 < args.StoreRequestCount {
				host.Blobs = newFs5T3BoundedBlobs(host.Blobs, args.StoreRequestCount)
			}
			tierHost, err := self.tier.NewHost(ctx, &host, self.settings.Engine, &fault.EvsTierDeps{
				AuthState: deps.AuthState, GateViews: deps.GateViews, ReceiptKeys: deps.ReceiptKeys,
			})
			if err != nil {
				return nil, err
			}
			tb.Cleanup(tierHost.Close)
			if err := fs5AwaitReady(tierHost, self.settings.ReadyTimeout); err != nil {
				return nil, err
			}
			cluster, err := host.Blobs.FirstCluster()
			if err != nil {
				return nil, err
			}
			handler := tierHost.Handler()
			if args.WrapHandler != nil {
				handler = args.WrapHandler(handler)
			}
			return &evsgit.WorldEvsHost{Handler: handler, Engine: tierHost.Engine(), Blobs: host.Blobs, Cluster: cluster}, nil
		},
		IssueToken: self.tier.IssueToken,
	}
	return evsgit.NewWorld(tb, worldSettings)
}

// The Net's stores as one block reaches them, at most requestCount requests in flight at once, each waiting under its
// ctx for a slot it holds until its call returns (an object's body is read after): the cap on connections a host that
// server's file-cluster transport does not set (raised as question fs5-on-t3-flake-evs-step4-read-fanout). A part's
// step 4 and a read's round 1 each read every entry they name at once, and minio-go's transport keeps only 16 idle
// connections a host, so unbounded they dial a burst of up to a thousand. Nothing else changes: each request goes to
// the Net's stores, their fault plans included, as it was made. Safe for concurrent use.
type fs5T3BoundedBlobs struct {
	store sidecar.BlobStore
	slots chan struct{}
}

// store with at most requestCount requests in flight.
func newFs5T3BoundedBlobs(store sidecar.BlobStore, requestCount int) *fs5T3BoundedBlobs {
	return &fs5T3BoundedBlobs{store: store, slots: make(chan struct{}, requestCount)}
}

// cluster on the store's slots, or err.
func (self *fs5T3BoundedBlobs) bound(cluster sidecar.BlobCluster, err error) (sidecar.BlobCluster, error) {
	if err != nil {
		return nil, err
	}
	return &fs5T3BoundedCluster{cluster: cluster, slots: self.slots}, nil
}

func (self *fs5T3BoundedBlobs) Cluster(clusterId string) (sidecar.BlobCluster, error) {
	return self.bound(self.store.Cluster(clusterId))
}

func (self *fs5T3BoundedBlobs) FirstCluster() (sidecar.BlobCluster, error) {
	return self.bound(self.store.FirstCluster())
}

func (self *fs5T3BoundedBlobs) SegmentCluster() (sidecar.BlobCluster, error) {
	return self.bound(self.store.SegmentCluster())
}

func (self *fs5T3BoundedBlobs) MetadataClusterIds() []string {
	return self.store.MetadataClusterIds()
}

func (self *fs5T3BoundedBlobs) NewRepoClusterId() string {
	return self.store.NewRepoClusterId()
}

// One cluster of a fs5T3BoundedBlobs, sharing its slots. Safe for concurrent use.
type fs5T3BoundedCluster struct {
	cluster sidecar.BlobCluster
	slots   chan struct{}
}

// Runs call in a slot, waiting for one under ctx.
func (self *fs5T3BoundedCluster) inSlot(ctx context.Context, call func() error) error {
	select {
	case self.slots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() {
		<-self.slots
	}()
	return call()
}

func (self *fs5T3BoundedCluster) ClusterId() string {
	return self.cluster.ClusterId()
}

func (self *fs5T3BoundedCluster) RequestTimeout() time.Duration {
	return self.cluster.RequestTimeout()
}

func (self *fs5T3BoundedCluster) SettlePassed() bool {
	return self.cluster.SettlePassed()
}

func (self *fs5T3BoundedCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (newEtag string, err error) {
	err = self.inSlot(ctx, func() error {
		newEtag, err = self.cluster.PutIfNoneMatch(ctx, key, body, options)
		return err
	})
	return newEtag, err
}

func (self *fs5T3BoundedCluster) PutIfMatch(ctx context.Context, key string, etag string, body []byte, options *sidecar.BlobPutOptions) (newEtag string, err error) {
	err = self.inSlot(ctx, func() error {
		newEtag, err = self.cluster.PutIfMatch(ctx, key, etag, body, options)
		return err
	})
	return newEtag, err
}

func (self *fs5T3BoundedCluster) PutStream(ctx context.Context, key string, body io.Reader, byteCount int64, checksumSha256 []byte) (newEtag string, err error) {
	err = self.inSlot(ctx, func() error {
		newEtag, err = self.cluster.PutStream(ctx, key, body, byteCount, checksumSha256)
		return err
	})
	return newEtag, err
}

func (self *fs5T3BoundedCluster) Get(ctx context.Context, key string) (object *sidecar.BlobObject, err error) {
	err = self.inSlot(ctx, func() error {
		object, err = self.cluster.Get(ctx, key)
		return err
	})
	return object, err
}

func (self *fs5T3BoundedCluster) GetRange(ctx context.Context, key string, offset int64, byteCount int64) (object *sidecar.BlobObject, err error) {
	err = self.inSlot(ctx, func() error {
		object, err = self.cluster.GetRange(ctx, key, offset, byteCount)
		return err
	})
	return object, err
}

func (self *fs5T3BoundedCluster) Head(ctx context.Context, key string) (info *sidecar.BlobInfo, err error) {
	err = self.inSlot(ctx, func() error {
		info, err = self.cluster.Head(ctx, key)
		return err
	})
	return info, err
}

func (self *fs5T3BoundedCluster) List(ctx context.Context, keyPrefix string, startAfter string, limit int) (objects []*sidecar.BlobInfo, more bool, err error) {
	err = self.inSlot(ctx, func() error {
		objects, more, err = self.cluster.List(ctx, keyPrefix, startAfter, limit)
		return err
	})
	return objects, more, err
}

func (self *fs5T3BoundedCluster) Delete(ctx context.Context, key string) error {
	return self.inSlot(ctx, func() error {
		return self.cluster.Delete(ctx, key)
	})
}

// The Net's sidecar.yml once networkId's world starts: evs open to it and to every world's network before it, so a
// world's start closes no other's.
func (self *fs5T3Net) gatesWith(networkId connect.Id) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.networks = append(self.networks, networkId.String())
	quotedNetworks := []string{}
	for _, network := range self.networks {
		quotedNetworks = append(quotedNetworks, fmt.Sprintf("%q", network))
	}
	return fmt.Sprintf("sidecar:\n  evs: {enabled: true, networks: [%s]}\n", strings.Join(quotedNetworks, ", "))
}

// Keeps the Net's clock at real time until tb ends: see the file header.
func fs5FollowRealTime(tb testing.TB, clock *sidecartest.FakeClock, interval time.Duration) {
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if now := time.Now(); clock.Now().Before(now) {
					clock.AdvanceTo(now)
				}
			}
		}
	}()
	tb.Cleanup(func() {
		close(stop)
		<-stopped
	})
}

// The block's first probe answered and it reads ready, as warp sends a block nothing until its /status does (§19.4).
// Real time bounds the wait only to fail it.
func fs5AwaitReady(host fault.EvsTierHost, timeout time.Duration) error {
	guard := time.After(timeout)
	for {
		count, changed := host.Engine().Probes()
		if 0 < count && host.Ready() {
			return nil
		}
		select {
		case <-changed:
		case <-guard:
			return errors.New("the evs block never read ready")
		}
	}
}
