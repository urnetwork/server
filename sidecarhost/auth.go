package sidecarhost

// ServiceHost.Authenticator on server/oauth (host_auth.go; §7.8, §9.15): a service token verified offline
// against the svc- key set this process mounts (oauth.VerifyServiceToken), its claims mapped onto
// sidecar.Principal. Every engine's callers hold service tokens, sockets' included (QD-1029): sockets is a
// service audience, and its block's Authenticator also reads §7.8's rev/ objects for each token's client,
// sidecar and member (NewRevocationAuthenticator), since its engine reads none itself, where evs's and gkm's
// do through their authorization state.
//
// Refusals and unavailability stay apart, so an engine answers 401 only for a token that will not
// authenticate and unavailable for everything else:
//
//   - an empty audience, an empty token, a token the verifier refuses (signature, kid, issuer, audience,
//     expiry, missing claims), a token a rev/ object voids, and a principal that Principal.Validate refuses
//     wrap sidecar.ErrAuthenticationRefused;
//   - a process with no usable verifier, an audience that is no service audience, and a rev/ read that
//     cannot answer, or a block given no rev/ reader, answer an error that does not.
//
// Safe for concurrent use.

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
)

var _ sidecar.Authenticator = (*Authenticator)(nil)

// What reads §7.8's rev/ objects for a verified token: sidecar.RevocationChecker over the first metadata
// cluster, or a test's.
type RevocationChecker interface {
	// nil when no rev/ object voids the token's client, sidecar or member; an error wrapping
	// sidecar.ErrRevocationVoidsToken when one does; any other error when the reader cannot tell.
	Check(ctx context.Context, principal *sidecar.Principal) error
}

var _ RevocationChecker = (*sidecar.RevocationChecker)(nil)

// Verifies service tokens, and checks one audience's against rev/ when it was made to. NewAuthenticator and
// NewRevocationAuthenticator make one.
type Authenticator struct {
	verify func(token string, audience string) (*oauth.ServiceTokenClaims, error)
	// The audience whose tokens are checked against rev/ here, "" for none.
	revocationAudience string
	// nil on a block that should read rev/ and was given no reader: its audience's tokens answer unavailable.
	revocations RevocationChecker
}

// An authenticator on the process's service token verifier, which leaves rev/ to the engine.
func NewAuthenticator() *Authenticator {
	return &Authenticator{verify: oauth.VerifyServiceToken}
}

// An authenticator on the process's service token verifier that also refuses audience's tokens whose client,
// sidecar or member a rev/ object voids: sockets' block passes socketsservice.SocketsAudience and its
// RevocationChecker. A nil revocations answers audience's tokens unavailable, never an unchecked allow.
func NewRevocationAuthenticator(audience string, revocations RevocationChecker) *Authenticator {
	return &Authenticator{verify: oauth.VerifyServiceToken, revocationAudience: audience, revocations: revocations}
}

// A claim's id as the root's.
func authPrincipalId(id *server.Id) *connect.Id {
	if id == nil {
		return nil
	}
	connectId := connect.Id(*id)
	return &connectId
}

// See sidecar.Authenticator.
func (self *Authenticator) Authenticate(ctx context.Context, audience string, jwt string) (*sidecar.Principal, error) {
	if audience == "" {
		return nil, fmt.Errorf("%w: no audience", sidecar.ErrAuthenticationRefused)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !oauth.IsServiceTokenAudience(audience) {
		return nil, fmt.Errorf("sidecarhost auth: %q is no service audience", audience)
	}
	if jwt == "" {
		return nil, fmt.Errorf("%w: no token", sidecar.ErrAuthenticationRefused)
	}
	claims, err := self.verify(jwt, audience)
	if err != nil {
		if errors.Is(err, oauth.ErrInvalidServiceToken) {
			return nil, fmt.Errorf("%w: %w", sidecar.ErrAuthenticationRefused, err)
		}
		// no verifier: nothing is known about the token
		return nil, fmt.Errorf("sidecarhost auth: %w", err)
	}
	principal := &sidecar.Principal{
		NetworkId:       connect.Id(claims.NetworkId),
		ClientId:        connect.Id(claims.ClientId),
		SubjectClientId: authPrincipalId(claims.SubjectClientId),
		SidecarId:       authPrincipalId(claims.SidecarId),
		MemberId:        authPrincipalId(claims.MemberId),
		Audience:        claims.Audience(),
		Scopes:          claims.Scopes(),
		ProjVersion:     claims.ProjVersion,
	}
	if claims.IssuedAt != nil {
		principal.IssuedAt = claims.IssuedAt.Time
	}
	if claims.ExpiresAt != nil {
		principal.ExpiresAt = claims.ExpiresAt.Time
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if principal.Audience != audience {
		return nil, fmt.Errorf("%w: another audience", sidecar.ErrAuthenticationRefused)
	}
	if self.revocationAudience != "" && audience == self.revocationAudience {
		if err := self.checkRevocations(ctx, principal); err != nil {
			return nil, err
		}
	}
	return principal, nil
}

// §7.8's check row for a verified principal: refused when a rev/ object voids its client, sidecar or member,
// and unavailable when rev/ cannot be read.
func (self *Authenticator) checkRevocations(ctx context.Context, principal *sidecar.Principal) error {
	if self.revocations == nil {
		return errors.New("sidecarhost auth: this block reads no rev/ objects")
	}
	err := self.revocations.Check(ctx, principal)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sidecar.ErrRevocationVoidsToken):
		return fmt.Errorf("%w: %w", sidecar.ErrAuthenticationRefused, err)
	}
	// nothing is known about the token's subjects, so the caller retries rather than signs out
	return fmt.Errorf("sidecarhost auth: %w", err)
}
