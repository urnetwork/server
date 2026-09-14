// The drain of the operator's outbound queue (connect/EXTENDER.md C6, C1).
//
// Rows reach `network_extender_publish` from three writers -- an activation
// (C2), a probe that loses an extender's last address (C3) and the drip (C4) --
// each of which signs inside the transaction that wrote the state the message
// describes. Nothing here signs or judges: it takes the oldest rows the claim
// hands it, puts each on the topic and stamps it.
//
// The claim locks with SKIP LOCKED and releases at the end of its own
// transaction, so publishing never holds a database lock and a second replica
// takes the rows this one did not. The stamp is a separate write afterwards,
// which is the deliberate order: a crash between the publish and the stamp
// republishes a message, which gossip absorbs, while a stamp before the
// publish would lose one.

package gossip

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026/model"
)

const (
	// The drain period and the rows one drain claims (C6).
	publishTimeout   = 5 * time.Second
	publishBatchSize = 64
)

// What one drain publishes to, which is the node in production and a counter
// in tests. The node is the only implementation that matters: the mesh is not
// faked anywhere.
type messagePublisher interface {
	Publish(ctx context.Context, message *protocol.ExtenderGossipMessage) error
}

// What one drain did, for the log and for the tests.
type publishOutcome struct {
	Claimed int
	// put on the topic and stamped
	Published int
	// stamped without being published, because the row does not decode
	Dropped int
	// left for the next drain
	Failed int
}

// runPublish drains for the life of ctx, one batch every timeout.
func runPublish(ctx context.Context, publisher messagePublisher, timeout time.Duration, limit int) {
	for {
		drainPublishes(ctx, publisher, limit)
		select {
		case <-ctx.Done():
			return
		case <-time.After(timeout):
		}
	}
}

// drainPublishes claims one batch, oldest first, and publishes it.
//
// A row that does not decode is stamped rather than retried. It can never be
// published, and leaving it would have every later drain claim it again and
// spend part of the batch on it forever -- with enough of them, the queue
// wedges behind rows nothing can ever deliver. It is an error log because a
// row that does not decode means a writer stored something a reader cannot
// take back, which is a fault somewhere upstream and not an expected state.
//
// A row that fails to publish is left unstamped, since a topic that is not
// ready yet is exactly the case the next drain should retry.
func drainPublishes(ctx context.Context, publisher messagePublisher, limit int) *publishOutcome {
	outcome := &publishOutcome{}
	publishes := model.ClaimUnpublishedExtenderPublishes(ctx, limit)
	outcome.Claimed = len(publishes)

	for _, publish := range publishes {
		if ctx.Err() != nil {
			break
		}
		message := &protocol.ExtenderGossipMessage{}
		if err := proto.Unmarshal(publish.Message, message); err != nil {
			glog.Errorf(
				"[gossip]publish %s (extender %s, kind %d) does not decode and is dropped: %s\n",
				publish.PublishId,
				publish.ExtenderId,
				publish.Kind,
				err,
			)
			model.MarkExtenderPublishPublished(ctx, publish.PublishId)
			outcome.Dropped += 1
			continue
		}
		if err := publisher.Publish(ctx, message); err != nil {
			glog.Errorf("[gossip]publish %s failed and is kept: %s\n", publish.PublishId, err)
			outcome.Failed += 1
			continue
		}
		model.MarkExtenderPublishPublished(ctx, publish.PublishId)
		outcome.Published += 1
	}

	if 0 < outcome.Claimed && glog.V(1) {
		glog.Infof(
			"[gossip]drained %d claimed, %d published, %d dropped, %d kept\n",
			outcome.Claimed,
			outcome.Published,
			outcome.Dropped,
			outcome.Failed,
		)
	}
	return outcome
}
