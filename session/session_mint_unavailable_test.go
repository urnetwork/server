package session

// Decision 3 at the session marker and at mint (R-mint-redis), with the
// stalled-dial stand-in for the 2026-11-18 Redis outage. These tests need the
// local test PostgreSQL: the marker path asks SQL, and mints write rows.

import (
	"context"
	"errors"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

func testingUntaggedMints(kind string) float64 {
	return testutil.ToFloat64(sessionMintUntaggedCounter.WithLabelValues(kind))
}

func testingProjectionWrites() float64 {
	total := 0.0
	for _, outcome := range []string{"success", "failure", "stale"} {
		total += testutil.ToFloat64(revocationRecordWriteCounter.WithLabelValues("projection", outcome))
	}
	return total
}

func testingClientSessionBinding(ctx context.Context, t testing.TB, networkId server.Id, clientId server.Id) *server.Id {
	var sessionId *server.Id
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT session_id FROM network_client WHERE network_id=$1 AND client_id=$2`, networkId, clientId).Scan(&sessionId))
	})
	return sessionId
}

func testingSessionScore(ctx context.Context, t testing.TB, networkId server.Id, sessionId server.Id) float64 {
	var score float64
	if err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		score, err = r.ZScore(ctx, SessionKey(networkId, "z"), sessionId.String()).Result()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return score
}

// With the record off (or in a transaction, which never reads the record), a
// session credential's marker that Redis does not answer is skipped: the SQL
// state query decides, and only an acceptance is counted.
func TestSessionMarkerUnansweredAcceptsWhenSqlAgrees(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(false)()
		actor := recoverySessionActor(t)
		ctx := WithStateQuerySource(actor.Ctx, StateQueryApiOther)
		_, pop := server.Testing_StallAuthRedisDials(48)
		defer pop()

		accepted := testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialSession)
		start := time.Now()
		if err := ValidateByJwtState(ctx, actor.ByJwt, false); err != nil {
			t.Fatalf("an unanswered marker refused the session: %v", err)
		}
		if elapsed := time.Since(start); RevocationCheckBudget+time.Second < elapsed {
			t.Errorf("the unanswered marker took %s", elapsed)
		}
		if got := testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialSession) - accepted; got != 1 {
			t.Errorf("counted %.0f unchecked acceptances, want 1", got)
		}

		inTx := testingUncheckedAcceptances(revocationCheckPathTransaction, revocationCheckCredentialSession)
		var txErr error
		server.Tx(ctx, func(tx server.PgTx) {
			txErr = ValidateByJwtStateInTx(ctx, tx, actor.ByJwt, false)
		})
		if txErr != nil {
			t.Fatalf("an unanswered marker refused the session in a transaction: %v", txErr)
		}
		if got := testingUncheckedAcceptances(revocationCheckPathTransaction, revocationCheckCredentialSession) - inTx; got != 1 {
			t.Errorf("counted %.0f unchecked transaction acceptances, want 1", got)
		}

		// SQL still decides: a rotated credential is refused and not counted
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, actor.ByJwt.UserId, server.NowUtc().Add(time.Minute)))
		})
		accepted = testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialSession)
		if err := ValidateByJwtState(ctx, actor.ByJwt, false); err == nil || errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("a rotated credential answered %v", err)
		}
		if got := testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialSession) - accepted; got != 0 {
			t.Errorf("counted %.0f acceptances of a refused credential", got)
		}
	})
}

// A record miss goes to SQL as before. When the session marker it then reads
// is unanswered, the state query still decides, the acceptance is counted,
// and nothing is projected: the projection would say the session is not
// revoked, which nobody checked.
func TestRevocationRecordFallbackSkipsAnUnansweredMarkerWithoutProjecting(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		actor := recoverySessionActor(t)
		ctx := WithStateQuerySource(actor.Ctx, StateQueryApiOther)
		read := readRevocationRecord
		readRevocationRecord = func(ctx context.Context, key string, fields []string) ([]any, error) {
			return make([]any, len(fields)), nil
		}
		defer func() { readRevocationRecord = read }()
		_, pop := server.Testing_StallAuthRedisDials(48)
		defer pop()

		accepted := testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialSession)
		projections := testingProjectionWrites()
		queries := testingStateQueries(StateQueryApiOther, false)
		if err := ValidateByJwtState(ctx, actor.ByJwt, false); err != nil {
			t.Fatalf("a record miss with an unanswered marker refused the session: %v", err)
		}
		if got := testingStateQueries(StateQueryApiOther, false) - queries; got != 1 {
			t.Errorf("the miss ran %.0f state queries, want 1", got)
		}
		if got := testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialSession) - accepted; got != 1 {
			t.Errorf("counted %.0f unchecked acceptances, want 1", got)
		}
		if got := testingProjectionWrites() - projections; got != 0 {
			t.Errorf("an unchecked session was projected %.0f times", got)
		}
	})
}

// R-mint-redis: a sign-in starts its session, so when Redis does not register
// it the token is signed untagged, as before sessions, and counted. Before,
// the sign-in failed with 503 after two seconds.
func TestNewSignInSignsUntaggedWhenRedisDoesNotRegister(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetSessionCreationEnabled(true)()
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		_, pop := server.Testing_StallAuthRedisDials(48)
		defer pop()
		untagged := testingUntaggedMints(SessionKindSignin)
		credential := NewByJwt(actor.ByJwt.NetworkId, actor.ByJwt.UserId, "untagged", false, false)
		start := time.Now()
		signed, err := MintNetworkSession(ctx, credential, "password")
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("a sign-in failed while Redis did not answer: %v", err)
		}
		// the marker in the transaction and the clock sample, a budget each
		if bound := 2*RevocationCheckBudget + time.Second; bound < elapsed {
			t.Errorf("the sign-in took %s, want at most %s", elapsed, bound)
		}
		if credential.SessionId != nil {
			t.Fatal("the sign-in kept a session Redis never registered")
		}
		parsed, err := ParseByJwt(ctx, signed)
		if err != nil || parsed.SessionId != nil {
			t.Fatalf("the signed token is not untagged: %v %v", parsed, err)
		}
		if got := testingUntaggedMints(SessionKindSignin) - untagged; got != 1 {
			t.Errorf("counted %.0f untagged mints, want 1", got)
		}
	})
}

// A fresh session's client binding, written earlier in the mint's
// transaction, is cleared with the session: otherwise the untagged token's
// next refresh, which derives its own lineage, would find a different binding
// and be refused as client_session_changed.
func TestFreshClientSessionUnbindsItsClientWhenRedisDoesNotRegister(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		mint := top.Renew()
		PrepareSessionMint(mint, true)
		if mint.SessionId == nil {
			t.Fatal("no session was prepared")
		}
		_, pop := server.Testing_StallAuthRedisDials(48)
		defer pop()
		untagged := testingUntaggedMints(SessionKindClient)
		var signed string
		err := sessionTx(ctx, func(tx server.PgTx) error {
			if err := LockSessionLifecycle(ctx, tx, network.networkId, false); err != nil {
				return err
			}
			if err := AssociateSessionClientInTx(ctx, tx, mint, false); err != nil {
				return err
			}
			var err error
			signed, err = RegisterAndSignInTx(ctx, tx, mint, "api_key_client", nil, false)
			return err
		})
		if err != nil || signed == "" {
			t.Fatalf("the client mint failed while Redis did not answer: %v", err)
		}
		if mint.SessionId != nil || mint.RootClientId == nil {
			t.Fatalf("the untagged client token has session %v and root %v", mint.SessionId, mint.RootClientId)
		}
		if binding := testingClientSessionBinding(ctx, t, network.networkId, *top.ClientId); binding != nil {
			t.Fatalf("the client stays bound to the unregistered session %s", binding)
		}
		if got := testingUntaggedMints(SessionKindClient) - untagged; got != 1 {
			t.Errorf("counted %.0f untagged mints, want 1", got)
		}
	})
}

// A legacy lineage derived from an untagged credential is signed untagged
// too, and keeps its binding: the next refresh derives the same session, and
// once Redis answers it registers and carries it.
func TestDerivedLineageKeepsItsBindingWhenRedisDoesNotRegister(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		derived := LegacySessionId(top, server.NewId())
		clientSession := NewLocalClientSession(ctx, "192.0.2.1:1", top)
		defer clientSession.Cancel()

		_, pop := server.Testing_StallAuthRedisDials(48)
		untagged := testingUntaggedMints(SessionKindClient)
		signed, err := RenewSessionCredential(clientSession, "derived", false, nil)
		pop()
		if err != nil {
			t.Fatalf("a legacy refresh failed while Redis did not answer: %v", err)
		}
		parsed, err := ParseByJwt(ctx, signed)
		if err != nil || parsed.SessionId != nil {
			t.Fatalf("the refreshed token is not untagged: %v %v", parsed, err)
		}
		if got := testingUntaggedMints(SessionKindClient) - untagged; got != 1 {
			t.Errorf("counted %.0f untagged mints, want 1", got)
		}
		if binding := testingClientSessionBinding(ctx, t, network.networkId, *top.ClientId); binding == nil || *binding != derived {
			t.Fatalf("the client is bound to %v, want its derived lineage %s", binding, derived)
		}

		// Redis answers again: the same lineage registers, with no conflict
		refreshed := NewLocalClientSession(ctx, "192.0.2.1:1", parsed)
		defer refreshed.Cancel()
		signed, err = RenewSessionCredential(refreshed, "derived", false, nil)
		if err != nil {
			t.Fatalf("the next refresh failed: %v", err)
		}
		parsed, err = ParseByJwt(ctx, signed)
		if err != nil || parsed.SessionId == nil || *parsed.SessionId != derived {
			t.Fatalf("the next refresh carries session %v, want %s: %v", parsed, derived, err)
		}
	})
}

func testingSessionMarkerTtl(ctx context.Context, t testing.TB, networkId server.Id, sessionId server.Id) time.Duration {
	var ttl time.Duration
	if err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		ttl, err = r.PTTL(ctx, SessionMarkerKey(networkId, sessionId)).Result()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return ttl
}

// A sign-in token an hour from its expiry, its session registered for it.
func testingRegisteredSignIn(t testing.TB, actor *ClientSession) *ClientSession {
	ctx := actor.Ctx
	credential := NewByJwt(actor.ByJwt.NetworkId, actor.ByJwt.UserId, "carried", false, false)
	sid := server.NewId()
	credential.SessionId = &sid
	credential.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
	if _, err := RegisterNetworkSession(ctx, credential, "password", nil, false, server.NowUtc()); err != nil {
		t.Fatal(err)
	}
	presented := NewLocalClientSession(ctx, "192.0.2.1:1", credential)
	t.Cleanup(presented.Cancel)
	return presented
}

// The no-extend fallback: a renewal carries its presented credential's
// session, so when Redis does not register it the token is signed on the
// session without extending it, expiring with the presented token, and
// counted. Before, the renewal failed unavailable, a 503. The session's horizon
// stays where the presented token's mint put it, a revocation once Redis
// answers keeps its marker past the token's own horizon and refuses it, and
// the next refresh registers and extends the session.
func TestCarriedSessionRenewalSignsUnextendedWhenRedisDoesNotRegister(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		networkId := actor.ByJwt.NetworkId
		for _, next := range []string{"revoke", "refresh"} {
			presented := testingRegisteredSignIn(t, actor)
			sid := *presented.ByJwt.SessionId
			horizon := testingSessionScore(ctx, t, networkId, sid)

			_, pop := server.Testing_StallAuthRedisDials(48)
			unextended := testingUnextendedMints(SessionKindSignin)
			untagged := testingUntaggedMints(SessionKindSignin)
			start := time.Now()
			signed, err := RenewSessionCredential(presented, "carried", false, nil)
			elapsed := time.Since(start)
			pop()
			if err != nil {
				t.Fatalf("a carried session's renewal failed while Redis did not answer: %v", err)
			}
			// the marker in the transaction and the clock sample, a budget each
			if bound := 2*RevocationCheckBudget + time.Second; bound < elapsed {
				t.Errorf("the renewal took %s, want at most %s", elapsed, bound)
			}
			renewed, err := ParseByJwt(ctx, signed)
			if err != nil || renewed.SessionId == nil || *renewed.SessionId != sid {
				t.Fatalf("the renewed token does not carry the session %s: %v %v", sid, renewed, err)
			}
			if !renewed.ExpiresAt.Time.Equal(presented.ByJwt.ExpiresAt.Time) {
				t.Fatalf("the unextended token expires %v, want the presented token's %v", renewed.ExpiresAt, presented.ByJwt.ExpiresAt)
			}
			if got := testingUnextendedMints(SessionKindSignin) - unextended; got != 1 {
				t.Errorf("counted %.0f unextended mints, want 1", got)
			}
			if got := testingUntaggedMints(SessionKindSignin) - untagged; got != 0 {
				t.Errorf("a carried session was signed untagged %.0f times", got)
			}
			if got := testingSessionScore(ctx, t, networkId, sid); got != horizon {
				t.Fatalf("the session's horizon moved from %.0f to %.0f", horizon, got)
			}
			if float64(DeadlineMillis(renewed.AcceptUntil())) > horizon {
				t.Fatalf("the token's horizon %d passes its session's %.0f", DeadlineMillis(renewed.AcceptUntil()), horizon)
			}
			if err := ValidateByJwtState(ctx, renewed, false); err != nil {
				t.Fatalf("the unextended token is refused: %v", err)
			}

			switch next {
			case "revoke":
				op := server.NewId()
				if err := prepareSessionOperation(actor, op, "single", &sid, nil); err != nil {
					t.Fatal(err)
				}
				if _, err := EnforceSessionOperation(ctx, networkId, op); err != nil {
					t.Fatal(err)
				}
				if ttl := testingSessionMarkerTtl(ctx, t, networkId, sid); time.Until(renewed.AcceptUntil()) > ttl {
					t.Fatalf("the revocation's marker lives %s, short of the unextended token's horizon in %s", ttl, time.Until(renewed.AcceptUntil()))
				}
				if err := ValidateByJwtState(ctx, renewed, false); !errors.Is(err, ErrSessionRevoked) {
					t.Fatalf("the unextended token of a revoked session answered %v", err)
				}
			case "refresh":
				refreshed := NewLocalClientSession(ctx, "192.0.2.1:1", renewed)
				defer refreshed.Cancel()
				signed, err := RenewSessionCredential(refreshed, "carried", false, nil)
				if err != nil {
					t.Fatalf("the next refresh failed: %v", err)
				}
				extended, err := ParseByJwt(ctx, signed)
				if err != nil || extended.SessionId == nil || *extended.SessionId != sid || !extended.ExpiresAt.Time.After(renewed.ExpiresAt.Time) {
					t.Fatalf("the next refresh did not extend the session %s: %v %v", sid, extended, err)
				}
				if got := testingSessionScore(ctx, t, networkId, sid); got < float64(DeadlineMillis(extended.AcceptUntil())) {
					t.Fatalf("the next refresh left the session's horizon at %.0f, short of its token's %d", got, DeadlineMillis(extended.AcceptUntil()))
				}
			}
		}
	})
}

// A presented token past its expiry gains nothing from a token with the same
// expiry: its renewal stays unavailable while Redis does not answer, and is
// not counted, and the caller keeps the token it holds.
func TestExpiredCarriedSessionRenewalStaysUnavailable(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetRejectExpired(false)()
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		credential := NewByJwt(actor.ByJwt.NetworkId, actor.ByJwt.UserId, "expired", false, false)
		sid := server.NewId()
		credential.SessionId = &sid
		credential.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Minute))
		if _, err := RegisterNetworkSession(ctx, credential, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		presented := NewLocalClientSession(ctx, "192.0.2.1:1", credential)
		defer presented.Cancel()
		_, pop := server.Testing_StallAuthRedisDials(48)
		defer pop()
		unextended := testingUnextendedMints(SessionKindSignin)
		if _, err := RenewSessionCredential(presented, "expired", false, nil); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("an expired carried session's renewal answered %v, want unavailable", err)
		}
		if got := testingUnextendedMints(SessionKindSignin) - unextended; got != 0 {
			t.Errorf("an expired carried session was counted %.0f times", got)
		}
	})
}

// The hosted bootstrap of a proxy with no session starts one; signed
// untagged, as hosted proxies were before sessions, it leaves the proxy
// unbound for its next mint. A proxy already bound to a session needs that
// session's registration: the hosted loader presents no credential whose
// expiry would bound the token.
func TestHostedBootstrapSignsUntaggedWhenRedisDoesNotRegister(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetSessionCreationEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		proxy := network.taggedClient(ctx, nil)
		bound := network.taggedClient(ctx, nil)
		boundSession := NewByJwt(network.networkId, network.userId, "hosted", false, false)
		sid := server.NewId()
		boundSession.SessionId = &sid
		if _, err := RegisterNetworkSession(ctx, boundSession, "legacy_proxy", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET session_id=$2,session_create_time=$3 WHERE client_id=$1`, *bound.ClientId, sid, boundSession.CreateTime))
		})

		_, pop := server.Testing_StallAuthRedisDials(48)
		defer pop()
		untagged := testingUntaggedMints(SessionKindClient)
		hosted := proxy.Renew()
		signed, err := MintHostedSession(ctx, hosted)
		if err != nil || signed == "" {
			t.Fatalf("a hosted bootstrap failed while Redis did not answer: %v", err)
		}
		if hosted.SessionId != nil {
			t.Fatal("the hosted token kept a session Redis never registered")
		}
		if binding := testingClientSessionBinding(ctx, t, network.networkId, *proxy.ClientId); binding != nil {
			t.Fatalf("the proxy stays bound to the unregistered session %s", binding)
		}
		if got := testingUntaggedMints(SessionKindClient) - untagged; got != 1 {
			t.Errorf("counted %.0f untagged mints, want 1", got)
		}

		// no presented credential bounds a bound proxy's token: it is not
		// signed on its session unextended either
		unextended := testingUnextendedMints(SessionKindClient)
		if _, err := MintHostedSession(ctx, bound.Renew()); !errors.Is(err, ErrAuthUnavailable) {
			t.Fatalf("a bound proxy's mint answered %v, want unavailable", err)
		}
		if got := testingUnextendedMints(SessionKindClient) - unextended; got != 0 {
			t.Errorf("a bound proxy's mint was signed unextended %.0f times", got)
		}
	})
}

// S2 R5: with new sessions off, a renewal of a tagged credential still
// registers and extends its session before signing, so a revocation retains
// its marker through the renewed token's horizon.
func TestTaggedRenewalExtendsItsSessionWhileCreationIsOff(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		defer Testing_SetSessionCreationEnabled(false)()
		actor := recoverySessionActor(t)
		ctx := actor.Ctx
		networkId := actor.ByJwt.NetworkId
		// a sign-in token a day from its expiry
		credential := NewByJwt(networkId, actor.ByJwt.UserId, "renewal", false, false)
		sid := server.NewId()
		credential.SessionId = &sid
		credential.ExpiresAt.Time = server.NowUtc().Add(24 * time.Hour)
		if _, err := RegisterNetworkSession(ctx, credential, "password", nil, false, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		before := testingSessionScore(ctx, t, networkId, sid)
		presented := NewLocalClientSession(ctx, "192.0.2.1:1", credential)
		defer presented.Cancel()
		signed, err := RenewSessionCredential(presented, "renewal", false, nil)
		if err != nil {
			t.Fatal(err)
		}
		renewed, err := ParseByJwt(ctx, signed)
		if err != nil || renewed.SessionId == nil || *renewed.SessionId != sid {
			t.Fatalf("the renewal does not carry the session: %v %v", renewed, err)
		}
		after := testingSessionScore(ctx, t, networkId, sid)
		if after < float64(DeadlineMillis(renewed.AcceptUntil())) || after <= before {
			t.Fatalf("the renewal left the session's horizon at %.0f (was %.0f), short of the renewed token's %d", after, before, DeadlineMillis(renewed.AcceptUntil()))
		}
		op := server.NewId()
		if err := prepareSessionOperation(actor, op, "single", &sid, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := EnforceSessionOperation(ctx, networkId, op); err != nil {
			t.Fatal(err)
		}
		if err := ValidateByJwtState(ctx, renewed, false); !errors.Is(err, ErrSessionRevoked) {
			t.Fatalf("the renewed token of a revoked session answered %v", err)
		}
	})
}
