package model

import (
	"fmt"
	"testing"

	"github.com/urnetwork/server"
)

// Before the post-quantum marker, a request that requires post-quantum
// encryption is answered the providers whose modern evidence of the last seven
// days is post-quantum, each where a capability-blind request puts it: a
// Quality native, a Speed native Quality borrows, and the online member of the
// Online fallback. A declaration counts for nothing (owner, 2026-11-13): a
// provider that declares without evidence is in no draw, named or discovered,
// and one with evidence is answered whether it declares or not. A provider
// whose newest evidence turns classical leaves the answer.
func TestFindProviders2PostQuantumRequestAnswersProvidersWithProbeEvidence(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		// every provider of the fixture is measured post-quantum with
		// resizable windows (egressTestHealth)
		fixture := newPostQuantumTestFixture(ctx, t, "Evidenced")
		declare := func(clientId server.Id) {
			SetProvideWithCapabilities(ctx, clientId, egressTestPublicAndNetwork, ProvideCapabilities{
				PostQuantumEncryption: true,
			})
		}
		// evidence and a declaration
		declare(fixture.natives[0])
		// a declaration and no evidence: its claim alone
		declare(fixture.natives[1])
		egressTestForgetModernEvidence(ctx, fixture.natives[1])
		// a declaration, and a newest observation that is classical
		declare(fixture.speedOnly[1])
		modernTestMeasure(ctx, fixture.speedOnly[1], false, true)
		// speedOnly[0] and onlineOnly: evidence and no declaration

		offset := egressTestBackfillOffset()
		want := map[RankMode]map[server.Id]int{
			RankModeQuality: {
				fixture.natives[0]:   0,
				fixture.speedOnly[0]: offset,
				fixture.onlineOnly:   2 * offset,
			},
			RankModeSpeed: {
				fixture.natives[0]:   0,
				fixture.speedOnly[0]: 0,
				fixture.onlineOnly:   2 * offset,
			},
		}
		offlineTestForEachReader(t, func(nativeReader bool) {
			for _, rankMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				label := fmt.Sprintf("reader=%t %s", nativeReader, rankMode)
				assertPostQuantumTestAnswer(t, label+" capability-blind", postQuantumTestFind(ctx, t, fixture.args(rankMode), false), fixture.answer(rankMode))
				assertPostQuantumTestAnswer(t, label, postQuantumTestFind(ctx, t, fixture.args(rankMode), true), want[rankMode])
				// a named provider without evidence is not answered, whatever
				// it declares, and a named one with evidence keeps its place
				// ahead of discovery
				assertPostQuantumTestAnswer(t, label+" named", postQuantumTestFind(ctx, t, fixture.args(rankMode, fixture.natives[1], fixture.natives[0]), true), want[rankMode])
			}
		})

		named := func(clientId server.Id) *FindProviders2Args {
			return &FindProviders2Args{
				Specs:    []*ProviderSpec{{ClientId: &clientId}},
				Count:    1,
				RankMode: RankModeQuality,
			}
		}
		assertPostQuantumTestAnswer(t, "named with evidence", postQuantumTestFind(ctx, t, named(fixture.natives[0]), true), map[server.Id]int{fixture.natives[0]: 0})
		assertPostQuantumTestAnswer(t, "named on its declaration alone", postQuantumTestFind(ctx, t, named(fixture.natives[1]), true), map[server.Id]int{})

		// the provider's next measurement is classical
		modernTestMeasure(ctx, fixture.natives[0], false, true)
		assertPostQuantumTestAnswer(t, "after the evidence turns classical", postQuantumTestFind(ctx, t, fixture.args(RankModeQuality), true), map[server.Id]int{
			fixture.speedOnly[0]: offset,
			fixture.onlineOnly:   2 * offset,
		})
	})
}
