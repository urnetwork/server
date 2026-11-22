package oauth

// The role gate's recognizers of `ur login`'s one command (urcli_own_key.go; §7.5's Member: own devices): only ur-cli's
// single-use key command under the auth_keys scope alone passes, at the consent page's routes and at the device grant's,
// and the body stays readable for the handler.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// A consent request as the consent page posts it for login's request.
func testUrcliConsentBody(t testing.TB, request *AuthorizationRequest) []byte {
	t.Helper()
	body, err := json.Marshal(&ConsentRequest{
		ClientId:             request.ClientId,
		RedirectUri:          request.RedirectUri,
		ResponseType:         request.ResponseType,
		Scope:                FormatScope(request.Scopes),
		State:                request.State,
		CodeChallenge:        request.CodeChallenge,
		CodeChallengeMethod:  request.CodeChallengeMethod,
		Resource:             request.Resource,
		DpopJkt:              request.DpopJkt,
		AuthorizationDetails: request.AuthorizationDetails,
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// `ur login`'s request is the one command passed; a request of another client or scope, a key with a tag, ephemeral or
// pre-approved field, more than one use or a longer life, another command or no command is not. Read from the consent
// page's body, it passes and the handler reads the body again; a body past the consent request's bound or none passes
// nothing.
func TestUrcliOwnKeyRequestIsOnlyLoginsCommand(t *testing.T) {
	if !isUrcliOwnKeyRequest(newTestUrcliLogin(t, testUrcliCommand()).request) {
		t.Fatal("ur login's request is not recognized")
	}
	for i, alter := range []func(login *testUrcliLogin){
		func(login *testUrcliLogin) { login.request.ClientId = "another-client" },
		func(login *testUrcliLogin) { login.request.Scopes = []string{UrcliScopeAll} },
		func(login *testUrcliLogin) { login.request.Scopes = []string{UrcliScopeAuthKeys, UrcliScopeAll} },
		func(login *testUrcliLogin) { login.request.Scopes = nil },
		func(login *testUrcliLogin) { login.request.AuthorizationDetails = "" },
	} {
		login := newTestUrcliLogin(t, testUrcliCommand())
		alter(login)
		if isUrcliOwnKeyRequest(login.request) {
			t.Errorf("alteration %d is recognized", i)
		}
	}
	for _, body := range []string{
		`{"duration_seconds":300,"max_uses":1,"tags":["` + strings.Repeat("ab", 16) + `"]}`,
		`{"duration_seconds":300,"max_uses":1,"ephemeral":true}`,
		`{"duration_seconds":300,"max_uses":1,"pre_approved":true}`,
		`{"duration_seconds":300,"max_uses":2}`,
		`{"duration_seconds":301,"max_uses":1}`,
	} {
		command := testUrcliCommand()
		command.Body = []byte(body)
		if isUrcliOwnKeyRequest(newTestUrcliLogin(t, command).request) {
			t.Errorf("a key of %s is recognized", body)
		}
	}
	for _, command := range []*sidecar.AdminCommand{
		{Method: "POST", Path: "/network/member-invite", Body: []byte(`{"email":"invitee@example.com","role":"admin","expire_days":7}`)},
		{Method: "GET", Path: "/network/auth-keys"},
		{Method: "POST", Path: "/network/auth-key-revoke", Body: []byte(`{"auth_key_id":"` + server.NewId().String() + `"}`)},
	} {
		// under the auth_keys scope too, so the command alone decides
		if isUrcliOwnKeyRequest(newTestUrcliLogin(t, command).request) {
			t.Errorf("%s %s is recognized", command.Method, command.Path)
		}
	}

	body := testUrcliConsentBody(t, newTestUrcliLogin(t, testUrcliCommand()).request)
	r := httptest.NewRequest(http.MethodPost, "/oauth/consent", bytes.NewReader(body))
	if !IsUrcliOwnKeyRequest(r) {
		t.Fatal("ur login's consent body is not recognized")
	}
	if left, err := io.ReadAll(r.Body); err != nil || !bytes.Equal(left, body) {
		t.Fatalf("the handler reads %q (%v), not the body", left, err)
	}
	other := newTestUrcliLogin(t, &sidecar.AdminCommand{Method: "POST", Path: "/network/member-invite", Body: []byte(`{"email":"invitee@example.com","role":"admin","expire_days":7}`)})
	other.request.Scopes = []string{UrcliScopeAll}
	if IsUrcliOwnKeyRequest(httptest.NewRequest(http.MethodPost, "/oauth/consent", bytes.NewReader(testUrcliConsentBody(t, other.request)))) {
		t.Fatal("an all command's consent body is recognized")
	}
	long := append(bytes.Clone(body), bytes.Repeat([]byte(" "), int(DefaultOauthHandlerSettings().ConsentRequestMaxByteCount))...)
	if IsUrcliOwnKeyRequest(httptest.NewRequest(http.MethodPost, "/oauth/consent", bytes.NewReader(long))) {
		t.Fatal("a body past the consent request's bound is recognized")
	}
	empty := httptest.NewRequest(http.MethodPost, "/oauth/consent", nil)
	empty.Body = nil
	if IsUrcliOwnKeyRequest(empty) {
		t.Fatal("a request with no body is recognized")
	}
}

// At the device grant's consent page, the waiting authorization a user code names decides: ur login's passes, as the
// person types its code; an all command's, an unknown code and a process with no store pass nothing.
func TestUrcliOwnKeyDeviceRequestReadsItsAuthorization(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		installUrcliTest(t)
		defer SetDeviceAuthorizationStore(nil)
		ctx := context.Background()
		SetDeviceAuthorizationStore(newTestDeviceStore())
		now := server.NowUtc()
		login := startTestDeviceLogin(t, ctx, testUrcliCommand(), now)
		other := startTestDeviceLogin(t, ctx, &sidecar.AdminCommand{
			Method: "POST",
			Path:   "/network/member-invite",
			Body:   []byte(`{"email":"invitee@example.com","role":"admin","expire_days":7}`),
		}, now)
		deviceRequest := func(path string, userCode string) *http.Request {
			body, err := json.Marshal(&DeviceConsentRequest{UserCode: userCode})
			if err != nil {
				t.Fatal(err)
			}
			return httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		}
		for _, path := range []string{"/oauth/device_consent", "/oauth/device_approve"} {
			r := deviceRequest(path, strings.ToLower(login.response.UserCode))
			if !IsUrcliOwnKeyDeviceRequest(r) {
				t.Fatalf("%s: ur login's user code is not recognized", path)
			}
			if left, err := io.ReadAll(r.Body); err != nil || !strings.Contains(string(left), "user_code") {
				t.Fatalf("%s: the handler reads %q (%v)", path, left, err)
			}
			if IsUrcliOwnKeyDeviceRequest(deviceRequest(path, other.response.UserCode)) {
				t.Fatalf("%s: an all command's user code is recognized", path)
			}
			if IsUrcliOwnKeyDeviceRequest(deviceRequest(path, "BCDF-GHJK")) {
				t.Fatalf("%s: an unknown user code is recognized", path)
			}
		}
		SetDeviceAuthorizationStore(nil)
		if IsUrcliOwnKeyDeviceRequest(deviceRequest("/oauth/device_consent", login.response.UserCode)) {
			t.Fatal("a process with no store recognizes a user code")
		}
	})
}
