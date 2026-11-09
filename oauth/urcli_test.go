package oauth

// ur-cli's grant and admin tokens (urcli.go, admin_token.go; §7.8, D-TEAM-7,
// §22.5's §7 row): v0 runs one command, no authorization completes without a
// consent receipt or on a stale sign-in, the code redeems only with a proof by
// the key the request named, and the admin token it mints authenticates only
// its own command, at the api's own origin, once per proof. The command's use
// is spent in model's transaction (model/admin_token_use_model_test.go).

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// The test's api origin, https://api.<WARP_DOMAIN>.
const testUrcliApiOrigin = "https://api.urcli.example"

func installUrcliTest(t testing.TB) {
	t.Helper()
	installTestSignerKeys(t)
	t.Setenv("WARP_DOMAIN", "urcli.example")
}

// v0's one command, as ur login sends it.
func testUrcliCommand() *sidecar.AdminCommand {
	return &sidecar.AdminCommand{Method: "POST", Path: "/network/auth-keys", Body: []byte(`{"duration_seconds":300,"max_uses":1}`)}
}

// What one ur login holds in its own process: its DPoP key, its PKCE verifier
// and its request.
type testUrcliLogin struct {
	privateKey ed25519.PrivateKey
	jkt        string
	verifier   string
	request    *AuthorizationRequest
}

func newTestUrcliLogin(t testing.TB, command *sidecar.AdminCommand) *testUrcliLogin {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifierBytes := make([]byte, 32)
	rand.Read(verifierBytes)
	verifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
	challenge := sha256.Sum256([]byte(verifier))
	consentCode, err := sidecar.NewAdminConsentCode(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	details, err := sidecar.FormatAdminCommandDetails(command, consentCode)
	if err != nil {
		t.Fatal(err)
	}
	return &testUrcliLogin{
		privateKey: privateKey,
		jkt:        session.DpopKeyThumbprint(publicKey),
		verifier:   verifier,
		request: &AuthorizationRequest{
			ClientId:             UrcliClientId,
			RedirectUri:          "http://127.0.0.1:49152/callback",
			ResponseType:         "code",
			Scopes:               []string{UrcliScopeAuthKeys},
			State:                "synthetic-state",
			CodeChallenge:        base64.RawURLEncoding.EncodeToString(challenge[:]),
			CodeChallengeMethod:  PkceMethodS256,
			Resource:             testUrcliApiOrigin,
			DpopJkt:              session.DpopKeyThumbprint(publicKey),
			AuthorizationDetails: details,
		},
	}
}

// A signed-in user with a fresh sign-in.
func newTestUrcliUser(t testing.TB, ctx context.Context) *AuthorizingUser {
	t.Helper()
	networkId := server.NewId()
	userId := server.NewId()
	// an id's random tail: ids made in one millisecond share their head
	model.Testing_CreateNetwork(ctx, networkId, "urcli-"+strings.ToLower(networkId.String()[24:]), userId)
	return &AuthorizingUser{UserId: userId, NetworkId: networkId, AuthTime: server.NowUtc()}
}

// The consent and authorization the page drives, and the code it returns.
func (self *testUrcliLogin) authorize(t testing.TB, ctx context.Context, user *AuthorizingUser) string {
	t.Helper()
	consent, err := DescribeUrcliConsent(self.request, user.UserId, user.AuthTime, server.NowUtc())
	if err != nil {
		t.Fatal(err)
	}
	code, err := authorizeUrcli(ctx, self.request, user, consent.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	return code
}

// The token request's proof: by this login's key, at the token endpoint, ath
// over the code.
func (self *testUrcliLogin) tokenProof(code string) string {
	return session.Testing_SignDpopProof(self.privateKey, "POST", TokenEndpoint(), code, server.NewId().String(), time.Now())
}

func (self *testUrcliLogin) exchange(ctx context.Context, code string, proofs []string) (*TokenResponse, error) {
	return ExchangeUrcliCode(ctx, &ExchangeCodeArgs{
		Code:         code,
		ClientId:     UrcliClientId,
		RedirectUri:  self.request.RedirectUri,
		CodeVerifier: self.verifier,
		Resource:     testUrcliApiOrigin,
	}, proofs)
}

// v0 runs one command: POST /network/auth-keys for a single-use, user-owned
// key of at most 5 minutes, and nothing else validates.
func TestUrcliRequestAllowsOnlyTheV0Command(t *testing.T) {
	installUrcliTest(t)
	if _, err := ValidateUrcliRequest(newTestUrcliLogin(t, testUrcliCommand()).request); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"duration_seconds":300,"max_uses":2}`,
		`{"duration_seconds":301,"max_uses":1}`,
		`{"duration_seconds":0,"max_uses":1}`,
		`{"duration_seconds":300,"max_uses":1,"tags":["` + strings.Repeat("ab", 16) + `"]}`,
		`{"duration_seconds":300,"max_uses":1,"ephemeral":true}`,
		`{"duration_seconds":300,"max_uses":1,"pre_approved":true}`,
		`{"max_uses":1}`,
	} {
		command := testUrcliCommand()
		command.Body = []byte(body)
		if _, err := ValidateUrcliRequest(newTestUrcliLogin(t, command).request); !errors.Is(err, ErrUrcliCommandRefused) {
			t.Errorf("%s: %v", body, err)
		}
	}
	for i, alter := range []func(login *testUrcliLogin){
		func(login *testUrcliLogin) { login.request.Scopes = []string{UrcliScopeAuthKeys, ScopeMcpRead} },
		func(login *testUrcliLogin) { login.request.Scopes = []string{ScopeOfflineAccess} },
		func(login *testUrcliLogin) { login.request.DpopJkt = "" },
		func(login *testUrcliLogin) { login.request.DpopJkt = "not-a-thumbprint" },
		func(login *testUrcliLogin) { login.request.AuthorizationDetails = "" },
		func(login *testUrcliLogin) { login.request.Resource = "https://api.elsewhere.example" },
	} {
		login := newTestUrcliLogin(t, testUrcliCommand())
		alter(login)
		if _, err := ValidateUrcliRequest(login.request); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("alteration %d: %v", i, err)
		}
	}
	for _, command := range []*sidecar.AdminCommand{
		{Method: "POST", Path: "/network/auth-key-revoke", Body: []byte(`{"auth_key_id":"` + server.NewId().String() + `"}`)},
		{Method: "GET", Path: "/network/auth-keys"},
		{Method: "POST", Path: "/auth/code-create", Body: []byte(`{}`)},
	} {
		if _, err := ValidateUrcliRequest(newTestUrcliLogin(t, command).request); !errors.Is(err, ErrUrcliCommandRefused) {
			t.Errorf("%s %s: %v", command.Method, command.Path, err)
		}
	}
}

// No ur-cli authorization completes without a consent that showed its code:
// the receipt /oauth/consent gave for this request, user and time.
func TestUrcliAuthorizationNeedsAConsentReceipt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		ctx := context.Background()
		user := newTestUrcliUser(t, ctx)
		login := newTestUrcliLogin(t, testUrcliCommand())

		if _, err := authorizeUrcli(ctx, login.request, user, ""); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("no receipt: %v", err)
		}
		if _, err := Authorize(ctx, login.request, user); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("the generic grant took ur-cli: %v", err)
		}

		consent, err := DescribeUrcliConsent(login.request, user.UserId, user.AuthTime, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		if consent.ConsentCode == "" || consent.Method != "POST" || consent.Path != "/network/auth-keys" {
			t.Fatalf("the consent shows %+v", consent)
		}
		// a receipt of another request: another command, code or key
		other := newTestUrcliLogin(t, testUrcliCommand())
		if _, err := authorizeUrcli(ctx, other.request, user, consent.Receipt); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("another request's receipt: %v", err)
		}
		// a receipt of another user
		otherUser := newTestUrcliUser(t, ctx)
		if _, err := authorizeUrcli(ctx, login.request, otherUser, consent.Receipt); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("another user's receipt: %v", err)
		}
		// an expired receipt
		if err := verifyUrcliConsentReceipt(consent.Receipt, user.UserId, login.request, func() time.Time {
			return time.Now().Add(DefaultUrcliSettings().ConsentReceiptDuration + time.Minute)
		}); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("an expired receipt: %v", err)
		}
		// an admin token is no receipt, though the same set signs both
		token, _, err := MintAdminToken(&MintAdminTokenArgs{
			NetworkId: user.NetworkId, MemberId: user.UserId, Scopes: []string{UrcliScopeAuthKeys},
			Jkt: login.jkt, Cmd: "c", Duration: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authorizeUrcli(ctx, login.request, user, token); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("an admin token as a receipt: %v", err)
		}
		if _, err := authorizeUrcli(ctx, login.request, user, consent.Receipt); err != nil {
			t.Fatalf("the receipt of this request: %v", err)
		}
	})
}

// A sign-in older than ten minutes is answered reauthentication_required,
// never a consent, and no code is minted on it.
func TestUrcliConsentNeedsAFreshSignIn(t *testing.T) {
	installUrcliTest(t)
	login := newTestUrcliLogin(t, testUrcliCommand())
	stale := server.NowUtc().Add(-DefaultUrcliSettings().MaxAuthAge - time.Minute)
	userId := server.NewId()
	if _, err := DescribeUrcliConsent(login.request, userId, stale, server.NowUtc()); !errors.Is(err, ErrUrcliReauthenticationRequired) {
		t.Fatalf("a stale sign-in: %v", err)
	}
	if _, err := DescribeUrcliConsent(login.request, userId, time.Time{}, server.NowUtc()); !errors.Is(err, ErrUrcliReauthenticationRequired) {
		t.Fatalf("no sign-in time: %v", err)
	}
	receipt, err := mintUrcliConsentReceipt(userId, login.request, server.NowUtc(), DefaultUrcliSettings())
	if err != nil {
		t.Fatal(err)
	}
	user := &AuthorizingUser{UserId: userId, NetworkId: server.NewId(), AuthTime: stale}
	if _, err := authorizeUrcli(context.Background(), login.request, user, receipt); !errors.Is(err, ErrUrcliReauthenticationRequired) {
		t.Fatalf("a code on a stale sign-in: %v", err)
	}
}

// The code redeems once, with its verifier and a proof by the key the request
// named, for an hour's DPoP-bound admin token of exactly the consented command.
func TestUrcliCodeGrantMintsADpopBoundAdminToken(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		ctx := context.Background()
		user := newTestUrcliUser(t, ctx)
		login := newTestUrcliLogin(t, testUrcliCommand())

		code := login.authorize(t, ctx, user)
		// a proof by another key
		otherLogin := newTestUrcliLogin(t, testUrcliCommand())
		if _, err := login.exchange(ctx, code, []string{otherLogin.tokenProof(code)}); !errors.Is(err, ErrInvalidDpopProof) {
			t.Fatalf("another key's proof: %v", err)
		}
		// the code was spent by that attempt: a code is single use
		code = login.authorize(t, ctx, user)
		if _, err := login.exchange(ctx, code, nil); !errors.Is(err, ErrInvalidDpopProof) {
			t.Fatalf("no proof: %v", err)
		}
		code = login.authorize(t, ctx, user)
		if _, err := ExchangeCode(ctx, &ExchangeCodeArgs{
			Code: code, ClientId: UrcliClientId, RedirectUri: login.request.RedirectUri,
			CodeVerifier: login.verifier, Resource: testUrcliApiOrigin,
		}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("the proofless grant took ur-cli's code: %v", err)
		}

		response, err := login.exchange(ctx, code, []string{login.tokenProof(code)})
		if err != nil {
			t.Fatal(err)
		}
		if response.TokenType != "DPoP" || response.RefreshToken != "" || response.IdToken != "" || response.Scope != UrcliScopeAuthKeys {
			t.Fatalf("response %+v", response)
		}
		if _, err := login.exchange(ctx, code, []string{login.tokenProof(code)}); !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("a code redeemed twice: %v", err)
		}

		claims, err := VerifyAdminToken(response.AccessToken)
		if err != nil {
			t.Fatal(err)
		}
		cmd, _ := testUrcliCommand().Digest()
		if claims.Cmd != cmd || claims.Confirmation.Jkt != login.jkt || claims.MemberId != user.UserId ||
			claims.NetworkId != user.NetworkId || claims.Scope != UrcliScopeAuthKeys {
			t.Fatalf("claims %+v", claims)
		}
		if lifetime := claims.ExpiresAt.Sub(claims.IssuedAt.Time); lifetime != time.Hour {
			t.Fatalf("a %s token", lifetime)
		}
		parsed, _, _ := gojwt.NewParser().ParseUnverified(response.AccessToken, &AdminTokenClaims{})
		if kid, _ := parsed.Header["kid"].(string); SignerKeySetOfKid(kid) != SignerKeySetAdmin {
			t.Fatalf("kid %v", parsed.Header["kid"])
		}
		// no other set's verifier takes it
		if _, err := newTestServiceTokenVerifier(t).Verify(response.AccessToken, "evs"); err == nil {
			t.Fatal("an admin token verified as a service token")
		}
	})
}

// An admin token request against the api: its body, a fresh proof, at origin.
func newTestAdminRequest(t testing.TB, login *testUrcliLogin, token string, url string, body string) *http.Request {
	t.Helper()
	request, err := http.NewRequest("POST", url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// a documentation address (RFC 5737)
	request.RemoteAddr = "192.0.2.1:1"
	request.Header.Set("Authorization", "DPoP "+token)
	request.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(login.privateKey, "POST", strings.Split(url, "?")[0], token, server.NewId().String(), time.Now()))
	return request
}

// The token authenticates its own command and nothing else: another body, a
// query, another origin, another key, a replayed proof and an expired token are
// refused, and the body stays readable for the handler.
func TestAdminTokenAuthenticatesOnlyItsCommand(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		ctx := context.Background()
		login := newTestUrcliLogin(t, testUrcliCommand())
		cmd, _ := testUrcliCommand().Digest()
		memberId := server.NewId()
		token, _, err := MintAdminToken(&MintAdminTokenArgs{
			NetworkId: server.NewId(), MemberId: memberId, Scopes: []string{UrcliScopeAuthKeys},
			Jkt: login.jkt, Cmd: cmd, Duration: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		url := testUrcliApiOrigin + "/network/auth-keys"
		body := `{ "max_uses": 1, "duration_seconds": 300 }`

		request := newTestAdminRequest(t, login, token, url, body)
		use, err := authenticateAdminToken(ctx, request, token)
		if err != nil {
			t.Fatal(err)
		}
		if use.Cmd != cmd || use.MemberId != memberId || use.Jti == "" {
			t.Fatalf("use %+v", use)
		}
		if read, _ := io.ReadAll(request.Body); string(read) != body {
			t.Fatalf("the handler reads %q", read)
		}
		// the same proof again
		replayed, _ := http.NewRequest("POST", url, strings.NewReader(body))
		replayed.Header = request.Header.Clone()
		if _, err := authenticateAdminToken(ctx, replayed, token); !errors.Is(err, ErrInvalidAdminToken) {
			t.Fatalf("a replayed proof: %v", err)
		}

		for name, request := range map[string]*http.Request{
			"another body":   newTestAdminRequest(t, login, token, url, `{"duration_seconds":300,"max_uses":2}`),
			"a query":        newTestAdminRequest(t, login, token, url+"?max_uses=2", body),
			"another origin": newTestAdminRequest(t, login, token, "https://api.elsewhere.example/network/auth-keys", body),
			"another path":   newTestAdminRequest(t, login, token, testUrcliApiOrigin+"/network/auth-key-revoke", body),
			"another key":    newTestAdminRequest(t, newTestUrcliLogin(t, testUrcliCommand()), token, url, body),
		} {
			if _, err := authenticateAdminToken(ctx, request, token); !errors.Is(err, ErrInvalidAdminToken) {
				t.Errorf("%s: %v", name, err)
			}
		}

		expired, err := func() (string, error) {
			signingKey, err := SigningKeyOf(SignerKeySetAdmin)
			if err != nil {
				return "", err
			}
			past := time.Now().Add(-2 * time.Hour)
			claims := &AdminTokenClaims{
				Scope: UrcliScopeAuthKeys, NetworkId: server.NewId(), MemberId: memberId,
				Confirmation: &AdminTokenConfirmation{Jkt: login.jkt}, Cmd: cmd,
				RegisteredClaims: gojwt.RegisteredClaims{
					Issuer: Issuer(), Audience: gojwt.ClaimStrings{AdminTokenAudience}, ID: server.NewId().String(),
					IssuedAt: gojwt.NewNumericDate(past), ExpiresAt: gojwt.NewNumericDate(past.Add(time.Hour)),
				},
			}
			unsigned := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
			unsigned.Header["kid"] = signingKey.Kid
			unsigned.Header["typ"] = adminTokenType
			return unsigned.SignedString(signingKey.PrivateKey)
		}()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authenticateAdminToken(ctx, newTestAdminRequest(t, login, expired, url, body), expired); !errors.Is(err, ErrInvalidAdminToken) {
			t.Fatalf("an expired token: %v", err)
		}
		// a service token is no admin token
		serviceToken, _, err := MintServiceToken(newTestServiceTokenArgs())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := authenticateAdminToken(ctx, newTestAdminRequest(t, login, serviceToken, url, body), serviceToken); !errors.Is(err, ErrInvalidAdminToken) {
			t.Fatalf("a service token: %v", err)
		}
	})
}

// The session takes an admin token only on a route whose model spends its use,
// as an unsigned identity of its member's network.
func TestAdminTokenEntersTheSessionUnsigned(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		login := newTestUrcliLogin(t, testUrcliCommand())
		cmd, _ := testUrcliCommand().Digest()
		networkId := server.NewId()
		memberId := server.NewId()
		token, _, err := MintAdminToken(&MintAdminTokenArgs{
			NetworkId: networkId, MemberId: memberId, Scopes: []string{UrcliScopeAuthKeys},
			Jkt: login.jkt, Cmd: cmd, Duration: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		request := newTestAdminRequest(t, login, token, testUrcliApiOrigin+"/network/auth-keys", string(testUrcliCommand().Body))
		clientSession, err := session.NewClientSessionFromRequest(request)
		if err != nil {
			t.Fatal(err)
		}
		defer clientSession.Cancel()
		if err := clientSession.Auth(request); err != nil {
			t.Fatal(err)
		}
		if !clientSession.UnsignedIdentity || clientSession.AdminCommand == nil || clientSession.ByJwt.NetworkId != networkId ||
			clientSession.ByJwt.UserId != memberId || clientSession.ByJwt.ClientId != nil {
			t.Fatalf("session %+v %+v", clientSession.AdminCommand, clientSession.ByJwt)
		}

		// a route whose model spends no admin token's use
		other := newTestAdminRequest(t, login, token, testUrcliApiOrigin+"/network/auth-key-revoke", `{}`)
		otherSession, err := session.NewClientSessionFromRequest(other)
		if err != nil {
			t.Fatal(err)
		}
		defer otherSession.Cancel()
		if err := otherSession.Auth(other); err == nil {
			t.Fatal("an admin token authenticated a route whose model spends no use")
		}
	})
}
