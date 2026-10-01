package jwt

import (
	"context"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// Credential rejections are driven entirely by client-supplied input, so they
// must not write a log line at the default verbosity — a retry loop or a
// hostile caller would otherwise spam the logs at will. The counter is the
// replacement signal, so these tests pin the contract the observability now
// depends on: every rejection path increments exactly one bounded cause.

func rejectionCount(cause AuthRejectionCause) float64 {
	return testutil.ToFloat64(authRejectionCounter.WithLabelValues(string(cause)))
}

func legacyAcceptCount(cause AuthLegacyAcceptCause) float64 {
	return testutil.ToFloat64(authLegacyAcceptCounter.WithLabelValues(string(cause)))
}

// TestAuthRejectionCountersByCause walks every rejection path and asserts the
// matching cause counter advances by exactly one.
func TestAuthRejectionCountersByCause(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// assertRejects runs reject and requires both that it failed and that
		// only the expected cause counter moved
		assertRejects := func(name string, cause AuthRejectionCause, reject func() error) {
			before := rejectionCount(cause)
			if err := reject(); err == nil {
				t.Fatalf("%s: expected a rejection", name)
			}
			if after := rejectionCount(cause); after != before+1 {
				t.Fatalf("%s: counter %s = %v, want %v", name, cause, after, before+1)
			}
		}

		freshClaims := func() *ByJwt {
			return NewByJwt(server.NewId(), server.NewId(), "test", false, false)
		}

		popMissing := Testing_SetRejectMissingExpiration(false)
		defer popMissing()
		popExpired := Testing_SetRejectExpired(true)
		defer popExpired()

		assertRejects("garbage token", AuthRejectionSignature, func() error {
			_, err := ParseByJwt(ctx, "not.a.token")
			return err
		})

		assertRejects("expired", AuthRejectionExpired, func() error {
			claims := freshClaims()
			claims.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
			_, err := ParseByJwt(ctx, claims.Sign())
			return err
		})

		assertRejects("future not before", AuthRejectionNotYetValid, func() error {
			claims := freshClaims()
			claims.NotBefore = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
			_, err := ParseByJwt(ctx, claims.Sign())
			return err
		})

		assertRejects("future issued at", AuthRejectionUsedBeforeIssued, func() error {
			claims := freshClaims()
			claims.IssuedAt = gojwt.NewNumericDate(server.NowUtc().Add(time.Hour))
			_, err := ParseByJwt(ctx, claims.Sign())
			return err
		})

		assertRejects("wrong issuer", AuthRejectionInvalidClaims, func() error {
			claims := freshClaims()
			claims.Issuer = "attacker"
			_, err := ParseByJwt(ctx, claims.Sign())
			return err
		})

		assertRejects("wrong audience", AuthRejectionInvalidClaims, func() error {
			claims := freshClaims()
			claims.Audience = gojwt.ClaimStrings{"attacker"}
			_, err := ParseByJwt(ctx, claims.Sign())
			return err
		})

		// with the strict gate on, an absent claim is its own cause
		popStrict := Testing_SetRejectMissingExpiration(true)
		assertRejects("missing expiration", AuthRejectionMissingClaims, func() error {
			claims := freshClaims()
			claims.ExpiresAt = nil
			_, err := ParseByJwt(ctx, claims.Sign())
			return err
		})
		popStrict()

		// state rejections
		assertRejects("nil token", AuthRejectionMissingToken, func() error {
			return ValidateByJwtState(ctx, nil, false)
		})

		assertRejects("client required", AuthRejectionClientRequired, func() error {
			return ValidateByJwtState(ctx, freshClaims(), true)
		})

		assertRejects("no active row", AuthRejectionNoActiveRow, func() error {
			// a well-formed credential for a network that does not exist
			return ValidateByJwtState(ctx, freshClaims(), false)
		})
	})
}

// TestAuthCredentialRotatedCounter covers the state rejection that needs real
// rows: a token minted before the user's credential_change_time.
func TestAuthCredentialRotatedCounter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "rotated-counter"
		// the rows are inserted directly: the jwt package cannot import model
		// (model's own tests import jwt, so the edge would be a cycle)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_user (user_id, user_name, auth_type, verified)
					VALUES ($1, $2, 'password', true)
				`,
				userId,
				"test",
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network (network_id, network_name, admin_user_id)
					VALUES ($1, $2, $3)
				`,
				networkId,
				networkName,
				userId,
			))
		})

		byJwt := NewByJwt(networkId, userId, networkName, false, false)
		connect.AssertEqual(t, ValidateByJwtState(ctx, byJwt, false), nil)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_user SET credential_change_time = $2 WHERE user_id = $1`,
				userId,
				server.NowUtc().Add(time.Minute),
			))
		})

		before := rejectionCount(AuthRejectionCredentialRotated)
		if err := ValidateByJwtState(ctx, byJwt, false); err == nil {
			t.Fatal("expected the rotated credential to be rejected")
		}
		if after := rejectionCount(AuthRejectionCredentialRotated); after != before+1 {
			t.Fatalf("counter %s = %v, want %v", AuthRejectionCredentialRotated, after, before+1)
		}
	})
}

// TestAuthLegacyAcceptCounters covers the gate-flip signal: a credential
// accepted only because a migration gate is off increments its cause, so the
// runbook can watch the counter reach zero instead of grepping logs.
func TestAuthLegacyAcceptCounters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		popMissing := Testing_SetRejectMissingExpiration(false)
		defer popMissing()
		popExpired := Testing_SetRejectExpired(false)
		defer popExpired()

		beforeMissing := legacyAcceptCount(AuthLegacyMissingExpiration)
		legacy := &ByJwt{
			NetworkId:   server.NewId(),
			UserId:      server.NewId(),
			NetworkName: "test",
			CreateTime:  server.CodecTime(server.NowUtc()),
		}
		_, err := ParseByJwt(ctx, legacy.Sign())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, legacyAcceptCount(AuthLegacyMissingExpiration), beforeMissing+1)

		beforeExpired := legacyAcceptCount(AuthLegacyExpired)
		expired := NewByJwt(server.NewId(), server.NewId(), "test", false, false)
		expired.ExpiresAt = gojwt.NewNumericDate(server.NowUtc().Add(-time.Hour))
		_, err = ParseByJwt(ctx, expired.Sign())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, legacyAcceptCount(AuthLegacyExpired), beforeExpired+1)

		// a current credential is not counted as a legacy accept in either bucket
		nowMissing := legacyAcceptCount(AuthLegacyMissingExpiration)
		nowExpired := legacyAcceptCount(AuthLegacyExpired)
		_, err = ParseByJwt(ctx, NewByJwt(server.NewId(), server.NewId(), "test", false, false).Sign())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, legacyAcceptCount(AuthLegacyMissingExpiration), nowMissing)
		connect.AssertEqual(t, legacyAcceptCount(AuthLegacyExpired), nowExpired)
	})
}
