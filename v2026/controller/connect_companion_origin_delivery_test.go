package controller

// The companion wait against a real origin notification owner whose sockets
// are an in-memory shard broker (internal/redisshardtest): the pair subscribes
// only after a read misses (CPUOPT7-ORIGIN-DELIVERY.md §4.1), and the wait
// re-reads once that subscription is acknowledged (`registration`). Virtual
// time pins every read against the read schedule the wait keeps today
// (§2.2): 0, 100ms, then every 500ms, the final read at 3s, an edge bringing a
// read forward but never within 100ms of the previous one.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/internal/redisshardtest"
	"github.com/urnetwork/server/v2026/model"
)

// An owner whose sockets are the broker's, publishing through it.
func newOriginDeliveryTestOwner(ctx context.Context, broker *redisshardtest.Broker) *model.ContractOriginNotifications {
	return model.Testing_NewContractOriginNotifications(ctx, model.DefaultContractOriginNotificationSettings(),
		func(ctx context.Context, channel string) (server.RedisShardSubscription, error) {
			socket, err := broker.Open(ctx, channel)
			if err != nil {
				return nil, err
			}
			return socket, nil
		},
		func(_ context.Context, channel string, payload string) error {
			broker.Publish(channel, payload)
			return nil
		})
}

// Which channels of an origin's pipeline a publisher sends: the bucket
// channel only (a publisher from before Build A), the pair channel only (a
// Build B publisher), or both (Build A).
type originDeliveryTestPublisher int

const (
	originDeliveryTestBucketChannel originDeliveryTestPublisher = iota + 1
	originDeliveryTestPairChannel
	originDeliveryTestBothChannels
)

func (self originDeliveryTestPublisher) String() string {
	switch self {
	case originDeliveryTestBucketChannel:
		return "bucket channel"
	case originDeliveryTestPairChannel:
		return "pair channel"
	default:
		return "both channels"
	}
}

func publishOriginDeliveryTestEdge(broker *redisshardtest.Broker, publisher originDeliveryTestPublisher, sourceId server.Id, destinationId server.Id) {
	for i, channelPayload := range model.Testing_ContractOriginPublications(sourceId, destinationId) {
		if (i == 0 && publisher == originDeliveryTestPairChannel) || (i == 1 && publisher == originDeliveryTestBucketChannel) {
			continue
		}
		broker.Publish(channelPayload[0], channelPayload[1])
	}
}

// Whether a channel is a pair's own: the bucket's hash tag, then the pair.
func isOriginDeliveryTestPairChannel(channel string) bool {
	return strings.Contains(channel, "}:")
}

// One wait's commit: never, at a time measured from the wait's start (found by
// any read that starts at or after it), or during the first read, after its
// snapshot, with its edge published before that read returns, so before the
// pair's subscription is requested.
type originDeliveryTestCommit struct {
	never  bool
	inRead bool
	at     time.Duration
}

func (self originDeliveryTestCommit) String() string {
	switch {
	case self.never:
		return "never"
	case self.inRead:
		return "during the first read"
	default:
		return self.at.String()
	}
}

func (self originDeliveryTestCommit) foundBy(read time.Duration) bool {
	switch {
	case self.never:
		return false
	case self.inRead:
		return 0 < read
	default:
		return self.at <= read
	}
}

// What one wait did: the start of each read, whether each read's arm was
// fenced, and its result.
type originDeliveryTestWait struct {
	reads   []time.Duration
	fenced  []bool
	escrow  *model.TransferEscrow
	err     error
	elapsed time.Duration
}

// Runs one companion wait on a fresh watch of the pair, as newContract does,
// with the commit's edge published by the publisher after publishDelay.
func runOriginDeliveryTestWait(
	ctx context.Context,
	broker *redisshardtest.Broker,
	owner *model.ContractOriginNotifications,
	sourceId server.Id,
	destinationId server.Id,
	commit originDeliveryTestCommit,
	publisher originDeliveryTestPublisher,
	publishDelay time.Duration,
) originDeliveryTestWait {
	watch := owner.Watch(sourceId, destinationId)
	defer watch.Close()
	start := time.Now()
	if !commit.never && !commit.inRead {
		time.AfterFunc(commit.at+publishDelay, func() {
			publishOriginDeliveryTestEdge(broker, publisher, sourceId, destinationId)
		})
	}
	result := originDeliveryTestWait{}
	want := &model.TransferEscrow{}
	result.escrow, result.err = waitForCompanionOrigin(ctx, watch, func() (*model.TransferEscrow, error) {
		read := time.Since(start)
		result.reads = append(result.reads, read)
		if commit.inRead && len(result.reads) == 1 {
			publishOriginDeliveryTestEdge(broker, publisher, sourceId, destinationId)
		}
		if commit.foundBy(read) {
			return want, nil
		}
		return nil, model.ErrMissingCompanionOrigin
	}, companionOriginWaitEvents{
		arm: func() <-chan struct{} {
			update, fenced := watch.Update()
			result.fenced = append(result.fenced, fenced)
			return update
		},
		missed: func() <-chan struct{} {
			registration, _ := watch.Missed()
			return registration
		},
	})
	result.elapsed = time.Since(start)
	if result.escrow != want {
		result.escrow = nil
	}
	return result
}

// The reads today's wait makes on a warm bucket channel, where the commit's
// edge arrives at edgeArrival: a wake at the earlier of the edge and the
// fallback, paced to 100ms after the previous read, the last at the deadline.
func originDeliveryTestReferenceReads(commit originDeliveryTestCommit, edgeArrival time.Duration) []time.Duration {
	reads := []time.Duration{0}
	if commit.foundBy(0) {
		return reads
	}
	edgePending := !commit.never
	last := time.Duration(0)
	attempts := 1
	for {
		fallback := companionOriginFallbackTimeout
		if attempts == 1 {
			fallback = CompanionOriginWaitPollTimeout
		}
		wake := last + min(fallback, CompanionOriginWaitTimeout-last)
		// the update is armed at each read's start
		if edgePending && last <= edgeArrival && edgeArrival < wake {
			wake = edgeArrival
			edgePending = false
		}
		next := wake
		if paced := min(CompanionOriginWaitTimeout, last+CompanionOriginWaitPollTimeout); next < paced {
			next = paced
		}
		reads = append(reads, next)
		last = next
		attempts += 1
		if commit.foundBy(next) || CompanionOriginWaitTimeout <= next {
			return reads
		}
	}
}

// Keeps a watch on the pair, as a busy api process does, until its bucket's
// socket has acknowledged the bucket channel.
func warmOriginDeliveryTestBucket(owner *model.ContractOriginNotifications, latency time.Duration, sourceId server.Id, destinationId server.Id) *model.ContractOriginWatch {
	watch := owner.Watch(sourceId, destinationId)
	synctest.Wait()
	time.Sleep(latency)
	synctest.Wait()
	return watch
}

// §4.3 test 3. Every order of the read snapshot, the subscription applying
// (a), its acknowledgement (a′), the commit (c) and its edge (p), with the
// acknowledgement before and after the 100ms read, for each publisher kind.
// The broker's latency is both a′ and each edge's delivery delay; the pair's
// subscription applies when the first read misses, so a commit during that
// read publishes before a. While a′ comes before the 100ms read the reads are
// exactly today's, even when only the pair channel carries the edge. In every
// order no read passes the 3s bound, no read starts within 100ms of the
// previous one, and a commit by the deadline is found.
func TestContractOriginSubscribeAfterMissOrderings(t *testing.T) {
	latencies := []time.Duration{0, 20 * time.Millisecond, 99 * time.Millisecond, 150 * time.Millisecond, 400 * time.Millisecond}
	commits := []originDeliveryTestCommit{
		{inRead: true},
		{at: 0},
		{at: 50 * time.Millisecond},
		{at: 99 * time.Millisecond},
		{at: 101 * time.Millisecond},
		{at: 250 * time.Millisecond},
		{at: 599 * time.Millisecond},
		{at: 601 * time.Millisecond},
		{at: 1234 * time.Millisecond},
		{at: 2999 * time.Millisecond},
		{never: true},
	}
	publishDelays := []time.Duration{0, 37 * time.Millisecond}
	publishers := []originDeliveryTestPublisher{originDeliveryTestBucketChannel, originDeliveryTestPairChannel, originDeliveryTestBothChannels}
	for _, latency := range latencies {
		for _, commit := range commits {
			for _, publishDelay := range publishDelays {
				if (commit.inRead || commit.never) && publishDelay != 0 {
					continue
				}
				for _, publisher := range publishers {
					name := fmt.Sprintf("latency=%s commit=%s publish=+%s %s", latency, commit, publishDelay, publisher)
					var wait originDeliveryTestWait
					synctest.Test(t, func(t *testing.T) {
						broker := redisshardtest.NewBroker()
						broker.SetLatency(latency)
						owner := newOriginDeliveryTestOwner(t.Context(), broker)
						defer owner.Close()
						sourceId, destinationId := server.NewId(), server.NewId()
						keep := warmOriginDeliveryTestBucket(owner, latency, sourceId, destinationId)
						defer keep.Close()
						wait = runOriginDeliveryTestWait(t.Context(), broker, owner, sourceId, destinationId, commit, publisher, publishDelay)
					})
					for i := 1; i < len(wait.reads); i++ {
						if wait.reads[i]-wait.reads[i-1] < CompanionOriginWaitPollTimeout {
							t.Fatalf("%s: read %d started %s after the previous one: %v", name, i, wait.reads[i]-wait.reads[i-1], wait.reads)
						}
					}
					last := wait.reads[len(wait.reads)-1]
					if CompanionOriginWaitTimeout < last || CompanionOriginWaitTimeout < wait.elapsed {
						t.Fatalf("%s: the wait passed its bound: reads %v, elapsed %s", name, wait.reads, wait.elapsed)
					}
					if commit.never {
						if !errors.Is(wait.err, model.ErrMissingCompanionOrigin) || last != CompanionOriginWaitTimeout {
							t.Fatalf("%s: an absent origin ended with %v after reads %v", name, wait.err, wait.reads)
						}
					} else if wait.err != nil || wait.escrow == nil || !commit.foundBy(last) {
						t.Fatalf("%s: a commit by the deadline was not found: err=%v reads=%v", name, wait.err, wait.reads)
					}
					if latency < CompanionOriginWaitPollTimeout {
						edgeArrival := commit.at + publishDelay + latency
						if commit.inRead {
							edgeArrival = latency
						}
						if want := originDeliveryTestReferenceReads(commit, edgeArrival); !slices.Equal(wait.reads, want) {
							t.Fatalf("%s: reads %v, today's %v", name, wait.reads, want)
						}
						// every read from the 100ms read on is fenced
						for i := 1; i < len(wait.fenced); i++ {
							if !wait.fenced[i] {
								t.Fatalf("%s: read %d at %s was not fenced: %v", name, i, wait.reads[i], wait.fenced)
							}
						}
					}
				}
			}
		}
	}
}

// §4.3 test 4. A slow node applies the pair's subscription 200ms after the
// first read missed, and acknowledges it then: after the 100ms read. A commit
// after that read, whose edge went out on the pair channel before the
// subscription applied and so reached no one, is found by the read the
// acknowledgement wakes (`registration`), not by the 600ms fallback. On a cold
// bucket Build A's bucket channel resync wakes the wait first, so the bucket
// is warm here.
func TestContractOriginSlowAcknowledgementWakesForReread(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		broker := redisshardtest.NewBroker()
		owner := newOriginDeliveryTestOwner(t.Context(), broker)
		defer owner.Close()
		sourceId, destinationId := server.NewId(), server.NewId()
		keep := warmOriginDeliveryTestBucket(owner, 0, sourceId, destinationId)
		defer keep.Close()
		subscribeDelay := 200 * time.Millisecond
		broker.SetCommandDelay(subscribeDelay)
		registrationsBefore := testutil.ToFloat64(companionOriginWakeCounter.WithLabelValues("registration"))
		commit := originDeliveryTestCommit{at: 120 * time.Millisecond}
		wait := runOriginDeliveryTestWait(t.Context(), broker, owner, sourceId, destinationId, commit, originDeliveryTestPairChannel, 0)
		want := []time.Duration{0, 100 * time.Millisecond, subscribeDelay}
		if wait.err != nil || wait.escrow == nil || !slices.Equal(wait.reads, want) {
			t.Fatalf("reads %v err %v, want the read the acknowledgement wakes to find the commit: %v", wait.reads, wait.err, want)
		}
		if !slices.Equal(wait.fenced, []bool{false, false, true}) {
			t.Fatalf("fenced reads %v, want only the one after the acknowledgement", wait.fenced)
		}
		if registrations := testutil.ToFloat64(companionOriginWakeCounter.WithLabelValues("registration")) - registrationsBefore; registrations != 1 {
			t.Fatalf("registration wakes %v, want 1", registrations)
		}
	})
}

// §4.3 test 5. With every subscribe refused, no subscription is ever
// acknowledged: the wait reads on today's poll schedule, no read is fenced,
// and its missing origin is not recorded. With only the pair channels refused
// each failed write replaces the socket, whose bucket channel resync may bring
// a read forward; the bounds still hold and nothing is fenced.
func TestContractOriginSubscriptionFailureFallsBackToPolls(t *testing.T) {
	for _, refused := range []string{"every channel", "pair channels"} {
		synctest.Test(t, func(t *testing.T) {
			broker := redisshardtest.NewBroker()
			broker.SetFailSubscribe(func(channel string) bool {
				return refused == "every channel" || isOriginDeliveryTestPairChannel(channel)
			})
			owner := newOriginDeliveryTestOwner(t.Context(), broker)
			defer owner.Close()
			sourceId, destinationId := server.NewId(), server.NewId()
			wait := runOriginDeliveryTestWait(t.Context(), broker, owner, sourceId, destinationId, originDeliveryTestCommit{never: true}, originDeliveryTestBothChannels, 0)
			if !errors.Is(wait.err, model.ErrMissingCompanionOrigin) || wait.reads[len(wait.reads)-1] != CompanionOriginWaitTimeout {
				t.Fatalf("%s refused: an absent origin ended with %v after %v", refused, wait.err, wait.reads)
			}
			if refused == "every channel" {
				want := []time.Duration{0, 100 * time.Millisecond, 600 * time.Millisecond, 1100 * time.Millisecond, 1600 * time.Millisecond, 2100 * time.Millisecond, 2600 * time.Millisecond, 3 * time.Second}
				if !slices.Equal(wait.reads, want) {
					t.Fatalf("%s refused: reads %v, want the poll schedule %v", refused, wait.reads, want)
				}
			}
			for i := 1; i < len(wait.reads); i++ {
				if gap := wait.reads[i] - wait.reads[i-1]; gap < CompanionOriginWaitPollTimeout || companionOriginFallbackTimeout < gap {
					t.Fatalf("%s refused: reads %v outside the poll bounds", refused, wait.reads)
				}
			}
			if slices.Contains(wait.fenced, true) {
				t.Fatalf("%s refused: a read was fenced without an acknowledged subscription: %v", refused, wait.fenced)
			}
		})
	}
}

// One Stream fallback request whose destination has no origin at its first
// read, with an owner on the broker. outcome decides what the re-check finds:
// the origin still missing, an origin committed between the two reads, or no
// acknowledgement in time. Returns the result, the origin reads, the reads
// counted after an acknowledgement, and whether the refusal was recorded.
func runStreamFallbackRecheck(t testing.TB, outcome string) (*protocol.CreateContractResult, int, float64, bool) {
	previousOriginExists := companionOriginExists
	defer func() { companionOriginExists = previousOriginExists }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	broker := redisshardtest.NewBroker()
	owner := newOriginDeliveryTestOwner(ctx, broker)
	defer owner.Close()
	ownerCtx := model.WithContractOriginNotifications(ctx, owner)
	sourceNetworkId, sourceId, destinationNetworkId, destinationId := companionOriginTestSetup(ownerCtx, t)
	// a window advertises only Stream, for its return traffic
	model.SetProvide(ownerCtx, destinationId, map[model.ProvideMode][]byte{
		model.ProvideModeStream: []byte("synthetic-window-stream-key-0004"),
	})
	awaitRouteRejectionBucket(t, ownerCtx, sourceId, destinationId)
	if outcome == "acknowledgement withheld" {
		broker.SetWithholdAcknowledgement(isOriginDeliveryTestPairChannel)
	}
	reads := 0
	companionOriginExists = func(ctx context.Context, _ server.Id, _ server.Id, _ time.Duration) bool {
		reads += 1
		if outcome == "committed between the reads" && reads == 2 {
			// the window's new forward contract, before the second read
			origin, err := model.CreateTransferEscrow(ownerCtx, destinationNetworkId, destinationId, sourceNetworkId, sourceId, model.ByteCount(1024*1024))
			if err != nil || origin == nil {
				t.Errorf("origin creation failed: %v", err)
			}
			return true
		}
		return false
	}
	registrationsBefore := testutil.ToFloat64(companionOriginLookupCounter.WithLabelValues("registration"))
	frames, err := CreateContract(ownerCtx, sourceId, &protocol.CreateContract{
		DestinationId:     destinationId.Bytes(),
		TransferByteCount: uint64(1024 * 1024),
	}, connect.DefaultContractManagerSettings())
	if err != nil || len(frames) != 1 {
		t.Fatalf("%s: contract request failed: frames=%d err=%v", outcome, len(frames), err)
	}
	message, err := connect.FromFrame(frames[0])
	returnConnectControlFrames(frames)
	result, ok := message.(*protocol.CreateContractResult)
	if err != nil || !ok {
		t.Fatalf("%s: contract result was not decoded: %v", outcome, err)
	}
	registrationReads := testutil.ToFloat64(companionOriginLookupCounter.WithLabelValues("registration")) - registrationsBefore
	_, recorded := owner.RouteRejection(sourceId, destinationId, false)
	return result, reads, registrationReads, recorded
}

// §4.3 test 10. A Stream fallback whose one origin lookup missed before the
// pair's subscription was acknowledged subscribes, waits for the
// acknowledgement, and decides with one more read, which alone may record the
// refusal. The broker stands in for Redis; the request runs against the test
// database.
func TestCreateContractStreamFallbackRechecksAfterAcknowledgement(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		result, reads, registrationReads, recorded := runStreamFallbackRecheck(t, "missing at both reads")
		if !isRouteReliabilityRefusal(result) || reads != 2 || registrationReads != 1 || !recorded {
			t.Fatalf("result %+v after %d reads (%v after the acknowledgement), recorded=%t; want a refusal recorded from the second read", result, reads, registrationReads, recorded)
		}
	})
}

// §4.3 test 10. An origin committed between the miss and the re-check's read
// lets the contract proceed, and records nothing.
func TestCreateContractStreamFallbackOriginBetweenReadsProceeds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		result, reads, _, recorded := runStreamFallbackRecheck(t, "committed between the reads")
		if result.Contract == nil || reads != 2 || recorded {
			t.Fatalf("result %+v after %d reads, recorded=%t; want the contract", result, reads, recorded)
		}
	})
}

// §4.3 test 10. Without the acknowledgement in time the miss is answered and
// not stored, after one read.
func TestCreateContractStreamFallbackWithoutAcknowledgementStoresNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		result, reads, registrationReads, recorded := runStreamFallbackRecheck(t, "acknowledgement withheld")
		if !isRouteReliabilityRefusal(result) || reads != 1 || registrationReads != 0 || recorded {
			t.Fatalf("result %+v after %d reads, recorded=%t; want a refusal answered and not stored", result, reads, recorded)
		}
	})
}
