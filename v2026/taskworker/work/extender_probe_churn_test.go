package work

// The uptime probe's false revocations (factury research
// extender-health-2026-11-11 §4 item 5), as root-cause tests that need no
// database, no network and no real clock.
//
// The pass runs through extenderProbeStore against an in-memory store that
// keeps the model's rules: a success clears an address's failure count, the
// sixth failure in a row deactivates it, and losing the last address revokes
// the extender with a signed revocation on the publish queue. The model's
// database tests prove those rules against the tables; what is proved here is
// what the pass decides to tell the store.
//
// Two defects are pinned:
//
//   - a pass whose failures are the prober's own -- every network's extenders
//     failing at once, or the prober unable to reach its own control target --
//     was charged to every extender, so half an hour of a prober fault revoked
//     the whole directory;
//   - an extender revoked for failed probes came back only through a new
//     activation of its own, so one that answered again minutes later stayed
//     out of every directory until its activator next posted.

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The fake clock's start. Passes are ExtenderProbeTimeout apart, as the chain
// runs them.
var testExtenderChurnStart = time.Date(2026, time.November, 11, 12, 0, 0, 0, time.UTC)

// One address the in-memory store holds.
type testChurnAddress struct {
	ipVersion            int
	ip                   netip.Addr
	activateTime         time.Time
	lastProbeTime        *time.Time
	lastProbeSuccessTime *time.Time
	failures             int
	active               bool
}

// One extender the in-memory store holds.
type testChurnExtender struct {
	extenderId      server.Id
	networkId       server.Id
	publicKey       []byte
	active          bool
	revokeTime      *time.Time
	recordIssueTime *time.Time
	// by family
	addresses map[int]*testChurnAddress
}

// One message the store queued, as the signer produced it.
type testChurnPublish struct {
	extenderId server.Id
	kind       int
	message    []byte
}

// The in-memory store of the churn tests.
type testChurnStore struct {
	stateLock sync.Mutex
	// in creation order
	order     []server.Id
	extenders map[server.Id]*testChurnExtender
	publishes []testChurnPublish
}

func newTestChurnStore() *testChurnStore {
	return &testChurnStore{extenders: map[server.Id]*testChurnExtender{}}
}

// Adds one active extender of the network, with an active address on each
// family given, activated at activateTime.
func (self *testChurnStore) addExtender(networkId server.Id, activateTime time.Time, ipVersions ...int) server.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	index := len(self.order) + 1
	extenderId := server.NewId()
	extender := &testChurnExtender{
		extenderId: extenderId,
		networkId:  networkId,
		publicKey:  []byte(fmt.Sprintf("extender-public-key-churn-%06d", index)),
		active:     true,
		addresses:  map[int]*testChurnAddress{},
	}
	for _, ipVersion := range ipVersions {
		ip := netip.AddrFrom4([4]byte{198, 51, byte(index / 250), byte(1 + index%250)})
		if ipVersion == 6 {
			ip = netip.MustParseAddr(fmt.Sprintf("2001:db8:5::%x", index))
		}
		extender.addresses[ipVersion] = &testChurnAddress{
			ipVersion:    ipVersion,
			ip:           ip,
			activateTime: activateTime,
			active:       true,
		}
	}
	self.order = append(self.order, extenderId)
	self.extenders[extenderId] = extender
	return extenderId
}

// A copy of one extender's state, for an assertion.
func (self *testChurnStore) extender(extenderId server.Id) testChurnExtender {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	extender := *self.extenders[extenderId]
	extender.addresses = map[int]*testChurnAddress{}
	for ipVersion, address := range self.extenders[extenderId].addresses {
		copied := *address
		extender.addresses[ipVersion] = &copied
	}
	return extender
}

// The extenders the store holds revoked.
func (self *testChurnStore) revokedCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, extender := range self.extenders {
		if !extender.active {
			count += 1
		}
	}
	return count
}

// The addresses the store holds deactivated.
func (self *testChurnStore) deactivatedCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	count := 0
	for _, extender := range self.extenders {
		for _, address := range extender.addresses {
			if !address.active {
				count += 1
			}
		}
	}
	return count
}

// The messages queued for one extender, oldest first.
func (self *testChurnStore) publishesOf(extenderId server.Id) []testChurnPublish {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	publishes := []testChurnPublish{}
	for _, publish := range self.publishes {
		if publish.extenderId == extenderId {
			publishes = append(publishes, publish)
		}
	}
	return publishes
}

func (self *testChurnStore) activeTargets(ctx context.Context) []*model.NetworkExtenderProbeTarget {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	type ordered struct {
		target        *model.NetworkExtenderProbeTarget
		lastProbeTime *time.Time
	}
	targets := []ordered{}
	for _, extenderId := range self.order {
		extender := self.extenders[extenderId]
		if !extender.active {
			continue
		}
		for _, ipVersion := range []int{4, 6} {
			address, ok := extender.addresses[ipVersion]
			if !ok || !address.active {
				continue
			}
			targets = append(targets, ordered{
				target:        testChurnProbeTarget(extender, address),
				lastProbeTime: address.lastProbeTime,
			})
		}
	}
	// oldest probe first, never probed first of all, as the model reads them
	slices.SortStableFunc(targets, func(a ordered, b ordered) int {
		switch {
		case a.lastProbeTime == nil && b.lastProbeTime == nil:
			return 0
		case a.lastProbeTime == nil:
			return -1
		case b.lastProbeTime == nil:
			return 1
		default:
			return a.lastProbeTime.Compare(*b.lastProbeTime)
		}
	})
	probeTargets := []*model.NetworkExtenderProbeTarget{}
	for _, target := range targets {
		probeTargets = append(probeTargets, target.target)
	}
	return probeTargets
}

// The probe target of one address.
func testChurnProbeTarget(extender *testChurnExtender, address *testChurnAddress) *model.NetworkExtenderProbeTarget {
	return &model.NetworkExtenderProbeTarget{
		ExtenderId: extender.extenderId,
		NetworkId:  extender.networkId,
		PublicKey:  slices.Clone(extender.publicKey),
		TcpPort:    443,
		DnsTld:     connect.DefaultExtenderDnsTld,
		IpVersion:  address.ipVersion,
		Ip:         address.ip,
		Carriers:   []string{connect.ExtenderCarrierTcp},
	}
}

// The model's rules (model.RecordNetworkExtenderProbeResult).
func (self *testChurnStore) recordResult(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	success bool,
	probeTime time.Time,
	maxConsecutiveProbeFailures int,
	signRevocation model.NetworkExtenderRevocationSigner,
) *model.NetworkExtenderProbeOutcome {
	outcome := &model.NetworkExtenderProbeOutcome{}
	if ctx.Err() != nil {
		// a write on a done context raises, as the model's does
		panic(ctx.Err())
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	extender, ok := self.extenders[target.ExtenderId]
	if !ok {
		return outcome
	}
	address, ok := extender.addresses[target.IpVersion]
	if !ok || !address.active {
		return outcome
	}
	probeTime = probeTime.UTC()
	address.lastProbeTime = &probeTime
	if success {
		address.failures = 0
		address.lastProbeSuccessTime = &probeTime
		return outcome
	}
	address.failures += 1
	if address.failures < maxConsecutiveProbeFailures {
		return outcome
	}
	address.active = false
	outcome.AddressDeactivated = true
	for _, sibling := range extender.addresses {
		if sibling.active {
			return outcome
		}
	}
	if !extender.active {
		return outcome
	}
	extender.active = false
	extender.revokeTime = &probeTime
	message, err := signRevocation(testChurnModelExtender(extender), probeTime)
	if err != nil {
		panic(err)
	}
	self.publishes = append(self.publishes, testChurnPublish{
		extenderId: extender.extenderId,
		kind:       model.NetworkExtenderPublishKindRevocation,
		message:    message,
	})
	outcome.ExtenderRevoked = true
	return outcome
}

// The model row a signer reads.
func testChurnModelExtender(extender *testChurnExtender) *model.NetworkExtender {
	return &model.NetworkExtender{
		ExtenderId:      extender.extenderId,
		NetworkId:       extender.networkId,
		PublicKey:       slices.Clone(extender.publicKey),
		TcpPort:         443,
		UdpPort:         443,
		DnsPort:         53,
		DnsTld:          connect.DefaultExtenderDnsTld,
		CountryCode:     "us",
		Active:          extender.active,
		RevokeTime:      extender.revokeTime,
		RecordIssueTime: extender.recordIssueTime,
		DirectoryTier:   connect.ExtenderDirectoryTierOpen,
	}
}

func (self *testChurnStore) discountBlockReports(ctx context.Context, extenderId server.Id, now time.Time) int64 {
	return 0
}

func (self *testChurnStore) logFailure(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	serverName string,
	probeErr error,
	now time.Time,
) {
}

// A churn test's world: the store, the clock and the prober the pass runs
// against, and the operator's root key.
type testChurnWorld struct {
	t             testing.TB
	store         *testChurnStore
	rootPublicKey ed25519.PublicKey

	stateLock sync.Mutex
	now       time.Time
	// answers one probe of one address; nil is a pass
	answer func(target *model.NetworkExtenderProbeTarget) error
}

// Installs the world's store, clock and prober for the length of the test.
func newTestChurnWorld(t testing.TB) *testChurnWorld {
	t.Helper()
	world := &testChurnWorld{
		t:             t,
		store:         newTestChurnStore(),
		rootPublicKey: installTestExtenderWorkConfig(t),
		now:           testExtenderChurnStart,
		answer: func(target *model.NetworkExtenderProbeTarget) error {
			return nil
		},
	}

	previousStore := newExtenderProbeStore
	newExtenderProbeStore = func() extenderProbeStore {
		return world.store
	}
	previousNow := extenderProbeNow
	extenderProbeNow = func() time.Time {
		world.stateLock.Lock()
		defer world.stateLock.Unlock()
		return world.now
	}
	previousProbe := probeExtenderAddress
	probeExtenderAddress = func(
		ctx context.Context,
		target *model.NetworkExtenderProbeTarget,
		serverName string,
		destinationHost string,
	) error {
		world.stateLock.Lock()
		answer := world.answer
		world.stateLock.Unlock()
		return answer(target)
	}
	t.Cleanup(func() {
		newExtenderProbeStore = previousStore
		extenderProbeNow = previousNow
		probeExtenderAddress = previousProbe
	})
	// the prober's own control target answers, unless a test says otherwise
	stubTestChurnControl(t, func(ipVersion int) string {
		return testChurnControlOk
	})
	return world
}

// Sets how every probe answers from the next pass on.
func (self *testChurnWorld) setAnswer(answer func(target *model.NetworkExtenderProbeTarget) error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.answer = answer
}

// Runs one pass at the current time, then moves the clock to the next.
func (self *testChurnWorld) pass() *ExtenderProbeResult {
	self.t.Helper()
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderProbe(&ExtenderProbeArgs{}, clientSession)
	if err != nil {
		self.t.Fatalf("the probe task must not fail on probe failures: %v", err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.now = self.now.Add(ExtenderProbeTimeout)
	return result
}

// What a dial from a prober that has lost its route says.
var errTestChurnProberUnreachable = errors.New("dial tcp: connect: network is unreachable")

// What a dial of an extender that is down says.
var errTestChurnExtenderDown = errors.New("dial tcp: i/o timeout")

func testChurnAlwaysFail(err error) func(target *model.NetworkExtenderProbeTarget) error {
	return func(target *model.NetworkExtenderProbeTarget) error {
		return err
	}
}

// The issue time a queued message carries, verified under the root key.
func (self *testChurnWorld) issueTimeMs(publish testChurnPublish) uint64 {
	self.t.Helper()
	message := &protocol.ExtenderGossipMessage{}
	if err := proto.Unmarshal(publish.message, message); err != nil {
		self.t.Fatalf("a queued message is not a gossip message: %v", err)
	}
	keySet := connect.NewExtenderRootKeySet(self.rootPublicKey)
	switch publish.kind {
	case model.NetworkExtenderPublishKindRevocation:
		body, err := keySet.VerifyRevocation(message.GetRevocation())
		if err != nil {
			self.t.Fatalf("a queued revocation does not verify: %v", err)
		}
		return body.IssueTimeMs
	default:
		body, err := keySet.VerifyRecord(message.GetRecord())
		if err != nil {
			self.t.Fatalf("a queued record does not verify: %v", err)
		}
		return body.IssueTimeMs
	}
}

// Root cause: the pass charged every failure to the extender it was a failure
// of, whatever failed with it. When the prober loses its own path, every
// network's extenders fail together, and six such passes in a row -- half an
// hour -- revoked the whole directory, which then waited on each extender's
// own activator to come back. A pass whose failures span every network is the
// prober's, and must count against no one.
func TestExtenderProbeChurnAProberOutageRevokesNoOne(t *testing.T) {
	world := newTestChurnWorld(t)
	// the share rule alone: the control target says nothing either way
	stubTestChurnControl(t, func(ipVersion int) string {
		return testChurnControlAbsent
	})

	// six operators, two extenders each
	networkIds := []server.Id{}
	extenderIds := []server.Id{}
	for range 6 {
		networkId := server.NewId()
		networkIds = append(networkIds, networkId)
		for range 2 {
			extenderIds = append(extenderIds, world.store.addExtender(networkId, testExtenderChurnStart, 4))
		}
	}

	healthy := world.pass()
	connect.AssertEqual(t, healthy.Probed, 12)
	connect.AssertEqual(t, healthy.Succeeded, 12)

	// the prober loses its route for half an hour and more
	world.setAnswer(testChurnAlwaysFail(errTestChurnProberUnreachable))
	revokedByPasses := 0
	for range 2 * ExtenderMaxConsecutiveProbeFailures {
		revokedByPasses += world.pass().Revoked
	}
	if revoked := world.store.revokedCount(); revoked != 0 || revokedByPasses != 0 {
		t.Fatalf("a prober outage that failed every network at once revoked %d of 12 extenders (%d by the passes' count)", revoked, revokedByPasses)
	}
	for _, extenderId := range extenderIds {
		address := world.store.extender(extenderId).addresses[4]
		if !address.active || address.failures != 0 {
			t.Fatalf("a prober outage was charged to an extender: active=%t failures=%d", address.active, address.failures)
		}
	}

	// the route returns; then one operator's two extenders really go down,
	// and leave after the same half hour as before
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		return nil
	})
	world.pass()
	down := networkIds[0]
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		if target.NetworkId == down {
			return errTestChurnExtenderDown
		}
		return nil
	})
	revoked := 0
	for range ExtenderMaxConsecutiveProbeFailures {
		revoked += world.pass().Revoked
	}
	connect.AssertEqual(t, revoked, 2)
	connect.AssertEqual(t, world.store.revokedCount(), 2)
	for _, extenderId := range extenderIds[:2] {
		if world.store.extender(extenderId).active {
			t.Fatal("an operator's real outage did not revoke its extenders")
		}
	}
}

// Root cause, per family: the prober's IPv6 path is its own. When it is lost,
// every IPv6 address fails while every IPv4 address answers, and the pass
// deactivated every IPv6 address and revoked every IPv6-only extender. A family
// whose failures span every network is judged on its own, and counts against
// no one.
func TestExtenderProbeChurnAProberIpv6OutageDeactivatesNoIpv6Address(t *testing.T) {
	world := newTestChurnWorld(t)
	stubTestChurnControl(t, func(ipVersion int) string {
		return testChurnControlAbsent
	})

	dualStackIds := []server.Id{}
	ipv6OnlyIds := []server.Id{}
	for range 5 {
		networkId := server.NewId()
		dualStackIds = append(dualStackIds, world.store.addExtender(networkId, testExtenderChurnStart, 4, 6))
		ipv6OnlyIds = append(ipv6OnlyIds, world.store.addExtender(networkId, testExtenderChurnStart, 6))
	}
	world.pass()

	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		if target.IpVersion == 6 {
			return errTestChurnProberUnreachable
		}
		return nil
	})
	for range 2 * ExtenderMaxConsecutiveProbeFailures {
		world.pass()
	}
	if deactivated := world.store.deactivatedCount(); deactivated != 0 {
		t.Fatalf("a prober IPv6 outage deactivated %d of 10 IPv6 addresses and revoked %d of 5 IPv6-only extenders", deactivated, world.store.revokedCount())
	}
	for _, extenderId := range ipv6OnlyIds {
		if !world.store.extender(extenderId).active {
			t.Fatal("a prober IPv6 outage revoked an IPv6-only extender")
		}
	}
	for _, extenderId := range dualStackIds {
		if address := world.store.extender(extenderId).addresses[6]; address.failures != 0 {
			t.Fatalf("a prober IPv6 outage was charged to a dual-stack extender: failures=%d", address.failures)
		}
	}
}

// Root cause: with too few networks for the failures to tell the prober from
// the extenders, a pass the prober itself could not make -- its own control
// target unreachable over the family -- was charged to every extender all the
// same. A control that answered before and fails now holds the pass.
func TestExtenderProbeChurnAFailedControlTargetRevokesNoOne(t *testing.T) {
	world := newTestChurnWorld(t)
	control := testChurnControlOk
	stubTestChurnControl(t, func(ipVersion int) string {
		return control
	})

	extenderIds := []server.Id{
		world.store.addExtender(server.NewId(), testExtenderChurnStart, 4),
		world.store.addExtender(server.NewId(), testExtenderChurnStart, 4),
	}
	// the control target answers, and so does every extender
	world.pass()

	// the prober loses its route: the control fails with every extender
	control = testChurnControlFailed
	world.setAnswer(testChurnAlwaysFail(errTestChurnProberUnreachable))
	for range 2 * ExtenderMaxConsecutiveProbeFailures {
		world.pass()
	}
	if revoked := world.store.revokedCount(); revoked != 0 {
		t.Fatalf("a prober that could not reach its own control target revoked %d of 2 extenders", revoked)
	}
	for _, extenderId := range extenderIds {
		extender := world.store.extender(extenderId)
		if !extender.active || extender.addresses[4].failures != 0 {
			t.Fatalf("a pass whose control target failed was charged to an extender: active=%t failures=%d", extender.active, extender.addresses[4].failures)
		}
	}
}

// Root cause: an extender revoked for failed probes was never probed again, so
// one that answered minutes later stayed out of every directory until its own
// activator posted, which a released activator does on its daily tick or when
// it sees its own revocation, and only gossip carries that. An extender the
// probes revoked is probed for its return, and once it answers it is active
// again, with a record newer than its revocation queued, so every directory
// that holds the revocation takes it back.
func TestExtenderProbeChurnARevokedExtenderThatAnswersAgainReturns(t *testing.T) {
	world := newTestChurnWorld(t)

	extenderId := world.store.addExtender(server.NewId(), testExtenderChurnStart, 4)
	world.pass()

	// a real outage of half an hour: revoked, as before
	world.setAnswer(testChurnAlwaysFail(errTestChurnExtenderDown))
	revoked := 0
	for range ExtenderMaxConsecutiveProbeFailures {
		revoked += world.pass().Revoked
	}
	connect.AssertEqual(t, revoked, 1)
	if world.store.extender(extenderId).active {
		t.Fatal("six consecutive failures did not revoke the extender")
	}
	publishes := world.store.publishesOf(extenderId)
	connect.AssertEqual(t, len(publishes), 1)
	connect.AssertEqual(t, publishes[0].kind, model.NetworkExtenderPublishKindRevocation)
	revocationIssueTimeMs := world.issueTimeMs(publishes[0])

	// while it stays down it stays revoked, and nothing more is published
	for range 3 {
		world.pass()
	}
	if world.store.extender(extenderId).active {
		t.Fatal("an extender that never answered came back")
	}
	connect.AssertEqual(t, len(world.store.publishesOf(extenderId)), 1)

	// it answers again
	world.setAnswer(func(target *model.NetworkExtenderProbeTarget) error {
		return nil
	})
	world.pass()

	extender := world.store.extender(extenderId)
	if !extender.active || !extender.addresses[4].active {
		t.Fatalf("a revoked extender that answered again is still out of the directory: active=%t address active=%t", extender.active, extender.addresses[4].active)
	}
	connect.AssertEqual(t, extender.revokeTime == nil, true)
	connect.AssertEqual(t, extender.addresses[4].failures, 0)
	publishes = world.store.publishesOf(extenderId)
	connect.AssertEqual(t, len(publishes), 2)
	connect.AssertEqual(t, publishes[1].kind, model.NetworkExtenderPublishKindRecord)
	if recordIssueTimeMs := world.issueTimeMs(publishes[1]); recordIssueTimeMs <= revocationIssueTimeMs {
		t.Fatalf("the returning record (%d) is not newer than the revocation (%d), so a directory keeps the extender revoked (B5)", recordIssueTimeMs, revocationIssueTimeMs)
	}

	// and it is an active address like any other from then on
	result := world.pass()
	connect.AssertEqual(t, result.Probed, 1)
	connect.AssertEqual(t, result.Succeeded, 1)
}
