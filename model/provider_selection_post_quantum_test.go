// A request that requires post-quantum encryption is answered only with
// providers whose latest provide declared post-quantum encryption sessions, in
// every draw of the request, named providers included. A capability-blind
// request is answered as before (support inbox 7918, 7843,
// https://github.com/urnetwork/connect/issues/220).
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// One location's providers as the score export publishes them.
type postQuantumTestFixture struct {
	city *Location
	// live, verified: Quality and Speed natives
	natives []server.Id
	// live, ARIN non-quality: Speed natives that Quality refuses
	speedOnly []server.Id
	// live, with a failing URL ratio: online, in neither native bucket
	onlineOnly server.Id
}

func newPostQuantumTestFixture(ctx context.Context, t testing.TB, name string) *postQuantumTestFixture {
	t.Helper()
	fixture := &postQuantumTestFixture{
		city: egressTestCity(ctx, name+" City", name+" Region", "United States", "us"),
	}
	for range 2 {
		provider := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
		egressTestProbed(ctx, provider, fixture.city, 0, "us")
		fixture.natives = append(fixture.natives, provider.clientId)
	}
	for range 2 {
		fixture.speedOnly = append(fixture.speedOnly, egressTestOverLine(ctx, t, fixture.city, egressTestFast).clientId)
	}
	onlineOnly := egressTestConnect(ctx, t, fixture.city, egressTestFast, nil, nil)
	egressTestHealth(ctx, onlineOnly.clientId, server.NowUtc(), 3, 1)
	fixture.onlineOnly = onlineOnly.clientId
	egressTestPasses(ctx, t)
	return fixture
}

// Every provider of the fixture at its tier in a capability-blind answer: in
// Quality the natives, then the Speed natives Quality refuses at the borrowed
// tier, then the online member at the Online fallback's; in Speed all four
// Speed natives, then the online member.
func (self *postQuantumTestFixture) answer(rankMode RankMode) map[server.Id]int {
	offset := egressTestBackfillOffset()
	speedOnlyTier := offset
	if rankMode == RankModeSpeed {
		speedOnlyTier = 0
	}
	return map[server.Id]int{
		self.natives[0]:   0,
		self.natives[1]:   0,
		self.speedOnly[0]: speedOnlyTier,
		self.speedOnly[1]: speedOnlyTier,
		self.onlineOnly:   2 * offset,
	}
}

// A request for the fixture's location, with the providers named by id.
func (self *postQuantumTestFixture) args(rankMode RankMode, namedClientIds ...server.Id) *FindProviders2Args {
	specs := egressTestLocationSpec(self.city)
	for _, clientId := range namedClientIds {
		specs = append(specs, &ProviderSpec{ClientId: &clientId})
	}
	return &FindProviders2Args{
		Specs:      specs,
		Count:      egressTestBackfillCount,
		ForceCount: true,
		RankMode:   rankMode,
	}
}

// Asks FindProviders2 as a caller in a new network. postQuantumEncryption is
// set the way a client sends it, as the request's wire field.
func postQuantumTestFind(ctx context.Context, t testing.TB, args *FindProviders2Args, postQuantumEncryption bool) []*FindProvidersProvider {
	t.Helper()
	if postQuantumEncryption {
		connect.AssertEqual(t, json.Unmarshal([]byte(`{"post_quantum_encryption":true}`), args), nil)
	}
	clientSession := testingCreateProviderSearchSession(
		ctx,
		session.NewByJwt(server.NewId(), server.NewId(), "post-quantum-test", false, false),
	)
	result, err := FindProviders2(args, clientSession)
	connect.AssertEqual(t, err, nil)
	return result.Providers
}

// Fails unless the answer is exactly `want`, each at its tier, in the
// client's order.
func assertPostQuantumTestAnswer(t testing.TB, label string, providers []*FindProvidersProvider, want map[server.Id]int) {
	t.Helper()
	assertEgressTestNoRepeats(t, providers)
	assertEgressTestTiersKeepOrder(t, providers)
	for _, provider := range providers {
		tier, ok := want[provider.ClientId]
		if !ok {
			t.Fatalf("%s: answered a provider that is not in the answer, %d providers in all", label, len(providers))
		}
		if provider.Tier != tier {
			t.Fatalf("%s: answered tier %d, want %d", label, provider.Tier, tier)
		}
	}
	if len(providers) != len(want) {
		t.Fatalf("%s: answered %d providers, want %d", label, len(providers), len(want))
	}
}

// No provider of the location declared post-quantum encryption, as no
// provider did before the declaration existed, so a request that requires it
// is answered nothing in either mode, whichever bucket a provider is in, and a
// provider it names by id is not answered either. Before this change the
// request was capability-blind and got every provider of the location, and the
// client then dialed and failed through each one, none of which can seal its
// session. The capability-blind request still gets them all.
func TestFindProviders2PostQuantumRequestLeavesOutUndeclaredProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newPostQuantumTestFixture(ctx, t, "Undeclared")
		offlineTestForEachReader(t, func(nativeReader bool) {
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				label := fmt.Sprintf("reader=%t %s", nativeReader, rankMode)
				blind := postQuantumTestFind(ctx, t, fixture.args(rankMode), false)
				assertPostQuantumTestAnswer(t, label+" capability-blind", blind, fixture.answer(rankMode))

				answered := postQuantumTestFind(ctx, t, fixture.args(rankMode, fixture.natives[0]), true)
				if 0 < len(answered) {
					t.Fatalf("%s: a post-quantum request was answered %d providers that never declared post-quantum encryption", label, len(answered))
				}
			}
		})
		// a request that only names a provider bypasses discovery
		named := &FindProviders2Args{
			Specs:    []*ProviderSpec{{ClientId: &fixture.natives[0]}},
			Count:    1,
			RankMode: RankModeQuality,
		}
		connect.AssertEqual(t, len(postQuantumTestFind(ctx, t, named, false)), 1)
		named = &FindProviders2Args{
			Specs:    []*ProviderSpec{{ClientId: &fixture.natives[0]}},
			Count:    1,
			RankMode: RankModeQuality,
		}
		if answered := postQuantumTestFind(ctx, t, named, true); 0 < len(answered) {
			t.Fatal("a post-quantum request was answered the named provider that never declared post-quantum encryption")
		}
	})
}
