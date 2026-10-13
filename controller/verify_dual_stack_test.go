// Providers with an address in each family (VALIDATOR.md §8.2, IPV6.md C7):
// hops confirm from either family, every hop scores as one prefix per
// provider, the per-source budgets stay one per provider, and a source the
// index cannot attribute keeps the existing failure reason.
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

// §8.1, D27, IPV6.md C7: a provider with an address in each family confirms
// its hop from either, and every hop it serves, the seed included, records
// its IPv4 prefix hash, so it scores as one prefix and not two. A provider
// with only an IPv6 address records that address's prefix.
func TestVerifyDualStackHopsRecordOnePrefixPerProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		SetVerifySettings(settings)

		const m = connect.VerifyMMin
		type addresses struct{ v4, v6 netip.Addr }
		providers := map[server.Id]addresses{}
		var seedProvider server.Id
		for i := 0; i < m; i++ {
			provider := addresses{v6: netip.MustParseAddr(fmt.Sprintf("2001:db8:%x::1", 0x60+i))}
			var providerId server.Id
			if i == m-1 {
				providerId = testVerifyProvider(ctx, provider.v6, settings)
			} else {
				provider.v4 = netip.MustParseAddr(fmt.Sprintf("203.0.113.%d", 60+10*i))
				providerId = testVerifyDualStackProvider(ctx, provider.v4, provider.v6, settings)
			}
			providers[providerId] = provider
			if i == 0 {
				seedProvider = providerId
			}
		}
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)

		// the seed and the even hops land on IPv6, the odd hops on IPv4 where
		// the provider has it: whatever the order, a dual-stack provider
		// confirms an EXTEND from its IPv6 address
		result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, m), testVerifyAddrSession(ctx, providers[seedProvider].v6))
		if err != nil {
			t.Fatal(err)
		}
		assign, ok := result.(*connect.VerifyAssignResult)
		if !ok || server.Id(assign.Trail[0]) != seedProvider {
			t.Fatalf("seed from the seed provider's IPv6 address = %#v, want its trail", result)
		}
		var final *connect.VerifyFinalResult
		for step := 0; final == nil; step++ {
			if m < step {
				t.Fatal("trail did not finalize within M steps")
			}
			provider, ok := providers[server.Id(assign.NextHop)]
			if !ok {
				t.Fatalf("assigned hop %s is not a registered provider", server.Id(assign.NextHop))
			}
			source := provider.v6
			if step%2 == 1 && provider.v4.IsValid() {
				source = provider.v4
			}
			result, err := Verify(testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign), testVerifyAddrSession(ctx, source))
			if err != nil {
				t.Fatalf("EXTEND from %s: %v", source, err)
			}
			if next, ok := result.(*connect.VerifyAssignResult); ok {
				assign = next
				continue
			}
			if final, ok = result.(*connect.VerifyFinalResult); !ok {
				t.Fatalf("extend result = %T", result)
			}
		}

		if len(final.Proof.Hops) != m {
			t.Fatalf("proof depth %d, want %d", len(final.Proof.Hops), m)
		}
		for i, hop := range final.Proof.Hops {
			provider := providers[server.Id(hop.ClientId)]
			prefix := provider.v6
			if provider.v4.IsValid() {
				prefix = provider.v4
			}
			if hop.EgressIpHash != model.VerifyEgressIpHashWithSettings(prefix, settings) {
				t.Fatalf("proof hop %d egress hash is not its provider's %s prefix", i, prefix)
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

// §4.2 step 5: an EXTEND from a source no provider holds keeps the reason
// source-egress-unresolved behind the generic response, as does one from a
// second live address in the pending provider's family, which stays
// ambiguous.
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
				_, err = Verify(testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign), testVerifyAddrSession(ctx, from))
				var failure *verifyExtendFailure
				if !errors.As(err, &failure) || failure.reason != "source-egress-unresolved" || err.Error() != "400 trail failed" {
					t.Fatalf("EXTEND from a %s source = %v, want source-egress-unresolved", source, err)
				}
			})
		})
	}
}
