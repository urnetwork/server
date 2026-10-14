// The proxy-side feeder's periodic refresh re-feeds the egress index from the
// current `proxy_client` allocations; the create-side feed in
// `CreateProxyClient` covers the gap until the first refresh. Rows deleted by
// the reaper stop being re-fed and age out within one `EgressTtl`.
//
// Every allocation has to be fed again before its lease lapses, and Main has
// about 820k of them. Feeding one allocation at a time costs seven dependent
// Redis round trips, and for a client without provide keys, which is nearly
// every proxy client, a PostgreSQL read and a mirror refill as well, so one
// pass took several times the egress ttl. A sweep therefore walks the
// allocations in client order one chunk at a time and feeds a chunk in a few
// pipelined waves, with the provide keys of the whole chunk read by the same
// statement that reads its allocations.
//
// A chunk applies the lease scripts, the bijection checks and the eligibility
// rule of `feedVerifyEgressLease` and `updateVerifyEligibleMembership` in
// their per-client order: leases, then the client's live addresses in both
// reverse index versions, then the forward entries eligibility reads
// (verifyLiveEgress.eligibilityForwardHashes), then membership. Clients of
// one chunk are independent: each lease and the shared forward entries are
// updated by slot-local scripts, and eligibility is rechecked when sampled. A
// chunk is fed completely before it is reported, so its last client is a safe
// position to resume from.
package model

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Allocations read and fed by one chunk of a refresh sweep. A wave puts about
// two commands per allocation on the cluster at once, so a chunk stays small
// enough to hold no node for more than a few milliseconds.
const VerifyProxyEgressRefreshChunkSize = 1024

// The lease scripts by hash, so a wave does not resend their source for every
// allocation. A node that has not cached a script refuses the hash, and the
// refused calls are sent again with the source, which caches it.
var verifyReverseEgressLeaseRedisScript = redis.NewScript(verifyReverseEgressLeaseScript)
var verifyForwardEgressLeaseRedisScript = redis.NewScript(verifyForwardEgressLeaseScript)

// A context-local observer sees each chunk a refresh fed and the network
// round trips it took. Tests charge synthetic time from it; production
// contexts carry none.
type verifyProxyEgressRefreshObserverKey struct{}

func Testing_WithVerifyProxyEgressRefreshObserver(ctx context.Context, observe func(clientIds []server.Id, roundTripCount int)) context.Context {
	return context.WithValue(ctx, verifyProxyEgressRefreshObserverKey{}, observe)
}

// One allocation: its address's v4 reverse field and the value suffix its
// feed writes (verifyClientEgressField, verifyClientEgressValueSuffix).
type verifyProxyEgress struct {
	clientId      server.Id
	field         string
	valueSuffix   string
	hasProvideKey bool
}

// Re-feeds every current proxy allocation in one complete sweep.
func RefreshVerifyProxyEgress(ctx context.Context, settings *VerifySettings) {
	after := server.Id{}
	for {
		last, clientCount := RefreshVerifyProxyEgressAfter(ctx, after, settings)
		if clientCount < VerifyProxyEgressRefreshChunkSize {
			return
		}
		after = last
	}
}

// Re-feeds the next chunk of proxy allocations after `after` in client order.
// Returns the last client of the chunk and its client count; fewer than a
// chunk means the sweep reached the last allocation. Any failed command is
// raised, so a returned chunk was fed completely.
func RefreshVerifyProxyEgressAfter(
	ctx context.Context,
	after server.Id,
	settings *VerifySettings,
) (last server.Id, clientCount int) {
	proxyEgresses := make([]*verifyProxyEgress, 0, VerifyProxyEgressRefreshChunkSize)
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				proxy_client.client_id,
				proxy_client.client_ipv4,
				EXISTS (
					SELECT 1 FROM provide_key
					WHERE provide_key.client_id = proxy_client.client_id
				) AS has_provide_key
			FROM proxy_client
			WHERE proxy_client.client_ipv4 IS NOT NULL AND proxy_client.client_id > $1
			ORDER BY proxy_client.client_id
			LIMIT $2
			`,
			after,
			VerifyProxyEgressRefreshChunkSize,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var clientIpv4 int64
				var hasProvideKey bool
				server.Raise(result.Scan(&clientId, &clientIpv4, &hasProvideKey))
				ip := IntToIpv4(clientIpv4)
				field, _ := verifyClientEgressField(ip, settings)
				proxyEgresses = append(proxyEgresses, &verifyProxyEgress{
					clientId:      clientId,
					field:         field,
					valueSuffix:   verifyClientEgressValueSuffix(ip, settings),
					hasProvideKey: hasProvideKey,
				})
			}
		})
	})
	if len(proxyEgresses) == 0 {
		return after, 0
	}

	var waveCount int
	server.Redis(ctx, func(r server.RedisClient) {
		waveCount = feedVerifyProxyEgresses(ctx, r, proxyEgresses, settings)
	})
	if observe, ok := ctx.Value(verifyProxyEgressRefreshObserverKey{}).(func([]server.Id, int)); ok {
		clientIds := make([]server.Id, len(proxyEgresses))
		for index, egress := range proxyEgresses {
			clientIds[index] = egress.clientId
		}
		// the chunk read and the waves
		observe(clientIds, 1+waveCount)
	}
	return proxyEgresses[len(proxyEgresses)-1].clientId, len(proxyEgresses)
}

// Feeds proxy allocations of distinct clients in pipelined waves and returns
// the waves sent. The provide keys come from PostgreSQL with the allocations
// rather than from the provide mirror: the mirror holds a client without keys
// for a minute, so a sweep would read PostgreSQL once per such client anyway.
func feedVerifyProxyEgresses(
	ctx context.Context,
	r server.RedisClient,
	proxyEgresses []*verifyProxyEgress,
	settings *VerifySettings,
) (waveCount int) {
	now := server.NowUtc()
	nowMs := uint64(now.UnixMilli())
	expires := now.Add(settings.EgressTtl)

	// pipelined commands report their own errors; a missing key is not one
	raiseErrors := func(cmds []redis.Cmder) {
		for _, cmd := range cmds {
			if err := cmd.Err(); err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
		}
	}

	// leases: the reverse update of every allocation and the update of each
	// forward entry its address feeds
	type leaseCall struct {
		script *redis.Script
		keys   []string
		args   []any
	}
	leaseCalls := []leaseCall{}
	for _, egress := range proxyEgresses {
		keys, args := verifyReverseEgressLeaseCall(verifyClientEgressKey(egress.clientId), egress.field, egress.valueSuffix, verifyProxyEgressLeaseOwner, now, expires)
		leaseCalls = append(leaseCalls, leaseCall{verifyReverseEgressLeaseRedisScript, keys, args})
		for _, egressHashHex := range parseVerifyClientEgressField(egress.field).forwardHashes() {
			keys, args := verifyForwardEgressLeaseCall(egress.clientId, egressHashHex, verifyProxyEgressLeaseOwner, now, expires)
			leaseCalls = append(leaseCalls, leaseCall{verifyForwardEgressLeaseRedisScript, keys, args})
		}
	}
	leaseCmds, _ := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, call := range leaseCalls {
			call.script.EvalSha(ctx, pipe, call.keys, call.args...)
		}
		return nil
	})
	waveCount += 1
	uncachedIndexes := []int{}
	for index, cmd := range leaseCmds {
		if err := cmd.Err(); err != nil {
			if !redis.HasErrorPrefix(err, "NOSCRIPT") {
				server.Raise(err)
			}
			uncachedIndexes = append(uncachedIndexes, index)
		}
	}
	if 0 < len(uncachedIndexes) {
		uncachedCmds, _ := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for _, index := range uncachedIndexes {
				call := leaseCalls[index]
				call.script.Eval(ctx, pipe, call.keys, call.args...)
			}
			return nil
		})
		waveCount += 1
		raiseErrors(uncachedCmds)
	}

	// each client's live addresses, in both reverse index versions
	reverseCmds := make([][]*redis.MapStringStringCmd, len(proxyEgresses))
	cmds, _ := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, egress := range proxyEgresses {
			for _, reverseKey := range verifyClientEgressKeys(egress.clientId) {
				reverseCmds[index] = append(reverseCmds[index], pipe.HGetAll(ctx, reverseKey))
			}
		}
		return nil
	})
	waveCount += 1
	raiseErrors(cmds)

	// the forward entries each client's eligibility reads, and the
	// opportunistic prune of expired v4 reverse fields
	lives := make([]verifyLiveEgress, len(proxyEgresses))
	forwardCmds := make([]map[string]*redis.StringCmd, len(proxyEgresses))
	cmds, _ = r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for index, egress := range proxyEgresses {
			live, expired := splitVerifyEgressFields(reverseCmds[index][0].Val(), nowMs)
			if 0 < len(expired) {
				queueVerifyExpiredEgressHashPrune(ctx, pipe, egress.clientId, expired)
			}
			legacyLive, _ := splitVerifyEgressFields(reverseCmds[index][1].Val(), nowMs)
			lives[index] = newVerifyLiveEgress(live, legacyLive)
			forwardCmds[index] = map[string]*redis.StringCmd{}
			for _, egressHashHex := range lives[index].eligibilityForwardHashes() {
				forwardCmds[index][egressHashHex] = pipe.Get(ctx, verifyEgressKeyFromHex(egressHashHex))
			}
		}
		return nil
	})
	if 0 < len(cmds) {
		waveCount += 1
		raiseErrors(cmds)
	}

	// membership (§5.1): a family that attributes to the client, or its one v2
	// entry, and a provide key
	eligibleMembers := []any{}
	ineligibleMembers := []any{}
	for index, egress := range proxyEgresses {
		member := egress.clientId.String()
		forwards := map[string]string{}
		for egressHashHex, cmd := range forwardCmds[index] {
			forwards[egressHashHex] = cmd.Val()
		}
		if lives[index].attributes(egress.clientId, forwards) && egress.hasProvideKey {
			eligibleMembers = append(eligibleMembers, member)
		} else {
			ineligibleMembers = append(ineligibleMembers, member)
		}
	}
	cmds, _ = r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		if 0 < len(eligibleMembers) {
			pipe.SAdd(ctx, verifyEligibleKey, eligibleMembers...)
			// hygiene bound only; see updateVerifyEligibleMembership
			pipe.Expire(ctx, verifyEligibleKey, verifyEligibleKeyTtl)
		}
		if 0 < len(ineligibleMembers) {
			pipe.SRem(ctx, verifyEligibleKey, ineligibleMembers...)
		}
		return nil
	})
	waveCount += 1
	raiseErrors(cmds)
	return
}
