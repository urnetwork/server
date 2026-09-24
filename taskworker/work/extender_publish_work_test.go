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
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The extender publish drip (connect/EXTENDER.md C4; connect/GEOMAP.md §2.8).

// The rotation is half the record's life, and the ticks are derived from it,
// so a record reaches every client with at least half its life left (D19).
// Pure arithmetic over the constants.
func TestExtenderPublishRotationIsHalfTheRecordLife(t *testing.T) {
	connect.AssertEqual(t, ExtenderPublishRotationTimeout, 12*time.Hour)
	connect.AssertEqual(t, 2*ExtenderPublishRotationTimeout, controller.ExtenderRecordExpireTimeout)
	connect.AssertEqual(t, ExtenderPublishRotationTickCount, 72)
	connect.AssertEqual(
		t,
		time.Duration(ExtenderPublishRotationTickCount)*ExtenderPublishTimeout,
		ExtenderPublishRotationTimeout,
	)

	// the stale cut: a record issued exactly at it has half its life left,
	// and one issued before it has less, which is when it may no longer wait
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	staleBefore := extenderPublishStaleBefore(now)
	connect.AssertEqual(t, now.Sub(staleBefore), ExtenderPublishRotationTimeout)
	connect.AssertEqual(
		t,
		staleBefore.Add(controller.ExtenderRecordExpireTimeout).Sub(now),
		controller.ExtenderRecordExpireTimeout/2,
	)
}

// The batch is a floor until the population needs more, and it rounds upward:
// a batch one short of the requirement lets the rotation drift past twelve
// hours, and every extender it leaves behind is then released at once as
// stale rather than spread over the ticks. Pure.
func TestExtenderPublishBatchSize(t *testing.T) {
	cases := []struct {
		activeCount int
		batchSize   int
	}{
		{activeCount: 0, batchSize: 8},
		{activeCount: 1, batchSize: 8},
		{activeCount: 500, batchSize: 8},
		// the most eight per tick rotates within the 72 ticks of twelve hours
		{activeCount: 576, batchSize: 8},
		{activeCount: 577, batchSize: 9},
		{activeCount: 8 * ExtenderPublishRotationTickCount, batchSize: 8},
		{activeCount: 8*ExtenderPublishRotationTickCount + 1, batchSize: 9},
		{activeCount: 9 * ExtenderPublishRotationTickCount, batchSize: 9},
		{activeCount: 9*ExtenderPublishRotationTickCount + 1, batchSize: 10},
		// a large fleet: ten thousand extenders need 139 a tick
		{activeCount: 10000, batchSize: 139},
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

		// twelve extenders, staggered so the order is not a tie, all published
		// within the rotation so none of them is stale
		extenderIds := []server.Id{}
		for i := range 12 {
			extenderIds = append(extenderIds, createTestExtender(ctx, 10+i, 4))
		}
		baseTime := server.NowUtc().Add(-6 * time.Hour)
		for i, extenderId := range extenderIds {
			stampTestExtenderPublishTime(ctx, t, extenderId, baseTime.Add(time.Duration(i)*time.Minute))
		}

		result := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, result.Active, 12)
		connect.AssertEqual(t, result.BatchSize, ExtenderPublishMinBatchSize)
		connect.AssertEqual(t, result.Selected, ExtenderPublishMinBatchSize)
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
		// valid for a day
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
				controller.ExtenderRecordExpireTimeout,
			)
			connect.AssertEqual(t, controller.ExtenderRecordExpireTimeout, 24*time.Hour)
		}
	})
}

// Moves one extender to a known publish time, as a drip publish at that time
// leaves it: its active addresses stamped and its newest record issued then.
// The stamp decides the drip order and the record time decides whether it is
// stale, so a test sets both or it describes a state no publish produces.
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
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender
			SET record_issue_time = $2
			WHERE extender_id = $1
			`,
			extenderId,
			lastPublishTime.UTC(),
		))
	})
}

// An extender whose newest record is older than the rotation is released on
// the next tick whatever the batch (GEOMAP §2.8): here twelve stale extenders
// against a batch of eight all go at once, and the three fresh ones wait
// their turn. The drip cannot let a record reach half its life while its
// extender is up, however far behind the batch has fallen.
func TestExtenderPublishReleasesStaleExtendersBeyondTheBatch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		installTestExtenderWorkConfig(t)

		now := server.NowUtc()
		staleIds := []server.Id{}
		for i := range 12 {
			extenderId := createTestExtender(ctx, 30+i, 4)
			// published just past the rotation ago, staggered
			stampTestExtenderPublishTime(ctx, t, extenderId, now.Add(-ExtenderPublishRotationTimeout-time.Duration(1+i)*time.Minute))
			staleIds = append(staleIds, extenderId)
		}
		freshIds := []server.Id{}
		for i := range 3 {
			extenderId := createTestExtender(ctx, 50+i, 4)
			stampTestExtenderPublishTime(ctx, t, extenderId, now.Add(-time.Hour-time.Duration(i)*time.Minute))
			freshIds = append(freshIds, extenderId)
		}

		result := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, result.Active, 15)
		connect.AssertEqual(t, result.BatchSize, ExtenderPublishMinBatchSize)
		connect.AssertEqual(t, result.Selected, len(staleIds))
		connect.AssertEqual(t, result.Published, len(staleIds))

		publishedExtenderIds := []server.Id{}
		for _, publish := range model.Testing_GetNetworkExtenderPublishes(ctx) {
			publishedExtenderIds = append(publishedExtenderIds, publish.ExtenderId)
		}
		for _, extenderId := range staleIds {
			if !slices.Contains(publishedExtenderIds, extenderId) {
				t.Fatalf("the stale extender %s was not released", extenderId)
			}
		}
		for _, extenderId := range freshIds {
			if slices.Contains(publishedExtenderIds, extenderId) {
				t.Fatalf("the fresh extender %s was released before its turn", extenderId)
			}
		}

		// released, they are fresh again, and the next tick is an ordinary
		// batch that starts with the oldest: the three that waited
		secondResult := runTestExtenderPublish(t, ctx)
		connect.AssertEqual(t, secondResult.Selected, ExtenderPublishMinBatchSize)
		connect.AssertEqual(t, secondResult.Published, ExtenderPublishMinBatchSize)
		secondPublishedExtenderIds := []server.Id{}
		for _, publish := range model.Testing_GetNetworkExtenderPublishes(ctx)[len(publishedExtenderIds):] {
			secondPublishedExtenderIds = append(secondPublishedExtenderIds, publish.ExtenderId)
		}
		for _, extenderId := range freshIds {
			if !slices.Contains(secondPublishedExtenderIds, extenderId) {
				t.Fatalf("the waiting extender %s was not in the next batch", extenderId)
			}
		}
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
