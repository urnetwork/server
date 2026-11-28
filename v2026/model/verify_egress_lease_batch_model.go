package model

// A connect process refreshes every connection's verify egress lease each
// EgressRefreshInterval (FeedVerifyConnectionEgress), and one refresh was
// seven to ten Redis round trips in sequence: about 1.2% of connect's CPU on
// 2026-11-20 (LBALTOPT2.md R18). Connect's announce batcher gathers the
// refreshes that come due together, and FeedVerifyConnectionEgressBatch
// serves them with one pipeline a step.

import (
	"context"
	"errors"
	"net/netip"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// One connection's verify egress lease refresh (FeedVerifyConnectionEgress).
type VerifyConnectionEgressFeed struct {
	ClientId     server.Id
	ConnectionId server.Id
	Ip           netip.Addr
	Settings     *VerifySettings
}

// FeedVerifyConnectionEgressBatch feeds many connections' leases, a pipeline
// a step in place of each feed's round trips, with each feed's steps in the
// order its own feed takes them: the reverse lease;
// the forward lease of the address, then of its IPv6 /64; then the
// eligibility of each fed client, read and written once for all its feeds.
// Every command routes by its own key, and every script keeps its keys under
// one hash tag, so each pipeline is cluster-safe.
//
// A feed whose command fails gets that error and takes no later step, as the
// raise ends one feed today, and the other feeds go on: a command that fails
// for its connection or its node, after go-redis's own retries of the
// pipeline, fails the feeds that sent it, not the batch, so one unreachable
// node fails only the feeds whose keys it holds. Eligibility that fails fails
// each feed of its client that reached it. The errors are in the order of
// feeds.
func FeedVerifyConnectionEgressBatch(ctx context.Context, feeds []VerifyConnectionEgressFeed) []error {
	errs := make([]error, len(feeds))
	if len(feeds) == 0 {
		return errs
	}
	now := server.NowUtc()
	type feedLease struct {
		// a valid address: the lease steps apply
		valid         bool
		field         string
		valueSuffix   string
		forwardHashes []string
	}
	leases := make([]feedLease, len(feeds))
	for i, feed := range feeds {
		if !feed.Ip.IsValid() {
			continue
		}
		field, _ := verifyClientEgressField(feed.Ip, feed.Settings)
		leases[i] = feedLease{
			valid:         true,
			field:         field,
			valueSuffix:   verifyClientEgressValueSuffix(feed.Ip, feed.Settings),
			forwardHashes: parseVerifyClientEgressField(field).forwardHashes(),
		}
	}

	// One pipeline of the commands queue returns for each feed it applies to
	// that has not failed, if any; a feed whose command fails takes its error.
	step := func(applies func(i int) bool, queue func(pipe redis.Pipeliner, i int) []redis.Cmder) {
		stepFeeds := []int{}
		for i := range feeds {
			if errs[i] == nil && applies(i) {
				stepFeeds = append(stepFeeds, i)
			}
		}
		if len(stepFeeds) == 0 {
			return
		}
		server.Redis(ctx, func(r server.RedisClient) {
			feedCmds := make([][]redis.Cmder, len(feeds))
			r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, i := range stepFeeds {
					feedCmds[i] = queue(pipe, i)
				}
				return nil
			})
			for i, cmds := range feedCmds {
				for _, cmd := range cmds {
					if err := cmd.Err(); err != nil && !errors.Is(err, redis.Nil) && errs[i] == nil {
						errs[i] = err
					}
				}
			}
		})
	}

	step(func(i int) bool {
		return leases[i].valid
	}, func(pipe redis.Pipeliner, i int) []redis.Cmder {
		feed := feeds[i]
		reverseKeys, reverseArgs := verifyReverseEgressLeaseCall(
			verifyClientEgressKey(feed.ClientId),
			leases[i].field,
			leases[i].valueSuffix,
			feed.ConnectionId.String(),
			now,
			now.Add(feed.Settings.EgressTtl),
		)
		return []redis.Cmder{
			pipe.Eval(ctx, verifyReverseEgressLeaseScript, reverseKeys, reverseArgs...),
		}
	})
	// each forward entry in turn: an IPv6 address's /64 only after its own
	maxForwardCount := 0
	for _, lease := range leases {
		maxForwardCount = max(maxForwardCount, len(lease.forwardHashes))
	}
	for forwardIndex := range maxForwardCount {
		step(func(i int) bool {
			return leases[i].valid && forwardIndex < len(leases[i].forwardHashes)
		}, func(pipe redis.Pipeliner, i int) []redis.Cmder {
			feed := feeds[i]
			forwardKeys, forwardArgs := verifyForwardEgressLeaseCall(
				feed.ClientId,
				leases[i].forwardHashes[forwardIndex],
				feed.ConnectionId.String(),
				now,
				now.Add(feed.Settings.EgressTtl),
			)
			return []redis.Cmder{
				pipe.Eval(ctx, verifyForwardEgressLeaseScript, forwardKeys, forwardArgs...),
			}
		})
	}

	// each client once, for its feeds that wrote their leases
	clientIds := []server.Id{}
	clientFeeds := map[server.Id][]int{}
	for i, feed := range feeds {
		if errs[i] != nil || !leases[i].valid {
			continue
		}
		if _, ok := clientFeeds[feed.ClientId]; !ok {
			clientIds = append(clientIds, feed.ClientId)
		}
		clientFeeds[feed.ClientId] = append(clientFeeds[feed.ClientId], i)
	}
	clientErrs := updateVerifyEligibleMembershipBatch(ctx, clientIds)
	for j, clientId := range clientIds {
		if clientErrs[j] == nil {
			continue
		}
		for _, i := range clientFeeds[clientId] {
			errs[i] = clientErrs[j]
		}
	}
	return errs
}

// updateVerifyEligibleMembership for many clients, a pipeline a step: both
// reverse hashes of each client, the prune of its expired v4 fields, its
// forward entries, the provide modes of the eligible, then one set write for
// all. A client whose read fails gets that error, as the raise ends its own
// update, and leaves its membership as it was. The errors are in the order of
// clientIds.
func updateVerifyEligibleMembershipBatch(ctx context.Context, clientIds []server.Id) []error {
	errs := make([]error, len(clientIds))
	if len(clientIds) == 0 {
		return errs
	}
	nowMs := uint64(server.NowUtc().UnixMilli())

	// One pipeline of the commands queue returns for each client it applies
	// to that has not failed, if any, as FeedVerifyConnectionEgressBatch's
	// steps.
	step := func(applies func(j int) bool, queue func(pipe redis.Pipeliner, j int) []redis.Cmder) [][]redis.Cmder {
		clientCmds := make([][]redis.Cmder, len(clientIds))
		stepClients := []int{}
		for j := range clientIds {
			if errs[j] == nil && applies(j) {
				stepClients = append(stepClients, j)
			}
		}
		if len(stepClients) == 0 {
			return clientCmds
		}
		server.Redis(ctx, func(r server.RedisClient) {
			r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for _, j := range stepClients {
					clientCmds[j] = queue(pipe, j)
				}
				return nil
			})
		})
		for j, cmds := range clientCmds {
			for _, cmd := range cmds {
				if err := cmd.Err(); err != nil && !errors.Is(err, redis.Nil) && errs[j] == nil {
					errs[j] = err
				}
			}
		}
		return clientCmds
	}
	always := func(int) bool {
		return true
	}

	// readVerifyLiveEgress: both reverse hashes, the current one first
	reverseCmds := step(always, func(pipe redis.Pipeliner, j int) []redis.Cmder {
		cmds := []redis.Cmder{}
		for _, reverseKey := range verifyClientEgressKeys(clientIds[j]) {
			cmds = append(cmds, pipe.HGetAll(ctx, reverseKey))
		}
		return cmds
	})
	lives := make([]verifyLiveEgress, len(clientIds))
	expiredFields := make([]map[string]string, len(clientIds))
	for j := range clientIds {
		if errs[j] != nil {
			continue
		}
		versions := []map[string]string{}
		for version, cmd := range reverseCmds[j] {
			live, expired := splitVerifyEgressFields(cmd.(*redis.MapStringStringCmd).Val(), nowMs)
			if version == 0 && 0 < len(expired) {
				expiredFields[j] = expired
			}
			versions = append(versions, live)
		}
		lives[j] = newVerifyLiveEgress(versions[0], versions[1])
	}
	step(func(j int) bool {
		return 0 < len(expiredFields[j])
	}, func(pipe redis.Pipeliner, j int) []redis.Cmder {
		return []redis.Cmder{
			queueVerifyExpiredEgressHashPrune(ctx, pipe, clientIds[j], expiredFields[j]),
		}
	})

	// verifyClientHasLiveEgress: the forward entries eligibility reads
	forwardHashes := make([][]string, len(clientIds))
	for j := range clientIds {
		forwardHashes[j] = lives[j].eligibilityForwardHashes()
	}
	forwardCmds := step(func(j int) bool {
		return 0 < len(forwardHashes[j])
	}, func(pipe redis.Pipeliner, j int) []redis.Cmder {
		cmds := []redis.Cmder{}
		for _, egressHashHex := range forwardHashes[j] {
			cmds = append(cmds, pipe.Get(ctx, verifyEgressKeyFromHex(egressHashHex)))
		}
		return cmds
	})
	eligibles := make([]bool, len(clientIds))
	eligibleClientIds := []server.Id{}
	eligibleIndexes := []int{}
	for j, clientId := range clientIds {
		if errs[j] != nil {
			continue
		}
		forwards := map[string]string{}
		for k, egressHashHex := range forwardHashes[j] {
			forwards[egressHashHex] = forwardCmds[j][k].(*redis.StringCmd).Val()
		}
		if lives[j].attributes(clientId, forwards) {
			eligibleClientIds = append(eligibleClientIds, clientId)
			eligibleIndexes = append(eligibleIndexes, j)
		}
	}

	// an eligible client also needs an active provide mode (§5.1); a read
	// that raises fails its client, as in updateVerifyEligibleMembership
	provideModes, provideModesErrs := GetProvideModesBatch(ctx, eligibleClientIds)
	for k, j := range eligibleIndexes {
		if provideModesErrs[k] != nil {
			errs[j] = provideModesErrs[k]
			continue
		}
		eligibles[j] = 0 < len(provideModes[k])
	}

	addMembers := []any{}
	removeMembers := []any{}
	for j, clientId := range clientIds {
		if errs[j] != nil {
			continue
		}
		if eligibles[j] {
			addMembers = append(addMembers, clientId.String())
		} else {
			removeMembers = append(removeMembers, clientId.String())
		}
	}
	// the set write's results are not read, as in
	// updateVerifyEligibleMembership: membership is checked again at sampling
	server.Redis(ctx, func(r server.RedisClient) {
		r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			if 0 < len(addMembers) {
				pipe.SAdd(ctx, verifyEligibleKey, addMembers...)
				// hygiene bound only (updateVerifyEligibleMembership)
				pipe.Expire(ctx, verifyEligibleKey, verifyEligibleKeyTtl)
			}
			if 0 < len(removeMembers) {
				pipe.SRem(ctx, verifyEligibleKey, removeMembers...)
			}
			return nil
		})
	})
	return errs
}
