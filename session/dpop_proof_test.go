package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The ClientKey proof (dpop_proof.go; §7.4, Q-CORE-4; §22.5's §7 row). Pure but
// for the tests of the replay store: nothing here reads the database.

const dpopTestToken = "header.claims.signature"

const dpopTestUrl = "https://api.example/auth/refresh"

func newDpopTestKey(t testing.TB) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return publicKey, privateKey
}

func newDpopTestRequest(method string, target string) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r
}

func dpopTestSettings() *DpopProofSettings {
	return &DpopProofSettings{
		IatWindow:    60 * time.Second,
		ReplayTtl:    120 * time.Second,
		MaxJtiLength: 16,
	}
}

func requireDpopRefused(t testing.TB, name string, err error) {
	t.Helper()
	if !errors.Is(err, ErrDpopProofRefused) {
		t.Fatalf("%s: err = %v, want a refused proof", name, err)
	}
}

// A proof signed by the bound key, for this request and this bearer, within its
// window, verifies, and its jti comes back unspent.
func TestDpopProofVerifies(t *testing.T) {
	publicKey, privateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	proof := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)

	jti, err := verifyDpopProof(proof, newDpopTestRequest(http.MethodGet, dpopTestUrl), dpopTestToken, publicKey, now, dpopTestSettings())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, jti, "jti-1")
}

// htu names the URI without its query and fragment (RFC 9449 §4.3), and hosts
// compare as URIs do.
func TestDpopProofHtuIgnoresQueryAndNormalizesTheHost(t *testing.T) {
	publicKey, privateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, htu := range []string{
		"https://api.example/auth/refresh",
		"https://api.example/auth/refresh?after=1",
		"https://api.example/auth/refresh#fragment",
		"https://API.example/auth/refresh",
		"https://api.example:443/auth/refresh",
	} {
		proof := Testing_SignDpopProof(privateKey, http.MethodGet, htu, dpopTestToken, "jti-1", now)
		r := newDpopTestRequest(http.MethodGet, "https://api.example/auth/refresh?other=2")
		if _, err := verifyDpopProof(proof, r, dpopTestToken, publicKey, now, dpopTestSettings()); err != nil {
			t.Errorf("htu %s: %s", htu, err)
		}
	}
}

// A proof for another method, URI or bearer proves nothing about this request,
// and a proof outside its window has aged out.
func TestDpopProofBindsTheRequest(t *testing.T) {
	publicKey, privateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	request := func() *http.Request { return newDpopTestRequest(http.MethodGet, dpopTestUrl) }

	for _, c := range []struct {
		name   string
		method string
		htu    string
		token  string
		iat    time.Time
	}{
		{name: "another method", method: http.MethodPost, htu: dpopTestUrl, token: dpopTestToken, iat: now},
		{name: "another path", method: http.MethodGet, htu: "https://api.example/network/auth-client", token: dpopTestToken, iat: now},
		{name: "another host", method: http.MethodGet, htu: "https://other.example/auth/refresh", token: dpopTestToken, iat: now},
		{name: "plain http", method: http.MethodGet, htu: "http://api.example/auth/refresh", token: dpopTestToken, iat: now},
		{name: "no scheme", method: http.MethodGet, htu: "/auth/refresh", token: dpopTestToken, iat: now},
		{name: "another bearer", method: http.MethodGet, htu: dpopTestUrl, token: "another.bearer.token", iat: now},
		{name: "too old", method: http.MethodGet, htu: dpopTestUrl, token: dpopTestToken, iat: now.Add(-61 * time.Second)},
		{name: "too new", method: http.MethodGet, htu: dpopTestUrl, token: dpopTestToken, iat: now.Add(61 * time.Second)},
	} {
		proof := Testing_SignDpopProof(privateKey, c.method, c.htu, c.token, "jti-1", c.iat)
		_, err := verifyDpopProof(proof, request(), dpopTestToken, publicKey, now, dpopTestSettings())
		requireDpopRefused(t, c.name, err)
	}

	// the edges of the window are inside it
	for _, iat := range []time.Time{now.Add(-60 * time.Second), now.Add(60 * time.Second)} {
		proof := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", iat)
		if _, err := verifyDpopProof(proof, request(), dpopTestToken, publicKey, now, dpopTestSettings()); err != nil {
			t.Errorf("iat %s: %s", iat, err)
		}
	}
}

// A local stack may name http, and nothing else may.
func TestDpopProofHttpIsLocalOnly(t *testing.T) {
	publicKey, privateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	proof := Testing_SignDpopProof(privateKey, http.MethodGet, "http://api.example:80/auth/refresh", dpopTestToken, "jti-1", now)
	r := newDpopTestRequest(http.MethodGet, dpopTestUrl)

	_, err := verifyDpopProof(proof, r, dpopTestToken, publicKey, now, dpopTestSettings())
	requireDpopRefused(t, "http", err)

	settings := dpopTestSettings()
	settings.AllowHttp = true
	_, err = verifyDpopProof(proof, r, dpopTestToken, publicKey, now, settings)
	connect.AssertEqual(t, err, nil)
}

// Only the client's newest bound key counts: a proof by any other, an older
// generation's included, is refused, and so is a proof that names the bound key
// and was signed by another.
func TestDpopProofNeedsTheBoundKey(t *testing.T) {
	boundKey, boundPrivateKey := newDpopTestKey(t)
	_, olderPrivateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	r := newDpopTestRequest(http.MethodGet, dpopTestUrl)

	older := Testing_SignDpopProof(olderPrivateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)
	_, err := verifyDpopProof(older, r, dpopTestToken, boundKey, now, dpopTestSettings())
	requireDpopRefused(t, "older generation's key", err)

	// the older key's signature under a header that names the bound key
	good := Testing_SignDpopProof(boundPrivateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)
	goodParts := strings.Split(good, ".")
	olderParts := strings.Split(older, ".")
	forged := goodParts[0] + "." + goodParts[1] + "." + olderParts[2]
	_, err = verifyDpopProof(forged, r, dpopTestToken, boundKey, now, dpopTestSettings())
	requireDpopRefused(t, "forged signature", err)
}

// A proof with a header and claims a test sets.
func signDpopHeaderProof(t testing.TB, privateKey ed25519.PrivateKey, edit func(token *gojwt.Token, claims *dpopProofClaims), now time.Time) string {
	t.Helper()
	bound := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)
	claims := &dpopProofClaims{}
	if _, _, err := gojwt.NewParser().ParseUnverified(bound, claims); err != nil {
		t.Fatal(err)
	}
	token := gojwt.NewWithClaims(gojwt.SigningMethodEdDSA, claims)
	token.Header["typ"] = dpopType
	token.Header["jwk"] = map[string]any{
		"kty": "OKP",
		"crv": "Ed25519",
		"x":   base64.RawURLEncoding.EncodeToString(privateKey.Public().(ed25519.PublicKey)),
	}
	edit(token, claims)
	signed, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// §7.4 fixes the proof's shape: anything else is refused.
func TestDpopProofHeaderAndClaimsAreExact(t *testing.T) {
	publicKey, privateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	r := newDpopTestRequest(http.MethodGet, dpopTestUrl)

	// the control: an unedited proof verifies
	proof := signDpopHeaderProof(t, privateKey, func(token *gojwt.Token, claims *dpopProofClaims) {}, now)
	_, err := verifyDpopProof(proof, r, dpopTestToken, publicKey, now, dpopTestSettings())
	connect.AssertEqual(t, err, nil)

	for name, edit := range map[string]func(token *gojwt.Token, claims *dpopProofClaims){
		"a plain jwt": func(token *gojwt.Token, claims *dpopProofClaims) { token.Header["typ"] = "JWT" },
		"no typ":      func(token *gojwt.Token, claims *dpopProofClaims) { delete(token.Header, "typ") },
		"no jwk":      func(token *gojwt.Token, claims *dpopProofClaims) { delete(token.Header, "jwk") },
		"a private jwk": func(token *gojwt.Token, claims *dpopProofClaims) {
			token.Header["jwk"].(map[string]any)["d"] = base64.RawURLEncoding.EncodeToString(privateKey.Seed())
		},
		"another curve": func(token *gojwt.Token, claims *dpopProofClaims) {
			token.Header["jwk"].(map[string]any)["crv"] = "Ed448"
		},
		"a short key": func(token *gojwt.Token, claims *dpopProofClaims) {
			token.Header["jwk"].(map[string]any)["x"] = "AAAA"
		},
		"no jti":      func(token *gojwt.Token, claims *dpopProofClaims) { claims.ID = "" },
		"a long jti":  func(token *gojwt.Token, claims *dpopProofClaims) { claims.ID = strings.Repeat("j", 17) },
		"no iat":      func(token *gojwt.Token, claims *dpopProofClaims) { claims.IssuedAt = nil },
		"no ath":      func(token *gojwt.Token, claims *dpopProofClaims) { claims.Ath = "" },
		"no htm":      func(token *gojwt.Token, claims *dpopProofClaims) { claims.Htm = "" },
		"no htu":      func(token *gojwt.Token, claims *dpopProofClaims) { claims.Htu = "" },
		"a lowercase": func(token *gojwt.Token, claims *dpopProofClaims) { claims.Htm = "get" },
	} {
		proof := signDpopHeaderProof(t, privateKey, edit, now)
		_, err := verifyDpopProof(proof, r, dpopTestToken, publicKey, now, dpopTestSettings())
		requireDpopRefused(t, name, err)
	}

	// EdDSA alone: the same claims under an HMAC keyed by the public key, the
	// classic confusion, and unsigned
	hmac := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{"jti": "jti-1"})
	hmac.Header["typ"] = dpopType
	signed, err := hmac.SignedString([]byte(publicKey))
	connect.AssertEqual(t, err, nil)
	_, err = verifyDpopProof(signed, r, dpopTestToken, publicKey, now, dpopTestSettings())
	requireDpopRefused(t, "hmac", err)

	none := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{"jti": "jti-1"})
	none.Header["typ"] = dpopType
	signed, err = none.SignedString(gojwt.UnsafeAllowNoneSignatureType)
	connect.AssertEqual(t, err, nil)
	_, err = verifyDpopProof(signed, r, dpopTestToken, publicKey, now, dpopTestSettings())
	requireDpopRefused(t, "none", err)

	_, err = verifyDpopProof("not a proof", r, dpopTestToken, publicKey, now, dpopTestSettings())
	requireDpopRefused(t, "not a proof", err)
}

// The request carries one proof, and a client with no bound key is refused
// without a look at it.
func TestVerifyDpopProofNeedsOneProofAndABoundKey(t *testing.T) {
	ctx := context.Background()
	publicKey, privateKey := newDpopTestKey(t)
	clientId := server.NewId()
	proof := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", server.NowUtc())

	r := newDpopTestRequest(http.MethodGet, dpopTestUrl)
	requireDpopRefused(t, "no proof", VerifyDpopProof(ctx, r, dpopTestToken, clientId, publicKey))

	r.Header.Add(DpopHeader, proof)
	r.Header.Add(DpopHeader, proof)
	requireDpopRefused(t, "two proofs", VerifyDpopProof(ctx, r, dpopTestToken, clientId, publicKey))

	r = newDpopTestRequest(http.MethodGet, dpopTestUrl)
	r.Header.Set(DpopHeader, proof)
	requireDpopRefused(t, "no bound key", VerifyDpopProof(ctx, r, dpopTestToken, clientId, nil))
}

// A jti is good once. A proof that fails spends nothing, so a bad proof cannot
// burn the jti of a good one, and one client's jtis are not another's.
func TestVerifyDpopProofRefusesAReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey, privateKey := newDpopTestKey(t)
		clientId := server.NewId()
		now := server.NowUtc()

		request := func(proof string) *http.Request {
			r := newDpopTestRequest(http.MethodGet, dpopTestUrl)
			r.Header.Set(DpopHeader, proof)
			return r
		}

		// another bearer's proof with this jti fails before it is spent
		wrongBearer := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, "another.bearer.token", "jti-1", now)
		requireDpopRefused(t, "wrong bearer", VerifyDpopProof(ctx, request(wrongBearer), dpopTestToken, clientId, publicKey))

		proof := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)
		connect.AssertEqual(t, VerifyDpopProof(ctx, request(proof), dpopTestToken, clientId, publicKey), nil)
		requireDpopRefused(t, "replay", VerifyDpopProof(ctx, request(proof), dpopTestToken, clientId, publicKey))

		// a new jti is a new proof
		next := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-2", now)
		connect.AssertEqual(t, VerifyDpopProof(ctx, request(next), dpopTestToken, clientId, publicKey), nil)

		// the same jti is another client's to use
		connect.AssertEqual(t, VerifyDpopProof(ctx, request(proof), dpopTestToken, server.NewId(), publicKey), nil)

		// the replay key is §7.4's, and it holds for §7.4's ttl
		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.TTL(ctx, "{dpop_"+clientId.String()+"}jti-1").Result()
			server.Raise(err)
		})
		if ttl <= 0 || 120*time.Second < ttl {
			t.Fatalf("jti ttl = %s", ttl)
		}
	})
}

// An unavailable store refuses: a proof that cannot be spent is not good.
func TestDpopJtiStoreFailureRefuses(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := spendDpopJti(ctx, server.NewId(), "jti-1", dpopTestSettings())
	requireDpopRefused(t, "canceled store", err)
	if !errors.Is(err, ErrAuthUnavailable) {
		t.Fatalf("err = %v, want it unavailable", err)
	}
}
