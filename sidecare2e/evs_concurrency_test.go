//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Many writers and readers on evs at once, with chaos (the end-to-end plan's e2e-evs-concurrency; the owner's
// end-to-end tests of 2026-11-21): N sidecars of one network (ClientCount, 8 by default) on the world's real keys,
// every one a writer, a reader and a subscriber of two repos, with random pauses throughout. sidecar.yml opens §19.11's
// format2 gate to the network, as production's M10 release does, since a DROP and a completion ERASE are format 2's.
//
// The repos: a general one, the writers' repo, and a group inbox, a general repo whose files keep their keys in slots
// (§11.12), so an erasure can name a path's keys. Setup writes a seed file at each repo's top and in each directory
// the phases write under, one push each, so the bursts' writers create only their own paths under standing parents.
//
// The first phase, the writes:
//
//   - first, every writer creates its own file under one directory that does not stand yet, all at once: their paths
//     are disjoint, so every write must land (§11.7's file row: evs lands a disjoint push itself), the parent's
//     concurrent creations resolved by the writers' clients planning once more (§11.20's Write);
//   - then every writer lands its own file on the general repo's file-mode main, FileWriteCount versions in order, its
//     own inbox file InboxWriteCount times, and DirectoryCount directories under one parent, each holding one file, so
//     the parent's directory deltas merge across writers (§11.7's directory row);
//   - every device reads other writers' files throughout, each read one of that path's writes and never older than
//     the last the same burst read (§11.9's pins, I-16);
//   - every device subscribed to both repos' main before the writes (§11.10), its listener counting each change;
//   - once ChaosCommitCount of the phase's commits are acknowledged, the evs block restarts on its stores mid-push:
//     the hook at its origin (evs_concurrency_harness_test.go) lets the block commit the next push, restarts it, and
//     loses the answer, so its writer's client sends the same push again to the restarted block.
//
// The second phase, drops, a hold and an erasure while the writers keep pushing their second files:
//
//   - a hold placed on the general repo, as api mirrors one (evs_concurrency_harness_test.go), and released once the
//     drop has committed; no answer, listing or read tells a device it stood (owner decisions 93, 184);
//   - the network's owner drops the history of one writer's first file with now (§11.7's dropped history): its dry
//     run, which taskworker's evs.drop runs and the harness's stand-in runs here, then the DROP, which the hook lets
//     the block commit before it restarts the block and loses the answer, so the restart lands mid-drop;
//   - the owner erases one writer's inbox file: an ERASE of every key its versions name, then a completion ERASE that
//     says the erasure complete (§11.12, QD-1137).
//
// The third phase, once the first two are verified: RacerCount devices race exact-head ACL saves on the config repo's
// file-mode main (review-fix-acl-config-ref-cas, owner decision 141 as amended on 2026-11-20) while the writers write
// their third files: every racer previews at one verified head, then all save at once, each save signing EXACT_HEAD on
// the head it previewed; one push lands per head, and every other save is refused as the head having moved, so its
// racer previews again at the next round. Each round's winner publishes a base, so the phase runs last: it ends with
// every device reading every path the case wrote, under the new bases.
//
// What must hold, strictly:
//
//   - the disjoint first creations under the new parent all land;
//   - every acknowledged commit is present once: on every device, each repo's main lists exactly the paths written,
//     each path's verified History lists exactly its acknowledged commits, newest first, at the push id, path version,
//     ref version and signer its writer was answered, and the ref versions of all of them are main's versions from 1 to
//     its head, each once, so nothing acknowledged was lost and nothing was applied twice, across both restarts; each
//     push whose answer a restart lost was answered again at the version the block first committed it at;
//   - no device raises a fork, tamper, regression or gap alarm, nor a drop it finds invalid;
//   - every subscriber hears each change of each repo's main once: past the version setup left, every version once and
//     none twice, with no gap;
//   - the exact-head races land one winner per head, one config push each, every other save refused as the head having
//     moved, and every racer's document lands once; and every device still reads every path once the races' bases
//     have landed;
//   - the dropped path reads as §11.7 says on every device: each listed version dropped, its history kept, and the
//     DROP's kept version, the path's next, holding the head's bytes; the erased path reads erased on every device, and
//     both ERASEs read back with the completion ending the erasure;
//   - the drop lists the same while the hold stands and after it ends, pending until its job's start, and lists at
//     once when its retry is answered, the restart mid-drop having drained the block that committed it;
//   - the ledgers at the end, the evs block drained by a restart: the network's stored bytes in server's quota ledger
//     are exactly the count evs.nightly lists from the clusters; every byte drawn from its balances is a debit on a
//     meter, exactly; api's write budget is exactly what the objects api wrote price at as created, the hold's object
//     among them (evs_concurrency_harness_test.go); the taskworker stand-in's report, written as its budget closed, is
//     exactly what its jobs were granted; every evs run a restart stopped wrote its newest report as its budget closed
//     in that restart, so the report holds all it spent, and each run that answered commits reported spend; every
//     process that spent on the metadata cluster's write budget has a report in the ledger, and the live ones hold
//     nothing in flight or queued.
//
// Not held to the byte: the blocks' own spend. evs prices each push before it issues it, from its record's and head's
// sizes, its changes and its slots (sidecar/evsservice/push_commit.go's cost), as gkm prices its writes, and the world
// builds each block's budget inside its host's run (evshost's newBlockEngine, gkm's Run) with no hook to count what
// the engine issued, so the case has no count of a block's spend to hold its reports to; it holds each evs run to the
// report its budget wrote as it closed, and per-object attribution of the blocks' spend waits for the review phase.
//
// A failure from a product bug stays failing; the case reports it with its evidence. Every finding it met is fixed:
//
//   - as it landed (2026-11-22), five: the disjoint first creations refused 409 PATH_CONFLICT for all but one writer
//     (evs-dir-creation-conflict, sidecar b9e302b2, 05ec181f); every read of a path untouched since two bases, and
//     every write whose plan read one, refused "config pin before what the reader retains" (QD-1178,
//     evs-unretained-pin-reads, sidecar aeca8de8, a9e7bb50); writes beside the races refused 403 by gkm on a token for
//     a stronger role than gkm's state gave the writer (QD-1179, gkm-stale-scope-retry, sidecar 22a842c3: the client
//     now tries such a call again under its own repo's scope); an evs run's spend since its last report lost as it
//     closed (write-budget-close-report, sidecar 5736a959, ea5593b0); and a commit answered just before the block's
//     drain losing its step 8, its stored-byte delta, hints and a DROP's table append (evs-drain-step8, sidecar
//     75f66a56), which the drop's listing no longer covers with a fresh read of its path;
//   - on those five fixes, in one run of six (e2e-evs-verify's, 2026-11-22), one more: a read beside the races, or a
//     write whose plan reads the head, refused by the device's own verifier "config pin ahead of the state: gkm auth
//     pin ahead of the state: 13 above 12", at the base the first race round's save had just published, under which
//     another writer whose follower held that base had pushed while the reader's follower was still a position behind
//     (run 2 refused a-7's read of files2/a-0.txt, and evs-drain-step8's runs met it on two writes). QD-1202 (question
//     e2e-evs-verify-statement-pin-ahead-of-reader) has the reader wait, and evs-read-wait-pin-ahead (sidecar 9e99861c
//     to 0c66baa8) built it: EvsVerifier.VerifyStatement waits for the device's config log to reach a pin above its
//     state, within EvsSettings.AuthPinWaitTimeout (60 s) and the caller's ctx, and judges the statement at its own
//     pin, withholding it with ErrConfigPinAhead only past that bound. A refusal of that kind still says when it came
//     back and where the device's follower and the newest one stood (pinAheadNote).
//
// On every fix, e2e-headers-final's run passed (2026-11-22, 51 s, server e4a38cd64 on sidecar c88a64af).
//
// Each phase has PhaseTimeout and dumps the world's progress when it stalls; the case has the conventions' 10 minutes.
// Run it through the build's wrapper: -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math/rand/v2"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/sidecart3"
)

// What the case runs with (QD-702): every count, name, pause and bound it uses.
type evsConcurrencySettings struct {
	// N, the sidecars of the one network: at least 3, since the drop's and the erasure's targets are writers other
	// than the owner.
	ClientCount int
	// The versions each writer writes to its own file in each phase, in order.
	FileWriteCount int
	// The versions each writer writes to its own inbox file in each phase, in order.
	InboxWriteCount int
	// The directories each writer creates under the shared parent in the first phase, each with one file.
	DirectoryCount int
	// The reads each device makes of other writers' files in each phase.
	ReadCount int
	// The devices, from the first, that race exact-head ACL saves on the config repo.
	RacerCount int
	// Each racer's ACL document, by its place: one grant of a host of its own, so every save changes the head.
	RaceAclPolicy func(index int) string
	// The first phase's commits on the general repo acknowledged before the evs block restarts mid-push.
	ChaosCommitCount int
	// sidecar.yml's format2 gate, a line under sidecar: (§19.11): open to every network, purge and drop observing, so
	// nothing deletes.
	Format2GateLine string
	// §7.9's config_bases_per_min for the case: each race round's winner publishes a base, which the Community
	// column's 1 a minute would hold a minute a round; api's limiter allows one base each 60 s over this, with no
	// burst, so the rounds' bases, a moment apart, need it high.
	ConfigBasesPerMinute int64
	// Each sidecar's push rebuilds (EvsSettings.PushRebuildCount): every writer of the network lands on one ref, and a
	// race round's base moves every writer's auth pin.
	PushRebuildCount int
	// The repos' directories and names: the directory the writers' first creations race under, each writer's first,
	// second and third files, the parent its directories go under and the file each holds, its first and second inbox
	// files, and the seed file setup writes at each repo's top and in each directory but the racing one.
	FreshDirectory       string
	FileDirectory        string
	SecondFileDirectory  string
	ThirdFileDirectory   string
	ParentDirectory      string
	DirectoryFileName    string
	InboxDirectory       string
	SecondInboxDirectory string
	SeedFileName         string
	// The writers whose first file is dropped and whose first inbox file is erased, by place; neither is the owner,
	// who signs both.
	DropTargetIndex  int
	EraseTargetIndex int
	// How long the hold's terms run from its placement: past the case.
	HoldLife time.Duration
	// The bursts' pauses and seed.
	Bursts *WorldBurstSettings
	// Each phase's bound.
	PhaseTimeout time.Duration
	// How long, after the writes, the subscribers may take to hear every change by hints alone, which are lossy by
	// design, so a miss is logged; and then, after each device's fresh read, how long it may take to hear the rest.
	HintSettleTimeout time.Duration
	SettleTimeout     time.Duration
	// How long a read or a preview answered unavailable while the evs block restarts is tried again, and how often.
	UnavailableTimeout       time.Duration
	UnavailableRetryInterval time.Duration
	// How often the taskworker stand-in runs the general repo's dry runs while the drop's preview waits.
	DryRunInterval time.Duration
	// How often the budget sampler reads every metadata cluster's reports: inside their 15 s ttl.
	BudgetSampleInterval time.Duration
	// How many times api's objects are listed around a ledger read before the check of api's write budget gives up on
	// their standing still, and the objects one LIST page asks for.
	ApiBudgetReadTryCount int
	ApiObjectPageCount    int
	// How many devices verify at once at the end.
	VerifyConcurrency int
	// The note the erasure's ERASEs seal.
	ErasureNote string
	// How many of a device's unread paths the third phase's failure names.
	FailurePathCount int
}

func defaultEvsConcurrencySettings() *evsConcurrencySettings {
	return &evsConcurrencySettings{
		ClientCount:     8,
		FileWriteCount:  3,
		InboxWriteCount: 2,
		DirectoryCount:  2,
		ReadCount:       6,
		RacerCount:      4,
		RaceAclPolicy: func(index int) string {
			return worldAclPolicy(fmt.Sprintf(`{"src": ["autogroup:member"], "dst": ["dns:race-%d"], "allow": ["admin"]}`, index))
		},
		ChaosCommitCount:         16,
		Format2GateLine:          "  format2: {enabled: true, percent: 100, purge: observe, drop: observe}",
		ConfigBasesPerMinute:     6000,
		PushRebuildCount:         64,
		FreshDirectory:           "fresh",
		FileDirectory:            "files",
		SecondFileDirectory:      "files2",
		ThirdFileDirectory:       "files3",
		ParentDirectory:          "dirs",
		DirectoryFileName:        "entry.txt",
		InboxDirectory:           "inbox",
		SecondInboxDirectory:     "inbox2",
		SeedFileName:             "seed.txt",
		DropTargetIndex:          1,
		EraseTargetIndex:         2,
		HoldLife:                 24 * time.Hour,
		Bursts:                   DefaultWorldBurstSettings(),
		PhaseTimeout:             4 * time.Minute,
		HintSettleTimeout:        20 * time.Second,
		SettleTimeout:            90 * time.Second,
		UnavailableTimeout:       2 * time.Minute,
		UnavailableRetryInterval: 500 * time.Millisecond,
		DryRunInterval:           time.Second,
		BudgetSampleInterval:     time.Second,
		ApiBudgetReadTryCount:    5,
		ApiObjectPageCount:       1000,
		VerifyConcurrency:        4,
		ErasureNote:              "e2e-evs-concurrency erasure",
		FailurePathCount:         4,
	}
}

// One acknowledged commit on a repo's main: a write, or the DROP.
type evsConcurrencyCommit struct {
	path   string
	writer *sidecart3.T3Device
	// the write's place among its path's writes, the content it wrote, and for the DROP the content its kept version
	// holds, the head's
	sequence int
	content  []byte
	drop     bool
	// what its writer was answered: the push, the ref version and the path's version
	pushId      sidecar.Id
	version     uint64
	pathVersion uint64
	// the evs block's run current when the answer came, from 1
	run int
}

// One repo of the case: its ids and every acknowledged commit on its main. Safe for concurrent use.
type evsConcurrencyRepo struct {
	name      string
	repoId    sidecar.Id
	mainRefId sidecar.Id
	// main's version once setup wrote its seed: a subscription's mark may sit at it or below it, so the changes every
	// subscriber must hear are those past it
	setupVersion uint64

	stateLock sync.Mutex
	// by ref version
	commits map[uint64]*evsConcurrencyCommit
	// two acknowledgments of one ref version, which no linear history holds
	duplicates []string
	// how many commits are acknowledged, for the chaos trigger's wait
	commitCount *sidecar.MonitorValue[int]
}

func newEvsConcurrencyRepo(name string, repoId sidecar.Id) *evsConcurrencyRepo {
	return &evsConcurrencyRepo{
		name:        name,
		repoId:      repoId,
		commits:     map[uint64]*evsConcurrencyCommit{},
		commitCount: sidecar.NewMonitorValue(0),
	}
}

// Keeps an acknowledged commit, a second one at its ref version kept as a duplicate.
func (self *evsConcurrencyRepo) add(commit *evsConcurrencyCommit) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if held, ok := self.commits[commit.version]; ok {
			self.duplicates = append(self.duplicates, fmt.Sprintf("%s's ref version %d acknowledged to %s's %s (push %s) and to %s's %s (push %s)",
				self.name, commit.version, held.writer.Name, held.path, held.pushId, commit.writer.Name, commit.path, commit.pushId))
			return
		}
		self.commits[commit.version] = commit
	}()
	self.commitCount.Update(func(count int) int { return count + 1 })
}

// Every acknowledged commit, by ref version.
func (self *evsConcurrencyRepo) commitList() []*evsConcurrencyCommit {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	commits := []*evsConcurrencyCommit{}
	for _, commit := range self.commits {
		commits = append(commits, commit)
	}
	sort.Slice(commits, func(i int, j int) bool { return commits[i].version < commits[j].version })
	return commits
}

// Each path's acknowledged commits, oldest first.
func (self *evsConcurrencyRepo) pathCommits() map[string][]*evsConcurrencyCommit {
	paths := map[string][]*evsConcurrencyCommit{}
	for _, commit := range self.commitList() {
		paths[commit.path] = append(paths[commit.path], commit)
	}
	return paths
}

// The newest ref version acknowledged, 0 for none.
func (self *evsConcurrencyRepo) headVersion() uint64 {
	commits := self.commitList()
	if len(commits) == 0 {
		return 0
	}
	return commits[len(commits)-1].version
}

// The content of a path's write at sequence.
func evsConcurrencyContent(repoName string, path string, sequence int) []byte {
	return []byte(fmt.Sprintf("%s:%s write %d\n", repoName, path, sequence))
}

// One device of the case: its client, what its listener heard and the alarms it raised. Safe for concurrent use.
type evsConcurrencyDevice struct {
	device *sidecart3.T3Device
	client *sidecar.EvsClient

	stateLock sync.Mutex
	// each repo's main versions its listener heard, and how often
	heard map[sidecar.Id]map[uint64]int
	gaps  []string
	// every head, evs or gap alarm, and every drop it found invalid
	alarms []string
	// counts every change the listener heard, for the settle's wait
	heardCount *sidecar.MonitorValue[uint64]
	removes    []func()
	// how many of its alarms a check has reported, the case's own goroutine's
	alarmsChecked int
}

// Records a change of one of the case's repos' main.
func (self *evsConcurrencyDevice) hear(repos []*evsConcurrencyRepo, change *sidecar.EvsRepoChange) {
	for _, repo := range repos {
		if change.RepoId != repo.repoId || change.RefId != repo.mainRefId {
			continue
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if change.Gap {
				self.gaps = append(self.gaps, fmt.Sprintf("%s's main at %d", repo.name, change.Version))
				return
			}
			versions := self.heard[repo.repoId]
			if versions == nil {
				versions = map[uint64]int{}
				self.heard[repo.repoId] = versions
			}
			versions[change.Version] += 1
		}()
	}
	self.heardCount.Update(func(count uint64) uint64 { return count + 1 })
}

// Records an alarm the case refuses.
func (self *evsConcurrencyDevice) event(event sidecar.SidecarEvent) {
	line := ""
	switch event.Kind {
	case sidecar.SidecarEventHeadAlarm:
		line = fmt.Sprintf("head_alarm %s", event.HeadAlarm)
	case sidecar.SidecarEventEvsAlarm:
		line = fmt.Sprintf("evs_alarm %s (%s) at version %d", event.EvsAlarm, event.EvsAlarmDetail, event.Version)
	case sidecar.SidecarEventEvsGap:
		line = fmt.Sprintf("evs_gap verified since %s", event.VerifiedSince)
	case sidecar.SidecarEventHistoryDrop:
		if event.DropState == sidecar.SidecarDropInvalid {
			line = "history_drop invalid"
		}
	}
	if line == "" {
		return
	}
	if event.RepoId != nil {
		line += fmt.Sprintf(" on repo %s", *event.RepoId)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.alarms = append(self.alarms, line)
}

// The versions of repo's main in (after, through] the listener has not heard, each heard more than once, and the gaps.
func (self *evsConcurrencyDevice) heardState(repo *evsConcurrencyRepo, after uint64, through uint64) (missing []uint64, repeated []string, gaps []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	versions := self.heard[repo.repoId]
	for version := after + 1; version <= through; version += 1 {
		if versions[version] == 0 {
			missing = append(missing, version)
		}
	}
	for version, count := range versions {
		if 1 < count {
			repeated = append(repeated, fmt.Sprintf("%d heard %d times", version, count))
		}
	}
	slices.Sort(repeated)
	return missing, repeated, slices.Clone(self.gaps)
}

// Waits until the listener heard every version of each repo's main past its setup's through its head, or the bound
// passes: the versions still missing then, by repo.
func (self *evsConcurrencyDevice) awaitHeard(ctx context.Context, repos []*evsConcurrencyRepo, heads map[*evsConcurrencyRepo]uint64, timeout time.Duration) map[string][]uint64 {
	deadline := time.After(timeout)
	for {
		_, changed := self.heardCount.Get()
		missingByRepo := map[string][]uint64{}
		for _, repo := range repos {
			if missing, _, _ := self.heardState(repo, repo.setupVersion, heads[repo]); 0 < len(missing) {
				missingByRepo[repo.name] = missing
			}
		}
		if len(missingByRepo) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return missingByRepo
		case <-deadline:
			return missingByRepo
		case <-changed:
		}
	}
}

// Every alarm the device raised.
func (self *evsConcurrencyDevice) alarmList() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.alarms)
}

// One exact-head race round: the config version its racers previewed at, and each racer's outcome.
type evsConcurrencyRaceRound struct {
	configVersion uint64
	winners       []string
	// each refused racer's error
	refusals map[string]error
	// the config version the winner's base named
	wonVersion uint64
	// when the winner's save was answered
	savedAt time.Time
}

// The case: the world, its devices and repos, and what each phase found.
type evsConcurrencyCase struct {
	t        testing.TB
	settings *evsConcurrencySettings
	world    *World
	worker   *evsConcurrencyWorker
	network  *WorldNetwork
	devices  []*evsConcurrencyDevice
	general  *evsConcurrencyRepo
	inbox    *evsConcurrencyRepo
	repos    []*evsConcurrencyRepo

	// the chaos hook at evs's origin, and the pushes whose answers its restarts lost, nil where none was taken
	origin *evsConcurrencyOrigin
	// the recorder at gkm's origin, and each repo's name for its lines: the case's two and the network's config
	gkmOrigin *evsConcurrencyGkmOrigin
	repoNames map[sidecar.Id]string
	// each evs run past the first, by its run's number: the window its start fell in, a restart's call and return
	runStarts map[int][2]time.Time
	midPush   *evsConcurrencyPushChaos
	midDrop   *evsConcurrencyPushChaos
	// what the drop and the erasure did, nil where they did not
	droppedState *evsConcurrencyDropped
	erasedState  *evsConcurrencyErased
	// the rev/ writer's writes in this process as the world began, and the P0 api's budget spent on the hold's object,
	// its create and its release's delete, which api's write budget is read against
	revWritesAtStart    *controlPlaneRevWrites
	holdPricedByteCount int64

	stateLock sync.Mutex
	// reads answered unavailable while the evs block restarted, then read again
	unavailableReads int
	// the race's rounds
	rounds []*evsConcurrencyRaceRound
}

// See the file's header.
func TestE2eEvsConcurrency(t *testing.T) {
	settings := defaultEvsConcurrencySettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		runEvsConcurrency(t, settings)
	})
}

// The case on settings: see the file's header.
func runEvsConcurrency(t testing.TB, settings *evsConcurrencySettings) {
	switch {
	case settings.ClientCount < 3:
		t.Fatalf("a case of %d sidecars has no writer to drop and another to erase beside its owner", settings.ClientCount)
	case settings.DropTargetIndex <= 0 || settings.EraseTargetIndex <= 0 || settings.ClientCount <= settings.DropTargetIndex || settings.ClientCount <= settings.EraseTargetIndex:
		t.Fatalf("the drop's and the erasure's targets must be writers other than the owner, of %d", settings.ClientCount)
	case settings.RacerCount < 2 || settings.ClientCount < settings.RacerCount:
		t.Fatalf("%d racers of %d sidecars race nothing", settings.RacerCount, settings.ClientCount)
	}
	revWritesAtStart, err := controlPlaneRevWriteCounts()
	if err != nil {
		t.Fatalf("the rev/ writer's writes: %v", err)
	}
	worldSettings := DefaultWorldSettings()
	worldSettings.NetworkCount = 1
	worldSettings.ClientsPerNetwork = settings.ClientCount
	worldSettings.SidecarYamlLines = []string{settings.Format2GateLine}
	t3Settings := sidecart3.DefaultT3WorldSettings()
	limits := *t3Settings.SidecarLimits
	limits.LimitKeyValues = maps.Clone(limits.LimitKeyValues)
	limits.LimitKeyValues["config_bases_per_min"] = settings.ConfigBasesPerMinute
	t3Settings.SidecarLimits = &limits
	worldSettings.T3 = t3Settings
	world := NewWorld(t, worldSettings)
	defer world.Close()
	origin, err := installEvsConcurrencyOrigin(world)
	if err != nil {
		t.Fatal(err)
	}
	gkmOrigin, err := installEvsConcurrencyGkmOrigin(world)
	if err != nil {
		t.Fatal(err)
	}
	sampler := startEvsConcurrencyBudgetSampler(world, settings.BudgetSampleInterval)
	defer sampler.stop()
	worker, err := newEvsConcurrencyWorker(world)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.close()
	self := &evsConcurrencyCase{
		t: t, settings: settings, world: world, worker: worker, origin: origin, gkmOrigin: gkmOrigin, runStarts: map[int][2]time.Time{},
		revWritesAtStart: revWritesAtStart,
	}

	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	if err := self.setUp(ctx); err != nil {
		cancel()
		world.FailProgress(t, "the setup", err, nil)
	}
	cancel()
	defer func() {
		for _, device := range self.devices {
			for _, remove := range device.removes {
				remove()
			}
		}
	}()
	self.logGkmRefusals("the setup")
	gkmOrigin.setDescribe(self.describeGkmCaller)

	self.freshCreations()
	self.writePhase()
	self.logGkmRefusals("the first phase")
	self.dropPhase()
	self.logGkmRefusals("the second phase")
	if t.Failed() {
		self.logState()
	}
	self.settle()
	self.verify()
	self.checkChaos()
	self.checkAlarms()
	self.racePhase()
	self.logGkmRefusals("the third phase")
	self.checkAlarms()
	self.checkLedgers(sampler)
}

// The network of ClientCount sidecars, both repos with their seeds, every device's client, evs component, alarms,
// listener and subscriptions.
func (self *evsConcurrencyCase) setUp(ctx context.Context) error {
	settings := self.settings
	network, err := self.world.AddNetwork(ctx, worldNetworkName(0), settings.ClientCount, func(name string, sidecarSettings *sidecar.SidecarSettings) {
		sidecarSettings.Evs.PushRebuildCount = settings.PushRebuildCount
	})
	if err != nil {
		return fmt.Errorf("the network: %w", err)
	}
	self.network = network
	owner := network.Owner()
	generalId, err := network.CreateSharedRepo(ctx, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER)
	if err != nil {
		return fmt.Errorf("the general repo: %w", err)
	}
	ownerClient, err := owner.EvsClient()
	if err != nil {
		return err
	}
	inboxRepo, err := ownerClient.CreateRepo(ctx, &sidecar.EvsCreateRepoArgs{Kind: protocol.RepoKind_REPO_KIND_GENERAL, Inbox: protocol.RepoInbox_REPO_INBOX_GROUP, Confirmed: true})
	if err != nil {
		return fmt.Errorf("the group inbox: %w", err)
	}
	if _, err := network.Grant(ctx, owner, inboxRepo.RepoId(), network.MemberId(), protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER); err != nil {
		return fmt.Errorf("the group inbox's grant: %w", err)
	}
	self.general = newEvsConcurrencyRepo("general", generalId)
	self.inbox = newEvsConcurrencyRepo("inbox", inboxRepo.RepoId())
	self.repos = []*evsConcurrencyRepo{self.general, self.inbox}
	self.repoNames = map[sidecar.Id]string{self.general.repoId: self.general.name, self.inbox.repoId: self.inbox.name}
	if follower := owner.Follower(); follower != nil && follower.Client() != nil && follower.Client().State() != nil {
		if configRepoId, err := sidecar.IdFromBytes(follower.Client().State().Genesis().GetConfigRepoId()); err == nil {
			self.repoNames[configRepoId] = "config"
		}
	}

	// each repo's seeds, its main's first commits, so main stands before anything subscribes to it and the bursts'
	// parents stand before they write under them
	ownerDevice := &evsConcurrencyDevice{device: owner, client: ownerClient}
	seedDirectories := map[*evsConcurrencyRepo][]string{
		self.general: {"", settings.FileDirectory, settings.SecondFileDirectory, settings.ThirdFileDirectory, settings.ParentDirectory},
		self.inbox:   {"", settings.InboxDirectory, settings.SecondInboxDirectory},
	}
	for _, repo := range self.repos {
		for _, directory := range seedDirectories[repo] {
			path := settings.SeedFileName
			if directory != "" {
				path = directory + "/" + settings.SeedFileName
			}
			if err := self.write(ctx, ownerDevice, repo, path, 0); err != nil {
				return err
			}
		}
		refId, err := ownerClient.Repo(repo.repoId).RefIdOf(ctx, sidecar.EvsMainRefName)
		if err != nil {
			return fmt.Errorf("%s's main ref: %w", repo.name, err)
		}
		repo.mainRefId = refId
		repo.setupVersion = repo.headVersion()
	}

	for _, device := range network.Clients {
		client, err := device.EvsClient()
		if err != nil {
			return fmt.Errorf("%s's evs client: %w", device.Name, err)
		}
		// its evs component, through which evs's hints reach its client and its subscriptions run
		if device.Sidecar().EvsClient() == nil {
			if _, err := device.Sidecar().AddComponent(sidecar.NewEvsComponent(client)); err != nil {
				return fmt.Errorf("%s's evs component: %w", device.Name, err)
			}
		}
		caseDevice := &evsConcurrencyDevice{
			device:     device,
			client:     client,
			heard:      map[sidecar.Id]map[uint64]int{},
			heardCount: sidecar.NewMonitorValue(uint64(0)),
		}
		caseDevice.removes = append(caseDevice.removes, device.Sidecar().AddEventCallback(caseDevice.event))
		repos := self.repos
		caseDevice.removes = append(caseDevice.removes, client.AddRepoChangeListener(nil, func(change *sidecar.EvsRepoChange) {
			caseDevice.hear(repos, change)
		}))
		for _, repo := range self.repos {
			subscription, err := client.Repo(repo.repoId).Subscribe(ctx, &sidecar.EvsSubscribeArgs{RefName: sidecar.EvsMainRefName})
			if err != nil {
				return fmt.Errorf("%s's subscription to %s: %w", device.Name, repo.name, err)
			}
			caseDevice.removes = append(caseDevice.removes, subscription.Close)
		}
		self.devices = append(self.devices, caseDevice)
	}
	return nil
}

// Restarts the evs block on its stores, keeping the window its new run started in.
func (self *evsConcurrencyCase) restartEvs() error {
	called := time.Now()
	err := self.world.Restart(WorldBlockEvs)
	run := self.world.Block(WorldBlockEvs).RunCount()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.runStarts[run] = [2]time.Time{called, time.Now()}
	return err
}

// One write of path's content at sequence as device, kept as acknowledged once its writer is answered. No write is
// retried: a refusal or an unknown outcome is the case's failure, never a second commit.
func (self *evsConcurrencyCase) write(ctx context.Context, device *evsConcurrencyDevice, repo *evsConcurrencyRepo, path string, sequence int) error {
	content := evsConcurrencyContent(repo.name, path, sequence)
	result, err := device.client.Repo(repo.repoId).Write(ctx, &sidecar.EvsWriteArgs{Path: path, Content: bytes.NewReader(content), Size: int64(len(content))})
	if err != nil {
		return fmt.Errorf("%s's write %d of %s on %s: %w%s", device.device.Name, sequence, path, repo.name, err, self.pinAheadNote(device.device, err))
	}
	repo.add(&evsConcurrencyCommit{
		path: path, writer: device.device, sequence: sequence, content: content,
		pushId: result.PushId, version: result.Version, pathVersion: result.PathVersion,
		run: self.world.Block(WorldBlockEvs).RunCount(),
	})
	return nil
}

// A read of path's head on repo as device, held to the writes of it: one of the first sequenceCount writes' content,
// at a path version never below the one the reader saw before (seen, the burst's own). An absent path reads as nothing
// until the reader has seen it. A read answered unavailable while the evs block restarts is read again, within
// UnavailableTimeout.
func (self *evsConcurrencyCase) read(ctx context.Context, device *evsConcurrencyDevice, repo *evsConcurrencyRepo, path string, sequenceCount int, fresh bool, seen map[string]uint64) error {
	deadline := time.Now().Add(self.settings.UnavailableTimeout)
	for {
		var content bytes.Buffer
		info, err := device.client.Repo(repo.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: fresh}, &content)
		switch {
		case err == nil:
		case errors.Is(err, sidecar.ErrEvsPathAbsent):
			if version, ok := seen[path]; ok {
				return fmt.Errorf("%s read %s on %s as absent after reading it at version %d", device.device.Name, path, repo.name, version)
			}
			return nil
		case evsConcurrencyUnavailable(err) && time.Now().Before(deadline):
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.unavailableReads += 1
			}()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(self.settings.UnavailableRetryInterval):
			}
			continue
		default:
			return fmt.Errorf("%s's read of %s on %s: %w%s", device.device.Name, path, repo.name, err, self.pinAheadNote(device.device, err))
		}
		matched := -1
		for sequence := range sequenceCount {
			if bytes.Equal(content.Bytes(), evsConcurrencyContent(repo.name, path, sequence)) {
				matched = sequence
				break
			}
		}
		if matched < 0 {
			return fmt.Errorf("%s read %s on %s as %q, no write of it", device.device.Name, path, repo.name, content.String())
		}
		if version, ok := seen[path]; ok && info.Version < version {
			return fmt.Errorf("%s read %s on %s back at version %d after version %d", device.device.Name, path, repo.name, info.Version, version)
		}
		seen[path] = info.Version
		return nil
	}
}

// For a refusal of a statement pinned past what device's state has reached (sidecar.ErrGkmAuthAhead), which evs's
// verifier gives since QD-1202 only once its wait for the device's log to reach the pin has run out
// (EvsSettings.AuthPinWaitTimeout): when it came back, and the log position device's config follower held then beside
// the newest any device of the case held, so the failure reads as what it refused (question
// e2e-evs-verify-statement-pin-ahead-of-reader); "" for any other error. Safe for concurrent use once setUp is done.
func (self *evsConcurrencyCase) pinAheadNote(device *sidecart3.T3Device, err error) string {
	if !errors.Is(err, sidecar.ErrGkmAuthAhead) {
		return ""
	}
	refusedAt := time.Now()
	position := func(device *sidecart3.T3Device) (uint64, bool) {
		follower := device.Follower()
		if follower == nil || follower.Client() == nil || follower.Client().State() == nil {
			return 0, false
		}
		return follower.Client().State().Pin().GetVersion(), true
	}
	own, held := position(device)
	newest, newestName := uint64(0), ""
	for _, other := range self.devices {
		if otherPosition, ok := position(other.device); ok && newest <= otherPosition {
			newest, newestName = otherPosition, other.device.Name
		}
	}
	if !held {
		return fmt.Sprintf(" (refused at %s, its follower holding no state, %s's the newest at log position %d)", refusedAt.Format("15:04:05.000"), newestName, newest)
	}
	return fmt.Sprintf(" (refused at %s, its follower at log position %d, %s's the newest at %d)", refusedAt.Format("15:04:05.000"), own, newestName, newest)
}

// A writer's file path in directory.
func (self *evsConcurrencyCase) filePath(directory string, device *sidecart3.T3Device) string {
	return directory + "/" + device.Name + ".txt"
}

// The k-th directory's file path a writer creates under the parent.
func (self *evsConcurrencyCase) directoryFilePath(device *sidecart3.T3Device, index int) string {
	return fmt.Sprintf("%s/%s-%d/%s", self.settings.ParentDirectory, device.Name, index, self.settings.DirectoryFileName)
}

// Each device's writer burst of steps, and its reader burst of ReadCount reads of other writers' files in directories,
// each a random one, fresh or not by the burst's own choice.
func (self *evsConcurrencyCase) bursts(phase string, steps func(device *evsConcurrencyDevice) []func(ctx context.Context) error, readRepo *evsConcurrencyRepo, readDirectories []string, sequenceCount int) []*WorldBurst {
	bursts := []*WorldBurst{}
	for _, device := range self.devices {
		writes := steps(device)
		bursts = append(bursts, &WorldBurst{
			Name:      fmt.Sprintf("%s's %s writes", device.device.Name, phase),
			StepCount: len(writes),
			Step: func(ctx context.Context, index int, random *rand.Rand) error {
				return writes[index](ctx)
			},
		})
	}
	for _, device := range self.devices {
		others := []*sidecart3.T3Device{}
		for _, other := range self.devices {
			if other != device {
				others = append(others, other.device)
			}
		}
		seen := map[string]uint64{}
		bursts = append(bursts, &WorldBurst{
			Name:      fmt.Sprintf("%s's %s reads", device.device.Name, phase),
			StepCount: self.settings.ReadCount,
			Step: func(ctx context.Context, index int, random *rand.Rand) error {
				other := others[random.IntN(len(others))]
				directory := readDirectories[random.IntN(len(readDirectories))]
				return self.read(ctx, device, readRepo, self.filePath(directory, other), sequenceCount, random.IntN(2) == 0, seen)
			},
		})
	}
	return bursts
}

// The first creations under a directory that does not stand yet (the file's header): every writer's own file there,
// released together. Every write must land: one refused is the case's failure, reported with the written paths its
// conflict names, none where evs found the conflict only on the parent, which the writers created at once.
func (self *evsConcurrencyCase) freshCreations() {
	t, settings := self.t, self.settings
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	start := make(chan struct{})
	errs := make([]error, len(self.devices))
	var group sync.WaitGroup
	for index, device := range self.devices {
		group.Go(func() {
			<-start
			errs[index] = self.write(ctx, device, self.general, self.filePath(settings.FreshDirectory, device.device), 0)
		})
	}
	close(start)
	group.Wait()
	refused := 0
	for index, err := range errs {
		if err == nil {
			continue
		}
		refused += 1
		named := "no written path"
		var conflict *sidecar.EvsPathConflictError
		if errors.As(err, &conflict) && 0 < len(conflict.Paths) {
			named = fmt.Sprint(conflict.Paths)
		}
		t.Errorf("%s's creation of its own file under %s, a directory its peers created at the same moment, was refused, its conflict naming %s: %v", self.devices[index].device.Name, settings.FreshDirectory, named, err)
	}
	t.Logf("%d of %d disjoint first creations under %s landed", len(errs)-refused, len(errs), settings.FreshDirectory)
}

// The first phase: every writer's first files and directories with reads beside them, and the evs block restarted
// mid-push.
func (self *evsConcurrencyCase) writePhase() {
	t, settings := self.t, self.settings
	steps := func(device *evsConcurrencyDevice) []func(ctx context.Context) error {
		writes := []func(ctx context.Context) error{}
		for index := range max(settings.FileWriteCount, settings.InboxWriteCount, settings.DirectoryCount) {
			if index < settings.FileWriteCount {
				writes = append(writes, func(ctx context.Context) error {
					return self.write(ctx, device, self.general, self.filePath(settings.FileDirectory, device.device), index)
				})
			}
			if index < settings.InboxWriteCount {
				writes = append(writes, func(ctx context.Context) error {
					return self.write(ctx, device, self.inbox, self.filePath(settings.InboxDirectory, device.device), index)
				})
			}
			if index < settings.DirectoryCount {
				writes = append(writes, func(ctx context.Context) error {
					return self.write(ctx, device, self.general, self.directoryFilePath(device.device, index), 0)
				})
			}
		}
		return writes
	}
	bursts := self.bursts("first", steps, self.general, []string{settings.FileDirectory}, settings.FileWriteCount)

	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	// chaos: once ChaosCommitCount of the phase's commits are acknowledged, the next push to the general repo commits,
	// the block restarts and its answer is lost
	seedCount := self.general.commitCount.Value()
	burstsDone := make(chan struct{})
	restarted := make(chan error, 1)
	go func() {
		for {
			count, changed := self.general.commitCount.Get()
			if seedCount+settings.ChaosCommitCount <= count {
				break
			}
			select {
			case <-ctx.Done():
				restarted <- fmt.Errorf("the restart mid-push never came: %d of %d commits acknowledged: %w", count-seedCount, settings.ChaosCommitCount, ctx.Err())
				return
			case <-changed:
			}
		}
		generalId := self.general.repoId.Bytes()
		armed := self.origin.arm(func(push *protocol.EvsPush) bool {
			return bytes.Equal(push.GetRepoId(), generalId)
		}, func() error {
			started := time.Now()
			err := self.restartEvs()
			t.Logf("the evs block restarted mid-push in %s: %v", time.Since(started).Round(time.Millisecond), err)
			return err
		})
		select {
		case <-armed.taken:
			self.midPush = armed
			restarted <- armed.chaosErr
		case <-burstsDone:
			self.origin.disarm(armed)
			restarted <- errors.New("the restart mid-push never came: no push reached the general repo once it was armed")
		case <-ctx.Done():
			self.origin.disarm(armed)
			restarted <- fmt.Errorf("the restart mid-push never came: %w", ctx.Err())
		}
	}()
	if err := RunBursts(ctx, t, "the first phase", bursts, settings.Bursts); err != nil {
		close(burstsDone)
		self.world.FailProgress(t, "the first phase", err, bursts)
	}
	close(burstsDone)
	FailBurstErrors(t, bursts)
	if err := <-restarted; err != nil {
		t.Errorf("the restart mid-push: %v", err)
	}
	if runs := self.world.Block(WorldBlockEvs).RunCount(); runs != 2 {
		t.Errorf("the evs block ran %d times after the restart mid-push", runs)
	}
}

// The exact-head races (the file's header): rounds until every racer's document has landed once.
func (self *evsConcurrencyCase) race(ctx context.Context) error {
	settings := self.settings
	racers := self.network.Clients[:settings.RacerCount]
	remaining := map[*sidecart3.T3Device]int{}
	for index, racer := range racers {
		remaining[racer] = index
	}
	for round := 0; 0 < len(remaining); round += 1 {
		if settings.RacerCount < round {
			return fmt.Errorf("%d rounds left %d racers' documents unlanded", round, len(remaining))
		}
		configVersion, err := self.configVersion(racers[0])
		if err != nil {
			return err
		}
		record := &evsConcurrencyRaceRound{configVersion: configVersion, refusals: map[string]error{}}
		// every remaining racer previews at the one verified head, then all save at once
		type racerPlan struct {
			racer  *sidecart3.T3Device
			source []byte
			plan   *sidecar.AclSavePlan
			err    error
		}
		plans := []*racerPlan{}
		for racer, index := range remaining {
			plans = append(plans, &racerPlan{racer: racer, source: []byte(settings.RaceAclPolicy(index))})
		}
		sort.Slice(plans, func(i int, j int) bool { return plans[i].racer.Name < plans[j].racer.Name })
		var group sync.WaitGroup
		for _, plan := range plans {
			group.Go(func() {
				plan.plan, plan.err = self.planAcl(ctx, plan.racer, plan.source)
			})
		}
		group.Wait()
		for _, plan := range plans {
			if plan.err != nil {
				return fmt.Errorf("round %d: %s's preview: %w", round, plan.racer.Name, plan.err)
			}
		}
		type racerSave struct {
			pin *bridge.AuthPin
			err error
			at  time.Time
		}
		saves := make([]racerSave, len(plans))
		for index, plan := range plans {
			group.Go(func() {
				pin, err := plan.racer.Sidecar().SaveAclPolicy(ctx, plan.source, plan.plan.Digest, sidecar.SidecarActConfirmed)
				saves[index] = racerSave{pin: pin, err: err, at: time.Now()}
			})
		}
		group.Wait()
		var winnerPin uint64
		var winner *sidecart3.T3Device
		for index, plan := range plans {
			save := saves[index]
			if save.err == nil {
				record.winners = append(record.winners, plan.racer.Name)
				winner, winnerPin = plan.racer, save.pin.GetVersion()
				record.savedAt = save.at
				delete(remaining, plan.racer)
				continue
			}
			record.refusals[plan.racer.Name] = save.err
			if !errors.Is(save.err, sidecar.ErrConfigHeadConflict) && !errors.Is(save.err, sidecar.ErrAclSaveStale) && !errors.Is(save.err, sidecar.ErrAclSaveUnconfirmed) {
				self.t.Errorf("round %d at config version %d: %s's save was refused other than as the head having moved: %v", round, configVersion, plan.racer.Name, save.err)
			}
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.rounds = append(self.rounds, record)
		}()
		if len(record.winners) != 1 {
			return fmt.Errorf("round %d at config version %d landed %d saves, not one: winners %v, refusals %v", round, configVersion, len(record.winners), record.winners, record.refusals)
		}
		if err := self.network.Sync(ctx, winnerPin); err != nil {
			return fmt.Errorf("round %d: the winner %s's base: %w", round, winner.Name, err)
		}
		if record.wonVersion, err = self.configVersion(racers[0]); err != nil {
			return err
		}
		if record.wonVersion != configVersion+1 {
			self.t.Errorf("round %d: %s's save took the config repo from version %d to %d, not one push", round, winner.Name, configVersion, record.wonVersion)
		}
	}
	return nil
}

// A racer's preview of source, read again while evs answers unavailable.
func (self *evsConcurrencyCase) planAcl(ctx context.Context, racer *sidecart3.T3Device, source []byte) (*sidecar.AclSavePlan, error) {
	deadline := time.Now().Add(self.settings.UnavailableTimeout)
	for {
		plan, err := racer.Sidecar().PlanAclPolicy(ctx, source)
		switch {
		case err == nil && plan.PreviewOnly:
			return nil, fmt.Errorf("%s's preview is a preview only: it writes no config head", racer.Name)
		case err == nil:
			if err := plan.Tests.Err(); err != nil {
				return nil, fmt.Errorf("the document fails its tests: %w", err)
			}
			return plan, nil
		case evsConcurrencyUnavailable(err) && time.Now().Before(deadline):
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(self.settings.UnavailableRetryInterval):
			}
		default:
			return nil, err
		}
	}
}

// The config version the newest base device's follower verified names.
func (self *evsConcurrencyCase) configVersion(device *sidecart3.T3Device) (uint64, error) {
	follower := device.Follower()
	if follower == nil || follower.Client() == nil {
		return 0, fmt.Errorf("%s follows no log", device.Name)
	}
	state := follower.Client().State()
	if state == nil || state.Base() == nil {
		return 0, fmt.Errorf("%s holds no base", device.Name)
	}
	return state.Base().GetConfigVersion(), nil
}

// What the drop phase did, for the verification.
type evsConcurrencyDropped struct {
	path    string
	push    *sidecar.EvsDropPushResult
	listing []*protocol.EvsDropStatus
}

// What the erasure did.
type evsConcurrencyErased struct {
	path       string
	erasureId  sidecar.Id
	keys       []*sidecar.EvsErasedKey
	first      *sidecar.EvsEraseResult
	completion *sidecar.EvsEraseResult
}

// The second phase: the writers' second files with reads beside them, a hold placed, the drop with the evs block
// restarting beside it, the hold released, and the erasure.
func (self *evsConcurrencyCase) dropPhase() {
	t, settings := self.t, self.settings
	steps := func(device *evsConcurrencyDevice) []func(ctx context.Context) error {
		writes := []func(ctx context.Context) error{}
		for index := range max(settings.FileWriteCount, settings.InboxWriteCount) {
			if index < settings.FileWriteCount {
				writes = append(writes, func(ctx context.Context) error {
					return self.write(ctx, device, self.general, self.filePath(settings.SecondFileDirectory, device.device), index)
				})
			}
			if index < settings.InboxWriteCount {
				writes = append(writes, func(ctx context.Context) error {
					return self.write(ctx, device, self.inbox, self.filePath(settings.SecondInboxDirectory, device.device), index)
				})
			}
		}
		return writes
	}
	bursts := self.bursts("second", steps, self.general, []string{settings.FileDirectory, settings.SecondFileDirectory}, settings.FileWriteCount)

	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	hold, err := placeEvsConcurrencyHold(ctx, self.world, self.network.NetworkId, []sidecar.Id{self.general.repoId}, settings.HoldLife)
	if err != nil {
		t.Fatalf("the hold: %v", err)
	}
	var group sync.WaitGroup
	var dropped *evsConcurrencyDropped
	var dropErr error
	group.Go(func() {
		dropped, dropErr = self.drop(ctx, hold)
	})
	var erased *evsConcurrencyErased
	var eraseErr error
	group.Go(func() {
		erased, eraseErr = self.erase(ctx)
	})
	if err := RunBursts(ctx, t, "the second phase", bursts, settings.Bursts); err != nil {
		self.world.FailProgress(t, "the second phase", err, bursts)
	}
	group.Wait()
	// what api's budget spent on the hold: its object's create and, once the drop released it, its delete
	self.holdPricedByteCount = hold.pricedByteCount
	FailBurstErrors(t, bursts)
	if dropErr != nil {
		t.Errorf("the drop: %v", dropErr)
	}
	if eraseErr != nil {
		t.Errorf("the erasure: %v", eraseErr)
	}
	if runs := self.world.Block(WorldBlockEvs).RunCount(); runs != 3 {
		t.Errorf("the evs block ran %d times after the restart mid-drop", runs)
	}
	if stands, err := hold.stands(ctx); err != nil || stands {
		t.Errorf("the hold's object stands %t after its release: %v", stands, err)
	}
	self.droppedState = dropped
	self.erasedState = erased
}

// The drop (the file's header): the dry run, run by the stand-in as taskworker would, then the DROP with now while the
// evs block restarts beside it; its listing read while the hold stands and again once it is released.
func (self *evsConcurrencyCase) drop(ctx context.Context, hold *evsConcurrencyHold) (*evsConcurrencyDropped, error) {
	settings := self.settings
	owner := self.devices[0]
	target := self.devices[settings.DropTargetIndex].device
	path := self.filePath(settings.FileDirectory, target)
	repo := owner.client.Repo(self.general.repoId)

	done := make(chan struct{})
	type dryRunOutcome struct {
		count int
		err   error
	}
	dryRuns := make(chan dryRunOutcome, 1)
	go func() {
		count, err := self.worker.runDryRuns(ctx, self.general.repoId, settings.DryRunInterval, done)
		dryRuns <- dryRunOutcome{count: count, err: err}
	}()
	var preview *sidecar.EvsDropPreview
	var previewErr error
	deadline := time.Now().Add(settings.UnavailableTimeout)
	for {
		preview, previewErr = repo.DropHistoryPreview(ctx, &sidecar.EvsDropPreviewArgs{Paths: []string{path}})
		if previewErr == nil || !evsConcurrencyUnavailable(previewErr) || !time.Now().Before(deadline) {
			break
		}
		if err := evsConcurrencyPause(ctx, settings.UnavailableRetryInterval); err != nil {
			previewErr = err
			break
		}
	}
	close(done)
	dryRun := <-dryRuns
	if previewErr != nil {
		return nil, fmt.Errorf("the dry run of %s (the stand-in finished %d, %v): %w", path, dryRun.count, dryRun.err, previewErr)
	}
	if dryRun.err != nil {
		return nil, fmt.Errorf("the stand-in's dry runs: %w", dryRun.err)
	}
	if len(preview.Batches) != 1 || len(preview.Lineages) != 1 || preview.VersionCount != uint64(settings.FileWriteCount) {
		return nil, fmt.Errorf("the dry run of %s lists %d versions in %d batches of %d lineages, not its %d writes in one", path, preview.VersionCount, len(preview.Batches), len(preview.Lineages), settings.FileWriteCount)
	}

	// the DROP, which the block commits before the hook restarts it and loses its answer
	generalId := self.general.repoId.Bytes()
	armed := self.origin.arm(func(push *protocol.EvsPush) bool {
		return bytes.Equal(push.GetRepoId(), generalId) && push.GetFlags()&uint32(protocol.EvsPushFlag_EVS_PUSH_DROP) != 0
	}, func() error {
		started := time.Now()
		err := self.restartEvs()
		self.t.Logf("the evs block restarted mid-drop in %s: %v", time.Since(started).Round(time.Millisecond), err)
		return err
	})
	result, dropErr := repo.DropHistory(ctx, &sidecar.EvsDropHistoryArgs{
		PreviewId: preview.PreviewId, ConfirmPath: sidecar.EvsDropConfirmText(preview), Now: true, ConfirmNow: sidecar.EvsDropConfirmNow,
	})
	select {
	case <-armed.taken:
		self.midDrop = armed
		if armed.chaosErr != nil {
			return nil, fmt.Errorf("the restart mid-drop: %w", armed.chaosErr)
		}
	default:
		self.origin.disarm(armed)
		self.t.Errorf("the restart mid-drop never came: no DROP reached evs's origin")
	}
	if dropErr != nil {
		return nil, fmt.Errorf("the DROP of %s: %w", path, dropErr)
	}
	if len(result.Pushes) != 1 {
		return nil, fmt.Errorf("the drop of %s committed %d pushes, not one", path, len(result.Pushes))
	}
	push := result.Pushes[0]
	commits := self.general.pathCommits()[path]
	if len(commits) != settings.FileWriteCount {
		return nil, fmt.Errorf("%s holds %d acknowledged writes at its drop, not %d", path, len(commits), settings.FileWriteCount)
	}
	last := commits[len(commits)-1]
	self.general.add(&evsConcurrencyCommit{
		path: path, writer: owner.device, sequence: last.sequence, content: last.content, drop: true,
		pushId: push.PushId, version: push.Version, pathVersion: last.pathVersion + 1,
		run: self.world.Block(WorldBlockEvs).RunCount(),
	})
	dropped := &evsConcurrencyDropped{path: path, push: push}

	// the drop lists alike while the hold stands and once it ends (owner decision 184)
	listed := func() ([]*protocol.EvsDropStatus, error) {
		drops, err := repo.Drops(ctx, &sidecar.EvsDropsArgs{KnownPaths: []string{path}})
		if err != nil {
			return nil, err
		}
		statuses := []*protocol.EvsDropStatus{}
		for _, drop := range drops {
			statuses = append(statuses, drop.Status)
		}
		return statuses, nil
	}
	// evs's table takes the DROP in its step 8, off the reply path, and the restart mid-drop drained the block that
	// committed it before its retry was answered, a drain waiting for every answered commit's step 8 (EvsEngine.Drain):
	// so the DROP lists at once, asked again only while evs answers unavailable. The case reads nothing of the repo to
	// make a lost append again, though a push or read that meets the DROP's entry on a head from the cluster does
	// (§11.13's who row), the bursts' among them, so a lost append need not show here
	var held []*protocol.EvsDropStatus
	var listErr error
	listDeadline := time.Now().Add(settings.UnavailableTimeout)
	for {
		statuses, err := listed()
		if err == nil && 0 < len(statuses) {
			held = statuses
			break
		}
		if err == nil {
			listErr = fmt.Errorf("the committed drop of %s does not list once its retry was answered, the restart mid-drop having drained the block that committed it: the listing %v", path, statuses)
			break
		}
		if !evsConcurrencyUnavailable(err) || !time.Now().Before(listDeadline) {
			listErr = fmt.Errorf("the drops while the hold stands: %w", err)
			break
		}
		if err := evsConcurrencyPause(ctx, settings.UnavailableRetryInterval); err != nil {
			listErr = err
			break
		}
	}
	if err := hold.release(ctx); err != nil {
		return dropped, fmt.Errorf("the hold's release: %w", err)
	}
	if listErr != nil {
		return dropped, listErr
	}
	released, err := listed()
	if err != nil {
		return dropped, fmt.Errorf("the drops after the hold: %w", err)
	}
	dropped.listing = released
	if len(held) != 1 || len(released) != 1 || !proto.Equal(held[0], released[0]) {
		return dropped, fmt.Errorf("the drops listed %v while the hold stood and %v after it", held, released)
	}
	status := released[0]
	// its job starts 25 min past its window's end at the earliest (§11.7's action row), so it lists pending
	if sidecar.Id(status.GetDropId()) != push.DropId || status.GetVersion() != push.Version || status.GetState() != protocol.EvsDropState_EVS_DROP_PENDING {
		return dropped, fmt.Errorf("the drop lists as %v, its DROP committing drop %s at %d", status, push.DropId, push.Version)
	}
	return dropped, nil
}

// The erasure (the file's header): an ERASE of every key the target's inbox file's versions name, then a completion
// that names the first of them.
func (self *evsConcurrencyCase) erase(ctx context.Context) (*evsConcurrencyErased, error) {
	settings := self.settings
	owner := self.devices[0]
	target := self.devices[settings.EraseTargetIndex].device
	path := self.filePath(settings.InboxDirectory, target)
	repo := owner.client.Repo(self.inbox.repoId)
	keys, err := repo.SlotKeysOf(ctx, nil, []string{path})
	if err != nil {
		return nil, fmt.Errorf("the slot keys of %s: %w", path, err)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s's versions name no slot key in the group inbox", path)
	}
	erased := &evsConcurrencyErased{path: path, erasureId: sidecar.NewId(), keys: keys}
	note := []byte(settings.ErasureNote)
	if erased.first, err = repo.Erase(ctx, &sidecar.EvsEraseArgs{ErasureId: erased.erasureId, Keys: keys, Note: note}); err != nil {
		return erased, fmt.Errorf("the ERASE of %s: %w", path, err)
	}
	if erased.completion, err = repo.Erase(ctx, &sidecar.EvsEraseArgs{ErasureId: erased.erasureId, Keys: keys[:1], Complete: true, Completion: true, Note: note}); err != nil {
		return erased, fmt.Errorf("the completion ERASE of %s: %w", path, err)
	}
	if erased.first.Version == 0 || erased.completion.Version == 0 {
		return erased, fmt.Errorf("the erasure's ERASE committed at %d and its completion at %d: evs took nothing to erase", erased.first.Version, erased.completion.Version)
	}
	return erased, nil
}

// Every device hears every change of both repos' main: first by hints alone, whose misses are logged, since hints are
// lossy by design (§11.10); then, after each device's fresh read raised its pin to the head, every change once, none
// twice and no gap.
func (self *evsConcurrencyCase) settle() {
	t, settings := self.t, self.settings
	heads := map[*evsConcurrencyRepo]uint64{}
	for _, repo := range self.repos {
		heads[repo] = repo.headVersion()
	}
	ctx, cancel := self.world.StepContext(t.Context(), settings.HintSettleTimeout+settings.SettleTimeout+settings.PhaseTimeout)
	defer cancel()
	var group sync.WaitGroup
	hintMissing := make([]map[string][]uint64, len(self.devices))
	for index, device := range self.devices {
		group.Go(func() {
			hintMissing[index] = device.awaitHeard(ctx, self.repos, heads, settings.HintSettleTimeout)
		})
	}
	group.Wait()
	for index, missing := range hintMissing {
		if 0 < len(missing) {
			t.Logf("%s had not heard %v by hints alone within %s of the last commit", self.devices[index].device.Name, missing, settings.HintSettleTimeout)
		}
	}
	errs := make([][]string, len(self.devices))
	for index, device := range self.devices {
		group.Go(func() {
			for _, repo := range self.repos {
				listing, err := self.listFresh(ctx, device, repo, "")
				if err != nil {
					errs[index] = append(errs[index], fmt.Sprintf("%s's fresh read of %s: %v", device.device.Name, repo.name, err))
				} else if listing.HeadVersion != heads[repo] {
					errs[index] = append(errs[index], fmt.Sprintf("%s reads %s's main at %d, its acknowledged commits reaching %d", device.device.Name, repo.name, listing.HeadVersion, heads[repo]))
				}
			}
			if missing := device.awaitHeard(ctx, self.repos, heads, settings.SettleTimeout); 0 < len(missing) {
				errs[index] = append(errs[index], fmt.Sprintf("%s never heard %v within %s of its fresh read", device.device.Name, missing, settings.SettleTimeout))
			}
		})
	}
	group.Wait()
	for index, device := range self.devices {
		for _, err := range errs[index] {
			t.Errorf("%s", err)
		}
		var gaps []string
		for _, repo := range self.repos {
			// the seed's version is past the subscription's mark on some devices and not others, so only what came after
			// it must be heard, but a repeat of any version is one
			var repeated []string
			_, repeated, gaps = device.heardState(repo, 0, 0)
			if 0 < len(repeated) {
				t.Errorf("%s heard %s's changes more than once: %v", device.device.Name, repo.name, repeated)
			}
		}
		if 0 < len(gaps) {
			t.Errorf("%s heard gaps in place of changes: %v", device.device.Name, gaps)
		}
	}
}

// A fresh List of path on repo's main as device, read again while evs answers unavailable.
func (self *evsConcurrencyCase) listFresh(ctx context.Context, device *evsConcurrencyDevice, repo *evsConcurrencyRepo, path string) (*sidecar.EvsListing, error) {
	deadline := time.Now().Add(self.settings.UnavailableTimeout)
	for {
		listing, err := device.client.Repo(repo.repoId).List(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true})
		if err == nil || !evsConcurrencyUnavailable(err) || !time.Now().Before(deadline) {
			return listing, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(self.settings.UnavailableRetryInterval):
		}
	}
}

// Every device verifies both repos (the file's header), VerifyConcurrency at a time.
func (self *evsConcurrencyCase) verify() {
	t, settings := self.t, self.settings
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	for _, repo := range self.repos {
		repo.stateLock.Lock()
		duplicates := slices.Clone(repo.duplicates)
		repo.stateLock.Unlock()
		for _, duplicate := range duplicates {
			t.Errorf("%s", duplicate)
		}
	}
	tickets := make(chan struct{}, max(1, settings.VerifyConcurrency))
	errs := make([][]error, len(self.devices))
	var group sync.WaitGroup
	for index, device := range self.devices {
		group.Go(func() {
			tickets <- struct{}{}
			defer func() { <-tickets }()
			errs[index] = self.verifyDevice(ctx, device)
		})
	}
	group.Wait()
	for _, deviceErrs := range errs {
		for _, err := range deviceErrs {
			t.Errorf("%v", err)
		}
	}
	if ctx.Err() != nil {
		self.world.FailProgress(t, "the verification", ctx.Err(), nil)
	}
	self.checkErasures(ctx)
}

// The names each directory of repo must list: every directory and file its acknowledged commits wrote, by parent, ""
// the top.
func (self *evsConcurrencyCase) expectedListings(repo *evsConcurrencyRepo) map[string][]string {
	children := map[string]map[string]bool{}
	for _, commit := range repo.commitList() {
		parent := ""
		for _, name := range strings.Split(commit.path, "/") {
			if children[parent] == nil {
				children[parent] = map[string]bool{}
			}
			children[parent][name] = true
			if parent == "" {
				parent = name
			} else {
				parent += "/" + name
			}
		}
	}
	listings := map[string][]string{}
	for parent, names := range children {
		listings[parent] = slices.Sorted(maps.Keys(names))
	}
	return listings
}

// One device's verification of both repos: each directory's listing, each path's history against its acknowledged
// commits, every version's content, or its dropped or erased read, and main's versions from 1 to its head each once.
func (self *evsConcurrencyCase) verifyDevice(ctx context.Context, device *evsConcurrencyDevice) []error {
	errs := []error{}
	name := device.device.Name
	for _, repo := range self.repos {
		head := repo.headVersion()
		for directory, want := range self.expectedListings(repo) {
			listing, err := self.listFresh(ctx, device, repo, directory)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s's listing of %q on %s: %w", name, directory, repo.name, err))
				continue
			}
			names := []string{}
			for _, entry := range listing.Entries {
				names = append(names, string(entry.Name))
			}
			slices.Sort(names)
			sortedWant := slices.Sorted(slices.Values(want))
			if !slices.Equal(names, sortedWant) {
				errs = append(errs, fmt.Errorf("%s lists %q on %s as %v, not %v", name, directory, repo.name, names, sortedWant))
			}
			if listing.HeadVersion != head {
				errs = append(errs, fmt.Errorf("%s lists %q on %s at main's version %d, its acknowledged commits reaching %d", name, directory, repo.name, listing.HeadVersion, head))
			}
		}
		commitVersions := map[uint64]string{}
		for path, commits := range repo.pathCommits() {
			pathErrs := self.verifyPath(ctx, device, repo, path, commits, commitVersions)
			errs = append(errs, pathErrs...)
		}
		for version := uint64(1); version <= head; version += 1 {
			if _, ok := commitVersions[version]; !ok {
				errs = append(errs, fmt.Errorf("%s reads no file version of %s committed at main's version %d", name, repo.name, version))
			}
		}
		if uint64(len(commitVersions)) != head {
			errs = append(errs, fmt.Errorf("%s reads %d commits on %s's main, which stands at %d", name, len(commitVersions), repo.name, head))
		}
	}
	return errs
}

// One path's history on device against its acknowledged commits, oldest first, each version's ref version kept in
// commitVersions; then each version's content, or its dropped or erased read.
func (self *evsConcurrencyCase) verifyPath(ctx context.Context, device *evsConcurrencyDevice, repo *evsConcurrencyRepo, path string, commits []*evsConcurrencyCommit, commitVersions map[uint64]string) []error {
	errs := []error{}
	name := device.device.Name
	repoClient := device.client.Repo(repo.repoId)
	dropped := self.droppedState != nil && repo == self.general && path == self.droppedState.path
	erased := self.erasedState != nil && repo == self.inbox && path == self.erasedState.path
	history, err := repoClient.History(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true})
	if err != nil {
		return append(errs, fmt.Errorf("%s's history of %s on %s: %w", name, path, repo.name, err))
	}
	if !history.Whole || len(history.Versions) != len(commits) {
		return append(errs, fmt.Errorf("%s's history of %s on %s lists %d versions, whole %t, not its %d acknowledged commits", name, path, repo.name, len(history.Versions), history.Whole, len(commits)))
	}
	now := time.Now()
	for index, version := range history.Versions {
		// newest first
		commit := commits[len(commits)-1-index]
		signer := commit.writer.Sidecar().Identity().SidecarId
		switch {
		case version.PushId != commit.pushId || version.Version != commit.pathVersion || version.CommitVersion != commit.version:
			errs = append(errs, fmt.Errorf("%s reads %s on %s at push %s, version %d, ref version %d; its writer was answered push %s, version %d, ref version %d",
				name, path, repo.name, version.PushId, version.Version, version.CommitVersion, commit.pushId, commit.pathVersion, commit.version))
		case version.SignerSidecarId != signer:
			errs = append(errs, fmt.Errorf("%s reads %s's version %d on %s as signed by %s, not %s", name, path, version.Version, repo.name, version.SignerSidecarId, signer))
		case version.Removed || version.Created != (index == len(history.Versions)-1):
			errs = append(errs, fmt.Errorf("%s reads %s's version %d on %s as removed %t, created %t", name, path, version.Version, repo.name, version.Removed, version.Created))
		}
		if other, held := commitVersions[version.CommitVersion]; held {
			errs = append(errs, fmt.Errorf("%s reads %s's main version %d twice, at %s and at %s's version %d", name, repo.name, version.CommitVersion, other, path, version.Version))
		}
		commitVersions[version.CommitVersion] = fmt.Sprintf("%s's version %d", path, version.Version)

		var content bytes.Buffer
		_, readErr := repoClient.ReadVersion(ctx, &sidecar.EvsReadArgs{Path: path, Version: version.Version}, &content)
		switch {
		case dropped && !commit.drop:
			// a version the DROP lists: dropped by now, its history kept (§11.7's states row)
			if !errors.Is(readErr, sidecar.ErrEvsDropped) || version.State != "dropped" || version.DropId != self.droppedState.push.DropId || now.Before(version.DropNotBefore) {
				errs = append(errs, fmt.Errorf("%s reads %s's dropped version %d as state %q of drop %s until %s: %v", name, path, version.Version, version.State, version.DropId, version.DropNotBefore, readErr))
			}
		case erased:
			if !errors.Is(readErr, sidecar.ErrEvsErased) {
				errs = append(errs, fmt.Errorf("%s reads %s's erased version %d on %s as %q: %v", name, path, version.Version, repo.name, content.String(), readErr))
			}
		case readErr != nil:
			errs = append(errs, fmt.Errorf("%s's read of %s's version %d on %s: %w", name, path, version.Version, repo.name, readErr))
		case !bytes.Equal(content.Bytes(), commit.content):
			errs = append(errs, fmt.Errorf("%s reads %s's version %d on %s as %q, not %q", name, path, version.Version, repo.name, content.String(), commit.content))
		}
	}
	if erased {
		var content bytes.Buffer
		if _, err := repoClient.ReadHead(ctx, &sidecar.EvsReadArgs{Path: path}, &content); !errors.Is(err, sidecar.ErrEvsErased) {
			errs = append(errs, fmt.Errorf("%s reads %s's head on %s as %q: %v", name, path, repo.name, content.String(), err))
		}
	}
	return errs
}

// The third phase (the file's header): the exact-head races beside every writer's third files and reads, then every
// device reading every path the case wrote under the races' bases.
func (self *evsConcurrencyCase) racePhase() {
	t, settings := self.t, self.settings
	steps := func(device *evsConcurrencyDevice) []func(ctx context.Context) error {
		writes := []func(ctx context.Context) error{}
		for index := range settings.FileWriteCount {
			writes = append(writes, func(ctx context.Context) error {
				return self.write(ctx, device, self.general, self.filePath(settings.ThirdFileDirectory, device.device), index)
			})
		}
		return writes
	}
	bursts := self.bursts("third", steps, self.general, []string{settings.FileDirectory, settings.SecondFileDirectory}, settings.FileWriteCount)
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	raced := make(chan error, 1)
	go func() {
		raced <- self.race(ctx)
	}()
	if err := RunBursts(ctx, t, "the third phase", bursts, settings.Bursts); err != nil {
		self.world.FailProgress(t, "the third phase", err, bursts)
	}
	FailBurstErrors(t, bursts)
	if err := <-raced; err != nil {
		t.Errorf("the exact-head races: %v", err)
	}
	self.checkRaces()

	// every device reads every acknowledged path's head under the races' bases, each at its newest acknowledged version
	unread := make([][]string, len(self.devices))
	var group sync.WaitGroup
	for index, device := range self.devices {
		group.Go(func() {
			for _, repo := range self.repos {
				for path, commits := range repo.pathCommits() {
					newest := commits[len(commits)-1]
					if self.erasedState != nil && repo == self.inbox && path == self.erasedState.path {
						continue
					}
					var content bytes.Buffer
					info, err := device.client.Repo(repo.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true}, &content)
					switch {
					case err != nil:
						unread[index] = append(unread[index], fmt.Sprintf("%s on %s: %v%s", path, repo.name, err, self.pinAheadNote(device.device, err)))
					case info.Version != newest.pathVersion || !bytes.Equal(content.Bytes(), newest.content):
						unread[index] = append(unread[index], fmt.Sprintf("%s on %s read at version %d as %q, its newest acknowledged %d holding %q", path, repo.name, info.Version, content.String(), newest.pathVersion, newest.content))
					}
				}
			}
		})
	}
	group.Wait()
	for index, device := range self.devices {
		if lines := unread[index]; 0 < len(lines) {
			slices.Sort(lines)
			t.Errorf("%s reads %d paths wrongly or not at all once the races' bases landed, first %s", device.device.Name, len(lines), strings.Join(lines[:min(len(lines), settings.FailurePathCount)], "; "))
		}
	}
}

// The races' rounds: one winner each, every refusal the head having moved, every racer's document landed once, the
// config repo moved one version a round.
func (self *evsConcurrencyCase) checkRaces() {
	t, settings := self.t, self.settings
	self.stateLock.Lock()
	rounds := slices.Clone(self.rounds)
	self.stateLock.Unlock()
	winners := map[string]int{}
	lines := []string{}
	for index, round := range rounds {
		for _, winner := range round.winners {
			winners[winner] += 1
		}
		refused := []string{}
		for racer, err := range round.refusals {
			refused = append(refused, fmt.Sprintf("%s (%v)", racer, err))
		}
		slices.Sort(refused)
		lines = append(lines, fmt.Sprintf("round %d at config version %d: won by %v to %d, refused %v", index, round.configVersion, round.winners, round.wonVersion, refused))
	}
	t.Logf("the exact-head races: %s", strings.Join(lines, "; "))
	if len(rounds) != settings.RacerCount {
		t.Errorf("the exact-head races took %d rounds for %d racers", len(rounds), settings.RacerCount)
	}
	for _, racer := range self.network.Clients[:settings.RacerCount] {
		if winners[racer.Name] != 1 {
			t.Errorf("%s's document landed %d times in the exact-head races", racer.Name, winners[racer.Name])
		}
	}
	for index := 1; index < len(rounds); index += 1 {
		if rounds[index].configVersion != rounds[index-1].wonVersion {
			t.Errorf("round %d previewed at config version %d, the round before it ending at %d", index, rounds[index].configVersion, rounds[index-1].wonVersion)
		}
	}
}

// Both ERASEs read back on every device, the completion ending the erasure, and the eraser's erasure done.
func (self *evsConcurrencyCase) checkErasures(ctx context.Context) {
	t := self.t
	erased := self.erasedState
	if erased == nil || erased.first == nil || erased.completion == nil {
		return
	}
	keySet := func(keys []*sidecar.EvsErasedKey) []string {
		set := []string{}
		for _, key := range keys {
			set = append(set, fmt.Sprintf("%s/%d", key.SlotId, key.KeyIndex))
		}
		slices.Sort(set)
		return set
	}
	owner := self.devices[0].device.Sidecar().Identity().SidecarId
	for _, device := range self.devices {
		erasures, err := device.client.Repo(self.inbox.repoId).ReadErasures(ctx, 0)
		if err != nil {
			t.Errorf("%s's read of the inbox's erasures: %v", device.device.Name, err)
			continue
		}
		if len(erasures) != 2 {
			t.Errorf("%s reads %d ERASEs on the inbox, not the erasure's two", device.device.Name, len(erasures))
			continue
		}
		first, completion := erasures[0], erasures[1]
		switch {
		case first.ErasureId != erased.erasureId || first.Version != erased.first.Version || first.Complete || first.Completion || first.EraserSidecarId != owner ||
			!slices.Equal(keySet(first.Keys), keySet(erased.keys)):
			t.Errorf("%s reads the erasure's ERASE as %+v", device.device.Name, first)
		case completion.ErasureId != erased.erasureId || completion.Version != erased.completion.Version || !completion.Complete || !completion.Completion ||
			completion.EraserSidecarId != owner || !slices.Equal(keySet(completion.Keys), keySet(erased.keys[:1])):
			t.Errorf("%s reads the erasure's completion as %+v", device.device.Name, completion)
		}
	}
	states, err := self.devices[0].client.Repo(self.inbox.repoId).Erasures(ctx)
	if err != nil {
		t.Errorf("the eraser's erasures: %v", err)
		return
	}
	for _, state := range states {
		if state.ErasureId == erased.erasureId {
			if state.State != "done" {
				t.Errorf("the eraser reads the erasure %s", state.State)
			}
			return
		}
	}
	t.Errorf("the eraser lists %d erasures, none of them the case's", len(states))
}

// Each push whose answer a restart lost was answered again at the version the block first committed it at, and its
// writer holds it acknowledged once.
func (self *evsConcurrencyCase) checkChaos() {
	t := self.t
	for _, taken := range []struct {
		name  string
		chaos *evsConcurrencyPushChaos
	}{{"mid-push", self.midPush}, {"mid-drop", self.midDrop}} {
		chaos := taken.chaos
		if chaos == nil {
			// reported where it never came
			continue
		}
		if chaos.status != http.StatusOK || chaos.version == 0 {
			t.Errorf("the push whose answer the restart %s lost was answered %d by the block before it, at version %d", taken.name, chaos.status, chaos.version)
			continue
		}
		found := []*evsConcurrencyCommit{}
		for _, repo := range self.repos {
			for _, commit := range repo.commitList() {
				if commit.pushId == chaos.pushId {
					found = append(found, commit)
				}
			}
		}
		switch {
		case len(found) != 1:
			t.Errorf("the push %s, whose answer the restart %s lost, is acknowledged %d times", chaos.pushId, taken.name, len(found))
		case found[0].version != chaos.version:
			t.Errorf("the push %s committed at version %d before the restart %s and was answered at %d after it", chaos.pushId, chaos.version, taken.name, found[0].version)
		default:
			t.Logf("the restart %s lost the answer to %s's push %s of %s, committed at %d; its retry was answered at %d", taken.name, found[0].writer.Name, chaos.pushId, found[0].path, chaos.version, found[0].version)
		}
	}
}

// No device raised a fork, tamper, regression or gap alarm, nor found a drop invalid, since the last check.
func (self *evsConcurrencyCase) checkAlarms() {
	for _, device := range self.devices {
		alarms := device.alarmList()
		for _, alarm := range alarms[min(device.alarmsChecked, len(alarms)):] {
			self.t.Errorf("%s raised %s", device.device.Name, alarm)
		}
		device.alarmsChecked = len(alarms)
	}
}

// The ledgers at the end (the file's header), the evs block drained by a restart.
func (self *evsConcurrencyCase) checkLedgers(sampler *evsConcurrencyBudgetSampler) {
	t, settings := self.t, self.settings
	if err := self.restartEvs(); err != nil {
		self.world.FailProgress(t, "the evs block's restart for its drain", err, nil)
	}
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	networkId := self.network.NetworkId
	ledgers, err := self.world.ReadLedgers(ctx, time.Time{})
	if err != nil {
		t.Fatalf("the ledgers: %v", err)
	}
	ledger := ledgers.Networks[networkId]
	if ledger == nil {
		t.Fatalf("%s has no quota ledger", self.network.Name)
	}
	counts, err := self.worker.storedByteCounts(ctx)
	// the stand-in's last job has run: its budget closes, writing its last report
	standInClosedAt := time.Now()
	self.worker.close()
	if err != nil {
		t.Errorf("evs.nightly's count: %v", err)
	} else {
		counted := counts[sidecar.Id(networkId)]
		t.Logf("%s's stored bytes: %d in server's quota ledger, %d as evs.nightly counts its objects", self.network.Name, ledger.StoredByteCount, counted)
		switch {
		case counted <= 0:
			t.Errorf("evs.nightly counts %d B stored for %s after its writes", counted, self.network.Name)
		case ledger.StoredByteCount != counted:
			t.Errorf("server's quota ledger stores %d B for %s, and evs.nightly counts %d B of its objects: %+d B the usage batches never reconciled", ledger.StoredByteCount, self.network.Name, counted, ledger.StoredByteCount-counted)
		}
	}
	switch {
	case ledger.Spent || ledger.BalanceByteCount < 0:
		t.Errorf("%s's balance at the end: %+v", self.network.Name, ledger)
	case ledgers.Meters.ByteCount(networkId, sidecar.QuotaMeterEvs) <= 0:
		t.Errorf("%s's evs meter holds nothing after its writes and reads: %s", self.network.Name, ledgers.Meters.Describe(networkId))
	case ledgers.Meters.TotalByteCount(networkId) != ledger.DebitedByteCount():
		t.Errorf("%s's balances were debited %d, its meters: %s", self.network.Name, ledger.DebitedByteCount(), ledgers.Meters.Describe(networkId))
	}
	t.Logf("%s's meters at the end: %s", self.network.Name, ledgers.Meters.Describe(networkId))

	// api's write budget against the objects api wrote and the hold's object, to the byte
	evsConcurrencyCheckApiBudget(t, ctx, self.world, settings.ApiBudgetReadTryCount, settings.ApiObjectPageCount, self.revWritesAtStart, self.holdPricedByteCount)

	// the write budget: every live process holds nothing in flight or queued once the case's work ended, read again a
	// report interval apart while one does, since a report is a level at its time
	var busy []string
	for attempt := 0; attempt < 3; attempt += 1 {
		live, err := self.world.ReadLedgers(ctx, time.Now())
		if err != nil {
			t.Errorf("the live budget reports: %v", err)
			break
		}
		busy = nil
		for clusterId, reports := range live.BudgetReports {
			for _, report := range reports {
				if report.InFlightByteCount != 0 || report.P1QueuedByteCount != 0 {
					busy = append(busy, fmt.Sprintf("%s's process %s: %d B in flight, %d B queued", clusterId, report.ProcessId, report.InFlightByteCount, report.P1QueuedByteCount))
				}
			}
		}
		if len(busy) == 0 {
			break
		}
	}
	for _, line := range busy {
		t.Errorf("the write budget holds %s after the case's work ended", line)
	}
	sampler.stop()
	clusterReports, sampleErrs := sampler.reports()
	for _, err := range sampleErrs {
		t.Errorf("the budget sampler: %v", err)
	}
	// each process's newest report on every cluster, by process id
	reportsByProcess := map[string][]*evsservice.WriteBudgetReport{}
	for _, reports := range clusterReports {
		for processId, report := range reports {
			reportsByProcess[processId] = append(reportsByProcess[processId], report)
		}
	}
	within := func(timeMs int64, window [2]time.Time) bool {
		return !time.UnixMilli(timeMs).Before(window[0].Truncate(time.Millisecond)) && !window[1].Before(time.UnixMilli(timeMs))
	}

	// the stand-in's jobs, to the byte: its newest report, the one its budget wrote as it closed, carries exactly what
	// its count saw each issue and grant take
	standInReports := reportsByProcess[self.worker.processId()]
	standInCount := self.worker.counted.counts()
	standIn := &evsservice.WriteBudgetReport{P0ByteCounts: map[sidecar.WriteP0Kind]int64{}}
	closed := 0 < len(standInReports)
	for _, report := range standInReports {
		for kind, byteCount := range report.P0ByteCounts {
			if byteCount != 0 {
				standIn.P0ByteCounts[kind] += byteCount
			}
		}
		standIn.P1ByteCount += report.P1ByteCount
		standIn.CreditByteCount += report.CreditByteCount
		standIn.UnfundedByteCount += report.UnfundedByteCount + report.FundedByteCount
		closed = closed && !time.UnixMilli(report.TimeMs).Before(standInClosedAt.Truncate(time.Millisecond))
	}
	switch {
	case !closed:
		t.Errorf("the taskworker stand-in's process %s holds %d reports, none written as its budget closed at %s", self.worker.processId(), len(standInReports), standInClosedAt.Format(time.RFC3339Nano))
	case !maps.Equal(standIn.P0ByteCounts, standInCount.P0ByteCounts) || standIn.P1ByteCount != standInCount.P1ByteCount || standIn.CreditByteCount != standInCount.CreditByteCount || standIn.UnfundedByteCount != standInCount.UnfundedByteCount:
		t.Errorf("the taskworker stand-in's process reported P0 %v, P1 %d, credit %d and P2 %d as its budget closed, and its jobs were granted P0 %v, P1 %d, credit %d and P2 %d", standIn.P0ByteCounts, standIn.P1ByteCount, standIn.CreditByteCount, standIn.UnfundedByteCount, standInCount.P0ByteCounts, standInCount.P1ByteCount, standInCount.CreditByteCount, standInCount.UnfundedByteCount)
	default:
		t.Logf("the taskworker stand-in's write budget: P0 %v, P1 %d, credit %d and P2 %d, as its jobs were granted", standIn.P0ByteCounts, standIn.P1ByteCount, standIn.CreditByteCount, standIn.UnfundedByteCount)
	}

	// each evs run's process: for a run a restart started, the block process that started in its window; for the
	// first, which started beside gkm's, the block process whose newest report the restart that ended it wrote. A run a
	// restart stopped wrote its newest report as its budget closed, inside that restart, so the report holds all it
	// spent, and a run that answered commits spent
	blockRuns := self.world.Block(WorldBlockEvs).RunCount() + self.world.Block(WorldBlockGkm).RunCount()
	evsRunCount := self.world.Block(WorldBlockEvs).RunCount()
	runCommits := map[int]int{}
	for _, repo := range self.repos {
		for _, commit := range repo.commitList() {
			runCommits[commit.run] += 1
		}
	}
	self.stateLock.Lock()
	runStarts := maps.Clone(self.runStarts)
	self.stateLock.Unlock()
	for run := 1; run <= evsRunCount; run += 1 {
		start, started := runStarts[run]
		stop, stopped := runStarts[run+1]
		processIds := []string{}
		for processId, reports := range reportsByProcess {
			if processId == ledgers.ApiProcessId || processId == self.worker.processId() {
				continue
			}
			switch {
			case started:
				if within(reports[0].StartMs, start) {
					processIds = append(processIds, processId)
				}
			case stopped:
				if within(reports[0].TimeMs, stop) {
					processIds = append(processIds, processId)
				}
			}
		}
		slices.Sort(processIds)
		var reports []*evsservice.WriteBudgetReport
		if len(processIds) == 1 {
			reports = reportsByProcess[processIds[0]]
		}
		total := int64(0)
		// the time of a report of a stopped run written outside the restart that stopped it, 0 for none
		outsideMs := int64(0)
		for _, report := range reports {
			total += evsConcurrencyReportByteCount(report)
			if stopped && !within(report.TimeMs, stop) {
				outsideMs = report.TimeMs
			}
		}
		t.Logf("evs's run %d answered %d acknowledged commits, its process's newest report %d B over %d reports", run, runCommits[run], total, len(reports))
		switch {
		case started && len(processIds) != 1:
			t.Errorf("evs's run %d, started between %s and %s, matches %d processes' reports: %v", run, start[0].Format(time.RFC3339Nano), start[1].Format(time.RFC3339Nano), len(processIds), processIds)
		case !started && len(processIds) != 1:
			t.Errorf("evs's run %d, which the restart between %s and %s stopped, matches %d block processes whose newest report that restart wrote, not one written as its budget closed: %v", run, stop[0].Format(time.RFC3339Nano), stop[1].Format(time.RFC3339Nano), len(processIds), processIds)
		case outsideMs != 0:
			t.Errorf("evs's run %d, which the restart between %s and %s stopped, wrote its newest report at %s, not as its budget closed in that restart: what it spent after that report never reached the ledger", run, stop[0].Format(time.RFC3339Nano), stop[1].Format(time.RFC3339Nano), time.UnixMilli(outsideMs).Format(time.RFC3339Nano))
		case 0 < runCommits[run] && total == 0:
			t.Errorf("evs's run %d answered %d acknowledged commits and its write budget reported no spend", run, runCommits[run])
		}
	}
	for clusterId, processReports := range clusterReports {
		lines := []string{}
		blockProcesses := 0
		for processId, report := range processReports {
			role := "a block's"
			switch processId {
			case ledgers.ApiProcessId:
				role = "api's"
			case self.worker.processId():
				role = "the taskworker stand-in's"
			default:
				blockProcesses += 1
			}
			lines = append(lines, fmt.Sprintf("%s %s: %d B (p0 %v, p1 %d, p2 %d unfunded and %d funded)", role, processId, evsConcurrencyReportByteCount(report), report.P0ByteCounts, report.P1ByteCount, report.UnfundedByteCount, report.FundedByteCount))
		}
		slices.Sort(lines)
		t.Logf("%s's write budget, each process's newest report: %s", clusterId, strings.Join(lines, "; "))
		if blockProcesses < blockRuns {
			t.Errorf("%s's write budget holds the reports of %d block processes, fewer than the %d runs of evs and gkm that spent on it", clusterId, blockProcesses, blockRuns)
		}
	}
	if len(clusterReports) == 0 {
		t.Errorf("the budget sampler read no metadata cluster's reports")
	}
}

// What the case says of the device whose sidecar is sidecarId when gkm refuses it (the recorder at gkm's origin): the
// role its config follower's state gives it on each repo the refused token scopes, and the role the newest state any
// device follows gives it there, each state at its log position. Called on gkm's request goroutines once setUp is done.
func (self *evsConcurrencyCase) describeGkmCaller(sidecarId sidecar.Id, scopes []string) string {
	name := fmt.Sprintf("sidecar %s, no device of the case", sidecarId)
	var own *sidecar.ConfigState
	var newest *sidecar.ConfigState
	newestName := ""
	for _, device := range self.devices {
		follower := device.device.Follower()
		if follower == nil || follower.Client() == nil {
			continue
		}
		state := follower.Client().State()
		if state == nil {
			continue
		}
		if device.device.Sidecar().Identity().SidecarId == sidecarId {
			name, own = device.device.Name, state
		}
		if newest == nil || newest.Pin().GetVersion() < state.Pin().GetVersion() {
			newest, newestName = state, device.device.Name
		}
	}
	now := time.Now()
	roles := func(state *sidecar.ConfigState) string {
		acl, err := state.Acl()
		if err != nil {
			return fmt.Sprintf("no rights (%v)", err)
		}
		parts := []string{}
		for _, scope := range scopes {
			repoId, ok := evsConcurrencyScopeRepoId(scope)
			if !ok {
				continue
			}
			rights := acl.EvsAccess(sidecar.AclSidecarSubject(sidecarId), repoId, sidecar.AclTokenAudienceGkm, now).Rights
			parts = append(parts, self.repoName(repoId)+":"+evsConcurrencyRole(rights))
		}
		return strings.Join(parts, " ")
	}
	if own == nil {
		return name
	}
	described := fmt.Sprintf("%s, whose follower's state at log position %d gives it %s", name, own.Pin().GetVersion(), roles(own))
	if newest != own {
		described += fmt.Sprintf(", where %s's, the newest at %d, gives it %s", newestName, newest.Pin().GetVersion(), roles(newest))
	}
	return described
}

// Logs each refusal gkm answered since the last call, with where it fell among the race rounds' saves, what the
// refused token held and what the case said of its caller then.
func (self *evsConcurrencyCase) logGkmRefusals(phase string) {
	refusals := self.gkmOrigin.takeRefusals()
	self.stateLock.Lock()
	rounds := slices.Clone(self.rounds)
	self.stateLock.Unlock()
	for _, refusal := range refusals {
		after := ""
		for index, round := range rounds {
			if !round.savedAt.IsZero() && round.savedAt.Before(refusal.time) {
				after = fmt.Sprintf(", %s after round %d's save took the config repo to version %d", refusal.time.Sub(round.savedAt).Round(time.Millisecond), index, round.wonVersion)
			}
		}
		token := "a bearer with no claims that parse"
		if refusal.claimed {
			names := []string{}
			for _, scope := range refusal.scopes {
				if repoId, ok := evsConcurrencyScopeRepoId(scope); ok {
					scope = self.repoName(repoId) + ":" + scope[strings.LastIndex(scope, ":")+1:]
				}
				names = append(names, scope)
			}
			token = fmt.Sprintf("a token from log position %d holding %s", refusal.projectionVersion, strings.Join(names, " "))
		}
		caller := refusal.device
		if caller == "" {
			caller = fmt.Sprintf("sidecar %s", refusal.sidecarId)
		}
		self.t.Logf("%s: gkm answered %s %d at %s%s: %s, presented by %s", phase, refusal.route, refusal.status, refusal.time.Format("15:04:05.000"), after, token, caller)
	}
}

// A repo's name in the case's lines: general, inbox or config, else its id.
func (self *evsConcurrencyCase) repoName(repoId sidecar.Id) string {
	if name, ok := self.repoNames[repoId]; ok {
		return name
	}
	return repoId.String()
}

// The repo a repo:<id>:<role> scope names.
func evsConcurrencyScopeRepoId(scope string) (sidecar.Id, bool) {
	rest, ok := strings.CutPrefix(scope, "repo:")
	if !ok {
		return sidecar.Id{}, false
	}
	idText, _, ok := strings.Cut(rest, ":")
	if !ok {
		return sidecar.Id{}, false
	}
	repoId, err := sidecar.ParseId(idText)
	if err != nil {
		return sidecar.Id{}, false
	}
	return repoId, true
}

// The strongest role rights carry, as a scope names it.
func evsConcurrencyRole(rights sidecar.AclEvsRights) string {
	switch {
	case rights.Has(sidecar.AclEvsAdmin):
		return "admin"
	case rights.Has(sidecar.AclEvsWrite):
		return "rw"
	case rights.Has(sidecar.AclEvsRead):
		return "r"
	case rights.Has(sidecar.AclEvsDeposit):
		return "deposit"
	}
	return "none"
}

// What the case did, for a failure's message.
func (self *evsConcurrencyCase) logState() {
	for _, repo := range self.repos {
		self.t.Logf("%s: %d acknowledged commits, main at %d", repo.name, len(repo.commitList()), repo.headVersion())
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.t.Logf("reads answered unavailable and read again: %d; race rounds: %d", self.unavailableReads, len(self.rounds))
}
