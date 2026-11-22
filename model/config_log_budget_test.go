package model

// api's write budget for the authorization log (§11.13's cost and wallet rows): the sequencer spends each proj/ and
// projrows/ object's create as P0 of kind log once the store shows the create took effect, so what it reports is what
// the objects it holds price at as created, the sum e2e's controlPlaneCheckApiBudget holds api's process to. Each test
// runs on T2's fake Blobs behind evsservice.BudgetedBlobs, so a PUT whose ctx no budget passed is refused, and holds
// the sequencers' budgets to the objects the store holds, with the PUTs sent counted by key:
//
//   - a base whose rows match a standing projrows/ object, as the it bridge case's block base does (a config head
//     change over base 1's rows), sends no PUT for them and spends nothing for them, at the instance that wrote them
//     and at one restarted over the same store;
//   - a retry of base 1 finds its genesis standing and spends nothing for it, and one whose genesis step failed
//     spends it once;
//   - an entry PUT whose outcome was unknown spends once, whether it landed, never landed and was sent again, or
//     stayed unknown until a retry of the append settled it; a 412 over nothing spends once, for the PUT that lands;
//   - a position another instance took spends nothing at the loser, and the base it signs again finds its rows
//     standing;
//   - an adopted run written again spends nothing, and the resumed base over base 1's rows spends its entry alone;
//   - every other append spends once per object, as it did.

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// What the tests price an object's create at: a part an object and a part a byte, so a total tells both how many
// objects were spent and which.
func testConfigLogBudgetPrice(objectByteCount int64) int64 {
	return 1000 + 3*objectByteCount
}

// A first cluster that counts the PUTs sent to each key.
type testConfigLogBudgetCluster struct {
	sidecar.BlobCluster

	stateLock    sync.Mutex
	putKeyCounts map[string]int
}

func (self *testConfigLogBudgetCluster) PutIfNoneMatch(ctx context.Context, key string, body []byte, options *sidecar.BlobPutOptions) (string, error) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.putKeyCounts[key] += 1
	}()
	return self.BlobCluster.PutIfNoneMatch(ctx, key, body, options)
}

// How many PUTs were sent to key.
func (self *testConfigLogBudgetCluster) putCount(key string) int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.putKeyCounts[key]
}

// T2's fake first cluster behind BudgetedBlobs, its PUTs counted, and the queue its faults are armed on.
func newTestConfigLogBudgetCluster(t *testing.T) (*testConfigLogBudgetCluster, *sidecartest.FaultQueue) {
	faults := sidecartest.NewFaultQueue()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), faults, nil, sidecartest.DefaultFakeBlobsSettings())
	budgeted, err := evsservice.NewBudgetedBlobs(blobs).FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return &testConfigLogBudgetCluster{BlobCluster: budgeted, putKeyCounts: map[string]int{}}, faults
}

// What the proj/ and projrows/ objects the cluster holds for the network price at as created: the P0 the budgets of
// the sequencers that wrote them must sum to.
func testConfigLogBudgetHeldPrice(t *testing.T, cluster sidecar.BlobCluster, networkId sidecar.Id) int64 {
	t.Helper()
	heldPrice := int64(0)
	for _, prefix := range []string{sidecar.ConfigEntryKeyPrefix(networkId), configLogRowsKeyPrefix(networkId)} {
		startAfter := ""
		for {
			infos, more, err := cluster.List(t.Context(), prefix, startAfter, 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, info := range infos {
				heldPrice += testConfigLogBudgetPrice(info.ByteCount)
				startAfter = info.Key
			}
			if !more || len(infos) == 0 {
				break
			}
		}
	}
	return heldPrice
}

// The P0 of kind log the budgets hold, summed, and any violation of the interface one saw.
func testConfigLogBudgetSpent(t *testing.T, clusterId string, budgets ...*sidecartest.FakeWriteBudget) int64 {
	t.Helper()
	spent := int64(0)
	for _, budget := range budgets {
		spent += budget.P0ByteCount(clusterId, sidecar.WriteP0KindLog)
		if violations := budget.Violations(); len(violations) != 0 {
			t.Errorf("the budget's violations: %v", violations)
		}
	}
	return spent
}

// One network's log for the budget's tests: its signers, the counted cluster and its faults.
type testConfigLogBudgetWorld struct {
	t       *testing.T
	cluster *testConfigLogBudgetCluster
	faults  *sidecartest.FaultQueue
	world   *sidecartest.ConfigLogSuiteWorld
}

func newTestConfigLogBudgetWorld(t *testing.T) *testConfigLogBudgetWorld {
	cluster, faults := newTestConfigLogBudgetCluster(t)
	return &testConfigLogBudgetWorld{t: t, cluster: cluster, faults: faults, world: sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)}
}

// A sequencer on the world's cluster and clock, as one api instance runs it, spending a budget of its own.
func (self *testConfigLogBudgetWorld) sequencer() (*ConfigLogSequencer, *sidecartest.FakeWriteBudget) {
	settings := DefaultConfigLogSettings()
	settings.Clock = self.world.Clock()
	sequencer := NewConfigLogSequencer(self.cluster, settings, nil)
	budget := sidecartest.NewFakeWriteBudget(nil)
	sequencer.SetWriteBudget(budget, testConfigLogBudgetPrice)
	return sequencer, budget
}

func (self *testConfigLogBudgetWorld) networkId() server.Id {
	return server.Id(self.world.NetworkId())
}

// Arms a fault on the world's store.
func (self *testConfigLogBudgetWorld) fault(plan *sidecartest.FaultPlan) {
	self.t.Helper()
	if _, err := self.faults.Add(plan); err != nil {
		self.t.Fatal(err)
	}
}

// Fails the test unless every armed fault fired.
func (self *testConfigLogBudgetWorld) checkFaultsFired() {
	self.t.Helper()
	if unfired := self.faults.Unfired(); len(unfired) != 0 {
		self.t.Errorf("faults that never fired: %v", unfired)
	}
}

// Fails the test unless the budgets spent exactly what the objects the store holds price at as created: a write
// spent and never made, or made and never spent, shows here.
func (self *testConfigLogBudgetWorld) checkSpent(step string, budgets ...*sidecartest.FakeWriteBudget) {
	self.t.Helper()
	held := testConfigLogBudgetHeldPrice(self.t, self.cluster, self.world.NetworkId())
	if spent := testConfigLogBudgetSpent(self.t, self.cluster.ClusterId(), budgets...); spent != held {
		self.t.Errorf("%s: spent P0 %d, and the objects the store holds price at %d as created", step, spent, held)
	}
}

// What the object at key prices at: the P0 its one create spends.
func (self *testConfigLogBudgetWorld) objectPrice(key string) int64 {
	self.t.Helper()
	info, err := self.cluster.Head(self.t.Context(), key)
	if err != nil {
		self.t.Fatal(err)
	}
	return testConfigLogBudgetPrice(info.ByteCount)
}

// C_version, the config repo's head after its version-th push, as a base names it.
func (self *testConfigLogBudgetWorld) configHead(version uint64) []byte {
	self.t.Helper()
	pushes := self.world.ConfigPushes(version-1, version)
	signed := &bridge.Signed{}
	push := &protocol.EvsPush{}
	if err := proto.Unmarshal(pushes[0], signed); err != nil {
		self.t.Fatal(err)
	}
	if err := proto.Unmarshal(signed.GetBody(), push); err != nil {
		self.t.Fatal(err)
	}
	pushId, err := sidecar.IdFromBytes(push.GetPushId())
	if err != nil {
		self.t.Fatal(err)
	}
	head := sidecar.EvsChainNext([sha256.Size]byte(push.GetBaseChain()), pushId, sidecar.EvsStatementSha256(pushes[0]))
	return head[:]
}

// A base the owner's first device publishes at tip over rows, folding through the tip, so it carries no tail, and
// naming the config head at configVersion, with the pushes after fromVersion that reach it: what a config writer's
// save publishes.
func (self *testConfigLogBudgetWorld) base(tip *bridge.AuthPin, rows []byte, fromVersion uint64, configVersion uint64) *ConfigLogBase {
	rowsSha256 := sha256.Sum256(rows)
	signedHeader := self.world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, &bridge.ProjectionHeader{
		NetworkId:        self.world.NetworkId().Bytes(),
		Version:          tip.GetVersion() + 1,
		PrevChain:        tip.GetChain(),
		FoldedThrough:    tip.GetVersion(),
		ConfigVersion:    configVersion,
		ConfigHeadSha256: self.configHead(configVersion),
		RowsSha256:       rowsSha256[:],
		RowsByteCount:    uint32(len(rows)),
	})
	return &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, ConfigPushes: self.world.ConfigPushes(fromVersion, configVersion)}
}

// Base 1's rows with a member row of memberId's added: the rows of a base folding in that member's admission.
func (self *testConfigLogBudgetWorld) rowsWithMember(memberId sidecar.Id) []byte {
	self.t.Helper()
	_, baseOneRows, _, _ := self.world.BaseOne()
	rows := &bridge.ProjectionRows{}
	if err := proto.Unmarshal(baseOneRows, rows); err != nil {
		self.t.Fatal(err)
	}
	rows.Members = append(rows.Members, &bridge.MemberRow{MemberId: memberId.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER})
	rowsBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(rows)
	if err != nil {
		self.t.Fatal(err)
	}
	return rowsBytes
}

// The owner's MemberAdmit of memberId at the world's tip.
func (self *testConfigLogBudgetWorld) memberAdmit(memberId sidecar.Id) []byte {
	return self.world.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		Header: self.world.Header(), MemberId: memberId.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER,
	})
}

// The pin of the entry the store holds at n.
func (self *testConfigLogBudgetWorld) entryPin(n uint64) *bridge.AuthPin {
	self.t.Helper()
	entryBytes, found, err := configLogGet(self.t.Context(), self.cluster, sidecar.ConfigEntryKey(self.world.NetworkId(), n))
	if err != nil || !found {
		self.t.Fatalf("the entry at %d: %v, %v", n, found, err)
	}
	entry := &bridge.AuthEntry{}
	if err := proto.Unmarshal(entryBytes, entry); err != nil {
		self.t.Fatal(err)
	}
	chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
	if err != nil {
		self.t.Fatal(err)
	}
	return &bridge.AuthPin{Version: n, Chain: chain[:]}
}

// The key of base 1's rows.
func (self *testConfigLogBudgetWorld) baseOneRowsKey() string {
	_, rows, _, _ := self.world.BaseOne()
	rowsSha256 := sha256.Sum256(rows)
	return sidecar.ConfigRowsKey(self.world.NetworkId(), rowsSha256[:])
}

// The status of a sequencer's refusal, 0 for none.
func testConfigLogBudgetStatus(err error) int {
	configLogError := &ConfigLogError{}
	if errors.As(err, &configLogError) {
		return configLogError.Status
	}
	return 0
}

// A config head change over base 1's rows, as the it bridge case's block base is, finds the rows standing: it sends
// no PUT for them and spends its entry alone, at the instance that wrote them and at one restarted over the same
// store, whose budget holds that one entry's price.
func TestConfigLogBaseOverStandingRowsSpendsNoP0(t *testing.T) {
	self := newTestConfigLogBudgetWorld(t)
	ctx := t.Context()
	sequencer, budget := self.sequencer()
	if err := self.world.Start(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
		t.Fatal(err)
	}
	self.checkSpent("base 1", budget)
	rowsKey := self.baseOneRowsKey()
	_, rows, _, _ := self.world.BaseOne()

	tip, err := sequencer.Tip(ctx, self.networkId())
	if err != nil {
		t.Fatal(err)
	}
	pin, err := sequencer.AppendBase(ctx, self.networkId(), self.base(tip, rows, 1, 2))
	if err != nil || pin.GetVersion() != 2 {
		t.Fatalf("a config head change over base 1's rows: %v, %v", pin, err)
	}
	self.world.SetTip(pin)
	if count := self.cluster.putCount(rowsKey); count != 1 {
		t.Errorf("base 1's rows were sent %d times, once for base 1 and again for a base over them", count)
	}
	self.checkSpent("a base over base 1's rows", budget)

	restarted, restartedBudget := self.sequencer()
	again, err := restarted.AppendBase(ctx, self.networkId(), self.base(pin, rows, 2, 3))
	if err != nil || again.GetVersion() != 3 {
		t.Fatalf("a base over base 1's rows at a restarted instance: %v, %v", again, err)
	}
	if count := self.cluster.putCount(rowsKey); count != 1 {
		t.Errorf("base 1's rows were sent %d times", count)
	}
	entryPrice := self.objectPrice(sidecar.ConfigEntryKey(self.world.NetworkId(), 3))
	if spent := restartedBudget.P0ByteCount(self.cluster.ClusterId(), sidecar.WriteP0KindLog); spent != entryPrice {
		t.Errorf("the restarted instance spent %d for a base over standing rows, and its entry prices at %d", spent, entryPrice)
	}
	self.checkSpent("a base over base 1's rows at a restarted instance", budget, restartedBudget)
}

// A retry of base 1 whose genesis step failed writes the genesis and spends it once, the failed PUT spending nothing;
// a retry of the landed base 1 finds its genesis standing and sends and spends nothing, at a restarted instance too.
func TestConfigLogBaseOneRetrySpendsItsGenesisOnce(t *testing.T) {
	self := newTestConfigLogBudgetWorld(t)
	ctx := t.Context()
	sequencer, budget := self.sequencer()
	genesisKey := sidecar.ConfigEntryKey(self.world.NetworkId(), 0)
	signedHeader, rows, signedGenesis, configPushes := self.world.BaseOne()
	baseOne := &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: signedGenesis, ConfigPushes: configPushes}

	// the genesis step is refused, nothing written
	self.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: genesisKey, Result: sidecartest.Held})
	if _, err := sequencer.AppendBase(ctx, self.networkId(), baseOne); testConfigLogBudgetStatus(err) != http.StatusServiceUnavailable || !errors.Is(err, sidecar.ErrBlobHeld) {
		t.Fatalf("base 1 whose genesis step was refused: %v", err)
	}
	self.checkFaultsFired()
	self.checkSpent("base 1 without its genesis", budget)

	pin, err := sequencer.AppendBase(ctx, self.networkId(), baseOne)
	if err != nil || pin.GetVersion() != 1 {
		t.Fatalf("the retry of base 1: %v, %v", pin, err)
	}
	if count := self.cluster.putCount(genesisKey); count != 2 {
		t.Errorf("the genesis was sent %d times, refused once and created once", count)
	}
	self.checkSpent("the retry that wrote the genesis", budget)

	again, err := sequencer.AppendBase(ctx, self.networkId(), baseOne)
	if err != nil || !proto.Equal(again, pin) {
		t.Fatalf("a retry of the landed base 1: %v, %v", again, err)
	}
	restarted, restartedBudget := self.sequencer()
	again, err = restarted.AppendBase(ctx, self.networkId(), baseOne)
	if err != nil || !proto.Equal(again, pin) {
		t.Fatalf("a retry of the landed base 1 at a restarted instance: %v, %v", again, err)
	}
	if count := self.cluster.putCount(genesisKey); count != 2 {
		t.Errorf("the genesis was sent %d times, again for retries that found it standing", count)
	}
	self.checkSpent("retries of the landed base 1", budget, restartedBudget)
}

// An entry PUT whose outcome was unknown spends once: its answer lost; dead before the store took it and sent again; a
// 412 over nothing and sent again; and with the read after it failing too, so the append answers 503 and its retry
// settles it, at the same time (the same bytes) or a later one, whether the first PUT landed or not.
func TestConfigLogEntryPutWithAnUnknownOutcomeSpendsOnce(t *testing.T) {
	type testCase struct {
		name string
		// the faults on the entry's key, the PUT's first
		results []sidecartest.FaultResult
		// whether the first append answers 503 and the statement is posted again
		retried bool
		// whether the clock moves before the retry, so its entry carries another time
		advance bool
	}
	testCases := []testCase{
		{name: "a lost answer", results: []sidecartest.FaultResult{sidecartest.ApplyThenDrop}},
		{name: "a request dead before the store took it", results: []sidecartest.FaultResult{sidecartest.FailBeforeApply}},
		{name: "a 412 over nothing", results: []sidecartest.FaultResult{sidecartest.Precondition}},
		{name: "a lost answer and a failed read, retried at once", results: []sidecartest.FaultResult{sidecartest.ApplyThenDrop, sidecartest.FailBeforeApply}, retried: true},
		{name: "a lost answer and a failed read, retried later", results: []sidecartest.FaultResult{sidecartest.ApplyThenDrop, sidecartest.FailBeforeApply}, retried: true, advance: true},
		{name: "a dead request and a failed read, retried at once", results: []sidecartest.FaultResult{sidecartest.FailBeforeApply, sidecartest.FailBeforeApply}, retried: true},
		{name: "a dead request and a failed read, retried later", results: []sidecartest.FaultResult{sidecartest.FailBeforeApply, sidecartest.FailBeforeApply}, retried: true, advance: true},
	}
	for _, testCase := range testCases {
		self := newTestConfigLogBudgetWorld(t)
		ctx := t.Context()
		sequencer, budget := self.sequencer()
		if err := self.world.Start(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
			t.Fatal(err)
		}
		entryKey := sidecar.ConfigEntryKey(self.world.NetworkId(), 2)
		for i, result := range testCase.results {
			op := sidecartest.FaultOpPut
			if 0 < i {
				op = sidecartest.FaultOpGet
			}
			self.fault(&sidecartest.FaultPlan{Op: op, KeyPrefix: entryKey, Result: result})
		}
		signed := self.world.MemberAdmit()
		pin, err := sequencer.AppendStatement(ctx, self.networkId(), signed)
		if testCase.retried {
			if testConfigLogBudgetStatus(err) != http.StatusServiceUnavailable {
				t.Fatalf("%s: the first append answered %v, %v", testCase.name, pin, err)
			}
			if testCase.advance {
				self.world.Clock().Advance(time.Second)
			}
			pin, err = sequencer.AppendStatement(ctx, self.networkId(), signed)
		}
		if err != nil || pin.GetVersion() != 2 {
			t.Fatalf("%s: the append answered %v, %v", testCase.name, pin, err)
		}
		self.checkFaultsFired()
		self.checkSpent(testCase.name, budget)
	}
}

// A position another instance took spends nothing at the loser, whose statement lands at the next; and a base signed
// at a tip that moved under the loser's PUT creates its rows once, which the base signed again at the new tip finds
// standing, so the loser spends its rows and its landed entries alone.
func TestConfigLogLostPositionSpendsNothingForIt(t *testing.T) {
	self := newTestConfigLogBudgetWorld(t)
	ctx := t.Context()
	first, firstBudget := self.sequencer()
	second, secondBudget := self.sequencer()
	if err := self.world.Start(testConfigLogSuiteSequencer{sequencer: first}); err != nil {
		t.Fatal(err)
	}
	statement := self.memberAdmit(sidecar.NewId())
	if _, err := self.world.PostMemberAdmit(testConfigLogSuiteSequencer{sequencer: second}); err != nil {
		t.Fatal(err)
	}
	// first's verifier is at base 1: it sends the statement to position 2, which second took
	pin, err := first.AppendStatement(ctx, self.networkId(), statement)
	if err != nil || pin.GetVersion() != 3 {
		t.Fatalf("a statement whose first position another instance took: %v, %v", pin, err)
	}
	self.world.SetTip(pin)
	if count := self.cluster.putCount(sidecar.ConfigEntryKey(self.world.NetworkId(), 2)); count != 2 {
		t.Errorf("position 2 took %d PUTs, second's and first's lost one", count)
	}
	self.checkSpent("a lost position", firstBudget, secondBudget)

	// second's verifier is at 2, its own entry: a base signed there loses position 3 to first's statement
	rows := self.rowsWithMember(sidecar.NewId())
	rowsSha256 := sha256.Sum256(rows)
	rowsKey := sidecar.ConfigRowsKey(self.world.NetworkId(), rowsSha256[:])
	if _, err := second.AppendBase(ctx, self.networkId(), self.base(self.entryPin(2), rows, 1, 2)); testConfigLogBudgetStatus(err) != http.StatusConflict {
		t.Fatalf("a base whose tip moved under its PUT: %v", err)
	}
	if count := self.cluster.putCount(rowsKey); count != 1 {
		t.Errorf("the base's rows were sent %d times", count)
	}
	self.checkSpent("a base whose position was lost", firstBudget, secondBudget)

	pin, err = second.AppendBase(ctx, self.networkId(), self.base(pin, rows, 1, 2))
	if err != nil || pin.GetVersion() != 4 {
		t.Fatalf("the base signed again at the new tip: %v, %v", pin, err)
	}
	if count := self.cluster.putCount(rowsKey); count != 1 {
		t.Errorf("the base's rows were sent %d times, again for the base signed again over them", count)
	}
	self.checkSpent("the base signed again", firstBudget, secondBudget)
}

// The rows of a base whose PUT's outcome stayed unknown, its read failing too, are spent once as issued, and the
// base's retry, which creates them as the first PUT never landed, spends nothing more for them.
func TestConfigLogRowsWithAnUnsettledOutcomeSpendOnce(t *testing.T) {
	self := newTestConfigLogBudgetWorld(t)
	ctx := t.Context()
	sequencer, budget := self.sequencer()
	if err := self.world.Start(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
		t.Fatal(err)
	}
	memberId := sidecar.NewId()
	pin, err := sequencer.AppendStatement(ctx, self.networkId(), self.memberAdmit(memberId))
	if err != nil {
		t.Fatal(err)
	}
	self.world.SetTip(pin)
	rows := self.rowsWithMember(memberId)
	rowsSha256 := sha256.Sum256(rows)
	rowsKey := sidecar.ConfigRowsKey(self.world.NetworkId(), rowsSha256[:])
	base := self.base(pin, rows, 1, 2)

	// the PUT dies before the store takes it, and the read that would tell fails: the base answers 503
	self.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpPut, KeyPrefix: rowsKey, Result: sidecartest.FailBeforeApply})
	self.fault(&sidecartest.FaultPlan{Op: sidecartest.FaultOpGet, KeyPrefix: rowsKey, Nth: 2, Result: sidecartest.FailBeforeApply})
	if _, err := sequencer.AppendBase(ctx, self.networkId(), base); testConfigLogBudgetStatus(err) != http.StatusServiceUnavailable {
		t.Fatalf("a base whose rows' outcome stayed unknown: %v", err)
	}
	self.checkFaultsFired()
	rowsPrice := testConfigLogBudgetPrice(int64(len(rows)))
	if spent, held := testConfigLogBudgetSpent(t, self.cluster.ClusterId(), budget), testConfigLogBudgetHeldPrice(t, self.cluster, self.world.NetworkId()); spent != held+rowsPrice {
		t.Errorf("spent %d with the rows' outcome unknown, and the objects held price at %d, the rows at %d", spent, held, rowsPrice)
	}

	landed, err := sequencer.AppendBase(ctx, self.networkId(), base)
	if err != nil || landed.GetVersion() != 3 {
		t.Fatalf("the base's retry: %v, %v", landed, err)
	}
	if count := self.cluster.putCount(rowsKey); count != 2 {
		t.Errorf("the rows were sent %d times", count)
	}
	self.checkSpent("the base's retry", budget)
}

// After a site loss the adopted run written to the empty store spends what it creates, and the same run written
// again, a retry, spends nothing; the resumed base over base 1's rows finds them standing and spends its entry alone.
func TestConfigLogAdoptedRunWrittenAgainSpendsNothing(t *testing.T) {
	self := newReseedWorld(t, true)
	owner, member := self.kept(self.ownerSidecarId), self.kept(self.memberSidecarId)
	adoption, err := AdoptConfigRun(self.tip, []*ConfigReseedUpload{owner, member})
	if err != nil {
		t.Fatal(err)
	}
	lost, _ := newTestConfigLogBudgetCluster(t)
	settings := DefaultConfigLogSettings()
	settings.Clock = self.world.Clock()
	restored := NewConfigLogSequencer(lost, settings, nil)
	budget := sidecartest.NewFakeWriteBudget(nil)
	restored.SetWriteBudget(budget, testConfigLogBudgetPrice)
	check := func(step string) {
		t.Helper()
		held := testConfigLogBudgetHeldPrice(t, lost, sidecar.Id(self.networkId))
		if spent := testConfigLogBudgetSpent(t, lost.ClusterId(), budget); spent != held {
			t.Errorf("%s: spent P0 %d, and the objects the store holds price at %d as created", step, spent, held)
		}
	}

	if err := restored.WriteAdoptedRun(self.ctx, self.networkId, adoption); err != nil {
		t.Fatal(err)
	}
	check("the adopted run")
	if err := restored.WriteAdoptedRun(self.ctx, self.networkId, adoption); err != nil {
		t.Fatal(err)
	}
	check("the adopted run written again")

	base := self.resumeBase()
	rowsSha256 := sha256.Sum256(base.Rows)
	rowsKey := sidecar.ConfigRowsKey(sidecar.Id(self.networkId), rowsSha256[:])
	rowsPutCount := lost.putCount(rowsKey)
	pin, err := restored.AppendBase(self.ctx, self.networkId, base)
	if err != nil || pin.GetVersion() != self.tip.GetVersion()+1 {
		t.Fatalf("the resumed base: %v, %v", pin, err)
	}
	if count := lost.putCount(rowsKey); count != rowsPutCount {
		t.Errorf("base 1's rows were sent %d more times for the resumed base over them", count-rowsPutCount)
	}
	check("the resumed base")
}

// Every other append spends once per object, as it did: base 1 with its genesis, its rows and its entry, statements,
// and a base with rows of its own.
func TestConfigLogAppendsSpendOncePerObject(t *testing.T) {
	self := newTestConfigLogBudgetWorld(t)
	ctx := t.Context()
	sequencer, budget := self.sequencer()
	if err := self.world.Start(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
		t.Fatal(err)
	}
	self.checkSpent("base 1", budget)
	memberId := sidecar.NewId()
	pin, err := sequencer.AppendStatement(ctx, self.networkId(), self.memberAdmit(memberId))
	if err != nil {
		t.Fatal(err)
	}
	self.world.SetTip(pin)
	self.checkSpent("a statement", budget)
	if pin, err = self.world.PostMemberAdmit(testConfigLogSuiteSequencer{sequencer: sequencer}); err != nil {
		t.Fatal(err)
	}
	self.checkSpent("another statement", budget)
	pin, err = sequencer.AppendBase(ctx, self.networkId(), self.base(pin, self.rowsWithMember(memberId), 1, 2))
	if err != nil {
		t.Fatal(err)
	}
	self.world.SetTip(pin)
	self.checkSpent("a base with rows of its own", budget)
	positions, _, err := sequencer.listPositions(ctx, self.networkId(), 0, 100)
	if err != nil || len(positions) != 4 {
		t.Fatalf("the log holds %v, %v", positions, err)
	}
}
