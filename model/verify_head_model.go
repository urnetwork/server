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
	"net/netip"
	"slices"
	"strings"
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
				versions := []map[string]string{}
				for _, cmd := range clientCmds {
					fields, _ := cmd.Result()
					live, _ := splitVerifyEgressFields(fields, nowMs)
					versions = append(versions, live)
				}
				live := newVerifyLiveEgress(versions[0], versions[1])
				// each list is sorted: its first is the least
				for _, egresses := range [][]string{live.networks(4), live.networks(6), live.legacy} {
					if 0 < len(egresses) {
						hashes[chunk[i]] = []string{egresses[0]}
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

// A settlement epoch's length when the st mirror does not carry it: sn
// deploy/mainnet/policy-v1.yml settles every 50400 subtensor blocks of 12
// seconds, seven days.
const (
	verifyHeadHashBlockTime   = 12 * time.Second
	verifyHeadHashEpochBlocks = 50400
)

// A pin outlives its window by this much, past every hop of a trail seeded in
// the window and a stalled mirror's last epoch.
const verifyHeadHashGrace = 24 * time.Hour

func verifyHeadHashKey(clientId server.Id, window string) string {
	return fmt.Sprintf("{vhead_%s}%s", clientId, window)
}

func verifyTrailHeadWindowKey(trailId server.Id) string {
	return fmt.Sprintf("{vtr_%s}window", trailId)
}

// The scoring window in effect now: the settlement epoch the validators score
// head prefixes by, from the st mirror, else a UTC hour.
func verifyHeadHashWindow(ctx context.Context, settings *VerifySettings) string {
	if epoch, ok := verifyCohortEpoch(ctx, settings.CohortDeploymentKey); ok {
		return fmt.Sprintf("e%d", epoch)
	}
	return fmt.Sprintf("h%d", server.NowUtc().Unix()/int64(verifyHeadHashFallbackWindow/time.Second))
}

// Select the seed's window before its original is retained. Publication and
// recovery must reuse this value instead of consulting a later epoch mirror.
func SelectVerifyTrailHeadWindow(ctx context.Context, settings *VerifySettings) string {
	return verifyHeadHashWindow(ctx, settings)
}

// How long a pin of the window lives: the window, an epoch of the mirror's
// TEpochBlocks subtensor blocks (a mainnet epoch when it does not say), or an
// hour, and the grace.
func verifyHeadHashLifetime(ctx context.Context, window string, settings *VerifySettings) time.Duration {
	if !strings.HasPrefix(window, "e") {
		return verifyHeadHashFallbackWindow + verifyHeadHashGrace
	}
	blocks := uint64(verifyHeadHashEpochBlocks)
	if settings.CohortDeploymentKey != "" {
		if summary := GetStEpochSummaryCache(ctx, settings.CohortDeploymentKey); summary != nil && 0 < summary.TEpochBlocks {
			blocks = summary.TEpochBlocks
		}
	}
	return time.Duration(blocks)*verifyHeadHashBlockTime + verifyHeadHashGrace
}

// SetVerifyTrailHeadWindow keeps the scoring window in effect at a trail's
// SEED for as long as the trail can live, so every hop of the trail pins in
// one window however long it takes (§8.1).
func SetVerifyTrailHeadWindow(ctx context.Context, trailId server.Id, m int, settings *VerifySettings) {
	window := verifyHeadHashWindow(ctx, settings)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, verifyTrailHeadWindowKey(trailId), window, settings.TrailTtl(m)).Err())
	})
}

// GetVerifyTrailHeadWindow is the scoring window the trail was seeded in.
// Historical originals that retained none keep the current-window fallback;
// new originals restore their retained window before an extension.
func GetVerifyTrailHeadWindow(ctx context.Context, trailId server.Id, settings *VerifySettings) (window string) {
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		window, err = r.Get(ctx, verifyTrailHeadWindowKey(trailId)).Result()
		if errors.Is(err, redis.Nil) {
			window, err = "", nil
		}
		server.Raise(err)
	})
	if window == "" {
		window = verifyHeadHashWindow(ctx, settings)
	}
	return
}

// The hash a provider's first hop of a window pins: the egress-IP-hash its
// feeder stored for its IPv4 address while that family attributes to it,
// else for its IPv6 address, the least when a /64 holds several, else the
// hop's own source's. It is a rule of the provider's index, not of the family
// a hop landed on, so no caller steers which prefix the provider scores.
func verifyHeadHashCandidate(ctx context.Context, r server.RedisClient, clientId server.Id, sourceIp netip.Addr, settings *VerifySettings, nowMs uint64) [32]byte {
	live := readVerifyLiveEgress(ctx, r, clientId, nowMs)
	for _, family := range []int{4, 6} {
		if !verifyFamilyAttributes(ctx, r, clientId, live, family) {
			continue
		}
		scores := []string{}
		for _, address := range live.byFamily[family] {
			if address.score != "" {
				scores = append(scores, address.score)
			}
		}
		if 0 < len(scores) {
			return decodeVerifyHeadHash(slices.Min(scores), "stored egress-IP-hash")
		}
	}
	return VerifyEgressIpHashWithSettings(sourceIp, settings)
}

// A head hash is 32 bytes as hex; anything else is refused, like every
// malformed verify state read.
func decodeVerifyHeadHash(hashHex string, what string) [32]byte {
	decoded, err := hex.DecodeString(hashHex)
	if err == nil && len(decoded) != 32 {
		err = fmt.Errorf("verify %s %q holds %d bytes", what, hashHex, len(decoded))
	}
	server.Raise(err)
	return [32]byte(decoded)
}

// PinVerifyHopEgressIpHash returns the egress-IP-hash a hop the provider
// confirms from sourceIp records (§8.1, D27): the provider's pin for the
// trail's scoring window. The validators union a fleet's hop hashes per
// window, so a provider whose families come and go (an IPv4 transport that
// reconnects or lapses, overlapping leases) still scores one prefix a window.
// The first hop of a window pins the provider's candidate
// (verifyHeadHashCandidate). A poison trail's hop reads the pin, or the
// candidate, but never sets it, with the same Redis work as a real one's.
func PinVerifyHopEgressIpHash(ctx context.Context, clientId server.Id, sourceIp netip.Addr, window string, poison bool, settings *VerifySettings) (pinned [32]byte) {
	key := verifyHeadHashKey(clientId, window)
	lifetime := verifyHeadHashLifetime(ctx, window, settings)
	setPin := "1"
	if poison {
		setPin = "0"
	}
	nowMs := uint64(server.NowUtc().UnixMilli())
	server.Redis(ctx, func(r server.RedisClient) {
		candidate := verifyHeadHashCandidate(ctx, r, clientId, sourceIp, settings, nowMs)
		pinnedHex, err := r.Eval(ctx, `
local pinned = redis.call('GET', KEYS[1])
if pinned then return pinned end
if ARGV[3] == '1' then redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2]) end
return ARGV[1]`, []string{key}, hex.EncodeToString(candidate[:]), lifetime.Milliseconds(), setPin).Text()
		server.Raise(err)
		pinned = decodeVerifyHeadHash(pinnedHex, "head hash "+key)
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
