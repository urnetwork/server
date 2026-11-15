//go:build sidecar_pending

package oauth

// RFC 8628's device grant for ur-cli (device_grant.go; TEAM-M6-08, D-TEAM-7), which needs MG6 for its team case: a
// code polled while pending and too fast, approved only with the consent receipt of a page that showed the command
// and its code on a fresh sign-in, redeemed once with a proof by the request's key, denied, and expired; with no store
// the grant is unavailable and the metadata names no device endpoint; and a team's token names the member. The store
// is the test's, since where device authorizations wait is open (questions/team-m6-08-device-grant-row.md).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// A device authorization store in memory, safe for concurrent use.
type testDeviceStore struct {
	stateLock      sync.Mutex
	authorizations map[[sha256.Size]byte]*DeviceAuthorization
}

func newTestDeviceStore() *testDeviceStore {
	return &testDeviceStore{authorizations: map[[sha256.Size]byte]*DeviceAuthorization{}}
}

func (self *testDeviceStore) Put(ctx context.Context, authorization *DeviceAuthorization) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, found := self.authorizations[authorization.DeviceCodeSha256]; found {
		return errors.New("in use")
	}
	copied := *authorization
	self.authorizations[authorization.DeviceCodeSha256] = &copied
	return nil
}

func (self *testDeviceStore) Get(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	authorization, found := self.authorizations[deviceCodeSha256]
	if !found {
		return nil, nil
	}
	copied := *authorization
	return &copied, nil
}

func (self *testDeviceStore) GetByUserCode(ctx context.Context, userCode string) (*DeviceAuthorization, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, authorization := range self.authorizations {
		if authorization.UserCode == userCode {
			copied := *authorization
			return &copied, nil
		}
	}
	return nil, nil
}

func (self *testDeviceStore) Update(ctx context.Context, authorization *DeviceAuthorization) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, found := self.authorizations[authorization.DeviceCodeSha256]; !found {
		return errors.New("gone")
	}
	copied := *authorization
	self.authorizations[authorization.DeviceCodeSha256] = &copied
	return nil
}

func (self *testDeviceStore) Take(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	_, found := self.authorizations[deviceCodeSha256]
	delete(self.authorizations, deviceCodeSha256)
	return found, nil
}

// A device login: ur-cli's request, the authorization it started, and its key.
type testDeviceLogin struct {
	login    *testUrcliLogin
	response *DeviceAuthorizationResponse
}

func startTestDeviceLogin(t testing.TB, ctx context.Context, command *sidecar.AdminCommand, now time.Time) *testDeviceLogin {
	t.Helper()
	login := newTestUrcliLogin(t, command)
	login.request.Scopes = []string{urcliCommandScope(command)}
	response, err := authorizeDeviceAt(ctx, &DeviceAuthorizationArgs{
		ClientId:             UrcliClientId,
		Scopes:               login.request.Scopes,
		Resource:             testUrcliApiOrigin,
		DpopJkt:              login.request.DpopJkt,
		AuthorizationDetails: login.request.AuthorizationDetails,
	}, DefaultDeviceGrantSettings(), rand.Reader, now)
	if err != nil {
		t.Fatal(err)
	}
	return &testDeviceLogin{login: login, response: response}
}

func (self *testDeviceLogin) poll(ctx context.Context, now time.Time, proofs ...string) (*TokenResponse, error) {
	return exchangeDeviceCodeAt(ctx, &ExchangeDeviceCodeArgs{ClientId: UrcliClientId, DeviceCode: self.response.DeviceCode, Resource: testUrcliApiOrigin}, proofs, DefaultDeviceGrantSettings(), now)
}

func (self *testDeviceLogin) proof() string {
	return session.Testing_SignDpopProof(self.login.privateKey, "POST", TokenEndpoint(), self.response.DeviceCode, server.NewId().String(), time.Now())
}

// The poll: pending, then slow_down for a poll inside the interval; approval only with the receipt of a consent that
// showed this command and its code, on a fresh sign-in; then the admin token, bound to the command and the request's
// key, once; the metadata names the device endpoint while a store is installed.
func TestDeviceGrantPollsUntilApprovedAndRedeemsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		SetDeviceAuthorizationStore(newTestDeviceStore())
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		now := server.NowUtc()
		device := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
		connect.AssertEqual(t, device.response.VerificationUri, DeviceVerificationUri())
		connect.AssertEqual(t, len(device.response.UserCode), 9)
		connect.AssertEqual(t, ServerMetadata().DeviceAuthorizationEndpoint, DeviceAuthorizationEndpoint())

		if _, err := device.poll(ctx, now, device.proof()); !errors.Is(err, ErrAuthorizationPending) {
			t.Fatalf("the first poll: %v", err)
		}
		if _, err := device.poll(ctx, now.Add(time.Second), device.proof()); !errors.Is(err, ErrSlowDown) {
			t.Fatalf("a poll inside the interval: %v", err)
		}

		user := newTestUrcliUser(t, ctx)
		if err := decideDeviceAt(ctx, device.response.UserCode, true, "", user, DefaultDeviceGrantSettings(), server.NowUtc); !errors.Is(err, ErrConsentRequired) {
			t.Fatalf("an approval with no receipt: %v", err)
		}
		consent, err := describeDeviceConsentAt(ctx, strings.ToLower(device.response.UserCode), user.UserId, user.AuthTime, DefaultDeviceGrantSettings(), server.NowUtc())
		connect.AssertEqual(t, err, nil)
		// the page shows the command and the code the terminal shows
		details, err := sidecar.ParseAdminCommandDetails(device.login.request.AuthorizationDetails)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, consent.ConsentCode, details.ConsentCode)
		connect.AssertEqual(t, consent.Path, "/network/auth-keys")
		stale := *user
		stale.AuthTime = server.NowUtc().Add(-time.Hour)
		if err := decideDeviceAt(ctx, device.response.UserCode, true, consent.Receipt, &stale, DefaultDeviceGrantSettings(), server.NowUtc); !errors.Is(err, ErrUrcliReauthenticationRequired) {
			t.Fatalf("an approval on a stale sign-in: %v", err)
		}
		if err := decideDeviceAt(ctx, device.response.UserCode, true, consent.Receipt, user, DefaultDeviceGrantSettings(), server.NowUtc); err != nil {
			t.Fatal(err)
		}

		if _, err := device.poll(ctx, now.Add(time.Minute)); !errors.Is(err, ErrInvalidDpopProof) {
			t.Fatalf("a poll with no proof: %v", err)
		}
		response, err := device.poll(ctx, now.Add(time.Minute), device.proof())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, response.TokenType, adminTokenResponseType)
		claims, err := VerifyAdminToken(response.AccessToken)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Confirmation.Jkt, device.login.jkt)
		cmd, err := testUrcliCommand().Digest()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.Cmd, cmd)
		connect.AssertEqual(t, claims.MemberId, user.UserId)
		if _, err := device.poll(ctx, now.Add(2*time.Minute), device.proof()); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("a second redemption: %v", err)
		}
	})
}

// A denial answers access_denied, a code past its life expired_token; a code no store holds invalid_grant.
func TestDeviceGrantDeniedAndExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		SetDeviceAuthorizationStore(newTestDeviceStore())
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		now := server.NowUtc()
		denied := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
		user := newTestUrcliUser(t, ctx)
		if err := decideDeviceAt(ctx, denied.response.UserCode, false, "", user, DefaultDeviceGrantSettings(), server.NowUtc); err != nil {
			t.Fatal(err)
		}
		if _, err := denied.poll(ctx, now, denied.proof()); !errors.Is(err, ErrAccessDenied) {
			t.Errorf("a denied code: %v", err)
		}
		if _, err := denied.poll(ctx, now.Add(time.Minute), denied.proof()); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("a denied code polled again: %v", err)
		}
		expired := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
		if _, err := expired.poll(ctx, now.Add(DefaultDeviceGrantSettings().CodeDuration), expired.proof()); !errors.Is(err, ErrExpiredToken) {
			t.Errorf("an expired code: %v", err)
		}
		if _, err := describeDeviceConsentAt(ctx, "BCDF-GHJK", user.UserId, user.AuthTime, DefaultDeviceGrantSettings(), now); !errors.Is(err, ErrInvalidGrant) {
			t.Errorf("an unknown user code: %v", err)
		}
	})
}

// With no store the grant is unavailable at each step and the metadata names no device endpoint, which ur-cli
// reports rather than polling.
func TestDeviceGrantUnavailableWithoutAStore(t *testing.T) {
	installUrcliTest(t)
	SetDeviceAuthorizationStore(nil)
	ctx := context.Background()
	login := newTestUrcliLogin(t, testUrcliCommand())
	if _, err := AuthorizeDevice(ctx, &DeviceAuthorizationArgs{ClientId: UrcliClientId, Scopes: login.request.Scopes, Resource: testUrcliApiOrigin, DpopJkt: login.request.DpopJkt, AuthorizationDetails: login.request.AuthorizationDetails}); !errors.Is(err, ErrDeviceGrantUnavailable) {
		t.Errorf("authorize: %v", err)
	}
	if _, err := ExchangeDeviceCode(ctx, &ExchangeDeviceCodeArgs{ClientId: UrcliClientId, DeviceCode: "x"}, nil); !errors.Is(err, ErrDeviceGrantUnavailable) {
		t.Errorf("poll: %v", err)
	}
	connect.AssertEqual(t, ServerMetadata().DeviceAuthorizationEndpoint, "")
	for _, grantType := range ServerMetadata().GrantTypesSupported {
		connect.AssertEqual(t, grantType != DeviceCodeGrantType, true)
	}
}

// In a team the admin token names the person's member_id, which the role gate reads, never the user; a person who is
// no live member of the team gets none.
func TestDeviceGrantNamesTheMemberInATeam(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		SetDeviceAuthorizationStore(newTestDeviceStore())
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		_, restore := model.Testing_InMemoryNetworkNameSearch(ctx)
		defer restore()
		personalNetworkId, userId := server.NewId(), server.NewId()
		suffix := strings.ReplaceAll(server.NewId().String(), "-", "")[24:32]
		model.Testing_CreateNetwork(ctx, personalNetworkId, fmt.Sprintf("device-owner-%s", suffix), userId)
		personal := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: personalNetworkId, UserId: userId})
		team, err := model.CreateNetworkTeam(&model.CreateNetworkTeamArgs{NetworkName: fmt.Sprintf("device-team-%s", suffix)}, personal)
		if err != nil || team.Error != nil {
			t.Fatalf("team-create: %v %+v", err, team)
		}
		command := &sidecar.AdminCommand{Method: "POST", Path: "/network/member-remove", Body: []byte(`{"member_id":"` + server.NewId().String() + `"}`)}
		session.RegisterAdminCommandRoute("POST", "/network/member-remove")
		now := server.NowUtc()
		device := startTestDeviceLogin(t, ctx, command, now)
		user := &AuthorizingUser{UserId: userId, NetworkId: *team.NetworkId, AuthTime: server.NowUtc()}
		consent, err := describeDeviceConsentAt(ctx, device.response.UserCode, user.UserId, user.AuthTime, DefaultDeviceGrantSettings(), now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, decideDeviceAt(ctx, device.response.UserCode, true, consent.Receipt, user, DefaultDeviceGrantSettings(), server.NowUtc), nil)
		response, err := device.poll(ctx, now.Add(time.Minute), device.proof())
		connect.AssertEqual(t, err, nil)
		claims, err := VerifyAdminToken(response.AccessToken)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, claims.MemberId, *team.MemberId)
		connect.AssertEqual(t, claims.Scope, UrcliScopeAll)

		stranger := startTestDeviceLogin(t, ctx, command, now)
		strangerUser := &AuthorizingUser{UserId: server.NewId(), NetworkId: *team.NetworkId, AuthTime: server.NowUtc()}
		consent, err = describeDeviceConsentAt(ctx, stranger.response.UserCode, strangerUser.UserId, strangerUser.AuthTime, DefaultDeviceGrantSettings(), now)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, decideDeviceAt(ctx, stranger.response.UserCode, true, consent.Receipt, strangerUser, DefaultDeviceGrantSettings(), server.NowUtc), nil)
		if _, err := stranger.poll(ctx, now.Add(time.Minute), stranger.proof()); !errors.Is(err, ErrUnauthorizedUser) {
			t.Errorf("a person who is no member: %v", err)
		}
	})
}

// Full `ur admin`: ur-cli runs every route whose model spends an admin token's use, under the all scope, with a body
// that is one JSON object; auth_keys stays v0's key's scope; a route that spends no use is refused.
func TestUrcliRunsEveryAdminCommandRoute(t *testing.T) {
	installUrcliTest(t)
	session.RegisterAdminCommandRoute("POST", "/network/member-remove")
	command := &sidecar.AdminCommand{Method: "POST", Path: "/network/member-remove", Body: []byte(`{"member_id":"` + server.NewId().String() + `"}`)}
	login := newTestUrcliLogin(t, command)
	login.request.Scopes = []string{UrcliScopeAll}
	if _, err := ValidateUrcliRequest(login.request); err != nil {
		t.Fatalf("a Team route under all: %v", err)
	}
	login.request.Scopes = []string{UrcliScopeAuthKeys}
	if _, err := ValidateUrcliRequest(login.request); !errors.Is(err, ErrUrcliCommandRefused) {
		t.Errorf("a Team route under auth_keys: %v", err)
	}
	notObject := &sidecar.AdminCommand{Method: "POST", Path: "/network/member-remove", Body: []byte(`["member"]`)}
	login = newTestUrcliLogin(t, notObject)
	login.request.Scopes = []string{UrcliScopeAll}
	if _, err := ValidateUrcliRequest(login.request); !errors.Is(err, ErrUrcliCommandRefused) {
		t.Errorf("a body that is no object: %v", err)
	}
	unregistered := &sidecar.AdminCommand{Method: "POST", Path: "/network/rename-everything", Body: []byte(`{}`)}
	login = newTestUrcliLogin(t, unregistered)
	login.request.Scopes = []string{UrcliScopeAll}
	if _, err := ValidateUrcliRequest(login.request); !errors.Is(err, ErrUrcliCommandRefused) {
		t.Errorf("a route that spends no use: %v", err)
	}
}
