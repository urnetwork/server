package main

// api's composition (main.go): with the device authorization store api installs before it serves (QD-830), api's own
// route set answers ur-cli's POST /oauth/device_authorization, the authorization waiting in Redis under its device
// code's hash and its user code, and the issuer's metadata names the device endpoint and the device_code grant, which
// ur-cli reads before it asks. Run on the private test stack only, through .stack/server-test.sh.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
)

func TestApiServesTheDeviceGrant(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		t.Setenv("WARP_DOMAIN", "urcli.example")
		t.Cleanup(oauth.Testing_SetIssuer("https://auth.urcli.example"))
		// what main installs before api.Run
		oauth.InstallDeviceAuthorizationStore()
		t.Cleanup(func() { oauth.SetDeviceAuthorizationStore(nil) })
		handler := router.NewRouter(ctx, api.Routes())
		serve := func(request *http.Request) *httptest.ResponseRecorder {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			return recorder
		}

		for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/openid-configuration"} {
			answered := serve(httptest.NewRequest(http.MethodGet, path, nil))
			if answered.Code != http.StatusOK {
				t.Fatalf("%s answered %d", path, answered.Code)
			}
			metadata := &oauth.AuthorizationServerMetadata{}
			if err := json.Unmarshal(answered.Body.Bytes(), metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.DeviceAuthorizationEndpoint != "https://auth.urcli.example/oauth/device_authorization" {
				t.Fatalf("%s names the device endpoint %q", path, metadata.DeviceAuthorizationEndpoint)
			}
			if !slices.Contains(metadata.GrantTypesSupported, oauth.DeviceCodeGrantType) {
				t.Fatalf("%s names the grants %v", path, metadata.GrantTypesSupported)
			}
		}

		// ur-cli's device authorization request for v0's command, as cmd_login_device.go sends it
		publicKey, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		consentCode, err := sidecar.NewAdminConsentCode(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		details, err := sidecar.FormatAdminCommandDetails(&sidecar.AdminCommand{
			Method: "POST",
			Path:   "/network/auth-keys",
			Body:   []byte(`{"duration_seconds":300,"max_uses":1}`),
		}, consentCode)
		if err != nil {
			t.Fatal(err)
		}
		form := url.Values{}
		form.Set("client_id", oauth.UrcliClientId)
		form.Set("scope", oauth.UrcliScopeAuthKeys)
		form.Set("resource", "https://api.urcli.example")
		form.Set("dpop_jkt", session.DpopKeyThumbprint(publicKey))
		form.Set("authorization_details", details)
		request := httptest.NewRequest(http.MethodPost, "/oauth/device_authorization", strings.NewReader(form.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		answered := serve(request)
		if answered.Code != http.StatusOK {
			// the answer's error code alone: a body that served would hold the device code
			refusal := struct {
				Error string `json:"error"`
			}{}
			json.Unmarshal(answered.Body.Bytes(), &refusal)
			t.Fatalf("the device endpoint answered %d %s", answered.Code, refusal.Error)
		}
		authorization := &oauth.DeviceAuthorizationResponse{}
		if err := json.Unmarshal(answered.Body.Bytes(), authorization); err != nil {
			t.Fatal(err)
		}
		if authorization.DeviceCode == "" || len(authorization.UserCode) != 9 || authorization.VerificationUri != oauth.DeviceVerificationUri() {
			t.Fatalf("the device endpoint answered a user code of %d characters at %q", len(authorization.UserCode), authorization.VerificationUri)
		}
		settings := oauth.DefaultDeviceGrantSettings()
		if authorization.ExpiresIn != int(settings.CodeDuration/time.Second) || authorization.Interval != int(settings.PollInterval/time.Second) {
			t.Fatalf("the device endpoint answered expires_in %d and interval %d", authorization.ExpiresIn, authorization.Interval)
		}

		// it waits in Redis, the user code pointing at the device code's hash, for the authorization's life
		deviceCodeSha256 := sha256.Sum256([]byte(authorization.DeviceCode))
		hash := hex.EncodeToString(deviceCodeSha256[:])
		userCode := strings.ReplaceAll(authorization.UserCode, "-", "")
		var ttl time.Duration
		var pointer string
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.PTTL(ctx, "{oauthdev_"+hash+"}").Result()
			server.Raise(err)
			pointer, err = r.Get(ctx, "{oauthuser_"+userCode+"}").Result()
			server.Raise(err)
		})
		if ttl <= 0 || settings.CodeDuration < ttl {
			t.Fatalf("the authorization's key has %s to live", ttl)
		}
		if pointer != hash {
			t.Fatal("the user code points elsewhere")
		}
	})
}
