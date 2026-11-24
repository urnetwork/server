package session

// The hosted path's budgets against the local test Redis, held for longer
// than RevocationCheckBudget and shorter than HostedRevocationCheckBudget
// (server.Testing_DelayAuthRedisCommands): the proxy processes' in-process
// Redis calls of 2026-11-21, 5-15% of them past 250ms while Redis answered in
// microseconds. These tests need the local test PostgreSQL and Redis.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Longer than the default check and mint budgets, shorter than the hosted
// ones: a check or step held this long is answered only within the hosted
// budget.
const testingLateRedis = 400 * time.Millisecond

// A hosted check whose record misses reads the session marker before its
// state query, within the hosted budget too: a marker that answers after
// 250ms is checked there, while the api's check still skips it and counts
// the acceptance.
func TestHostedRecordMissReadsTheMarkerWithinTheHostedBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		actor := recoverySessionActor(t)
		read := readRevocationRecord
		// a miss: no field of the record is present
		readRevocationRecord = func(ctx context.Context, key string, fields []string) ([]any, error) {
			return make([]any, len(fields)), nil
		}
		defer func() { readRevocationRecord = read }()
		pop := server.Testing_DelayAuthRedisCommands(testingLateRedis)
		defer pop()

		for _, test := range []struct {
			name      string
			source    StateQuerySource
			path      string
			unchecked float64
		}{
			{"hosted control", StateQueryHostedControl, revocationCheckPathHosted, 0},
			{"api", StateQueryApiOther, revocationCheckPathApi, 1},
		} {
			ctx := WithStateQuerySource(actor.Ctx, test.source)
			accepted := testingUncheckedAcceptances(test.path, revocationCheckCredentialSession)
			queries := testingStateQueries(test.source, false)
			if err := ValidateByJwtState(ctx, actor.ByJwt, false); err != nil {
				t.Fatalf("the %s check refused a valid session: %v", test.name, err)
			}
			if got := testingStateQueries(test.source, false) - queries; got != 1 {
				t.Errorf("the %s miss ran %.0f state queries, want 1", test.name, got)
			}
			if got := testingUncheckedAcceptances(test.path, revocationCheckCredentialSession) - accepted; got != test.unchecked {
				t.Errorf("the %s check counted %.0f unchecked acceptances of a marker Redis answered after %s, want %.0f", test.name, got, testingLateRedis, test.unchecked)
			}
		}
	})
}

// Root cause (2026-11-21), at the hosted proxy's own session mint: a device
// open on the proxy hosts mints the proxy's session (MintHostedSession), and
// with the mint budget a Redis that answered after 250ms left a bootstrap
// signed untagged, outside the Sessions list, and a stored binding refused.
// Each Redis step of that mint now waits the hosted budget: the session is
// registered, carried and bound.
func TestHostedSessionMintRegistersWithinTheHostedBudget(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		proxy := network.taggedClient(ctx, nil)

		// pop closes the held pool once; the session's score is read unheld
		pop := server.Testing_DelayAuthRedisCommands(testingLateRedis)
		defer pop()
		untagged := testingUntaggedMints(SessionKindClient)
		hosted := proxy.Renew()
		signed, err := MintHostedSession(ctx, hosted)
		if err != nil || signed == "" {
			t.Fatalf("a hosted bootstrap failed while Redis answered after %s: %v", testingLateRedis, err)
		}
		if hosted.SessionId == nil {
			t.Fatalf("the hosted bootstrap was signed untagged though Redis answered after %s, within the hosted budget %s", testingLateRedis, HostedRevocationCheckBudget)
		}
		sid := *hosted.SessionId
		// a stored binding: the proxy's next device mints on the same session
		again := proxy.Renew()
		signedAgain, err := MintHostedSession(ctx, again)
		pop()
		if err != nil || signedAgain == "" {
			t.Fatalf("a bound proxy's mint failed while Redis answered after %s: %v", testingLateRedis, err)
		}
		if again.SessionId == nil || *again.SessionId != sid {
			t.Fatalf("the bound proxy's mint carries session %v, want %s", again.SessionId, sid)
		}

		if got := testingUntaggedMints(SessionKindClient) - untagged; got != 0 {
			t.Errorf("counted %.0f untagged mints, want 0", got)
		}
		if binding := testingClientSessionBinding(ctx, t, network.networkId, *proxy.ClientId); binding == nil || *binding != sid {
			t.Fatalf("the proxy is bound to %v, want its session %s", binding, sid)
		}
		parsed, err := ParseByJwt(ctx, signed)
		if err != nil || parsed.SessionId == nil || *parsed.SessionId != sid {
			t.Fatalf("the signed hosted token carries %v, want the session %s: %v", parsed, sid, err)
		}
		if score := testingSessionScore(ctx, t, network.networkId, sid); score < float64(DeadlineMillis(parsed.AcceptUntil())) {
			t.Fatalf("the session's live score %.0f is short of the token's horizon %d", score, DeadlineMillis(parsed.AcceptUntil()))
		}
	})
}
