// The verified claims cache (by_jwt_verified_cache.go) against the local
// stores. A token whose verification is remembered is refused exactly as
// without the cache once its session is revoked, its client deactivated or its
// account's credentials rotated: every state and revocation check runs on every
// request, and only the signature and the decode are reused.
package session

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A network as newTestingStateRefusalNetwork makes one, named after its id so
// a test can make several.
func newByJwtTestNetwork(ctx context.Context) *testingStateRefusalNetwork {
	network := &testingStateRefusalNetwork{networkId: server.NewId(), userId: server.NewId(), deviceId: server.NewId()}
	name := "verified-cache-" + network.networkId.String()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_user(user_id,user_name,auth_type,verified) VALUES($1,$2,'password',true)`, network.userId, name))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network(network_id,network_name,admin_user_id) VALUES($1,$2,$3)`, network.networkId, name, network.userId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO device(device_id,network_id,device_name,device_spec) VALUES($1,$2,'synthetic','synthetic')`, network.deviceId, network.networkId))
	})
	return network
}

// Two keyrings over the process's vault keys: one that remembers
// verifications, as the process keyring does, and one that does not.
func newByJwtTestVaultKeyrings() (cachedKeyring *byJwtKeyring, uncachedKeyring *byJwtKeyring) {
	uncachedSettings := defaultByJwtKeyringSettings()
	uncachedSettings.verifiedCacheMaxEntryCount = 0
	return newByJwtKeyring(defaultByJwtKeyringSettings()), newByJwtKeyring(uncachedSettings)
}

// The decision a request with signed gets: the token parse, then the state
// checks, as the api's authentication makes them.
func byJwtTestAuthorize(ctx context.Context, keyring *byJwtKeyring, signed string, requireClient bool) error {
	byJwt, err := parseByJwtForAudience(ctx, keyring, signed, ByJwtAudienceApi)
	if err != nil {
		return err
	}
	return ValidateByJwtState(ctx, byJwt, requireClient)
}

func assertByJwtTestSameDecision(t testing.TB, description string, cachedErr error, uncachedErr error, want error) {
	t.Helper()
	if !errors.Is(cachedErr, want) || !errors.Is(uncachedErr, want) {
		t.Fatalf("%s: cached %v, uncached %v, want %v", description, cachedErr, uncachedErr, want)
	}
	if cachedErr != nil && cachedErr.Error() != uncachedErr.Error() {
		t.Fatalf("%s: cached %v, uncached %v", description, cachedErr, uncachedErr)
	}
}

// A revoked session's token is refused on its next request, read from the
// revocation record or from the session's revocation marker, although its
// verification is remembered.
func TestByJwtVerifiedCacheRevokedSessionIsRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, recordEnabled := range []bool{true, false} {
			func() {
				defer Testing_SetRevocationRecordEnabled(recordEnabled)()
				ctx := WithStateQuerySource(t.Context(), StateQueryApiControl)
				network := newByJwtTestNetwork(ctx)
				account := network.sessionCredential()
				if _, err := RegisterNetworkSession(ctx, account, "password", nil, false, server.NowUtc()); err != nil {
					t.Fatal(err)
				}
				signed := account.Testing_Sign()
				cachedKeyring, uncachedKeyring := newByJwtTestVaultKeyrings()
				for attempt := range 2 {
					assertByJwtTestSameDecision(t, "before the revocation", byJwtTestAuthorize(ctx, cachedKeyring, signed, false), byJwtTestAuthorize(ctx, uncachedKeyring, signed, false), nil)
					if hitCount := cachedKeyring.verifiedCache.hitCount.Load(); hitCount != uint64(attempt) {
						t.Fatalf("record=%t attempt %d: %d hits", recordEnabled, attempt, hitCount)
					}
				}

				if recordEnabled {
					if err := RevokeSessionRecords(ctx, network.networkId, []server.Id{*account.SessionId}); err != nil {
						t.Fatal(err)
					}
				} else if _, err := enforceSessionRevoke(ctx, network.networkId, server.NewId(), "single", account.SessionId, nil); err != nil {
					t.Fatal(err)
				}
				hitCount := cachedKeyring.verifiedCache.hitCount.Load()
				assertByJwtTestSameDecision(t, "after the revocation", byJwtTestAuthorize(ctx, cachedKeyring, signed, false), byJwtTestAuthorize(ctx, uncachedKeyring, signed, false), ErrSessionRevoked)
				if cachedKeyring.verifiedCache.hitCount.Load() != hitCount+1 {
					t.Fatalf("record=%t: the refused request did not reuse the verification", recordEnabled)
				}
			}()
		}
	})
}

// A deactivated client's token and a token older than its account's
// credential rotation are refused on their next request, although their
// verification is remembered.
func TestByJwtVerifiedCacheClientStateIsCheckedOnEveryRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		for _, recordEnabled := range []bool{true, false} {
			func() {
				defer Testing_SetRevocationRecordEnabled(recordEnabled)()
				ctx := WithStateQuerySource(t.Context(), StateQueryApiControl)
				cachedKeyring, uncachedKeyring := newByJwtTestVaultKeyrings()

				network := newByJwtTestNetwork(ctx)
				client := network.taggedClient(ctx, nil)
				clientSigned := client.Testing_Sign()
				for range 2 {
					assertByJwtTestSameDecision(t, "active client", byJwtTestAuthorize(ctx, cachedKeyring, clientSigned, true), byJwtTestAuthorize(ctx, uncachedKeyring, clientSigned, true), nil)
				}
				network.deactivate(ctx, client)
				assertByJwtTestSameDecision(t, "deactivated client", byJwtTestAuthorize(ctx, cachedKeyring, clientSigned, true), byJwtTestAuthorize(ctx, uncachedKeyring, clientSigned, true), ErrByJwtInactive)

				rotatedNetwork := newByJwtTestNetwork(ctx)
				rotated := rotatedNetwork.taggedClient(ctx, nil)
				rotatedSigned := rotated.Testing_Sign()
				for range 2 {
					assertByJwtTestSameDecision(t, "before the rotation", byJwtTestAuthorize(ctx, cachedKeyring, rotatedSigned, true), byJwtTestAuthorize(ctx, uncachedKeyring, rotatedSigned, true), nil)
				}
				cutoff := rotated.CreateTime.Add(time.Second)
				server.Tx(ctx, func(tx server.PgTx) {
					server.Raise(RotateCredentialRecordsInTx(ctx, tx, rotatedNetwork.userId, cutoff))
					server.RaisePgResult(tx.Exec(ctx, `UPDATE network_user SET credential_change_time=$2 WHERE user_id=$1`, rotatedNetwork.userId, cutoff))
				})
				assertByJwtTestSameDecision(t, "after the rotation", byJwtTestAuthorize(ctx, cachedKeyring, rotatedSigned, true), byJwtTestAuthorize(ctx, uncachedKeyring, rotatedSigned, true), ErrByJwtInactive)

				// every request after the first of each token reused its verification
				if hitCount := cachedKeyring.verifiedCache.hitCount.Load(); hitCount != 4 {
					t.Fatalf("record=%t: %d hits, want 4", recordEnabled, hitCount)
				}
			}()
		}
	})
}

// The api's own authentication (ClientSession.AuthUnlessAnswered) through the
// process keyring: the second request with a client token reuses its
// verification, and the request after the client is deactivated is refused.
func TestByJwtVerifiedCacheClientSessionRefusesDeactivatedClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		defer Testing_SetRevocationRecordEnabled(true)()
		ctx := t.Context()
		network := newByJwtTestNetwork(ctx)
		top := network.taggedClient(ctx, nil)
		signed := top.Testing_Sign()
		authenticate := func() (bool, error) {
			request, err := http.NewRequest(http.MethodGet, "https://api.example.test/", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+signed)
			clientSession := NewLocalClientSession(ctx, "127.0.0.1:1", nil)
			defer clientSession.Cancel()
			return clientSession.AuthUnlessAnswered(request, func(context.Context, server.Id) bool { return false })
		}
		cache := defaultByJwtKeyring.verifiedCache
		hitCount := cache.hitCount.Load()
		for attempt := range 2 {
			if answered, err := authenticate(); answered || err != nil {
				t.Fatalf("attempt %d: answered %v, %v", attempt, answered, err)
			}
		}
		if hits := cache.hitCount.Load() - hitCount; hits != 1 {
			t.Fatalf("two requests reused the verification %d times, want 1", hits)
		}
		if defaultByJwtKeyring.verifiedClaims(signed) == nil {
			t.Fatal("the process keyring does not remember the token")
		}
		network.deactivate(ctx, top)
		if answered, err := authenticate(); answered || !errors.Is(err, ErrByJwtInactive) {
			t.Fatalf("a deactivated client: answered %v, %v", answered, err)
		}
		if hits := cache.hitCount.Load() - hitCount; hits != 2 {
			t.Fatalf("the refused request did not reuse the verification: %d hits", hits)
		}
	})
}
