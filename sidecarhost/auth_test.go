package sidecarhost

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
)

// The test's issuer and svc- key, installed in place of auth.yml's, and a verifier of that key, as a
// non-issuer block's mounted jwks gives it.
func installTestServiceTokenKeys(t *testing.T) *oauth.ServiceTokenVerifier {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	key := &oauth.SignerKey{
		Kid:        oauth.SignerKeySetService.KidPrefix() + thumbprint,
		Alg:        "ES256",
		Set:        oauth.SignerKeySetService,
		PrivateKey: privateKey,
	}
	t.Cleanup(oauth.Testing_SetIssuer("https://auth.example"))
	t.Cleanup(oauth.Testing_SetSignerKeys(key))
	verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
		Issuer: "https://auth.example",
		Keys:   oauth.PublicJwks(oauth.SignerKeySetService).Keys,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(oauth.Testing_SetServiceTokenVerifier(verifier))
	return verifier
}

// A minted token's §7.8 claims reach Principal, each id as the root's.
func TestAuthenticatorMapsServiceTokenClaims(t *testing.T) {
	installTestServiceTokenKeys(t)
	sidecarId := server.NewId()
	subjectClientId := server.NewId()
	args := &oauth.MintServiceTokenArgs{
		NetworkId:       server.NewId(),
		ClientId:        server.NewId(),
		SubjectClientId: &subjectClientId,
		SidecarId:       &sidecarId,
		Audience:        "evs",
		Scopes:          []string{"repo:" + server.NewId().String() + ":rw"},
		ProjVersion:     9,
	}
	token, expiry, err := oauth.MintServiceToken(args)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := NewAuthenticator().Authenticate(t.Context(), "evs", token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.NetworkId != connect.Id(args.NetworkId) || principal.ClientId != connect.Id(args.ClientId) {
		t.Fatalf("ids %+v", principal)
	}
	if principal.SidecarId == nil || *principal.SidecarId != connect.Id(sidecarId) ||
		principal.SubjectClientId == nil || *principal.SubjectClientId != connect.Id(subjectClientId) ||
		principal.MemberId != nil {
		t.Fatalf("optional ids %+v", principal)
	}
	if principal.Audience != "evs" || principal.ProjVersion != 9 || !slices.Equal(principal.Scopes, args.Scopes) {
		t.Fatalf("claims %+v", principal)
	}
	if principal.IssuedAt.IsZero() || !principal.ExpiresAt.Equal(expiry.Truncate(time.Second)) {
		t.Fatalf("times %v %v, expiry %v", principal.IssuedAt, principal.ExpiresAt, expiry)
	}
	// ServiceHost.Authenticate holds the host's answer to the same rules
	host := &sidecar.ServiceHost{Authenticator: NewAuthenticator()}
	if _, err := host.Authenticate(t.Context(), "evs", token); err != nil {
		t.Fatal(err)
	}
}

// Empty, wrong, expired and other-audience tokens are refused as ErrAuthenticationRefused, so the
// engine answers 401.
func TestAuthenticatorRefusesTokensThatWillNotAuthenticate(t *testing.T) {
	verifier := installTestServiceTokenKeys(t)
	token, expiry, err := oauth.MintServiceToken(&oauth.MintServiceTokenArgs{
		NetworkId: server.NewId(),
		ClientId:  server.NewId(),
		Audience:  "gkm",
	})
	if err != nil {
		t.Fatal(err)
	}
	authenticator := NewAuthenticator()
	ctx := t.Context()
	refused := map[string]func() error{
		"an empty audience": func() error { _, err := authenticator.Authenticate(ctx, "", token); return err },
		"an empty token":    func() error { _, err := authenticator.Authenticate(ctx, "gkm", ""); return err },
		"a wrong token": func() error {
			_, err := authenticator.Authenticate(ctx, "gkm", "eyJhbGciOiJFUzI1NiJ9.e30.c2lnbmF0dXJl")
			return err
		},
		"a tampered token": func() error {
			_, err := authenticator.Authenticate(ctx, "gkm", token[:len(token)-4]+"AAAA")
			return err
		},
		"another audience": func() error { _, err := authenticator.Authenticate(ctx, "evs", token); return err },
	}
	for name, authenticate := range refused {
		if err := authenticate(); !errors.Is(err, sidecar.ErrAuthenticationRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}

	verifier.Testing_SetNow(func() time.Time { return expiry.Add(time.Second) })
	if _, err := authenticator.Authenticate(ctx, "gkm", token); !errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Errorf("an expired token: %v", err)
	}
	verifier.Testing_SetNow(server.NowUtc)
	if _, err := authenticator.Authenticate(ctx, "gkm", token); err != nil {
		t.Errorf("the token itself: %v", err)
	}
}

// A scripted Client-class verifier: what a test sets, and what it was asked.
type testClientTokens struct {
	byJwt *session.ByJwt
	err   error
	asked []string
}

func (self *testClientTokens) Verify(ctx context.Context, jwtSigned string) (*session.ByJwt, error) {
	self.asked = append(self.asked, jwtSigned)
	return self.byJwt, self.err
}

// A by client jwt's claims as a verifier answers them.
func testClientByJwt(sidecarId *server.Id) *session.ByJwt {
	clientId, deviceId := server.NewId(), server.NewId()
	byJwt := session.NewByJwt(server.NewId(), server.NewId(), "synthetic", false, false).Client(deviceId, clientId)
	byJwt.SidecarId = sidecarId
	return byJwt
}

// A verified Client-class token reaches Principal: its network and client, its sidecar_id claim as SidecarId,
// the engine's audience, its times, and no scopes; and ServiceHost.Authenticate holds the host's answer.
func TestClientTokenAuthenticatorMapsAClientToken(t *testing.T) {
	sidecarId := server.NewId()
	byJwt := testClientByJwt(&sidecarId)
	clientTokens := &testClientTokens{byJwt: byJwt}
	authenticator := NewClientTokenAuthenticator("sockets", clientTokens)
	principal, err := authenticator.Authenticate(t.Context(), "sockets", "client-token")
	if err != nil {
		t.Fatal(err)
	}
	if principal.NetworkId != connect.Id(byJwt.NetworkId) || principal.ClientId != connect.Id(*byJwt.ClientId) ||
		principal.SidecarId == nil || *principal.SidecarId != connect.Id(sidecarId) ||
		principal.SubjectClientId != nil || principal.MemberId != nil {
		t.Fatalf("ids %+v", principal)
	}
	if principal.Audience != "sockets" || 0 < len(principal.Scopes) || principal.ProjVersion != 0 {
		t.Fatalf("claims %+v", principal)
	}
	if !principal.IssuedAt.Equal(byJwt.IssuedAt.Time) || !principal.ExpiresAt.Equal(byJwt.ExpiresAt.Time) {
		t.Fatalf("times %v %v", principal.IssuedAt, principal.ExpiresAt)
	}
	host := &sidecar.ServiceHost{Authenticator: authenticator}
	if _, err := host.Authenticate(t.Context(), "sockets", "client-token"); err != nil {
		t.Fatal(err)
	}
	// an edge platform client binds no sidecar: the engine decides what that may do
	clientTokens.byJwt = testClientByJwt(nil)
	if principal, err := authenticator.Authenticate(t.Context(), "sockets", "edge-token"); err != nil || principal.SidecarId != nil {
		t.Fatalf("no sidecar: %+v %v", principal, err)
	}
	if !slices.Equal(clientTokens.asked, []string{"client-token", "client-token", "edge-token"}) {
		t.Fatalf("asked %v", clientTokens.asked)
	}
}

// The verifier's refusal is a refusal (401); its unavailable answer carries its wait and is none; anything else
// is unavailable with no wait. An empty token refuses without asking, and a service audience never reaches the
// Client-class verifier.
func TestClientTokenAuthenticatorKeepsRefusedAndUnavailableApart(t *testing.T) {
	clientTokens := &testClientTokens{}
	authenticator := NewClientTokenAuthenticator("sockets", clientTokens)
	ctx := t.Context()

	clientTokens.err = fmt.Errorf("%w: %w", session.ErrClientTokenRefused, session.ErrByJwtInactive)
	if _, err := authenticator.Authenticate(ctx, "sockets", "removed-client-token"); !errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Errorf("a refused token: %v", err)
	}

	clientTokens.err = &session.ClientTokenUnavailableError{RetryAfter: 5 * time.Second, Err: errors.New("the revocation record cannot answer")}
	_, err := authenticator.Authenticate(ctx, "sockets", "missed-client-token")
	unavailable := &sidecar.AuthenticationUnavailableError{}
	if !errors.As(err, &unavailable) || unavailable.RetryAfter != 5*time.Second || errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Errorf("a record miss: %v", err)
	}

	clientTokens.err = errors.New("a verifier fault")
	if _, err := authenticator.Authenticate(ctx, "sockets", "client-token"); err == nil || errors.Is(err, sidecar.ErrAuthenticationRefused) || errors.As(err, &unavailable) {
		t.Errorf("another error: %v", err)
	}

	asked := len(clientTokens.asked)
	if _, err := authenticator.Authenticate(ctx, "sockets", ""); !errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Errorf("no token: %v", err)
	}
	installTestServiceTokenKeys(t)
	if _, err := authenticator.Authenticate(ctx, "evs", "eyJhbGciOiJFUzI1NiJ9.e30.c2lnbmF0dXJl"); !errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Errorf("a wrong service token: %v", err)
	}
	if len(clientTokens.asked) != asked {
		t.Errorf("the verifier was asked %v", clientTokens.asked[asked:])
	}
}

// A process with no verifier, and an audience no verifier here serves (sockets' Client-class tokens on a host
// given no Client-class verifier, plan-SOCK-client-auth), answer unavailable, not refused: nothing is known
// about the token, so the caller retries rather than sign out, and nothing is allowed.
func TestAuthenticatorUnavailableIsNotARefusal(t *testing.T) {
	broken := &Authenticator{verify: func(token string, audience string) (*oauth.ServiceTokenClaims, error) {
		return nil, errors.New("no service token verifier: oauth-jwks-svc.json is unreadable")
	}}
	principal, err := broken.Authenticate(t.Context(), "evs", "token")
	if err == nil || principal != nil || errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Fatalf("no verifier: %v, %v", principal, err)
	}

	installTestServiceTokenKeys(t)
	for _, audience := range []string{"sockets", "api", "tlsedge"} {
		principal, err := NewAuthenticator().Authenticate(t.Context(), audience, "token")
		if err == nil || principal != nil || errors.Is(err, sidecar.ErrAuthenticationRefused) {
			t.Errorf("%s: %v, %v", audience, principal, err)
		}
	}
}
