// Providers with an address in each family (VALIDATOR.md §8.2, IPV6.md C7):
// hops confirm from either family, a provider scores one prefix a window, the
// per-source budgets stay one per provider, and a source the step cannot
// attribute is answered as retryable before it fails the step, its hop's
// latency counting from the attributed resend. Only a registered key learns
// anything of attribution.
package controller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
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
// the egress-IP-hash it pinned, its IPv4 address's by the rule of its index
// though its first hop lands on IPv6, so it scores one prefix, not two.
// Providers with one family record their own; the seed hop, which never
// scores, records its source's.
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
					want = dual.v4
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
// source counts on its provider meter and an unresolved one on its scratch
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
		if got := model.IncrVerifyExtendProviderRate(ctx, fmt.Sprint(providerIp), provider, settings); got != 2 {
			t.Fatalf("the provider counter took %d increments, want 1", got-1)
		}
		if got := model.PadVerifyExtendProviderRate(ctx, fmt.Sprint(strangerIp), settings); got != 2 {
			t.Fatalf("the scratch counter took %d increments, want 1", got-1)
		}
	})
}

// §9 (probe TestProbeVerifySeedDoesNotRevealAttribution, N1): only a
// registered validator key learns anything of its source's attribution. A
// SEED under any other key gets the poison answer wherever its source
// resolves: never the retryable 421, and a synthetic trail[0] that names no
// client, so a caller in a provider's /64, or behind its address, does not
// learn the provider. Its seeds do not count against the provider. A
// registered validator's SEED from an unattributed source is answered as
// retryable.
func TestVerifySeedDoesNotRevealAttribution(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		// one fixed rate window for the whole test
		settings.SeedRateWindow = 1000 * time.Hour
		SetVerifySettings(settings)
		defer SetVerifySettings(model.DefaultVerifySettings())
		providerIp := netip.MustParseAddr("2001:db8:a0::1")
		provider := testVerifyProvider(ctx, providerIp, settings)
		testVerifyProvider(ctx, netip.MustParseAddr("203.0.113.100"), settings)
		neighbor, far := netip.MustParseAddr("2001:db8:a0::99"), netip.MustParseAddr("2001:db8:b0::99")
		if got := model.ResolveVerifyEgress(ctx, neighbor, settings); got == nil || *got != provider {
			t.Fatalf("control: the neighbor's address resolved to %v, want the provider through its /64", got)
		}
		// a caller with no registered key; any keypair signs a SEED
		strangerVpk, strangerKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		stranger := server.NewId()
		for i, source := range []netip.Addr{neighbor, far, providerIp} {
			result, err := Verify(testVerifySeedArgs(t, stranger, strangerVpk, strangerKey, connect.VerifyMMin+i), testVerifyAddrSession(ctx, source))
			if err != nil {
				t.Fatalf("an unregistered key's SEED from %s = %v, want the poison answer", source, err)
			}
			if trail0 := server.Id(result.(*connect.VerifyAssignResult).Trail[0]); trail0 != verifySyntheticSeedId(source.String()) {
				t.Fatalf("an unregistered key's SEED from %s named %s in trail[0], want the synthetic hop", source, trail0)
			}
		}
		if got := model.IncrVerifySeedProviderRate(ctx, providerIp.String(), provider, settings); got != 1 {
			t.Fatalf("an unregistered key's seeds counted %d against the provider", got-1)
		}
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)
		if _, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, far)); err == nil || err.Error() != testVerifyRetryableAnswer {
			t.Fatalf("control: a registered validator's SEED from an unattributed source = %v, want the retryable answer", err)
		}
	})
}

// §4.4, §8.2 (probe TestProbeVerifyExtendFromACoLocatedProvidersAddressIsRetried,
// N4): two providers in one household share its NAT IPv4 and its /64, and
// each legacy transport indexed one family. A hop through one that lands on
// the family only the other indexed resolves to the other. It is answered as
// retryable with the trail unchanged, and the resend from the provider's own
// indexed address confirms the hop, in both directions.
func TestVerifyExtendFromACoLocatedProvidersAddressIsRetried(t *testing.T) {
	natV4 := netip.MustParseAddr("198.51.100.110")
	pendingV6, neighborV6 := netip.MustParseAddr("2001:db8:d0::2"), netip.MustParseAddr("2001:db8:d0::1")
	for _, c := range []struct {
		name                            string
		neighborIndexed, pendingIndexed netip.Addr
		landing, retry                  netip.Addr
	}{
		{name: "IPv6 lands in the neighbor's /64", neighborIndexed: neighborV6, pendingIndexed: natV4, landing: pendingV6, retry: natV4},
		{name: "IPv4 lands on the NAT the neighbor indexed", neighborIndexed: natV4, pendingIndexed: pendingV6, landing: natV4, retry: pendingV6},
	} {
		t.Run(c.name, func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				testVerifyInstallServerKey()
				settings := model.DefaultVerifySettings()
				SetVerifySettings(settings)
				seedIp := netip.MustParseAddr("192.0.2.110")
				testVerifyProvider(ctx, seedIp, settings)
				// the neighbor holds its lease but is not sampled (no provide modes)
				neighbor := server.NewId()
				model.FeedVerifyEgress(ctx, neighbor, c.neighborIndexed, settings)
				pending := testVerifyProvider(ctx, c.pendingIndexed, settings)
				if got := model.ResolveVerifyEgress(ctx, c.landing, settings); got == nil || *got != neighbor {
					t.Fatalf("control: %s resolved to %v, want the neighbor", c.landing, got)
				}
				validatorId, vpk, vpkKey := testVerifyValidator(ctx)
				result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, seedIp))
				if err != nil {
					t.Fatal(err)
				}
				assign := result.(*connect.VerifyAssignResult)
				if server.Id(assign.NextHop) != pending {
					t.Fatalf("assigned %s, want the only eligible non-seed provider", server.Id(assign.NextHop))
				}
				extend := testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign)
				// the next assignment after the confirmed hop
				testVerifyProvider(ctx, netip.MustParseAddr("192.0.2.112"), settings)
				if _, err := Verify(extend, testVerifyAddrSession(ctx, c.landing)); err == nil || err.Error() != testVerifyRetryableAnswer {
					t.Fatalf("EXTEND landing on %s = %v, want the retryable answer", c.landing, err)
				}
				requireVerifyTrailAtSeed(t, ctx, server.Id(assign.TrailId), pending)
				result, err = Verify(extend, testVerifyAddrSession(ctx, c.retry))
				if err != nil {
					t.Fatalf("the resend from the provider's own indexed %s = %v, want its hop confirmed", c.retry, err)
				}
				if _, ok := result.(*connect.VerifyAssignResult); !ok {
					t.Fatalf("the resend = %T, want the next assignment", result)
				}
			})
		})
	}
}

// §8.1, §9 (probe TestProbeVerifyPoisonTrailDoesNotPinTheHeadHash, N3):
// poison trails replace provider-state writes with pads, so a poison trail's
// confirmed hop sets no head-hash pin. Anyone (no registered key) can seed a
// poison trail and route its EXTEND to an assigned dual-stack provider on the
// family of its choice; the provider's scored prefix for the window stays
// its own choice. A real trail's hop pins it.
func TestVerifyPoisonTrailDoesNotPinTheHeadHash(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		SetVerifySettings(settings)
		v4, v6 := netip.MustParseAddr("203.0.113.121"), netip.MustParseAddr("2001:db8:121::1")
		provider := testVerifyDualStackProvider(ctx, v4, v6, settings)
		pinKey := func(trailId connect.Id) string {
			return fmt.Sprintf("{vhead_%s}%s", provider, model.GetVerifyTrailHeadWindow(ctx, server.Id(trailId), settings))
		}
		requirePinned := func(trailId connect.Id, want bool) {
			t.Helper()
			server.Redis(ctx, func(r server.RedisClient) {
				count, err := r.Exists(ctx, pinKey(trailId)).Result()
				server.Raise(err)
				if (count == 1) != want {
					t.Fatalf("the provider's pin exists=%t, want %t", count == 1, want)
				}
			})
		}
		strangerVpk, strangerKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		stranger := server.NewId()
		// an unattributed seed source: the provider is the only next hop
		seedIp := netip.MustParseAddr("192.0.2.120")
		result, err := Verify(testVerifySeedArgs(t, stranger, strangerVpk, strangerKey, connect.VerifyMMin), testVerifyAddrSession(ctx, seedIp))
		if err != nil {
			t.Fatal(err)
		}
		assign := result.(*connect.VerifyAssignResult)
		if server.Id(assign.NextHop) != provider {
			t.Fatalf("assigned %s, want the dual-stack provider", server.Id(assign.NextHop))
		}
		if _, err := Verify(testVerifyExtendArgs(t, stranger, strangerVpk, strangerKey, assign), testVerifyAddrSession(ctx, v6)); err != nil {
			t.Fatalf("control: the poison trail's EXTEND = %v", err)
		}
		requirePinned(assign.TrailId, false)

		// a real trail of M = 4 through three more providers reaches it
		sources := map[server.Id]netip.Addr{provider: v6}
		realSeedIp := netip.MustParseAddr("192.0.2.122")
		testVerifyProvider(ctx, realSeedIp, settings)
		for _, ip := range []netip.Addr{netip.MustParseAddr("192.0.2.123"), netip.MustParseAddr("192.0.2.124")} {
			sources[testVerifyProvider(ctx, ip, settings)] = ip
		}
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)
		result, err = Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, realSeedIp))
		if err != nil {
			t.Fatal(err)
		}
		assign = result.(*connect.VerifyAssignResult)
		for {
			next := server.Id(assign.NextHop)
			source, ok := sources[next]
			if !ok {
				t.Fatalf("assigned %s, not a registered provider", next)
			}
			result, err = Verify(testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign), testVerifyAddrSession(ctx, source))
			if err != nil {
				t.Fatalf("the real trail's EXTEND from %s = %v", source, err)
			}
			if next == provider {
				break
			}
			assign = result.(*connect.VerifyAssignResult)
		}
		requirePinned(assign.TrailId, true)
		own := model.VerifyEgressIpHashWithSettings(v4, settings)
		if got := model.PinVerifyHopEgressIpHash(ctx, provider, v4, model.GetVerifyTrailHeadWindow(ctx, server.Id(assign.TrailId), settings), false, settings); got != own {
			t.Errorf("an IPv4 hop after the provider's IPv6 hops recorded %x, not its IPv4 prefix", got)
		}
	})
}

// A trail whose first EXTEND, from its pending provider's unindexed family,
// is answered as retryable `retries` times, each after unattributedDelay,
// before the resend from the indexed address confirms the hop. It returns the
// pending hop's ASSIGN time, the step's last retryable answer, and the trail
// and provider.
func testVerifyHopAfterRetryableAnswers(t testing.TB, ctx context.Context, settings *model.VerifySettings, retries int) (assignedMs uint64, answeredMs uint64, trailId server.Id, pending server.Id) {
	t.Helper()
	testVerifyInstallServerKey()
	SetVerifySettings(settings)
	seedIp := netip.MustParseAddr("192.0.2.130")
	testVerifyProvider(ctx, seedIp, settings)
	indexed, unindexed := netip.MustParseAddr("203.0.113.131"), netip.MustParseAddr("2001:db8:131::1")
	pending = testVerifyProvider(ctx, indexed, settings)
	validatorId, vpk, vpkKey := testVerifyValidator(ctx)
	result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, connect.VerifyMMin), testVerifyAddrSession(ctx, seedIp))
	if err != nil {
		t.Fatal(err)
	}
	assign := result.(*connect.VerifyAssignResult)
	if server.Id(assign.NextHop) != pending {
		t.Fatalf("assigned %s, want the only other provider", server.Id(assign.NextHop))
	}
	trailId = server.Id(assign.TrailId)
	assignedMs = model.GetVerifyTrail(ctx, trailId).Pending.AssignedMs
	extend := testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign)
	// the next assignment after the confirmed hop
	testVerifyProvider(ctx, netip.MustParseAddr("192.0.2.132"), settings)
	for attempt := 0; attempt < retries; attempt++ {
		time.Sleep(testVerifyUnattributedDelay)
		if _, err := Verify(extend, testVerifyAddrSession(ctx, unindexed)); err == nil || err.Error() != testVerifyRetryableAnswer {
			t.Fatalf("attempt %d from the unindexed family = %v, want the retryable answer", attempt+1, err)
		}
		if trail := model.GetVerifyTrail(ctx, trailId); trail.Pending.AssignedMs != assignedMs {
			t.Fatalf("a retryable answer moved the pending hop's ASSIGN time to %d", trail.Pending.AssignedMs)
		}
		answeredMs = model.GetVerifyUnattributedExtendAnsweredMs(ctx, trailId, 1)
	}
	if _, err := Verify(extend, testVerifyAddrSession(ctx, indexed)); err != nil {
		t.Fatalf("the attributed attempt = %v", err)
	}
	return
}

// The time an unattributed attempt of testVerifyHopAfterRetryableAnswers takes.
const testVerifyUnattributedDelay = 300 * time.Millisecond

// §7.5, §8.2 (N6, with hop_latency_from_attributed_attempt; the validator's
// counterpart is sn probe TestProbeTrailHopLatencyExcludesTheUnattributedAttempt):
// a hop's recorded latency, and so the published percentiles, count from the
// attempt that was attributed. A hop answered 421 once or twice records the
// step's last retryable answer as its start, when its resend went, so its
// latency excludes the unattributed attempts; the 421s leave the pending hop,
// and so the step's deadline, unchanged. A hop with no 421 keeps its ASSIGN
// time.
func TestVerifyHopLatencyExcludesTheUnattributedAttempts(t *testing.T) {
	for _, retries := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("%d retryable answers", retries), func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				settings := model.DefaultVerifySettings()
				settings.HopLatencyFromAttributedAttempt = true
				assignedMs, answeredMs, trailId, pending := testVerifyHopAfterRetryableAnswers(t, ctx, settings, retries)
				hop := model.GetVerifyTrail(ctx, trailId).Hops[1]
				wantStart := assignedMs
				if 0 < retries {
					wantStart = answeredMs
				}
				if hop.ClientId != pending || hop.AssignedMs != wantStart {
					t.Fatalf("the confirmed hop starts at %d, want %d (ASSIGN at %d)", hop.AssignedMs, wantStart, assignedMs)
				}
				latencyMs := hop.ConfirmedMs - hop.AssignedMs
				delayMs := uint64(testVerifyUnattributedDelay.Milliseconds())
				if 0 < retries {
					if hop.ConfirmedMs-assignedMs < uint64(retries)*delayMs {
						t.Fatalf("control: the step took %d ms, less than its unattributed attempts", hop.ConfirmedMs-assignedMs)
					}
					if delayMs <= latencyMs {
						t.Fatalf("the recorded latency %d ms includes an unattributed attempt", latencyMs)
					}
				}

				// the published percentile is the attributed attempt's
				model.RollupVerifyProviderStats(ctx, server.NowUtc(), settings)
				want := model.VerifyLatencyBucketMs(model.VerifyLatencyBucketIndex(int64(latencyMs)))
				published := false
				for _, row := range model.GetVerifyProviderStats(ctx, pending) {
					if row.Confirmations == 0 {
						continue
					}
					published = true
					if row.Confirmations != 1 || row.LatencyP50Ms == nil || *row.LatencyP50Ms != want {
						t.Fatalf("the provider's published row = %+v, want one confirmation at p50 %d ms", row, want)
					}
				}
				if !published {
					t.Fatal("the confirmation was not published")
				}
			})
		})
	}
}

// §7.5 (N6, by default): without hop_latency_from_attributed_attempt a hop
// confirmed after one or two 421s keeps its ASSIGN time, exactly the record
// main writes: in the receipts, the confirmed hop is its pending hop with only
// the confirmation time and egress-IP-hash added, which every validator's
// receipt check accepts. Its latency still holds the unattributed attempts.
func TestVerifyHopKeepsItsAssignTimeByDefault(t *testing.T) {
	for _, retries := range []int{1, 2} {
		t.Run(fmt.Sprintf("%d retryable answers", retries), func(t *testing.T) {
			server.DefaultTestEnv().Run(t, func(t testing.TB) {
				ctx := context.Background()
				settings := model.DefaultVerifySettings()
				if settings.HopLatencyFromAttributedAttempt {
					t.Fatal("the default counts a hop's latency from the attributed attempt")
				}
				start := server.NowUtc()
				assignedMs, _, trailId, _ := testVerifyHopAfterRetryableAnswers(t, ctx, settings, retries)
				hop := model.GetVerifyTrail(ctx, trailId).Hops[1]
				if hop.AssignedMs != assignedMs || hop.ConfirmedMs-hop.AssignedMs < uint64(retries)*uint64(testVerifyUnattributedDelay.Milliseconds()) {
					t.Fatalf("the confirmed hop starts at %d after %d ms, want its ASSIGN time %d", hop.AssignedMs, hop.ConfirmedMs-hop.AssignedMs, assignedMs)
				}

				// the receipt that assigned the hop, and the one that confirmed it
				bodies := map[int]*model.VerifyOriginalBody{}
				for _, original := range model.ListVerifyOriginals(ctx, start.Add(-time.Minute), server.NowUtc().Add(time.Minute), 100) {
					body, err := model.DecodeVerifyOriginal(original)
					if err != nil {
						t.Fatal(err)
					}
					if body.Trail.TrailId == trailId {
						bodies[body.PreviousDepth] = body
					}
				}
				if bodies[0] == nil || bodies[1] == nil || bodies[0].Trail.Pending == nil || len(bodies[1].Trail.Hops) != 2 {
					t.Fatalf("the trail's receipts = %+v", bodies)
				}
				advanced := *bodies[0].Trail.Pending
				confirmed := bodies[1].Trail.Hops[1]
				advanced.ConfirmedMs, advanced.EgressIpHash = confirmed.ConfirmedMs, confirmed.EgressIpHash
				want, err := json.Marshal(&advanced)
				if err != nil {
					t.Fatal(err)
				}
				got, err := json.Marshal(confirmed)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, want) {
					t.Fatalf("the confirming receipt's hop %s is not its assigning receipt's pending hop %s confirmed", got, want)
				}
			})
		})
	}
}

// §7.5: hop_latency_from_attributed_attempt is a server-only verify.yml key,
// off when absent, read with the other settings.
func TestParseVerifySettingsHopLatencyFromAttributedAttempt(t *testing.T) {
	policyHash := [32]byte{4, 5, 6}
	coordinator := common.HexToAddress("0xC18925925E2B7bb9059b7d696b8c92762AE86406")
	egressKey := base64.StdEncoding.EncodeToString(make([]byte, 32))
	for key, want := range map[string]bool{
		"": false,
		"  hop_latency_from_attributed_attempt: false\n": false,
		"  hop_latency_from_attributed_attempt: true\n":  true,
	} {
		SetStConfig(&StConfig{Profile: "testnet", PolicyHash: policyHash, ChainId: 964, ContractAddress: coordinator})
		pop := server.Vault.PushSimpleResource("verify-latency-test.yml", []byte(fmt.Sprintf(`profile: testnet
policy_hash: "0x%x"
egress_hash_key: %s
settings:
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
%s`, policyHash, egressKey, key)))
		settings := parseVerifySettings(server.Vault.RequireSimpleResource("verify-latency-test.yml"))
		pop()
		SetStConfig(nil)
		if settings.HopLatencyFromAttributedAttempt != want {
			t.Fatalf("with %q the setting is %t, want %t", key, settings.HopLatencyFromAttributedAttempt, want)
		}
	}
}
