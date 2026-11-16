// The drain of the operator's outbound queue (connect/EXTENDER.md C6, C1).
//
// Rows reach `network_extender_publish` from three writers -- an activation
// (C2), a probe that loses an extender's last address (C3) and the drip (C4) --
// each of which signs inside the transaction that wrote the state the message
// describes. Nothing here signs: it takes the oldest rows the claim hands it,
// puts each on the topic and stamps it. It judges one thing only, as the last
// guard of the mesh: a record that is not an ordinary open one -- the gated
// tier or a canary channel -- is never published, whichever writer queued it
// (connect/EXTENDER.md R1, R4; connect/EXTENDER-UPDATE2.md §1.3). Revocations
// name a key and no address, so they always go.
//
// The claim locks with SKIP LOCKED and releases at the end of its own
// transaction, so publishing never holds a database lock and a second replica
// takes the rows this one did not. The stamp is a separate write afterwards,
// which is the deliberate order: a crash between the publish and the stamp
// republishes a message, which gossip absorbs, while a stamp before the
// publish would lose one.
//
// The drain outlives the database. A claim or a stamp that raises while the
// database is away ends its own pass and no more: the next pass runs a backoff
// later and takes what the raise left. On 2026-11-06 a claim raised during the
// 07:44Z Main brownout, the raise ended the whole drain, and nothing was
// published for five days (factury/research/extender-health-2026-11-11.md
// §1.4).

package gossip

import (
	"context"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	// The drain period and the rows one drain claims (C6).
	publishTimeout   = 5 * time.Second
	publishBatchSize = 64

	// The longest wait after passes that raised in a row, and between the
	// readiness checks of a replica that is not ready yet. Short enough that
	// a database that comes back is drained within a minute, long enough that
	// one that stays away costs a failed pass a minute rather than one every
	// period.
	publishMaxBackoff = 60 * time.Second
)

// The wait before the next pass: the period after a pass that completed, and
// after `failures` passes in a row that raised, the period doubled once per
// failure, up to publishMaxBackoff.
func publishBackoff(timeout time.Duration, failures int) time.Duration {
	ceiling := max(timeout, publishMaxBackoff)
	backoff := timeout
	for range failures {
		backoff *= 2
		if ceiling <= backoff {
			return ceiling
		}
	}
	return backoff
}

// What one drain publishes to, which is the node in production and a counter
// in tests. The node is the only implementation that matters: the mesh is not
// faked anywhere.
type messagePublisher interface {
	Publish(ctx context.Context, message *protocol.ExtenderGossipMessage) error
}

// The two writes a drain makes to the queue: the claim of its head and the
// stamp of one row. Production is the queue table; a test substitutes a queue
// that raises, which is how a database that went away reaches the drain.
type publishQueue interface {
	claim(ctx context.Context, limit int) []*model.NetworkExtenderPublish
	stamp(ctx context.Context, publishId server.Id)
}

// The queue table, `network_extender_publish`.
type modelPublishQueue struct{}

func (modelPublishQueue) claim(ctx context.Context, limit int) []*model.NetworkExtenderPublish {
	return model.ClaimUnpublishedExtenderPublishes(ctx, limit)
}

func (modelPublishQueue) stamp(ctx context.Context, publishId server.Id) {
	model.MarkExtenderPublishPublished(ctx, publishId)
}

// Waits out one period of the drain. False when ctx ended first, which ends
// the loop that waits.
type publishWait func(ctx context.Context, timeout time.Duration) bool

// The wait of production: the clock.
func waitPublish(ctx context.Context, timeout time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(timeout):
		return true
	}
}

// What one drain did, for the log and for the tests.
type publishOutcome struct {
	Claimed int
	// put on the topic and stamped
	Published int
	// stamped without being published, because the row does not decode
	Dropped int
	// stamped without being published, because its record is gated or
	// channel restricted
	Withheld int
	// left for the next drain
	Failed int
}

// runPublish drains for the life of ctx, one batch every timeout.
//
// A pass that raises is logged and retried after publishBackoff, and nothing
// else: the loop ends only when ctx does. A raise from a canceled ctx is the
// shutdown, which the wait then ends.
func runPublish(
	ctx context.Context,
	queue publishQueue,
	publisher messagePublisher,
	timeout time.Duration,
	limit int,
	wait publishWait,
) {
	failures := 0
	for {
		if r := drainPass(ctx, queue, publisher, limit); r == nil {
			failures = 0
		} else {
			failures += 1
			if ctx.Err() == nil {
				glog.Errorf(
					"[gossip]drain pass failed (%d in a row), the next in %s: %s\n",
					failures,
					publishBackoff(timeout, failures),
					server.ErrorJsonNoStack(r),
				)
			}
		}
		if !wait(ctx, publishBackoff(timeout, failures)) {
			return
		}
	}
}

// One pass of the drain. A raise from the claim, a stamp or the publisher ends
// this pass and is returned, never passed on to the loop: whatever the pass
// did not stamp is claimed again by the next one.
func drainPass(
	ctx context.Context,
	queue publishQueue,
	publisher messagePublisher,
	limit int,
) any {
	return server.HandleError(func() {
		drainQueue(ctx, queue, publisher, limit)
	})
}

// drainPublishes drains one batch of the queue table.
func drainPublishes(ctx context.Context, publisher messagePublisher, limit int) *publishOutcome {
	return drainQueue(ctx, modelPublishQueue{}, publisher, limit)
}

// drainQueue claims one batch, oldest first, and publishes it.
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
func drainQueue(
	ctx context.Context,
	queue publishQueue,
	publisher messagePublisher,
	limit int,
) *publishOutcome {
	outcome := &publishOutcome{}
	publishes := queue.claim(ctx, limit)
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
			queue.stamp(ctx, publish.PublishId)
			outcome.Dropped += 1
			continue
		}
		if !openChannelMessage(message) {
			// a writer queued what the open mesh must never carry. Stamped
			// rather than retried, since it can never be published; an error
			// log, since every writer already leaves these rows out
			glog.Errorf(
				"[gossip]publish %s (kind %d) carries a gated or channel restricted record and is withheld\n",
				publish.PublishId,
				publish.Kind,
			)
			queue.stamp(ctx, publish.PublishId)
			outcome.Withheld += 1
			continue
		}
		if err := publisher.Publish(ctx, message); err != nil {
			glog.Errorf("[gossip]publish %s failed and is kept: %s\n", publish.PublishId, err)
			outcome.Failed += 1
			continue
		}
		queue.stamp(ctx, publish.PublishId)
		outcome.Published += 1
	}

	if 0 < outcome.Claimed && glog.V(1) {
		glog.Infof(
			"[gossip]drained %d claimed, %d published, %d dropped, %d withheld, %d kept\n",
			outcome.Claimed,
			outcome.Published,
			outcome.Dropped,
			outcome.Withheld,
			outcome.Failed,
		)
	}
	return outcome
}

// Whether a message may go on the open mesh: a revocation, or a record whose
// signed body is an ordinary open one (connect.ExtenderRecordOpen). A record
// body that does not decode cannot be shown to be open, so it does not go.
func openChannelMessage(message *protocol.ExtenderGossipMessage) bool {
	record := message.GetRecord()
	if record == nil {
		return true
	}
	body := &protocol.ExtenderRecordBody{}
	if err := proto.Unmarshal(record.GetBody(), body); err != nil {
		return false
	}
	return connect.ExtenderRecordOpen(body)
}
