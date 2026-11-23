//go:build sidecar_pending

package alt

// The routes api's installs serve, asked through alt's api front and through api's router over one install of what
// api's route set runs on (controller.InstallApiRoutes), the call alt's main and api's main each make
// (alt_installs_test.go reads both), against the database and Redis with MG1 to MG9. The install runs over the host
// alt's main builds, on a fake first metadata cluster in place of vault's file-clusters-api.yml, so its projection
// installs api's gate cache over a store that holds no network's log: a network's verified state never loads, and
// each route that reads it answers unavailable, the closed answer, after asking the cache for the network's load.
//
//	before   with nothing installed, as alt's process ran: the authorization log's read answers 503 and the device
//	         grant temporarily_unavailable, alike through both fronts, and there is no gate cache at all
//	after    every route answers through alt's front exactly as through api's router: the log's read serves the empty
//	         network's projection, the device grant serves an authorization and the issuer's metadata names it; and
//	         the approval cancel, the smtp reports read, the DKIM publish's mail_send, the exchange's scopes and the mail
//	         enable's scope labels each reach api's gate cache from alt's front: the cache, which held nothing of the
//	         request's network before alt's request, tracks it after
//	undone   the install's undo takes it all out again: the log's read answers 503 once more and no cache is left
//
// Each request comes from a peer of its own, so alt's per-address limits never decide an answer. The answers are
// compared whole, status and body, but for the device grant's, whose codes are fresh each time.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/sidecarhost"
)

// One front's answer to one request.
type altInstallsAnswer struct {
	code int
	body string
}

// The two fronts over one process's installs: api's route table as api serves it, and alt's api front over
// api.NewRouter, as alt.Run mounts it.
type altInstallsFronts struct {
	t   testing.TB
	api http.Handler
	alt http.Handler
	// the last peer a request came from
	peer int
}

// Serves the request newRequest builds through front, from a peer of its own.
func (self *altInstallsFronts) serve(front http.Handler, newRequest func() *http.Request) altInstallsAnswer {
	self.peer += 1
	r := newRequest()
	r.RemoteAddr = fmt.Sprintf("[2001:db8:a17::%x]:53200", self.peer)
	w := httptest.NewRecorder()
	front.ServeHTTP(w, r)
	return altInstallsAnswer{code: w.Code, body: w.Body.String()}
}

// The request through alt's front, then through api's router: alt's answer, then api's.
func (self *altInstallsFronts) both(newRequest func() *http.Request) (altInstallsAnswer, altInstallsAnswer) {
	return self.serve(self.alt, newRequest), self.serve(self.api, newRequest)
}

// The request through both, which must answer alike.
func (self *altInstallsFronts) alike(name string, newRequest func() *http.Request) altInstallsAnswer {
	self.t.Helper()
	altAnswer, apiAnswer := self.both(newRequest)
	if altAnswer != apiAnswer {
		self.t.Errorf("%s: alt answered %d %q, api %d %q", name, altAnswer.code, altAnswer.body, apiAnswer.code, apiAnswer.body)
	}
	return altAnswer
}

// A JSON request, body nil for none.
func altInstallsJsonRequest(method string, path string, authorization string, body any) func() *http.Request {
	return func() *http.Request {
		bodyText := ""
		if body != nil {
			bodyBytes, err := json.Marshal(body)
			server.Raise(err)
			bodyText = string(bodyBytes)
		}
		r := httptest.NewRequest(method, path, strings.NewReader(bodyText))
		if authorization != "" {
			r.Header.Set("Authorization", authorization)
		}
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		return r
	}
}

// A form posted to path, edit run on each request built.
func altInstallsFormRequest(path string, form url.Values, edit func(r *http.Request)) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if edit != nil {
			edit(r)
		}
		return r
	}
}

// A personal network of its own and its admin's network token.
type altInstallsNetwork struct {
	networkId   server.Id
	userId      server.Id
	networkName string
}

func newAltInstallsNetwork(ctx context.Context) *altInstallsNetwork {
	networkId, userId := server.NewId(), server.NewId()
	networkName := fmt.Sprintf("altinstalls%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	return &altInstallsNetwork{networkId: networkId, userId: userId, networkName: networkName}
}

func (self *altInstallsNetwork) byJwt() *session.ByJwt {
	return session.NewByJwt(self.networkId, self.userId, self.networkName, false, false)
}

// A client of a device of the network's own, as an app holds one.
func (self *altInstallsNetwork) clientBearer(ctx context.Context) string {
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(ctx, self.networkId, deviceId, clientId, "alt installs", "alt installs")
	return "Bearer " + self.byJwt().Client(deviceId, clientId).Testing_Sign()
}

// A sidecar of the network: its strict_exp client, minted with the network token and bound to the sidecar's
// identity, as a daemon holds one.
type altInstallsSidecar struct {
	token     string
	clientKey ed25519.PrivateKey
	// the proofs made so far
	jtiCount int
}

func (self *altInstallsFronts) newSidecar(network *altInstallsNetwork) *altInstallsSidecar {
	self.t.Helper()
	minted := self.serve(self.api, altInstallsJsonRequest(http.MethodPost, "/network/auth-client", "Bearer "+network.byJwt().Testing_Sign(), map[string]any{
		"description":   "sidecar",
		"device_spec":   "sidecar",
		"admin_refused": true,
		"strict_exp":    true,
	}))
	result := &model.AuthNetworkClientResult{}
	if minted.code != http.StatusOK || json.Unmarshal([]byte(minted.body), result) != nil || result.ClientId == nil || result.ByClientJwt == nil || !result.StrictExp {
		self.t.Fatalf("the sidecar's client: %d %s", minted.code, minted.body)
	}
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	_, identityKey, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	binding, err := sidecar.NewSidecarIdentityBinding(&sidecar.SidecarIdentityBinding{
		NetworkId:  sidecar.Id(network.networkId),
		SidecarId:  sidecar.Id(server.NewId()),
		ClientId:   sidecar.Id(*result.ClientId),
		Generation: 1,
		IssuedAt:   server.NowUtc(),
	}, identityKey.Seed(), clientKey.Seed())
	server.Raise(err)
	bound := self.serve(self.api, altInstallsJsonRequest(http.MethodPost, "/sidecar/bind", "Bearer "+*result.ByClientJwt, map[string]any{"binding": binding}))
	if bound.code != http.StatusOK {
		self.t.Fatalf("the sidecar's bind: %d %s", bound.code, bound.body)
	}
	return &altInstallsSidecar{token: *result.ByClientJwt, clientKey: clientKey}
}

func (self *altInstallsSidecar) authorization() string {
	return "Bearer " + self.token
}

// A fresh proof of a POST to the issuer's token endpoint by the client's key over its token, as its exchange sends.
func (self *altInstallsSidecar) exchangeProof(r *http.Request) {
	self.jtiCount += 1
	jti := fmt.Sprintf("alt-installs-%d", self.jtiCount)
	r.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(self.clientKey, http.MethodPost, oauth.TokenEndpoint(), self.token, jti, server.NowUtc()))
}

// What api's gate cache says of the network now, unknown with no cache at all.
func altInstallsCacheStatus(networkId server.Id) sidecar.AclGateStatus {
	cache := controller.ApiAuthorizationCache()
	if cache == nil {
		return sidecar.AclGateUnknown
	}
	_, status := cache.State(sidecar.Id(networkId))
	return status
}

// ur-cli's device authorization request, as cmd_login_device.go sends it, for a fresh key.
func altInstallsDeviceAuthorizationForm(t testing.TB) url.Values {
	t.Helper()
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	server.Raise(err)
	consentCode, err := sidecar.NewAdminConsentCode(rand.Reader)
	server.Raise(err)
	details, err := sidecar.FormatAdminCommandDetails(&sidecar.AdminCommand{
		Method: "POST",
		Path:   "/network/auth-keys",
		Body:   []byte(`{"duration_seconds":300,"max_uses":1}`),
	}, consentCode)
	server.Raise(err)
	form := url.Values{}
	form.Set("client_id", oauth.UrcliClientId)
	form.Set("scope", oauth.UrcliScopeAuthKeys)
	form.Set("resource", "https://api."+server.RequireDomain())
	form.Set("dpop_jkt", session.DpopKeyThumbprint(publicKey))
	form.Set("authorization_details", details)
	return form
}

// One selector to publish at domain, its key RSA-2048, as a sidecar publishes it.
func altInstallsDkimPublish(t testing.TB, domain string) *sidecar.SmtpDkimKeysArgs {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	server.Raise(err)
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	server.Raise(err)
	random := make([]byte, 16)
	_, err = rand.Read(random)
	server.Raise(err)
	selector := base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding).EncodeToString(random)
	return &sidecar.SmtpDkimKeysArgs{Publish: []*sidecar.SmtpDkimKeyPublish{{Selector: selector, Domain: domain, PublicKey: der}}}
}

func TestAltAnswersTheRoutesApisInstallsServeAsApiDoes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer oauth.Testing_SetIssuer("https://auth.altinstalls.example")()

		// one network a route, so the network a request names is one the cache held nothing of before it
		projectionNetwork := newAltInstallsNetwork(ctx)
		approvalNetwork := newAltInstallsNetwork(ctx)
		reportsNetwork := newAltInstallsNetwork(ctx)
		publishNetwork := newAltInstallsNetwork(ctx)
		exchangeNetwork := newAltInstallsNetwork(ctx)
		mailNetwork := newAltInstallsNetwork(ctx)
		defer server.Testing_PushSidecarConfig(fmt.Sprintf(
			"sidecar:\n  preview_networks: [%q, %q, %q]\n  smtpedge: {enabled: true, networks: [%q]}\n  mxedge: {enabled: true, networks: [%q]}\n",
			reportsNetwork.networkId,
			publishNetwork.networkId,
			exchangeNetwork.networkId,
			publishNetwork.networkId,
			mailNetwork.networkId,
		))()
		root := fmt.Sprintf("a%s.example.", strings.ReplaceAll(publishNetwork.networkId.String(), "-", "")[20:])
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			tb.Fatal(err)
		}
		defer model.Testing_SetDnsUserRoot(root)()

		apiRouter, closeApiRouter, err := api.NewRouter(ctx, ctx)
		if err != nil {
			tb.Fatal(err)
		}
		defer closeApiRouter()
		fronts := &altInstallsFronts{
			t:   tb,
			api: router.NewRouter(ctx, api.Routes()),
			alt: newApiFrontHandler(apiRouter, NewLimits(ctx, DefaultLimitsSettings())),
		}

		projectionRead := altInstallsJsonRequest(http.MethodGet, "/network/config/projection", projectionNetwork.clientBearer(ctx), nil)
		deviceAuthorization := altInstallsFormRequest("/oauth/device_authorization", altInstallsDeviceAuthorizationForm(tb), nil)

		// before: what alt's process answered with nothing installed
		if answer := fronts.alike("the log's read with nothing installed", projectionRead); answer.code != http.StatusServiceUnavailable {
			tb.Errorf("the log's read with nothing installed: %d %s", answer.code, answer.body)
		}
		if answer := fronts.alike("the device grant with nothing installed", deviceAuthorization); answer.code == http.StatusOK || !strings.Contains(answer.body, "temporarily_unavailable") {
			tb.Errorf("the device grant with nothing installed: %d %s", answer.code, answer.body)
		}
		if controller.ApiAuthorizationCache() != nil {
			tb.Fatal("a gate cache before the install")
		}

		// the install both mains make, over the host alt's main builds, on a fake first metadata cluster
		host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true})
		blobs := sidecartest.NewFakeBlobs(nil, nil, nil, nil)
		undoInstalls := controller.InstallApiRoutes(ctx, host, &controller.ApiRoutesInstallDeps{
			Clusters: func(ctx context.Context) (sidecar.BlobStore, func(), error) {
				return blobs, func() {}, nil
			},
		})
		undone := false
		defer func() {
			if !undone {
				undoInstalls()
			}
		}()
		if controller.ApiAuthorizationCache() == nil {
			tb.Fatal("the install published no gate cache")
		}

		// the authorization log's sequencer: the empty network's projection, alike
		if answer := fronts.alike("the log's read", projectionRead); answer.code != http.StatusOK {
			tb.Errorf("the log's read: %d %s", answer.code, answer.body)
		}

		// the device grant: an authorization through each, and the metadata naming it
		altAnswer, apiAnswer := fronts.both(deviceAuthorization)
		for name, answer := range map[string]altInstallsAnswer{"alt": altAnswer, "api": apiAnswer} {
			authorization := &oauth.DeviceAuthorizationResponse{}
			if answer.code != http.StatusOK || json.Unmarshal([]byte(answer.body), authorization) != nil ||
				authorization.DeviceCode == "" || len(authorization.UserCode) != 9 || authorization.VerificationUri != oauth.DeviceVerificationUri() {
				tb.Errorf("the device grant through %s: %d", name, answer.code)
			}
		}
		metadata := &oauth.AuthorizationServerMetadata{}
		answer := fronts.alike("the issuer's metadata", altInstallsJsonRequest(http.MethodGet, "/.well-known/oauth-authorization-server", "", nil))
		if answer.code != http.StatusOK || json.Unmarshal([]byte(answer.body), metadata) != nil ||
			metadata.DeviceAuthorizationEndpoint != oauth.DeviceAuthorizationEndpoint() || !slices.Contains(metadata.GrantTypesSupported, oauth.DeviceCodeGrantType) {
			tb.Errorf("the issuer's metadata: %d %s", answer.code, answer.body)
		}

		// a route over the gate cache, through alt's front first: the cache holds nothing of its network before alt's
		// request and tracks it after, and api's router answers the same
		throughTheCache := func(name string, network *altInstallsNetwork, newRequest func() *http.Request) altInstallsAnswer {
			tb.Helper()
			if status := altInstallsCacheStatus(network.networkId); status != sidecar.AclGateUnknown {
				tb.Fatalf("%s: the cache held the network before the request (%d)", name, status)
			}
			altAnswer := fronts.serve(fronts.alt, newRequest)
			if status := altInstallsCacheStatus(network.networkId); status != sidecar.AclGateNotLoaded {
				tb.Errorf("%s through alt's front did not ask the cache for its network: %d %s, cache %d", name, altAnswer.code, altAnswer.body, status)
			}
			apiAnswer := fronts.serve(fronts.api, newRequest)
			if altAnswer != apiAnswer {
				tb.Errorf("%s: alt answered %d %q, api %d %q", name, altAnswer.code, altAnswer.body, apiAnswer.code, apiAnswer.body)
			}
			return altAnswer
		}

		// the approval gate's states: a cancel of a network the cache has not loaded is unavailable
		cancelBody := map[string]any{"cancel": []byte{1}, "decision": []byte{1}}
		if answer := throughTheCache("the approval cancel", approvalNetwork, altInstallsJsonRequest(http.MethodPost, "/approval/cancel", approvalNetwork.clientBearer(ctx), cancelBody)); answer.code != http.StatusServiceUnavailable {
			tb.Errorf("the approval cancel: %d %s", answer.code, answer.body)
		}

		// the smtp routes' §8 checks: the reports' read, then a publish's mail_send
		reader := fronts.newSidecar(reportsNetwork)
		if answer := throughTheCache("the reports' read", reportsNetwork, altInstallsJsonRequest(http.MethodGet, "/smtp/reports?since=0", reader.authorization(), nil)); answer.code != http.StatusServiceUnavailable {
			tb.Errorf("the reports' read: %d %s", answer.code, answer.body)
		}
		var label string
		server.Tx(ctx, func(tx server.PgTx) {
			label, err = model.EnsureNetworkLabelInTx(ctx, tx, publishNetwork.networkId, root)
			server.Raise(err)
		})
		sender := fronts.newSidecar(publishNetwork)
		publish := altInstallsDkimPublish(tb, label+"."+strings.TrimSuffix(root, "."))
		if answer := throughTheCache("the DKIM publish", publishNetwork, altInstallsJsonRequest(http.MethodPost, "/smtp/dkim-keys", sender.authorization(), publish)); answer.code != http.StatusServiceUnavailable {
			tb.Errorf("the DKIM publish: %d %s", answer.code, answer.body)
		}
		if key, _ := model.FindSmtpDkimKey(ctx, publish.Publish[0].Selector, publish.Publish[0].Domain); key != nil {
			tb.Error("a publish answered unavailable wrote its selector")
		}

		// the exchange's scopes: a service token for evs, its proof fresh for each request
		exchanger := fronts.newSidecar(exchangeNetwork)
		exchangeForm := url.Values{
			"grant_type":         {oauth.ExchangeGrantType},
			"subject_token":      {exchanger.token},
			"subject_token_type": {oauth.ExchangeSubjectTokenType},
			"resource":           {"https://evs." + server.RequireDomain()},
		}
		if answer := throughTheCache("the exchange", exchangeNetwork, altInstallsFormRequest("/oauth/token", exchangeForm, exchanger.exchangeProof)); answer.code != http.StatusServiceUnavailable || !strings.Contains(answer.body, "temporarily_unavailable") {
			tb.Errorf("the exchange: %d %s", answer.code, answer.body)
		}

		// the dns scopes' labels, which turning mail on reads to project the network's routes; mail goes on, alike
		mailEnable := altInstallsJsonRequest(http.MethodPost, "/mail/enable", mailNetwork.clientBearer(ctx), nil)
		if answer := throughTheCache("the mail enable", mailNetwork, mailEnable); answer.code != http.StatusOK {
			tb.Errorf("the mail enable: %d %s", answer.code, answer.body)
		}

		// undone: the log's read is unavailable again, and no cache is left
		undoInstalls()
		undone = true
		if answer := fronts.alike("the log's read once undone", projectionRead); answer.code != http.StatusServiceUnavailable {
			tb.Errorf("the log's read once undone: %d %s", answer.code, answer.body)
		}
		if controller.ApiAuthorizationCache() != nil {
			tb.Error("the undone install left its gate cache")
		}
	})
}
