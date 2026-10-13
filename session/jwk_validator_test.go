package session

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-jose/go-jose/v3"
	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The Google and Apple validators differ only by their key set url. A local
// server stands in for both, so the fetch and parse run without the network.
func TestJwkValidatorLoadsServedKeySet(t *testing.T) {
	key := testingSsoKey(t)
	keySetBytes, err := json.Marshal(jose.JSONWebKeySet{
		Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "synthetic", Algorithm: "RS256", Use: "sig"}},
	})
	connect.AssertEqual(t, err, nil)

	var requestsLock sync.Mutex
	var requests []string
	keySetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestsLock.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		requestsLock.Unlock()
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(keySetBytes)
	}))
	defer keySetServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	keys := NewJwkValidator(ctx, "synthetic", keySetServer.URL+"/oauth2/v3/certs").Keys()
	connect.AssertEqual(t, len(keys), 1)
	publicKey, ok := keys[0].(*rsa.PublicKey)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, publicKey.Equal(&key.PublicKey), true)

	requestsLock.Lock()
	defer requestsLock.Unlock()
	connect.AssertEqual(t, requests, []string{"GET /oauth2/v3/certs"})
}

func TestSsoJwkValidatorsUseProviderKeySets(t *testing.T) {
	// a cancelled context ends the refresh before its first fetch
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	connect.AssertEqual(t, NewGoogleJwkValidator(ctx).jwkUrl, "https://www.googleapis.com/oauth2/v3/certs")
	connect.AssertEqual(t, NewAppleJwkValidator(ctx).jwkUrl, "https://appleid.apple.com/auth/keys")
}

// Tests in other packages verify sso tokens against a fixed key set, so they
// never start the live key set fetch.
func TestSetSsoJwkKeysVerifiesWithoutTheLiveKeySets(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		key := testingSsoKey(t)
		pop := Testing_SetSsoJwkKeys([]any{&key.PublicKey})

		googleJwt, err := ParseGoogleJwt(testingSsoSignJwt(t, gojwt.SigningMethodRS256, key, testingGoogleClaims()))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, googleJwt.UserAuth, "xcolwell@gmail.com")
		appleJwt, err := ParseAppleJwt(testingSsoSignJwt(t, gojwt.SigningMethodRS256, key, testingAppleClaims()))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, appleJwt.UserAuth, "xcolwell@gmail.com")

		// a token signed by any other key is refused as with the live keys
		otherKey := testingSsoKey(t)
		_, err = ParseGoogleJwt(testingSsoSignJwt(t, gojwt.SigningMethodRS256, otherKey, testingGoogleClaims()))
		connect.AssertEqual(t, err.Error(), "Could not verify signed token.")
		_, err = ParseAppleJwt(testingSsoSignJwt(t, gojwt.SigningMethodRS256, otherKey, testingAppleClaims()))
		connect.AssertEqual(t, err.Error(), "Could not verify signed token.")

		// nested overrides restore in order
		popEmpty := Testing_SetSsoJwkKeys([]any{})
		_, err = ParseGoogleJwt(testingSsoSignJwt(t, gojwt.SigningMethodRS256, key, testingGoogleClaims()))
		connect.AssertEqual(t, err.Error(), "Could not verify signed token.")
		popEmpty()
		connect.AssertEqual(t, *ssoJwkKeysOverride.Load(), []any{&key.PublicKey})
		pop()
		connect.AssertEqual(t, ssoJwkKeysOverride.Load() == nil, true)
	})
}
