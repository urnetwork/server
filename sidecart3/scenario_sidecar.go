package sidecart3

// T3's enrolled sidecars (§22.7's S1, F1; TEST-M2-11; I-6, I-7): a sidecar.Sidecar, NewSidecar's own, that enrolls
// through server's own api and connects on server's exchange, so a scenario whose clause a fake api proves nothing
// about runs on what production gives a device.
//
//   - T3Api is server's api in process: every request a sidecar or a scenario sends goes through server's router over
//     api's and oauth's route tables, as api's listener serves it, from a documentation address. Every credential an
//     answer carries (by_client_jwt, by_jwt, auth_key, close) and every bearer a request presents is handed to the Net's
//     capture as a secret, so the scan looks for each in every form, everywhere it reads.
//   - T3Api is the service origins' lb too: a request whose host is a service's origin (ServiceOrigin), for a service a
//     test serves through ServeService, goes to that service's handler, as warp routes the service's host to its
//     block, served in process from the same documentation address with its bearer handed to the capture; its answer
//     streams, so a long-lived one reaches its client as the handler writes it. A service nobody serves answers as api
//     does, as before: server's router, which knows none of its routes. The services a test may serve are
//     T3SidecarSettings.ServiceNames: the four a sidecar's settings name (evs, gkm, sockets, dns) and those whose
//     api-facing routes a test reaches beside them (tlsedge, smtpedge, mxedge, mcp).
//   - A T3Sidecar is a sidecar on a memory instance whose SidecarApi reaches T3Api (sidecar.SidecarApiPlatform) and
//     whose client generations run on server's exchange (exchange.go): its own client id and token, an h1 transport to
//     server's ConnectHandler, which checks the token, and server's controller for its control. Everything it logs, up to
//     verbosity 1, goes to the capture (CC4). Its host reports no credential store of this machine's, as a T2 sidecar's
//     does (D-TRUST-3). Its service clients send on SidecarApi's transport, so each reaches the service a test serves at
//     its origin.
//
// Time is real, as server's api and exchange keep it: a T3 sidecar's DPoP proofs and refreshes are judged on server's
// clock, so it runs on sidecar.RealClock, never the Net's fake one.
//
// Not here: a T3 Net's AddSidecar, whose Net helpers are sidecartest's and stand on its fake api, which a T3 test
// replaces with a T3World's networks (world_networks.go); and a sidecar's control over api's /connect/control route,
// which server's controller answers in process here, as exchange.go's clients' does.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/api"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/router"
)

// What T3's sidecars run with.
type T3SidecarSettings struct {
	// The origin a T3 sidecar names for api and auth: a .example host nothing dials, since T3Api serves every request in
	// process whatever its host.
	ApiOrigin string
	// The origin it names for each service, the service's name in place of %s: .example hosts nothing dials, since
	// T3Api serves each in process, through the handler a test serves it with (T3Api.ServeService).
	ServiceOriginFormat string
	// The services a test may serve at their origins: evs, gkm, sockets and dns, which a sidecar's settings name, and
	// tlsedge, smtpedge, mxedge and mcp, whose api-facing routes a test reaches through T3Api. A name outside it is
	// refused, so a misspelt service never answers as api.
	ServiceNames []string
	// The documentation address each request comes from (RFC 5737), which server hashes for its rate limits.
	RemoteAddr string
	// The verbosity a sidecar's log reaches the capture up to: connect's per-frame tracing stays out of memory.
	MaxLogLevel int32
	// How long, in real time, waiting on a sidecar's state may take before it fails: a broken run's bound, since
	// connect's timers and server's are real.
	StateTimeout time.Duration
	// How long, in real time, closing a sidecar may take.
	CloseTimeout time.Duration
	// The keys of an answer's JSON whose string values are credentials.
	SecretAnswerKeys []string
}

func DefaultT3SidecarSettings() *T3SidecarSettings {
	return &T3SidecarSettings{
		ApiOrigin:           "https://api.sidecart3.example",
		ServiceOriginFormat: "https://%s.sidecart3.example",
		ServiceNames:        []string{"evs", "gkm", "sockets", "dns", "tlsedge", "smtpedge", "mxedge", "mcp"},
		RemoteAddr:          "198.51.100.72:4000",
		MaxLogLevel:         1,
		StateTimeout:        60 * time.Second,
		CloseTimeout:        30 * time.Second,
		SecretAnswerKeys:    []string{"by_client_jwt", "by_jwt", "auth_key", "close"},
	}
}

// Server's api in process, for a scenario's T3 sidecars and its own calls. Safe for concurrent use.
type T3Api struct {
	settings *T3SidecarSettings
	handler  http.Handler
	capture  *sidecartest.PrivacyCapture

	stateLock sync.Mutex
	// each credential handed to the capture, so each is added once
	secrets map[string]bool
	// the newest token an enrollment or a mint answered for each client
	clientIdTokens map[server.Id]string
	// each served service origin's handler, by the origin's host (ServeService)
	serviceHostHandlers map[string]http.Handler
	// what serves api's internal gate surface in place of api's own routes there (ServeInternalGate), nil for api's
	internalGateHandler http.Handler
}

// Server's api over api's and oauth's routes, its credentials handed to net's capture. settings nil takes the defaults.
func NewT3Api(ctx context.Context, net *sidecartest.Net, settings *T3SidecarSettings) *T3Api {
	if settings == nil {
		settings = DefaultT3SidecarSettings()
	}
	return &T3Api{
		settings:            settings,
		handler:             router.NewRouter(ctx, slices.Concat(api.Routes(), oauth.Routes())),
		capture:             net.Capture(),
		secrets:             map[string]bool{},
		clientIdTokens:      map[server.Id]string{},
		serviceHostHandlers: map[string]http.Handler{},
	}
}

// The settings T3Api runs with, which its sidecars take their origins from.
func (self *T3Api) Settings() *T3SidecarSettings {
	return self.settings
}

// The origin a sidecar names for service: ServiceOriginFormat with service's name.
func (self *T3Api) ServiceOrigin(service string) string {
	return fmt.Sprintf(self.settings.ServiceOriginFormat, service)
}

// The host of service's origin, lowercased as a request's host is matched, or an error for a service ServiceNames
// does not name or an origin that is no https url with a host.
func (self *T3Api) serviceHost(service string) (string, error) {
	if !slices.Contains(self.settings.ServiceNames, service) {
		return "", fmt.Errorf("%q is no service T3Api serves at an origin (T3SidecarSettings.ServiceNames)", service)
	}
	origin, err := url.Parse(self.ServiceOrigin(service))
	if err != nil || origin.Scheme != "https" || origin.Host == "" {
		return "", fmt.Errorf("the origin of %s, %q, is no https origin", service, self.ServiceOrigin(service))
	}
	return strings.ToLower(origin.Host), nil
}

// Serves service's origin through handler from now on, as the lb routes the service's host to its block (the file's
// header); nil stops serving it, and its origin answers as api does again. A service ServiceNames does not name is
// refused, and so is a second handler for a service already served, which a test stops first.
func (self *T3Api) ServeService(service string, handler http.Handler) error {
	host, err := self.serviceHost(service)
	if err != nil {
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if handler == nil {
		delete(self.serviceHostHandlers, host)
		return nil
	}
	if _, served := self.serviceHostHandlers[host]; served {
		return fmt.Errorf("%s is served already", service)
	}
	self.serviceHostHandlers[host] = handler
	return nil
}

// Serves api's internal gate surface, every path under server.InternalGatePathPrefix, through handler from now on: a
// world's own internal gate routes on its throwaway bearers' digests, as api's are on its vault's verify file, which a
// test's api has none of. nil serves api's own routes there again, which refuse every bearer for want of the file.
func (self *T3Api) ServeInternalGate(handler http.Handler) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.internalGateHandler = handler
}

// The handler api serves request with: the internal gate's for a path under its prefix while one is set, else api's
// router.
func (self *T3Api) apiHandler(request *http.Request) http.Handler {
	if request.URL != nil && strings.HasPrefix(request.URL.Path, server.InternalGatePathPrefix) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.internalGateHandler != nil {
			return self.internalGateHandler
		}
	}
	return self.handler
}

// The handler serving request's origin, nil for none: an https request to a served service's host.
func (self *T3Api) serviceHandler(request *http.Request) http.Handler {
	if request.URL == nil || request.URL.Scheme != "https" {
		return nil
	}
	host := strings.ToLower(request.URL.Host)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.serviceHostHandlers[host]
}

// A credential the scan looks for, added once.
func (self *T3Api) addSecret(kind string, value string) {
	if value == "" {
		return
	}
	added := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.secrets[value] {
			return false
		}
		self.secrets[value] = true
		return true
	}()
	if added {
		self.capture.AddSecret(kind, []byte(value))
	}
}

// Every credential an answer's JSON carries under one of the secret keys, at any depth.
func (self *T3Api) addAnswerSecrets(route string, answer []byte) {
	var walk func(value any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				if text, ok := child.(string); ok && slices.Contains(self.settings.SecretAnswerKeys, key) {
					self.addSecret(key+" from "+route, text)
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	var decoded any
	if json.Unmarshal(answer, &decoded) == nil {
		walk(decoded)
	}
	enrolled := &struct {
		ByClientJwt string    `json:"by_client_jwt"`
		ClientId    server.Id `json:"client_id"`
	}{}
	if json.Unmarshal(answer, enrolled) == nil && enrolled.ByClientJwt != "" && enrolled.ClientId != (server.Id{}) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.clientIdTokens[enrolled.ClientId] = enrolled.ByClientJwt
	}
}

// The newest token an enrollment or a mint answered for clientId, "" for none: what a scenario presents to call api as
// that client's sidecar does. A refresh's token, whose answer names no client, is not kept here.
func (self *T3Api) ClientToken(clientId sidecar.Id) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.clientIdTokens[server.Id(clientId)]
}

// The transport a sidecar's SidecarApi and a scenario's own client send through. See T3Api.
func (self *T3Api) Transport() http.RoundTripper {
	return &t3ApiTransport{api: self}
}

type t3ApiTransport struct {
	api *T3Api
}

func (self *t3ApiTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	settings := self.api.settings
	if handler := self.api.serviceHandler(request); handler != nil {
		if bearer, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer "); ok {
			self.api.addSecret("a bearer sent to "+request.URL.Host, bearer)
		}
		return serveServiceRequest(handler, request, settings.RemoteAddr)
	}
	if bearer, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer "); ok {
		self.api.addSecret("a bearer sent to api", bearer)
	}
	var body []byte
	if request.Body != nil {
		var err error
		body, err = io.ReadAll(request.Body)
		request.Body.Close()
		if err != nil {
			return nil, err
		}
	}
	served := httptest.NewRequestWithContext(request.Context(), request.Method, request.URL.RequestURI(), bytes.NewReader(body))
	served.Header = request.Header.Clone()
	served.RemoteAddr = settings.RemoteAddr
	recorder := httptest.NewRecorder()
	self.api.apiHandler(request).ServeHTTP(recorder, served)
	route := request.Method + " " + request.URL.Path
	self.api.addAnswerSecrets(route, recorder.Body.Bytes())
	response := recorder.Result()
	response.Request = request
	return response, nil
}

// One request through api as a scenario sends it: its status and body. authorization "" sends none.
func (self *T3Api) Call(ctx context.Context, method string, path string, authorization string, body any) (int, []byte, error) {
	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		bodyReader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, self.settings.ApiOrigin+path, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := self.Transport().RoundTrip(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	return response.StatusCode, answer, err
}

// A T3 sidecar's host: its instance's store, a memory key store, no token source, and no credential store of this
// machine's.
type t3SidecarHost struct {
	store    sidecar.SidecarStore
	keyStore sidecar.SidecarKeyStore
}

func (self *t3SidecarHost) Store() sidecar.SidecarStore {
	return self.store
}

func (self *t3SidecarHost) KeyStore() sidecar.SidecarKeyStore {
	return self.keyStore
}

func (self *t3SidecarHost) TokenSource() sidecar.SidecarTokenSource {
	return nil
}

// None (sidecar.SidecarHostCredentialScanner), so the build machine's own app state never decides a run.
func (self *t3SidecarHost) HostCredentialStores() []sidecar.SidecarHostCredentialStore {
	return nil
}

// T3Api for a sidecar's requests and server's exchange for its clients.
type t3SidecarPlatform struct {
	api      *T3Api
	exchange *exchangePlatform
}

func (self *t3SidecarPlatform) ApiTransport() http.RoundTripper {
	return self.api.Transport()
}

// One generation's client on server's exchange: the generation's own client id, settings and token, loopback ICE
// alone, server's controller as its control, and an h1 transport to server's handler, which checks the token. See
// sidecar.SidecarPlatform.
func (self *t3SidecarPlatform) OpenClient(ctx context.Context, args *sidecar.SidecarPlatformClientArgs) (*sidecar.SidecarPlatformClient, error) {
	clientSettings := *args.ClientSettings
	if clientSettings.WebRtcSettings != nil {
		webRtcSettings := *clientSettings.WebRtcSettings
		webRtcSettings.IceServerUrls = nil
		webRtcSettings.IceServerPoolUrls = nil
		webRtcSettings.UseLoopbackOnlyIceInterfaces = true
		clientSettings.WebRtcSettings = &webRtcSettings
	}
	client := connect.NewClient(
		ctx,
		args.ClientId,
		&controllerOutOfBandControl{
			ctx:                     self.exchange.ctx,
			clientId:                server.Id(args.ClientId),
			contractManagerSettings: clientSettings.ContractManagerSettings,
		},
		&clientSettings,
	)
	transportSettings := *args.PlatformTransportSettings
	// no dns carrier, as the exchange's own clients dial none
	transportSettings.DnsPort = 0
	transport := connect.NewPlatformTransportWithTargetMode(
		ctx,
		self.exchange.clientStrategy,
		client.RouteManager(),
		self.exchange.platformUrl,
		args.Auth,
		connect.TransportModeH1,
		&transportSettings,
	)
	return &sidecar.SidecarPlatformClient{
		Client:  client,
		SetAuth: transport.SetAuth,
		Connected: func() (bool, <-chan struct{}) {
			// armed first, as ConnectedNotify asks
			notify := transport.ConnectedNotify()
			return transport.IsConnected(), notify
		},
		CloseAndWait: func(ctx context.Context) error {
			client.Cancel()
			transportErr := transport.CloseAndWait(ctx)
			clientErr := client.CloseAndWait(ctx)
			if transportErr != nil {
				return transportErr
			}
			return clientErr
		},
	}, nil
}

// The capture's logger up to a verbosity.
type t3SidecarLogger struct {
	capture  sidecar.Logger
	maxLevel int32
}

func (self *t3SidecarLogger) Info(args ...any) {
	self.capture.Info(args...)
}

func (self *t3SidecarLogger) Infof(format string, args ...any) {
	self.capture.Infof(format, args...)
}

func (self *t3SidecarLogger) Warningf(format string, args ...any) {
	self.capture.Warningf(format, args...)
}

func (self *t3SidecarLogger) Errorf(format string, args ...any) {
	self.capture.Errorf(format, args...)
}

func (self *t3SidecarLogger) V(level int32) sidecar.Verbose {
	if self.maxLevel < level {
		return sidecar.NewNoopLogger().V(level)
	}
	return self.capture.V(level)
}

// Server's exchange for a scenario's T3 sidecars, until ctx ends or the returned close. Needs server's schema and the
// private stores, as a T3 Net with clients does.
func NewT3SidecarExchange(ctx context.Context, t testing.TB, settings *ExchangeSettings) (*T3SidecarExchange, error) {
	if settings == nil {
		settings = DefaultExchangeSettings()
	}
	platform, err := newExchangePlatform(ctx, t, settings)
	if err != nil {
		return nil, err
	}
	return &T3SidecarExchange{platform: platform}, nil
}

// Server's exchange as T3 sidecars dial it.
type T3SidecarExchange struct {
	platform *exchangePlatform
}

// Closes the exchange, its handler and its http server. Every sidecar on it is closed first.
func (self *T3SidecarExchange) Close() {
	self.platform.Close()
}

// The url a client's platform transport dials: the exchange's connect handler on its loopback listener.
func (self *T3SidecarExchange) PlatformUrl() string {
	return self.platform.platformUrl
}

// One sidecar on T3: its Sidecar, its instance and what ends them.
type T3Sidecar struct {
	Name     string
	Settings *sidecar.SidecarSettings

	t3Settings *T3SidecarSettings
	running    *sidecar.Sidecar
	instance   *sidecar.SidecarInstance
	cancel     context.CancelFunc
	closeOnce  sync.Once
}

// A running sidecar named name on a memory instance of its own, Unenrolled, its api T3Api and its clients on exchange.
// settings nil takes DefaultSidecarSettings; either way what ties it to T3 is set here. The caller closes it.
func StartT3Sidecar(ctx context.Context, net *sidecartest.Net, name string, t3Api *T3Api, exchange *T3SidecarExchange, settings *sidecar.SidecarSettings) (*T3Sidecar, error) {
	if settings == nil {
		settings = sidecar.DefaultSidecarSettings()
	}
	t3Settings := t3Api.settings
	settings.ApiUrl = t3Settings.ApiOrigin
	settings.AuthUrl = t3Settings.ApiOrigin
	settings.PlatformUrl = exchange.platform.platformUrl
	settings.EvsUrl = t3Api.ServiceOrigin("evs")
	settings.GkmUrl = t3Api.ServiceOrigin("gkm")
	settings.SocketsUrl = t3Api.ServiceOrigin("sockets")
	settings.DnsUrl = t3Api.ServiceOrigin("dns")
	settings.Clock = sidecar.RealClock{}
	settings.Log = &t3SidecarLogger{capture: net.Capture().Logger("sidecar " + name), maxLevel: t3Settings.MaxLogLevel}
	settings.KeychainSettings.AutoLockTimeout = 0
	settings.Connect.Platform = &t3SidecarPlatform{api: t3Api, exchange: exchange.platform}

	runCtx, cancel := context.WithCancel(ctx)
	keyStore := sidecar.NewSidecarMemoryKeyStore()
	instance, err := sidecar.NewSidecarInstance(runCtx, &sidecar.SidecarInstanceArgs{
		Backend:  sidecar.NewSidecarMemoryBackend(),
		KeyStore: keyStore,
		Settings: settings,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	if err := instance.Unlock(nil); err != nil {
		instance.Close()
		cancel()
		return nil, err
	}
	running, err := sidecar.NewSidecar(runCtx, &t3SidecarHost{store: instance.Store(nil), keyStore: keyStore}, nil, settings)
	if err != nil {
		instance.Close()
		cancel()
		return nil, err
	}
	return &T3Sidecar{Name: name, Settings: settings, t3Settings: t3Settings, running: running, instance: instance, cancel: cancel}, nil
}

// The running Sidecar.
func (self *T3Sidecar) Sidecar() *sidecar.Sidecar {
	return self.running
}

// Waits until the sidecar's state is state, or StateTimeout passes.
func (self *T3Sidecar) AwaitState(ctx context.Context, state sidecar.SidecarState) error {
	ctx, cancel := context.WithTimeout(ctx, self.t3Settings.StateTimeout)
	defer cancel()
	changed := make(chan struct{}, 1)
	// subscribed before the read, so a move between them wakes the wait
	remove := self.running.AddEventCallback(func(event sidecar.SidecarEvent) {
		if event.Kind != sidecar.SidecarEventState {
			return
		}
		select {
		case changed <- struct{}{}:
		default:
		}
	})
	defer remove()
	for {
		status := self.running.Status()
		if status.State == state {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s is %s (%s), not %s: %w", self.Name, status.State, status.Reason, state, ctx.Err())
		case <-changed:
		}
	}
}

// Enrolls from credential, which Enroll erases, and waits until the sidecar is Online on server's exchange.
func (self *T3Sidecar) EnrollOnline(ctx context.Context, credential *sidecar.SidecarEnrollCredential) error {
	if err := self.AwaitState(ctx, sidecar.SidecarStateUnenrolled); err != nil {
		return err
	}
	if err := self.running.Enroll(ctx, credential); err != nil {
		return fmt.Errorf("%s did not enroll: %w", self.Name, err)
	}
	return self.AwaitState(ctx, sidecar.SidecarStateOnline)
}

// Ends the sidecar, waiting for its generation, and its instance. Safe to call again.
func (self *T3Sidecar) Close() {
	self.closeOnce.Do(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), self.t3Settings.CloseTimeout)
		defer closeCancel()
		self.running.CloseAndWait(closeCtx)
		self.instance.Close()
		self.cancel()
	})
}
