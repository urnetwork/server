// The Client-class check sockets runs (plan-SOCK-client-auth, QD-040) on the
// private stack: records are projected by the real SQL fallback, as connect's
// recheck projects them, and every check runs with Postgres refused. A valid
// token passes offline; a removal or a rotation refuses at once, in or out of
// the grace; a lost store and a miss answer unavailable with their waits.
package session

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/server"
)

// A verifier on the process keyring's exported keys and a clock the test moves.
type clientTokenTestVerifier struct {
	verifier *ClientTokenVerifier
	now      time.Time
}

func newClientTokenTestVerifier(t testing.TB, document func() ([]byte, error)) *clientTokenTestVerifier {
	t.Helper()
	if document == nil {
		document = Testing_ByJwtPublicJwks
	}
	test := &clientTokenTestVerifier{now: server.NowUtc()}
	settings := DefaultClientTokenVerifierSettings()
	settings.Keyring.Document = document
	settings.Keyring.Now = func() time.Time { return test.now }
	settings.Now = func() time.Time { return test.now }
	test.verifier = NewClientTokenVerifier(settings)
	return test
}

// Verifies signed with every Postgres acquisition in the process refused, and
// fails the test on any.
func (self *clientTokenTestVerifier) verifyWithoutSql(t testing.TB, ctx context.Context, signed string) (*ByJwt, error) {
	t.Helper()
	attempts, stop := server.DenyPostgresForTest(t)
	defer stop()
	byJwt, err := self.verifier.Verify(ctx, signed)
	if count := attempts(); count != 0 {
		t.Fatalf("the check acquired Postgres %d times", count)
	}
	return byJwt, err
}

// The client's last valid time as Redis holds it, "" when absent.
func testingClientTokenValid(ctx context.Context, t testing.TB, clientId server.Id) string {
	t.Helper()
	var value string
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		value, err = r.Get(ctx, clientTokenValidKey(clientId)).Result()
		if errors.Is(err, server.RedisNil) {
			value, err = "", nil
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// Removes one field of a client's record, as its positive ttl does.
func testingLapseRevocationField(ctx context.Context, t testing.TB, client *ByJwt, field string) {
	t.Helper()
	lineage, _ := revocationRecordLineage(client)
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.HDel(ctx, revocationRecordKey(client.NetworkId, lineage), field).Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The wait an unavailable answer names, or a failure when err is no such answer.
func clientTokenRetryAfter(t testing.TB, err error) time.Duration {
	t.Helper()
	unavailable := &ClientTokenUnavailableError{}
	if !errors.As(err, &unavailable) || errors.Is(err, ErrClientTokenRefused) {
		t.Fatalf("not unavailable: %v", err)
	}
	return unavailable.RetryAfter
}

// A projected client's token passes with no SQL, its claims intact, and the
// check records its last valid time, written again only after the write
// interval.
func TestClientTokenVerifierPassesAProjectedClientWithNoSql(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		client := network.taggedClient(ctx, nil)
		sidecarId := server.NewId()
		client.SidecarId = &sidecarId
		if err := ValidateByJwtState(ctx, client, true); err != nil {
			t.Fatal(err)
		}
		test := newClientTokenTestVerifier(t, nil)

		byJwt, err := test.verifyWithoutSql(t, ctx, client.Testing_Sign())
		if err != nil {
			t.Fatal(err)
		}
		if byJwt.NetworkId != network.networkId || *byJwt.ClientId != *client.ClientId || byJwt.SidecarId == nil || *byJwt.SidecarId != sidecarId {
			t.Fatalf("claims %+v", byJwt)
		}
		first := testingClientTokenValid(ctx, t, *client.ClientId)
		if first == "" {
			t.Fatal("a valid check wrote no last valid time")
		}

		test.now = test.now.Add(time.Second)
		if _, err := test.verifyWithoutSql(t, ctx, client.Testing_Sign()); err != nil {
			t.Fatal(err)
		}
		if again := testingClientTokenValid(ctx, t, *client.ClientId); again != first {
			t.Fatalf("written again within the interval: %s, then %s", first, again)
		}
		test.now = test.now.Add(DefaultClientTokenVerifierSettings().GraceWriteInterval)
		if _, err := test.verifyWithoutSql(t, ctx, client.Testing_Sign()); err != nil {
			t.Fatal(err)
		}
		if again := testingClientTokenValid(ctx, t, *client.ClientId); again == first {
			t.Fatal("not written again after the interval")
		}
	})
}

// A removed client's tombstone and a rotation's cutoff refuse, each with
// ErrClientTokenRefused, so sockets answers 401.
func TestClientTokenVerifierRefusesARemovedOrRotatedClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		removed := network.taggedClient(ctx, nil)
		rotated := network.taggedClient(ctx, nil)
		for _, client := range []*ByJwt{removed, rotated} {
			if err := ValidateByJwtState(ctx, client, true); err != nil {
				t.Fatal(err)
			}
		}
		test := newClientTokenTestVerifier(t, nil)
		for _, client := range []*ByJwt{removed, rotated} {
			if _, err := test.verifyWithoutSql(t, ctx, client.Testing_Sign()); err != nil {
				t.Fatal(err)
			}
		}

		network.deactivate(ctx, removed)
		if _, err := test.verifyWithoutSql(t, ctx, removed.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("a removed client: %v", err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(RotateCredentialRecordsInTx(ctx, tx, network.userId, rotated.CreateTime.Add(time.Second)))
		})
		if _, err := test.verifyWithoutSql(t, ctx, rotated.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("a rotated credential: %v", err)
		}
	})
}

// A lost store answers unavailable with UnavailableRetryAfter, and a record
// that cannot answer, outside any grace, with MissRetryAfter; neither is a
// refusal, and neither reaches SQL.
func TestClientTokenVerifierUnavailableOnStoreLossAndMiss(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		client := network.taggedClient(ctx, nil)
		if err := ValidateByJwtState(ctx, client, true); err != nil {
			t.Fatal(err)
		}
		test := newClientTokenTestVerifier(t, nil)
		settings := DefaultClientTokenVerifierSettings()

		func() {
			defer Testing_FailClientTokenRecordReads(errors.New("synthetic redis loss"))()
			_, err := test.verifyWithoutSql(t, ctx, client.Testing_Sign())
			if retryAfter := clientTokenRetryAfter(t, err); retryAfter != settings.UnavailableRetryAfter {
				t.Fatalf("a lost store asked for %s", retryAfter)
			}
		}()

		// a client this check never found valid, whose record is gone
		testingDropRevocationRecords(ctx, t, network.networkId)
		_, err := test.verifyWithoutSql(t, ctx, client.Testing_Sign())
		if retryAfter := clientTokenRetryAfter(t, err); retryAfter != settings.MissRetryAfter {
			t.Fatalf("a miss asked for %s", retryAfter)
		}
		if testingClientTokenValid(ctx, t, *client.ClientId) != "" {
			t.Fatal("a miss wrote a last valid time")
		}
	})
}

// Checks each client valid, then lapses its own record field, as its positive
// ttl does, leaving the rest of the record.
func testingValidThenLapsed(t testing.TB, ctx context.Context, test *clientTokenTestVerifier, clients ...*ByJwt) {
	t.Helper()
	for _, client := range clients {
		if err := ValidateByJwtState(ctx, client, true); err != nil {
			t.Fatal(err)
		}
		if _, err := test.verifyWithoutSql(t, ctx, client.Testing_Sign()); err != nil {
			t.Fatal(err)
		}
		testingLapseRevocationField(ctx, t, client, revocationClientField(*client.ClientId))
	}
}

// A miss within ValidGrace of the last valid check passes and does not move it;
// past the grace it answers unavailable. A removal of the client's root in the
// grace refuses its child, whose own field has lapsed.
func TestClientTokenVerifierGraceCoversAMissButNoRemoval(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		lapsed := network.taggedClient(ctx, nil)
		root := network.taggedClient(ctx, nil)
		child := network.taggedClient(ctx, root)
		test := newClientTokenTestVerifier(t, nil)
		settings := DefaultClientTokenVerifierSettings()
		testingValidThenLapsed(t, ctx, test, lapsed, child)

		validTime := testingClientTokenValid(ctx, t, *lapsed.ClientId)
		test.now = test.now.Add(settings.ValidGrace - time.Second)
		if _, err := test.verifyWithoutSql(t, ctx, lapsed.Testing_Sign()); err != nil {
			t.Fatalf("a miss within the grace: %v", err)
		}
		if moved := testingClientTokenValid(ctx, t, *lapsed.ClientId); moved != validTime {
			t.Fatalf("the grace moved the last valid time: %s, then %s", validTime, moved)
		}

		// within the same grace, the removal refuses
		network.deactivate(ctx, &ByJwt{ClientId: root.ClientId})
		if _, err := test.verifyWithoutSql(t, ctx, child.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) {
			t.Fatalf("a child whose root was removed, its own field lapsed: %v", err)
		}

		test.now = test.now.Add(time.Second)
		_, err := test.verifyWithoutSql(t, ctx, lapsed.Testing_Sign())
		if retryAfter := clientTokenRetryAfter(t, err); retryAfter != settings.MissRetryAfter {
			t.Fatalf("past the grace a miss asked for %s", retryAfter)
		}
	})
}

// A rotation in the grace refuses a client whose own field has lapsed: the
// network's credential cutoff stays on the record.
func TestClientTokenVerifierGraceCoversNoRotation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		rotated := network.taggedClient(ctx, nil)
		test := newClientTokenTestVerifier(t, nil)
		testingValidThenLapsed(t, ctx, test, rotated)
		test.now = test.now.Add(time.Second)
		if _, err := test.verifyWithoutSql(t, ctx, rotated.Testing_Sign()); err != nil {
			t.Fatalf("a miss within the grace: %v", err)
		}

		server.Tx(ctx, func(tx server.PgTx) {
			server.Raise(RotateCredentialRecordsInTx(ctx, tx, network.userId, rotated.CreateTime.Add(time.Second)))
		})
		if _, err := test.verifyWithoutSql(t, ctx, rotated.Testing_Sign()); !errors.Is(err, ErrClientTokenRefused) || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("a rotation while the client's field lapsed: %v", err)
		}
	})
}

// What the check cannot vouch for: a token with no root claim, one past its exp
// or with none, a forgery under a held kid, each refused; a key the block was
// not given, a member's credential and a record not yet enabled, each
// unavailable. None reaches SQL.
func TestClientTokenVerifierAnswersWhatItCannotVouchFor(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newTestingStateRefusalNetwork(ctx)
		client := network.taggedClient(ctx, nil)
		if err := ValidateByJwtState(ctx, client, true); err != nil {
			t.Fatal(err)
		}
		test := newClientTokenTestVerifier(t, nil)

		legacy := network.client(ctx, nil, true)
		noExp := *client
		noExp.ExpiresAt = nil
		expired := *client
		expired.ExpiresAt = gojwt.NewNumericDate(test.now.Add(-time.Minute))
		foreignKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		held := client.Testing_Sign()
		forged := signByJwtWithKeyAndKid(t, foreignKey, byJwtTokenKid(t, held))
		for name, signed := range map[string]string{
			"no root claim": legacy.Testing_Sign(),
			"no exp":        noExp.Testing_Sign(),
			"past its exp":  expired.Testing_Sign(),
			"a forgery":     forged,
			"no token":      "",
		} {
			if _, err := test.verifyWithoutSql(t, ctx, signed); !errors.Is(err, ErrClientTokenRefused) {
				t.Errorf("%s: %v", name, err)
			}
		}

		member := *client
		memberId := server.NewId()
		member.MemberId = &memberId
		_, err = test.verifyWithoutSql(t, ctx, member.Testing_Sign())
		if retryAfter := clientTokenRetryAfter(t, err); retryAfter != DefaultClientTokenVerifierSettings().MissRetryAfter {
			t.Fatalf("a member's credential asked for %s", retryAfter)
		}

		otherDocument, err := json.Marshal(&jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       &foreignKey.PublicKey,
			KeyID:     byJwtTestKid(t, foreignKey),
			Algorithm: "ES256",
			Use:       "sig",
		}}})
		if err != nil {
			t.Fatal(err)
		}
		otherBlock := newClientTokenTestVerifier(t, func() ([]byte, error) { return otherDocument, nil })
		_, err = otherBlock.verifyWithoutSql(t, ctx, held)
		clientTokenRetryAfter(t, err)

		func() {
			defer Testing_SetRevocationRecordEnabled(false)()
			_, err := test.verifyWithoutSql(t, ctx, held)
			clientTokenRetryAfter(t, err)
			if test.verifier.Check() == nil {
				t.Fatal("a verifier whose record is not enabled checks as ready")
			}
		}()
		if err := test.verifier.Check(); err != nil {
			t.Fatal(err)
		}
	})
}
