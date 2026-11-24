package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"time"

	"github.com/urnetwork/server"
)

const (
	testingGrantRedirectUri  = "https://client.example/callback"
	testingGrantResource     = "https://mcp.bringyour.com"
	testingGrantCodeVerifier = "testing-grant-code-verifier-testing-grant-code-verifier"
)

// Testing_Grant is an application the user approved, for tests that check
// what a credential rotation does to a grant: a test client approved through
// Authorize, its code redeemed for a refresh token, and a second approval's
// code not redeemed yet.
//
// The code and the refresh token are redeemed and rotated as the token
// endpoint does it (RedeemAuthorizationCode, RotateRefreshToken), without
// minting access tokens, which needs the oauth signing keys of vault auth.yml.
type Testing_Grant struct {
	UserId       server.Id
	ClientId     string
	RefreshToken string
	PendingCode  string
}

// Testing_CreateGrant approves a new test client for the user as the consent
// page does, by a sign-in whose credential was created at credentialTime.
func Testing_CreateGrant(ctx context.Context, userId server.Id, networkId server.Id, credentialTime time.Time) (*Testing_Grant, error) {
	client := &Client{
		ClientId:        "urn_client_testing_" + server.NewId().String(),
		ClientType:      ClientTypePreregistered,
		ClientName:      "Testing Client",
		ApplicationType: ApplicationTypeWeb,
		RedirectUris:    []string{testingGrantRedirectUri},
	}
	saveClient(ctx, client, time.Time{})

	user := &AuthorizingUser{
		UserId:         userId,
		NetworkId:      networkId,
		NetworkName:    "testing",
		AuthTime:       server.NowUtc(),
		CredentialTime: credentialTime,
	}
	sum := sha256.Sum256([]byte(testingGrantCodeVerifier))
	request := &AuthorizationRequest{
		ClientId:            client.ClientId,
		RedirectUri:         testingGrantRedirectUri,
		ResponseType:        "code",
		Scopes:              []string{ScopeOfflineAccess, ScopeMcpRead},
		State:               "testing-state",
		CodeChallenge:       base64.RawURLEncoding.EncodeToString(sum[:]),
		CodeChallengeMethod: PkceMethodS256,
		Resource:            testingGrantResource,
	}

	codeStr, err := Authorize(ctx, request, user)
	if err != nil {
		return nil, err
	}
	grant := &Testing_Grant{
		UserId:   userId,
		ClientId: client.ClientId,
	}
	code, err := grant.redeem(ctx, codeStr)
	if err != nil {
		return nil, err
	}
	// what the token endpoint issues for offline_access (issueTokens)
	grant.RefreshToken, err = CreateRefreshToken(ctx, &RefreshToken{
		ClientId:  code.ClientId,
		UserId:    code.UserId,
		NetworkId: code.NetworkId,
		Resource:  code.Resource,
		Scopes:    code.Scopes,
		Principal: code.Principal,
		Roles:     code.Roles,
		AuthTime:  code.AuthTime,
	})
	if err != nil {
		return nil, err
	}

	grant.PendingCode, err = Authorize(ctx, request, user)
	if err != nil {
		return nil, err
	}
	return grant, nil
}

func (self *Testing_Grant) redeem(ctx context.Context, codeStr string) (*AuthorizationCode, error) {
	return RedeemAuthorizationCode(
		ctx,
		codeStr,
		self.ClientId,
		testingGrantRedirectUri,
		testingGrantCodeVerifier,
		testingGrantResource,
	)
}

// Testing_Refresh rotates the grant's refresh token and keeps the successor.
func (self *Testing_Grant) Testing_Refresh(ctx context.Context) error {
	_, next, err := RotateRefreshToken(ctx, self.RefreshToken, self.ClientId, nil)
	if err != nil {
		return err
	}
	self.RefreshToken = next
	return nil
}

// Testing_RedeemPendingCode redeems the second approval's code.
func (self *Testing_Grant) Testing_RedeemPendingCode(ctx context.Context) error {
	_, err := self.redeem(ctx, self.PendingCode)
	return err
}

// Testing_Remembered reports whether the approval is remembered, so that the
// consent page would approve the client again without asking.
func (self *Testing_Grant) Testing_Remembered(ctx context.Context) bool {
	return ConsentSatisfies(ctx, self.UserId, self.ClientId, []string{ScopeMcpRead})
}
