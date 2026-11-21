//go:build sidecar_e2e && sidecar_localstack

// Full sidecars pushing to and reading from evs at once (the end-to-end tests' groundwork, e2e-client-origins): a T3
// world (world_networks.go) with evs served at its origin (world_evs.go), and in it two networks, the first with
// FirstSidecarCount sidecars and the second with SecondSidecarCount, every one a sidecar.Sidecar enrolled through
// server's api from an auth key its owner made and Online on server's exchange. Each network's first sidecar bootstraps
// it and admits the rest, creates a general repo through evs and grants its member, so all its sidecars, writer; then
// every sidecar of both networks runs BurstCount bursts, one goroutine a burst, each writing its own path
// BurstWriteCount times in order with random pauses and reading another burst's path between its writes. Every
// sidecar runs at once; TestT3WorldEvsBursts runs each sidecar's bursts one after another on its evs client, and
// TestT3WorldEvsBurstsShareEachClient runs them all at once on it, as an app's writes run beside its sync's reads.
//
// What must hold, strictly:
//
//   - every write commits, once: in each network, every sidecar's own verified listing names exactly the network's
//     paths, and its verified History of each path lists exactly that path's writes, newest first, each at the push id,
//     path version and ref version its writer was answered, signed by its writer, with the content written; every
//     commit's ref version is distinct, and the ref's head counts them all;
//   - a burst's reads of a path never go back: each names one of that path's writes, never one older than the burst read
//     before (§11.9's pins, I-16), and none is refused;
//   - the second network sees none of the first's: its listing holds none of the first's paths, none of its sidecars
//     reads the first's repo through its client, the exchange grants none of them a scope on it, and evs refuses each
//     one's own token there with 403.
//
// Each phase has PhaseTimeout and dumps every burst's progress when it stalls; the case has CaseTimeout. Run it with
// the private stores through the build's wrapper, -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.
//
// TestT3WorldEvsBurstsShareEachClient fails today, most runs, and TestT3WorldEvsReadAnsweredBeforeItsOwnWrite every run,
// and both stay failing until the client is fixed (the e2e conventions): one EvsClient's concurrent reads and writes
// judge a read's reply against the pin and read records another of its calls moved after the read was asked, so a head
// evs served at or above the read's own pin is refused as a fork (ErrEvsHeadForked, head_alarm), a path's entry as
// below what this device read (ErrEvsEntryRefused, a tamper alarm), or a write's head as below the pin
// (ErrEvsPinLower); and the read records it leaves fail a later listing on the same device with no call beside it.
// Run one call at a time per client, the burst case passes. The read-answered case fixes the order with a hold at evs's
// origin (T3EvsSettings.Handler), so it fails the same way every run.

package sidecart3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

// What the burst case runs with: every count, pause and timeout it uses (QD-702).
type t3EvsBurstSettings struct {
	// The first network's sidecars, at least 8, and the second's.
	FirstSidecarCount  int
	SecondSidecarCount int
	// Each sidecar's bursts, each a goroutine of its own, and the writes each burst makes to its own path, in order.
	BurstCount      int
	BurstWriteCount int
	// How many of one sidecar's bursts run at once, on its one evs client: all of them, as an app's writes run beside its
	// sync's reads. 1 runs a sidecar's bursts one after another, the sidecars still all at once.
	SidecarBurstConcurrency int
	// The longest random pause before a burst and after each of its writes.
	MaxPause time.Duration
	// The pauses' and the reads' seed, logged so a run's shape repeats.
	Seed uint64
	// How many times one write builds its push again after evs asks for it, every sidecar of a network writing one ref
	// at once (sidecar.EvsSettings.PushRebuildCount, 8 by default).
	PushRebuildCount int
	// How many sidecars verify at once at the end.
	VerifyConcurrency int
	// Each phase's bound, after which it fails with every burst's progress, and the case's.
	PhaseTimeout time.Duration
	CaseTimeout  time.Duration
}

func defaultT3EvsBurstSettings() *t3EvsBurstSettings {
	return &t3EvsBurstSettings{
		FirstSidecarCount:       8,
		SecondSidecarCount:      3,
		BurstCount:              3,
		BurstWriteCount:         3,
		SidecarBurstConcurrency: 1,
		MaxPause:                50 * time.Millisecond,
		Seed:                    uint64(time.Now().UnixNano()),
		PushRebuildCount:        64,
		VerifyConcurrency:       4,
		PhaseTimeout:            4 * time.Minute,
		CaseTimeout:             10 * time.Minute,
	}
}

// One network of the case: its sidecars, its repo, and the paths every burst of it writes.
type t3EvsBurstNetwork struct {
	network *T3Network
	devices []*T3Device
	repoId  sidecar.Id
	// every burst of the network's sidecars, in order
	bursts []*t3EvsBurst
	// each content written in the network, by its text: the burst and the write's place in it
	contentWrites map[string]*t3EvsBurstWrite
}

// One burst: a sidecar, its path, its writes as their answers came, and how far its reads have got.
type t3EvsBurst struct {
	device *T3Device
	index  int
	path   string

	stateLock sync.Mutex
	writes    []*t3EvsBurstWrite
	// what failed, a write's or a read's
	errs []error
	done bool
}

// One write of a burst: its place, its content, and what its writer was answered.
type t3EvsBurstWrite struct {
	burst    *t3EvsBurst
	sequence int
	content  []byte
	result   *sidecar.EvsWriteResult
}

// The content of burst's write at sequence: the network, the sidecar, the burst and the place, so a read names it.
func t3EvsBurstContent(network string, device string, burstIndex int, sequence int) []byte {
	return []byte(fmt.Sprintf("%s: %s's burst %d, write %d\n", network, device, burstIndex, sequence))
}

// A network named name with count sidecars, its first bootstrapping it and admitting the rest, and a general repo the
// first created and granted its member writer: every sidecar's evs client ready to write it.
func setUpT3EvsBurstNetwork(ctx context.Context, world *T3World, name string, count int, settings *t3EvsBurstSettings) (*t3EvsBurstNetwork, error) {
	network, err := world.AddNetwork(ctx, name)
	if err != nil {
		return nil, err
	}
	devices, err := network.AddSidecars(ctx, count, func(name string, sidecarSettings *sidecar.SidecarSettings) {
		sidecarSettings.Evs.PushRebuildCount = settings.PushRebuildCount
	})
	if err != nil {
		return nil, fmt.Errorf("%s's sidecars: %w", name, err)
	}
	owner := devices[0]
	if err := network.Bootstrap(ctx, owner); err != nil {
		return nil, err
	}
	if err := network.Admit(ctx, devices[1:]); err != nil {
		return nil, err
	}
	repo, err := network.CreateRepo(ctx, owner, protocol.RepoKind_REPO_KIND_GENERAL)
	if err != nil {
		return nil, err
	}
	if _, err := network.Grant(ctx, owner, repo.RepoId(), network.MemberId(), protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER); err != nil {
		return nil, err
	}
	self := &t3EvsBurstNetwork{network: network, devices: devices, repoId: repo.RepoId(), contentWrites: map[string]*t3EvsBurstWrite{}}
	for _, device := range devices {
		if _, err := device.EvsClient(); err != nil {
			return nil, err
		}
		for index := range settings.BurstCount {
			burst := &t3EvsBurst{device: device, index: index, path: fmt.Sprintf("%s-burst-%d.txt", device.Name, index)}
			for sequence := range settings.BurstWriteCount {
				write := &t3EvsBurstWrite{burst: burst, sequence: sequence, content: t3EvsBurstContent(name, device.Name, index, sequence)}
				burst.writes = append(burst.writes, write)
				self.contentWrites[string(write.content)] = write
			}
			self.bursts = append(self.bursts, burst)
		}
	}
	return self, nil
}

// The repo as device's own evs client reads and writes it.
func (self *t3EvsBurstNetwork) repo(device *T3Device) (*sidecar.EvsRepo, error) {
	client, err := device.EvsClient()
	if err != nil {
		return nil, err
	}
	return client.Repo(self.repoId), nil
}

// Each path the network's bursts write, sorted.
func (self *t3EvsBurstNetwork) paths() []string {
	paths := []string{}
	for _, burst := range self.bursts {
		paths = append(paths, burst.path)
	}
	slices.Sort(paths)
	return paths
}

func (self *t3EvsBurst) fail(err error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errs = append(self.errs, err)
}

// What the burst has done, for a stalled phase's dump.
func (self *t3EvsBurst) progress() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	committed := 0
	for _, write := range self.writes {
		if write.result != nil {
			committed += 1
		}
	}
	return fmt.Sprintf("%s: %d of %d written, done %t, %d errors", self.path, committed, len(self.writes), self.done, len(self.errs))
}

// One burst's run: a random pause, then each write in order, each followed by a read of another burst's path of the
// same network, whose content must be one of that path's writes and never older than the burst read before.
func (self *t3EvsBurst) run(ctx context.Context, network *t3EvsBurstNetwork, settings *t3EvsBurstSettings, random *rand.Rand) {
	defer func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.done = true
	}()
	repo, err := network.repo(self.device)
	if err != nil {
		self.fail(err)
		return
	}
	pause := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(time.Duration(random.Int64N(int64(settings.MaxPause) + 1))):
			return true
		}
	}
	// the newest write of each path this burst has read
	seenSequences := map[string]int{}
	for _, write := range self.writes {
		if !pause() {
			self.fail(fmt.Errorf("%s stopped before write %d: %w", self.path, write.sequence, ctx.Err()))
			return
		}
		result, err := repo.Write(ctx, &sidecar.EvsWriteArgs{Path: self.path, Content: bytes.NewReader(write.content), Size: int64(len(write.content))})
		if err != nil {
			// no write is retried: a refusal or an unknown outcome is the case's failure, never a second commit
			self.fail(fmt.Errorf("%s's write %d: %w", self.path, write.sequence, err))
			return
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			write.result = result
		}()

		other := network.bursts[random.IntN(len(network.bursts))]
		if other == self {
			continue
		}
		var read bytes.Buffer
		info, err := repo.ReadHead(ctx, &sidecar.EvsReadArgs{Path: other.path}, &read)
		if err != nil {
			if errors.Is(err, sidecar.ErrEvsPathAbsent) {
				if _, seen := seenSequences[other.path]; seen {
					self.fail(fmt.Errorf("%s read %s as gone after reading it", self.path, other.path))
				}
				continue
			}
			self.fail(fmt.Errorf("%s's read of %s: %w", self.path, other.path, err))
			continue
		}
		readWrite := network.contentWrites[read.String()]
		if readWrite == nil || readWrite.burst != other || info == nil {
			self.fail(fmt.Errorf("%s read %s as %q, no write of its", self.path, other.path, read.String()))
			continue
		}
		if seen, ok := seenSequences[other.path]; ok && readWrite.sequence < seen {
			self.fail(fmt.Errorf("%s read %s back at write %d after write %d", self.path, other.path, readWrite.sequence, seen))
		}
		seenSequences[other.path] = readWrite.sequence
	}
}

// Fails t with every burst's progress: what a stalled phase shows.
func failT3EvsBurstProgress(t testing.TB, phase string, networks []*t3EvsBurstNetwork, err error) {
	t.Helper()
	lines := []string{}
	for _, network := range networks {
		for _, burst := range network.bursts {
			lines = append(lines, "  "+burst.progress())
		}
	}
	t.Fatalf("%s did not finish: %v\n%s", phase, err, strings.Join(lines, "\n"))
}

// device verifies the network's repo as the case's header says: its listing, every path's history and content, and
// every commit's ref version.
func (self *t3EvsBurstNetwork) verify(ctx context.Context, device *T3Device) error {
	repo, err := self.repo(device)
	if err != nil {
		return err
	}
	listing, err := repo.List(ctx, &sidecar.EvsReadArgs{Fresh: true})
	if err != nil {
		return fmt.Errorf("%s's listing: %w", device.Name, err)
	}
	names := []string{}
	for _, entry := range listing.Entries {
		names = append(names, string(entry.Name))
	}
	slices.Sort(names)
	if !slices.Equal(names, self.paths()) {
		return fmt.Errorf("%s lists %v, not the bursts' %v", device.Name, names, self.paths())
	}
	commitVersions := map[uint64]string{}
	for _, burst := range self.bursts {
		history, err := repo.History(ctx, &sidecar.EvsReadArgs{Path: burst.path, Fresh: true})
		if err != nil {
			return fmt.Errorf("%s's history of %s: %w", device.Name, burst.path, err)
		}
		if !history.Whole || len(history.Versions) != len(burst.writes) {
			return fmt.Errorf("%s's history of %s lists %d versions, whole %t, not %d", device.Name, burst.path, len(history.Versions), history.Whole, len(burst.writes))
		}
		writer := burst.device.Sidecar().Identity().SidecarId
		for index, version := range history.Versions {
			// newest first
			write := burst.writes[len(burst.writes)-1-index]
			switch {
			case version.Removed || version.Created != (write.sequence == 0):
				return fmt.Errorf("%s reads %s's version %d as removed %t, created %t", device.Name, burst.path, version.Version, version.Removed, version.Created)
			case version.PushId != write.result.PushId || version.Version != write.result.PathVersion || version.CommitVersion != write.result.Version:
				return fmt.Errorf("%s reads %s's write %d at push %s, version %d, ref version %d; its writer was answered %s, %d, %d",
					device.Name, burst.path, write.sequence, version.PushId, version.Version, version.CommitVersion, write.result.PushId, write.result.PathVersion, write.result.Version)
			case version.SignerSidecarId != writer:
				return fmt.Errorf("%s reads %s's write %d as signed by %s, not its writer %s", device.Name, burst.path, write.sequence, version.SignerSidecarId, writer)
			}
			if other, held := commitVersions[version.CommitVersion]; held {
				return fmt.Errorf("%s reads ref version %d twice, at %s and at %s's write %d", device.Name, version.CommitVersion, other, burst.path, write.sequence)
			}
			commitVersions[version.CommitVersion] = fmt.Sprintf("%s's write %d", burst.path, write.sequence)
			var content bytes.Buffer
			if _, err := repo.ReadVersion(ctx, &sidecar.EvsReadArgs{Path: burst.path, Version: version.Version}, &content); err != nil {
				return fmt.Errorf("%s's read of %s's version %d: %w", device.Name, burst.path, version.Version, err)
			}
			if !bytes.Equal(content.Bytes(), write.content) {
				return fmt.Errorf("%s reads %s's version %d as %q, not %q", device.Name, burst.path, version.Version, content.String(), write.content)
			}
		}
	}
	if listing.HeadVersion != uint64(len(commitVersions)) {
		return fmt.Errorf("%s reads the ref's head at %d after %d commits", device.Name, listing.HeadVersion, len(commitVersions))
	}
	return nil
}

// The second network's sidecars against the first's repo: none reads it through its own client, whose state holds no
// such repo; the exchange grants none of them a scope on it; and evs refuses each one's token, a read scope on its own
// network's repo, at the first's repo, so isolation stands at both servers and not only on the device.
func checkT3EvsBurstIsolation(ctx context.Context, world *T3World, first *t3EvsBurstNetwork, second *t3EvsBurstNetwork) error {
	firstPath := first.paths()[0]
	evsOrigin := world.Api().ServiceOrigin("evs")
	for _, device := range second.devices {
		client, err := device.EvsClient()
		if err != nil {
			return err
		}
		if listing, err := client.Repo(first.repoId).List(ctx, &sidecar.EvsReadArgs{Fresh: true}); err == nil {
			return fmt.Errorf("%s of the second network listed the first's repo: %d entries", device.Name, len(listing.Entries))
		}
		var read bytes.Buffer
		if _, err := client.Repo(first.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: firstPath}, &read); err == nil || 0 < read.Len() {
			return fmt.Errorf("%s of the second network read the first's %s", device.Name, firstPath)
		}
		if _, err := device.Sidecar().ServiceToken(ctx, evsOrigin, []string{"repo:" + first.repoId.String() + ":r"}); err == nil {
			return fmt.Errorf("the exchange granted %s of the second network a read of the first's repo", device.Name)
		}
		token, err := device.Sidecar().ServiceToken(ctx, evsOrigin, []string{"repo:" + second.repoId.String() + ":r"})
		if err != nil {
			return fmt.Errorf("%s's read token for its own network's repo: %w", device.Name, err)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, evsOrigin+"/evs/v1/repos/"+sidecar.EvsKeyId(first.repoId), nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := world.Api().Transport().RoundTrip(request)
		if err != nil {
			return err
		}
		answer, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			return fmt.Errorf("evs answered %s of the second network %d at the first's repo, not 403: %q", device.Name, response.StatusCode, answer)
		}
	}
	return nil
}

// The case on settings: see the file's header.
func runT3EvsBursts(t *testing.T, settings *t3EvsBurstSettings) {
	if settings.FirstSidecarCount < 8 {
		t.Fatalf("the first network runs %d sidecars, fewer than the case's 8", settings.FirstSidecarCount)
	}
	t.Logf("seed %d", settings.Seed)
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		caseCtx, cancelCase := context.WithTimeout(context.Background(), settings.CaseTimeout)
		defer cancelCase()
		world, err := NewT3World(tb, nil)
		if err != nil {
			tb.Fatal(err)
		}
		defer world.Close()
		if _, err := world.ServeEvs(caseCtx, nil); err != nil {
			tb.Fatal(err)
		}

		setUpCtx, cancelSetUp := context.WithTimeout(caseCtx, settings.PhaseTimeout)
		first, err := setUpT3EvsBurstNetwork(setUpCtx, world, "a", settings.FirstSidecarCount, settings)
		if err != nil {
			cancelSetUp()
			tb.Fatalf("the first network: %v", err)
		}
		second, err := setUpT3EvsBurstNetwork(setUpCtx, world, "b", settings.SecondSidecarCount, settings)
		cancelSetUp()
		if err != nil {
			tb.Fatalf("the second network: %v", err)
		}
		networks := []*t3EvsBurstNetwork{first, second}

		// every burst of both networks at once, at most SidecarBurstConcurrency of one sidecar's
		burstCtx, cancelBursts := context.WithTimeout(caseCtx, settings.PhaseTimeout)
		var group sync.WaitGroup
		for networkIndex, network := range networks {
			deviceTickets := map[*T3Device]chan struct{}{}
			for _, device := range network.devices {
				deviceTickets[device] = make(chan struct{}, max(1, settings.SidecarBurstConcurrency))
			}
			for burstIndex, burst := range network.bursts {
				random := rand.New(rand.NewPCG(settings.Seed, uint64(networkIndex*1000000+burstIndex)))
				tickets := deviceTickets[burst.device]
				group.Go(func() {
					select {
					case tickets <- struct{}{}:
					case <-burstCtx.Done():
						burst.fail(fmt.Errorf("%s never started: %w", burst.path, burstCtx.Err()))
						return
					}
					defer func() { <-tickets }()
					burst.run(burstCtx, network, settings, random)
				})
			}
		}
		group.Wait()
		burstErr := burstCtx.Err()
		cancelBursts()
		if burstErr != nil {
			failT3EvsBurstProgress(tb, "the bursts", networks, burstErr)
		}
		// a burst's failure fails the case, and the verification still reads what the writes left
		for _, network := range networks {
			for _, burst := range network.bursts {
				for _, err := range burst.errs {
					tb.Errorf("%v", err)
				}
			}
		}

		// every sidecar of each network verifies its network's repo, once every write of the network committed
		verifyCtx, cancelVerify := context.WithTimeout(caseCtx, settings.PhaseTimeout)
		defer cancelVerify()
		tickets := make(chan struct{}, max(1, settings.VerifyConcurrency))
		verifyErrs := make(chan error, settings.FirstSidecarCount+settings.SecondSidecarCount)
		for _, network := range networks {
			uncommitted := 0
			for _, burst := range network.bursts {
				for _, write := range burst.writes {
					if write.result == nil {
						uncommitted += 1
					}
				}
			}
			if 0 < uncommitted {
				tb.Errorf("%d writes of network %s did not commit, so its repo is not verified", uncommitted, network.network.Name)
				continue
			}
			for _, device := range network.devices {
				group.Go(func() {
					tickets <- struct{}{}
					defer func() { <-tickets }()
					if err := network.verify(verifyCtx, device); err != nil {
						verifyErrs <- err
					}
				})
			}
		}
		group.Wait()
		close(verifyErrs)
		for err := range verifyErrs {
			tb.Errorf("%v", err)
		}
		if verifyCtx.Err() != nil {
			failT3EvsBurstProgress(tb, "the verification", networks, verifyCtx.Err())
		}

		// the second network sees none of the first's
		for _, path := range first.paths() {
			if slices.Contains(second.paths(), path) {
				tb.Fatalf("both networks write %s, so the second's listing proves nothing", path)
			}
		}
		if err := checkT3EvsBurstIsolation(verifyCtx, world, first, second); err != nil {
			tb.Error(err)
		}
	})
}

// Every sidecar of both networks at once, each running its bursts one after another on its evs client.
func TestT3WorldEvsBursts(t *testing.T) {
	runT3EvsBursts(t, defaultT3EvsBurstSettings())
}

// The same case with each sidecar's bursts all at once on its one evs client, which documents itself safe for
// concurrent use, as an app's writes run beside its sync's reads.
func TestT3WorldEvsBurstsShareEachClient(t *testing.T) {
	settings := defaultT3EvsBurstSettings()
	settings.SidecarBurstConcurrency = settings.BurstCount
	runT3EvsBursts(t, settings)
}

// Holds evs's answer to the one read a test arms it for: the block answers that read in full, then its answer waits
// until the test releases it, as a slow link holds a reply that has left evs. Every other request passes.
type t3EvsReadHold struct {
	// closed once the armed read's answer waits
	held chan struct{}
	// closed by the test
	release chan struct{}

	stateLock sync.Mutex
	armed     bool
}

func newT3EvsReadHold() *t3EvsReadHold {
	return &t3EvsReadHold{held: make(chan struct{}), release: make(chan struct{})}
}

// Holds the next read's answer.
func (self *t3EvsReadHold) arm() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.armed = true
}

// Whether request is the armed read, which this takes.
func (self *t3EvsReadHold) take(request *http.Request) bool {
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/read") {
		return false
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	taken := self.armed
	self.armed = false
	return taken
}

// The block's handler with the hold in front of it (T3EvsSettings.Handler).
func (self *t3EvsReadHold) wrap(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !self.take(r) {
			handler.ServeHTTP(w, r)
			return
		}
		answer := httptest.NewRecorder()
		handler.ServeHTTP(answer, r)
		close(self.held)
		select {
		case <-self.release:
		case <-r.Context().Done():
			return
		}
		for key, values := range answer.Header() {
			w.Header()[key] = values
		}
		w.WriteHeader(answer.Code)
		w.Write(answer.Body.Bytes())
	})
}

// One sidecar's read, answered by evs before the same sidecar's own write commits and reaching it after: evs's answer
// is the ref's head when the read was asked, at or past the pin the read named, and its chain is the one the sidecar
// then pinned past, so the read stands, with no alarm. The hold makes the order: the read's answer waits at evs's
// origin while the sidecar writes another path and reads it back, which pins the newer head, then goes on. Today the
// client judges the answer against the pin its write moved, refusing it as a fork (ErrEvsHeadForked) and raising
// head_alarm: TestT3WorldEvsBurstsShareEachClient's failure, in a fixed order. It stays failing until the client is
// fixed.
func TestT3WorldEvsReadAnsweredBeforeItsOwnWrite(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		world, err := NewT3World(tb, nil)
		if err != nil {
			tb.Fatal(err)
		}
		defer world.Close()
		hold := newT3EvsReadHold()
		evsSettings := DefaultT3EvsSettings()
		evsSettings.Handler = hold.wrap
		if _, err := world.ServeEvs(ctx, evsSettings); err != nil {
			tb.Fatal(err)
		}
		network, err := world.AddNetwork(ctx, "a")
		if err != nil {
			tb.Fatal(err)
		}
		devices, err := network.AddSidecars(ctx, 1, nil)
		if err != nil {
			tb.Fatal(err)
		}
		device := devices[0]
		if err := network.Bootstrap(ctx, device); err != nil {
			tb.Fatal(err)
		}
		repo, err := network.CreateRepo(ctx, device, protocol.RepoKind_REPO_KIND_GENERAL)
		if err != nil {
			tb.Fatal(err)
		}
		alarms := make(chan sidecar.SidecarEvent, 16)
		removeAlarms := device.Sidecar().AddEventCallback(func(event sidecar.SidecarEvent) {
			if event.Kind != sidecar.SidecarEventHeadAlarm && event.Kind != sidecar.SidecarEventEvsAlarm {
				return
			}
			select {
			case alarms <- event:
			default:
			}
		})
		defer removeAlarms()
		write := func(path string, content string) {
			if _, err := repo.Write(ctx, &sidecar.EvsWriteArgs{Path: path, Content: strings.NewReader(content), Size: int64(len(content))}); err != nil {
				tb.Fatalf("the write of %s: %v", path, err)
			}
		}
		write("asked.txt", "the version the held read asks for")

		type readResult struct {
			content string
			err     error
		}
		read := make(chan readResult, 1)
		hold.arm()
		go func() {
			var content bytes.Buffer
			_, err := repo.ReadHead(ctx, &sidecar.EvsReadArgs{Path: "asked.txt"}, &content)
			read <- readResult{content: content.String(), err: err}
		}()
		select {
		case <-hold.held:
		case <-ctx.Done():
			tb.Fatal("the read's answer never reached the hold")
		}
		// the device's own write commits, and its read back pins the head past the held answer's
		write("written.txt", "a write that commits while the read's answer is held")
		var pinned bytes.Buffer
		if _, err := repo.ReadHead(ctx, &sidecar.EvsReadArgs{Path: "written.txt", Fresh: true}, &pinned); err != nil {
			tb.Fatalf("the read back of the device's own write: %v", err)
		}
		close(hold.release)

		var result readResult
		select {
		case result = <-read:
		case <-ctx.Done():
			tb.Fatal("the held read never returned")
		}
		if result.err != nil || result.content != "the version the held read asks for" {
			tb.Errorf("the read evs answered before the device's own write: %q, %v", result.content, result.err)
		}
		select {
		case alarm := <-alarms:
			tb.Errorf("the device raised %s (%s) over its own write", alarm.Kind, alarm.HeadAlarm)
		default:
		}
	})
}
