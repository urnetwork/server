// verify_head_model exposes the live egress index (verify_model.go) in bulk
// for the server-side head-tier estimate (`GET /sn/head`, WHITEPAPER §8.4):
// which providers are eligible, which egress-IP hashes each one currently
// backs, and which client currently owns each hash. Everything is a read of
// the same Redis structures FeedVerifyEgress maintains.
package model

import (
	"context"
	"errors"
	"slices"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// VerifyEgressAmbiguousOwner is the forward-entry marker for an egress hash
// currently backed by more than one client.
const VerifyEgressAmbiguousOwner = verifyEgressAmbiguous

// GetVerifyEligibleClientIds returns the eligible-provider set (§5.1).
func GetVerifyEligibleClientIds(ctx context.Context) []server.Id {
	clientIds := []server.Id{}
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.SMembers(ctx, verifyEligibleKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for _, member := range members {
			if clientId, parseErr := server.ParseId(member); parseErr == nil {
				clientIds = append(clientIds, clientId)
			}
		}
	})
	return clientIds
}

// GetVerifyLiveEgressHashes returns the live (unexpired) egress hashes, as
// hex, that each client scores by: its IPv4 addresses while it has any, else
// its others, so a provider with an address in each family counts once
// (§8.1). Both reverse index versions are read. Expired entries are skipped,
// not deleted; the feeder path owns cleanup.
func GetVerifyLiveEgressHashes(ctx context.Context, clientIds []server.Id) map[server.Id][]string {
	hashes := map[server.Id][]string{}
	if len(clientIds) == 0 {
		return hashes
	}
	nowMs := uint64(server.NowUtc().UnixMilli())
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(clientIds); start += 512 {
			end := min(start+512, len(clientIds))
			chunk := clientIds[start:end]
			cmds := make([][]*redis.MapStringStringCmd, len(chunk))
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, clientId := range chunk {
					for _, reverseKey := range verifyClientEgressKeys(clientId) {
						cmds[i] = append(cmds[i], pipe.HGetAll(ctx, reverseKey))
					}
				}
				return nil
			})
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			for i, clientCmds := range cmds {
				entries := map[string]verifyEgressEntry{}
				for _, cmd := range clientCmds {
					if fields, cmdErr := cmd.Result(); cmdErr == nil {
						addVerifyEgressEntries(entries, fields, nowMs)
					}
				}
				scored := []string{}
				for egressHashHex, entry := range entries {
					if entry.family == 4 {
						scored = append(scored, egressHashHex)
					}
				}
				if len(scored) == 0 {
					for egressHashHex := range entries {
						scored = append(scored, egressHashHex)
					}
				}
				if 0 < len(scored) {
					slices.Sort(scored)
					hashes[chunk[i]] = scored
				}
			}
		}
	})
	return hashes
}

// GetVerifyEgressOwners resolves forward entries in bulk: egress hash hex ->
// owning client id string, or VerifyEgressAmbiguousOwner. Hashes without a
// live forward entry are absent.
func GetVerifyEgressOwners(ctx context.Context, egressHashesHex []string) map[string]string {
	owners := map[string]string{}
	if len(egressHashesHex) == 0 {
		return owners
	}
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(egressHashesHex); start += 512 {
			end := min(start+512, len(egressHashesHex))
			chunk := egressHashesHex[start:end]
			cmds := make([]*redis.StringCmd, len(chunk))
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				for i, egressHashHex := range chunk {
					cmds[i] = pipe.Get(ctx, verifyEgressKeyFromHex(egressHashHex))
				}
				return nil
			})
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			for i, cmd := range cmds {
				owner, cmdErr := cmd.Result()
				if cmdErr != nil {
					continue
				}
				owners[chunk[i]] = owner
			}
		}
	})
	return owners
}
