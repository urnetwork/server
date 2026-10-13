// verify_head_model exposes the live egress index (verify_model.go) in bulk
// for the server-side head-tier estimate (`GET /sn/head`, WHITEPAPER §8.4):
// which providers are eligible, which egress-IP hash each one currently
// scores by, and which client currently owns each hash. It also pins the
// egress-IP-hash a provider's confirmed hops record in one scoring window.
package model

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

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

// GetVerifyLiveEgressHashes returns the live (unexpired) egress hash, as hex,
// that each client scores by: one per provider (§8.1), its IPv4 address while
// it has one, else its IPv6 /64, else a v2 entry, the least hash of the
// family when there are several. Both reverse index versions are read.
// Expired entries are skipped, not deleted; the feeder path owns cleanup.
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
				versions := [][]string{}
				for _, cmd := range clientCmds {
					fields, _ := cmd.Result()
					live, _ := splitVerifyEgressFields(fields, nowMs)
					versions = append(versions, live)
				}
				live := newVerifyLiveEgress(versions[0], versions[1])
				for _, addresses := range [][]string{live.byFamily[4], live.byFamily[6], live.legacy} {
					if 0 < len(addresses) {
						hashes[chunk[i]] = []string{slices.Min(addresses)}
						break
					}
				}
			}
		}
	})
	return hashes
}

// The scoring window of a provider's head hash when the settlement epoch is
// unknown: no st deployment, or a stalled mirror.
const verifyHeadHashFallbackWindow = time.Hour

// Outlives any scoring window; each window has a key of its own.
const verifyHeadHashTtl = 24 * time.Hour

func verifyHeadHashKey(clientId server.Id, window string) string {
	return fmt.Sprintf("{vhead_%s}%s", clientId, window)
}

// The scoring window: the settlement epoch the validators score head prefixes
// by, from the st mirror, else a UTC hour.
func verifyHeadHashWindow(ctx context.Context, settings *VerifySettings) string {
	if epoch, ok := verifyCohortEpoch(ctx, settings.CohortDeploymentKey); ok {
		return fmt.Sprintf("e%d", epoch)
	}
	return fmt.Sprintf("h%d", server.NowUtc().Unix()/int64(verifyHeadHashFallbackWindow/time.Second))
}

// PinVerifyHopEgressIpHash returns the egress-IP-hash a confirmed hop of the
// provider records (§8.1, D27): that of the first hop it confirmed in the
// current scoring window. The validators union a fleet's hop hashes per
// window, so a provider whose families come and go (an IPv4 transport that
// reconnects or lapses, overlapping leases) still scores one prefix a window.
func PinVerifyHopEgressIpHash(ctx context.Context, clientId server.Id, egressIpHash [32]byte, settings *VerifySettings) (pinned [32]byte) {
	key := verifyHeadHashKey(clientId, verifyHeadHashWindow(ctx, settings))
	server.Redis(ctx, func(r server.RedisClient) {
		pinnedHex, err := r.Eval(ctx, `
local pinned = redis.call('GET', KEYS[1])
if pinned then return pinned end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return ARGV[1]`, []string{key}, hex.EncodeToString(egressIpHash[:]), verifyHeadHashTtl.Milliseconds()).Text()
		server.Raise(err)
		decoded, err := hex.DecodeString(pinnedHex)
		if err == nil && len(decoded) != len(pinned) {
			err = fmt.Errorf("verify head hash %s holds %d bytes", key, len(decoded))
		}
		server.Raise(err)
		pinned = [32]byte(decoded)
	})
	return
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
