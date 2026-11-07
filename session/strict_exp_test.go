package session

import (
	"context"
	"errors"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// D-TEAM-14: a strict_exp token lasts 7 days, keeps its mark through every
// derivation, and ends at its exp whatever reject_expired or AcceptUntil say
// once sidecar.yml's p0_enforce is on. Pure: tokens are signed with the local
// vault key, and every refusal here comes before any store is read.

const strictExpP0EnforceYaml = "sidecar:\n  p0_enforce: {enabled: true}\n"

func newStrictExpTestClient(t testing.TB) *ByJwt {
	t.Helper()
	network := NewByJwt(server.NewId(), server.NewId(), "strict-test", false, false)
	client := network.Client(server.NewId(), server.NewId())
	client.SetStrictExp(true)
	return client
}

// Moves the token's exp, which NewByJwt set from now.
func withStrictExpExpiry(byJwt *ByJwt, exp time.Time) *ByJwt {
	byJwt.ExpiresAt = gojwt.NewNumericDate(exp)
	byJwt.IssuedAt = gojwt.NewNumericDate(exp.Add(-7 * 24 * time.Hour))
	byJwt.NotBefore = byJwt.IssuedAt
	return byJwt
}

func requireExpiryCloseTo(t testing.TB, byJwt *ByJwt, lifetime time.Duration) {
	t.Helper()
	if d := byJwt.ExpiresAt.Time.Sub(server.NowUtc().Add(lifetime)); d < -time.Minute || time.Minute < d {
		t.Fatalf("exp = %s, want %s from now", byJwt.ExpiresAt.Time, lifetime)
	}
}

// The mark sets a 7-day exp and survives every way a credential is derived and
// signed. Nothing else gets it, which is what keeps it to sidecar-held tokens
// (owner decision 131): an ordinary client token keeps its 30 days.
func TestStrictExpClaimIsKeptAndSetsTheLifetime(t *testing.T) {
	strict := newStrictExpTestClient(t)
	requireExpiryCloseTo(t, strict, 7*24*time.Hour)
	connect.AssertEqual(t, strict.RootClientId != nil, true)

	renewed := strict.Renew()
	connect.AssertEqual(t, renewed.StrictExp, true)
	requireExpiryCloseTo(t, renewed, 7*24*time.Hour)

	child := strict.Client(server.NewId(), server.NewId())
	connect.AssertEqual(t, child.StrictExp, true)
	requireExpiryCloseTo(t, child, 7*24*time.Hour)
	connect.AssertEqual(t, *child.RootClientId, *strict.RootClientId)

	signed := strict.Testing_Sign()
	connect.AssertEqual(t, ByJwtStrictExpUnverified(signed), true)
	parsed, err := ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceApi)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.StrictExp, true)

	ordinary := NewByJwt(server.NewId(), server.NewId(), "strict-test", false, false).Client(server.NewId(), server.NewId())
	connect.AssertEqual(t, ordinary.StrictExp, false)
	requireExpiryCloseTo(t, ordinary, 30*24*time.Hour)
	requireExpiryCloseTo(t, ordinary.Renew(), 30*24*time.Hour)
	connect.AssertEqual(t, ByJwtStrictExpUnverified(ordinary.Testing_Sign()), false)
	connect.AssertEqual(t, ByJwtStrictExpUnverified("not a token"), false)

	// and a mint that learns a client's mark late, or never, gets the lifetime
	// that matches
	strict.SetStrictExp(false)
	requireExpiryCloseTo(t, strict, 30*24*time.Hour)
}

// Past its exp a strict_exp token is refused at both audiences with reject_expired
// off, the state before the fleet flips it, and inside AcceptUntil's 60 days.
func TestExpiredStrictExpTokenIsRefusedWithRejectExpiredOff(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	defer server.Testing_PushSidecarConfig(strictExpP0EnforceYaml)()

	expired := withStrictExpExpiry(newStrictExpTestClient(t), server.NowUtc().Add(-time.Hour))
	if !server.NowUtc().Before(expired.AcceptUntil()) {
		t.Fatal("the fixture should sit inside AcceptUntil")
	}
	signed := expired.Testing_Sign()
	for _, audience := range []string{ByJwtAudienceApi, ByJwtAudienceConnect} {
		before := testutil.ToFloat64(authRejectionCounter.WithLabelValues(string(AuthRejectionExpired)))
		if _, err := ParseByJwtForAudience(context.Background(), signed, audience); err == nil {
			t.Fatalf("%s: an expired strict_exp token verified", audience)
		}
		connect.AssertEqual(t, testutil.ToFloat64(authRejectionCounter.WithLabelValues(string(AuthRejectionExpired))), before+1)
	}

	// the same token without the mark passes today, as the gate's rollout needs
	ordinary := withStrictExpExpiry(newStrictExpTestClient(t), server.NowUtc().Add(-time.Hour))
	ordinary.StrictExp = false
	_, err := ParseByJwtForAudience(context.Background(), ordinary.Testing_Sign(), ByJwtAudienceApi)
	connect.AssertEqual(t, err, nil)
}

// Owner decision 103 grants no grace, so the clock leeway other tokens get is not
// a strict_exp token's.
func TestStrictExpTokenHasNoGrace(t *testing.T) {
	defer server.Testing_PushSidecarConfig(strictExpP0EnforceYaml)()

	byJwt := newStrictExpTestClient(t)
	exp := byJwt.ExpiresAt.Time
	connect.AssertEqual(t, validateStrictExp(byJwt, exp.Add(-time.Second)), nil)
	if err := validateStrictExp(byJwt, exp); err == nil {
		t.Fatal("a strict_exp token verified at its exp")
	}
	if err := validateStrictExp(byJwt, exp.Add(time.Second)); err == nil {
		t.Fatal("a strict_exp token verified inside the clock leeway")
	}
}

// Log-only: until p0_enforce is on, a passed exp is accepted and counted as what
// it is, so the first days of D-TEAM-14 cost nothing and show everything.
func TestExpiredStrictExpTokenIsCountedWhileLogOnly(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	defer server.Testing_PushSidecarConfig("")()

	signed := withStrictExpExpiry(newStrictExpTestClient(t), server.NowUtc().Add(-time.Hour)).Testing_Sign()
	counter := authLegacyAcceptCounter.WithLabelValues(string(AuthLegacyStrictExpired), string(AuthCredentialClient))
	before := testutil.ToFloat64(counter)
	parsed, err := ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceConnect)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.StrictExp, true)
	connect.AssertEqual(t, testutil.ToFloat64(counter), before+1)

	// reject_expired needs no gate: once it is on it refuses this token too
	defer Testing_SetRejectExpired(true)()
	if _, err := ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceApi); err == nil {
		t.Fatal("reject_expired let an expired strict_exp token through")
	}
}

// Enforcement needs only the file: the same binary refuses after a config push.
func TestStrictExpEnforcementFollowsTheConfig(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	signed := withStrictExpExpiry(newStrictExpTestClient(t), server.NowUtc().Add(-time.Hour)).Testing_Sign()

	popLogOnly := server.Testing_PushSidecarConfig("sidecar:\n  p0_enforce: {enabled: false}\n")
	_, err := ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceApi)
	connect.AssertEqual(t, err, nil)
	popLogOnly()

	popEnforce := server.Testing_PushSidecarConfig(strictExpP0EnforceYaml)
	defer popEnforce()
	if _, err := ParseByJwtForAudience(context.Background(), signed, ByJwtAudienceApi); err == nil {
		t.Fatal("p0_enforce did not refuse the token")
	}
}

// exp and the root claim are part of what a strict_exp token is, so one signed
// without them, which no mint does, is refused in every mode.
func TestStrictExpTokenNeedsExpAndItsRoot(t *testing.T) {
	defer Testing_SetRejectMissingExpiration(false)()
	defer server.Testing_PushSidecarConfig("")()

	noExp := newStrictExpTestClient(t)
	noExp.ExpiresAt = nil
	noRoot := newStrictExpTestClient(t)
	noRoot.RootClientId = nil
	noClient := NewByJwt(server.NewId(), server.NewId(), "strict-test", false, false)
	noClient.StrictExp = true
	for name, byJwt := range map[string]*ByJwt{"no exp": noExp, "no root": noRoot, "no client": noClient} {
		if _, err := ParseByJwtForAudience(context.Background(), byJwt.Testing_Sign(), ByJwtAudienceApi); err == nil {
			t.Fatalf("%s: verified", name)
		}
	}
}

// A connection outlives its parse, so the end of a strict_exp token is read again
// wherever state is checked, which a lease's recheck does: it retires the
// transport for a definitive refusal, without reading a store.
func TestStateCheckRefusesAnExpiredStrictExpToken(t *testing.T) {
	defer server.Testing_PushSidecarConfig(strictExpP0EnforceYaml)()

	expired := withStrictExpExpiry(newStrictExpTestClient(t), server.NowUtc().Add(-time.Hour))
	err := ValidateByJwtState(context.Background(), expired, true)
	if err == nil {
		t.Fatal("the state check passed an expired strict_exp token")
	}
	if !authorizationRejectionDefinitive(err) {
		t.Fatalf("err = %v, want a definitive refusal", err)
	}
	var rejection *byJwtRejection
	connect.AssertEqual(t, errors.As(err, &rejection), true)
}

// A transport admitted on a strict_exp token ends at its exp, not at the 60-day
// horizon of its session claims.
func TestAuthorizationLeaseEndsAtAStrictExpTokensExp(t *testing.T) {
	byJwt := newStrictExpTestClient(t)
	check := func(ctx context.Context, credential *ByJwt) error { return nil }
	start := func() *AuthorizationLease {
		lease, err := startAuthorizationLeaseWithCheck(context.Background(), byJwt, func(int) {}, nil, check)
		connect.AssertEqual(t, err, nil)
		t.Cleanup(lease.Close)
		return lease
	}

	func() {
		defer server.Testing_PushSidecarConfig(strictExpP0EnforceYaml)()
		connect.AssertEqual(t, start().terminal.Equal(byJwt.ExpiresAt.Time), true)
	}()

	// log-only keeps the horizon every client token has
	defer server.Testing_PushSidecarConfig("")()
	connect.AssertEqual(t, start().terminal.Equal(byJwt.AcceptUntil()), true)
}
