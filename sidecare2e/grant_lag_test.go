//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// Acting on a grant api's gate has not applied yet (the e2e plan's e2e-grant-lag-case; QD-1226; the owner's end-to-end
// tests of 2026-11-21). §8.9 gives server gates 5 s to apply a new entry, revocations included, and a device may act
// on a grant its own state already shows while api's gate does not show it yet. QD-1226 has evs's client ride the
// token exchange's invalid_scope out for EvsSettings.ExchangeScopeWaitTimeout (5 s) on a write, a lease and a delivery
// before it ends the call with it, while a read still ends at the first refusal. The other e2e cases sync every gate
// before a device acts on a statement (T3Network.Sync, T3World.SyncGates), so api's gate never lags a grant there.
// Here the exchange's scopes come from a gate of the case's own that applies each entry Lag after it landed, or holds
// it until the case releases it (grant_lag_harness_test.go), while every follower and evs's and gkm's gates hold the
// entry once its filer's sync returns.
//
// One network of ClientCount sidecars runs on the world's real keys, with the evs and gkm blocks. Its document is
// grant 1 alone, so no rule gives a member device evs access and a repo's readers are its admin and its grants (as
// in the gkm case). Its owner's device creates the one repo and grants its writer device writer, and the upgraders
// and the subscribers reader. Every holder takes the repo's key, the owner writes a seed, and each upgrader and
// subscriber reads it, so each holds the ref's head and epoch. Each subscriber's read is then revoked (RepoRevoke),
// and its read token dropped from its token cache, as the cache's own 10 minutes would drop it before a later
// re-grant. The writer commits while they are revoked.
//
// The phases:
//
//   - fresh grants at once, api's gate Lag behind each: every upgrader is granted writer and writes at once, and
//     every subscriber is granted reader again and subscribes at once (its lease), while the writer keeps committing.
//     Each act must land. Its exchanges are refused invalid_scope only while api's gate lacks its grant and granted
//     once it holds it, and the act is done within the window of its first refusal. api's gate must apply every grant
//     within §8.9's 5 s of its landing, and at least one write and one lease must meet the lag. Each subscriber then
//     hears every commit past the version it last read, the ones made while it was revoked among them, once each, in
//     order, with no gap
//   - the first write after a shrink: one subscriber's read is revoked, and at once another is granted writer and
//     writes, api's gate Lag behind both, so its write must rotate the generation before it seals (§10.5). It is held
//     to landing as the first phase's writes are
//   - a grant that never reaches api's gate: one subscriber's read is revoked again, its read token dropped, and the
//     writer commits, rotating past the shrink. Then api's gate is held while another subscriber is granted writer
//     and writes, and the first is granted reader and subscribes. Each must end with the exchange's invalid_scope
//     once the window from its first refusal has passed, no sooner and not much later. Every exchange of its scope
//     is refused, the write asking more than once and the lease at its doubling wait. Once the hold is released both
//     go through
//   - revoked devices: one upgrader's grant is revoked (RepoRevoke) and another upgrader is revoked (SidecarRevoke),
//     at once, api's gate Lag behind, and each writes at once and then reads the seed, its read token dropped first.
//     Neither write nor read may go through, and each refusal must come within Lag and the window of its
//     revocation's landing (QD-1226's cost). Where the exchange, still behind, issued the read's token, evs's own
//     gate must refuse it. api's gate must apply each revocation within §8.9's 5 s; once it has, the exchange refuses
//     the revoked grant's scopes invalid_scope and the revoked device outright
//
// At the end every acknowledged write reads back on the owner's device with its content, at a version of its own;
// no refused write's path stands; and no device raised a fork, tamper, regression or gap alarm.
//
// What it found, in its runs of 2026-11-23 on sidecar 2a4e4b33 (runs 3 and 4 alike):
//
//   - the rotation phase fails, the product's (question e2e-grant-lag-case-gkm-rotation-grant-lag). The new writer's
//     key client rotates inside EnsureWritable, and asks the exchange one token for its state's batch of repo scopes.
//     The exchange, behind the grant, leaves the writer's rw out of that token, and gkm refuses the rotation's claim.
//     QD-1179's single-scope retry does not apply, since the batch already carried the role, so the write ends within
//     half a second of its grant (0.1 s and 0.47 s) with ErrGkmWait ("gkm write gate closed: the generation's pin 23
//     is below holders_shrunk_at 31"), while api's gate applied the grant 2.5 s after it landed. gkm's client rides
//     out no exchange refusal, as QD-1226's question noted
//   - every other check held. The fresh grants' writes and leases were refused at 0, 1 and 2 s and granted near 3 s,
//     api's gate applying each 2.4 s to 2.5 s after it landed, and every subscriber heard every commit. In the never
//     phase the write asked 11 times, about once a second in each of its two uploads' loops, ending 5.03 s and 5.17 s
//     after it began, and the lease at 0, 1, 3 and 5 s, each ending with invalid_scope. The revoked devices' writes
//     were refused within 150 ms by their own key clients or gkm's gate, and their reads by evs's gate, 403 SCOPE, on
//     tokens the lagging exchange had still issued
//
// Not here, each for the reason given:
//
//   - a delivery's own log read riding the window out. A subscription's delivery reads the log under the repo's r
//     scope only after a read or a write of the ref raised the device's pin, and every read asks the same scope
//     first. A read ends at the exchange's first refusal by design (QD-1226: "Reads (evsHttp.send) retry no exchange
//     refusal"), so in a composed client a delivery made right after a grant meets api's gate already holding it, or
//     never starts. The case holds each re-granted subscriber's deliveries to every change past its last read once
//     the gate caught up, which is what the device sees. The delivery's window itself is tested by sidecar's
//     TestEvsDeliveryRaisesAGapAtALogTheExchangeRefusesForGood, over a scripted exchange
//   - a fresh reader's first act. A device that never read the repo reads its head before a lease or a write's plan,
//     and that read ends at the first invalid_scope (QD-1226's residual), so the case's lease and write devices have
//     read the ref before their grant
//   - EvsShare. In a personal network every untagged device is a consented reader of every owner-bound repo, so a
//     share names no new reader. A share to a tagged device or another member's would need the team or tag setup the
//     control-plane case runs, and the exchange's scopes for it come from the same gate the RepoGrants here go through
//
// A failure from a product bug stays failing; the case reports it with its evidence and runs on.
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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server/sidecart3"
)

// What the case runs with (QD-702): every count, path, bound and pause it uses.
type grantLagSettings struct {
	// The devices granted writer at once in the first phase, at least 2, since the revoked phase revokes two of them.
	UpgradeCount int
	// The devices granted reader again at once in the first phase, at least 4, since the never phase acts as two of
	// them, and the rotation phase shrinks the repo's holders by a third's read and grants the fourth writer.
	SubscribeCount int
	// How long api's gate takes each new entry from its first read of it: under §8.9's 5 s, so a grant reaches the
	// gate within the window a device rides out, and over a filer's sync, so a device acting once its grant's sync
	// returned meets it.
	Lag time.Duration
	// §8.9's target for an entry to reach server gates, revocations included.
	GateTarget time.Duration
	// How long a device rides the exchange's invalid_scope out: its own EvsSettings.ExchangeScopeWaitTimeout.
	Window time.Duration
	// How far past its window an act refused to the end may end: its last exchange and the work around it.
	WindowSlack time.Duration
	// The writer's commits while the subscribers are revoked, and while the first phase's devices act.
	InterimCommitCount int
	PhaseCommitCount   int
	// The network's ACL document: grant 1 alone, so no rule gives a member device evs access.
	AclPolicy string
	// §7.9's config_bases_per_min for the case: the document's save publishes a base, which the Community column's 1
	// a minute would hold a minute.
	ConfigBasesPerMinute int64
	// The repo's paths: the seed, and the directories each kind of write goes under.
	SeedPath         string
	InterimDirectory string
	PhaseDirectory   string
	UpgradeDirectory string
	NeverDirectory   string
	RotateDirectory  string
	RevokedDirectory string
	FinalDirectory   string
	// The bursts' pauses and seed.
	Bursts *WorldBurstSettings
	// Each phase's bound, and the setup's.
	PhaseTimeout time.Duration
	// How long a device may take to hold the repo's newest generation, and how often a wait asks its key client.
	KeyTimeout   time.Duration
	PollInterval time.Duration
	// How long api's lagging gate may take to apply an entry the case waits on: past Lag and a load.
	ApplyTimeout time.Duration
	// How long the subscribers may take to hear every change once the writer's last commit landed.
	DeliveryTimeout time.Duration
}

func defaultGrantLagSettings() *grantLagSettings {
	bursts := DefaultWorldBurstSettings()
	bursts.MaxPause = 200 * time.Millisecond
	return &grantLagSettings{
		UpgradeCount:       3,
		SubscribeCount:     4,
		Lag:                2500 * time.Millisecond,
		GateTarget:         5 * time.Second,
		Window:             sidecar.DefaultEvsSettings().ExchangeScopeWaitTimeout,
		WindowSlack:        2 * time.Second,
		InterimCommitCount: 2,
		PhaseCommitCount:   3,
		AclPolicy: worldAclPolicy(
			`{"src": ["autogroup:member", "autogroup:untagged"], "dst": ["autogroup:self"], "via": ["autogroup:self"], "allow": ["dial", "route"]}`,
		),
		ConfigBasesPerMinute: 6000,
		SeedPath:             "seed.txt",
		InterimDirectory:     "interim",
		PhaseDirectory:       "phase",
		UpgradeDirectory:     "upgrade",
		NeverDirectory:       "never",
		RotateDirectory:      "rotate",
		RevokedDirectory:     "revoked",
		FinalDirectory:       "final",
		Bursts:               bursts,
		PhaseTimeout:         4 * time.Minute,
		KeyTimeout:           2 * time.Minute,
		PollInterval:         250 * time.Millisecond,
		ApplyTimeout:         30 * time.Second,
		DeliveryTimeout:      90 * time.Second,
	}
}

// One device of the case. Safe for concurrent use.
type grantLagDevice struct {
	device *sidecart3.T3Device
	name   string
	client *sidecar.EvsClient

	stateLock sync.Mutex
	alarms    []string
	// each version of the repo's main its listener heard, and how often, and each gap
	heard map[uint64]int
	gaps  []uint64
	// counts every change heard, which a wait arms on
	heardCount    *sidecar.MonitorValue[uint64]
	subscriptions []*sidecar.EvsSubscription
	removes       []func()
}

// The device's sidecar id and client id.
func (self *grantLagDevice) sidecarId() sidecar.Id {
	return self.device.Sidecar().Identity().SidecarId
}

func (self *grantLagDevice) clientId() sidecar.Id {
	return self.device.Sidecar().Identity().ClientId
}

// Records an alarm the case refuses.
func (self *grantLagDevice) event(event sidecar.SidecarEvent) {
	line := ""
	switch event.Kind {
	case sidecar.SidecarEventHeadAlarm:
		line = fmt.Sprintf("head_alarm %s", event.HeadAlarm)
	case sidecar.SidecarEventEvsAlarm:
		line = fmt.Sprintf("evs_alarm %s (%s) at version %d", event.EvsAlarm, event.EvsAlarmDetail, event.Version)
	case sidecar.SidecarEventEvsGap:
		line = fmt.Sprintf("evs_gap verified since %s", event.VerifiedSince)
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

// Keeps a change of the repo's main.
func (self *grantLagDevice) hear(mainRefId sidecar.Id, change *sidecar.EvsRepoChange) {
	if change.RefId != mainRefId {
		return
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if change.Gap {
			self.gaps = append(self.gaps, change.Version)
			return
		}
		self.heard[change.Version] += 1
	}()
	self.heardCount.Update(func(count uint64) uint64 { return count + 1 })
}

// The versions in (after, through] the listener has not heard, each heard more than once, and the gaps.
func (self *grantLagDevice) heardState(after uint64, through uint64) (missing []uint64, repeated []string, gaps []uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for version := after + 1; version <= through; version += 1 {
		if self.heard[version] == 0 {
			missing = append(missing, version)
		}
	}
	for version, count := range self.heard {
		if 1 < count {
			repeated = append(repeated, fmt.Sprintf("%d heard %d times", version, count))
		}
	}
	slices.Sort(repeated)
	return missing, repeated, slices.Clone(self.gaps)
}

// Keeps a subscription to close at the case's end.
func (self *grantLagDevice) keepSubscription(subscription *sidecar.EvsSubscription) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.subscriptions = append(self.subscriptions, subscription)
}

// Closes every subscription the device holds.
func (self *grantLagDevice) closeSubscriptions() {
	subscriptions := func() []*sidecar.EvsSubscription {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		subscriptions := self.subscriptions
		self.subscriptions = nil
		return subscriptions
	}()
	for _, subscription := range subscriptions {
		subscription.Close()
	}
}

// One write the case made, acknowledged or refused.
type grantLagWrite struct {
	writer  *grantLagDevice
	path    string
	content []byte
	// the ref version it committed at, 0 for a refused one
	version uint64
}

// One device's act right after a statement about it landed: a write or a lease, its scope, the statement's position,
// when api answered the statement and when its filer's sync ended, the act's own times and its outcome.
type grantLagAct struct {
	device *grantLagDevice
	kind   string
	scope  string
	pin    *bridge.AuthPin
	// when api answered the statement, and when its filer's sync returned
	landedAt time.Time
	syncedAt time.Time
	// the act's own start and end
	startedAt time.Time
	endedAt   time.Time
	err       error
}

// A line naming the act.
func (self *grantLagAct) name() string {
	return fmt.Sprintf("%s's %s on the statement at %d", self.device.name, self.kind, self.pin.GetVersion())
}

// See the file's header.
type grantLagCase struct {
	t        testing.TB
	settings *grantLagSettings
	world    *World
	network  *WorldNetwork
	gate     *grantLagGate
	recorder *grantLagRecorder
	// the network's id as the gate keys it, the repo, its main ref and evs's origin, which every exchange of an evs
	// scope names
	networkId sidecar.Id
	repoId    sidecar.Id
	mainRefId sidecar.Id
	evsOrigin string
	// the roles
	owner       *grantLagDevice
	writer      *grantLagDevice
	upgraders   []*grantLagDevice
	subscribers []*grantLagDevice
	devices     []*grantLagDevice
	// the main version every upgrader and subscriber read before the phases
	seedVersion uint64

	stateLock sync.Mutex
	writes    []*grantLagWrite
	refused   []*grantLagWrite
}

func TestE2eGrantLag(t *testing.T) {
	settings := defaultGrantLagSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		runGrantLag(t, settings)
	})
}

// The case on settings: see the file's header.
func runGrantLag(t testing.TB, settings *grantLagSettings) {
	switch {
	case settings.UpgradeCount < 2 || settings.SubscribeCount < 4:
		t.Fatalf("%d upgraders and %d subscribers have no room for the later phases' roles", settings.UpgradeCount, settings.SubscribeCount)
	case settings.Window <= settings.Lag || settings.GateTarget <= settings.Lag:
		t.Fatalf("a lag of %s does not reach the gate within the window %s and §8.9's %s", settings.Lag, settings.Window, settings.GateTarget)
	}
	worldSettings := DefaultWorldSettings()
	worldSettings.NetworkCount = 1
	worldSettings.ClientsPerNetwork = 2 + settings.UpgradeCount + settings.SubscribeCount
	worldSettings.Blocks = []string{WorldBlockEvs, WorldBlockGkm}
	t3Settings := sidecart3.DefaultT3WorldSettings()
	limits := *t3Settings.SidecarLimits
	limits.LimitKeyValues = maps.Clone(limits.LimitKeyValues)
	limits.LimitKeyValues["config_bases_per_min"] = settings.ConfigBasesPerMinute
	t3Settings.SidecarLimits = &limits
	worldSettings.T3 = t3Settings
	world := NewWorld(t, worldSettings)
	defer world.Close()

	self := &grantLagCase{t: t, settings: settings, world: world, evsOrigin: world.T3().Api().ServiceOrigin(WorldBlockEvs)}
	defer func() {
		for _, device := range self.devices {
			device.closeSubscriptions()
			for _, remove := range device.removes {
				remove()
			}
		}
	}()
	ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
	if err := self.setUp(ctx); err != nil {
		cancel()
		self.failProgress("the setup", err, nil)
	}
	cancel()
	defer self.gate.close()

	self.grantPhase()
	self.rotateFirstPhase()
	self.neverPhase()
	self.revokedPhase()
	self.checkWrites()
	self.checkAlarms()
	total, refused := self.recorder.counts()
	t.Logf("api answered %d token exchanges, %d of them invalid_scope", total, refused)
}

// The network, its document, the repo and its grants, every holder holding its key and having read the seed, and the
// subscribers revoked; then api's lagging gate and record in front of the exchange.
func (self *grantLagCase) setUp(ctx context.Context) error {
	settings := self.settings
	network, err := self.world.AddNetwork(ctx, worldNetworkName(0), 2+settings.UpgradeCount+settings.SubscribeCount, nil)
	if err != nil {
		return fmt.Errorf("the network: %w", err)
	}
	self.network = network
	self.networkId = sidecar.Id(network.NetworkId)
	for _, client := range network.Clients {
		evsClient, err := client.EvsClient()
		if err != nil {
			return fmt.Errorf("%s's evs client: %w", client.Name, err)
		}
		device := &grantLagDevice{device: client, name: client.Name, client: evsClient, heard: map[uint64]int{}, heardCount: sidecar.NewMonitorValue(uint64(0))}
		device.removes = append(device.removes, client.Sidecar().AddEventCallback(device.event))
		self.devices = append(self.devices, device)
	}
	self.owner, self.writer = self.devices[0], self.devices[1]
	self.upgraders = self.devices[2 : 2+settings.UpgradeCount]
	self.subscribers = self.devices[2+settings.UpgradeCount:]

	// api's gate and record before anything the case files, the gate's lag 0 until the phases
	if self.gate, err = newGrantLagGate(self.world); err != nil {
		return err
	}
	self.recorder = installGrantLagRecorder(self.world)

	if _, err := network.SaveAclPolicy(ctx, self.owner.device, []byte(settings.AclPolicy)); err != nil {
		return fmt.Errorf("the network's document: %w", err)
	}
	repo, err := network.CreateRepo(ctx, self.owner.device, protocol.RepoKind_REPO_KIND_GENERAL)
	if err != nil {
		return fmt.Errorf("the repo: %w", err)
	}
	self.repoId = repo.RepoId()
	if _, err := network.Grant(ctx, self.owner.device, self.repoId, self.writer.sidecarId(), protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER); err != nil {
		return fmt.Errorf("the writer's grant: %w", err)
	}
	readers := slices.Concat(self.upgraders, self.subscribers)
	for _, device := range readers {
		if _, err := network.Grant(ctx, self.owner.device, self.repoId, device.sidecarId(), protocol.RepoGrantRole_REPO_GRANT_ROLE_READER); err != nil {
			return fmt.Errorf("%s's read grant: %w", device.name, err)
		}
	}
	for _, device := range slices.Concat([]*grantLagDevice{self.writer}, readers) {
		if err := self.awaitKey(ctx, device); err != nil {
			return err
		}
	}

	// the seed, which each upgrader and subscriber reads, so each holds the ref's head and epoch
	seed, err := self.write(ctx, self.owner, settings.SeedPath)
	if err != nil {
		return err
	}
	self.seedVersion = seed.version
	refId, err := self.owner.client.Repo(self.repoId).RefIdOf(ctx, sidecar.EvsMainRefName)
	if err != nil {
		return fmt.Errorf("the repo's main ref: %w", err)
	}
	self.mainRefId = refId
	for _, device := range readers {
		if err := self.read(ctx, device, seed); err != nil {
			return err
		}
		device.removes = append(device.removes, device.client.AddRepoChangeListener(&self.repoId, func(change *sidecar.EvsRepoChange) {
			device.hear(self.mainRefId, change)
		}))
	}

	// each subscriber's read revoked, applied at api's gate, and its read token gone from its cache
	for _, device := range self.subscribers {
		pin, err := network.FileStatement(ctx, self.owner.device, bridge.SignedType_SIGNED_REPO_REVOKE, &protocol.RepoRevoke{
			RepoId:  self.repoId.Bytes(),
			Subject: device.sidecarId().Bytes(),
		})
		if err != nil {
			return fmt.Errorf("%s's read revoked: %w", device.name, err)
		}
		if _, err := self.gate.awaitApplied(ctx, self.networkId, pin.GetVersion(), settings.ApplyTimeout); err != nil {
			return err
		}
		self.forgetToken(device, "r")
	}
	// the writer commits while they are revoked
	for index := range settings.InterimCommitCount {
		if _, err := self.write(ctx, self.writer, fmt.Sprintf("%s/%d.txt", settings.InterimDirectory, index)); err != nil {
			return err
		}
	}
	return nil
}

// The repo scope of role, as evs's client asks for it.
func (self *grantLagCase) scope(role string) string {
	return "repo:" + self.repoId.String() + ":" + role
}

// Drops the device's token for the repo's role at evs from its token cache: what the cache's own 10 minutes do to a
// token kept from before a revocation by the time a later grant comes.
func (self *grantLagCase) forgetToken(device *grantLagDevice, role string) {
	device.device.Sidecar().ForgetServiceToken(self.evsOrigin, []string{self.scope(role)})
}

// The content a write of path carries.
func grantLagContent(path string) []byte {
	return []byte("grant-lag " + path)
}

// One write of path as device, kept as acknowledged once answered and as refused otherwise. Never retried: a refusal
// is the case's to judge.
func (self *grantLagCase) write(ctx context.Context, device *grantLagDevice, path string) (*grantLagWrite, error) {
	content := grantLagContent(path)
	written := &grantLagWrite{writer: device, path: path, content: content}
	result, err := device.client.Repo(self.repoId).Write(ctx, &sidecar.EvsWriteArgs{Path: path, Content: bytes.NewReader(content), Size: int64(len(content))})
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err != nil {
		self.refused = append(self.refused, written)
		return written, fmt.Errorf("%s's write of %s: %w", device.name, path, err)
	}
	written.version = result.Version
	self.writes = append(self.writes, written)
	return written, nil
}

// device reads written's path fresh, which must hold written's content.
func (self *grantLagCase) read(ctx context.Context, device *grantLagDevice, written *grantLagWrite) error {
	var content bytes.Buffer
	if _, err := device.client.Repo(self.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: written.path, Fresh: true}, &content); err != nil {
		return fmt.Errorf("%s's read of %s: %w", device.name, written.path, err)
	}
	if !bytes.Equal(content.Bytes(), written.content) {
		return fmt.Errorf("%s reads %s as %q, not %q", device.name, written.path, content.String(), written.content)
	}
	return nil
}

// Waits until device's key client holds a generation of the repo, asking every PollInterval.
func (self *grantLagCase) awaitKey(ctx context.Context, device *grantLagDevice) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.KeyTimeout)
	defer cancel()
	for {
		_, err := device.device.Gkm.CurrentRepoKey(ctx, self.repoId)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s holds no key of the repo: %v (%w)", device.name, err, ctx.Err())
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// Waits until device's key client holds the newest generation the owner's holds.
func (self *grantLagCase) awaitNewestKey(ctx context.Context, device *grantLagDevice) error {
	ctx, cancel := context.WithTimeout(ctx, self.settings.KeyTimeout)
	defer cancel()
	for {
		newest, ownerErr := self.owner.device.Gkm.CurrentRepoKey(ctx, self.repoId)
		key, err := device.device.Gkm.CurrentRepoKey(ctx, self.repoId)
		if ownerErr == nil && err == nil && newest.Generation() <= key.Generation() {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s holds no key of the repo at the owner's newest generation: %v, %v (%w)", device.name, err, ownerErr, ctx.Err())
		case <-time.After(self.settings.PollInterval):
		}
	}
}

// granter files signedType's body about act's device, synced at its entry as every statement of the world is (every
// follower and evs's and gkm's gates hold it, api's lagging gate aside), then the device acts at once by run: what
// each burst of the phases runs. act keeps the statement's position, when api answered it and when its sync returned,
// and the act's own times and outcome; a statement that could not be filed is the burst's error.
func (self *grantLagCase) fileThenAct(ctx context.Context, granter *grantLagDevice, signedType bridge.SignedType, body proto.Message, act *grantLagAct, run func(ctx context.Context) error) error {
	pin, err := self.network.FileStatement(ctx, granter.device, signedType, body)
	if err != nil {
		return fmt.Errorf("%s's %s about %s: %w", granter.name, signedType, act.device.name, err)
	}
	act.pin = pin
	act.syncedAt = time.Now()
	// as the entry's {acl_}v event does, in case it has not reached api's lagging gate yet
	self.gate.changed(self.networkId)
	if landedAt, ok := self.recorder.landedAt(pin.GetVersion()); ok {
		act.landedAt = landedAt
	}
	act.startedAt = time.Now()
	act.err = run(ctx)
	act.endedAt = time.Now()
	return nil
}

// A RepoGrant of the repo to device as role.
func (self *grantLagCase) repoGrant(device *grantLagDevice, role protocol.RepoGrantRole) *protocol.RepoGrant {
	return &protocol.RepoGrant{RepoId: self.repoId.Bytes(), Subject: device.sidecarId().Bytes(), Role: role}
}

// A RepoRevoke of every grant of the repo to device.
func (self *grantLagCase) repoRevoke(device *grantLagDevice) *protocol.RepoRevoke {
	return &protocol.RepoRevoke{RepoId: self.repoId.Bytes(), Subject: device.sidecarId().Bytes()}
}

// A burst of one step: act's statement filed by granter, then the act.
func (self *grantLagCase) actBurst(granter *grantLagDevice, signedType bridge.SignedType, body proto.Message, act *grantLagAct, run func(ctx context.Context) error) *WorldBurst {
	return &WorldBurst{
		Name:      act.device.name + "'s " + act.kind,
		StepCount: 1,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			return self.fileThenAct(ctx, granter, signedType, body, act, run)
		},
	}
}

// A write act of device's at path, kept as acknowledged or refused (write).
func (self *grantLagCase) writeAct(device *grantLagDevice, path string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := self.write(ctx, device, path)
		return err
	}
}

// A lease act of device's: a subscription of the repo's whole main, kept to close at the case's end once it stands.
func (self *grantLagCase) leaseAct(device *grantLagDevice) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		subscription, err := device.client.Repo(self.repoId).Subscribe(ctx, &sidecar.EvsSubscribeArgs{RefName: sidecar.EvsMainRefName})
		if err != nil {
			return err
		}
		device.keepSubscription(subscription)
		return nil
	}
}

// The first phase (the file's header).
func (self *grantLagCase) grantPhase() {
	t := self.t
	settings := self.settings
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	self.gate.setLag(self.networkId, settings.Lag)

	acts := []*grantLagAct{}
	bursts := []*WorldBurst{}
	for _, device := range self.upgraders {
		act := &grantLagAct{device: device, kind: "write", scope: self.scope("rw")}
		acts = append(acts, act)
		path := fmt.Sprintf("%s/%s.txt", settings.UpgradeDirectory, device.name)
		bursts = append(bursts, self.actBurst(self.owner, bridge.SignedType_SIGNED_REPO_GRANT, self.repoGrant(device, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER), act, self.writeAct(device, path)))
	}
	for _, device := range self.subscribers {
		act := &grantLagAct{device: device, kind: "lease", scope: self.scope("r")}
		acts = append(acts, act)
		bursts = append(bursts, self.actBurst(self.owner, bridge.SignedType_SIGNED_REPO_GRANT, self.repoGrant(device, protocol.RepoGrantRole_REPO_GRANT_ROLE_READER), act, self.leaseAct(device)))
	}
	bursts = append(bursts, &WorldBurst{
		Name:      self.writer.name + "'s commits",
		StepCount: settings.PhaseCommitCount,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			_, err := self.write(ctx, self.writer, fmt.Sprintf("%s/%d.txt", settings.PhaseDirectory, index))
			return err
		},
	})
	if err := RunBursts(ctx, t, "the fresh grants", bursts, settings.Bursts); err != nil {
		self.failProgress("the fresh grants", err, bursts)
	}
	FailBurstErrors(t, bursts)

	rodeOut := map[string]int{}
	for _, act := range acts {
		if act.pin == nil {
			continue
		}
		if self.checkLanded(ctx, act) {
			rodeOut[act.kind] += 1
		}
	}
	for _, kind := range []string{"write", "lease"} {
		if rodeOut[kind] == 0 {
			t.Errorf("no %s met api's gate behind its grant: each grant's lag of %s had passed before its device acted, so the phase tested no ride-out", kind, settings.Lag)
		}
	}

	// every re-granted subscriber hears each commit past the seed it read, those made while it was revoked among them
	final, err := self.write(ctx, self.writer, settings.FinalDirectory+"/grants.txt")
	if err != nil {
		t.Errorf("the writer's commit after the fresh grants: %v", err)
		return
	}
	subscribed := []*grantLagDevice{}
	for _, act := range acts {
		if act.kind == "lease" && act.pin != nil && act.err == nil {
			subscribed = append(subscribed, act.device)
		}
	}
	self.checkDeliveries(ctx, subscribed, self.seedVersion, final.version)
}

// Holds act, a device's act on a fresh grant, to landing as QD-1226 says: it went through; api's gate applied its
// grant within §8.9's target of its landing; every exchange of the act's scope was refused invalid_scope only while
// api's gate lacked the grant and granted only once it held it; and an act refused at first was done within the
// window of its first refusal. Whether the act met the lag at all.
func (self *grantLagCase) checkLanded(ctx context.Context, act *grantLagAct) bool {
	t := self.t
	settings := self.settings
	if act.err != nil {
		t.Errorf("%s did not land: %v", act.name(), act.err)
		self.logExchanges(act)
	}
	// an act that ended before the gate applied its grant waits for it here, within ApplyTimeout
	appliedAt, err := self.gate.awaitApplied(ctx, self.networkId, act.pin.GetVersion(), settings.ApplyTimeout)
	if err != nil {
		t.Errorf("%s: %v", act.name(), err)
		return false
	}
	if act.landedAt.IsZero() {
		t.Errorf("%s: api's record holds no answer of the statement", act.name())
		return false
	}
	if lag := appliedAt.Sub(act.landedAt); settings.GateTarget < lag {
		t.Errorf("%s: api's gate applied the statement %s after it landed, past §8.9's %s", act.name(), lag.Round(time.Millisecond), settings.GateTarget)
	}
	exchanges := self.recorder.exchangesOf(act.device.clientId(), self.evsOrigin, act.scope, act.startedAt, act.endedAt)
	lines := []string{}
	refused := []*grantLagExchange{}
	granted := false
	for _, exchange := range exchanges {
		lines = append(lines, exchange.describe(act.landedAt))
		switch {
		case exchange.scopeRefused():
			refused = append(refused, exchange)
			if exchange.at.After(appliedAt) {
				t.Errorf("%s: api refused %s invalid_scope at +%s though its gate held the grant from +%s", act.name(), act.scope, exchange.at.Sub(act.landedAt).Round(time.Millisecond), appliedAt.Sub(act.landedAt).Round(time.Millisecond))
			}
		case exchange.grants(act.scope):
			granted = true
			if exchange.answered.Before(appliedAt) {
				t.Errorf("%s: api granted %s at +%s, before its gate held the grant at +%s", act.name(), act.scope, exchange.answered.Sub(act.landedAt).Round(time.Millisecond), appliedAt.Sub(act.landedAt).Round(time.Millisecond))
			}
		default:
			t.Errorf("%s: api answered an exchange of %s %d %s", act.name(), act.scope, exchange.status, exchange.code)
		}
	}
	t.Logf("%s, landed at +0, synced +%s, api's gate +%s, acted +%s to +%s: %s", act.name(), act.syncedAt.Sub(act.landedAt).Round(time.Millisecond), appliedAt.Sub(act.landedAt).Round(time.Millisecond), act.startedAt.Sub(act.landedAt).Round(time.Millisecond), act.endedAt.Sub(act.landedAt).Round(time.Millisecond), strings.Join(lines, "; "))
	if act.err == nil && !granted {
		t.Errorf("%s: no exchange granted %s while it ran", act.name(), act.scope)
	}
	if len(refused) == 0 {
		return false
	}
	if act.err == nil {
		if took := act.endedAt.Sub(refused[0].answered); settings.Window+settings.WindowSlack < took {
			t.Errorf("%s was done %s after its first refusal, past the window %s", act.name(), took.Round(time.Millisecond), settings.Window)
		}
		if act.endedAt.Before(appliedAt) {
			t.Errorf("%s was done before api's gate held its grant", act.name())
		}
	}
	return true
}

// Logs every exchange act's device asked while the act ran, at any service and of any scopes, timed from the
// statement's landing: a failed act's evidence.
func (self *grantLagCase) logExchanges(act *grantLagAct) {
	since := act.landedAt
	if since.IsZero() {
		since = act.startedAt
	}
	lines := []string{}
	for _, exchange := range self.recorder.allOf(act.device.clientId(), act.startedAt, act.endedAt) {
		lines = append(lines, exchange.resource+" "+exchange.describe(since))
	}
	self.t.Logf("%s's exchanges while it ran: %s", act.name(), strings.Join(lines, "; "))
}

// Waits until each device's listener heard every version of the repo's main in (after, through], then holds each to
// having heard each once, with no gap.
func (self *grantLagCase) checkDeliveries(ctx context.Context, devices []*grantLagDevice, after uint64, through uint64) {
	t := self.t
	deadline := time.After(self.settings.DeliveryTimeout)
	for _, device := range devices {
		waited := func() error {
			for {
				// armed before the read, so a change between them wakes the wait
				_, changed := device.heardCount.Get()
				if missing, _, _ := device.heardState(after, through); len(missing) == 0 {
					return nil
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-deadline:
					return fmt.Errorf("the deliveries' bound of %s passed", self.settings.DeliveryTimeout)
				case <-changed:
				}
			}
		}()
		missing, repeated, gaps := device.heardState(after, through)
		if waited != nil || 0 < len(missing) {
			t.Errorf("%s did not hear %v of the repo's main past %d through %d: %v", device.name, missing, after, through, waited)
		}
		for _, line := range repeated {
			t.Errorf("%s heard version %s", device.name, line)
		}
		if 0 < len(gaps) {
			t.Errorf("%s heard gaps at %v", device.name, gaps)
		}
	}
}

// The rotation phase (the file's header): a shrink of the repo's holders, then at once a reader granted writer, which
// writes at once, so its write is the first since the shrink and must rotate the generation before it seals (§10.5:
// nothing is sealed to a generation a non-reader holds). api's gate lags both statements Lag. QD-1226 has a write
// made within §8.9's target of its grant land, so the write is held to landing as the first phase's are.
func (self *grantLagCase) rotateFirstPhase() {
	t := self.t
	settings := self.settings
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	writer, shrunk := self.subscribers[2], self.subscribers[3]
	if err := self.awaitNewestKey(ctx, writer); err != nil {
		self.failProgress("the rotation phase's key", err, nil)
	}
	shrunk.closeSubscriptions()
	act := &grantLagAct{device: writer, kind: "write", scope: self.scope("rw")}
	shrink := &WorldBurst{
		Name:      shrunk.name + "'s revocation and " + writer.name + "'s write",
		StepCount: 1,
		Step: func(ctx context.Context, index int, random *rand.Rand) error {
			if _, err := self.network.FileStatement(ctx, self.owner.device, bridge.SignedType_SIGNED_REPO_REVOKE, self.repoRevoke(shrunk)); err != nil {
				return fmt.Errorf("%s's read revoked: %w", shrunk.name, err)
			}
			path := fmt.Sprintf("%s/%s.txt", settings.RotateDirectory, writer.name)
			return self.fileThenAct(ctx, self.owner, bridge.SignedType_SIGNED_REPO_GRANT, self.repoGrant(writer, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER), act, self.writeAct(writer, path))
		},
	}
	bursts := []*WorldBurst{shrink}
	if err := RunBursts(ctx, t, "the first write after a shrink", bursts, settings.Bursts); err != nil {
		self.failProgress("the first write after a shrink", err, bursts)
	}
	FailBurstErrors(t, bursts)
	if act.pin != nil {
		self.checkLanded(ctx, act)
	}
}

// The never phase (the file's header).
func (self *grantLagCase) neverPhase() {
	t := self.t
	settings := self.settings
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	writer, subscriber := self.subscribers[0], self.subscribers[1]
	// the subscriber reads no more until it is granted again
	subscriber.closeSubscriptions()
	pin, err := self.network.FileStatement(ctx, self.owner.device, bridge.SignedType_SIGNED_REPO_REVOKE, self.repoRevoke(subscriber))
	if err != nil {
		self.failProgress("the never phase's revocation", err, nil)
	}
	if _, err := self.gate.awaitApplied(ctx, self.networkId, pin.GetVersion(), settings.ApplyTimeout); err != nil {
		self.failProgress("the never phase's revocation", err, nil)
	}
	self.forgetToken(subscriber, "r")
	// the writer's commit rotates the generation past the shrink (§10.5), so the held writer-to-be has nothing to rotate
	// and its write meets evs's exchange alone, sealing to the newest generation, which it holds first
	if _, err := self.write(ctx, self.writer, fmt.Sprintf("%s/rotation.txt", settings.NeverDirectory)); err != nil {
		t.Errorf("the writer's commit after the never phase's shrink: %v", err)
	}
	if err := self.awaitNewestKey(ctx, writer); err != nil {
		self.failProgress("the never phase's key", err, nil)
	}

	self.gate.hold(self.networkId)
	write := &grantLagAct{device: writer, kind: "write", scope: self.scope("rw")}
	lease := &grantLagAct{device: subscriber, kind: "lease", scope: self.scope("r")}
	bursts := []*WorldBurst{
		self.actBurst(self.owner, bridge.SignedType_SIGNED_REPO_GRANT, self.repoGrant(writer, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER), write, self.writeAct(writer, fmt.Sprintf("%s/%s-held.txt", settings.NeverDirectory, writer.name))),
		self.actBurst(self.owner, bridge.SignedType_SIGNED_REPO_GRANT, self.repoGrant(subscriber, protocol.RepoGrantRole_REPO_GRANT_ROLE_READER), lease, self.leaseAct(subscriber)),
	}
	if err := RunBursts(ctx, t, "the grants api's gate never applies", bursts, settings.Bursts); err != nil {
		self.gate.release(self.networkId)
		self.failProgress("the grants api's gate never applies", err, bursts)
	}
	FailBurstErrors(t, bursts)
	for _, act := range []*grantLagAct{write, lease} {
		if act.pin != nil {
			self.checkRefusedThroughWindow(act)
		}
	}

	// released, the gate applies both, and each goes through
	self.gate.release(self.networkId)
	for _, act := range []*grantLagAct{write, lease} {
		if act.pin == nil {
			continue
		}
		if _, err := self.gate.awaitApplied(ctx, self.networkId, act.pin.GetVersion(), settings.ApplyTimeout); err != nil {
			self.failProgress("the never phase's release", err, nil)
		}
	}
	if _, err := self.write(ctx, writer, fmt.Sprintf("%s/%s.txt", settings.NeverDirectory, writer.name)); err != nil {
		t.Errorf("once api's gate holds its grant: %v", err)
	}
	if err := self.leaseAct(subscriber)(ctx); err != nil {
		t.Errorf("%s's lease once api's gate holds its grant: %v", subscriber.name, err)
	}
}

// Holds act, a device's act on a grant api's gate never applied, to ending as QD-1226 says: with the exchange's
// invalid_scope, once the window from its first refusal had passed, no sooner and not much later, every exchange of its
// scope refused, and more than one asked.
func (self *grantLagCase) checkRefusedThroughWindow(act *grantLagAct) {
	t := self.t
	settings := self.settings
	if code := grantLagExchangeCode(act.err); code != "invalid_scope" {
		t.Errorf("%s ended with %v, not the exchange's invalid_scope", act.name(), act.err)
		self.logExchanges(act)
	}
	exchanges := self.recorder.exchangesOf(act.device.clientId(), self.evsOrigin, act.scope, act.startedAt, act.endedAt)
	lines := []string{}
	for _, exchange := range exchanges {
		lines = append(lines, exchange.describe(act.startedAt))
		if !exchange.scopeRefused() {
			t.Errorf("%s: api answered an exchange of %s %d %s while its gate held the grant back", act.name(), act.scope, exchange.status, exchange.code)
		}
	}
	t.Logf("%s, acted +0 to +%s: %s", act.name(), act.endedAt.Sub(act.startedAt).Round(time.Millisecond), strings.Join(lines, "; "))
	if len(exchanges) < 2 {
		t.Errorf("%s asked the exchange %d times: it did not ride the refusal out", act.name(), len(exchanges))
		return
	}
	first, last := exchanges[0], exchanges[len(exchanges)-1]
	if last.at.Before(first.answered.Add(settings.Window)) {
		t.Errorf("%s asked last %s after its first refusal, before the window %s had passed", act.name(), last.at.Sub(first.answered).Round(time.Millisecond), settings.Window)
	}
	if took := act.endedAt.Sub(first.at); settings.Window+settings.WindowSlack < took {
		t.Errorf("%s ended %s after its first ask, past the window %s", act.name(), took.Round(time.Millisecond), settings.Window)
	}
}

// A revoked device's read of the seed right after its write.
type grantLagRevokedRead struct {
	startedAt time.Time
	endedAt   time.Time
	err       error
}

// A revoked device's act: a write at once, then a read of the seed with its read token dropped from its cache first,
// so the read asks the exchange, which answers from api's gate, still behind the revocation, while evs's gate holds it.
// The write's error is the act's, and the read's outcome is kept on read.
func (self *grantLagCase) revokedAct(device *grantLagDevice, read *grantLagRevokedRead) func(ctx context.Context) error {
	write := self.writeAct(device, fmt.Sprintf("%s/%s.txt", self.settings.RevokedDirectory, device.name))
	return func(ctx context.Context) error {
		err := write(ctx)
		self.forgetToken(device, "r")
		read.startedAt = time.Now()
		var content bytes.Buffer
		_, read.err = device.client.Repo(self.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: self.settings.SeedPath, Fresh: true}, &content)
		read.endedAt = time.Now()
		return err
	}
}

// The third phase (the file's header).
func (self *grantLagCase) revokedPhase() {
	t := self.t
	settings := self.settings
	ctx, cancel := self.world.StepContext(t.Context(), settings.PhaseTimeout)
	defer cancel()
	ungranted, revoked := self.upgraders[0], self.upgraders[1]
	// a revoked device follows nothing more, so no sync waits on it
	self.network.Retire(revoked.device)
	ungrant := &grantLagAct{device: ungranted, kind: "write", scope: self.scope("rw")}
	revoke := &grantLagAct{device: revoked, kind: "write", scope: self.scope("rw")}
	reads := map[*grantLagAct]*grantLagRevokedRead{ungrant: {}, revoke: {}}
	bursts := []*WorldBurst{
		self.actBurst(self.owner, bridge.SignedType_SIGNED_REPO_REVOKE, self.repoRevoke(ungranted), ungrant, self.revokedAct(ungranted, reads[ungrant])),
		self.actBurst(self.owner, bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
			SidecarId: revoked.sidecarId().Bytes(),
			Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
		}, revoke, self.revokedAct(revoked, reads[revoke])),
	}
	if err := RunBursts(ctx, t, "the revocations", bursts, settings.Bursts); err != nil {
		self.failProgress("the revocations", err, bursts)
	}
	FailBurstErrors(t, bursts)
	for _, act := range []*grantLagAct{ungrant, revoke} {
		if act.pin == nil {
			continue
		}
		read := reads[act]
		if act.err == nil {
			t.Errorf("%s landed after its revocation had reached evs's gate", act.name())
		} else {
			t.Logf("%s was refused %s after its revocation landed: %v", act.name(), act.endedAt.Sub(act.landedAt).Round(time.Millisecond), act.err)
		}
		bound := settings.Lag + settings.Window + settings.WindowSlack
		if !act.landedAt.IsZero() && bound < act.endedAt.Sub(act.landedAt) {
			t.Errorf("%s was refused %s after its revocation landed, past the lag and the window, %s", act.name(), act.endedAt.Sub(act.landedAt).Round(time.Millisecond), bound)
		}
		appliedAt, err := self.gate.awaitApplied(ctx, self.networkId, act.pin.GetVersion(), settings.ApplyTimeout)
		if err != nil {
			t.Errorf("%s: %v", act.name(), err)
			continue
		}
		if lag := appliedAt.Sub(act.landedAt); !act.landedAt.IsZero() && settings.GateTarget < lag {
			t.Errorf("%s: api's gate applied the revocation %s after it landed, past §8.9's %s", act.name(), lag.Round(time.Millisecond), settings.GateTarget)
		}

		// the read after it: refused, by evs's own gate where api's lagging gate still issued the token
		if read.err == nil {
			t.Errorf("%s read the repo %s after its revocation landed", act.device.name, read.startedAt.Sub(act.landedAt).Round(time.Millisecond))
			continue
		}
		if !act.landedAt.IsZero() && bound < read.endedAt.Sub(act.landedAt) {
			t.Errorf("%s's read was refused %s after its revocation landed, past the lag and the window, %s", act.device.name, read.endedAt.Sub(act.landedAt).Round(time.Millisecond), bound)
		}
		exchanges := self.recorder.exchangesOf(act.device.clientId(), self.evsOrigin, self.scope("r"), read.startedAt, read.endedAt)
		lines := []string{}
		issuedBehind := false
		for _, exchange := range exchanges {
			lines = append(lines, exchange.describe(act.landedAt))
			if exchange.grants(self.scope("r")) && exchange.answered.Before(appliedAt) {
				issuedBehind = true
			}
		}
		t.Logf("%s's read %s after its revocation landed, api's gate holding it from +%s, was refused: %v; its exchanges: %s", act.device.name, read.startedAt.Sub(act.landedAt).Round(time.Millisecond), appliedAt.Sub(act.landedAt).Round(time.Millisecond), read.err, strings.Join(lines, "; "))
		if evsError, ok := sidecar.EvsErrorOf(read.err); issuedBehind && (!ok || evsError.Status/100 != 4) {
			t.Errorf("%s's read on a token api's lagging gate issued after the revocation was not refused by evs's gate: %v", act.device.name, read.err)
		}
	}

	// once api's gate holds both, the exchange refuses the revoked grant's scopes and the revoked device outright
	for _, role := range []string{"rw", "r"} {
		self.forgetToken(ungranted, role)
		_, err := ungranted.device.Sidecar().ServiceToken(ctx, self.evsOrigin, []string{self.scope(role)})
		if code := grantLagExchangeCode(err); code != "invalid_scope" {
			t.Errorf("the exchange answered %s's %s after its grant's revocation with %v, not invalid_scope", ungranted.name, self.scope(role), err)
		}
	}
	self.forgetToken(revoked, "r")
	if _, err := revoked.device.Sidecar().ServiceToken(ctx, self.evsOrigin, []string{self.scope("r")}); err == nil {
		t.Errorf("the exchange granted %s a token after its revocation", revoked.name)
	} else {
		t.Logf("the exchange refused %s after its revocation: %v", revoked.name, err)
	}
}

// Every acknowledged write reads back on the owner's device with its content, each at a version of its own, and no
// refused write's path stands.
func (self *grantLagCase) checkWrites() {
	t := self.t
	ctx, cancel := self.world.StepContext(t.Context(), self.settings.PhaseTimeout)
	defer cancel()
	writes, refused := func() ([]*grantLagWrite, []*grantLagWrite) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return slices.Clone(self.writes), slices.Clone(self.refused)
	}()
	versionPaths := map[uint64]string{}
	for _, written := range writes {
		if path, ok := versionPaths[written.version]; ok {
			t.Errorf("%s and %s were both answered at version %d", path, written.path, written.version)
		}
		versionPaths[written.version] = written.path
		if err := self.read(ctx, self.owner, written); err != nil {
			t.Errorf("an acknowledged write: %v", err)
		}
	}
	for _, written := range refused {
		var content bytes.Buffer
		_, err := self.owner.client.Repo(self.repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: written.path, Fresh: true}, &content)
		if !errors.Is(err, sidecar.ErrEvsPathAbsent) {
			t.Errorf("%s's refused write of %s reads on the owner's device as %q, %v", written.writer.name, written.path, content.String(), err)
		}
	}
	t.Logf("%d writes acknowledged and %d refused", len(writes), len(refused))
}

// No device raised a fork, tamper, regression or gap alarm.
func (self *grantLagCase) checkAlarms() {
	for _, device := range self.devices {
		alarms := func() []string {
			device.stateLock.Lock()
			defer device.stateLock.Unlock()
			return slices.Clone(device.alarms)
		}()
		for _, alarm := range alarms {
			self.t.Errorf("%s raised %s", device.name, alarm)
		}
	}
}

// The world's progress dump, after api's lagging gate's positions and the record's counts.
func (self *grantLagCase) failProgress(phase string, err error, bursts []*WorldBurst) {
	if self.gate != nil {
		self.t.Logf("api's lagging gate holds the network at %d", self.gate.applied(self.networkId))
	}
	if self.recorder != nil {
		total, refused := self.recorder.counts()
		self.t.Logf("api answered %d token exchanges, %d of them invalid_scope", total, refused)
	}
	self.world.FailProgress(self.t, phase, err, bursts)
}
