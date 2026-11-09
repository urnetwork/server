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

// The check of a proof of a GET of dpopTestUrl by key.
func newDpopTestCheck(proof string, key ed25519.PublicKey) *DpopRequest {
	return &DpopRequest{
		Proof:    proof,
		Method:   http.MethodGet,
		Uri:      dpopTestUrl,
		Token:    dpopTestToken,
		Key:      key,
		ReplayId: server.NewId(),
	}
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

	jti, err := verifyDpopProof(newDpopTestCheck(proof, publicKey), now, dpopTestSettings())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, jti, "jti-1")

	// and by the key's thumbprint, as an admin token's cnf.jkt names it
	check := newDpopTestCheck(proof, nil)
	check.KeyThumbprint = DpopKeyThumbprint(publicKey)
	jti, err = verifyDpopProof(check, now, dpopTestSettings())
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
		if _, err := verifyDpopProof(newDpopTestCheck(proof, publicKey), now, dpopTestSettings()); err != nil {
			t.Errorf("htu %s: %s", htu, err)
		}
	}
}

// A proof for another method, URI or bearer proves nothing about this request,
// and a proof outside its window has aged out.
func TestDpopProofBindsTheRequest(t *testing.T) {
	publicKey, privateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

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
		{name: "credentials", method: http.MethodGet, htu: "https://user@api.example/auth/refresh", token: dpopTestToken, iat: now},
		{name: "another bearer", method: http.MethodGet, htu: dpopTestUrl, token: "another.bearer.token", iat: now},
		{name: "too old", method: http.MethodGet, htu: dpopTestUrl, token: dpopTestToken, iat: now.Add(-61 * time.Second)},
		{name: "too new", method: http.MethodGet, htu: dpopTestUrl, token: dpopTestToken, iat: now.Add(61 * time.Second)},
	} {
		proof := Testing_SignDpopProof(privateKey, c.method, c.htu, c.token, "jti-1", c.iat)
		_, err := verifyDpopProof(newDpopTestCheck(proof, publicKey), now, dpopTestSettings())
		requireDpopRefused(t, c.name, err)
	}

	// the edges of the window, ±60 s on a fixed clock, are inside it
	for _, iat := range []time.Time{now.Add(-60 * time.Second), now.Add(60 * time.Second)} {
		proof := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", iat)
		if _, err := verifyDpopProof(newDpopTestCheck(proof, publicKey), now, dpopTestSettings()); err != nil {
			t.Errorf("iat %s: %s", iat, err)
		}
	}
}

// htu is compared with the origin api is configured with, never with the Host
// the request names or a forwarded header: a proof a client made for another
// origin, which an attacker relays here naming that origin as its Host, proves
// nothing. Pure: WARP_DOMAIN is set for the test.
func TestDpopApiRequestComparesTheConfiguredOrigin(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "dpop.example")
	publicKey, privateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	clientId := server.NewId()

	request := func(proof string, edit func(r *http.Request)) *DpopRequest {
		r := httptest.NewRequest(http.MethodGet, "https://elsewhere.example/auth/refresh?after=2", nil)
		r.Header.Set(DpopHeader, proof)
		if edit != nil {
			edit(r)
		}
		dpopRequest, err := NewApiDpopRequest(r, dpopTestToken, clientId, publicKey)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, dpopRequest.Uri, "https://api.dpop.example/auth/refresh")
		connect.AssertEqual(t, dpopRequest.ReplayId, clientId)
		return dpopRequest
	}

	good := Testing_SignDpopProof(privateKey, http.MethodGet, "https://api.dpop.example/auth/refresh", dpopTestToken, "jti-1", now)
	_, err := verifyDpopProof(request(good, nil), now, dpopTestSettings())
	connect.AssertEqual(t, err, nil)

	elsewhere := Testing_SignDpopProof(privateKey, http.MethodGet, "https://elsewhere.example/auth/refresh", dpopTestToken, "jti-1", now)
	for name, edit := range map[string]func(r *http.Request){
		"its own host":       nil,
		"a forwarded host":   func(r *http.Request) { r.Header.Set("X-Forwarded-Host", "elsewhere.example") },
		"a forwarded header": func(r *http.Request) { r.Header.Set("Forwarded", "host=elsewhere.example;proto=https") },
	} {
		_, err := verifyDpopProof(request(elsewhere, edit), now, dpopTestSettings())
		requireDpopRefused(t, name, err)
	}

	// http is never named, a local stack included
	plain := Testing_SignDpopProof(privateKey, http.MethodGet, "http://api.dpop.example/auth/refresh", dpopTestToken, "jti-1", now)
	_, err = verifyDpopProof(request(plain, nil), now, dpopTestSettings())
	requireDpopRefused(t, "http", err)

	// with no configured origin there is nothing to check against, so nothing passes
	t.Setenv("WARP_DOMAIN", "")
	r := httptest.NewRequest(http.MethodGet, "https://api.dpop.example/auth/refresh", nil)
	r.Header.Set(DpopHeader, good)
	_, err = NewApiDpopRequest(r, dpopTestToken, clientId, publicKey)
	requireDpopRefused(t, "no origin", err)
}

// RFC 8037 appendix A.3's thumbprint of its Ed25519 example key.
func TestDpopKeyThumbprintIsRfc7638(t *testing.T) {
	x, err := base64.RawURLEncoding.DecodeString("11qYAYKxCrfVS_7TyWQHOg7hcvPapiMlrwIaaPcHURo")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, DpopKeyThumbprint(ed25519.PublicKey(x)), "kPrK_qmxVWaYVA9wwBF6Iuo3vVzz7TxHCTwXBygrS4k")
}

// Only the client's newest bound key counts: a proof by any other, an older
// generation's included, is refused, and so is a proof that names the bound key
// and was signed by another.
func TestDpopProofNeedsTheBoundKey(t *testing.T) {
	boundKey, boundPrivateKey := newDpopTestKey(t)
	olderKey, olderPrivateKey := newDpopTestKey(t)
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	older := Testing_SignDpopProof(olderPrivateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)
	_, err := verifyDpopProof(newDpopTestCheck(older, boundKey), now, dpopTestSettings())
	requireDpopRefused(t, "older generation's key", err)

	// the older key's signature under a header that names the bound key
	good := Testing_SignDpopProof(boundPrivateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)
	goodParts := strings.Split(good, ".")
	olderParts := strings.Split(older, ".")
	forged := goodParts[0] + "." + goodParts[1] + "." + olderParts[2]
	_, err = verifyDpopProof(newDpopTestCheck(forged, boundKey), now, dpopTestSettings())
	requireDpopRefused(t, "forged signature", err)

	// by thumbprint, too, only the named key counts
	check := newDpopTestCheck(good, nil)
	check.KeyThumbprint = DpopKeyThumbprint(olderKey)
	_, err = verifyDpopProof(check, now, dpopTestSettings())
	requireDpopRefused(t, "another key's thumbprint", err)

	// a check names exactly one key
	check = newDpopTestCheck(good, boundKey)
	check.KeyThumbprint = DpopKeyThumbprint(boundKey)
	_, err = verifyDpopProof(check, now, dpopTestSettings())
	requireDpopRefused(t, "two keys", err)
	_, err = verifyDpopProof(newDpopTestCheck(good, nil), now, dpopTestSettings())
	requireDpopRefused(t, "no key", err)
	_, err = verifyDpopProof(newDpopTestCheck(good, boundKey[:16]), now, dpopTestSettings())
	requireDpopRefused(t, "a short key", err)
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

	// the control: an unedited proof verifies
	proof := signDpopHeaderProof(t, privateKey, func(token *gojwt.Token, claims *dpopProofClaims) {}, now)
	_, err := verifyDpopProof(newDpopTestCheck(proof, publicKey), now, dpopTestSettings())
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
		// signed by the bound key and naming another: the proof's own key must be
		// the bound key, whatever the signature says
		"another key": func(token *gojwt.Token, claims *dpopProofClaims) {
			otherKey, _ := newDpopTestKey(t)
			token.Header["jwk"].(map[string]any)["x"] = base64.RawURLEncoding.EncodeToString(otherKey)
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
		_, err := verifyDpopProof(newDpopTestCheck(proof, publicKey), now, dpopTestSettings())
		requireDpopRefused(t, name, err)
	}

	// EdDSA alone: the same claims under an HMAC keyed by the public key, the
	// classic confusion, and unsigned
	hmac := gojwt.NewWithClaims(gojwt.SigningMethodHS256, gojwt.MapClaims{"jti": "jti-1"})
	hmac.Header["typ"] = dpopType
	signed, err := hmac.SignedString([]byte(publicKey))
	connect.AssertEqual(t, err, nil)
	_, err = verifyDpopProof(newDpopTestCheck(signed, publicKey), now, dpopTestSettings())
	requireDpopRefused(t, "hmac", err)

	none := gojwt.NewWithClaims(gojwt.SigningMethodNone, gojwt.MapClaims{"jti": "jti-1"})
	none.Header["typ"] = dpopType
	signed, err = none.SignedString(gojwt.UnsafeAllowNoneSignatureType)
	connect.AssertEqual(t, err, nil)
	_, err = verifyDpopProof(newDpopTestCheck(signed, publicKey), now, dpopTestSettings())
	requireDpopRefused(t, "none", err)

	_, err = verifyDpopProof(newDpopTestCheck("not a proof", publicKey), now, dpopTestSettings())
	requireDpopRefused(t, "not a proof", err)
}

// An api request carries one proof, and a client with no bound key is refused
// without a look at it.
func TestVerifyDpopProofNeedsOneProofAndABoundKey(t *testing.T) {
	ctx := context.Background()
	publicKey, privateKey := newDpopTestKey(t)
	clientId := server.NewId()
	proof := Testing_SignDpopProof(privateKey, http.MethodGet, Testing_DpopApiUrl("/auth/refresh"), dpopTestToken, "jti-1", server.NowUtc())

	r := httptest.NewRequest(http.MethodGet, "/auth/refresh", nil)
	_, err := NewApiDpopRequest(r, dpopTestToken, clientId, publicKey)
	requireDpopRefused(t, "no proof", err)

	r.Header.Add(DpopHeader, proof)
	r.Header.Add(DpopHeader, proof)
	_, err = NewApiDpopRequest(r, dpopTestToken, clientId, publicKey)
	requireDpopRefused(t, "two proofs", err)

	r = httptest.NewRequest(http.MethodGet, "/auth/refresh", nil)
	r.Header.Set(DpopHeader, proof)
	dpopRequest, err := NewApiDpopRequest(r, dpopTestToken, clientId, nil)
	connect.AssertEqual(t, err, nil)
	requireDpopRefused(t, "no bound key", VerifyDpopProof(ctx, dpopRequest))
	requireDpopRefused(t, "no check", VerifyDpopProof(ctx, nil))
}

// A jti is good once. A proof that fails spends nothing, so a bad proof cannot
// burn the jti of a good one, and one client's jtis are not another's.
func TestVerifyDpopProofRefusesAReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		publicKey, privateKey := newDpopTestKey(t)
		clientId := server.NewId()
		now := server.NowUtc()

		request := func(proof string, replayId server.Id) *DpopRequest {
			check := newDpopTestCheck(proof, publicKey)
			check.ReplayId = replayId
			return check
		}

		// another bearer's proof with this jti fails before it is spent
		wrongBearer := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, "another.bearer.token", "jti-1", now)
		requireDpopRefused(t, "wrong bearer", VerifyDpopProof(ctx, request(wrongBearer, clientId)))

		proof := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-1", now)
		connect.AssertEqual(t, VerifyDpopProof(ctx, request(proof, clientId)), nil)
		requireDpopRefused(t, "replay", VerifyDpopProof(ctx, request(proof, clientId)))

		// a new jti is a new proof
		next := Testing_SignDpopProof(privateKey, http.MethodGet, dpopTestUrl, dpopTestToken, "jti-2", now)
		connect.AssertEqual(t, VerifyDpopProof(ctx, request(next, clientId)), nil)

		// the same jti is another client's to use
		connect.AssertEqual(t, VerifyDpopProof(ctx, request(proof, server.NewId())), nil)

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
