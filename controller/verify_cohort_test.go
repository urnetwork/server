package controller

// The verify.yml cohort keys, the cohort epoch taken from the st sync mirror,
// and the controller's refusal to name a synthetic next hop once sampling is
// bounded by a cohort.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestVerifyCohortSettings(t *testing.T) {
	value := func(v int) *int { return &v }
	for _, test := range []struct {
		name        string
		profile     string
		size, limit *int
		wantSize    int
		wantLimit   int
		wantErr     bool
	}{
		{name: "mainnet defaults", profile: "mainnet", wantSize: 1500, wantLimit: 2000},
		{name: "testnet unchanged", profile: "testnet", wantSize: 0, wantLimit: 0},
		{name: "testnet opt-in", profile: "testnet", size: value(50), wantSize: 50, wantLimit: 2000},
		{name: "explicit", profile: "mainnet", size: value(800), limit: value(900), wantSize: 800, wantLimit: 900},
		{name: "size above default limit", profile: "mainnet", size: value(3000), wantSize: 3000, wantLimit: 3000},
		{name: "limit below default size", profile: "mainnet", limit: value(1000), wantSize: 1000, wantLimit: 1000},
		{name: "limit above default size", profile: "testnet", limit: value(2500), wantSize: 1500, wantLimit: 2500},
		{name: "mainnet zero refused", profile: "mainnet", size: value(0), wantErr: true},
		{name: "testnet zero refused", profile: "testnet", size: value(0), wantErr: true},
		{name: "negative refused", profile: "mainnet", size: value(-1), limit: value(10), wantErr: true},
		{name: "limit below size refused", profile: "mainnet", size: value(100), limit: value(50), wantErr: true},
		{name: "zero limit refused", profile: "mainnet", limit: value(0), wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			size, limit, err := verifyCohortSettings(test.profile, test.size, test.limit)
			if test.wantErr {
				if err == nil {
					t.Fatalf("accepted %d/%d", size, limit)
				}
				return
			}
			if err != nil || size != test.wantSize || limit != test.wantLimit {
				t.Fatalf("got %d/%d %v, want %d/%d", size, limit, err, test.wantSize, test.wantLimit)
			}
		})
	}
}

// The loader applies the cohort keys and names the st deployment whose epoch
// mirror the cohort reads. The mainnet verify.yml generated from the policy
// has no cohort keys; it must still bound sampling.
func TestParseVerifySettingsCohort(t *testing.T) {
	policyHash := [32]byte{1, 2, 3}
	coordinator := common.HexToAddress("0xC18925925E2B7bb9059b7d696b8c92762AE86406")
	egressKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	document := func(profile string, cohort string) []byte {
		return []byte(fmt.Sprintf(`profile: %s
policy_hash: "0x%x"
egress_hash_key: %s
settings:
  trail_depth: 8
  step_timeout_seconds: 30
  step_timeout_grace_seconds: 5
  trail_ttl_grace_seconds: 60
  egress_ttl_seconds: 600
  egress_refresh_seconds: 120
  reliability_a_min: 8
  stats_period_seconds: 900
  egress_ipv4_prefix: 29
  egress_ipv6_prefix: 48
  egress_hash_key_id: test-v1
  soft_guardrails_enabled: false
  hard_seed_per_minute_per_source: 40
  hard_extend_per_minute_per_source: 240
  hard_active_trails_per_source: 32
%s`, profile, policyHash, egressKey, cohort))
	}
	parse := func(t *testing.T, profile string, cohort string) (settings *model.VerifySettings, err error) {
		SetStConfig(&StConfig{Profile: profile, PolicyHash: policyHash, ChainId: 964, ContractAddress: coordinator})
		defer SetStConfig(nil)
		pop := server.Vault.PushSimpleResource("verify-cohort-test.yml", document(profile, cohort))
		defer pop()
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("%v", recovered)
			}
		}()
		return parseVerifySettings(server.Vault.RequireSimpleResource("verify-cohort-test.yml")), nil
	}
	deployment := model.StDeploymentKey("964:" + strings.ToLower(coordinator.Hex()))

	settings, err := parse(t, "mainnet", "")
	if err != nil || settings.CohortSize != 1500 || settings.CohortLifetimeLimit != 2000 || settings.CohortDeploymentKey != deployment {
		t.Fatalf("mainnet defaults: %+v %v", settings, err)
	}
	settings, err = parse(t, "mainnet", "  cohort_size: 1200\n  cohort_lifetime_limit: 1600\n")
	if err != nil || settings.CohortSize != 1200 || settings.CohortLifetimeLimit != 1600 {
		t.Fatalf("mainnet explicit: %+v %v", settings, err)
	}
	settings, err = parse(t, "testnet", "")
	if err != nil || settings.CohortSize != 0 || settings.CohortLifetimeLimit != 0 {
		t.Fatalf("testnet default must keep uniform sampling: %+v %v", settings, err)
	}
	settings, err = parse(t, "testnet", "  cohort_size: 40\n")
	if err != nil || settings.CohortSize != 40 || settings.CohortLifetimeLimit != 2000 {
		t.Fatalf("testnet opt-in: %+v %v", settings, err)
	}
	if _, err := parse(t, "mainnet", "  cohort_size: 0\n"); err == nil || !strings.Contains(err.Error(), "cohort_size") {
		t.Fatalf("mainnet zero cohort accepted: %v", err)
	}
	if _, err := parse(t, "mainnet", "  cohort_size: 100\n  cohort_lifetime_limit: 99\n"); err == nil {
		t.Fatal("lifetime limit below the cohort size accepted")
	}
}

// The cohort epoch is the coordinator epoch the st sync task mirrors, so the
// cohort turns over exactly at the coordinator boundary: block 9,247,227 is
// the last block of the launch's epoch 0 and 9,247,228 starts epoch 1.
func TestVerifyCohortFollowsStSyncEpoch(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cfg := &StConfig{Profile: "mainnet", Enabled: true, ChainId: 964, Netuid: 25, NoId: 1, DeploymentId: "sn25-cohort-epoch",
			ContractAddress: common.HexToAddress("0xC18925925E2B7bb9059b7d696b8c92762AE86406"), BlockSeconds: 12}
		oldCfg, oldClient := stConfigInstance, stClientInstance
		t.Cleanup(func() { stConfigInstance, stClientInstance = oldCfg, oldClient })
		SetStConfig(cfg)
		client := newStubStClient(&StEpochState{Epoch: 0, PendingEpoch: 0, EpochStartBlock: 9_240_028, TEpochBlocks: 7_200, HeadBlock: 9_247_227, HeadBlockTime: server.NowUtc()})
		SetStClient(client)

		settings := model.DefaultVerifySettings()
		settings.CohortSize, settings.CohortLifetimeLimit = 2, 4
		settings.CohortDeploymentKey = cfg.DeploymentKey()
		for i := 0; i < 4; i += 1 {
			testVerifyProvider(ctx, netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", 10+10*i)), settings)
		}
		cohort := func(epoch uint64) (members []string) {
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				members, err = r.SMembers(ctx, fmt.Sprintf("{verify_cohort:%s}e%d", cfg.DeploymentKey(), epoch)).Result()
				server.Raise(err)
			})
			return
		}

		// no mirror yet: nothing is sampled
		if nextHop, _ := model.SampleVerifyNextHop(ctx, nil, settings); nextHop != nil {
			t.Fatalf("sampled %s before the epoch was mirrored", nextHop)
		}
		if _, err := StSyncChainState(ctx); err != nil {
			t.Fatal(err)
		}
		first, _ := model.SampleVerifyNextHop(ctx, nil, settings)
		if first == nil || !slices.Equal(cohort(0), []string{first.String()}) {
			t.Fatalf("epoch 0 hop %v cohort %v", first, cohort(0))
		}

		client.state = &StEpochState{Epoch: 1, PendingEpoch: 1, EpochStartBlock: 9_247_228, TEpochBlocks: 50_400, HeadBlock: 9_247_228, HeadBlockTime: server.NowUtc()}
		if _, err := StSyncChainState(ctx); err != nil {
			t.Fatal(err)
		}
		// the new epoch's cohort starts from the carried member
		second, _ := model.SampleVerifyNextHop(ctx, nil, settings)
		if second == nil || *second != *first || !slices.Equal(cohort(1), []string{first.String()}) {
			t.Fatalf("epoch 1 hop %v cohort %v, want carried %s", second, cohort(1), first)
		}
	})
}

// With a cohort, no ASSIGN ever names a provider outside it: an unknown epoch
// refuses the seed, and a trail that exhausts the cohort fails instead of
// receiving a synthetic hop (which a validator would record as a fresh
// provider). Real and poison trails get the same responses.
func TestVerifyCohortNeverAssignsOutsideCohort(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		settings.CohortSize = 3
		settings.CohortLifetimeLimit = 3
		settings.CohortDeploymentKey = "964:0xc18925925e2b7bb9059b7d696b8c92762ae86406"
		SetVerifySettings(settings)
		defer SetVerifySettings(model.DefaultVerifySettings())

		providerIps := map[server.Id]string{}
		var seedProvider server.Id
		for i := 0; i < 6; i += 1 {
			ip := fmt.Sprintf("203.0.113.%d", 10+10*i)
			provider := testVerifyProvider(ctx, netip.MustParseAddr(ip), settings)
			providerIps[provider] = ip
			if i == 0 {
				seedProvider = provider
			}
		}
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)
		poisonValidatorId, poisonVpk, poisonKey := testVerifyValidator(ctx)
		unresolvedIp := "198.18.0.9" // never fed to the egress index: poison
		activeTrails := func(vpk ed25519.PublicKey) (count int) {
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				count, err = r.Get(ctx, "verify_trails_"+hex.EncodeToString(vpk)).Int()
				if errors.Is(err, redis.Nil) {
					count, err = 0, nil
				}
				server.Raise(err)
			})
			return
		}

		// unknown settlement epoch: both seeds are refused and release their slot
		for _, seed := range []struct {
			args *VerifyArgs
			ip   string
			vpk  ed25519.PublicKey
		}{
			{testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), providerIps[seedProvider], vpk},
			{testVerifySeedArgs(t, poisonValidatorId, poisonVpk, poisonKey, connect.VerifyMMin), unresolvedIp, poisonVpk},
		} {
			result, err := Verify(seed.args, testVerifySession(ctx, seed.ip))
			if err == nil || err.Error() != "503 verify next hop unavailable" {
				t.Fatalf("seed without an epoch = %T %v", result, err)
			}
			if count := activeTrails(seed.vpk); count != 0 {
				t.Fatalf("refused seed kept %d active trails", count)
			}
		}

		model.SetStEpochSummaryCache(ctx, settings.CohortDeploymentKey, &model.StEpochSummary{Epoch: 4}, time.Hour)
		cohortKey := fmt.Sprintf("{verify_cohort:%s}e4", settings.CohortDeploymentKey)
		inCohort := func(id connect.Id) bool {
			var member bool
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				member, err = r.SIsMember(ctx, cohortKey, server.Id(id).String()).Result()
				server.Raise(err)
			})
			return member
		}
		// walk follows every assignment from the assigned provider's egress
		// and returns the number of hops assigned before the trail ended
		walk := func(args *VerifyArgs, seedIp string, validator server.Id, vpk ed25519.PublicKey, key ed25519.PrivateKey) (assigned int, final bool, err error) {
			result, err := Verify(args, testVerifySession(ctx, seedIp))
			for err == nil {
				assign, ok := result.(*connect.VerifyAssignResult)
				if !ok {
					return assigned, true, nil
				}
				assigned += 1
				if !inCohort(assign.NextHop) {
					t.Fatalf("assigned %s outside the cohort", server.Id(assign.NextHop))
				}
				result, err = Verify(testVerifyExtendArgs(t, validator, vpk, key, assign), testVerifySession(ctx, providerIps[server.Id(assign.NextHop)]))
			}
			return assigned, false, err
		}

		// a trail within the cohort completes
		if assigned, final, err := walk(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), providerIps[seedProvider], validatorId, vpk, vpkKey); err != nil || !final || assigned != connect.VerifyMMin-1 {
			t.Fatalf("trail at M=%d: assigned %d final %t err %v", connect.VerifyMMin, assigned, final, err)
		}
		// deeper real and poison trails exhaust the three members and fail
		// with one identical error, never receiving a synthetic hop
		const m = connect.VerifyMMin + 1
		realAssigned, realFinal, realErr := walk(testVerifySeedArgs(t, validatorId, vpk, vpkKey, m), providerIps[seedProvider], validatorId, vpk, vpkKey)
		poisonAssigned, poisonFinal, poisonErr := walk(testVerifySeedArgs(t, poisonValidatorId, poisonVpk, poisonKey, m), unresolvedIp, poisonValidatorId, poisonVpk, poisonKey)
		if realFinal || poisonFinal || realAssigned != 3 || poisonAssigned != 3 || realErr == nil || poisonErr == nil || realErr.Error() != poisonErr.Error() {
			t.Fatalf("exhausted trails: real %d %t %v, poison %d %t %v", realAssigned, realFinal, realErr, poisonAssigned, poisonFinal, poisonErr)
		}
		if count := activeTrails(vpk) + activeTrails(poisonVpk); count != 0 {
			t.Fatalf("failed trails kept %d active trails", count)
		}
		var size int64
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			size, err = r.SCard(ctx, cohortKey).Result()
			server.Raise(err)
		})
		if size != 3 {
			t.Fatalf("cohort size %d", size)
		}
	})
}
