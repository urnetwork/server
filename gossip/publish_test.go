// The drain of the operator's outbound queue (connect/EXTENDER.md C6).
//
// What is proved here is the whole path a record takes from the row a writer
// left behind to the directory of an app that is nowhere near the database:
// the claim order, the stamp, the mesh hop, and the three ways a drain can go
// wrong -- a row that cannot be decoded, a publish that fails, and a second
// replica claiming the same queue.

package gossip

import (
	"context"
	"slices"
	"testing"

	"github.com/urnetwork/connect"
	connectgossip "github.com/urnetwork/connect/gossip"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The drip is a rotation, so a drain that took rows in any other order would
// starve the oldest extender exactly as its record approached expiry (C4). The
// batch bound is what keeps one drain off a queue that has built up.
func TestGossipDrainTakesTheOldestRowsFirstAndStampsThem(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := newTestConfig(t)

		extenders := []*testExtender{}
		for index := range 3 {
			extender := newTestExtender(t, ctx, index, 4)
			extender.queueRecord(t, ctx, config)
			extenders = append(extenders, extender)
		}

		publisher := &testPublisher{}
		outcome := drainPublishes(ctx, publisher, 2)
		if outcome.Claimed != 2 || outcome.Published != 2 {
			t.Fatalf("first drain = %+v, want 2 claimed and 2 published", outcome)
		}
		expectKeyHexes := []string{
			"record:" + hexKey(extenders[0]),
			"record:" + hexKey(extenders[1]),
		}
		if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, expectKeyHexes) {
			t.Fatalf("first drain published %v, want %v", keyHexes, expectKeyHexes)
		}

		// exactly the published rows are stamped; the third is still waiting
		stamped := 0
		for _, publish := range testPublishes(ctx) {
			if publish.PublishedTime != nil {
				stamped += 1
			}
		}
		if stamped != 2 {
			t.Fatalf("stamped %d rows after the first drain, want 2", stamped)
		}

		outcome = drainPublishes(ctx, publisher, 2)
		if outcome.Claimed != 1 || outcome.Published != 1 {
			t.Fatalf("second drain = %+v, want 1 claimed and 1 published", outcome)
		}
		expectKeyHexes = append(expectKeyHexes, "record:"+hexKey(extenders[2]))
		if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, expectKeyHexes) {
			t.Fatalf("second drain published %v, want %v", keyHexes, expectKeyHexes)
		}
		for _, publish := range testPublishes(ctx) {
			if publish.PublishedTime == nil {
				t.Fatalf("row %s is still unstamped", publish.PublishId)
			}
		}

		// a drained queue costs nothing and publishes nothing
		outcome = drainPublishes(ctx, publisher, 2)
		if outcome.Claimed != 0 {
			t.Fatalf("third drain = %+v, want an empty queue", outcome)
		}
	})
}

// The end of the path: a row queued by the operator's own writes reaches the
// directory of a member that has nothing but the mesh (D1, D6). The revocation
// proves the same hop for the message that takes an extender away again.
func TestGossipDrainReachesAMemberDirectory(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := newTestConfig(t)

		operator := newTestOperator(t, config, 0)
		defer operator.close()
		member := newTestMember(t, config, operator.addrs(t))
		defer member.close()

		// a locally published message floods to the topic peers this node
		// holds, so the member must be in the topic before the drain runs
		waitForNodeStatus(t, operator.node, "the member in the topic", func(status connectgossip.NodeStatus) bool {
			return 1 <= status.MeshPeerCount
		})

		extender := newTestExtender(t, ctx, 0, 4)
		extender.queueRecord(t, ctx, config)

		publisher := &testPublisher{publisher: operator.node}
		if outcome := drainPublishes(ctx, publisher, publishBatchSize); outcome.Published != 1 {
			t.Fatalf("record drain = %+v, want 1 published", outcome)
		}
		waitForDirectory(t, member.directory, "the record", func(snapshot *connect.ExtenderDirectorySnapshot) bool {
			return snapshotState(snapshot, extender.ip) == connect.ExtenderStateActive
		})

		extender.queueRevocation(t, ctx, config)
		if outcome := drainPublishes(ctx, publisher, publishBatchSize); outcome.Published != 1 {
			t.Fatalf("revocation drain = %+v, want 1 published", outcome)
		}
		waitForDirectory(t, member.directory, "the revocation", func(snapshot *connect.ExtenderDirectorySnapshot) bool {
			return snapshotState(snapshot, extender.ip) == connect.ExtenderStateRevoked
		})
	})
}

// A row that cannot be decoded can never be published, so it is stamped and
// logged rather than retried: left unstamped it would be claimed by every
// later drain, and enough of them would wedge the head of the queue behind
// messages nothing can ever deliver.
func TestGossipDrainDropsAnUndecodableRowAndKeepsGoing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := newTestConfig(t)

		// the undecodable row is first, so a drain that stopped on it would
		// publish nothing at all
		undecodable := newTestExtender(t, ctx, 0, 4)
		queueUndecodablePublish(t, ctx, undecodable)
		extender := newTestExtender(t, ctx, 1, 6)
		extender.queueRecord(t, ctx, config)

		publisher := &testPublisher{}
		outcome := drainPublishes(ctx, publisher, publishBatchSize)
		if outcome.Claimed != 2 || outcome.Dropped != 1 || outcome.Published != 1 {
			t.Fatalf("drain = %+v, want 2 claimed, 1 dropped and 1 published", outcome)
		}
		expectKeyHexes := []string{"record:" + hexKey(extender)}
		if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, expectKeyHexes) {
			t.Fatalf("drain published %v, want %v", keyHexes, expectKeyHexes)
		}
		for _, publish := range testPublishes(ctx) {
			if publish.PublishedTime == nil {
				t.Fatalf("row %s is still unstamped, so the queue is wedged", publish.PublishId)
			}
		}

		// and nothing is left for the next drain to claim
		if outcome := drainPublishes(ctx, publisher, publishBatchSize); outcome.Claimed != 0 {
			t.Fatalf("second drain = %+v, want an empty queue", outcome)
		}
	})
}

// A publish that fails is the one case a row must survive: the topic may be
// unreachable for a moment, and a stamp before the publish would lose the
// record for good.
func TestGossipDrainKeepsARowThatFailedToPublish(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := newTestConfig(t)

		extender := newTestExtender(t, ctx, 0, 4)
		extender.queueRecord(t, ctx, config)

		failing := &testPublisher{err: errTestPublish}
		outcome := drainPublishes(ctx, failing, publishBatchSize)
		if outcome.Claimed != 1 || outcome.Failed != 1 || outcome.Published != 0 {
			t.Fatalf("failing drain = %+v, want 1 claimed and 1 failed", outcome)
		}
		for _, publish := range testPublishes(ctx) {
			if publish.PublishedTime != nil {
				t.Fatalf("row %s was stamped even though it never published", publish.PublishId)
			}
		}

		publisher := &testPublisher{}
		if outcome := drainPublishes(ctx, publisher, publishBatchSize); outcome.Published != 1 {
			t.Fatalf("retry drain = %+v, want 1 published", outcome)
		}
		expectKeyHexes := []string{"record:" + hexKey(extender)}
		if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, expectKeyHexes) {
			t.Fatalf("retry drain published %v, want %v", keyHexes, expectKeyHexes)
		}
	})
}

// Two drains on the same queue partition it and never publish one row twice
// (C6). The contention is made exact rather than raced: one drain's claim is
// pinned open on the first half of the queue while the other drain claims, so
// SKIP LOCKED is what decides the outcome and no timing does.
func TestGossipConcurrentDrainsNeverPublishARowTwice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := newTestConfig(t)

		extenders := []*testExtender{}
		for index := range 4 {
			extender := newTestExtender(t, ctx, index, 4)
			extender.queueRecord(t, ctx, config)
			extenders = append(extenders, extender)
		}
		publishes := testPublishes(ctx)
		if len(publishes) != 4 {
			t.Fatalf("queued %d rows, want 4", len(publishes))
		}
		heldPublishIds := []server.Id{
			publishes[0].PublishId,
			publishes[1].PublishId,
		}

		publisher := &testPublisher{}
		var heldOutcome *publishOutcome
		model.Testing_HoldExtenderPublishLock(ctx, heldPublishIds, func() {
			heldOutcome = drainPublishes(ctx, publisher, publishBatchSize)
		})
		if heldOutcome.Claimed != 2 || heldOutcome.Published != 2 {
			t.Fatalf("the contending drain = %+v, want the 2 rows it was not locked out of", heldOutcome)
		}
		expectKeyHexes := []string{
			"record:" + hexKey(extenders[2]),
			"record:" + hexKey(extenders[3]),
		}
		if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, expectKeyHexes) {
			t.Fatalf("the contending drain published %v, want %v", keyHexes, expectKeyHexes)
		}

		// the holder rolled its claim back, so the rows it held are still
		// waiting and the next drain takes exactly those
		outcome := drainPublishes(ctx, publisher, publishBatchSize)
		if outcome.Claimed != 2 || outcome.Published != 2 {
			t.Fatalf("the second drain = %+v, want the 2 released rows", outcome)
		}
		expectKeyHexes = append(
			expectKeyHexes,
			"record:"+hexKey(extenders[0]),
			"record:"+hexKey(extenders[1]),
		)
		keyHexes := publisher.publishedKeyHexes()
		if !slices.Equal(keyHexes, expectKeyHexes) {
			t.Fatalf("the two drains published %v, want %v", keyHexes, expectKeyHexes)
		}
		// one publish per row and no more, which is the whole point
		if len(keyHexes) != len(publishes) {
			t.Fatalf("published %d messages for %d rows", len(keyHexes), len(publishes))
		}
		for _, publish := range testPublishes(ctx) {
			if publish.PublishedTime == nil {
				t.Fatalf("row %s is still unstamped", publish.PublishId)
			}
		}
	})
}
