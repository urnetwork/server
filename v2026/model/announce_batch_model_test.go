// The batched announce reads and writes (announce_batch_model.go,
// verify_egress_lease_batch_model.go) against the per-connection functions
// they replace, on the fixture's isolated database and Redis: each batch
// leaves the same state and returns the same results, isolates one item's
// failure, and makes a fixed number of round trips for any number of items.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// Counts the Redis round trips whose commands name any of tokens: single
// commands, and pipelines with the commands in them. Installed on the shared
// client for one test, and inert after it.
type testRedisRoundTrips struct {
	tokens []string
	active atomic.Bool

	stateLock        sync.Mutex
	singles          int
	pipelines        int
	pipelineCommands int
}

func installTestRedisRoundTrips(ctx context.Context, tokens ...string) *testRedisRoundTrips {
	counter := &testRedisRoundTrips{tokens: tokens}
	counter.active.Store(true)
	server.Redis(ctx, func(r server.RedisClient) {
		r.AddHook(counter)
	})
	return counter
}

func (self *testRedisRoundTrips) stop() {
	self.active.Store(false)
}

func (self *testRedisRoundTrips) reset() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.singles = 0
	self.pipelines = 0
	self.pipelineCommands = 0
}

func (self *testRedisRoundTrips) counts() (singles int, pipelines int, pipelineCommands int) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.singles, self.pipelines, self.pipelineCommands
}

func (self *testRedisRoundTrips) touches(cmd redis.Cmder) bool {
	for _, arg := range cmd.Args() {
		value := fmt.Sprint(arg)
		for _, token := range self.tokens {
			if strings.Contains(value, token) {
				return true
			}
		}
	}
	return false
}

func (self *testRedisRoundTrips) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *testRedisRoundTrips) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if self.active.Load() && self.touches(cmd) {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.singles += 1
			}()
		}
		return next(ctx, cmd)
	}
}

func (self *testRedisRoundTrips) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if self.active.Load() {
			touching := 0
			for _, cmd := range cmds {
				if self.touches(cmd) {
					touching += 1
				}
			}
			if 0 < touching {
				func() {
					self.stateLock.Lock()
					defer self.stateLock.Unlock()
					self.pipelines += 1
					self.pipelineCommands += touching
				}()
			}
		}
		return next(ctx, cmds)
	}
}

// The main pool's acquires so far: one for each Db or Tx call.
func testDbAcquireCount(t testing.TB) int64 {
	t.Helper()
	stat, ok := server.DbPoolAcquireStats()
	if !ok {
		t.Fatal("the database pool is not open")
	}
	return stat.AcquireCount
}

// A batch of sync round reads returns what each read returns alone: a
// provider's changes in and out of its window, a client that provides
// nothing, a client whose mirror lapsed and one whose mirror cannot be
// decoded, both read from postgres and refilled, and one client read twice.
func TestGetProvideKeyChangesBatchMatchesSingleReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := server.NowUtc()
		providing := server.NewId()
		SetProvide(ctx, providing, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		SetProvide(ctx, providing, map[ProvideMode][]byte{
			ProvideModePublic:  make([]byte, 32),
			ProvideModeNetwork: make([]byte, 32),
		})
		idle := server.NewId()
		lapsed := server.NewId()
		SetProvide(ctx, lapsed, map[ProvideMode][]byte{ProvideModePublic: make([]byte, 32)})
		undecodable := server.NewId()
		SetProvide(ctx, undecodable, map[ProvideMode][]byte{ProvideModeNetwork: make([]byte, 32)})
		breakMirrors := func() {
			server.Redis(ctx, func(r server.RedisClient) {
				err := r.Del(ctx, provideModesKey(lapsed)).Err()
				server.Raise(err)
				err = r.Set(ctx, provideModesKey(undecodable), "not a provide mode list", time.Hour).Err()
				server.Raise(err)
			})
		}
		reads := []ProvideKeyChangesRead{
			{ClientId: providing, MinTime: start.Add(-time.Minute)},
			{ClientId: providing, MinTime: server.NowUtc().Add(time.Minute)},
			{ClientId: idle, MinTime: start.Add(-time.Minute)},
			{ClientId: lapsed, MinTime: start.Add(-time.Minute)},
			{ClientId: undecodable, MinTime: start.Add(-time.Minute)},
		}

		breakMirrors()
		batchResults := GetProvideKeyChangesBatch(ctx, reads)
		breakMirrors()
		for i, read := range reads {
			changedCount, provideModes := GetProvideKeyChanges(ctx, read.ClientId, read.MinTime)
			got := batchResults[i]
			if got.Err != nil {
				t.Fatalf("read %d failed in the batch: %v", i, got.Err)
			}
			if got.ChangedCount != changedCount || !maps.Equal(got.ProvideModes, provideModes) {
				t.Fatalf(
					"read %d: the batch read %d changes and modes %v, alone %d and %v",
					i, got.ChangedCount, got.ProvideModes, changedCount, provideModes,
				)
			}
		}
		if batchResults[0].ChangedCount == 0 || batchResults[1].ChangedCount != 0 {
			t.Fatalf("the provider's changes were %d in its window and %d after it, want some and none", batchResults[0].ChangedCount, batchResults[1].ChangedCount)
		}
		if !batchResults[3].ProvideModes[ProvideModePublic] || !batchResults[4].ProvideModes[ProvideModeNetwork] {
			t.Fatal("a client without a decodable mirror was not read from postgres")
		}

		// the postgres reads refilled both mirrors, so the next batch is one
		// statement and one pipeline
		roundTrips := installTestRedisRoundTrips(ctx, providing.String(), idle.String(), lapsed.String(), undecodable.String())
		defer roundTrips.stop()
		acquiresBefore := testDbAcquireCount(t)
		results := GetProvideKeyChangesBatch(ctx, reads)
		acquires := testDbAcquireCount(t) - acquiresBefore
		singles, pipelines, pipelineCommands := roundTrips.counts()
		for i, result := range results {
			if result.Err != nil || result.ChangedCount != batchResults[i].ChangedCount {
				t.Fatalf("read %d changed between batches: %+v", i, result)
			}
		}
		if acquires != 1 || singles != 0 || pipelines != 1 || pipelineCommands != len(reads) {
			t.Fatalf(
				"%d reads took %d database acquires, %d Redis commands and %d pipelines of %d commands, want 1, 0 and 1 of %d",
				len(reads), acquires, singles, pipelines, pipelineCommands, len(reads),
			)
		}
	})
}

// The client reliability stats hashes of the current and previous block.
func testClientReliabilityStatsState(ctx context.Context) map[string]string {
	state := map[string]string{}
	currentBlockNumber := reliabilityBlockNumber(server.NowUtc())
	server.Redis(ctx, func(r server.RedisClient) {
		for blockNumber := currentBlockNumber - 1; blockNumber <= currentBlockNumber; blockNumber += 1 {
			for shard := 0; shard < clientReliabilityStatsShardCount; shard += 1 {
				statsKey := clientReliabilityStatsKey(blockNumber, shard)
				fields, err := r.HGetAll(ctx, statsKey).Result()
				server.Raise(err)
				for field, value := range fields {
					state[statsKey+"/"+fmt.Sprintf("%x", field)] = value
				}
				if 0 < len(fields) {
					ttl, err := r.PTTL(ctx, statsKey).Result()
					server.Raise(err)
					state[statsKey+"/ttl"] = strconv.FormatBool(0 < ttl && ttl <= clientReliabilityStatsRedisTtl)
				}
			}
		}
	})
	return state
}

func testClearClientReliabilityStats(ctx context.Context) {
	currentBlockNumber := reliabilityBlockNumber(server.NowUtc())
	server.Redis(ctx, func(r server.RedisClient) {
		for blockNumber := currentBlockNumber - 1; blockNumber <= currentBlockNumber; blockNumber += 1 {
			for shard := 0; shard < clientReliabilityStatsShardCount; shard += 1 {
				err := r.Del(ctx, clientReliabilityStatsKey(blockNumber, shard)).Err()
				server.Raise(err)
			}
		}
	})
}

// A batch of stats records writes the counters the records write one by one:
// several clients in one shard hash, a record of zeros, a record whose range
// spans the previous block and the current one, and one that starts before
// the drainable window and is clamped.
func TestRecordClientReliabilityStatsRangeBatchMatchesSingleRecords(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// both runs land in one block
		if untilNextBlock := ReliabilityBlockDuration - time.Duration(server.NowUtc().UnixMilli()%ReliabilityBlockDuration.Milliseconds())*time.Millisecond; untilNextBlock < 20*time.Second {
			time.Sleep(untilNextBlock + time.Second)
		}
		blockStart := time.UnixMilli(reliabilityBlockNumber(server.NowUtc()) * ReliabilityBlockDuration.Milliseconds()).UTC()
		networkId := server.NewId()
		records := []ClientReliabilityStatsRecord{}
		addressHash := func(seed byte) [32]byte {
			var hash [32]byte
			for i := range hash {
				hash[i] = seed
			}
			return hash
		}
		// eight clients whose ids end in one byte share a shard hash
		for i := range 8 {
			clientId := server.NewId()
			clientIdBytes := clientId.Bytes()
			clientIdBytes[len(clientIdBytes)-1] = 7
			clientId, _ = server.IdFromBytes(clientIdBytes)
			records = append(records, ClientReliabilityStatsRecord{
				NetworkId:         networkId,
				ClientId:          clientId,
				ClientAddressHash: addressHash(byte(i)),
				StatsStartTime:    blockStart.Add(time.Second),
				StatsEndTime:      blockStart.Add(2 * time.Second),
				Stats: ClientReliabilityStats{
					ConnectionEstablishedCount: 1,
					ProvideEnabledCount:        1,
					ReceiveMessageCount:        uint64(i + 1),
					ReceiveByteCount:           ByteCount(1024 * (i + 1)),
				},
			})
		}
		records = append(records,
			ClientReliabilityStatsRecord{
				NetworkId:         networkId,
				ClientId:          server.NewId(),
				ClientAddressHash: addressHash(100),
				StatsStartTime:    blockStart.Add(time.Second),
				StatsEndTime:      blockStart.Add(2 * time.Second),
			},
			ClientReliabilityStatsRecord{
				NetworkId:         networkId,
				ClientId:          server.NewId(),
				ClientAddressHash: addressHash(101),
				StatsStartTime:    blockStart.Add(-30 * time.Second),
				StatsEndTime:      blockStart.Add(time.Second),
				Stats:             ClientReliabilityStats{ConnectionNewCount: 1, SendMessageCount: 3},
			},
			ClientReliabilityStatsRecord{
				NetworkId:         networkId,
				ClientId:          server.NewId(),
				ClientAddressHash: addressHash(102),
				StatsStartTime:    blockStart.Add(-10 * time.Minute),
				StatsEndTime:      blockStart.Add(time.Second),
				Stats:             ClientReliabilityStats{ProvideChangedCount: 1, SendByteCount: 7},
			},
		)

		testClearClientReliabilityStats(ctx)
		for _, record := range records {
			stats := record.Stats
			RecordClientReliabilityStatsRange(ctx, record.NetworkId, record.ClientId, record.ClientAddressHash, record.StatsStartTime, record.StatsEndTime, &stats)
		}
		singleState := testClientReliabilityStatsState(ctx)

		testClearClientReliabilityStats(ctx)
		roundTrips := installTestRedisRoundTrips(ctx, "client_reliability_stats.")
		defer roundTrips.stop()
		RecordClientReliabilityStatsRangeBatch(ctx, records)
		singles, pipelines, _ := roundTrips.counts()
		batchState := testClientReliabilityStatsState(ctx)

		if len(singleState) == 0 || !maps.Equal(singleState, batchState) {
			t.Fatalf("the batch wrote %d counters, the records alone %d:\nbatch %v\nalone %v", len(batchState), len(singleState), batchState, singleState)
		}
		if singles != 0 || pipelines != 1 {
			t.Fatalf("%d records took %d Redis commands and %d pipelines, want 0 and 1", len(records), singles, pipelines)
		}
	})
}

// The verify egress state of the clients and addresses of a test, with each
// deadline replaced by T and each ttl by whether one is set.
func testVerifyEgressState(ctx context.Context, clientIds []server.Id, ips []netip.Addr, settings *VerifySettings) map[string]string {
	normalize := func(value string) string {
		head, tail, found := strings.Cut(value, "_")
		if deadline, err := strconv.ParseInt(head, 10, 64); err == nil && 1_000_000_000_000 <= deadline {
			if found {
				return "T_" + tail
			}
			return "T"
		}
		return value
	}
	state := map[string]string{}
	server.Redis(ctx, func(r server.RedisClient) {
		readHash := func(key string) map[string]string {
			fields, err := r.HGetAll(ctx, key).Result()
			server.Raise(err)
			for field, value := range fields {
				state[key+"/"+field] = normalize(value)
			}
			if 0 < len(fields) {
				ttl, err := r.PTTL(ctx, key).Result()
				server.Raise(err)
				state[key+"/ttl"] = strconv.FormatBool(0 < ttl)
			}
			return fields
		}
		for _, clientId := range clientIds {
			for _, reverseKey := range verifyClientEgressKeys(clientId) {
				for field := range readHash(reverseKey) {
					readHash(verifyClientEgressLeaseKey(reverseKey, parseVerifyClientEgressField(field).address))
				}
			}
			eligible, err := r.SIsMember(ctx, verifyEligibleKey, clientId.String()).Result()
			server.Raise(err)
			state["eligible/"+clientId.String()] = strconv.FormatBool(eligible)
		}
		for _, ip := range ips {
			_, parsed := verifyClientEgressField(ip, settings)
			for _, egressHashHex := range parsed.forwardHashes() {
				forward, err := r.Get(ctx, verifyEgressKeyFromHex(egressHashHex)).Result()
				if err != nil && !errors.Is(err, redis.Nil) {
					server.Raise(err)
				}
				state["forward/"+egressHashHex] = forward
				readHash(verifyForwardEgressLeaseKey(egressHashHex))
			}
		}
	})
	return state
}

// Removes every key of testVerifyEgressState and the clients' membership.
func testClearVerifyEgressState(ctx context.Context, clientIds []server.Id, ips []netip.Addr, settings *VerifySettings) {
	server.Redis(ctx, func(r server.RedisClient) {
		for _, clientId := range clientIds {
			for _, reverseKey := range verifyClientEgressKeys(clientId) {
				// a key a test broke on purpose has no fields to follow
				fields, _ := r.HGetAll(ctx, reverseKey).Result()
				for field := range fields {
					err := r.Del(ctx, verifyClientEgressLeaseKey(reverseKey, parseVerifyClientEgressField(field).address)).Err()
					server.Raise(err)
				}
				err := r.Del(ctx, reverseKey).Err()
				server.Raise(err)
			}
			err := r.SRem(ctx, verifyEligibleKey, clientId.String()).Err()
			server.Raise(err)
		}
		for _, ip := range ips {
			_, parsed := verifyClientEgressField(ip, settings)
			for _, egressHashHex := range parsed.forwardHashes() {
				err := r.Del(ctx, verifyEgressKeyFromHex(egressHashHex), verifyForwardEgressLeaseKey(egressHashHex)).Err()
				server.Raise(err)
			}
		}
	})
}

// Sets a client's provide mode mirror to modes.
func testSetProvideModesMirror(ctx context.Context, clientId server.Id, provideModes ...ProvideMode) {
	if provideModes == nil {
		provideModes = []ProvideMode{}
	}
	provideModesJson, err := json.Marshal(provideModes)
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		err := r.Set(ctx, provideModesKey(clientId), provideModesJson, time.Hour).Err()
		server.Raise(err)
	})
}

// A batch of lease refreshes leaves the state the refreshes leave one by one,
// for clients that share no address: two connections of one client at one
// address, an IPv6 address and its /64, a client that provides nothing,
// another client, and a client with an expired field to prune.
func TestFeedVerifyConnectionEgressBatchMatchesSingleFeeds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("batch-connection-egress-test")
		twoConnections, ipv6, notProviding, another, expired := server.NewId(), server.NewId(), server.NewId(), server.NewId(), server.NewId()
		clientIds := []server.Id{twoConnections, ipv6, notProviding, another, expired}
		ips := []netip.Addr{
			netip.MustParseAddr("192.0.2.101"),
			netip.MustParseAddr("2001:db8:101::1"),
			netip.MustParseAddr("192.0.2.103"),
			netip.MustParseAddr("192.0.2.104"),
			netip.MustParseAddr("192.0.2.105"),
			// the address of the expired field
			netip.MustParseAddr("192.0.2.205"),
		}
		feeds := []VerifyConnectionEgressFeed{
			{ClientId: twoConnections, ConnectionId: server.NewId(), Ip: ips[0], Settings: settings},
			{ClientId: twoConnections, ConnectionId: server.NewId(), Ip: ips[0], Settings: settings},
			{ClientId: ipv6, ConnectionId: server.NewId(), Ip: ips[1], Settings: settings},
			{ClientId: notProviding, ConnectionId: server.NewId(), Ip: ips[2], Settings: settings},
			{ClientId: another, ConnectionId: server.NewId(), Ip: ips[3], Settings: settings},
			{ClientId: expired, ConnectionId: server.NewId(), Ip: ips[4], Settings: settings},
		}
		setUp := func() {
			testClearVerifyEgressState(ctx, clientIds, ips, settings)
			for _, clientId := range []server.Id{twoConnections, ipv6, another, expired} {
				testSetProvideModesMirror(ctx, clientId, ProvideModePublic)
			}
			testSetProvideModesMirror(ctx, notProviding)
			expiredField, _ := verifyClientEgressField(ips[5], settings)
			server.Redis(ctx, func(r server.RedisClient) {
				past := server.NowUtc().Add(-time.Minute).UnixMilli()
				err := r.HSet(ctx, verifyClientEgressKey(expired), expiredField, fmt.Sprintf("%d%s", past, verifyClientEgressValueSuffix(ips[5], settings))).Err()
				server.Raise(err)
			})
		}

		setUp()
		for _, feed := range feeds {
			FeedVerifyConnectionEgress(ctx, feed.ClientId, feed.ConnectionId, feed.Ip, feed.Settings)
		}
		singleState := testVerifyEgressState(ctx, clientIds, ips, settings)

		setUp()
		errs := FeedVerifyConnectionEgressBatch(ctx, feeds)
		batchState := testVerifyEgressState(ctx, clientIds, ips, settings)
		defer testClearVerifyEgressState(ctx, clientIds, ips, settings)

		for i, err := range errs {
			if err != nil {
				t.Fatalf("feed %d failed in the batch: %v", i, err)
			}
		}
		if !maps.Equal(singleState, batchState) {
			keys := slices.Sorted(maps.Keys(singleState))
			for _, key := range slices.Sorted(maps.Keys(batchState)) {
				if !slices.Contains(keys, key) {
					keys = append(keys, key)
				}
			}
			for _, key := range keys {
				if singleState[key] != batchState[key] {
					t.Errorf("%s: batch %q, alone %q", key, batchState[key], singleState[key])
				}
			}
			t.FailNow()
		}
		for _, clientId := range []server.Id{twoConnections, ipv6, another, expired} {
			if batchState["eligible/"+clientId.String()] != "true" {
				t.Fatalf("providing client %s is not eligible after the batch", clientId)
			}
		}
		if batchState["eligible/"+notProviding.String()] != "false" {
			t.Fatal("the batch made a client that provides nothing eligible")
		}
	})
}

// Two clients feeding one address in one batch both lose it to the ambiguous
// marker, and neither stays eligible, as in either order of single feeds the
// second one leaves the address ambiguous.
func TestFeedVerifyConnectionEgressBatchSharedAddress(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("batch-shared-egress-test")
		first, second := server.NewId(), server.NewId()
		ip := netip.MustParseAddr("192.0.2.150")
		testSetProvideModesMirror(ctx, first, ProvideModePublic)
		testSetProvideModesMirror(ctx, second, ProvideModePublic)
		defer testClearVerifyEgressState(ctx, []server.Id{first, second}, []netip.Addr{ip}, settings)
		errs := FeedVerifyConnectionEgressBatch(ctx, []VerifyConnectionEgressFeed{
			{ClientId: first, ConnectionId: server.NewId(), Ip: ip, Settings: settings},
			{ClientId: second, ConnectionId: server.NewId(), Ip: ip, Settings: settings},
		})
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("a shared address failed its feeds: %v", errs)
		}
		if resolved := ResolveVerifyEgress(ctx, ip, settings); resolved != nil {
			t.Fatalf("a shared address resolved to %s", *resolved)
		}
		if testVerifyEligible(ctx, first) || testVerifyEligible(ctx, second) {
			t.Fatal("a client of a shared address stayed eligible")
		}
	})
}

// A feed whose reverse index holds the wrong kind of value fails alone: it
// writes no forward entry and is not made eligible, and every other feed of
// the batch writes its lease and is made eligible.
func TestFeedVerifyConnectionEgressBatchIsolatesAFailedFeed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("batch-failed-egress-test")
		clientIds := []server.Id{}
		ips := []netip.Addr{}
		feeds := []VerifyConnectionEgressFeed{}
		for i := range 6 {
			clientId := server.NewId()
			ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 160+i))
			clientIds = append(clientIds, clientId)
			ips = append(ips, ip)
			testSetProvideModesMirror(ctx, clientId, ProvideModePublic)
			feeds = append(feeds, VerifyConnectionEgressFeed{ClientId: clientId, ConnectionId: server.NewId(), Ip: ip, Settings: settings})
		}
		defer testClearVerifyEgressState(ctx, clientIds, ips, settings)
		const failing = 3
		server.Redis(ctx, func(r server.RedisClient) {
			err := r.Set(ctx, verifyClientEgressKey(clientIds[failing]), "not a lease index", time.Minute).Err()
			server.Raise(err)
		})
		errs := FeedVerifyConnectionEgressBatch(ctx, feeds)
		for i, err := range errs {
			if i == failing {
				if err == nil || !strings.Contains(err.Error(), "WRONGTYPE") {
					t.Fatalf("the feed with a broken index returned %v, want its WRONGTYPE error", err)
				}
				continue
			}
			if err != nil {
				t.Fatalf("feed %d failed with the broken one: %v", i, err)
			}
			if resolved := ResolveVerifyEgress(ctx, ips[i], settings); resolved == nil || *resolved != clientIds[i] {
				t.Fatalf("feed %d's address resolved to %v, want its client", i, resolved)
			}
			if !testVerifyEligible(ctx, clientIds[i]) {
				t.Fatalf("feed %d's client is not eligible", i)
			}
		}
		_, parsed := verifyClientEgressField(ips[failing], settings)
		forward := ""
		server.Redis(ctx, func(r server.RedisClient) {
			forward, _ = r.Get(ctx, verifyEgressKeyFromHex(parsed.address)).Result()
		})
		if forward != "" || testVerifyEligible(ctx, clientIds[failing]) {
			t.Fatalf("the failed feed went on: forward %q, eligible %t", forward, testVerifyEligible(ctx, clientIds[failing]))
		}
	})
}

// Sixteen refreshes in one batch take six pipelines and no single command:
// the reverse leases, the forward leases, the reverse reads, the forward
// reads, the provide modes and the set write. Alone, each takes its own
// commands.
func TestFeedVerifyConnectionEgressBatchRoundTrips(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultVerifySettings()
		settings.EgressHashKey = []byte("batch-round-trip-egress-test")
		const feedCount = 16
		clientIds := []server.Id{}
		ips := []netip.Addr{}
		feeds := []VerifyConnectionEgressFeed{}
		tokens := []string{}
		for i := range feedCount {
			clientId := server.NewId()
			ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", 170+i))
			clientIds = append(clientIds, clientId)
			ips = append(ips, ip)
			testSetProvideModesMirror(ctx, clientId, ProvideModePublic)
			feeds = append(feeds, VerifyConnectionEgressFeed{ClientId: clientId, ConnectionId: server.NewId(), Ip: ip, Settings: settings})
			_, parsed := verifyClientEgressField(ip, settings)
			tokens = append(tokens, clientId.String(), parsed.address)
		}
		defer testClearVerifyEgressState(ctx, clientIds, ips, settings)
		roundTrips := installTestRedisRoundTrips(ctx, tokens...)
		defer roundTrips.stop()

		for _, feed := range feeds {
			FeedVerifyConnectionEgress(ctx, feed.ClientId, feed.ConnectionId, feed.Ip, feed.Settings)
		}
		aloneSingles, alonePipelines, _ := roundTrips.counts()
		roundTrips.reset()
		errs := FeedVerifyConnectionEgressBatch(ctx, feeds)
		singles, pipelines, pipelineCommands := roundTrips.counts()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("feed %d failed: %v", i, err)
			}
		}
		t.Logf(
			"%d feeds alone: %d commands and %d pipelines; in a batch: %d commands and %d pipelines of %d commands",
			feedCount, aloneSingles, alonePipelines, singles, pipelines, pipelineCommands,
		)
		if singles != 0 || pipelines != 6 {
			t.Fatalf("a batch of %d feeds took %d commands and %d pipelines, want 0 and 6", feedCount, singles, pipelines)
		}
		if aloneSingles+alonePipelines < 6*feedCount {
			t.Fatalf("%d feeds alone took %d round trips, fewer than the six each the batch replaces", feedCount, aloneSingles+alonePipelines)
		}
	})
}
