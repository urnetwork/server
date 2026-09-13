package work

import (
	"context"
	"net/netip"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The extender publish drip (connect/EXTENDER.md C4).

// The batch is a floor until the population needs more, and it rounds upward:
// a batch one short of the requirement lets the rotation drift past seven days
// and eventually past a record's fourteen day expiry.
func TestExtenderPublishBatchSize(t *testing.T) {
	cases := []struct {
		activeCount int
		batchSize   int
	}{
		{activeCount: 0, batchSize: 8},
		{activeCount: 1, batchSize: 8},
		{activeCount: 1008, batchSize: 8},
		{activeCount: 1009, batchSize: 8},
		// the most eight per tick rotates within the window
		{activeCount: 8 * ExtenderPublishRotationTickCount, batchSize: 8},
		{activeCount: 8*ExtenderPublishRotationTickCount + 1, batchSize: 9},
		{activeCount: 9 * ExtenderPublishRotationTickCount, batchSize: 9},
		{activeCount: 9*ExtenderPublishRotationTickCount + 1, batchSize: 10},
	}
	for _, c := range cases {
		batchSize := extenderPublishBatchSize(c.activeCount)
		if batchSize != c.batchSize {
			t.Errorf(
				"extenderPublishBatchSize(%d) = %d, want %d",
				c.activeCount,
				batchSize,
				c.batchSize,
			)
		}
		// the whole population must be covered within the rotation window
		if c.activeCount > batchSize*ExtenderPublishRotationTickCount {
			t.Errorf(
				"a batch of %d leaves %d extenders unpublished within the rotation window",
				batchSize,
				c.activeCount,
			)
		}
	}
}

func runTestExtenderPublish(t testing.TB, ctx context.Context) *ExtenderPublishResult {
	t.Helper()
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := ExtenderPublish(&ExtenderPublishArgs{}, clientSession)
	if err != nil {
		t.Fatalf("ExtenderPublish: %v", err)
	}
	return result
}

// The drip takes the oldest records first and stamps what it published, so a
// batch that has just been published goes to the back and the whole set
// rotates instead of the head being republished forever.
func TestExtenderPublishDripsOldestFirstAndStampsThem(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installTestExtenderWorkConfig(t)

		// twelve extenders, staggered so the order is not a tie
		extenderIds := []server.Id{}
		for i := range 12 {
			extenderIds = append(extenderIds, createTestExtender(ctx, 10+i, 4))
		}
		baseTime := server.NowUtc().Add(-24 * time.Hour)
		for i, extenderId := range extenderIds {
			stampTestExtenderPublishTime(ctx, t, extenderId, baseTime.Add(time.Duration(i)*time.Minute))
		}

		result := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, result.Active, 12)
		connect.AssertEqual(t, result.BatchSize, ExtenderPublishMinBatchSize)
		connect.AssertEqual(t, result.Published, ExtenderPublishMinBatchSize)

		firstPublishes := model.Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(firstPublishes), ExtenderPublishMinBatchSize)
		firstPublishIds := []server.Id{}
		firstExtenderIds := []server.Id{}
		for _, publish := range firstPublishes {
			connect.AssertEqual(t, publish.Kind, model.NetworkExtenderPublishKindRecord)
			firstPublishIds = append(firstPublishIds, publish.PublishId)
			firstExtenderIds = append(firstExtenderIds, publish.ExtenderId)
		}
		for _, extenderId := range extenderIds[0:ExtenderPublishMinBatchSize] {
			if !slices.Contains(firstExtenderIds, extenderId) {
				t.Fatalf("the oldest extender %s was not published", extenderId)
			}
		}

		// the stamped batch is now the newest, so the second tick takes the
		// four that were left before it comes back to any of the first batch
		second := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, second.Published, ExtenderPublishMinBatchSize)
		secondExtenderIds := []server.Id{}
		for _, publish := range model.Testing_GetNetworkExtenderPublishes(ctx) {
			if !slices.Contains(firstPublishIds, publish.PublishId) {
				secondExtenderIds = append(secondExtenderIds, publish.ExtenderId)
			}
		}
		connect.AssertEqual(t, len(secondExtenderIds), ExtenderPublishMinBatchSize)
		for _, extenderId := range extenderIds[ExtenderPublishMinBatchSize:] {
			if !slices.Contains(secondExtenderIds, extenderId) {
				t.Fatalf("the never-stamped extender %s was not published second", extenderId)
			}
		}

		// every published record verifies under the configured root key and is
		// valid for fourteen days
		keySet := connect.NewExtenderRootKeySet(rootPublicKey)
		for _, publish := range model.Testing_GetNetworkExtenderPublishes(ctx) {
			message := &protocol.ExtenderGossipMessage{}
			if err := proto.Unmarshal(publish.Message, message); err != nil {
				t.Fatalf("the publish row is not a gossip message: %v", err)
			}
			body, err := keySet.VerifyRecord(message.GetRecord())
			if err != nil {
				t.Fatalf("a published record does not verify: %v", err)
			}
			connect.AssertEqual(t, body.NetworkHost, testExtenderWorkNetworkHost)
			connect.AssertEqual(t, len(body.Addresses), 1)
			issueTime := time.UnixMilli(int64(body.IssueTimeMs))
			expireTime := time.UnixMilli(int64(body.ExpireTimeMs))
			connect.AssertEqual(
				t,
				expireTime.Sub(issueTime).Round(time.Minute),
				14*24*time.Hour,
			)
		}
	})
}

// Moves one extender's active addresses to a known publish time so the drip
// order is deterministic.
func stampTestExtenderPublishTime(
	ctx context.Context,
	t testing.TB,
	extenderId server.Id,
	lastPublishTime time.Time,
) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_address
			SET last_publish_time = $2
			WHERE extender_id = $1
			`,
			extenderId,
			lastPublishTime.UTC(),
		))
	})
}

// Above what the floor can rotate within the window, the batch grows with the
// population. Without this an operator large enough would publish records
// slower than they expire and its own directory would drain.
func TestExtenderPublishGrowsTheBatchWithThePopulation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t)

		activeCount := 8*ExtenderPublishRotationTickCount + 1
		model.Testing_CreateNetworkExtenderPopulation(
			ctx,
			server.NewId(),
			server.NewId(),
			activeCount,
		)

		result := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, result.Active, activeCount)
		connect.AssertEqual(t, result.BatchSize, 9)
		connect.AssertEqual(t, result.Published, 9)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 9)
	})
}

// A dripped record carries the dns ports of every family the extender has, as
// one ascending union (L2, C4). The record names one extender rather than one
// address, so a port that only the v6 family answered on still has to be in it
// -- a client that reads the record picks its own family's address and dials
// the ports the record lists.
func TestExtenderPublishCarriesTheDnsPortUnion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rootPublicKey := installTestExtenderWorkConfig(t)

		extenderId := server.NewId()
		createTime := server.NowUtc()
		model.Testing_CreateNetworkExtender(
			ctx,
			&model.NetworkExtender{
				ExtenderId:  extenderId,
				NetworkId:   server.NewId(),
				ClientId:    server.NewId(),
				PublicKey:   []byte("extender-public-key-work-dnsports"),
				CreateTime:  createTime,
				TcpPort:     443,
				UdpPort:     443,
				DnsPort:     connect.DefaultWhodisPort,
				DnsTld:      connect.DefaultExtenderDnsTld,
				CountryCode: "US",
				Active:      true,
			},
			[]*model.NetworkExtenderAddress{
				{
					IpVersion:    4,
					Ip:           netip.MustParseAddr("192.0.2.40"),
					Carriers:     []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns},
					DnsPorts:     []int{connect.DefaultWhodisPort},
					ActivateTime: createTime,
					Active:       true,
				},
				{
					IpVersion:    6,
					Ip:           netip.MustParseAddr("2001:db8::40"),
					Carriers:     []string{connect.ExtenderCarrierTcp, connect.ExtenderCarrierDns},
					DnsPorts:     []int{connect.DefaultWhodisPort, connect.DefaultDnsPort},
					ActivateTime: createTime,
					Active:       true,
				},
			},
		)

		result := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, result.Published, 1)

		publishes := model.Testing_GetNetworkExtenderPublishes(ctx)
		connect.AssertEqual(t, len(publishes), 1)
		message := &protocol.ExtenderGossipMessage{}
		if err := proto.Unmarshal(publishes[0].Message, message); err != nil {
			t.Fatalf("the publish row is not a gossip message: %v", err)
		}
		body, err := connect.NewExtenderRootKeySet(rootPublicKey).VerifyRecord(message.GetRecord())
		if err != nil {
			t.Fatalf("the published record does not verify: %v", err)
		}
		connect.AssertEqual(t, len(body.Addresses), 2)
		wantDnsPorts := []uint32{connect.DefaultDnsPort, connect.DefaultWhodisPort}
		if !slices.Equal(body.DnsPorts, wantDnsPorts) {
			t.Fatalf("record dns ports = %v, want %v", body.DnsPorts, wantDnsPorts)
		}
		// the configured port is still the extender's own, for a reader that
		// predates the list
		connect.AssertEqual(t, int(body.DnsPort), connect.DefaultWhodisPort)
	})
}

// An operator with no root key publishes nothing rather than queueing
// unsigned messages, and the chain still re-arms.
func TestExtenderPublishPausesWithoutARootKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfigYaml(t, "network_host: "+testExtenderWorkNetworkHost)
		createTestExtender(ctx, 100, 4)

		result := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, result.Published, 0)
		connect.AssertEqual(t, len(model.Testing_GetNetworkExtenderPublishes(ctx)), 0)
	})
}

// The chain re-arms every ten minutes; a Post that did not reschedule would
// stop the rotation and every record would eventually expire.
func TestExtenderPublishPostRearmsTheChain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer clientSession.Cancel()

		before := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := ExtenderPublishPost(
				&ExtenderPublishArgs{},
				&ExtenderPublishResult{},
				clientSession,
				tx,
			); err != nil {
				t.Fatalf("ExtenderPublishPost: %v", err)
			}
		})

		runAt := testExtenderTaskRunAt(t, ctx, "extender_publish")
		want := before.Add(ExtenderPublishTimeout)
		if runAt.Before(want.Add(-time.Second)) || want.Add(5*time.Second).Before(runAt) {
			t.Fatalf("publish run_at = %s, want about %s", runAt, want)
		}
	})
}
