package gossip

// The drain as the last guard of the open mesh (connect/EXTENDER.md R1, R4;
// connect/EXTENDER-UPDATE2.md §1.3): whichever writer queued it, a gated or
// channel-restricted record is stamped and never published, while open
// records and revocations go as before.

import (
	"context"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Queues a record for this extender whose signed body a writer changed before
// signing, which is what a writer that forgot the tier rule would leave.
func (self *testExtender) queueChangedRecord(
	t testing.TB,
	ctx context.Context,
	config *testConfig,
	change func(body *protocol.ExtenderRecordBody),
) {
	t.Helper()
	published := model.PublishNetworkExtenderRecord(
		ctx,
		self.extenderId,
		func(
			extender *model.NetworkExtender,
			addresses []*model.NetworkExtenderAddress,
			issueTime time.Time,
		) ([]byte, error) {
			record, _, err := controller.SignExtenderRecord(config.extender, config.rootPrivateKey, extender, addresses, issueTime)
			if err != nil {
				return nil, err
			}
			body := &protocol.ExtenderRecordBody{}
			if err := proto.Unmarshal(record.Body, body); err != nil {
				return nil, err
			}
			change(body)
			changed, err := connect.SignExtenderRecord(config.rootPrivateKey, body)
			if err != nil {
				return nil, err
			}
			return proto.Marshal(&protocol.ExtenderGossipMessage{
				Message: &protocol.ExtenderGossipMessage_Record{Record: changed},
			})
		},
	)
	if !published {
		t.Fatalf("the changed record of extender %s was not queued", self.extenderId)
	}
}

// Root cause guarded: the drain published every row it claimed, so a gated
// record any writer queued by mistake went to the whole mesh. It is withheld
// and stamped, so it is never retried; the open record and the revocation
// beside it are published in order.
func TestGossipDrainWithholdsGatedAndChannelRestrictedRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		config := newTestConfig(t)

		gated := newTestExtender(t, ctx, 0, 4)
		gated.queueChangedRecord(t, ctx, config, func(body *protocol.ExtenderRecordBody) {
			body.DirectoryTier = connect.ExtenderDirectoryTierGated
		})
		open := newTestExtender(t, ctx, 1, 4)
		open.queueRecord(t, ctx, config)
		// a canary channel on the open wire tier still fails closed
		canary := newTestExtender(t, ctx, 2, 6)
		canary.queueChangedRecord(t, ctx, config, func(body *protocol.ExtenderRecordBody) {
			body.CanaryChannel = connect.ExtenderChannelDns
		})
		revoked := newTestExtender(t, ctx, 3, 4)
		revoked.queueRevocation(t, ctx, config)

		publisher := &testPublisher{}
		outcome := drainPublishes(ctx, publisher, publishBatchSize)
		if outcome.Claimed != 4 || outcome.Published != 2 || outcome.Withheld != 2 || outcome.Dropped != 0 || outcome.Failed != 0 {
			t.Fatalf("drain = %+v, want 4 claimed, 2 published and 2 withheld", outcome)
		}
		expectKeyHexes := []string{"record:" + hexKey(open), "revocation:" + hexKey(revoked)}
		if keyHexes := publisher.publishedKeyHexes(); !slices.Equal(keyHexes, expectKeyHexes) {
			t.Fatalf("drain published %v, want %v", keyHexes, expectKeyHexes)
		}
		for _, publish := range testPublishes(ctx) {
			if publish.PublishedTime == nil {
				t.Fatalf("row %s is still unstamped, so a withheld record would be claimed again", publish.PublishId)
			}
		}
		if outcome := drainPublishes(ctx, publisher, publishBatchSize); outcome.Claimed != 0 {
			t.Fatalf("second drain = %+v, want an empty queue", outcome)
		}
	})
}

// A record body that does not decode cannot be shown to be open, so it is not
// published; an empty message carries nothing to restrict.
func TestGossipOpenChannelMessageFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		message *protocol.ExtenderGossipMessage
		open    bool
	}{
		{name: "open record", message: testRecordMessage(t, &protocol.ExtenderRecordBody{}), open: true},
		{name: "gated record", message: testRecordMessage(t, &protocol.ExtenderRecordBody{DirectoryTier: connect.ExtenderDirectoryTierGated}), open: false},
		{name: "gated canary", message: testRecordMessage(t, &protocol.ExtenderRecordBody{DirectoryTier: connect.ExtenderDirectoryTierGated, CanaryChannel: connect.ExtenderChannelGated}), open: false},
		{name: "dns canary", message: testRecordMessage(t, &protocol.ExtenderRecordBody{CanaryChannel: connect.ExtenderChannelDns}), open: false},
		{name: "undecodable body", message: &protocol.ExtenderGossipMessage{Message: &protocol.ExtenderGossipMessage_Record{Record: &protocol.ExtenderRecord{Body: []byte{0xff, 0xff, 0xff}}}}, open: false},
		{name: "revocation", message: &protocol.ExtenderGossipMessage{Message: &protocol.ExtenderGossipMessage_Revocation{Revocation: &protocol.ExtenderRevocation{}}}, open: true},
		{name: "empty", message: &protocol.ExtenderGossipMessage{}, open: true},
	}
	for _, c := range cases {
		if open := openChannelMessage(c.message); open != c.open {
			t.Errorf("%s: open = %t, want %t", c.name, open, c.open)
		}
	}
}

func testRecordMessage(t *testing.T, body *protocol.ExtenderRecordBody) *protocol.ExtenderGossipMessage {
	t.Helper()
	bodyBytes, err := proto.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return &protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: &protocol.ExtenderRecord{Body: bodyBytes}},
	}
}
