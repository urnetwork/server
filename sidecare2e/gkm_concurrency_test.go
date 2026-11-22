//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Group keys under membership churn, rotations under contention, late and offline devices, and chaos (the end-to-end
// plan's e2e-gkm-concurrency; the owner's end-to-end tests of 2026-11-21): N sidecars of one network (ClientCount, 8
// by default) on the world's real keys, each device's key client its own at gkm's block in KeyDelivery mode (no MLS,
// owner decision 610), with §10.6's reconciler running beside the blocks as taskworker runs it, so a shrink or a
// growth of a repo's key holders hints pending to its writers (gkm_concurrency_harness_test.go).
//
// The network saves an ACL document with grant 1 alone, so no rule gives a member device evs access (§8.5's implicit
// document gives every member device admin on every general repo) and a repo's readers are its admin and its grants.
// Its owner's device creates the repos:
//
//   - shared: its member granted writer, so every device of the network, a joiner included, holds its key;
//   - partials, PartialRepoCount of them: each granted to PartialWriterCount devices as writers, by sidecar;
//   - and the network's config repo, which every admitted sidecar reads.
//
// The phases, each with random pauses between its bursts' steps (WorldBurstSettings):
//
//   - growth and contention: JoinCount new devices enroll and are admitted, one after another; each partial repo is
//     granted to GrowGrantCount more devices as readers; RotatorCount writers rotate the shared repo at once,
//     RotationRoundCount rounds, each round one new generation that every rotator returns; and every writer writes
//     every repo it writes. Nothing here shrinks a holder set, so nothing here may rotate with cause REMOVE, and an
//     Add keeps the generation (§10.5: a joiner takes a LATE delivery, never a rotation)
//   - shrink and chaos: RevokeCount devices are revoked (SidecarRevoke, reason removed) and each partial repo's
//     grant to ShrinkRevokeCount writers is revoked (RepoRevoke), while writers write and race rotations of a partial
//     repo; gkm restarts on its stores twice, once with the next claim of a generation answered and its answer lost
//     (mid-rotation) and once with the next deliveries batch lost unsent (mid-delivery). Each shrink must rotate with
//     cause REMOVE at a pin covering it (§10.5's event table), and no revoked or removed device may take a delivery of
//     that generation or a later one
//   - a late device: one device is cut off from gkm (its requests answered as the lb answers with no upstream) while
//     the shared repo rotates OfflineRotationCount times and a partial repo is granted to it, every delivery a holder
//     makes to it in that repo lost meanwhile; once back it catches up: it reads content sealed under the shared repo's
//     newest generation, which takes the generations it missed (STAGED to it, its ReaderKey standing), and asks late
//     for the partial repo's, which a writer delivers (§10.6's late wraps); its granting device, the owner, which
//     signed its admission and the grant and so would deliver at once (DESIGN-DETAILS 6.2.4's may(c)), is away from
//     gkm meanwhile, so another writer answers, after its LateDeliverJitter
//   - a device dies mid-delivery: one writer grants another device a partial repo it holds, and is closed while its
//     own LATE delivery to that device, the granting device's, is in flight, the request lost; the recipient takes
//     the key from another holder at its late ask
//
// What must hold, strictly:
//
//   - within a bound, every member device the case runs holds the same newest generation of every repo it reads, the
//     generation gkm took last: the bound is §8.9's hint latency (gate and client), the writers' PendingCommitWait, a
//     holder's LateDeliverJitter and the requests' own time; where a chaos may have lost a hint, the devices' resync
//     interval too, since a lost hint costs at most that (§6.8). gkm's late-ask widening (LateAskWidenAge, 15 min) is
//     past every bound here, so every late ask must be answered by a writer, never by the widened hint
//   - gkm took one commitment for each generation of each group, and every device holds that one: no fork
//   - each shrink rotated: a generation of cause REMOVE at a pin at or past the shrink's entry in every repo the
//     removed holder read
//   - every delivery verifies under its signer's admitted identity key, is sealed to a ReaderKey its recipient
//     published under its own identity key, and names a holder of the repo at the delivery's state (a STAGED one at its
//     generation's, a LATE one at its own), so no revoked or removed device, and no device that never read the repo,
//     takes a delivery of a generation past its removal
//   - no device holds a key of a repo it never read, and none of a generation past its removal's rotation, as its own
//     keychain shows
//   - no device raises a fork, tamper or regression alarm (head_alarm, a gkm refusal, evs_alarm, evs_gap, key_changed)
//   - after convergence, every member's write under the newest generation reads on every other member through evs
//   - every gkm 401 and 403 is accounted for by the recorder at gkm's origin: a revoked caller, a caller whose grant
//     on that repo ended, a caller whose token holds a role its state changed since (QD-1179's stale stronger scope,
//     reported with the token's log position and scopes), or gkm's gate behind an entry the caller already holds;
//     anything else is the case's failure
//   - every chaos took its request, and the claim whose answer was lost stands once, at the commitment its rotator
//     sent, never a second generation for it
//
// Not here, each for the reason given: EvsShares, since the world's networks are personal and admit only their owner
// member's untagged devices, every one a consented reader of every owner-bound repo, so a share names no new holder;
// and a restart of a device keeping its keychain, since sidecart3 starts a T3Sidecar on a memory instance of its own
// and offers no restart on it, so a device here can only die. A re-form under contention and a personal network's
// new second device are cases of their own below (TestE2eGkmConcurrencyReform, TestE2eGkmConcurrencySecondDevice).
//
// A failure from a product bug stays failing; the case reports it with its evidence and runs on, so one finding does
// not hide the next. As it landed (2026-11-22) it passed: every shrink rotated with cause REMOVE within about 1.4 s of
// its entry's sync, every holder converged within a second of each phase's last change, the late device's ask was
// answered by a writer that was not its granting device about half a second after it came back, and every gkm
// refusal was a revoked caller's or one whose grant had ended. Each phase has PhaseTimeout and dumps the world's
// progress when it stalls; the case has the conventions' 10 minutes. Run it through the build's wrapper:
// -tags 'sidecar_e2e sidecar_pending sidecar_localstack'.

package sidecare2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/rand/v2"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/sidecart3"
)

// What the case runs with (QD-702): every count, pause and bound it uses.
type gkmConcurrencySettings struct {
	// N, the sidecars of the one network at its start: at least gkmConcurrencyMinClientCount, so the late device,
	// the dying writer, its recipient and the revoked devices are all apart.
	ClientCount int
	// The devices that enroll and are admitted during the growth phase.
	JoinCount int
	// The devices revoked during the shrink phase, the last of the network's first N.
	RevokeCount int
	// The partial repos, each read by its admin and its grants alone.
	PartialRepoCount int
	// The devices each partial repo is granted to as writers at setup.
	PartialWriterCount int
	// The devices each partial repo is granted to as readers during the growth phase.
	GrowGrantCount int
	// The writers whose grant on each partial repo is revoked during the shrink phase.
	ShrinkRevokeCount int
	// The writers that rotate one repo at once, and how many rounds they race.
	RotatorCount       int
	RotationRoundCount int
	// The writes each writer makes to each repo it writes, in each of the first two phases.
	WriteCount int
	// The shared repo's rotations while the late device is cut off from gkm.
	OfflineRotationCount int
	// The network's ACL document: grant 1 alone, so no rule gives a member device evs access.
	AclPolicy string
	// §7.9's config_bases_per_min for the case: the document's save publishes a base, which the Community column's 1
	// a minute would hold a minute.
	ConfigBasesPerMinute int64
	// Each device's gkm timings (GkmSettings): a writer that is not the granting device waits PendingCommitDelay plus
	// up to PendingCommitJitter at a pending hint; a holder answers a late ask's hint after up to LateDeliverJitter;
	// and every hint handler runs again each ResyncInterval, which bounds what a lost hint costs.
	PendingCommitDelay  time.Duration
	PendingCommitJitter time.Duration
	LateDeliverJitter   time.Duration
	ResyncInterval      time.Duration
	// The reconciler's repeat of a pending hint while a repo's gate stays closed, and of its dropped-leaf hint.
	ReconcilerHintInterval time.Duration
	// §8.9's targets: a statement reaches a server gate within GateLatency and is applied on a connected client within
	// ClientApplyLatency (its p99).
	GateLatency        time.Duration
	ClientApplyLatency time.Duration
	// What the requests of a rotation or a delivery take beside the waits above: staging, the claim, the hint, the
	// holders' fetches.
	WorkAllowance time.Duration
	// gkm's own widening of a late ask's hint to other holders (GkmServiceSettings.LateAskWidenAge): every bound must
	// stay below it, so a late ask here is answered by a writer at once.
	LateAskWidenAge time.Duration
	// The bursts' pauses and seed.
	Bursts *WorldBurstSettings
	// Each phase's bound, and the setup's.
	PhaseTimeout time.Duration
	// How often a wait asks a device's key client again: a key client's learning has no hook a test reads.
	PollInterval time.Duration
	// How long a statement's sync on the gates and the devices may take.
	SyncTimeout time.Duration
	// How long an armed chaos may wait for the request it takes.
	ChaosTimeout time.Duration
	// How many devices read at once at the end.
	VerifyConcurrency int
}

// The fewest sidecars the case's roles fit in: the owner, the late device, the dying writer, its recipient, two
// writers more and the revoked devices.
const gkmConcurrencyMinClientCount = 6

func defaultGkmConcurrencySettings() *gkmConcurrencySettings {
	return &gkmConcurrencySettings{
		ClientCount:          8,
		JoinCount:            2,
		RevokeCount:          2,
		PartialRepoCount:     2,
		PartialWriterCount:   3,
		GrowGrantCount:       2,
		ShrinkRevokeCount:    1,
		RotatorCount:         4,
		RotationRoundCount:   3,
		WriteCount:           2,
		OfflineRotationCount: 3,
		AclPolicy: worldAclPolicy(
			`{"src": ["autogroup:member", "autogroup:untagged"], "dst": ["autogroup:self"], "via": ["autogroup:self"], "allow": ["dial", "route"]}`,
		),
		ConfigBasesPerMinute:   6000,
		PendingCommitDelay:     3 * time.Second,
		PendingCommitJitter:    1 * time.Second,
		LateDeliverJitter:      2 * time.Second,
		ResyncInterval:         45 * time.Second,
		ReconcilerHintInterval: 10 * time.Second,
		GateLatency:            5 * time.Second,
		ClientApplyLatency:     5 * time.Second,
		WorkAllowance:          20 * time.Second,
		LateAskWidenAge:        gkmservice.DefaultGkmServiceSettings().LateAskWidenAge,
		Bursts:                 DefaultWorldBurstSettings(),
		PhaseTimeout:           4 * time.Minute,
		PollInterval:           500 * time.Millisecond,
		SyncTimeout:            60 * time.Second,
		ChaosTimeout:           90 * time.Second,
		VerifyConcurrency:      4,
	}
}

// The bound within which every holder holds the newest generation (the file's header): §8.9's latencies, the writers'
// PendingCommitWait, a holder's late jitter and the requests' own time, and, where a chaos may have lost a hint, the
// resync interval.
func (self *gkmConcurrencySettings) convergenceBound(lostHints bool) time.Duration {
	bound := self.GateLatency + self.ClientApplyLatency + self.PendingCommitDelay + self.PendingCommitJitter + self.LateDeliverJitter + self.WorkAllowance
	if lostHints {
		bound += self.ResyncInterval
	}
	return bound
}

// Each device's gkm settings past the defaults: the case's timings.
func (self *gkmConcurrencySettings) sidecarSettings(name string, sidecarSettings *sidecar.SidecarSettings) {
	sidecarSettings.Gkm.PendingCommitDelay = self.PendingCommitDelay
	sidecarSettings.Gkm.PendingCommitJitter = self.PendingCommitJitter
	sidecarSettings.Gkm.LateDeliverJitter = self.LateDeliverJitter
	sidecarSettings.Gkm.ResyncInterval = self.ResyncInterval
}

// One device of the case. Safe for concurrent use.
type gkmConcurrencyDevice struct {
	device      *sidecart3.T3Device
	name        string
	sidecarId   sidecar.Id
	identityKey []byte
	// admitted during the growth phase
	joined bool

	stateLock sync.Mutex
	// its admission's position
	admittedAt uint64
	// its revocation's position, 0 for none, and when its filing began: a refusal of its token from then is its own
	revokedAt   uint64
	revokeFiled time.Time
	// closed by the case: it runs no more
	dead bool
	// cut off from gkm now
	cutOff bool
	// every alarm it raised, and the events of its own revocation
	alarms  []string
	removes []func()
}

// Whether the device still runs and is not revoked: what follows the log, writes and is asked for keys.
func (self *gkmConcurrencyDevice) live() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.dead && self.revokedAt == 0 && self.revokeFiled.IsZero()
}

// Whether the device is live and not cut off from gkm.
func (self *gkmConcurrencyDevice) reachable() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return !self.dead && self.revokedAt == 0 && self.revokeFiled.IsZero() && !self.cutOff
}

// Its revocation's position, and whether its filing began.
func (self *gkmConcurrencyDevice) revocation() (uint64, time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.revokedAt, self.revokeFiled
}

// Its admission's position.
func (self *gkmConcurrencyDevice) admission() uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.admittedAt
}

// Records an alarm the case refuses.
func (self *gkmConcurrencyDevice) event(event sidecar.SidecarEvent) {
	line := ""
	switch event.Kind {
	case sidecar.SidecarEventHeadAlarm:
		line = fmt.Sprintf("head_alarm %s", event.HeadAlarm)
	case sidecar.SidecarEventRefused:
		if event.GroupId == nil {
			line = "refused: its generation"
		} else {
			line = fmt.Sprintf("refused: gkm group %s", *event.GroupId)
		}
	case sidecar.SidecarEventEvsAlarm:
		line = fmt.Sprintf("evs_alarm %s (%s) at version %d", event.EvsAlarm, event.EvsAlarmDetail, event.Version)
	case sidecar.SidecarEventEvsGap:
		line = fmt.Sprintf("evs_gap verified since %s", event.VerifiedSince)
	case sidecar.SidecarEventKeyChanged:
		line = "key_changed"
	}
	if line == "" {
		return
	}
	if event.RepoId != nil {
		line += fmt.Sprintf(" on repo %s", *event.RepoId)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	line = fmt.Sprintf("%s at %s", line, time.Now().Format("15:04:05.000"))
	if self.revokedAt != 0 || !self.revokeFiled.IsZero() {
		line += " (after its revocation)"
	}
	self.alarms = append(self.alarms, line)
}

// One change of a repo's holders, or of a device's standing, as the case filed it.
type gkmConcurrencyEvent struct {
	// grant, revoke-grant, revoke-device or admit
	kind string
	// the repo a grant or its revocation names; nil for a device's event
	repo    *gkmConcurrencyRepo
	subject *gkmConcurrencyDevice
	role    protocol.RepoGrantRole
	// its entry's position
	position uint64
	// when its filing began, and when the gates and the devices held it
	filed  time.Time
	synced time.Time
}

// A shrink: an event that drops a holder, which must rotate (§10.5's event table).
func (self *gkmConcurrencyEvent) shrinks() bool {
	return self.kind == "revoke-grant" || self.kind == "revoke-device"
}

// One repo of the case. Safe for concurrent use.
type gkmConcurrencyRepo struct {
	name   string
	repoId sidecar.Id
	config bool
	// the device that created it, its admin and so a holder; nil for the config repo
	admin *gkmConcurrencyDevice

	stateLock sync.Mutex
	// the member's grant's position, the shared repo's: every member device reads it from there
	memberGrantAt uint64
	// its grants to devices and their revocations, in position order
	grants []*gkmConcurrencyEvent
}

// The repo's grants and revocations, in position order, each a copy as it stands now.
func (self *gkmConcurrencyRepo) grantList() []*gkmConcurrencyEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	grants := []*gkmConcurrencyEvent{}
	for _, grant := range self.grants {
		copied := *grant
		grants = append(grants, &copied)
	}
	return grants
}

// Keeps a grant or its revocation.
func (self *gkmConcurrencyRepo) addGrant(event *gkmConcurrencyEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.grants = append(self.grants, event)
}

// Orders the grants by position once one took its position.
func (self *gkmConcurrencyRepo) resort() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	sort.SliceStable(self.grants, func(i int, j int) bool { return self.grants[i].position < self.grants[j].position })
}

// The case: the world, its devices and repos, and what each phase found.
type gkmConcurrencyCase struct {
	t          testing.TB
	settings   *gkmConcurrencySettings
	world      *World
	network    *WorldNetwork
	origin     *gkmConcurrencyOrigin
	reconciler *gkmConcurrencyReconciler
	owner      *gkmConcurrencyDevice
	config     *gkmConcurrencyRepo
	shared     *gkmConcurrencyRepo
	partials   []*gkmConcurrencyRepo
	// the repos of the cases beside the churn: every member device reads each from its memberGrantAt
	others []*gkmConcurrencyRepo
	// the roles the later phases give: the late device, the writer that dies mid-delivery and its recipient
	late      *gkmConcurrencyDevice
	dying     *gkmConcurrencyDevice
	recipient *gkmConcurrencyDevice

	stateLock sync.Mutex
	devices   []*gkmConcurrencyDevice
	events    []*gkmConcurrencyEvent
	// the chaos the shrink phase armed, nil where it was not armed
	midRotation *gkmConcurrencyChaos
	midDelivery *gkmConcurrencyChaos
	// how many writes each phase made, each refusal the case accounted for and each write tried again, by its reason
	writeCount      int
	refusalAccounts map[string]int
	retryCounts     map[string]int
	// every acknowledged write
	written []*gkmConcurrencyWrite
}

// See the file's header.
func TestE2eGkmConcurrency(t *testing.T) {
	settings := defaultGkmConcurrencySettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		runGkmConcurrency(t, settings)
	})
}

// The case on settings: see the file's header.
func runGkmConcurrency(t testing.TB, settings *gkmConcurrencySettings) {
	switch {
	case settings.ClientCount < gkmConcurrencyMinClientCount:
		t.Fatalf("a case of %d sidecars has no room for its roles; it needs %d", settings.ClientCount, gkmConcurrencyMinClientCount)
	case settings.ClientCount-1-settings.RevokeCount < 4:
		t.Fatalf("%d revocations of %d sidecars leave too few writers", settings.RevokeCount, settings.ClientCount)
	case settings.RotatorCount < 2:
		t.Fatalf("%d rotators race nothing", settings.RotatorCount)
	case settings.LateAskWidenAge <= settings.convergenceBound(true):
		t.Fatalf("the bound %s reaches gkm's widening %s, so a late ask could wait on it", settings.convergenceBound(true), settings.LateAskWidenAge)
	}
	worldSettings := DefaultWorldSettings()
	worldSettings.NetworkCount = 1
	worldSettings.ClientsPerNetwork = settings.ClientCount
	t3Settings := sidecart3.DefaultT3WorldSettings()
	limits := *t3Settings.SidecarLimits
	limits.LimitKeyValues = maps.Clone(limits.LimitKeyValues)
	limits.LimitKeyValues["config_bases_per_min"] = settings.ConfigBasesPerMinute
	t3Settings.SidecarLimits = &limits
	worldSettings.T3 = t3Settings
	world := NewWorld(t, worldSettings)
	defer world.Close()
	// the recorder before any device, so every ReaderKey, claim and delivery passes it
	origin, err := installGkmConcurrencyOrigin(world)
	if err != nil {
		t.Fatal(err)
	}
	reconcilerSettings := gkmservice.DefaultGkmReconcilerSettings()
	reconcilerSettings.PendingHintInterval = settings.ReconcilerHintInterval
	reconcilerSettings.DroppedLeafHintInterval = settings.ReconcilerHintInterval
	reconciler, err := startGkmConcurrencyReconciler(world, reconcilerSettings)
	if err != nil {
		t.Fatal(err)
	}
	defer reconciler.close()
	self := &gkmConcurrencyCase{t: t, settings: settings, world: world, origin: origin, reconciler: reconciler, refusalAccounts: map[string]int{}, retryCounts: map[string]int{}}
	origin.setDescribe(self.describeCaller)
	defer func() {
		for _, device := range self.deviceList() {
			for _, remove := range device.removes {
				remove()
			}
		}
	}()

	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	if err := self.setUp(ctx); err != nil {
		cancel()
		world.FailProgress(t, "the setup", err, nil)
	}
	cancel()
	self.accountRefusals("the setup")

	self.growthPhase()
	self.accountRefusals("the growth phase")
	self.shrinkPhase()
	self.accountRefusals("the shrink phase")
	self.latePhase()
	self.accountRefusals("the late device's phase")
	self.dyingPhase()
	self.accountRefusals("the dying writer's phase")
	self.finalPhase()
	self.accountRefusals("the final phase")
	self.checkGenerations()
	self.checkDeliveries()
	self.checkShrinksRotated()
	self.checkKeychains()
	self.checkAlarms()
	self.checkChaos()
	self.logSummary()
}

// Every device, in the order the case added them.
func (self *gkmConcurrencyCase) deviceList() []*gkmConcurrencyDevice {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.devices)
}

// The devices that still run and are not revoked, whose followers a statement's sync waits on.
func (self *gkmConcurrencyCase) following() []*sidecart3.T3Device {
	devices := []*sidecart3.T3Device{}
	for _, device := range self.deviceList() {
		if device.live() {
			devices = append(devices, device.device)
		}
	}
	return devices
}

// Every repo of the case: the config repo, the shared one, the partials and any other a case adds.
func (self *gkmConcurrencyCase) repoList() []*gkmConcurrencyRepo {
	repos := []*gkmConcurrencyRepo{}
	for _, repo := range slices.Concat([]*gkmConcurrencyRepo{self.config, self.shared}, self.partials, self.others) {
		if repo != nil {
			repos = append(repos, repo)
		}
	}
	return repos
}

// The case's device a sidecar id names, nil for none.
func (self *gkmConcurrencyCase) deviceOf(sidecarId sidecar.Id) *gkmConcurrencyDevice {
	for _, device := range self.deviceList() {
		if device.sidecarId == sidecarId {
			return device
		}
	}
	return nil
}

// The admitted identity key of a sidecar the case runs, which every signature of the case verifies under.
func (self *gkmConcurrencyCase) identityKey(sidecarId sidecar.Id) ([]byte, error) {
	if device := self.deviceOf(sidecarId); device != nil {
		return device.identityKey, nil
	}
	return nil, fmt.Errorf("sidecar %s is no device of the case", sidecarId)
}

// Adds a device the network admitted, its admission's position read from the owner's verified state, with its alarm
// callback.
func (self *gkmConcurrencyCase) addDevice(device *sidecart3.T3Device, joined bool) (*gkmConcurrencyDevice, error) {
	identity := device.Sidecar().Identity()
	if identity == nil {
		return nil, fmt.Errorf("%s has no identity", device.Name)
	}
	added := &gkmConcurrencyDevice{device: device, name: device.Name, sidecarId: identity.SidecarId, identityKey: bytes.Clone(identity.IdentityPublicKey), joined: joined}
	if follower := device.Follower(); follower != nil && follower.Client() != nil && follower.Client().State() != nil {
		if row := follower.Client().State().SidecarRow(identity.SidecarId); row != nil {
			added.admittedAt = max(1, row.GetAdmittedAt())
		}
	}
	if added.admittedAt == 0 {
		return nil, fmt.Errorf("%s's own state holds no admission of it", device.Name)
	}
	added.removes = append(added.removes, device.Sidecar().AddEventCallback(added.event))
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.devices = append(self.devices, added)
	return added, nil
}

// Keeps an event the case filed.
func (self *gkmConcurrencyCase) addEvent(event *gkmConcurrencyEvent) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.events = append(self.events, event)
}

// Every event the case filed or is filing, in position order, those with no position yet first, each a copy as it
// stands now.
func (self *gkmConcurrencyCase) eventList() []*gkmConcurrencyEvent {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	events := []*gkmConcurrencyEvent{}
	for _, event := range self.events {
		copied := *event
		events = append(events, &copied)
	}
	sort.SliceStable(events, func(i int, j int) bool { return events[i].position < events[j].position })
	return events
}

// Whether device reads repo at position, by the case's own record of what it filed: an admitted, unrevoked device
// reads the config repo, a repo it created, the shared repo from the member's grant on, and a partial repo while its
// newest grant there at or before position is no revocation.
func (self *gkmConcurrencyCase) holdsAt(repo *gkmConcurrencyRepo, device *gkmConcurrencyDevice, position uint64) bool {
	admittedAt := device.admission()
	revokedAt, _ := device.revocation()
	if admittedAt == 0 || position < admittedAt || revokedAt != 0 && revokedAt <= position {
		return false
	}
	if repo.config || repo.admin == device {
		return true
	}
	repo.stateLock.Lock()
	memberGrantAt := repo.memberGrantAt
	repo.stateLock.Unlock()
	if memberGrantAt != 0 && memberGrantAt <= position {
		return true
	}
	held := false
	for _, grant := range repo.grantList() {
		if grant.position == 0 {
			continue
		}
		if position < grant.position {
			break
		}
		if grant.subject == device {
			held = grant.kind == "grant"
		}
	}
	return held
}

// Whether device writes repo at position: the shared repo's every holder, a partial repo's admin and writers.
func (self *gkmConcurrencyCase) writesAt(repo *gkmConcurrencyRepo, device *gkmConcurrencyDevice, position uint64) bool {
	if repo.config || !self.holdsAt(repo, device, position) {
		return false
	}
	if repo.admin == device || repo == self.shared {
		return true
	}
	role := protocol.RepoGrantRole_REPO_GRANT_ROLE_UNSPECIFIED
	for _, grant := range repo.grantList() {
		if grant.position == 0 {
			continue
		}
		if position < grant.position {
			break
		}
		if grant.subject == device && grant.kind == "grant" {
			role = grant.role
		}
	}
	return role == protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER
}

// The devices that hold repo now and that the case can ask: live and not cut off.
func (self *gkmConcurrencyCase) reachableHolders(repo *gkmConcurrencyRepo) []*gkmConcurrencyDevice {
	holders := []*gkmConcurrencyDevice{}
	for _, device := range self.deviceList() {
		if device.reachable() && self.holdsAt(repo, device, math.MaxUint64) {
			holders = append(holders, device)
		}
	}
	return holders
}

// The repo the case names a repo id by, nil for none.
func (self *gkmConcurrencyCase) repoOf(repoId sidecar.Id) *gkmConcurrencyRepo {
	for _, repo := range self.repoList() {
		if repo != nil && repo.repoId == repoId {
			return repo
		}
	}
	return nil
}

// The repo whose group groupId is, as the claims gkm was asked to take show it; nil for none.
func (self *gkmConcurrencyCase) repoOfGroup(groupId sidecar.Id) *gkmConcurrencyRepo {
	for _, claim := range self.origin.claimList() {
		if claim.groupId == groupId && claim.rotate != nil {
			return self.repoOf(claim.repoId)
		}
	}
	for _, form := range self.origin.formList() {
		if form.form != nil && !form.ladder && bytes.Equal(form.form.GetGroupId(), groupId.Bytes()) {
			return self.repoOf(form.repoId)
		}
	}
	return nil
}

// The repo's group now: the group of its newest re-form gkm took, else the group its generations stand in.
func (self *gkmConcurrencyCase) repoGroup(repo *gkmConcurrencyRepo) (sidecar.Id, bool) {
	groupId, formed := sidecar.Id{}, false
	for _, form := range self.origin.formList() {
		if !form.ladder && form.taken() && form.repoId == repo.repoId {
			if id, err := sidecar.IdFromBytes(form.form.GetGroupId()); err == nil {
				groupId, formed = id, true
			}
		}
	}
	if formed {
		return groupId, true
	}
	for _, claim := range self.origin.claimList() {
		if claim.taken() && claim.rotate != nil && claim.repoId == repo.repoId {
			return claim.groupId, true
		}
	}
	return sidecar.Id{}, false
}

// gkm's newest generation of repo: the highest it took in the repo's group now, nil before any.
func (self *gkmConcurrencyCase) newestGeneration(repo *gkmConcurrencyRepo) *gkmConcurrencyGeneration {
	groupId, ok := self.repoGroup(repo)
	if !ok {
		return nil
	}
	generations, _ := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	var newest *gkmConcurrencyGeneration
	for _, generation := range generations[groupId] {
		if newest == nil || newest.rotate.GetGeneration() < generation.rotate.GetGeneration() {
			newest = generation
		}
	}
	return newest
}

// A short name of a repo for the case's lines.
func (self *gkmConcurrencyCase) repoName(repoId sidecar.Id) string {
	if repo := self.repoOf(repoId); repo != nil {
		return repo.name
	}
	return repoId.String()
}

// The network of ClientCount sidecars, its document, its repos with their grants, and every holder holding
// generation 1 of each.
func (self *gkmConcurrencyCase) setUp(ctx context.Context) error {
	settings := self.settings
	network, err := self.world.AddNetwork(ctx, worldNetworkName(0), settings.ClientCount, settings.sidecarSettings)
	if err != nil {
		return fmt.Errorf("the network: %w", err)
	}
	self.network = network
	for _, client := range network.Clients {
		if _, err := self.addDevice(client, false); err != nil {
			return err
		}
	}
	devices := self.deviceList()
	self.owner = devices[0]
	configRepoId, err := gkmConcurrencyConfigRepoId(self.owner.device)
	if err != nil {
		return err
	}
	self.config = &gkmConcurrencyRepo{name: "config", repoId: configRepoId, config: true}

	// the document: grant 1 alone, so no rule reaches evs
	if _, err := network.SaveAclPolicy(ctx, self.owner.device, []byte(settings.AclPolicy)); err != nil {
		return fmt.Errorf("the network's document: %w", err)
	}

	// the roles: the late device, the dying writer and its recipient first among the others, the revoked devices last
	others := devices[1:]
	self.late, self.dying, self.recipient = others[0], others[1], others[2]
	revokeTargets := others[len(others)-settings.RevokeCount:]

	// the shared repo, its member granted writer
	sharedRepo, err := network.CreateRepo(ctx, self.owner.device, protocol.RepoKind_REPO_KIND_GENERAL)
	if err != nil {
		return fmt.Errorf("the shared repo: %w", err)
	}
	self.shared = &gkmConcurrencyRepo{name: "shared", repoId: sharedRepo.RepoId(), admin: self.owner}
	memberGrant, err := gkmConcurrencyFile(ctx, self.world, network, self.owner.device, bridge.SignedType_SIGNED_REPO_GRANT, &protocol.RepoGrant{
		RepoId:  self.shared.repoId.Bytes(),
		Subject: network.MemberId().Bytes(),
		Role:    protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER,
	}, self.following(), settings.SyncTimeout)
	if err != nil {
		return fmt.Errorf("the shared repo's grant to its member: %w", err)
	}
	self.shared.memberGrantAt = memberGrant.GetVersion()

	// the partials, each granted to PartialWriterCount writers by sidecar: the dying writer always one of the first
	// partial's, its recipient none of them, and the late device none of the second's
	pool := slices.DeleteFunc(slices.Clone(others[1:]), func(device *gkmConcurrencyDevice) bool { return device == self.recipient })
	for index := range settings.PartialRepoCount {
		repo, err := network.CreateRepo(ctx, self.owner.device, protocol.RepoKind_REPO_KIND_GENERAL)
		if err != nil {
			return fmt.Errorf("partial repo %d: %w", index, err)
		}
		partial := &gkmConcurrencyRepo{name: fmt.Sprintf("partial%d", index), repoId: repo.RepoId(), admin: self.owner}
		self.partials = append(self.partials, partial)
		writers := []*gkmConcurrencyDevice{}
		for offset := 0; len(writers) < settings.PartialWriterCount && offset < len(pool); offset += 1 {
			writers = append(writers, pool[(index*settings.PartialWriterCount+offset)%len(pool)])
		}
		if index == 0 && !slices.Contains(writers, self.dying) {
			writers[0] = self.dying
		}
		for _, writer := range writers {
			if err := self.grant(ctx, self.owner, partial, writer, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER); err != nil {
				return err
			}
		}
	}
	// a revoked device reads a partial repo too, so its revocation shrinks more than the shared and config repos
	for _, target := range revokeTargets {
		if !slices.ContainsFunc(self.partials, func(partial *gkmConcurrencyRepo) bool { return self.holdsAt(partial, target, math.MaxUint64) }) {
			if err := self.grant(ctx, self.owner, self.partials[len(self.partials)-1], target, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER); err != nil {
				return err
			}
		}
	}

	// every holder takes generation 1 of each repo, the config repo's generation as the bootstrap left it
	if err := self.awaitConvergence(ctx, "the setup", settings.convergenceBound(false)); err != nil {
		return err
	}
	return nil
}

// granter files a RepoGrant of repo to subject as role, kept as the case's event.
func (self *gkmConcurrencyCase) grant(ctx context.Context, granter *gkmConcurrencyDevice, repo *gkmConcurrencyRepo, subject *gkmConcurrencyDevice, role protocol.RepoGrantRole) error {
	return self.grantFollowing(ctx, granter, repo, subject, role, self.following())
}

// grant, its sync waiting on following.
func (self *gkmConcurrencyCase) grantFollowing(ctx context.Context, granter *gkmConcurrencyDevice, repo *gkmConcurrencyRepo, subject *gkmConcurrencyDevice, role protocol.RepoGrantRole, following []*sidecart3.T3Device) error {
	event := self.beginEvent(&gkmConcurrencyEvent{kind: "grant", repo: repo, subject: subject, role: role})
	pin, err := gkmConcurrencyFile(ctx, self.world, self.network, granter.device, bridge.SignedType_SIGNED_REPO_GRANT, &protocol.RepoGrant{
		RepoId:  repo.repoId.Bytes(),
		Subject: subject.sidecarId.Bytes(),
		Role:    role,
	}, following, self.settings.SyncTimeout)
	self.endEvent(event, pin)
	if err != nil {
		return fmt.Errorf("%s's grant of %s to %s: %w", granter.name, repo.name, subject.name, err)
	}
	return nil
}

// The owner revokes repo's grant to subject (RepoRevoke), kept as the case's event.
func (self *gkmConcurrencyCase) revokeGrant(ctx context.Context, repo *gkmConcurrencyRepo, subject *gkmConcurrencyDevice) error {
	event := self.beginEvent(&gkmConcurrencyEvent{kind: "revoke-grant", repo: repo, subject: subject})
	pin, err := gkmConcurrencyFile(ctx, self.world, self.network, self.owner.device, bridge.SignedType_SIGNED_REPO_REVOKE, &protocol.RepoRevoke{
		RepoId:  repo.repoId.Bytes(),
		Subject: subject.sidecarId.Bytes(),
	}, self.following(), self.settings.SyncTimeout)
	self.endEvent(event, pin)
	if err != nil {
		return fmt.Errorf("the revocation of %s's grant to %s: %w", repo.name, subject.name, err)
	}
	return nil
}

// The owner revokes target (SidecarRevoke, reason removed), its sync waiting on no revoked device.
func (self *gkmConcurrencyCase) revokeDevice(ctx context.Context, target *gkmConcurrencyDevice) error {
	event := self.beginEvent(&gkmConcurrencyEvent{kind: "revoke-device", subject: target})
	func() {
		target.stateLock.Lock()
		defer target.stateLock.Unlock()
		target.revokeFiled = event.filed
	}()
	pin, err := gkmConcurrencyFile(ctx, self.world, self.network, self.owner.device, bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
		SidecarId: target.sidecarId.Bytes(),
		Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
	}, self.following(), self.settings.SyncTimeout)
	self.endEvent(event, pin)
	if pin != nil {
		func() {
			target.stateLock.Lock()
			defer target.stateLock.Unlock()
			target.revokedAt = pin.GetVersion()
		}()
	}
	if err != nil {
		return fmt.Errorf("%s's revocation: %w", target.name, err)
	}
	return nil
}

// Keeps event as filing from now, at no position yet: what a refusal or a rotator's failure meanwhile is judged by,
// while the record of what a device holds takes the event only from its position on.
func (self *gkmConcurrencyCase) beginEvent(event *gkmConcurrencyEvent) *gkmConcurrencyEvent {
	event.filed = time.Now()
	self.addEvent(event)
	if event.repo != nil {
		event.repo.addGrant(event)
	}
	return event
}

// Sets event's position, where its entry stands whether or not every follower took it in time, and when it was
// synced; an event no entry stands for keeps no position and changes nothing a device holds.
func (self *gkmConcurrencyCase) endEvent(event *gkmConcurrencyEvent, pin *bridge.AuthPin) {
	if pin == nil {
		return
	}
	// under the case's lock and then the repo's, the order nothing else nests them in, so every copy is whole
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if event.repo != nil {
			event.repo.stateLock.Lock()
			defer event.repo.stateLock.Unlock()
		}
		event.position = pin.GetVersion()
		event.synced = time.Now()
	}()
	if event.repo != nil {
		event.repo.resort()
	}
}

// Whether device's access to repo is ending: a revocation of its grant there, or of the device, whose filing began.
func (self *gkmConcurrencyCase) removalFiled(repo *gkmConcurrencyRepo, device *gkmConcurrencyDevice) bool {
	for _, event := range self.eventList() {
		if event.subject == device && (event.kind == "revoke-device" || event.kind == "revoke-grant" && event.repo == repo) {
			return true
		}
	}
	return false
}

// One write of path in repo as device, under the generation its key client gives new content, kept as acknowledged
// once its writer is answered.
func (self *gkmConcurrencyCase) write(ctx context.Context, device *gkmConcurrencyDevice, repo *gkmConcurrencyRepo, path string) error {
	content := gkmConcurrencyContent(repo, path)
	if err := worldKeysWrite(ctx, device.device, repo.repoId, path, content); err != nil {
		return fmt.Errorf("%s's write of %s on %s: %w", device.name, path, repo.name, err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeCount += 1
	self.written = append(self.written, &gkmConcurrencyWrite{repo: repo, path: path, writer: device})
	return nil
}

// One write its writer was answered for.
type gkmConcurrencyWrite struct {
	repo   *gkmConcurrencyRepo
	path   string
	writer *gkmConcurrencyDevice
}

// Every acknowledged write, oldest first.
func (self *gkmConcurrencyCase) writtenList() []*gkmConcurrencyWrite {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.written)
}

// A write during the churn: tried again while it is refused as the design says a write is refused for now, within the
// convergence bound with a lost hint (§6.8), each retry counted by its reason: gkm unreachable, its 429 or 503, or a
// lost answer (KF7, whose outcome a retry resolves), a generation that does not cover the writer's state yet
// (ErrGkmWait, EnsureWritable's step 4, which a writer's own rotation or another's ends) or a state behind the newest
// generation's pin (ErrGkmAuthAhead), and evs's own answers that refuse nothing. A writer refused as no writer, where
// the case's record says it lost the grant meanwhile, wrote nothing it may write, which is no failure. Anything else is
// the write's failure.
func (self *gkmConcurrencyCase) churnWrite(ctx context.Context, device *gkmConcurrencyDevice, repo *gkmConcurrencyRepo, path string) error {
	deadline := time.Now().Add(self.settings.convergenceBound(true))
	for {
		err := self.write(ctx, device, repo, path)
		if err == nil {
			return nil
		}
		if errors.Is(err, sidecar.ErrGkmNotWriter) && (!self.writesAt(repo, device, math.MaxUint64) || self.removalFiled(repo, device)) {
			self.countRetry("a writer whose grant ended")
			return nil
		}
		reason := gkmConcurrencyTransient(err)
		if reason == "" || ctx.Err() != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still refused for now past %s: %w", self.settings.convergenceBound(true), err)
		}
		self.countRetry(reason)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// Why err refuses a write for now only, by the design's own words; "" for an error that refuses it.
func gkmConcurrencyTransient(err error) string {
	switch {
	case errors.Is(err, sidecar.ErrGkmWait):
		return "a generation that does not cover the writer's state yet"
	case errors.Is(err, sidecar.ErrGkmAuthAhead):
		return "a writer's state behind the newest generation's pin"
	case errors.Is(err, sidecar.ErrGkmNoAnswer), errors.Is(err, sidecar.ErrGkmUnavailable):
		return "gkm unreachable"
	}
	if gkmError, ok := sidecar.GkmErrorOf(err); ok && gkmError.RefusesNothing() {
		return "gkm's answer that refuses nothing"
	}
	if evsConcurrencyUnavailable(err) {
		return "evs's answer that refuses nothing"
	}
	return ""
}

// Counts one retry or pass of the churn by its reason.
func (self *gkmConcurrencyCase) countRetry(reason string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.retryCounts[reason] += 1
}

// What path holds once written in repo.
func gkmConcurrencyContent(repo *gkmConcurrencyRepo, path string) []byte {
	return []byte(fmt.Sprintf("%s:%s\n", repo.name, path))
}

// Waits until every holder the case can ask holds gkm's newest generation of every repo (the file's header), asking
// every PollInterval, within bound of the case's last change: the newest of its events' syncs, its generations' claims
// and its chaos, else now. The lagging holders past it, as an error.
func (self *gkmConcurrencyCase) awaitConvergence(ctx context.Context, phase string, bound time.Duration) error {
	since := self.lastChange()
	deadline := since.Add(bound)
	for {
		lagging := self.laggingHolders(ctx, self.repoList())
		if len(lagging) == 0 {
			self.t.Logf("%s: every holder holds the newest generation of every repo %s after the case's last change", phase, time.Since(since).Round(time.Millisecond))
			return nil
		}
		if time.Now().After(deadline) {
			self.logLaggingDevices(lagging)
			return fmt.Errorf("%s: %d holders do not hold the newest generation %s after the case's last change, past the bound %s: %s", phase, len(lagging), time.Since(since).Round(time.Millisecond), bound, strings.Join(lagging, "; "))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: %w, %d holders lagging: %s", phase, ctx.Err(), len(lagging), strings.Join(lagging, "; "))
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// Logs the newest log lines of each device a lagging line names, the evidence a stalled convergence leaves on it.
func (self *gkmConcurrencyCase) logLaggingDevices(lagging []string) {
	for _, device := range self.deviceList() {
		named := slices.ContainsFunc(lagging, func(line string) bool { return strings.HasPrefix(line, device.name+" ") })
		if !named {
			continue
		}
		for _, line := range self.world.SidecarLogLines(device.name) {
			self.t.Logf("%s | %s", device.name, line)
		}
	}
}

// When the case last changed what a holder must hold: its newest event's sync, generation's claim or chaos, else now.
func (self *gkmConcurrencyCase) lastChange() time.Time {
	last := time.Time{}
	for _, event := range self.eventList() {
		if last.Before(event.synced) {
			last = event.synced
		}
	}
	for _, claim := range self.origin.claimList() {
		if claim.taken() && last.Before(claim.time) {
			last = claim.time
		}
	}
	self.stateLock.Lock()
	for _, armed := range []*gkmConcurrencyChaos{self.midRotation, self.midDelivery} {
		if armed != nil && armed.request != nil && last.Before(armed.request.time) {
			last = armed.request.time
		}
	}
	self.stateLock.Unlock()
	if last.IsZero() {
		return time.Now()
	}
	return last
}

// Each reachable holder of repos that does not hold gkm's newest generation, as a line.
func (self *gkmConcurrencyCase) laggingHolders(ctx context.Context, repos []*gkmConcurrencyRepo) []string {
	lagging := []string{}
	for _, repo := range repos {
		newest := self.newestGeneration(repo)
		if newest == nil {
			lagging = append(lagging, fmt.Sprintf("%s has no generation at gkm", repo.name))
			continue
		}
		for _, device := range self.reachableHolders(repo) {
			if line := self.holdsNewest(ctx, device, repo, newest); line != "" {
				lagging = append(lagging, line)
			}
		}
	}
	return lagging
}

// "" where device's key client holds newest as its newest generation of repo, else what it holds.
func (self *gkmConcurrencyCase) holdsNewest(ctx context.Context, device *gkmConcurrencyDevice, repo *gkmConcurrencyRepo, newest *gkmConcurrencyGeneration) string {
	key, err := device.device.Gkm.CurrentRepoKey(ctx, repo.repoId)
	if err != nil {
		return fmt.Sprintf("%s holds no generation of %s (gkm's newest %d): %v", device.name, repo.name, newest.rotate.GetGeneration(), err)
	}
	// a generation a re-form's ladder carried names the group it was claimed in, as its key does
	if key.GroupId() != sidecar.Id(newest.rotate.GetGroupId()) || key.Generation() != newest.rotate.GetGeneration() || !bytes.Equal(key.Commitment(), newest.rotate.GetCommitment()) {
		return fmt.Sprintf("%s holds %s's generation %d, gkm's newest %d", device.name, repo.name, key.Generation(), newest.rotate.GetGeneration())
	}
	return ""
}

// Waits until device holds gkm's newest generation of repo, within bound, asking every PollInterval: each ask of a
// device that holds none takes what gkm holds for it, a late ask among it.
func (self *gkmConcurrencyCase) awaitDeviceKey(ctx context.Context, device *gkmConcurrencyDevice, repo *gkmConcurrencyRepo, bound time.Duration) error {
	started := time.Now()
	deadline := started.Add(bound)
	for {
		newest := self.newestGeneration(repo)
		line := "no generation at gkm"
		if newest != nil {
			line = self.holdsNewest(ctx, device, repo, newest)
		}
		if line == "" {
			self.t.Logf("%s holds %s's newest generation %d, %s after the wait began", device.name, repo.name, newest.rotate.GetGeneration(), time.Since(started).Round(time.Millisecond))
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("past the bound %s: %s", bound, line)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %s", ctx.Err(), line)
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// RotatorCount devices rotate repo at once, released together: every rotator returns a generation gkm took, its key
// that generation's, and where nothing else may rotate meanwhile (exact), every rotator returns the one generation
// past the newest before the round.
func (self *gkmConcurrencyCase) raceRotations(ctx context.Context, repo *gkmConcurrencyRepo, rotators []*gkmConcurrencyDevice, exact bool) error {
	// every rotator first follows the log to the newest position the owner holds, so a round races rotations rather
	// than followers
	devices := []*sidecart3.T3Device{}
	for _, rotator := range rotators {
		devices = append(devices, rotator.device)
	}
	if err := gkmConcurrencySync(ctx, self.world, self.network, gkmConcurrencyStateVersion(self.owner.device), devices, self.settings.SyncTimeout); err != nil {
		return fmt.Errorf("the rotators of %s did not follow the log before their round: %w", repo.name, err)
	}
	before := self.newestGeneration(repo)
	release := make(chan struct{})
	keys := make([]*sidecar.EvsRepoKey, len(rotators))
	errs := make([]error, len(rotators))
	var group sync.WaitGroup
	for index, rotator := range rotators {
		group.Go(func() {
			<-release
			keys[index], errs[index] = rotator.device.Gkm.RotateRepo(ctx, repo.repoId)
		})
	}
	close(release)
	group.Wait()
	generations, _ := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	problems := []string{}
	returned := map[uint64][]byte{}
	for index, rotator := range rotators {
		if errs[index] != nil {
			if !self.writesAt(repo, rotator, math.MaxUint64) || self.removalFiled(repo, rotator) {
				// its grant ended during the round: the exchange gives it no scope there, and it rotates nothing
				self.countRetry("a rotator whose grant ended during its round")
				continue
			}
			if errors.Is(errs[index], sidecar.ErrGkmAuthAhead) {
				// the winner's pin is past its state, which follows the log a moment behind (§8.9): it judges and adopts
				// nothing yet, and rotates nothing
				self.countRetry("a rotator whose state is behind the winner's pin")
				continue
			}
			if reason := gkmConcurrencyTransient(errs[index]); reason != "" && !exact {
				// gkm restarting under the shrink phase's chaos: the rotation's outcome is unknown and resolved as after a
				// lost answer (KF7), its draft resumed by the next rotation or write
				self.countRetry("a rotation refused for now: " + reason)
				continue
			}
			problems = append(problems, fmt.Sprintf("%s's rotation: %v", rotator.name, errs[index]))
			continue
		}
		key := keys[index]
		taken := generations[key.GroupId()][key.Generation()]
		if taken == nil || !bytes.Equal(taken.rotate.GetCommitment(), key.Commitment()) {
			problems = append(problems, fmt.Sprintf("%s's rotation returned generation %d of %s, which gkm did not take under its commitment", rotator.name, key.Generation(), repo.name))
			continue
		}
		if held, ok := returned[key.Generation()]; ok && !bytes.Equal(held, key.Commitment()) {
			problems = append(problems, fmt.Sprintf("two rotators of %s returned generation %d under different commitments", repo.name, key.Generation()))
		}
		returned[key.Generation()] = key.Commitment()
	}
	if exact && before != nil && len(problems) == 0 {
		want := before.rotate.GetGeneration() + 1
		if len(returned) != 1 || returned[want] == nil {
			problems = append(problems, fmt.Sprintf("the race on %s from generation %d returned generations %v, not %d alone", repo.name, before.rotate.GetGeneration(), slices.Sorted(maps.Keys(returned)), want))
		}
		if newest := self.newestGeneration(repo); newest == nil || newest.rotate.GetGeneration() != want {
			problems = append(problems, fmt.Sprintf("the race on %s left gkm's newest generation past %d", repo.name, want))
		}
	}
	if 0 < len(problems) {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// A burst that never ends at a step's failure: each failure is kept in the burst's errors and the next step runs, so
// one finding does not hide the next.
func gkmConcurrencyBurst(name string, steps []func(ctx context.Context, random *rand.Rand) error) *WorldBurst {
	burst := &WorldBurst{Name: name, StepCount: len(steps)}
	burst.Step = func(ctx context.Context, index int, random *rand.Rand) error {
		if err := steps[index](ctx, random); err != nil {
			if ctx.Err() != nil {
				return err
			}
			burst.Fail(fmt.Errorf("%s's step %d: %w", name, index, err))
		}
		return nil
	}
	return burst
}

// Each writer's writes to every repo it writes now, WriteCount each, named for phase.
func (self *gkmConcurrencyCase) writeBursts(phase string, writers []*gkmConcurrencyDevice) []*WorldBurst {
	bursts := []*WorldBurst{}
	for _, writer := range writers {
		steps := []func(ctx context.Context, random *rand.Rand) error{}
		for _, repo := range self.repoList() {
			if !self.writesAt(repo, writer, math.MaxUint64) {
				continue
			}
			for index := range self.settings.WriteCount {
				// at the repo's top, which stands: evs's planning of a parent several writers create at once is the evs
				// case's finding (e2e-evs-concurrency's disjoint first creations), not this one's
				path := fmt.Sprintf("%s-%s-%d.txt", phase, writer.name, index)
				steps = append(steps, func(ctx context.Context, random *rand.Rand) error {
					if !writer.live() {
						return nil
					}
					return self.churnWrite(ctx, writer, repo, path)
				})
			}
		}
		bursts = append(bursts, gkmConcurrencyBurst(fmt.Sprintf("%s's %s writes", writer.name, phase), steps))
	}
	return bursts
}

// Fails each burst's errors on the case, with phase.
func (self *gkmConcurrencyCase) failBursts(phase string, bursts []*WorldBurst) {
	for _, burst := range bursts {
		for _, err := range burst.Errors() {
			self.t.Errorf("%s: %v", phase, err)
		}
	}
}

// The growth phase (the file's header).
func (self *gkmConcurrencyCase) growthPhase() {
	settings := self.settings
	ctx, cancel := self.world.StepContext(self.t.Context(), settings.PhaseTimeout)
	defer cancel()
	// every writer at the phase's start, the owner among them
	writers := []*gkmConcurrencyDevice{}
	for _, device := range self.deviceList() {
		if device.live() {
			writers = append(writers, device)
		}
	}
	joins := []func(ctx context.Context, random *rand.Rand) error{}
	for range settings.JoinCount {
		joins = append(joins, func(ctx context.Context, random *rand.Rand) error {
			added, err := self.network.AddSidecars(ctx, 1, settings.sidecarSettings)
			if err != nil {
				return fmt.Errorf("a joiner's enrollment: %w", err)
			}
			if err := self.network.Admit(ctx, added); err != nil {
				return fmt.Errorf("%s's admission: %w", added[0].Name, err)
			}
			device, err := self.addDevice(added[0], true)
			if err != nil {
				return err
			}
			self.addEvent(&gkmConcurrencyEvent{kind: "admit", subject: device, position: device.admission(), filed: time.Now(), synced: time.Now()})
			return nil
		})
	}
	grants := []func(ctx context.Context, random *rand.Rand) error{}
	for _, partial := range self.partials {
		granted := 0
		for _, device := range self.deviceList()[1:] {
			if granted == settings.GrowGrantCount {
				break
			}
			if self.holdsAt(partial, device, math.MaxUint64) || device == self.recipient && partial == self.partials[0] || device == self.late && 1 < len(self.partials) && partial == self.partials[1] {
				continue
			}
			granted += 1
			grants = append(grants, func(ctx context.Context, random *rand.Rand) error {
				return self.grant(ctx, self.owner, partial, device, protocol.RepoGrantRole_REPO_GRANT_ROLE_READER)
			})
		}
		if granted < settings.GrowGrantCount {
			self.t.Errorf("the growth phase: %s has %d devices to grant, not %d", partial.name, granted, settings.GrowGrantCount)
		}
	}
	before := map[*gkmConcurrencyRepo]uint64{}
	for _, repo := range self.repoList() {
		if newest := self.newestGeneration(repo); newest != nil {
			before[repo] = newest.rotate.GetGeneration()
		}
	}
	// the rotators: writers of the shared repo, never the owner, which files the phase's statements
	rotators := writers[1 : 1+min(settings.RotatorCount, len(writers)-1)]
	races := []func(ctx context.Context, random *rand.Rand) error{}
	for range settings.RotationRoundCount {
		races = append(races, func(ctx context.Context, random *rand.Rand) error {
			return self.raceRotations(ctx, self.shared, rotators, true)
		})
	}
	bursts := []*WorldBurst{
		gkmConcurrencyBurst("the joins", joins),
		gkmConcurrencyBurst("the reader grants", grants),
		gkmConcurrencyBurst("the shared repo's rotation races", races),
	}
	bursts = append(bursts, self.writeBursts("growth", writers)...)
	if err := RunBursts(ctx, self.t, "the growth phase", bursts, settings.Bursts); err != nil {
		self.world.FailProgress(self.t, "the growth phase", err, bursts)
	}
	self.failBursts("the growth phase", bursts)
	if err := self.awaitConvergence(ctx, "the growth phase", settings.convergenceBound(false)); err != nil {
		self.t.Errorf("%v", err)
	}
	// nothing shrank, so no generation of the phase rotated for a removal; and an Add keeps the generation (§10.5), so
	// the phase's only generations are the races' own
	for _, claim := range self.origin.claimList() {
		if claim.taken() && claim.rotate != nil && sidecar.GkmCause(claim.rotate.GetCause()) == sidecar.GkmCauseRemove {
			self.t.Errorf("the growth phase: %s's generation %d took cause REMOVE, though nothing shrank", self.repoName(claim.repoId), claim.rotate.GetGeneration())
		}
	}
	for _, repo := range self.repoList() {
		want := before[repo]
		if repo == self.shared {
			want += uint64(settings.RotationRoundCount)
		}
		if newest := self.newestGeneration(repo); newest == nil || newest.rotate.GetGeneration() != want {
			self.t.Errorf("the growth phase moved %s from generation %d to %s, not to %d: an admission or a grant rotated", repo.name, before[repo], describeGkmConcurrencyGeneration(newest), want)
		}
	}
}

// The shrink phase (the file's header).
func (self *gkmConcurrencyCase) shrinkPhase() {
	settings := self.settings
	ctx, cancel := self.world.StepContext(self.t.Context(), settings.PhaseTimeout)
	defer cancel()
	initial := []*gkmConcurrencyDevice{}
	for _, device := range self.deviceList() {
		if !device.joined {
			initial = append(initial, device)
		}
	}
	targets := initial[len(initial)-settings.RevokeCount:]
	writers := []*gkmConcurrencyDevice{}
	for _, device := range self.deviceList() {
		if device.live() && !slices.Contains(targets, device) {
			writers = append(writers, device)
		}
	}
	revoked := make(chan struct{})
	revocations := []func(ctx context.Context, random *rand.Rand) error{}
	for index, target := range targets {
		revocations = append(revocations, func(ctx context.Context, random *rand.Rand) error {
			err := self.revokeDevice(ctx, target)
			if index == 0 {
				close(revoked)
			}
			return err
		})
	}
	// each partial repo's grants to ShrinkRevokeCount holders, a writer's or a reader's alike, never a revocation
	// target's, whose revocation shrinks it anyway, nor the dying writer's, whose own phase needs its grant
	grantRevokes := []func(ctx context.Context, random *rand.Rand) error{}
	for _, partial := range self.partials {
		subjects := []*gkmConcurrencyDevice{}
		for _, grant := range partial.grantList() {
			subject := grant.subject
			if len(subjects) == settings.ShrinkRevokeCount || grant.kind != "grant" || slices.Contains(subjects, subject) || slices.Contains(targets, subject) || subject == self.dying || !self.holdsAt(partial, subject, math.MaxUint64) {
				continue
			}
			subjects = append(subjects, subject)
			grantRevokes = append(grantRevokes, func(ctx context.Context, random *rand.Rand) error {
				return self.revokeGrant(ctx, partial, subject)
			})
		}
		if len(subjects) < settings.ShrinkRevokeCount {
			self.t.Errorf("the shrink phase: %s has %d grants to revoke, not %d", partial.name, len(subjects), settings.ShrinkRevokeCount)
		}
	}
	// the race: a partial repo's writers that keep their grant, at most RotatorCount
	raceRepo := self.partials[0]
	rotators := []*gkmConcurrencyDevice{}
	for _, writer := range writers {
		if len(rotators) < settings.RotatorCount && writer != self.owner && self.writesAt(raceRepo, writer, math.MaxUint64) {
			rotators = append(rotators, writer)
		}
	}
	races := []func(ctx context.Context, random *rand.Rand) error{}
	if 2 <= len(rotators) {
		for range settings.RotationRoundCount {
			races = append(races, func(ctx context.Context, random *rand.Rand) error {
				// shrinks land meanwhile, so a rotator may rotate past another's generation: each must still return one
				// gkm took
				return self.raceRotations(ctx, raceRepo, slices.DeleteFunc(slices.Clone(rotators), func(device *gkmConcurrencyDevice) bool {
					return !self.writesAt(raceRepo, device, math.MaxUint64)
				}), false)
			})
		}
	}
	restart := func() error {
		return self.world.Restart(WorldBlockGkm)
	}
	chaos := []func(ctx context.Context, random *rand.Rand) error{
		func(ctx context.Context, random *rand.Rand) error {
			select {
			case <-revoked:
			case <-ctx.Done():
				return ctx.Err()
			}
			armed := self.origin.arm("mid-rotation", func(request *gkmConcurrencyRequest) bool {
				return request.kind == gkmConcurrencyRequestClaim
			}, true, restart)
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.midRotation = armed
			}()
			return self.awaitChaos(ctx, armed)
		},
		func(ctx context.Context, random *rand.Rand) error {
			armed := self.origin.arm("mid-delivery", func(request *gkmConcurrencyRequest) bool {
				return request.kind == gkmConcurrencyRequestDeliver
			}, false, restart)
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.midDelivery = armed
			}()
			return self.awaitChaos(ctx, armed)
		},
	}
	bursts := []*WorldBurst{
		gkmConcurrencyBurst("the revocations", revocations),
		gkmConcurrencyBurst("the grants' revocations", grantRevokes),
		gkmConcurrencyBurst("the partial repo's rotation races", races),
		gkmConcurrencyBurst("gkm's restarts", chaos),
	}
	bursts = append(bursts, self.writeBursts("shrink", writers)...)
	if err := RunBursts(ctx, self.t, "the shrink phase", bursts, settings.Bursts); err != nil {
		self.world.FailProgress(self.t, "the shrink phase", err, bursts)
	}
	self.failBursts("the shrink phase", bursts)
	// a restart may have lost a hint, which costs at most a resync
	if err := self.awaitConvergence(ctx, "the shrink phase", settings.convergenceBound(true)); err != nil {
		self.t.Errorf("%v", err)
	}
}

// Waits until armed took its request, within ChaosTimeout, and answers the chaos's own failure.
func (self *gkmConcurrencyCase) awaitChaos(ctx context.Context, armed *gkmConcurrencyChaos) error {
	select {
	case <-armed.taken:
		if armed.chaosErr != nil {
			return fmt.Errorf("the %s chaos: %w", armed.name, armed.chaosErr)
		}
		self.t.Logf("the %s chaos took %s from %s at %s, the block answering %d", armed.name, armed.request.route, self.nameOf(armed.request.sidecarId), armed.request.time.Format("15:04:05.000"), armed.status)
		return nil
	case <-time.After(self.settings.ChaosTimeout):
		self.origin.disarm(armed)
		return fmt.Errorf("the %s chaos took no request within %s", armed.name, self.settings.ChaosTimeout)
	case <-ctx.Done():
		self.origin.disarm(armed)
		return ctx.Err()
	}
}

// A sidecar's name in the case's lines.
func (self *gkmConcurrencyCase) nameOf(sidecarId sidecar.Id) string {
	if device := self.deviceOf(sidecarId); device != nil {
		return device.name
	}
	return "sidecar " + sidecarId.String()
}

// The late device's phase (the file's header).
func (self *gkmConcurrencyCase) latePhase() {
	settings := self.settings
	ctx, cancel := self.world.StepContext(self.t.Context(), settings.PhaseTimeout)
	defer cancel()
	late := self.late
	if !late.live() || len(self.partials) < 2 {
		self.t.Errorf("the late device's phase: %s is not live, or the case has no second partial repo", late.name)
		return
	}
	partial := self.partials[1]
	if self.holdsAt(partial, late, math.MaxUint64) {
		self.t.Errorf("the late device's phase: %s already reads %s", late.name, partial.name)
		return
	}
	partialGroup := self.newestGeneration(partial)
	sharedBefore := self.newestGeneration(self.shared)
	// cut off from gkm, and every delivery to it in the partial repo lost while it is
	_, removeCut := self.origin.cut("the late device", func(request *gkmConcurrencyRequest) bool {
		return request.sidecarId == late.sidecarId
	})
	lostDeliveries, removeLost := self.origin.cut("the late device's deliveries", func(request *gkmConcurrencyRequest) bool {
		if request.kind != gkmConcurrencyRequestDeliver || partialGroup == nil || request.groupId != partialGroup.groupId {
			return false
		}
		batch := &protocol.GkmWrapBatch{}
		if proto.Unmarshal(request.body, batch) != nil || len(batch.GetDeliveries()) == 0 {
			return false
		}
		for _, signedBytes := range batch.GetDeliveries() {
			_, body, _, err := readGkmConcurrencyDelivery(signedBytes)
			if err != nil || !bytes.Equal(body.GetRecipient(), late.sidecarId.Bytes()) {
				return false
			}
		}
		return true
	})
	func() {
		late.stateLock.Lock()
		defer late.stateLock.Unlock()
		late.cutOff = true
	}()
	cutAt := time.Now()
	// rotations it misses, each STAGED to it, its ReaderKey standing at gkm
	rotator := self.dying
	if !rotator.live() {
		rotator = self.owner
	}
	for index := range settings.OfflineRotationCount {
		if _, err := rotator.device.Gkm.RotateRepo(ctx, self.shared.repoId); err != nil {
			self.t.Errorf("the late device's phase: rotation %d of the shared repo by %s: %v", index, rotator.name, err)
		}
	}
	if err := self.grant(ctx, self.owner, partial, late, protocol.RepoGrantRole_REPO_GRANT_ROLE_READER); err != nil {
		self.t.Errorf("the late device's phase: %v", err)
	}
	// the granting device delivers at its pending hint at once; that delivery is lost
	deadline := time.Now().Add(settings.convergenceBound(false))
	for self.origin.cutCount(lostDeliveries) == 0 && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
		case <-time.After(settings.PollInterval):
		}
	}
	lostCount := self.origin.cutCount(lostDeliveries)
	self.t.Logf("the late device's phase: %d deliveries batches to %s in %s lost while it was cut off", lostCount, late.name, partial.name)
	if lostCount == 0 {
		self.t.Errorf("the late device's phase: no holder delivered %s's key of %s within %s of its grant, though the granting device delivers at its pending hint", late.name, partial.name, settings.convergenceBound(false))
	}
	removeLost()
	removeCut()
	func() {
		late.stateLock.Lock()
		defer late.stateLock.Unlock()
		late.cutOff = false
	}()
	// and its granting device, the owner, which signed its admission and the grant, away from gkm while it is back, so
	// its late ask must be answered by another writer, after its late jitter (DeliverLate)
	_, removeOwnerCut := self.origin.cut("the granting device", func(request *gkmConcurrencyRequest) bool {
		return request.sidecarId == self.owner.sidecarId
	})
	func() {
		self.owner.stateLock.Lock()
		defer self.owner.stateLock.Unlock()
		self.owner.cutOff = true
	}()
	defer func() {
		removeOwnerCut()
		self.owner.stateLock.Lock()
		defer self.owner.stateLock.Unlock()
		self.owner.cutOff = false
	}()
	backAt := time.Now()

	// it reads what a writer sealed under the shared repo's newest generation since: its read takes the generations it
	// missed
	writer := rotator
	if writer == self.owner {
		for _, device := range self.reachableHolders(self.shared) {
			if device != late {
				writer = device
				break
			}
		}
	}
	path := fmt.Sprintf("late-%s.txt", writer.name)
	if err := self.write(ctx, writer, self.shared, path); err != nil {
		self.t.Errorf("the late device's phase: %v", err)
	} else {
		bound := settings.convergenceBound(false)
		readDeadline := time.Now().Add(bound)
		for {
			err := worldKeysRead(ctx, late.device, self.shared.repoId, path, gkmConcurrencyContent(self.shared, path))
			if err == nil {
				break
			}
			if time.Now().After(readDeadline) {
				self.t.Errorf("the late device's phase: %s back from %s cut off reads nothing sealed since within %s: %v", late.name, backAt.Sub(cutAt).Round(time.Millisecond), bound, err)
				break
			}
			select {
			case <-ctx.Done():
			case <-time.After(settings.PollInterval):
			}
		}
	}
	if err := self.awaitDeviceKey(ctx, late, self.shared, settings.convergenceBound(false)); err != nil {
		self.t.Errorf("the late device's phase: %s's shared repo: %v", late.name, err)
	}
	if sharedAfter := self.newestGeneration(self.shared); sharedBefore != nil && sharedAfter != nil && sharedAfter.rotate.GetGeneration() < sharedBefore.rotate.GetGeneration()+uint64(settings.OfflineRotationCount) {
		self.t.Errorf("the late device's phase: the shared repo moved from generation %d to %d, not %d rotations", sharedBefore.rotate.GetGeneration(), sharedAfter.rotate.GetGeneration(), settings.OfflineRotationCount)
	}
	// and asks late for the partial repo's, which a writer delivers
	if err := self.awaitDeviceKey(ctx, late, partial, settings.convergenceBound(false)); err != nil {
		self.t.Errorf("the late device's phase: %s's %s: %v", late.name, partial.name, err)
	}
	asked := false
	for _, ask := range self.origin.askList() {
		if ask.sidecarId == late.sidecarId && partialGroup != nil && ask.groupId == partialGroup.groupId && !ask.time.Before(backAt) {
			asked = true
		}
	}
	if !asked {
		self.t.Errorf("the late device's phase: %s took %s's key with no late ask once back, though every delivery to it was lost", late.name, partial.name)
	}
	for _, delivery := range self.origin.deliveryList() {
		if delivery.recipient == late.sidecarId && partialGroup != nil && delivery.groupId == partialGroup.groupId && delivery.status == 201 && !delivery.time.Before(backAt) {
			self.t.Logf("the late device's phase: %s delivered %s's generation %d to %s at its late ask, %s after it was back", self.nameOf(delivery.uploader), partial.name, delivery.generation, late.name, delivery.time.Sub(backAt).Round(time.Millisecond))
		}
	}
}

// The dying writer's phase (the file's header).
func (self *gkmConcurrencyCase) dyingPhase() {
	settings := self.settings
	ctx, cancel := self.world.StepContext(self.t.Context(), settings.PhaseTimeout)
	defer cancel()
	dying, recipient, partial := self.dying, self.recipient, self.partials[0]
	if !dying.live() || !recipient.live() || !self.holdsAt(partial, dying, math.MaxUint64) || self.holdsAt(partial, recipient, math.MaxUint64) {
		self.t.Errorf("the dying writer's phase: %s must read %s and %s must not, both live", dying.name, partial.name, recipient.name)
		return
	}
	group := self.newestGeneration(partial)
	armed := self.origin.arm("dying writer", func(request *gkmConcurrencyRequest) bool {
		if request.kind != gkmConcurrencyRequestDeliver || request.sidecarId != dying.sidecarId || group == nil || request.groupId != group.groupId {
			return false
		}
		batch := &protocol.GkmWrapBatch{}
		if proto.Unmarshal(request.body, batch) != nil {
			return false
		}
		for _, signedBytes := range batch.GetDeliveries() {
			if _, body, _, err := readGkmConcurrencyDelivery(signedBytes); err == nil && bytes.Equal(body.GetRecipient(), recipient.sidecarId.Bytes()) {
				return true
			}
		}
		return false
	}, false, func() error {
		func() {
			dying.stateLock.Lock()
			defer dying.stateLock.Unlock()
			dying.dead = true
		}()
		// its close waits for its generation, whose request this hook holds, so it runs beside the hook
		go dying.device.Close()
		return nil
	})
	// the dying writer grants the recipient, so it is the granting device, which delivers at once; its own sync waits
	// on no follower of its own, which its close ends
	following := slices.DeleteFunc(self.following(), func(device *sidecart3.T3Device) bool { return device == dying.device })
	if err := self.grantFollowing(ctx, dying, partial, recipient, protocol.RepoGrantRole_REPO_GRANT_ROLE_READER, following); err != nil {
		self.t.Errorf("the dying writer's phase: %v", err)
	}
	if err := self.awaitChaos(ctx, armed); err != nil {
		self.t.Errorf("the dying writer's phase: %s, the granting device, made no delivery to %s: %v", dying.name, recipient.name, err)
		// whatever happened, it dies now
		func() {
			dying.stateLock.Lock()
			defer dying.stateLock.Unlock()
			dying.dead = true
		}()
		go dying.device.Close()
	}
	// the recipient takes its key from another holder at its late ask
	if err := self.awaitDeviceKey(ctx, recipient, partial, settings.convergenceBound(false)); err != nil {
		self.t.Errorf("the dying writer's phase: %s's %s after %s died mid-delivery: %v", recipient.name, partial.name, dying.name, err)
	}
}

// The final phase: convergence, then every writer's write read on every other holder.
func (self *gkmConcurrencyCase) finalPhase() {
	settings := self.settings
	ctx, cancel := self.world.StepContext(self.t.Context(), settings.PhaseTimeout)
	defer cancel()
	if err := self.awaitConvergence(ctx, "the final phase", settings.convergenceBound(true)); err != nil {
		self.t.Errorf("%v", err)
	}
	var group sync.WaitGroup
	for _, device := range self.deviceList() {
		if !device.reachable() {
			continue
		}
		group.Go(func() {
			for _, repo := range self.repoList() {
				if !self.writesAt(repo, device, math.MaxUint64) {
					continue
				}
				path := fmt.Sprintf("final-%s.txt", device.name)
				if err := self.write(ctx, device, repo, path); err != nil {
					self.t.Errorf("the final phase: %v", err)
					continue
				}
				// written under the newest generation, which every holder holds
				if newest := self.newestGeneration(repo); newest != nil {
					if key, err := device.device.Gkm.CurrentRepoKey(ctx, repo.repoId); err != nil || key.Generation() != newest.rotate.GetGeneration() {
						self.t.Errorf("the final phase: %s wrote %s past gkm's newest generation %d", device.name, repo.name, newest.rotate.GetGeneration())
					}
				}
			}
		})
	}
	group.Wait()
	// every acknowledged write of the case reads on every holder of its repo: the final ones under the newest
	// generation, the churn's under older ones through the ladder, a joiner's whole history among them (§10.5); a path
	// written several times reads its newest content, which is the same bytes
	written := map[string]*gkmConcurrencyWrite{}
	for _, write := range self.writtenList() {
		written[write.repo.name+"/"+write.path] = write
	}
	readers := make(chan *gkmConcurrencyDevice)
	var readGroup sync.WaitGroup
	for range settings.VerifyConcurrency {
		readGroup.Go(func() {
			for reader := range readers {
				for _, write := range written {
					if !self.holdsAt(write.repo, reader, math.MaxUint64) {
						continue
					}
					if err := worldKeysRead(ctx, reader.device, write.repo.repoId, write.path, gkmConcurrencyContent(write.repo, write.path)); err != nil {
						if strings.Contains(err.Error(), "config pin before what the reader retains") {
							err = fmt.Errorf("%w (QD-1178: a path written before two config bases)", err)
						}
						self.t.Errorf("the final phase: %v", err)
					}
				}
			}
		})
	}
	for _, device := range self.deviceList() {
		if device.reachable() {
			readers <- device
		}
	}
	close(readers)
	readGroup.Wait()
}

// No fork: gkm took one commitment for each generation of each group, and every device the case can ask holds that one
// as its newest, its keychain holding no other at any generation gkm took.
func (self *gkmConcurrencyCase) checkGenerations() {
	claims := self.origin.claimList()
	generations, forks := gkmConcurrencyGenerations(claims, self.origin.formList())
	for _, fork := range forks {
		self.t.Errorf("a fork at gkm: %s", fork)
	}
	for _, claim := range claims {
		if !claim.taken() {
			continue
		}
		if err := gkmConcurrencyVerifyClaim(claim, self.identityKey); err != nil {
			self.t.Errorf("gkm took %s's claim of %s's generation %d, which does not verify: %v", self.nameOf(claim.uploader), self.repoName(claim.repoId), claim.rotate.GetGeneration(), err)
		}
	}
	for groupId, groupGenerations := range generations {
		// a ladder with no gap: each generation's parent the one below's commitment
		for g, generation := range groupGenerations {
			if g == 1 {
				continue
			}
			parent := groupGenerations[g-1]
			if parent == nil {
				self.t.Errorf("group %s holds generation %d with no generation %d below it", groupId, g, g-1)
				continue
			}
			if !bytes.Equal(generation.rotate.GetParentCommitment(), parent.rotate.GetCommitment()) {
				self.t.Errorf("group %s's generation %d does not chain to its generation %d", groupId, g, g-1)
			}
		}
	}
	for _, repo := range self.repoList() {
		newest := self.newestGeneration(repo)
		if newest == nil {
			self.t.Errorf("%s has no generation at gkm", repo.name)
			continue
		}
		for _, device := range self.reachableHolders(repo) {
			for g, generation := range generations[newest.groupId] {
				held := device.device.Sidecar().GkmHeldKey(repo.repoId, g)
				if held != nil && !bytes.Equal(held.Commitment(), generation.rotate.GetCommitment()) {
					self.t.Errorf("%s holds %s's generation %d under another commitment than gkm took", device.name, repo.name, g)
				}
			}
		}
	}
}

// Every delivery verifies under its signer's admitted key, is sealed to a ReaderKey its recipient published, and names
// a holder of the repo at the delivery's state.
func (self *gkmConcurrencyCase) checkDeliveries() {
	readerKeys := map[sidecar.Id][][]byte{}
	for _, readerKey := range self.origin.readerKeyList() {
		if readerKey.status != 200 && readerKey.status != 201 {
			continue
		}
		if err := gkmConcurrencyVerifyReaderKey(readerKey, self.identityKey); err != nil {
			self.t.Errorf("gkm took %s's ReaderKey, which does not verify under its identity key: %v", self.nameOf(readerKey.sidecarId), err)
			continue
		}
		readerKeys[readerKey.sidecarId] = append(readerKeys[readerKey.sidecarId], readerKey.keySha256)
	}
	claims := self.origin.claimList()
	generations, _ := gkmConcurrencyGenerations(claims, self.origin.formList())
	counts := map[string]int{}
	// one delivery taken per (group, generation, commitment, recipient, key), gkm's If-None-Match at the object's path:
	// a second is one applied twice
	takenDeliveries := map[string][]byte{}
	for _, delivery := range self.origin.deliveryList() {
		if delivery.status != 201 || delivery.body == nil {
			continue
		}
		// the object's path: wrap/<g>/<c8>/<recipient>/<k8>, so two drafts of one generation number stage apart
		key := fmt.Sprintf("%s/%d/%x/%s/%x", delivery.groupId, delivery.generation, delivery.body.GetCommitment(), delivery.recipient, delivery.body.GetRecipientKeySha256())
		if held, ok := takenDeliveries[key]; ok && !bytes.Equal(held, delivery.signed.GetSignature()) {
			self.t.Errorf("gkm took two deliveries of group %s's generation %d, commitment %x, to %s's one key", delivery.groupId, delivery.generation, delivery.body.GetCommitment()[:8], self.nameOf(delivery.recipient))
		}
		takenDeliveries[key] = delivery.signed.GetSignature()
	}
	for _, delivery := range self.origin.deliveryList() {
		repo := self.repoOfGroup(delivery.groupId)
		where := fmt.Sprintf("%s's delivery of group %s's generation %d to %s", self.nameOf(delivery.uploader), delivery.groupId, delivery.generation, self.nameOf(delivery.recipient))
		if repo != nil {
			where = fmt.Sprintf("%s's delivery of %s's generation %d to %s", self.nameOf(delivery.uploader), repo.name, delivery.generation, self.nameOf(delivery.recipient))
		}
		if err := gkmConcurrencyVerifyDelivery(delivery, self.identityKey); err != nil {
			self.t.Errorf("%s does not verify: %v", where, err)
			continue
		}
		body := delivery.body
		if body.GetGeneration() != delivery.generation || !bytes.Equal(body.GetGroupId(), delivery.groupId.Bytes()) {
			self.t.Errorf("%s names group %x's generation %d in its body", where, body.GetGroupId(), body.GetGeneration())
		}
		if !slices.ContainsFunc(readerKeys[delivery.recipient], func(keySha256 []byte) bool { return bytes.Equal(keySha256, body.GetRecipientKeySha256()) }) {
			self.t.Errorf("%s is sealed to key %x, no ReaderKey its recipient published", where, body.GetRecipientKeySha256()[:8])
		}
		if repo == nil {
			self.t.Errorf("%s names a group no repo of the case formed", where)
			continue
		}
		recipient := self.deviceOf(delivery.recipient)
		if recipient == nil {
			self.t.Errorf("%s names a recipient that is no device of the case", where)
			continue
		}
		// the state the delivery was made at: a LATE one's own, a STAGED one's generation's
		position := uint64(0)
		switch sidecar.GkmDeliveryKind(body.GetDeliveryKind()) {
		case sidecar.GkmDeliveryLate:
			position = body.GetAuth().GetVersion()
			counts["late"] += 1
		case sidecar.GkmDeliveryStaged:
			counts["staged"] += 1
			if taken := generations[delivery.groupId][delivery.generation]; taken != nil && bytes.Equal(taken.rotate.GetCommitment(), body.GetCommitment()) {
				position = taken.rotate.GetAuth().GetVersion()
			} else {
				// a lost claim's staged delivery: the draft's own claim, where its rotator sent one
				for _, claim := range claims {
					if claim.rotate != nil && claim.groupId == delivery.groupId && bytes.Equal(claim.rotate.GetCommitment(), body.GetCommitment()) {
						position = claim.rotate.GetAuth().GetVersion()
					}
				}
				if position == 0 {
					counts["staged of a claim never sent"] += 1
				}
			}
		default:
			self.t.Errorf("%s is of kind %d", where, body.GetDeliveryKind())
			continue
		}
		if position != 0 && !self.holdsAt(repo, recipient, position) {
			self.t.Errorf("%s names a sidecar that did not read %s at the delivery's state, log position %d", where, repo.name, position)
		}
		if revokedAt, _ := recipient.revocation(); revokedAt != 0 && revokedAt <= position {
			self.t.Errorf("%s reaches a device revoked at log position %d, from a state at %d", where, revokedAt, position)
		}
	}
	self.t.Logf("deliveries checked: %v", counts)
}

// Each shrink rotated: in every repo the removed holder read before it, a generation gkm took of cause REMOVE at a pin
// at or past the shrink's entry, and how long after the entry's sync it landed.
func (self *gkmConcurrencyCase) checkShrinksRotated() {
	generations, _ := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	for _, event := range self.eventList() {
		if !event.shrinks() || event.position == 0 {
			continue
		}
		repos := []*gkmConcurrencyRepo{}
		if event.repo != nil {
			repos = append(repos, event.repo)
		} else {
			for _, repo := range self.repoList() {
				if self.holdsAt(repo, event.subject, event.position-1) {
					repos = append(repos, repo)
				}
			}
		}
		for _, repo := range repos {
			var covering *gkmConcurrencyGeneration
			for _, groupGenerations := range generations {
				for _, generation := range groupGenerations {
					if generation.repoId != repo.repoId || generation.rotate.GetAuth().GetVersion() < event.position {
						continue
					}
					if covering == nil || generation.rotate.GetGeneration() < covering.rotate.GetGeneration() {
						covering = generation
					}
				}
			}
			switch {
			case covering == nil:
				self.t.Errorf("%s of %s at log position %d never rotated %s: no generation stands at a pin past it", event.kind, event.subject.name, event.position, repo.name)
			case sidecar.GkmCause(covering.rotate.GetCause()) != sidecar.GkmCauseRemove:
				self.t.Errorf("%s of %s at log position %d rotated %s first by generation %d of cause %d, not REMOVE", event.kind, event.subject.name, event.position, repo.name, covering.rotate.GetGeneration(), covering.rotate.GetCause())
			default:
				self.t.Logf("%s of %s at log position %d: %s's generation %d (REMOVE, at %d) landed %s after the entry's sync", event.kind, event.subject.name, event.position, repo.name, covering.rotate.GetGeneration(), covering.rotate.GetAuth().GetVersion(), covering.time.Sub(event.synced).Round(time.Millisecond))
			}
		}
	}
}

// No device holds a key of a repo it never read, and none of a generation past its removal's rotation, as its own
// keychain shows.
func (self *gkmConcurrencyCase) checkKeychains() {
	generations, _ := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	for _, repo := range self.repoList() {
		newest := self.newestGeneration(repo)
		if newest == nil {
			continue
		}
		groupGenerations := generations[newest.groupId]
		for _, device := range self.deviceList() {
			device.stateLock.Lock()
			dead := device.dead
			device.stateLock.Unlock()
			if dead {
				continue
			}
			ever := false
			removedAt := uint64(0)
			for _, event := range self.eventList() {
				if event.subject != device || event.position == 0 {
					continue
				}
				if event.kind == "revoke-device" || event.kind == "revoke-grant" && event.repo == repo {
					if removedAt == 0 && self.holdsAt(repo, device, event.position-1) {
						removedAt = event.position
					}
				}
			}
			for position := uint64(1); position <= self.lastPosition(); position += 1 {
				if self.holdsAt(repo, device, position) {
					ever = true
					break
				}
			}
			if !ever {
				if device.device.Sidecar().GkmRepoKeyHeld(repo.repoId) {
					self.t.Errorf("%s never read %s and its keychain holds a key of it", device.name, repo.name)
				}
				continue
			}
			if removedAt == 0 {
				continue
			}
			for g, generation := range groupGenerations {
				if generation.rotate.GetAuth().GetVersion() < removedAt {
					continue
				}
				if held := device.device.Sidecar().GkmHeldKey(repo.repoId, g); held != nil {
					self.t.Errorf("%s, removed from %s at log position %d, holds its generation %d, at a pin past the removal", device.name, repo.name, removedAt, g)
				}
			}
		}
	}
}

// The newest position the case filed, admitted or granted its member a repo at.
func (self *gkmConcurrencyCase) lastPosition() uint64 {
	last := uint64(0)
	for _, event := range self.eventList() {
		last = max(last, event.position)
	}
	for _, device := range self.deviceList() {
		last = max(last, device.admission())
	}
	for _, repo := range self.repoList() {
		repo.stateLock.Lock()
		last = max(last, repo.memberGrantAt)
		repo.stateLock.Unlock()
	}
	return last
}

// No device raised a fork, tamper or regression alarm; a revoked device's alarms after its revocation are logged.
func (self *gkmConcurrencyCase) checkAlarms() {
	for _, device := range self.deviceList() {
		device.stateLock.Lock()
		alarms := slices.Clone(device.alarms)
		device.stateLock.Unlock()
		for _, alarm := range alarms {
			if strings.HasSuffix(alarm, "(after its revocation)") {
				self.t.Logf("%s: %s", device.name, alarm)
				continue
			}
			self.t.Errorf("%s raised %s", device.name, alarm)
		}
	}
}

// Every chaos took its request; the claim whose answer was lost stands once, at the commitment its rotator sent.
func (self *gkmConcurrencyCase) checkChaos() {
	self.stateLock.Lock()
	midRotation, midDelivery := self.midRotation, self.midDelivery
	self.stateLock.Unlock()
	for _, armed := range []*gkmConcurrencyChaos{midRotation, midDelivery} {
		if armed == nil {
			self.t.Errorf("a chaos of the shrink phase was never armed")
			continue
		}
		select {
		case <-armed.taken:
		default:
			self.t.Errorf("the %s chaos took no request", armed.name)
		}
	}
	if midRotation == nil || midRotation.request == nil {
		return
	}
	rotate, _, err := readGkmConcurrencyRotate(midRotation.request.body)
	if err != nil {
		self.t.Errorf("the mid-rotation chaos took a claim that does not parse: %v", err)
		return
	}
	generations, _ := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	taken := generations[midRotation.request.groupId][rotate.GetGeneration()]
	switch {
	case midRotation.status == 201 && (taken == nil || !bytes.Equal(taken.rotate.GetCommitment(), rotate.GetCommitment())):
		self.t.Errorf("gkm took the claim of %s's generation %d before the restart lost its answer, and the generation stands under another commitment", self.repoName(sidecar.Id(rotate.GetRepoId())), rotate.GetGeneration())
	case taken == nil:
		self.t.Errorf("no claim of %s's generation %d stands after the restart", self.repoName(sidecar.Id(rotate.GetRepoId())), rotate.GetGeneration())
	default:
		self.t.Logf("the mid-rotation chaos: %s's generation %d stands once, by %s's claim, the block having answered %d before the restart", self.repoName(sidecar.Id(rotate.GetRepoId())), rotate.GetGeneration(), self.nameOf(taken.signerId), midRotation.status)
	}
}

// Accounts for every gkm 401 and 403 since the last call (the file's header): each accounted one is counted by its
// reason and logged with the recorder's evidence; any other fails the case.
func (self *gkmConcurrencyCase) accountRefusals(phase string) {
	for _, refusal := range self.origin.takeRefusals() {
		request := refusal.request
		token := "a bearer with no claims that parse"
		if request.claims != nil {
			token = fmt.Sprintf("a token from log position %d holding %s", request.claims.ProjVersion, self.scopeNames(request.claims.Scope))
		}
		line := fmt.Sprintf("gkm answered %s %d at %s: %s, presented by %s (%s)", request.route, refusal.status, request.time.Format("15:04:05.000"), token, refusal.device, refusal.body)
		reason := self.refusalReason(request)
		if reason == "" {
			self.t.Errorf("%s: an unaccounted refusal: %s", phase, line)
			continue
		}
		self.stateLock.Lock()
		self.refusalAccounts[reason] += 1
		self.stateLock.Unlock()
		self.t.Logf("%s: %s: %s", phase, reason, line)
	}
}

// Why gkm refused request, by the case's record, or "" where nothing it filed explains it.
func (self *gkmConcurrencyCase) refusalReason(request *gkmConcurrencyRequest) string {
	caller := self.deviceOf(request.sidecarId)
	if caller == nil {
		return ""
	}
	if _, revokeFiled := caller.revocation(); !revokeFiled.IsZero() && !request.time.Before(revokeFiled) {
		return "a revoked caller"
	}
	caller.stateLock.Lock()
	dead := caller.dead
	caller.stateLock.Unlock()
	if dead {
		return "a closed caller"
	}
	// §10.6's checks row: gkm refuses a group its repo's newest GroupForm no longer names, once a re-form replaced it
	for _, form := range self.origin.formList() {
		if !form.ladder && form.taken() && request.groupId != (sidecar.Id{}) && bytes.Equal(form.form.GetPreviousGroupId(), request.groupId.Bytes()) && !request.time.Before(form.time) {
			return "a request to a group a re-form replaced"
		}
	}
	repo := self.repoOfGroup(request.groupId)
	if repo == nil && request.repoId != (sidecar.Id{}) {
		repo = self.repoOf(request.repoId)
	}
	if repo == nil || request.claims == nil {
		return ""
	}
	tokenAt := request.claims.ProjVersion
	for _, event := range slices.Backward(self.eventList()) {
		if event.subject != caller || event.repo != repo && event.kind != "revoke-device" {
			continue
		}
		switch {
		case event.kind == "revoke-grant" && !request.time.Before(event.filed):
			return "a caller whose grant on the repo was revoked"
		case event.kind == "grant" && (event.synced.IsZero() || request.time.Before(event.synced)):
			return "gkm's gate behind a grant the caller already holds"
		case event.position != 0 && tokenAt < event.position && self.holdsAt(repo, caller, math.MaxUint64):
			return "a token from before a change of the caller's role (QD-1179's stale stronger scope)"
		}
		break
	}
	return ""
}

// A token's scopes, each repo by the case's name.
func (self *gkmConcurrencyCase) scopeNames(scope string) string {
	names := []string{}
	for _, part := range strings.Fields(scope) {
		if repoId, ok := evsConcurrencyScopeRepoId(part); ok {
			part = self.repoName(repoId) + ":" + part[strings.LastIndex(part, ":")+1:]
		}
		names = append(names, part)
	}
	return strings.Join(names, " ")
}

// What the case says of a refused request's caller: its name, standing and its state's roles on the token's repos.
func (self *gkmConcurrencyCase) describeCaller(request *gkmConcurrencyRequest) string {
	caller := self.deviceOf(request.sidecarId)
	if caller == nil {
		return fmt.Sprintf("sidecar %s, no device of the case", request.sidecarId)
	}
	described := caller.name
	if revokedAt, revokeFiled := caller.revocation(); !revokeFiled.IsZero() {
		described += fmt.Sprintf(", revoked at log position %d", revokedAt)
	}
	follower := caller.device.Follower()
	if follower == nil || follower.Client() == nil || follower.Client().State() == nil || request.claims == nil {
		return described
	}
	state := follower.Client().State()
	acl, err := state.Acl()
	if err != nil {
		return described
	}
	parts := []string{}
	for _, scope := range strings.Fields(request.claims.Scope) {
		repoId, ok := evsConcurrencyScopeRepoId(scope)
		if !ok {
			continue
		}
		rights := acl.EvsAccess(sidecar.AclSidecarSubject(caller.sidecarId), repoId, sidecar.AclTokenAudienceGkm, time.Now()).Rights
		parts = append(parts, self.repoName(repoId)+":"+evsConcurrencyRole(rights))
	}
	return fmt.Sprintf("%s, whose follower's state at log position %d gives it %s", described, state.Pin().GetVersion(), strings.Join(parts, " "))
}

// The case's counts, for the log.
func (self *gkmConcurrencyCase) logSummary() {
	generations, _ := gkmConcurrencyGenerations(self.origin.claimList(), self.origin.formList())
	for _, repo := range self.repoList() {
		if newest := self.newestGeneration(repo); newest != nil {
			self.t.Logf("%s: %d generations at gkm, the newest %d of cause %d", repo.name, len(generations[newest.groupId]), newest.rotate.GetGeneration(), newest.rotate.GetCause())
		}
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.t.Logf("%d devices, %d events filed, %d writes, %d claims, %d deliveries, %d late asks; refusals accounted %v; writes and rotations tried again %v", len(self.devices), len(self.events), self.writeCount, len(self.origin.claimList()), len(self.origin.deliveryList()), len(self.origin.askList()), self.refusalAccounts, self.retryCounts)
}
