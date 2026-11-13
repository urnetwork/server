// The drain outlives the database it drains (connect/EXTENDER.md C6).
//
// On 2026-11-06 at 07:49:44Z the drain published for the last time: the claim
// raised during that morning's Main brownout, the raise ended the drain's
// goroutine, and nothing started it again, so 10,919 records and revocations
// waited five days on four replicas that never restarted. A replica that
// started while the database was unready never started its drain at all
// (factury/research/extender-health-2026-11-11.md §1.4).
//
// Nothing here touches the database or the clock. The queue is held in memory
// and raises on the passes a test names, the way the queue table raises while
// the database is away, and the wait records the period it was asked for and
// returns at once. The drains run on the test goroutine, so every outcome is
// exact.

package gossip

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// What a claim or a stamp raises while the database is away.
var errTestDatabaseAway = errors.New("the test database is away")

// What readiness answers while the database is unready.
var errTestNotReady = errors.New("the test database is not ready")

// The queue table, held in memory. A claim takes the oldest unstamped rows;
// the claims and the stamps a test names raise instead. Used from the test
// goroutine only.
type testRaisingQueue struct {
	rows []*model.NetworkExtenderPublish
	// the claims, counted from 1, that raise
	raisingClaims []int
	// the stamps, counted from 1, that raise
	raisingStamps []int

	claimCount int
	stampCount int
	stamps     map[server.Id]int
}

func newTestRaisingQueue(rows ...*model.NetworkExtenderPublish) *testRaisingQueue {
	return &testRaisingQueue{
		rows:   rows,
		stamps: map[server.Id]int{},
	}
}

func (self *testRaisingQueue) claim(_ context.Context, limit int) []*model.NetworkExtenderPublish {
	self.claimCount += 1
	if slices.Contains(self.raisingClaims, self.claimCount) {
		server.Raise(errTestDatabaseAway)
	}
	claimed := []*model.NetworkExtenderPublish{}
	for _, row := range self.rows {
		if len(claimed) == limit {
			break
		}
		if 0 < self.stamps[row.PublishId] {
			continue
		}
		claimed = append(claimed, row)
	}
	return claimed
}

func (self *testRaisingQueue) stamp(_ context.Context, publishId server.Id) {
	self.stampCount += 1
	if slices.Contains(self.raisingStamps, self.stampCount) {
		server.Raise(errTestDatabaseAway)
	}
	self.stamps[publishId] += 1
}

// One queued revocation, which the drain publishes without judging it: a
// revocation names a key and no address, so it always goes (R1).
func testRevocationRow(t testing.TB, index int) (*model.NetworkExtenderPublish, string) {
	t.Helper()
	publicKey := sha256.Sum256([]byte(fmt.Sprintf("gossip-drain-fixture:%d", index)))
	body, err := proto.Marshal(&protocol.ExtenderRevocationBody{
		PublicKey:   publicKey[:],
		IssueTimeMs: uint64(1 + index),
		NetworkHost: testNetworkHost,
	})
	if err != nil {
		t.Fatal(err)
	}
	message, err := proto.Marshal(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Revocation{
			Revocation: &protocol.ExtenderRevocation{Body: body},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := &model.NetworkExtenderPublish{
		PublishId:  server.NewId(),
		ExtenderId: server.NewId(),
		Kind:       model.NetworkExtenderPublishKindRevocation,
		Message:    message,
		CreateTime: time.Date(2026, 11, 6, 7, 44, index, 0, time.UTC),
	}
	return row, "revocation:" + hex.EncodeToString(publicKey[:])
}

// A wait that never sleeps. It records every period the drain asked for and
// ends the drain at the period numbered stopAt, the way a shutdown would.
type testDrainWait struct {
	cancel context.CancelFunc
	stopAt int
	waits  []time.Duration
}

func (self *testDrainWait) wait(_ context.Context, timeout time.Duration) bool {
	self.waits = append(self.waits, timeout)
	if 0 < self.stopAt && self.stopAt <= len(self.waits) {
		self.cancel()
		return false
	}
	return true
}

// Runs the drain loop on the test goroutine until the wait ends it. A raise
// that escapes the loop is returned rather than crashing the test binary: it
// is the defect these tests exist for.
func runTestDrain(
	ctx context.Context,
	queue publishQueue,
	publisher messagePublisher,
	wait *testDrainWait,
) (escaped any) {
	defer func() {
		escaped = recover()
	}()
	runPublish(ctx, queue, publisher, publishTimeout, publishBatchSize, wait.wait)
	return nil
}

// The defect of 11-06: a claim that raises while the database is away ends
// that pass and no more. The next pass, a backoff later, claims what the
// raise left and publishes it, and the drain then goes back to its period.
func TestGossipDrainSurvivesAClaimThatRaises(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first, firstKeyHex := testRevocationRow(t, 0)
	second, secondKeyHex := testRevocationRow(t, 1)
	queue := newTestRaisingQueue(first, second)
	queue.raisingClaims = []int{1}
	publisher := &testPublisher{}
	wait := &testDrainWait{cancel: cancel, stopAt: 3}

	if escaped := runTestDrain(ctx, queue, publisher, wait); escaped != nil {
		t.Fatalf("the drain ended on the claim's raise and nothing restarts it: %v", escaped)
	}

	if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, []string{firstKeyHex, secondKeyHex}) {
		t.Fatalf("the drain published %v, want both rows the raise left", keyHexes)
	}
	for _, row := range []*model.NetworkExtenderPublish{first, second} {
		if queue.stamps[row.PublishId] != 1 {
			t.Fatalf("row %s stamped %d times, want once", row.PublishId, queue.stamps[row.PublishId])
		}
	}
	if queue.claimCount != 3 {
		t.Fatalf("the drain claimed %d times, want 3", queue.claimCount)
	}
	// a backoff after the raise, then the period after each pass that
	// completed
	if want := []time.Duration{10 * time.Second, 5 * time.Second, 5 * time.Second}; !slices.Equal(wait.waits, want) {
		t.Fatalf("the drain waited %v, want %v", wait.waits, want)
	}
}

// A database that stays away for minutes costs a pass per backoff, not one
// every period: the wait doubles from the period with every pass in a row that
// raised, up to a minute, and drops back to the period after the first pass
// that completes.
func TestGossipDrainBacksOffWhileTheDatabaseIsAway(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	row, keyHex := testRevocationRow(t, 0)
	queue := newTestRaisingQueue(row)
	queue.raisingClaims = []int{1, 2, 3, 4, 5, 6}
	publisher := &testPublisher{}
	wait := &testDrainWait{cancel: cancel, stopAt: 8}

	if escaped := runTestDrain(ctx, queue, publisher, wait); escaped != nil {
		t.Fatalf("the drain ended on the claim's raise and nothing restarts it: %v", escaped)
	}

	want := []time.Duration{
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		60 * time.Second,
		60 * time.Second,
		60 * time.Second,
		// the seventh claim completes and publishes the row
		5 * time.Second,
		5 * time.Second,
	}
	if !slices.Equal(wait.waits, want) {
		t.Fatalf("the drain waited %v, want %v", wait.waits, want)
	}
	if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, []string{keyHex}) {
		t.Fatalf("the drain published %v, want the one row once the database was back", keyHexes)
	}
	if queue.stamps[row.PublishId] != 1 {
		t.Fatalf("the row was stamped %d times, want once", queue.stamps[row.PublishId])
	}
}

// A stamp that raises after its publish is the crash between the publish and
// the stamp that the queue is ordered for (publish.go): the row is left
// unstamped, the next pass publishes it again, which gossip absorbs, and the
// row is never lost.
func TestGossipDrainSurvivesAStampThatRaises(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	row, keyHex := testRevocationRow(t, 0)
	queue := newTestRaisingQueue(row)
	queue.raisingStamps = []int{1}
	publisher := &testPublisher{}
	wait := &testDrainWait{cancel: cancel, stopAt: 3}

	if escaped := runTestDrain(ctx, queue, publisher, wait); escaped != nil {
		t.Fatalf("the drain ended on the stamp's raise and nothing restarts it: %v", escaped)
	}

	if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, []string{keyHex, keyHex}) {
		t.Fatalf("the drain published %v, want the row again after its stamp raised", keyHexes)
	}
	if queue.stamps[row.PublishId] != 1 {
		t.Fatalf("the row was stamped %d times, want once", queue.stamps[row.PublishId])
	}
	if want := []time.Duration{10 * time.Second, 5 * time.Second, 5 * time.Second}; !slices.Equal(wait.waits, want) {
		t.Fatalf("the drain waited %v, want %v", wait.waits, want)
	}
}

// A replica that starts while its database is unready drains once readiness
// passes, rather than never: readiness is checked again a backoff after each
// failure, and only then are the stats pusher and the drain started.
func TestGossipDrainStartsOnceReadinessPasses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readinessCount := 0
	readiness := func(context.Context) error {
		readinessCount += 1
		if readinessCount <= 2 {
			return errTestNotReady
		}
		return nil
	}
	events := []string{}
	startStatsPusher := func(context.Context) func() {
		events = append(events, "pusher")
		return func() {
			events = append(events, "flush")
		}
	}
	drain := func(context.Context) {
		events = append(events, "drain")
		// the shutdown, once the drain has started
		cancel()
	}
	wait := &testDrainWait{cancel: cancel}

	runPublishWhenReady(ctx, readiness, startStatsPusher, drain, wait.wait)

	if !slices.Contains(events, "drain") {
		t.Fatalf("readiness failed %d of %d checks and the drain never started", min(readinessCount, 2), readinessCount)
	}
	if readinessCount != 3 {
		t.Fatalf("readiness was checked %d times, want 3", readinessCount)
	}
	// the pusher starts with the drain, and is flushed once it ended
	if want := []string{"pusher", "drain", "flush"}; !slices.Equal(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	if want := []time.Duration{10 * time.Second, 20 * time.Second}; !slices.Equal(wait.waits, want) {
		t.Fatalf("readiness waited %v, want %v", wait.waits, want)
	}
}

// A replica that is shut down before its database is ever ready ends without
// starting the pusher or the drain.
func TestGossipDrainNeverStartsAfterAShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readinessCount := 0
	readiness := func(context.Context) error {
		readinessCount += 1
		return errTestNotReady
	}
	started := false
	startStatsPusher := func(context.Context) func() {
		started = true
		return func() {}
	}
	drained := false
	drain := func(context.Context) {
		drained = true
	}
	// the shutdown lands during the first backoff
	wait := &testDrainWait{cancel: cancel, stopAt: 1}

	runPublishWhenReady(ctx, readiness, startStatsPusher, drain, wait.wait)

	if started || drained {
		t.Fatalf("a replica shut down while unready started the pusher (%t) or the drain (%t)", started, drained)
	}
	if 1 < readinessCount {
		t.Fatalf("readiness was checked %d times after the shutdown", readinessCount)
	}
}
