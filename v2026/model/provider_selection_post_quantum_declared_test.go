package model

import (
	"fmt"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// The providers whose latest provide declared post-quantum encryption are
// answered to a request that requires it, each where a capability-blind
// request puts it: a Quality native, a Speed native Quality borrows, and the
// online member of the Online fallback. The undeclared ones are in no draw,
// named or discovered. A provider whose latest provide stops declaring leaves
// the answer.
func TestFindProviders2PostQuantumRequestAnswersDeclaredProviders(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		onlineTestFreshSubscriberCache(t)
		ctx := t.Context()
		fixture := newPostQuantumTestFixture(ctx, t, "Declared")
		declare := func(clientId server.Id, postQuantumEncryption bool) {
			SetProvideWithCapabilities(ctx, clientId, egressTestPublicAndNetwork, ProvideCapabilities{
				PostQuantumEncryption: postQuantumEncryption,
			})
		}
		declare(fixture.natives[0], true)
		declare(fixture.speedOnly[0], true)
		declare(fixture.onlineOnly, true)
		// what an older release provides: no declaration
		declare(fixture.natives[1], false)

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
				// a named undeclared provider is not answered, and a named
				// declared one keeps its place ahead of discovery
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
		assertPostQuantumTestAnswer(t, "named declared", postQuantumTestFind(ctx, t, named(fixture.natives[0]), true), map[server.Id]int{fixture.natives[0]: 0})
		assertPostQuantumTestAnswer(t, "named undeclared", postQuantumTestFind(ctx, t, named(fixture.natives[1]), true), map[server.Id]int{})

		// the provider's next provide no longer declares it
		declare(fixture.natives[0], false)
		assertPostQuantumTestAnswer(t, "after the declaration ends", postQuantumTestFind(ctx, t, fixture.args(RankModeQuality), true), map[server.Id]int{
			fixture.speedOnly[0]: offset,
			fixture.onlineOnly:   2 * offset,
		})
	})
}
