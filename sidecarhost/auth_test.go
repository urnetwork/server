package sidecarhost

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
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

// A process with no verifier, and an audience no verifier here serves (sockets' Client-class tokens,
// plan-SOCK-client-auth), answer unavailable, not refused: nothing is known about the token, so the
// caller retries rather than sign out, and nothing is allowed.
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
