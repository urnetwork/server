//go:build sidecar_pending

package main

// The routes api's installs serve, asked through api's route table, which all's process serves as api's does (all
// runs api.Run), first over all's installs, the parts all's main installs (operatorInstallParts), then over api's,
// the install api's main makes (controller.InstallApiRoutes), against the database and Redis with MG1 to MG9. Each
// runs over the host its own main builds, on a fake first metadata cluster in place of vault's file-clusters-api.yml,
// so its projection installs api's gate cache over a store that holds no network's log: a network's verified state
// never loads, and each route that reads it answers unavailable, the closed answer, after asking the cache for the
// network's load. sidecar.yml's acl_contract_gate is on, which only CreateContract and the residents' peer lists read.
//
//	before   with nothing installed, as all's process ran: the authorization log's read answers 503, the device
//	         grant temporarily_unavailable, and there is no gate cache and no contract gate for connect's residents
//	all      the log's read serves the empty network's projection, the device grant serves an authorization and the
//	         issuer's metadata names it; the approval cancel, the smtp reports read, the DKIM publish's mail_send, the
//	         exchange's scopes and the mail enable's scope labels each reach api's gate cache, which held nothing of
//	         the request's network before the request and tracks it after; and the contract gate is installed
//	undone   the one undo takes it all out again: the log's read answers 503 once more, and no cache or gate is left
//	api      api's install answers each request as all's did
//
// Each request comes from a peer of its own. The answers are compared whole, status and body, but for the device
// grant's, whose codes are fresh each time.

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

// One answer to one request.
type allInstallsAnswer struct {
	code int
	body string
}

// api's route table, as all's process and api's serve it.
type allInstallsRoutes struct {
	t       testing.TB
	handler http.Handler
	// the last peer a request came from
	peer int
}

// Serves the request newRequest builds, from a peer of its own.
func (self *allInstallsRoutes) serve(newRequest func() *http.Request) allInstallsAnswer {
	self.peer += 1
	r := newRequest()
	r.RemoteAddr = fmt.Sprintf("[2001:db8:a11::%x]:53200", self.peer)
	w := httptest.NewRecorder()
	self.handler.ServeHTTP(w, r)
	return allInstallsAnswer{code: w.Code, body: w.Body.String()}
}

// A JSON request, body nil for none.
func allInstallsJsonRequest(method string, path string, authorization string, body any) func() *http.Request {
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
func allInstallsFormRequest(path string, form url.Values, edit func(r *http.Request)) func() *http.Request {
	return func() *http.Request {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if edit != nil {
			edit(r)
		}
		return r
	}
}

// A personal network of its own and its admin.
type allInstallsNetwork struct {
	networkId   server.Id
	userId      server.Id
	networkName string
}

func newAllInstallsNetwork(ctx context.Context) *allInstallsNetwork {
	networkId, userId := server.NewId(), server.NewId()
	networkName := fmt.Sprintf("allinstalls%s", strings.ReplaceAll(networkId.String(), "-", "")[:12])
	model.Testing_CreateNetwork(ctx, networkId, networkName, userId)
	return &allInstallsNetwork{networkId: networkId, userId: userId, networkName: networkName}
}

func (self *allInstallsNetwork) byJwt() *session.ByJwt {
	return session.NewByJwt(self.networkId, self.userId, self.networkName, false, false)
}

// A client of a device of the network's own, as an app holds one.
func (self *allInstallsNetwork) clientBearer(ctx context.Context) string {
	deviceId, clientId := server.NewId(), server.NewId()
	model.Testing_CreateDevice(ctx, self.networkId, deviceId, clientId, "all installs", "all installs")
	return "Bearer " + self.byJwt().Client(deviceId, clientId).Testing_Sign()
}

// A sidecar of the network: its strict_exp client, minted with the network token and bound to the sidecar's
// identity, as a daemon holds one.
type allInstallsSidecar struct {
	token     string
	clientKey ed25519.PrivateKey
	// the proofs made so far
	jtiCount int
}

func (self *allInstallsRoutes) newSidecar(network *allInstallsNetwork) *allInstallsSidecar {
	self.t.Helper()
	minted := self.serve(allInstallsJsonRequest(http.MethodPost, "/network/auth-client", "Bearer "+network.byJwt().Testing_Sign(), map[string]any{
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
	bound := self.serve(allInstallsJsonRequest(http.MethodPost, "/sidecar/bind", "Bearer "+*result.ByClientJwt, map[string]any{"binding": binding}))
	if bound.code != http.StatusOK {
		self.t.Fatalf("the sidecar's bind: %d %s", bound.code, bound.body)
	}
	return &allInstallsSidecar{token: *result.ByClientJwt, clientKey: clientKey}
}

func (self *allInstallsSidecar) authorization() string {
	return "Bearer " + self.token
}

// A fresh proof of a POST to the issuer's token endpoint by the client's key over its token, as its exchange sends.
func (self *allInstallsSidecar) exchangeProof(r *http.Request) {
	self.jtiCount += 1
	jti := fmt.Sprintf("all-installs-%s-%d", self.token[len(self.token)-8:], self.jtiCount)
	r.Header.Set(session.DpopHeader, session.Testing_SignDpopProof(self.clientKey, http.MethodPost, oauth.TokenEndpoint(), self.token, jti, server.NowUtc()))
}

// What api's gate cache says of the network now, unknown with no cache at all.
func allInstallsCacheStatus(networkId server.Id) sidecar.AclGateStatus {
	cache := controller.ApiAuthorizationCache()
	if cache == nil {
		return sidecar.AclGateUnknown
	}
	_, status := cache.State(sidecar.Id(networkId))
	return status
}

// ur-cli's device authorization request, as cmd_login_device.go sends it, for a fresh key.
func allInstallsDeviceAuthorizationForm(t testing.TB) url.Values {
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
func allInstallsDkimPublish(t testing.TB, domain string) *sidecar.SmtpDkimKeysArgs {
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

func TestAllAnswersTheRoutesApisInstallsServeAsApiDoes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer oauth.Testing_SetIssuer("https://auth.allinstalls.example")()

		// one network a route, so the network a request names is one each install's cache held nothing of before it
		projectionNetwork := newAllInstallsNetwork(ctx)
		approvalNetwork := newAllInstallsNetwork(ctx)
		reportsNetwork := newAllInstallsNetwork(ctx)
		publishNetwork := newAllInstallsNetwork(ctx)
		exchangeNetwork := newAllInstallsNetwork(ctx)
		mailNetwork := newAllInstallsNetwork(ctx)
		defer server.Testing_PushSidecarConfig(fmt.Sprintf(
			"sidecar:\n  preview_networks: [%q, %q, %q]\n  smtpedge: {enabled: true, networks: [%q]}\n  mxedge: {enabled: true, networks: [%q]}\n  acl_contract_gate: {enabled: true}\n",
			reportsNetwork.networkId,
			publishNetwork.networkId,
			exchangeNetwork.networkId,
			publishNetwork.networkId,
			mailNetwork.networkId,
		))()
		root := fmt.Sprintf("b%s.example.", strings.ReplaceAll(publishNetwork.networkId.String(), "-", "")[20:])
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			tb.Fatal(err)
		}
		defer model.Testing_SetDnsUserRoot(root)()
		var label string
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			label, err = model.EnsureNetworkLabelInTx(ctx, tx, publishNetwork.networkId, root)
			server.Raise(err)
		})

		routes := &allInstallsRoutes{t: tb, handler: router.NewRouter(ctx, api.Routes())}
		projectionRead := allInstallsJsonRequest(http.MethodGet, "/network/config/projection", projectionNetwork.clientBearer(ctx), nil)
		deviceAuthorization := allInstallsFormRequest("/oauth/device_authorization", allInstallsDeviceAuthorizationForm(tb), nil)

		// before: what all's process answered with nothing installed
		if answer := routes.serve(projectionRead); answer.code != http.StatusServiceUnavailable {
			tb.Errorf("the log's read with nothing installed: %d %s", answer.code, answer.body)
		}
		if answer := routes.serve(deviceAuthorization); answer.code == http.StatusOK || !strings.Contains(answer.body, "temporarily_unavailable") {
			tb.Errorf("the device grant with nothing installed: %d %s", answer.code, answer.body)
		}
		if controller.ApiAuthorizationCache() != nil || controller.AclContractGateInstalled() {
			tb.Fatal("a gate cache or a contract gate before any install")
		}

		// a fake first metadata cluster in place of vault's file-clusters-api.yml, for each install
		blobs := sidecartest.NewFakeBlobs(nil, nil, nil, nil)
		clusters := &controller.ApiRoutesInstallDeps{
			Clusters: func(ctx context.Context) (sidecar.BlobStore, func(), error) {
				return blobs, func() {}, nil
			},
		}

		// the routes over one process's installs, which install makes and its undo takes out: each answer by route
		askRoutes := func(name string, install func() func()) map[string]allInstallsAnswer {
			answers := map[string]allInstallsAnswer{}
			undo := install()
			undone := false
			defer func() {
				if !undone {
					undo()
				}
			}()
			if controller.ApiAuthorizationCache() == nil {
				tb.Fatalf("%s's installs published no gate cache", name)
			}
			// connect's residents read it at each CreateContract and peer list
			if !controller.AclContractGateInstalled() {
				tb.Errorf("%s's installs installed no contract gate", name)
			}

			// the authorization log's sequencer: the empty network's projection
			answers["the log's read"] = routes.serve(projectionRead)

			// the device grant: an authorization, its codes fresh, and the metadata naming it
			answer := routes.serve(deviceAuthorization)
			authorization := &oauth.DeviceAuthorizationResponse{}
			if answer.code == http.StatusOK && json.Unmarshal([]byte(answer.body), authorization) == nil &&
				authorization.DeviceCode != "" && len(authorization.UserCode) == 9 && authorization.VerificationUri == oauth.DeviceVerificationUri() {
				answer.body = "an authorization"
			}
			answers["the device grant"] = answer
			answers["the issuer's metadata"] = routes.serve(allInstallsJsonRequest(http.MethodGet, "/.well-known/oauth-authorization-server", "", nil))

			// a route over the gate cache: the cache holds nothing of its network before the request and tracks it after
			throughTheCache := func(route string, network *allInstallsNetwork, newRequest func() *http.Request) {
				tb.Helper()
				if status := allInstallsCacheStatus(network.networkId); status != sidecar.AclGateUnknown {
					tb.Fatalf("%s, %s: the cache held the network before the request (%d)", name, route, status)
				}
				answers[route] = routes.serve(newRequest)
				if status := allInstallsCacheStatus(network.networkId); status != sidecar.AclGateNotLoaded {
					tb.Errorf("%s, %s did not ask the cache for its network: %d %s, cache %d", name, route, answers[route].code, answers[route].body, status)
				}
			}

			// the approval gate's states
			throughTheCache("the approval cancel", approvalNetwork, allInstallsJsonRequest(http.MethodPost, "/approval/cancel", approvalNetwork.clientBearer(ctx), map[string]any{"cancel": []byte{1}, "decision": []byte{1}}))

			// the smtp routes' §8 checks: the reports' read, then a publish's mail_send
			reader := routes.newSidecar(reportsNetwork)
			throughTheCache("the reports' read", reportsNetwork, allInstallsJsonRequest(http.MethodGet, "/smtp/reports?since=0", reader.authorization(), nil))
			sender := routes.newSidecar(publishNetwork)
			publish := allInstallsDkimPublish(tb, label+"."+strings.TrimSuffix(root, "."))
			throughTheCache("the DKIM publish", publishNetwork, allInstallsJsonRequest(http.MethodPost, "/smtp/dkim-keys", sender.authorization(), publish))
			if key, _ := model.FindSmtpDkimKey(ctx, publish.Publish[0].Selector, publish.Publish[0].Domain); key != nil {
				tb.Errorf("%s: a publish answered unavailable wrote its selector", name)
			}

			// the exchange's scopes: a service token for evs
			exchanger := routes.newSidecar(exchangeNetwork)
			exchangeForm := url.Values{
				"grant_type":         {oauth.ExchangeGrantType},
				"subject_token":      {exchanger.token},
				"subject_token_type": {oauth.ExchangeSubjectTokenType},
				"resource":           {"https://evs." + server.RequireDomain()},
			}
			throughTheCache("the exchange", exchangeNetwork, allInstallsFormRequest("/oauth/token", exchangeForm, exchanger.exchangeProof))

			// the dns scopes' labels, which turning mail on reads to project the network's routes
			throughTheCache("the mail enable", mailNetwork, allInstallsJsonRequest(http.MethodPost, "/mail/enable", mailNetwork.clientBearer(ctx), nil))

			// undone: the log's read is unavailable again, and no cache or gate is left
			undo()
			undone = true
			if answer := routes.serve(projectionRead); answer.code != http.StatusServiceUnavailable {
				tb.Errorf("%s: the log's read once undone: %d %s", name, answer.code, answer.body)
			}
			if controller.ApiAuthorizationCache() != nil || controller.AclContractGateInstalled() {
				tb.Errorf("%s: the undone installs left a gate cache or a contract gate", name)
			}
			return answers
		}

		// all's installs, as all's main makes them, over the host it builds
		allAnswers := askRoutes("all", func() func() {
			return controller.InstallProcessParts(ctx, operatorInstallParts(newProcessHost(ctx), commandOptions{}, clusters))
		})
		// api's, as api's main makes them, over the host it builds
		apiAnswers := askRoutes("api", func() func() {
			return controller.InstallApiRoutes(ctx, sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Postgres: true}), clusters)
		})

		// each route answers through all's installs as through api's, and as the installs have it answer
		for route, want := range map[string]int{
			"the log's read":        http.StatusOK,
			"the device grant":      http.StatusOK,
			"the issuer's metadata": http.StatusOK,
			"the approval cancel":   http.StatusServiceUnavailable,
			"the reports' read":     http.StatusServiceUnavailable,
			"the DKIM publish":      http.StatusServiceUnavailable,
			"the exchange":          http.StatusServiceUnavailable,
			"the mail enable":       http.StatusOK,
		} {
			allAnswer, apiAnswer := allAnswers[route], apiAnswers[route]
			if allAnswer != apiAnswer {
				tb.Errorf("%s: all answered %d %q, api %d %q", route, allAnswer.code, allAnswer.body, apiAnswer.code, apiAnswer.body)
			}
			if allAnswer.code != want {
				tb.Errorf("%s over all's installs: %d %s", route, allAnswer.code, allAnswer.body)
			}
		}
		if answer := allAnswers["the device grant"]; answer.body != "an authorization" {
			tb.Errorf("the device grant over all's installs: %d %s", answer.code, answer.body)
		}
		metadata := &oauth.AuthorizationServerMetadata{}
		if answer := allAnswers["the issuer's metadata"]; json.Unmarshal([]byte(answer.body), metadata) != nil ||
			metadata.DeviceAuthorizationEndpoint != oauth.DeviceAuthorizationEndpoint() || !slices.Contains(metadata.GrantTypesSupported, oauth.DeviceCodeGrantType) {
			tb.Errorf("the issuer's metadata over all's installs: %s", answer.body)
		}
		if answer := allAnswers["the exchange"]; !strings.Contains(answer.body, "temporarily_unavailable") {
			tb.Errorf("the exchange over all's installs: %s", answer.body)
		}
	})
}
