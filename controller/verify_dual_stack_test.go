// Providers with an address in each family (VALIDATOR.md §8.2, IPV6.md C7):
// hops confirm from either family, a provider scores one prefix a window, the
// per-source budgets stay one per provider, and a source the index cannot
// attribute is answered as retryable before it fails the step.
package controller

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The answer to a well-formed request whose source resolves to no provider.
const testVerifyRetryableAnswer = "421 source-egress-unresolved"

// Registers an eligible provider holding an IPv4 and an IPv6 address, the
// second as a family-pinned transport's connection lease (IPV6.md A).
func testVerifyDualStackProvider(ctx context.Context, v4 netip.Addr, v6 netip.Addr, settings *model.VerifySettings) server.Id {
	clientId := testVerifyProvider(ctx, v4, settings)
	model.FeedVerifyConnectionEgress(ctx, clientId, server.NewId(), v6, settings)
	return clientId
}

// A request session from ip, bracketed when IPv6.
func testVerifyAddrSession(ctx context.Context, ip netip.Addr) *session.ClientSession {
	return session.NewLocalClientSession(ctx, netip.AddrPortFrom(ip, 40000).String(), nil)
}

// Requires the trail to be active, unchanged at one confirmed hop with the
// given pending hop.
func requireVerifyTrailAtSeed(t testing.TB, ctx context.Context, trailId server.Id, pending server.Id) {
	t.Helper()
	trail := model.GetVerifyTrail(ctx, trailId)
	if trail == nil || trail.Status != model.VerifyTrailStatusActive || len(trail.Hops) != 1 || trail.Pending == nil || trail.Pending.ClientId != pending {
		t.Fatalf("trail after a retryable answer = %+v, want it active and unchanged", trail)
	}
}

// §8.1, D27, IPV6.md C7: a provider with an address in each family confirms
// its hop from either, and every hop it serves in a scoring window records
// the egress-IP-hash of its first confirmed one, so it scores one prefix,
// not two. Providers with one family record their own; the seed hop, which
// never scores, records its source's.
func TestVerifyDualStackHopsRecordOnePrefixPerProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		// a fixed settlement epoch is the scoring window
		settings.CohortDeploymentKey = model.StDeploymentKey("964:0xdual-stack-head-hash")
		model.SetStEpochSummaryCache(ctx, settings.CohortDeploymentKey, &model.StEpochSummary{Epoch: 11}, time.Hour)
		SetVerifySettings(settings)

		seedIp := netip.MustParseAddr("203.0.113.60")
		seedProvider := testVerifyProvider(ctx, seedIp, settings)
		type addresses struct{ v4, v6 netip.Addr }
		providers := map[server.Id]addresses{}
		dual := addresses{netip.MustParseAddr("203.0.113.70"), netip.MustParseAddr("2001:db8:70::1")}
		dualId := testVerifyDualStackProvider(ctx, dual.v4, dual.v6, settings)
		providers[dualId] = dual
		v4Only := addresses{v4: netip.MustParseAddr("203.0.113.80")}
		providers[testVerifyProvider(ctx, v4Only.v4, settings)] = v4Only
		v6Only := addresses{v6: netip.MustParseAddr("2001:db8:90::1")}
		providers[testVerifyProvider(ctx, v6Only.v6, settings)] = v6Only

		// M = 4: every trail assigns all three providers. The dual-stack one
		// confirms from IPv6 in the first trail and from IPv4 in the second.
		for trailIndex, dualSource := range []netip.Addr{dual.v6, dual.v4} {
			validatorId, vpk, vpkKey := testVerifyValidator(ctx)
			result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, seedIp))
			if err != nil {
				t.Fatal(err)
			}
			assign := result.(*connect.VerifyAssignResult)
			var final *connect.VerifyFinalResult
			for step := 0; final == nil; step++ {
				if connect.VerifyMMin < step {
					t.Fatal("trail did not finalize within M steps")
				}
				provider, ok := providers[server.Id(assign.NextHop)]
				if !ok {
					t.Fatalf("assigned hop %s is not a registered provider", server.Id(assign.NextHop))
				}
				source := provider.v4
				if server.Id(assign.NextHop) == dualId {
					source = dualSource
				} else if !source.IsValid() {
					source = provider.v6
				}
				result, err := Verify(testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign), testVerifyAddrSession(ctx, source))
				if err != nil {
					t.Fatalf("trail %d EXTEND from %s: %v", trailIndex, source, err)
				}
				if next, ok := result.(*connect.VerifyAssignResult); ok {
					assign = next
				} else if final, ok = result.(*connect.VerifyFinalResult); !ok {
					t.Fatalf("extend result = %T", result)
				}
			}
			for i, hop := range final.Proof.Hops {
				var want netip.Addr
				switch clientId := server.Id(hop.ClientId); {
				case clientId == seedProvider:
					want = seedIp
				case clientId == dualId:
					want = dual.v6
				case providers[clientId].v4.IsValid():
					want = providers[clientId].v4
				default:
					want = providers[clientId].v6
				}
				if hop.EgressIpHash != model.VerifyEgressIpHashWithSettings(want, settings) {
					t.Fatalf("trail %d proof hop %d egress hash is not %s's prefix", trailIndex, i, want)
				}
			}
		}
	})
}

// §9: a provider's per-source budgets do not double with a second family.
// Seeds and EXTENDs from either of its addresses count against one hard
// limit, so the request past it is refused while each address is still under
// its own. The active-trail cap counts per vpk, whatever the seed's source.
func TestVerifyDualStackProviderKeepsOneBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		// one fixed rate window for the whole test
		settings.SeedRateWindow = 1000 * time.Hour
		settings.SeedRateHardLimit = 2
		settings.ExtendRateHardLimit = 2
		SetVerifySettings(settings)
		defer SetVerifySettings(model.DefaultVerifySettings())
		v4, v6 := netip.MustParseAddr("203.0.113.70"), netip.MustParseAddr("2001:db8:70::1")
		testVerifyDualStackProvider(ctx, v4, v6, settings)
		testVerifyProvider(ctx, netip.MustParseAddr("203.0.113.80"), settings)

		// a fresh validator per seed, so no vpk counter reaches the limit
		seed := func(source netip.Addr) error {
			validatorId, vpk, vpkKey := testVerifyValidator(ctx)
			_, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, source))
			return err
		}
		for _, source := range []netip.Addr{v4, v6} {
			if err := seed(source); err != nil {
				t.Fatalf("seed from %s: %v", source, err)
			}
		}
		if err := seed(v4); err == nil || err.Error() != "429 rate limit exceeded" {
			t.Fatalf("third seed through one provider = %v, want its one budget refused", err)
		}

		// metered before the trail loads, so an unknown trail still counts
		extend := func(source netip.Addr) error {
			trailId := server.NewId()
			_, err := Verify(&VerifyArgs{
				ClientId:  server.NewId(),
				TrailId:   &trailId,
				Trail:     []server.Id{server.NewId()},
				ExtendSig: make([]byte, ed25519.SignatureSize),
			}, testVerifyAddrSession(ctx, source))
			return err
		}
		for _, source := range []netip.Addr{v4, v6} {
			if err := extend(source); err == nil || err.Error() != "400 trail not found" {
				t.Fatalf("EXTEND from %s = %v, want the unknown trail", source, err)
			}
		}
		if err := extend(v6); err == nil || err.Error() != "429 rate limit exceeded" {
			t.Fatalf("third EXTEND through one provider = %v, want its one budget refused", err)
		}

		// only the active-trail cap can refuse here; distinct M values keep
		// the second seed from replaying the first
		settings.SeedRateHardLimit = 40
		settings.ActiveTrailsHardLimit = 1
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)
		if _, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, v4)); err != nil {
			t.Fatalf("first active trail: %v", err)
		}
		if _, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin+1), testVerifyAddrSession(ctx, v6)); err == nil || err.Error() != "429 rate limit exceeded" {
			t.Fatalf("second active trail from the other family = %v, want the vpk's cap", err)
		}
	})
}

// §4.4, §8.2, IPV6.md C7: a provider indexed on one family only (a legacy
// single-transport provider on a dual-stack host) is reached on its other
// family by an IPv6-first request. The EXTEND is answered as retryable with
// the trail unchanged, and the retry of the identical body that lands on the
// indexed address confirms the hop.
func TestVerifyExtendOnUnindexedFamilyIsRetried(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		SetVerifySettings(settings)
		seedIp := netip.MustParseAddr("192.0.2.90")
		testVerifyProvider(ctx, seedIp, settings)
		indexedV4, unindexedV6 := netip.MustParseAddr("203.0.113.91"), netip.MustParseAddr("2001:db8:91::1")
		pending := testVerifyProvider(ctx, indexedV4, settings)
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)
		result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, seedIp))
		if err != nil {
			t.Fatal(err)
		}
		assign := result.(*connect.VerifyAssignResult)
		if server.Id(assign.NextHop) != pending {
			t.Fatalf("assigned %s, want the only other provider", server.Id(assign.NextHop))
		}
		extend := testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign)
		// the next assignment after the confirmed hop
		testVerifyProvider(ctx, netip.MustParseAddr("192.0.2.92"), settings)

		if _, err := Verify(extend, testVerifyAddrSession(ctx, unindexedV6)); err == nil || err.Error() != testVerifyRetryableAnswer {
			t.Fatalf("EXTEND from the unindexed family = %v, want the retryable answer", err)
		}
		requireVerifyTrailAtSeed(t, ctx, server.Id(assign.TrailId), pending)
		result, err = Verify(extend, testVerifyAddrSession(ctx, indexedV4))
		if err != nil {
			t.Fatalf("the alternate-family retry = %v, want its hop confirmed", err)
		}
		if _, ok := result.(*connect.VerifyAssignResult); !ok {
			t.Fatalf("the alternate-family retry = %T, want the next assignment", result)
		}
	})
}

// §4.1, §9, IPV6.md C7: a SEED reaching the server on its seed provider's
// unindexed family is answered as retryable without creating a trail or
// taking an active-trail slot, and its identical retry from the indexed
// address seeds a real trail. An unresolved source past the retryable
// attempts is poisoned as before.
func TestVerifySeedOnUnindexedFamilyIsRetried(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		settings.ActiveTrailsHardLimit = 1
		SetVerifySettings(settings)
		defer SetVerifySettings(model.DefaultVerifySettings())
		indexedV4, unindexedV6 := netip.MustParseAddr("192.0.2.93"), netip.MustParseAddr("2001:db8:93::1")
		seedProvider := testVerifyProvider(ctx, indexedV4, settings)
		testVerifyProvider(ctx, netip.MustParseAddr("192.0.2.94"), settings)
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)
		seed := testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin)
		if _, err := Verify(seed, testVerifyAddrSession(ctx, unindexedV6)); err == nil || err.Error() != testVerifyRetryableAnswer {
			t.Fatalf("SEED from the unindexed family = %v, want the retryable answer", err)
		}
		result, err := Verify(seed, testVerifyAddrSession(ctx, indexedV4))
		if err != nil {
			t.Fatalf("the alternate-family retry = %v, want a trail within the one active slot", err)
		}
		if assign, ok := result.(*connect.VerifyAssignResult); !ok || server.Id(assign.Trail[0]) != seedProvider {
			t.Fatalf("the alternate-family retry = %+v, want the seed provider's trail", result)
		}

		poisonValidator, poisonVpk, poisonKey := testVerifyValidator(ctx)
		poisonSeed := testVerifySeedArgs(t, poisonValidator, poisonVpk, poisonKey, connect.VerifyMMin)
		for attempt := 0; attempt < model.VerifyUnattributedAttemptLimit; attempt++ {
			if _, err := Verify(poisonSeed, testVerifyAddrSession(ctx, unindexedV6)); err == nil || err.Error() != testVerifyRetryableAnswer {
				t.Fatalf("unresolved seed attempt %d = %v, want the retryable answer", attempt+1, err)
			}
		}
		result, err = Verify(poisonSeed, testVerifyAddrSession(ctx, unindexedV6))
		if err != nil {
			t.Fatalf("unresolved seed past the retryable attempts = %v, want a poisoned trail", err)
		}
		if assign, ok := result.(*connect.VerifyAssignResult); !ok || server.Id(assign.Trail[0]) == seedProvider {
			t.Fatalf("unresolved seed past the retryable attempts = %+v, want a synthetic seed hop", result)
		}
	})
}

// §4.2 step 5, §4.4: an EXTEND from a source no provider holds, or from a
// second live address in the pending provider's family, is answered as
// retryable with the trail unchanged while the step allows, then fails the
// trail with the reason source-egress-unresolved behind the generic response.
func TestVerifyControllerUnattributedSourceKeepsTheUnresolvedReason(t *testing.T) {
	for _, source := range []string{"unknown", "duplicate"} {
		t.Run(source, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				testVerifyInstallServerKey()
				settings := model.DefaultVerifySettings()
				SetVerifySettings(settings)
				seedIp := netip.MustParseAddr("192.0.2.80")
				testVerifyProvider(ctx, seedIp, settings)
				pending := testVerifyDualStackProvider(ctx, netip.MustParseAddr("192.0.2.81"), netip.MustParseAddr("2001:db8:81::1"), settings)
				validatorId, vpk, vpkKey := testVerifyValidator(ctx)
				result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, seedIp))
				if err != nil {
					t.Fatal(err)
				}
				assign := result.(*connect.VerifyAssignResult)
				if server.Id(assign.NextHop) != pending {
					t.Fatalf("assigned %s, want the only other provider", server.Id(assign.NextHop))
				}
				from := netip.MustParseAddr("198.18.0.81")
				if source == "duplicate" {
					from = netip.MustParseAddr("192.0.2.82")
					model.FeedVerifyConnectionEgress(ctx, pending, server.NewId(), from, settings)
				}
				extend := testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign)
				for attempt := 0; attempt < model.VerifyUnattributedAttemptLimit; attempt++ {
					if _, err := Verify(extend, testVerifyAddrSession(ctx, from)); err == nil || err.Error() != testVerifyRetryableAnswer {
						t.Fatalf("EXTEND attempt %d from a %s source = %v, want the retryable answer", attempt+1, source, err)
					}
					requireVerifyTrailAtSeed(t, ctx, server.Id(assign.TrailId), pending)
				}
				_, err = Verify(extend, testVerifyAddrSession(ctx, from))
				var failure *verifyExtendFailure
				if !errors.As(err, &failure) || failure.reason != "source-egress-unresolved" || err.Error() != "400 trail failed" {
					t.Fatalf("EXTEND from a %s source past the retryable attempts = %v, want source-egress-unresolved", source, err)
				}
			})
		})
	}
}

// §9, §4.2 step 1: an EXTEND for an unknown trail does the same Redis work
// whether or not its source resolves: before the trail loads, a resolved
// source counts on its provider meter and an unresolved one on the scratch
// counter, one increment each.
func TestVerifyExtendWorkDoesNotDependOnResolution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		// one fixed rate window for the whole test
		settings.SeedRateWindow = 1000 * time.Hour
		SetVerifySettings(settings)
		defer SetVerifySettings(model.DefaultVerifySettings())
		providerIp, strangerIp := netip.MustParseAddr("203.0.113.92"), netip.MustParseAddr("203.0.113.93")
		provider := testVerifyProvider(ctx, providerIp, settings)
		for _, source := range []netip.Addr{providerIp, strangerIp} {
			trailId := server.NewId()
			_, err := Verify(&VerifyArgs{
				ClientId:  server.NewId(),
				TrailId:   &trailId,
				Trail:     []server.Id{server.NewId()},
				ExtendSig: make([]byte, ed25519.SignatureSize),
			}, testVerifyAddrSession(ctx, source))
			if err == nil || err.Error() != "400 trail not found" {
				t.Fatalf("control: EXTEND from %s = %v, want the unknown trail", source, err)
			}
		}
		// each counter took exactly one increment: the next is its second
		for name, counter := range map[string]server.Id{"provider": provider, "scratch": {}} {
			if got := model.IncrVerifyExtendProviderRate(ctx, fmt.Sprint(providerIp), counter, settings); got != 2 {
				t.Fatalf("the %s counter took %d increments, want 1", name, got-1)
			}
		}
	})
}
