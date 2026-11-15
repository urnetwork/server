package oauth

// RFC 8628's device grant for ur-cli (device_grant.go; TEAM-M6-08, D-TEAM-7), over the in-memory store and
// production's on the test stack's Redis (device_grant_store.go; QD-830): a code polled while pending and too fast,
// approved only with the consent receipt of a page that showed the command and its code on a fresh sign-in, redeemed
// once with a proof by the request's key, denied, and expired; a code the store no longer holds answers expired_token;
// the consent page's answer and a poll that race, on any instance, never undo each other; a user code in use is drawn
// again; and with no store the grant is unavailable and the metadata names no device endpoint. A team's token naming
// the member needs MG6 (device_grant_team_test.go).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// A device authorization store in memory, beside Redis's: it keeps an authorization past its life until a poll gives
// it up, as a store with no ttl does, and its versions are a counter's values. Safe for concurrent use.
type testDeviceStore struct {
	stateLock sync.Mutex
	entries   map[[sha256.Size]byte]*testDeviceStoreEntry
	revision  uint64
}

type testDeviceStoreEntry struct {
	authorization DeviceAuthorization
	version       []byte
}

func newTestDeviceStore() *testDeviceStore {
	return &testDeviceStore{entries: map[[sha256.Size]byte]*testDeviceStoreEntry{}}
}

// A new entry holding a copy of authorization, under the next version.
func (self *testDeviceStore) newEntryWithLock(authorization *DeviceAuthorization) *testDeviceStoreEntry {
	self.revision += 1
	entry := &testDeviceStoreEntry{
		authorization: *authorization,
		version:       []byte(strconv.FormatUint(self.revision, 10)),
	}
	entry.authorization.Scopes = slices.Clone(authorization.Scopes)
	entry.authorization.storeVersion = nil
	return entry
}

// A copy of the entry's authorization, with its version.
func (self *testDeviceStoreEntry) answer() *DeviceAuthorization {
	authorization := self.authorization
	authorization.Scopes = slices.Clone(self.authorization.Scopes)
	authorization.storeVersion = self.version
	return &authorization
}

func (self *testDeviceStore) Put(ctx context.Context, authorization *DeviceAuthorization) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, found := self.entries[authorization.DeviceCodeSha256]; found {
		return errors.New("in use")
	}
	for _, entry := range self.entries {
		if entry.authorization.UserCode == authorization.UserCode {
			return ErrDeviceUserCodeInUse
		}
	}
	self.entries[authorization.DeviceCodeSha256] = self.newEntryWithLock(authorization)
	return nil
}

func (self *testDeviceStore) Get(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, found := self.entries[deviceCodeSha256]
	if !found {
		return nil, nil
	}
	return entry.answer(), nil
}

func (self *testDeviceStore) GetByUserCode(ctx context.Context, userCode string) (*DeviceAuthorization, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, entry := range self.entries {
		if entry.authorization.UserCode == userCode {
			return entry.answer(), nil
		}
	}
	return nil, nil
}

func (self *testDeviceStore) Swap(ctx context.Context, read *DeviceAuthorization, next *DeviceAuthorization) (bool, error) {
	if read.storeVersion == nil || next.DeviceCodeSha256 != read.DeviceCodeSha256 || next.UserCode != read.UserCode || !next.ExpireTime.Equal(read.ExpireTime) {
		return false, errors.New("not a swap")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, found := self.entries[read.DeviceCodeSha256]
	if !found || !bytes.Equal(entry.version, read.storeVersion) {
		return false, nil
	}
	self.entries[read.DeviceCodeSha256] = self.newEntryWithLock(next)
	return true, nil
}

func (self *testDeviceStore) Take(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, found := self.entries[deviceCodeSha256]
	if !found {
		return nil, nil
	}
	delete(self.entries, deviceCodeSha256)
	return entry.answer(), nil
}

// The stores the grant's tests run over: the in-memory one and production's, on the test's Redis.
func testDeviceStores() []DeviceAuthorizationStore {
	return []DeviceAuthorizationStore{newTestDeviceStore(), NewRedisDeviceAuthorizationStore(DefaultDeviceGrantSettings())}
}

// A store whose reads each run a hook once, after the read, as another instance's write lands between a step's read
// and its write. The hooks are not safe for concurrent use.
type testHookedDeviceStore struct {
	DeviceAuthorizationStore
	afterGet           func()
	afterGetByUserCode func()
}

func (self *testHookedDeviceStore) Get(ctx context.Context, deviceCodeSha256 [sha256.Size]byte) (*DeviceAuthorization, error) {
	authorization, err := self.DeviceAuthorizationStore.Get(ctx, deviceCodeSha256)
	if hook := self.afterGet; hook != nil {
		self.afterGet = nil
		hook()
	}
	return authorization, err
}

func (self *testHookedDeviceStore) GetByUserCode(ctx context.Context, userCode string) (*DeviceAuthorization, error) {
	authorization, err := self.DeviceAuthorizationStore.GetByUserCode(ctx, userCode)
	if hook := self.afterGetByUserCode; hook != nil {
		self.afterGetByUserCode = nil
		hook()
	}
	return authorization, err
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

func (self *testDeviceLogin) deviceCodeSha256() [sha256.Size]byte {
	return sha256.Sum256([]byte(self.response.DeviceCode))
}

// The consent page's approval of the login for user, with the receipt its description gave.
func (self *testDeviceLogin) approve(ctx context.Context, user *AuthorizingUser, settings *DeviceGrantSettings) error {
	consent, err := describeDeviceConsentAt(ctx, self.response.UserCode, user.UserId, user.AuthTime, settings, server.NowUtc())
	if err != nil {
		return err
	}
	return decideDeviceAt(ctx, self.response.UserCode, true, consent.Receipt, user, settings, server.NowUtc)
}

// The poll: pending, then slow_down for a poll inside the interval; approval only with the receipt of a consent that
// showed this command and its code, on a fresh sign-in; then the admin token, bound to the command and the request's
// key, once, a second poll finding no code; the metadata names the device endpoint while a store is installed.
func TestDeviceGrantPollsUntilApprovedAndRedeemsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		for _, store := range testDeviceStores() {
			SetDeviceAuthorizationStore(store)
			now := server.NowUtc()
			device := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
			connect.AssertEqual(t, device.response.VerificationUri, DeviceVerificationUri())
			connect.AssertEqual(t, len(device.response.UserCode), 9)
			connect.AssertEqual(t, ServerMetadata().DeviceAuthorizationEndpoint, DeviceAuthorizationEndpoint())

			if _, err := device.poll(ctx, now, device.proof()); !errors.Is(err, ErrAuthorizationPending) {
				t.Fatalf("%T: the first poll: %v", store, err)
			}
			if _, err := device.poll(ctx, now.Add(time.Second), device.proof()); !errors.Is(err, ErrSlowDown) {
				t.Fatalf("%T: a poll inside the interval: %v", store, err)
			}

			user := newTestUrcliUser(t, ctx)
			if err := decideDeviceAt(ctx, device.response.UserCode, true, "", user, DefaultDeviceGrantSettings(), server.NowUtc); !errors.Is(err, ErrConsentRequired) {
				t.Fatalf("%T: an approval with no receipt: %v", store, err)
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
				t.Fatalf("%T: an approval on a stale sign-in: %v", store, err)
			}
			if err := decideDeviceAt(ctx, device.response.UserCode, true, consent.Receipt, user, DefaultDeviceGrantSettings(), server.NowUtc); err != nil {
				t.Fatal(err)
			}
			// an approved authorization waits for no second answer
			if err := decideDeviceAt(ctx, device.response.UserCode, false, "", user, DefaultDeviceGrantSettings(), server.NowUtc); !errors.Is(err, ErrInvalidGrant) {
				t.Fatalf("%T: a denial after the approval: %v", store, err)
			}

			if _, err := device.poll(ctx, now.Add(time.Minute)); !errors.Is(err, ErrInvalidDpopProof) {
				t.Fatalf("%T: a poll with no proof: %v", store, err)
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
			if _, err := device.poll(ctx, now.Add(2*time.Minute), device.proof()); !errors.Is(err, ErrExpiredToken) {
				t.Errorf("%T: a second redemption: %v", store, err)
			}
		}
	})
}

// A denial answers access_denied once, then the code is gone; a code past its life answers expired_token; a code
// never issued expired_token; and an unknown user code invalid_grant.
func TestDeviceGrantDeniedAndExpired(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		for _, store := range testDeviceStores() {
			SetDeviceAuthorizationStore(store)
			now := server.NowUtc()
			denied := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
			user := newTestUrcliUser(t, ctx)
			if err := decideDeviceAt(ctx, denied.response.UserCode, false, "", user, DefaultDeviceGrantSettings(), server.NowUtc); err != nil {
				t.Fatal(err)
			}
			if _, err := denied.poll(ctx, now, denied.proof()); !errors.Is(err, ErrAccessDenied) {
				t.Errorf("%T: a denied code: %v", store, err)
			}
			if _, err := denied.poll(ctx, now.Add(time.Minute), denied.proof()); !errors.Is(err, ErrExpiredToken) {
				t.Errorf("%T: a denied code polled again: %v", store, err)
			}
			expired := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
			if _, err := expired.poll(ctx, now.Add(DefaultDeviceGrantSettings().CodeDuration), expired.proof()); !errors.Is(err, ErrExpiredToken) {
				t.Errorf("%T: an expired code: %v", store, err)
			}
			if _, err := exchangeDeviceCodeAt(ctx, &ExchangeDeviceCodeArgs{ClientId: UrcliClientId, DeviceCode: "never-issued", Resource: testUrcliApiOrigin}, nil, DefaultDeviceGrantSettings(), now); !errors.Is(err, ErrExpiredToken) {
				t.Errorf("%T: a code never issued: %v", store, err)
			}
			if _, err := describeDeviceConsentAt(ctx, "BCDF-GHJK", user.UserId, user.AuthTime, DefaultDeviceGrantSettings(), now); !errors.Is(err, ErrInvalidGrant) {
				t.Errorf("%T: an unknown user code: %v", store, err)
			}
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
	if _, err := DescribeDeviceConsent(ctx, "BCDF-GHJK", server.NewId(), server.NowUtc()); !errors.Is(err, ErrDeviceGrantUnavailable) {
		t.Errorf("consent: %v", err)
	}
	if err := DecideDevice(ctx, "BCDF-GHJK", false, "", &AuthorizingUser{UserId: server.NewId()}); !errors.Is(err, ErrDeviceGrantUnavailable) {
		t.Errorf("answer: %v", err)
	}
	connect.AssertEqual(t, ServerMetadata().DeviceAuthorizationEndpoint, "")
	for _, grantType := range ServerMetadata().GrantTypesSupported {
		connect.AssertEqual(t, grantType != DeviceCodeGrantType, true)
	}
}

// The consent page approves between a poll's read and its write: the poll's write is refused and it answers
// slow_down, recording nothing, and the approval stands for the next poll, which redeems.
func TestDeviceGrantApprovalSurvivesARacingPoll(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		for _, store := range testDeviceStores() {
			hooked := &testHookedDeviceStore{DeviceAuthorizationStore: store}
			SetDeviceAuthorizationStore(hooked)
			now := server.NowUtc()
			device := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
			user := newTestUrcliUser(t, ctx)
			hooked.afterGet = func() {
				if err := device.approve(ctx, user, DefaultDeviceGrantSettings()); err != nil {
					t.Fatalf("%T: the approval inside the poll: %v", store, err)
				}
			}
			if _, err := device.poll(ctx, now, device.proof()); !errors.Is(err, ErrSlowDown) {
				t.Fatalf("%T: the poll that raced the approval: %v", store, err)
			}
			authorization, err := store.Get(ctx, device.deviceCodeSha256())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, authorization.Approved, true)
			connect.AssertEqual(t, authorization.UserId, user.UserId)
			connect.AssertEqual(t, authorization.LastPollTime.IsZero(), true)
			response, err := device.poll(ctx, now.Add(time.Minute), device.proof())
			connect.AssertEqual(t, err, nil)
			claims, err := VerifyAdminToken(response.AccessToken)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, claims.MemberId, user.UserId)
		}
	})
}

// A poll writes between the consent page's read and its write: the answer reads again and lands on what the poll
// left; and an authorization that keeps changing under the answer, past the attempts settings allow, is answered
// temporarily_unavailable, approving nothing.
func TestDeviceGrantApprovalRereadsAfterARacingPoll(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		for _, store := range testDeviceStores() {
			hooked := &testHookedDeviceStore{DeviceAuthorizationStore: store}
			SetDeviceAuthorizationStore(hooked)
			now := server.NowUtc()
			device := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
			user := newTestUrcliUser(t, ctx)
			consent, err := describeDeviceConsentAt(ctx, device.response.UserCode, user.UserId, user.AuthTime, DefaultDeviceGrantSettings(), server.NowUtc())
			connect.AssertEqual(t, err, nil)
			hooked.afterGetByUserCode = func() {
				if _, err := device.poll(ctx, now, device.proof()); !errors.Is(err, ErrAuthorizationPending) {
					t.Fatalf("%T: the poll inside the answer: %v", store, err)
				}
			}
			if err := decideDeviceAt(ctx, device.response.UserCode, true, consent.Receipt, user, DefaultDeviceGrantSettings(), server.NowUtc); err != nil {
				t.Fatalf("%T: the answer a poll raced: %v", store, err)
			}
			authorization, err := store.Get(ctx, device.deviceCodeSha256())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, authorization.Approved, true)
			// the poll's bookkeeping stands under the approval
			connect.AssertEqual(t, authorization.LastPollTime.Equal(now), true)

			busy := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
			consent, err = describeDeviceConsentAt(ctx, busy.response.UserCode, user.UserId, user.AuthTime, DefaultDeviceGrantSettings(), server.NowUtc())
			connect.AssertEqual(t, err, nil)
			settings := DefaultDeviceGrantSettings()
			settings.DecisionAttemptCount = 2
			pollCount := 0
			var interrupt func()
			interrupt = func() {
				pollCount += 1
				// each poll writes the authorization, the second as a slow_down
				busy.poll(ctx, now.Add(time.Duration(pollCount)*time.Millisecond), busy.proof())
				hooked.afterGetByUserCode = interrupt
			}
			hooked.afterGetByUserCode = interrupt
			if err := decideDeviceAt(ctx, busy.response.UserCode, true, consent.Receipt, user, settings, server.NowUtc); !errors.Is(err, ErrDeviceGrantUnavailable) {
				t.Fatalf("%T: an answer under polls that never stop: %v", store, err)
			}
			hooked.afterGetByUserCode = nil
			connect.AssertEqual(t, pollCount, settings.DecisionAttemptCount)
			authorization, err = store.Get(ctx, busy.deviceCodeSha256())
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, authorization.Approved, false)
		}
	})
}

// A user code another live authorization holds is drawn again, so a code names one authorization; when every draw
// settings allow is in use, the authorization is refused and nothing of it is kept.
func TestDeviceGrantDrawsAnotherUserCodeWhileOneIsInUse(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		settings := DefaultDeviceGrantSettings()
		// a device code's bytes all of one value, then user code draws, each of one letter: 0 draws B, 1 draws C
		random := func(deviceCodeByte byte, userCodeBytes ...byte) *bytes.Reader {
			draws := bytes.Repeat([]byte{deviceCodeByte}, settings.DeviceCodeByteCount)
			for _, userCodeByte := range userCodeBytes {
				draws = append(draws, bytes.Repeat([]byte{userCodeByte}, settings.UserCodeLength)...)
			}
			return bytes.NewReader(draws)
		}
		authorize := func(random *bytes.Reader) (*DeviceAuthorizationResponse, error) {
			login := newTestUrcliLogin(t, testUrcliCommand())
			return authorizeDeviceAt(ctx, &DeviceAuthorizationArgs{
				ClientId:             UrcliClientId,
				Scopes:               login.request.Scopes,
				Resource:             testUrcliApiOrigin,
				DpopJkt:              login.request.DpopJkt,
				AuthorizationDetails: login.request.AuthorizationDetails,
			}, settings, random, server.NowUtc())
		}
		for _, store := range testDeviceStores() {
			SetDeviceAuthorizationStore(store)
			first, err := authorize(random(1, 0))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, first.UserCode, "BBBB-BBBB")
			second, err := authorize(random(2, 0, 1))
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, second.UserCode, "CCCC-CCCC")
			for _, response := range []*DeviceAuthorizationResponse{first, second} {
				authorization, err := store.GetByUserCode(ctx, strings.ReplaceAll(response.UserCode, "-", ""))
				connect.AssertEqual(t, err, nil)
				connect.AssertEqual(t, authorization.DeviceCodeSha256, sha256.Sum256([]byte(response.DeviceCode)))
			}

			inUse := bytes.Repeat([]byte{0}, settings.UserCodeAttemptCount)
			if _, err := authorize(random(3, inUse...)); !errors.Is(err, ErrDeviceUserCodeInUse) {
				t.Fatalf("%T: every draw in use: %v", store, err)
			}
			refused, err := store.Get(ctx, sha256.Sum256([]byte(base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, settings.DeviceCodeByteCount)))))
			if refused != nil || err != nil {
				t.Fatalf("%T: a refused authorization was kept: %+v %v", store, refused, err)
			}
			authorization, err := store.GetByUserCode(ctx, "BBBBBBBB")
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, authorization.DeviceCodeSha256, sha256.Sum256([]byte(first.DeviceCode)))
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
