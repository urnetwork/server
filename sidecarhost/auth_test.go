package sidecarhost

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

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

// A test's rev/ objects on fake blobs, signed with the test's svc- key as api signs them, and a checker of
// them that reads on every check.
type testRevocations struct {
	t       *testing.T
	cluster sidecar.BlobCluster
	checker *sidecar.RevocationChecker
}

func newTestRevocations(t *testing.T) *testRevocations {
	t.Helper()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	settings := sidecar.DefaultRevocationSettings()
	settings.CacheTimeout = 0
	return &testRevocations{t: t, cluster: cluster, checker: sidecar.NewRevocationChecker(blobs, oauth.VerifyRevocation, settings)}
}

// Writes the rev/ object that voids kind's subject in networkId for tokens issued before notBefore.
func (self *testRevocations) revoke(networkId server.Id, kind sidecar.RevocationKind, subjectId server.Id, notBefore time.Time) {
	self.t.Helper()
	record := &sidecar.RevocationRecord{
		NetworkId: sidecar.Id(networkId),
		Kind:      kind,
		SubjectId: sidecar.Id(subjectId),
		NotBefore: notBefore.Truncate(time.Second),
		Reason:    sidecar.RevocationReasonRemoved,
		Time:      server.NowUtc().Truncate(time.Second),
	}
	payload, err := sidecar.EncodeRevocationPayload(record)
	if err != nil {
		self.t.Fatal(err)
	}
	object, err := oauth.SignRevocation(payload)
	if err != nil {
		self.t.Fatal(err)
	}
	if _, err := self.cluster.PutIfNoneMatch(context.Background(), record.Key(), object, nil); err != nil {
		self.t.Fatal(err)
	}
}

// A sockets token of a fresh network's client, bound to a sidecar of a member.
type testSocketsToken struct {
	token     string
	networkId server.Id
	clientId  server.Id
	sidecarId server.Id
	memberId  server.Id
	expiry    time.Time
}

func mintTestSocketsToken(t *testing.T) *testSocketsToken {
	t.Helper()
	self := &testSocketsToken{networkId: server.NewId(), clientId: server.NewId(), sidecarId: server.NewId(), memberId: server.NewId()}
	token, expiry, err := oauth.MintServiceToken(&oauth.MintServiceTokenArgs{
		NetworkId: self.networkId,
		ClientId:  self.clientId,
		SidecarId: &self.sidecarId,
		MemberId:  &self.memberId,
		Audience:  "sockets",
	})
	if err != nil {
		t.Fatal(err)
	}
	self.token, self.expiry = token, expiry
	return self
}

// sockets' callers hold service tokens (QD-1029): a sockets token maps onto Principal with its sidecar and member,
// passes the block's rev/ check while nothing voids it, and ServiceHost.Authenticate holds the host's answer; an
// object that voids only tokens issued before this one was leaves it standing.
func TestRevocationAuthenticatorTakesASocketsToken(t *testing.T) {
	installTestServiceTokenKeys(t)
	revocations := newTestRevocations(t)
	authenticator := NewRevocationAuthenticator("sockets", revocations.checker)
	minted := mintTestSocketsToken(t)

	principal, err := authenticator.Authenticate(t.Context(), "sockets", minted.token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.NetworkId != connect.Id(minted.networkId) || principal.ClientId != connect.Id(minted.clientId) ||
		principal.SidecarId == nil || *principal.SidecarId != connect.Id(minted.sidecarId) ||
		principal.MemberId == nil || *principal.MemberId != connect.Id(minted.memberId) ||
		principal.SubjectClientId != nil || principal.Audience != "sockets" || 0 < len(principal.Scopes) {
		t.Fatalf("principal %+v", principal)
	}
	host := &sidecar.ServiceHost{Authenticator: authenticator}
	if _, err := host.Authenticate(t.Context(), "sockets", minted.token); err != nil {
		t.Fatal(err)
	}

	// a removal before this token was issued, whose object an earlier token's would void
	revocations.revoke(minted.networkId, sidecar.RevocationKindClient, minted.clientId, principal.IssuedAt.Add(-time.Hour))
	if _, err := authenticator.Authenticate(t.Context(), "sockets", minted.token); err != nil {
		t.Fatalf("a token issued after its client's not_before: %v", err)
	}
}

// sockets refuses an evs token, an expired sockets token, and a sockets token whose client, sidecar or member a
// rev/ object voids, each as ErrAuthenticationRefused, so the engine answers 401.
func TestRevocationAuthenticatorRefusesTokensSocketsMustNotTake(t *testing.T) {
	verifier := installTestServiceTokenKeys(t)
	ctx := t.Context()

	evsToken, _, err := oauth.MintServiceToken(&oauth.MintServiceTokenArgs{NetworkId: server.NewId(), ClientId: server.NewId(), Audience: "evs"})
	if err != nil {
		t.Fatal(err)
	}
	authenticator := NewRevocationAuthenticator("sockets", newTestRevocations(t).checker)
	if _, err := authenticator.Authenticate(ctx, "sockets", evsToken); !errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Errorf("an evs token: %v", err)
	}

	minted := mintTestSocketsToken(t)
	verifier.Testing_SetNow(func() time.Time { return minted.expiry.Add(time.Second) })
	if _, err := authenticator.Authenticate(ctx, "sockets", minted.token); !errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Errorf("an expired token: %v", err)
	}
	verifier.Testing_SetNow(server.NowUtc)

	for _, kind := range []sidecar.RevocationKind{sidecar.RevocationKindClient, sidecar.RevocationKindSidecar, sidecar.RevocationKindMember} {
		revocations := newTestRevocations(t)
		authenticator := NewRevocationAuthenticator("sockets", revocations.checker)
		minted := mintTestSocketsToken(t)
		if _, err := authenticator.Authenticate(ctx, "sockets", minted.token); err != nil {
			t.Fatalf("%s: before its object: %v", kind, err)
		}
		subjectId := map[sidecar.RevocationKind]server.Id{
			sidecar.RevocationKindClient:  minted.clientId,
			sidecar.RevocationKindSidecar: minted.sidecarId,
			sidecar.RevocationKindMember:  minted.memberId,
		}[kind]
		revocations.revoke(minted.networkId, kind, subjectId, server.NowUtc().Add(time.Minute))
		_, err := authenticator.Authenticate(ctx, "sockets", minted.token)
		if !errors.Is(err, sidecar.ErrAuthenticationRefused) || !errors.Is(err, sidecar.ErrRevocationVoidsToken) {
			t.Errorf("a token whose %s a rev/ object voids: %v", kind, err)
		}
	}
}

// A block made to read rev/ that cannot read it answers its audience's tokens unavailable, never refused and never
// allowed: with no reader, and with a reader whose read fails. Another audience's tokens take no rev/ check here.
func TestRevocationAuthenticatorUnavailableWithoutRev(t *testing.T) {
	installTestServiceTokenKeys(t)
	ctx := t.Context()
	minted := mintTestSocketsToken(t)

	for name, authenticator := range map[string]*Authenticator{
		"no reader":     NewRevocationAuthenticator("sockets", nil),
		"a failed read": NewRevocationAuthenticator("sockets", failingRevocations{}),
	} {
		principal, err := authenticator.Authenticate(ctx, "sockets", minted.token)
		if err == nil || principal != nil || errors.Is(err, sidecar.ErrAuthenticationRefused) {
			t.Errorf("%s: %v, %v", name, principal, err)
		}
	}

	gkmToken, _, err := oauth.MintServiceToken(&oauth.MintServiceTokenArgs{NetworkId: server.NewId(), ClientId: server.NewId(), Audience: "gkm"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewRevocationAuthenticator("sockets", nil).Authenticate(ctx, "gkm", gkmToken); err != nil {
		t.Errorf("another audience: %v", err)
	}
}

// A rev/ reader whose every read fails.
type failingRevocations struct{}

func (self failingRevocations) Check(ctx context.Context, principal *sidecar.Principal) error {
	return errors.New("the first metadata cluster cannot be read")
}

// A process with no verifier, and an audience that is no service audience, answer unavailable, not refused:
// nothing is known about the token, so the caller retries rather than sign out, and nothing is allowed.
func TestAuthenticatorUnavailableIsNotARefusal(t *testing.T) {
	broken := &Authenticator{verify: func(token string, audience string) (*oauth.ServiceTokenClaims, error) {
		return nil, errors.New("no service token verifier: oauth-jwks-svc.json is unreadable")
	}}
	principal, err := broken.Authenticate(t.Context(), "evs", "token")
	if err == nil || principal != nil || errors.Is(err, sidecar.ErrAuthenticationRefused) {
		t.Fatalf("no verifier: %v, %v", principal, err)
	}

	installTestServiceTokenKeys(t)
	for _, audience := range []string{"api", "tlsedge"} {
		principal, err := NewAuthenticator().Authenticate(t.Context(), audience, "token")
		if err == nil || principal != nil || errors.Is(err, sidecar.ErrAuthenticationRefused) {
			t.Errorf("%s: %v, %v", audience, principal, err)
		}
	}
}
