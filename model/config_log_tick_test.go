package model

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// §7.9's ticks and retention at api (TEAM-M2-14).

// api appends only a tick its verifier takes at the tip: every reaped id an ephemeral admission, every
// activation an open proposal's whose window ran; a refused tick appends nothing.
func TestConfigLogTickIsCheckedAtTheTip(t *testing.T) {
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	suite := testConfigLogSuiteSequencer{sequencer: sequencer}
	if err := world.Start(suite); err != nil {
		t.Fatal(err)
	}
	networkId := server.Id(world.NetworkId())
	tip, err := sequencer.Tip(t.Context(), networkId)
	if err != nil {
		t.Fatal(err)
	}

	genesis, err := testConfigLogGenesisOf(world.SignedGenesis())
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range []*bridge.ApiTick{
		{ReapedSidecarIds: [][]byte{genesis.GetOwnerSidecarId()}},
		{ReapedSidecarIds: [][]byte{sidecar.NewId().Bytes()}},
		{ReapedSidecarIds: [][]byte{[]byte("short")}},
		{ActivatedProposalIds: [][]byte{make([]byte, 32)}},
	} {
		_, err := sequencer.AppendTick(t.Context(), networkId, tick)
		configLogError := &ConfigLogError{}
		if !errors.As(err, &configLogError) || configLogError.Status != http.StatusForbidden {
			t.Fatalf("tick %v: %v", tick, err)
		}
	}
	after, err := sequencer.Tip(t.Context(), networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, after.GetVersion(), tip.GetVersion())
	connect.AssertEqual(t, bytes.Equal(after.GetChain(), tip.GetChain()), true)

	// with no live ephemeral admission there is no tick to make
	result, err := appendConfigLogTickAt(t.Context(), sequencer, networkId, DefaultConfigLogTickSettings())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.N == nil && !result.More, true)
	live, err := sequencer.LiveEphemeralAdmissions(t.Context(), networkId, 10)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(live), 0)
}

// The Genesis a world's Signed Genesis carries.
func testConfigLogGenesisOf(signedGenesis []byte) (*bridge.Genesis, error) {
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedGenesis, signed); err != nil {
		return nil, err
	}
	message, err := sidecar.ParseConfigStatement(bridge.SignedPurposeAuthority, bridge.SignedType_SIGNED_GENESIS, signed.GetBody())
	if err != nil {
		return nil, err
	}
	genesis, ok := message.(*bridge.Genesis)
	if !ok {
		return nil, errors.New("not a genesis")
	}
	return genesis, nil
}

// The retention point: the newest base at least 30 days old sets it, from its folded_through and the base
// before it; younger bases retire nothing yet, and the first base keeps everything.
func TestConfigLogRetentionPoint(t *testing.T) {
	day := 24 * time.Hour
	keep := 30 * day
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	at := func(age time.Duration) int64 {
		return now.Add(-age).UnixMilli()
	}
	bases := []*configLogBaseInfo{
		{n: 1, timeMs: at(90 * day), foldedThrough: 0},
		{n: 1100, timeMs: at(60 * day), foldedThrough: 1099},
		{n: 2200, timeMs: at(31 * day), foldedThrough: 1500},
		{n: 3300, timeMs: at(2 * day), foldedThrough: 3299},
	}
	cases := []struct {
		bases []*configLogBaseInfo
		point uint64
	}{
		{bases: bases[:1], point: 1},
		{bases: bases[:2], point: 1},
		// base 2200, 31 days old: min(1100, 1501)
		{bases: bases[:3], point: 1100},
		// base 3300 is 2 days old, so 2200 still decides
		{bases: bases, point: 1100},
		// with 3300 past 30 days too: min(2200, 3300)
		{bases: append(append([]*configLogBaseInfo{}, bases[:3]...), &configLogBaseInfo{n: 3300, timeMs: at(31 * day), foldedThrough: 3299}), point: 2200},
		// a fold that stayed: min(previous base, folded_through + 1)
		{bases: []*configLogBaseInfo{{n: 1, timeMs: at(90 * day)}, {n: 50, timeMs: at(40 * day), foldedThrough: 0}}, point: 1},
		{bases: nil, point: 1},
	}
	for _, c := range cases {
		connect.AssertEqual(t, configLogRetentionPoint(c.bases, now.UnixMilli(), keep), c.point)
	}
	retained := configLogRetainedRows([]*configLogBaseInfo{
		{n: 10, rowsSha256: []byte("a")},
		{n: 20, rowsSha256: []byte("b")},
		{n: 30, rowsSha256: []byte("a")},
	}, 20)
	connect.AssertEqual(t, retained, map[string]bool{"a": true, "b": true})
}

// A pass deletes the entries below the point, never entry 0 or one at or past it, and the rows no retained
// base names once 30 days old; bounded passes resume where the last stopped.
func TestConfigLogRetentionDeletesOnlyPastThePoint(t *testing.T) {
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	networkId := sidecar.NewId()
	other := sidecar.NewId()
	put := func(key string) {
		if _, err := cluster.PutIfNoneMatch(ctx, key, []byte(key), nil); err != nil {
			t.Fatal(err)
		}
	}
	for n := uint64(0); n <= 12; n += 1 {
		put(sidecar.ConfigEntryKey(networkId, n))
	}
	put(sidecar.ConfigEntryKey(other, 1))
	rowsSha := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	put(sidecar.ConfigRowsKey(networkId, rowsSha(1)))
	put(sidecar.ConfigRowsKey(networkId, rowsSha(2)))
	clock.Advance(31 * 24 * time.Hour)
	// rows written just now, as an append writes them ahead of its entry
	put(sidecar.ConfigRowsKey(networkId, rowsSha(3)))

	settings := DefaultConfigLogRetentionSettings()
	settings.DeleteMaxCount = 3
	settings.ListPageCount = 2
	retained := map[string]bool{string(rowsSha(2)): true}
	deletedCount := 0
	for pass := 0; ; pass += 1 {
		result, err := retainConfigLogBelow(ctx, cluster, networkId, 8, retained, clock.Now(), settings)
		if err != nil {
			t.Fatal(err)
		}
		deletedCount += result.DeletedCount
		if !result.More {
			break
		}
		if 10 < pass {
			t.Fatal("retention never finished")
		}
	}
	// entries 1..7 and the old unnamed rows
	connect.AssertEqual(t, deletedCount, 8)
	exists := func(key string) bool {
		_, err := cluster.Head(ctx, key)
		return err == nil
	}
	connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(networkId, 0)), true)
	for n := uint64(1); n < 8; n += 1 {
		connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(networkId, n)), false)
	}
	for n := uint64(8); n <= 12; n += 1 {
		connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(networkId, n)), true)
	}
	connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(other, 1)), true)
	connect.AssertEqual(t, exists(sidecar.ConfigRowsKey(networkId, rowsSha(1))), false)
	connect.AssertEqual(t, exists(sidecar.ConfigRowsKey(networkId, rowsSha(2))), true)
	connect.AssertEqual(t, exists(sidecar.ConfigRowsKey(networkId, rowsSha(3))), true)

	// a point of 1 retires nothing
	result, err := retainConfigLogBelow(ctx, cluster, other, 1, nil, clock.Now(), settings)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.DeletedCount, 0)
}

// A network whose log holds only base 1 keeps everything: its retention point is 1.
func TestConfigLogRetentionKeepsASingleBase(t *testing.T) {
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	suite := testConfigLogSuiteSequencer{sequencer: sequencer}
	if err := world.Start(suite); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := world.PostMemberAdmit(suite); err != nil {
			t.Fatal(err)
		}
	}
	networkId := server.Id(world.NetworkId())
	result, err := sequencer.RetainNetwork(t.Context(), networkId, world.Clock().Now().Add(400*24*time.Hour), DefaultConfigLogRetentionSettings())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Point, uint64(1))
	connect.AssertEqual(t, result.DeletedCount, 0)
	read, err := sequencer.Read(t.Context(), networkId, 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(read.Entries), 4)
	connect.AssertEqual(t, read.SignedGenesis != nil, true)
}

// The admission suite's api over server's sequencer on T2's fake cluster: its three appends and the read; it
// appends nothing its own check refuses.
type testConfigLogAdmissionApi struct {
	testConfigLogSuiteSequencer
}

func (self testConfigLogAdmissionApi) ReadConfigProjection(ctx context.Context, networkId sidecar.Id, after uint64) (*sidecar.ConfigProjection, error) {
	read, err := self.sequencer.Read(ctx, server.Id(networkId), after)
	if err != nil {
		return nil, testConfigLogSuiteError(err)
	}
	return &sidecar.ConfigProjection{SignedGenesis: read.SignedGenesis, Entries: read.Entries, Rows: read.Rows, Tip: read.Tip, More: read.More}, nil
}

func (self testConfigLogAdmissionApi) SequenceAnyway(ctx context.Context, networkId sidecar.Id, kind uint32, body []byte) (*bridge.AuthPin, error) {
	return nil, errors.New("server's sequencer appends nothing its check refuses")
}

// One activation task the sequencer's hook asked for.
type testConfigLogActivationSchedule struct {
	proposalId []byte
	runAt      time.Time
}

// A network of the admission suite on server's sequencer, every reader of it following each entry, with the
// activation tasks the sequencer schedules kept in order, as taskworker would hold them.
type testConfigLogActivationNet struct {
	*sidecartest.AdmissionNet
	t         *testing.T
	cluster   sidecar.BlobCluster
	sequencer *ConfigLogSequencer
	scheduled []testConfigLogActivationSchedule
	settings  *ConfigLogTickSettings
	// the sequencer's cap on a tick's activations, nil for none
	activationCap func(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, tick *bridge.ApiTick, timeMs int64) error
}

func newTestConfigLogActivationNet(t *testing.T) *testConfigLogActivationNet {
	return newTestConfigLogActivationNetWithCap(t, nil)
}

func newTestConfigLogActivationNetWithCap(
	t *testing.T,
	activationCap func(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, tick *bridge.ApiTick, timeMs int64) error,
) *testConfigLogActivationNet {
	self := &testConfigLogActivationNet{t: t, cluster: testConfigLogFakeCluster(t), settings: DefaultConfigLogTickSettings(), activationCap: activationCap}
	suiteSettings := sidecartest.DefaultAdmissionSuiteSettings()
	suiteSettings.ForgedAppends = false
	tier := &sidecartest.AdmissionTier{
		Cluster:  self.cluster,
		Settings: suiteSettings,
		NewApi: func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
			settings := DefaultConfigLogSettings()
			settings.Clock = clock
			settings.ActivationCap = self.activationCap
			settings.Activation = func(ctx context.Context, networkId server.Id, proposalId []byte, runAt time.Time) {
				if networkId != server.Id(self.NetworkId()) {
					t.Errorf("an activation scheduled for network %s", networkId)
				}
				self.scheduled = append(self.scheduled, testConfigLogActivationSchedule{proposalId: bytes.Clone(proposalId), runAt: runAt})
			}
			self.sequencer = NewConfigLogSequencer(cluster, settings, nil)
			return testConfigLogAdmissionApi{testConfigLogSuiteSequencer{sequencer: self.sequencer}}
		},
	}
	self.AdmissionNet = sidecartest.NewAdmissionNet(t, tier)
	t.Cleanup(self.AdmissionNet.Close)
	return self
}

// The schedules since the last take.
func (self *testConfigLogActivationNet) takeScheduled() []testConfigLogActivationSchedule {
	scheduled := self.scheduled
	self.scheduled = nil
	return scheduled
}

// The one schedule since the last take, which must be of proposalId.
func (self *testConfigLogActivationNet) mustSchedule(proposalId []byte) time.Time {
	self.t.Helper()
	scheduled := self.takeScheduled()
	if len(scheduled) != 1 || !bytes.Equal(scheduled[0].proposalId, proposalId) {
		self.t.Fatalf("scheduled %v", scheduled)
	}
	return scheduled[0].runAt
}

// One run of the proposal's activation task at the clock, then every reader follows what it appended.
func (self *testConfigLogActivationNet) run(proposalId []byte) *AppendConfigLogActivationResult {
	self.t.Helper()
	result, err := appendConfigLogActivationAt(self.Context(), self.sequencer, server.Id(self.NetworkId()), proposalId, self.settings)
	if err != nil {
		self.t.Fatal(err)
	}
	self.Sync()
	return result
}

// Moves api's clock to at.
func (self *testConfigLogActivationNet) waitUntil(at time.Time) {
	self.t.Helper()
	wait := at.Sub(self.Clock().Now())
	if wait < 0 {
		self.t.Fatalf("the clock is %s past %s", -wait, at)
	}
	self.Wait(wait)
}

// The owner's recovery key, bound.
func (self *testConfigLogActivationNet) ownerKey() *sidecartest.AdmissionRecoveryKey {
	self.t.Helper()
	owner := self.Owner()
	key := self.RecoveryKey(owner.MemberId, "owner")
	self.MustLand(self.Sign(owner, bridge.SignedType_SIGNED_RECOVERY_BIND, self.RecoveryBind(key)))
	return key
}

// §22.7's recovery window row on server's sequencer: api takes the activation ticks its readers take and
// refuses the early, vetoed, voided and repeated ones, a new device and a gate at each base agreeing after
// every entry.
func TestConfigLogSequencerPassesTheRecoveryWindowRow(t *testing.T) {
	suiteSettings := sidecartest.DefaultAdmissionSuiteSettings()
	suiteSettings.ForgedAppends = false
	sidecartest.RunAdmissionRow(t, "the recovery window", &sidecartest.AdmissionTier{
		Cluster:  testConfigLogFakeCluster(t),
		Settings: suiteSettings,
		NewApi: func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
			settings := DefaultConfigLogSettings()
			settings.Clock = clock
			return testConfigLogAdmissionApi{testConfigLogSuiteSequencer{sequencer: NewConfigLogSequencer(cluster, settings, nil)}}
		},
	})
}

// A recovery admission activates 72 h after its newest recovery signature (D-TEAM-21; QD-745): the proposal's
// entry schedules its task at window_time_ms + 72 h, and so does a retry of it; a run before then appends nothing and names the
// window's end; the run at it appends one ApiTick naming the proposal, which every reader takes; a run after
// that does nothing. A tick naming it a millisecond early is refused at api and appends nothing.
func TestARecoveryAdmissionActivatesAtItsWindowsEnd(t *testing.T) {
	net := newTestConfigLogActivationNet(t)
	networkId := server.Id(net.NetworkId())
	key := net.ownerKey()
	if scheduled := net.takeScheduled(); len(scheduled) != 0 {
		t.Fatalf("a binding scheduled %v", scheduled)
	}
	phone := net.Device("owner phone", net.Owner().MemberId)
	signed := net.SignByRecovery(key, bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL, net.Proposal(bridge.SignedType_SIGNED_SIDECAR_ADMIT, net.Admission(phone, nil, false, 0, 0)))
	proposed := net.MustLand(signed)
	proposalId := sidecartest.AdmissionStatementId(signed)
	window := net.Window(proposalId)
	due := time.UnixMilli(window.Row.GetWindowTimeMs()).Add(72 * time.Hour)
	if runAt := net.mustSchedule(proposalId); !runAt.Equal(due) {
		t.Fatalf("scheduled at %s, the window ends %s", runAt, due)
	}
	// a retry of the same bytes answers the entry and schedules what it owes again, for a first answer lost
	// with its process
	if pin, err := net.Post(signed); err != nil || pin.GetVersion() != proposed.Pin.GetVersion() {
		t.Fatalf("the retry: %v, %v", pin, err)
	}
	if runAt := net.mustSchedule(proposalId); !runAt.Equal(due) {
		t.Fatalf("the retry scheduled at %s, the window ends %s", runAt, due)
	}

	// a run at once, and one a millisecond before the window's end
	for _, early := range []time.Time{net.Clock().Now(), due.Add(-time.Millisecond)} {
		net.waitUntil(early)
		tip := net.Tip().GetVersion()
		result := net.run(proposalId)
		if result.N != nil || result.RunAt == nil || !result.RunAt.Equal(due) {
			t.Fatalf("a run at %s: %+v", early, result)
		}
		if net.Tip().GetVersion() != tip || net.Admitted(phone) {
			t.Fatal("an early run appended")
		}
	}
	// a forged early tick: api's own check refuses it
	_, err := net.sequencer.AppendTick(net.Context(), networkId, &bridge.ApiTick{ActivatedProposalIds: [][]byte{proposalId}})
	configLogError := &ConfigLogError{}
	if !errors.As(err, &configLogError) || configLogError.Status != http.StatusForbidden || !errors.Is(err, sidecar.ErrConfigRule) {
		t.Fatalf("an early tick: %v", err)
	}
	net.Sync()
	if net.Admitted(phone) {
		t.Fatal("an early tick admitted")
	}

	net.waitUntil(due)
	result := net.run(proposalId)
	if result.N == nil || result.RunAt != nil {
		t.Fatalf("the run at the window's end: %+v", result)
	}
	if verdict := net.VerdictAt(*result.N); !verdict.Accepted() || verdict.Kind != bridge.AuthEntryKindTick {
		t.Fatalf("the tick at %d: %v", *result.N, verdict.Refusal)
	}
	if !net.Admitted(phone) || net.Row(phone).GetAdmittedAt() != *result.N {
		t.Fatalf("the phone: %v", net.Row(phone))
	}
	if closed := net.Window(proposalId); closed.Closed != sidecar.ConfigProposalActivated || closed.ClosedAt != *result.N {
		t.Fatalf("the window: %+v", closed)
	}
	net.CheckAgreement()

	tip := net.Tip().GetVersion()
	net.Wait(2 * time.Minute)
	if again := net.run(proposalId); again.N != nil || again.RunAt != nil || net.Tip().GetVersion() != tip {
		t.Fatalf("a run after the activation: %+v", again)
	}
	// a tick was no statement, so nothing was scheduled by it
	if scheduled := net.takeScheduled(); len(scheduled) != 0 {
		t.Fatalf("scheduled %v", scheduled)
	}
}

// A task that runs after its proposal closed does nothing: a veto an hour before the window's end, a
// RecoveryRebind of the subject, the proposal's own not_after, and a proposal this log never held.
func TestAnActivationTaskAfterItsProposalClosedDoesNothing(t *testing.T) {
	net := newTestConfigLogActivationNet(t)
	owner := net.Owner()
	key := net.ownerKey()
	phone, tablet, watch := net.Device("owner phone", owner.MemberId), net.Device("owner tablet", owner.MemberId), net.Device("owner watch", owner.MemberId)
	nothing := func(name string, proposalId []byte) {
		t.Helper()
		tip := net.Tip().GetVersion()
		if result := net.run(proposalId); result.N != nil || result.RunAt != nil || net.Tip().GetVersion() != tip {
			t.Fatalf("%s: %+v", name, result)
		}
	}

	vetoedId := net.ProposeByRecovery(key, phone)
	due := net.mustSchedule(vetoedId)
	net.waitUntil(due.Add(-time.Hour))
	net.MustLand(net.Sign(owner, bridge.SignedType_SIGNED_RECOVERY_VETO, net.RecoveryVeto(vetoedId)))
	if scheduled := net.takeScheduled(); len(scheduled) != 0 {
		t.Fatalf("a veto scheduled %v", scheduled)
	}
	net.waitUntil(due)
	nothing("vetoed", vetoedId)
	if net.Admitted(phone) {
		t.Fatal("a vetoed admission activated")
	}

	voidedId := net.ProposeByRecovery(key, tablet)
	due = net.mustSchedule(voidedId)
	next := net.RecoveryKey(owner.MemberId, "owner next")
	net.MustLand(net.Sign(owner, bridge.SignedType_SIGNED_RECOVERY_REBIND, net.RecoveryRebind(next, bridge.RecoveryRebindReason_RECOVERY_REBIND_REASON_ROUTINE)))
	net.waitUntil(due)
	nothing("voided", voidedId)
	if net.Admitted(tablet) {
		t.Fatal("a voided admission activated")
	}

	// a proposal good for an hour: its window outlasts it
	short := net.Proposal(bridge.SignedType_SIGNED_SIDECAR_ADMIT, net.Admission(watch, nil, false, 0, 0))
	short.Header = net.HeaderFor(time.Hour)
	signed := net.SignByRecovery(next, bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL, short)
	net.MustLand(signed)
	expiredId := sidecartest.AdmissionStatementId(signed)
	due = net.mustSchedule(expiredId)
	net.waitUntil(due)
	nothing("expired", expiredId)
	if net.Admitted(watch) {
		t.Fatal("an expired admission activated")
	}

	nothing("unknown", bytes.Repeat([]byte{7}, 32))
	nothing("malformed", []byte("short"))
	net.CheckAgreement()
}

// k = 2 (D-TEAM-21): every signature on a proposal a window holds schedules its task at the window's end as
// the row then has it. An admin's cosign of a recovery-signed proposal leaves the window where it was; a
// recovery cosign on an admin's proposal starts one there, so the run at the old time moves itself to the new
// end; a run before the k-th signature finds the tip taking no activation and looks again an hour on; and a
// veto after the recovery cosign wins.
func TestALaterSignatureMovesTheActivation(t *testing.T) {
	net := newTestConfigLogActivationNet(t)
	owner := net.Owner()
	memberB := net.AdmitMember(owner, "admin b", bridge.MemberRole_MEMBER_ROLE_MEMBER)
	memberC := net.AdmitMember(owner, "admin c", bridge.MemberRole_MEMBER_ROLE_MEMBER)
	adminB, adminC := net.Device("admin b laptop", memberB), net.Device("admin c laptop", memberC)
	net.AdmitDevice(owner, adminB, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED))
	net.AdmitDevice(owner, adminC, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED))
	net.MustLand(net.Sign(owner, bridge.SignedType_SIGNED_ADMIN_SET, net.AdminSet([]sidecar.Id{owner.MemberId}, []sidecar.Id{memberB, memberC}, 0)))
	keyB := net.RecoveryKey(memberB, "b")
	bindId, _ := net.Propose(adminB, bridge.SignedType_SIGNED_RECOVERY_BIND, net.RecoveryBind(keyB))
	net.MustLand(net.Sign(adminC, bridge.SignedType_SIGNED_AUTHORITY_COSIGN, net.Cosign(bindId)))
	// proposals no window holds schedule nothing
	if scheduled := net.takeScheduled(); len(scheduled) != 0 {
		t.Fatalf("scheduled %v", scheduled)
	}

	// b's key proposes b's phone: the window runs from the proposal, and k waits on c
	bPhone := net.Device("b phone", memberB)
	recoveryId := net.ProposeByRecovery(keyB, bPhone)
	due := net.mustSchedule(recoveryId)
	net.waitUntil(due)
	short := net.run(recoveryId)
	if short.N != nil || short.RunAt == nil || !short.RunAt.Equal(due.Add(net.settings.ActivationRecheckInterval)) || net.Admitted(bPhone) {
		t.Fatalf("a run short of k: %+v", short)
	}
	if cosigned := net.MustLand(net.Sign(adminC, bridge.SignedType_SIGNED_AUTHORITY_COSIGN, net.Cosign(recoveryId))); cosigned.Completed() {
		t.Fatal("the k-th signature completed a recovery proposal")
	}
	// the cosign schedules it again, at the window's end, which has passed: the task runs at once
	if runAt := net.mustSchedule(recoveryId); !runAt.Equal(due) {
		t.Fatalf("the cosign scheduled %s, the window ended %s", runAt, due)
	}
	if activated := net.run(recoveryId); activated.N == nil || !net.Admitted(bPhone) {
		t.Fatalf("the run after the k-th signature: %+v", activated)
	}

	// the owner proposes b's tablet and watch; no window holds either until b's key cosigns
	bTablet, bWatch := net.Device("b tablet", memberB), net.Device("b watch", memberB)
	adminId, _ := net.Propose(owner, bridge.SignedType_SIGNED_SIDECAR_ADMIT, net.Admission(bTablet, nil, false, 0, 0))
	vetoedId, _ := net.Propose(owner, bridge.SignedType_SIGNED_SIDECAR_ADMIT, net.Admission(bWatch, nil, false, 0, 0))
	if scheduled := net.takeScheduled(); len(scheduled) != 0 {
		t.Fatalf("scheduled %v", scheduled)
	}
	net.Wait(24 * time.Hour)
	late := net.MustLand(net.SignByRecovery(keyB, bridge.SignedType_SIGNED_AUTHORITY_COSIGN, net.Cosign(adminId)))
	window := net.Window(adminId)
	if window.Row.GetWindowEntry() != late.Pin.GetVersion() {
		t.Fatalf("the window: %+v", window.Row)
	}
	moved := time.UnixMilli(window.Row.GetWindowTimeMs()).Add(72 * time.Hour)
	if runAt := net.mustSchedule(adminId); !runAt.Equal(moved) {
		t.Fatalf("the recovery cosign scheduled %s, the window ends %s", runAt, moved)
	}
	net.MustLand(net.SignByRecovery(keyB, bridge.SignedType_SIGNED_AUTHORITY_COSIGN, net.Cosign(vetoedId)))
	vetoedDue := net.mustSchedule(vetoedId)
	// 72 h after the proposal itself is 48 h into the window: a task run then waits for the window's end
	net.Wait(48 * time.Hour)
	if early := net.run(adminId); early.N != nil || early.RunAt == nil || !early.RunAt.Equal(moved) || net.Admitted(bTablet) {
		t.Fatalf("a run 48 h into the window: %+v", early)
	}
	net.MustLand(net.Sign(adminC, bridge.SignedType_SIGNED_RECOVERY_VETO, net.RecoveryVeto(vetoedId)))
	net.waitUntil(vetoedDue)
	activated := net.run(adminId)
	if activated.N == nil || !net.Admitted(bTablet) {
		t.Fatalf("the run at the moved window's end: %+v", activated)
	}
	// the tick named the one proposal: the vetoed one stays closed, and its own task does nothing
	if result := net.run(vetoedId); result.N != nil || result.RunAt != nil || net.Admitted(bWatch) {
		t.Fatalf("the vetoed proposal's run: %+v", result)
	}
	net.CheckAgreement()
}

// §7.9's tick row, at most one tick a minute per network: two proposals whose windows ended share the first
// task's tick; a third, due inside the same minute, is put off to the minute's end and activates there; and a
// tick appended through AppendTickSpaced under a minute after the newest is refused with the time it may go.
func TestActivationTicksKeepAMinuteApart(t *testing.T) {
	net := newTestConfigLogActivationNet(t)
	networkId := server.Id(net.NetworkId())
	owner := net.Owner()
	key := net.ownerKey()
	phone, tablet, watch := net.Device("owner phone", owner.MemberId), net.Device("owner tablet", owner.MemberId), net.Device("owner watch", owner.MemberId)
	phoneId := net.ProposeByRecovery(key, phone)
	net.mustSchedule(phoneId)
	tabletId := net.ProposeByRecovery(key, tablet)
	tabletDue := net.mustSchedule(tabletId)
	net.Wait(20 * time.Second)
	watchId := net.ProposeByRecovery(key, watch)
	watchDue := net.mustSchedule(watchId)

	net.waitUntil(tabletDue)
	first := net.run(phoneId)
	if first.N == nil || !net.Admitted(phone) || !net.Admitted(tablet) || net.Admitted(watch) {
		t.Fatalf("the first run: %+v", first)
	}
	tickTime := net.Clock().Now()
	if result := net.run(tabletId); result.N != nil || result.RunAt != nil {
		t.Fatalf("the second proposal's own run: %+v", result)
	}

	net.waitUntil(watchDue)
	spacedUntil := tickTime.Add(net.settings.TickInterval)
	put := net.run(watchId)
	if put.N != nil || put.RunAt == nil || !put.RunAt.Equal(spacedUntil) || net.Admitted(watch) {
		t.Fatalf("a run inside the minute: %+v, the minute ends %s", put, spacedUntil)
	}
	// the append itself holds the minute too, whoever asks
	tick := &bridge.ApiTick{ActivatedProposalIds: [][]byte{watchId}}
	_, err := net.sequencer.AppendTickSpaced(net.Context(), networkId, tick, net.settings.TickInterval)
	spacing := &ConfigLogTickSpacingError{}
	configLogError := &ConfigLogError{}
	if !errors.As(err, &spacing) || !spacing.After.Equal(spacedUntil) || !errors.As(err, &configLogError) || configLogError.Status != http.StatusForbidden {
		t.Fatalf("a tick inside the minute: %v", err)
	}
	net.Sync()
	if net.Admitted(watch) {
		t.Fatal("a tick inside the minute landed")
	}
	net.waitUntil(spacedUntil)
	if result := net.run(watchId); result.N == nil || !net.Admitted(watch) {
		t.Fatalf("the run at the minute's end: %+v", result)
	}
	net.CheckAgreement()
}

// A task reads the tip the store holds, whichever api instance wrote it: a second instance, loaded before a
// proposal landed through the first, activates that proposal from the store's entries; and the first, which
// never saw the second's tick, finds it when its own task reads, and keeps its next tick a minute from it.
func TestAnActivationTaskReadsTheStoresTip(t *testing.T) {
	net := newTestConfigLogActivationNet(t)
	networkId := server.Id(net.NetworkId())
	owner := net.Owner()
	key := net.ownerKey()
	phone, tablet := net.Device("owner phone", owner.MemberId), net.Device("owner tablet", owner.MemberId)
	phoneId := net.ProposeByRecovery(key, phone)
	net.mustSchedule(phoneId)

	settings := DefaultConfigLogSettings()
	settings.Clock = net.Clock()
	other := NewConfigLogSequencer(net.cluster, settings, nil)
	// the other instance loads now, and the first lands an entry after it
	if admitted, err := other.Admitted(net.Context(), networkId, owner.SidecarId); err != nil || !admitted {
		t.Fatalf("the other instance's load: %v", err)
	}
	tabletId := net.ProposeByRecovery(key, tablet)
	due := net.mustSchedule(tabletId)

	net.waitUntil(due)
	// one activation a tick here, so the phone's is left for the first instance
	single := DefaultConfigLogTickSettings()
	single.TickActivationMaxCount = 1
	result, err := appendConfigLogActivationAt(net.Context(), other, networkId, tabletId, single)
	if err != nil || result.N == nil {
		t.Fatalf("the other instance's run: %+v, %v", result, err)
	}
	tickTime := net.Clock().Now()
	net.Sync()
	if net.Admitted(phone) || !net.Admitted(tablet) {
		t.Fatal("the other instance's tick did not activate the tablet alone")
	}

	net.Wait(time.Second)
	spacedUntil := tickTime.Add(net.settings.TickInterval)
	if put := net.run(phoneId); put.N != nil || put.RunAt == nil || !put.RunAt.Equal(spacedUntil) || net.Admitted(phone) {
		t.Fatalf("the first instance's run inside the other's minute: %+v, the minute ends %s", put, spacedUntil)
	}
	net.waitUntil(spacedUntil)
	if activated := net.run(phoneId); activated.N == nil || !net.Admitted(phone) {
		t.Fatalf("the first instance's run at the minute's end: %+v", activated)
	}
	net.CheckAgreement()
}

// An activation's statement takes effect at its tick (§7.9's Recovery), so the tick carries §7.10's effects as
// the statement's own entry would: a SidecarRevoke the owner's recovery key proposed revokes its device at the
// activating tick, which the effects read names with that device, removed, while the proposal's own entry and
// a tick 71 h on that api refuses carry none.
func TestAnActivationTicksEffectsAreRead(t *testing.T) {
	net := newTestConfigLogActivationNet(t)
	networkId := server.Id(net.NetworkId())
	owner := net.Owner()
	key := net.ownerKey()
	laptop := net.Device("owner second laptop", owner.MemberId)
	net.AdmitDevice(owner, laptop, uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED))
	signed := net.SignByRecovery(key, bridge.SignedType_SIGNED_AUTHORITY_PROPOSAL,
		net.Proposal(bridge.SignedType_SIGNED_SIDECAR_REVOKE, net.Revocation(laptop, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_LOST, 0)))
	if verdict := net.MustLand(signed); verdict.Completed() {
		t.Fatal("a recovery key's revocation completed at once")
	}
	proposalId := sidecartest.AdmissionStatementId(signed)
	due := net.mustSchedule(proposalId)

	read, err := net.sequencer.readConfigLogEffects(net.Context(), networkId, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, effect := range read.effects {
		if 0 < len(effect.revokedSidecarIds) {
			t.Fatalf("entry %d revoked %v before the activation", effect.n, effect.revokedSidecarIds)
		}
	}
	net.sequencer.configLogEffectsApplied(networkId, read.through)

	net.waitUntil(due)
	result := net.run(proposalId)
	if result.N == nil || net.Admitted(laptop) || net.Row(laptop).GetRevokedAt() != *result.N {
		t.Fatalf("the activation: %+v, the laptop %v", result, net.Row(laptop))
	}
	read, err = net.sequencer.readConfigLogEffects(net.Context(), networkId, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.effects) != 1 || read.through != *result.N {
		t.Fatalf("%d effects through %d", len(read.effects), read.through)
	}
	effect := read.effects[0]
	if effect.n != *result.N || effect.action != AdminEventActionConfigPrefix+"tick" || effect.reason != sidecar.RevocationReasonRemoved ||
		len(effect.revokedSidecarIds) != 1 || effect.revokedSidecarIds[0] != server.Id(laptop.SidecarId) {
		t.Fatalf("the tick's effect: %+v", effect)
	}
}

// The activation task's schedule on Postgres (QD-745): one run-once task a proposal, keyed by its network and
// proposal_id, carrying both; a second schedule of a pending one keeps the earlier time and makes no second
// task; a run's result with a time schedules the next look and one without schedules nothing; and a process
// with no sequencer runs nothing and retries.
func TestAnActivationTaskIsScheduledOncePerProposal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		proposalId, otherId := bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32)
		due := server.NowUtc().Add(72 * time.Hour).Truncate(time.Millisecond)
		functionName := task.NewTaskTarget(AppendConfigLogActivationTask).TargetFunctionName()
		type pending struct {
			runAt time.Time
			args  AppendConfigLogActivationArgs
		}
		read := func() map[string]pending {
			tasks := map[string]pending{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT run_once_key, run_at, args_json FROM pending_task WHERE function_name = $1`, functionName)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var key string
						var argsJson string
						var one pending
						server.Raise(result.Scan(&key, &one.runAt, &argsJson))
						server.Raise(json.Unmarshal([]byte(argsJson), &one.args))
						tasks[key] = one
					}
				})
			})
			return tasks
		}
		key := func(proposalId []byte) string {
			return task.RunOnce("config_log_activation", networkId, hex.EncodeToString(proposalId)).String()
		}

		ScheduleConfigLogActivation(ctx, networkId, proposalId, due)
		ScheduleConfigLogActivation(ctx, networkId, proposalId, due.Add(24*time.Hour))
		tasks := read()
		one, found := tasks[key(proposalId)]
		if len(tasks) != 1 || !found || !one.runAt.Equal(due) || one.args.NetworkId != networkId || !bytes.Equal(one.args.ProposalId, proposalId) {
			t.Fatalf("after two schedules of one proposal: %+v", tasks)
		}
		ScheduleConfigLogActivation(ctx, networkId, otherId, due)
		if tasks = read(); len(tasks) != 2 || !bytes.Equal(tasks[key(otherId)].args.ProposalId, otherId) {
			t.Fatalf("after another proposal's schedule: %+v", tasks)
		}

		// the run's result decides the next look
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()
		thirdId := bytes.Repeat([]byte{5}, 32)
		args := &AppendConfigLogActivationArgs{NetworkId: networkId, ProposalId: thirdId}
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AppendConfigLogActivationTaskPost(args, &AppendConfigLogActivationResult{}, owner, tx))
		})
		if tasks = read(); len(tasks) != 2 {
			t.Fatalf("a finished run scheduled another: %+v", tasks)
		}
		again := due.Add(time.Hour)
		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(AppendConfigLogActivationTaskPost(args, &AppendConfigLogActivationResult{RunAt: &again}, owner, tx))
		})
		if tasks = read(); len(tasks) != 3 || !tasks[key(thirdId)].runAt.Equal(again) {
			t.Fatalf("a run that asked for another look: %+v", tasks)
		}

		// with no sequencer in this process the task appends nothing and retries
		SetConfigLogSequencer(nil)
		if result, err := AppendConfigLogActivationTask(args, owner); err == nil || result != nil {
			t.Fatalf("a run with no sequencer: %+v, %v", result, err)
		}
	})
}

// §20.4's config_admissions holds for what an activation admits (the sequencer's ActivationCap): with room for
// one more admission, two due proposals' tick names only the first; the second's task finds no room and looks
// again an hour on, never appending; and once a removal frees the room it activates. A tick past the cap is
// refused with the QuotaRefusal and appends nothing.
func TestAnActivationKeepsToConfigAdmissions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		tb.Cleanup(Testing_SetEnforceSidecarLimits(true))
		tb.Cleanup(Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{ConfigAdmissionsLimitKey: 2}))
		net := newTestConfigLogActivationNetWithCap(t, CheckConfigActivationCap)
		networkId := server.Id(net.NetworkId())
		owner := net.Owner()
		key := net.ownerKey()
		phone, tablet := net.Device("owner phone", owner.MemberId), net.Device("owner tablet", owner.MemberId)
		phoneId := net.ProposeByRecovery(key, phone)
		net.mustSchedule(phoneId)
		tabletId := net.ProposeByRecovery(key, tablet)
		due := net.mustSchedule(tabletId)
		both := &bridge.ApiTick{ActivatedProposalIds: [][]byte{phoneId, tabletId}}
		if count := ConfigTickAdmissionCount(net.State(), both); count != 2 {
			t.Fatalf("the tick admits %d", count)
		}

		net.waitUntil(due)
		tip := net.Tip().GetVersion()
		_, err := net.sequencer.AppendTick(net.Context(), networkId, both)
		quotaRefusal := &QuotaRefusalError{}
		if !errors.As(err, &quotaRefusal) || quotaRefusal.Refusal.LimitKey != ConfigAdmissionsLimitKey || quotaRefusal.Refusal.Limit != 2 {
			t.Fatalf("a tick past the cap: %v", err)
		}
		net.Sync()
		if net.Tip().GetVersion() != tip {
			t.Fatal("a tick past the cap landed")
		}

		first := net.run(phoneId)
		if first.N == nil || !net.Admitted(phone) || net.Admitted(tablet) {
			t.Fatalf("the first run: %+v", first)
		}
		net.Wait(net.settings.TickInterval)
		full := net.run(tabletId)
		if full.N != nil || full.RunAt == nil || !full.RunAt.Equal(net.Clock().Now().Add(net.settings.ActivationRecheckInterval)) || net.Admitted(tablet) {
			t.Fatalf("a run with no room: %+v", full)
		}
		net.MustLand(net.Sign(owner, bridge.SignedType_SIGNED_SIDECAR_REVOKE, net.Revocation(phone, bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED, 0)))
		net.waitUntil(*full.RunAt)
		if activated := net.run(tabletId); activated.N == nil || !net.Admitted(tablet) {
			t.Fatalf("the run after a removal freed the room: %+v", activated)
		}
		net.CheckAgreement()
	})
}
