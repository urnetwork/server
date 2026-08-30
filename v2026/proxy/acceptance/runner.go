// Package acceptance drives the deployed proxy control plane and each public
// proxy data plane without changing the host's routes or proxy settings.
package acceptance

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	Platform               = "server/proxy"
	defaultProbeTimeout    = 120 * time.Second
	defaultSoakDuration    = 5 * time.Minute
	defaultSoakInterval    = 5 * time.Second
	readinessRetryInterval = 2 * time.Second
	maxAPIResponseBytes    = 1024 * 1024
	maxProbeResponseBytes  = 64 * 1024
)

var protocolNames = []string{"socks", "http", "wireguard"}

// Options identifies one production acceptance campaign.
type Options struct {
	APIURL          string
	TargetURL       string
	CredentialsPath string
	Repeat          int
	ProbeTimeout    time.Duration
	SoakDuration    time.Duration
	SoakInterval    time.Duration
	// TrackHostedDevice records a redacted control-plane timeline for the
	// temporary hosted DeviceLocal. It does not change device settings; failures
	// can then be joined to the provider and reliability state that carried them.
	TrackHostedDevice bool
	// OverlapProtocols repeats the sustained campaigns concurrently on the
	// same provisioned proxy device. Users may enable HTTP, SOCKS, and
	// WireGuard at the same time; a sequential-only check cannot detect a
	// device-wide return-path switch that makes those protocols steal packets
	// from one another.
	OverlapProtocols bool
	// Progress receives identity-free, redacted campaign milestones. It is
	// optional so package callers that only consume the result matrix remain
	// silent.
	Progress func(string)
}

// Result is one row in the root acceptance matrix.
type Result struct {
	Case   string
	Status string
	Detail string
}

type credentials struct {
	user     string
	password string
}

type apiError struct {
	Message string `json:"message"`
}

type loginResult struct {
	Network *struct {
		ByJWT string `json:"by_jwt"`
	} `json:"network,omitempty"`
	VerificationRequired *struct {
		UserAuth string `json:"user_auth"`
	} `json:"verification_required,omitempty"`
	Error *apiError `json:"error,omitempty"`
}

type proxyConfigResult struct {
	SocksProxyURL string           `json:"socks_proxy_url"`
	HTTPProxyURL  string           `json:"http_proxy_url"`
	APIBaseURL    string           `json:"api_base_url"`
	AuthToken     string           `json:"auth_token"`
	ProxyHost     string           `json:"proxy_host"`
	InstanceID    string           `json:"instance_id"`
	WgConfig      *wireGuardConfig `json:"wg_config"`
}

type wireGuardConfig struct {
	ProxyPort        int    `json:"wg_proxy_port"`
	ClientPrivateKey string `json:"client_private_key"`
	ClientPublicKey  string `json:"client_public_key"`
	ProxyPublicKey   string `json:"proxy_public_key"`
	ClientIPv4       string `json:"client_ipv4"`
	Config           string `json:"config"`
}

type provisionResult struct {
	ClientID          string             `json:"client_id"`
	ByClientJWT       string             `json:"by_client_jwt"`
	ProxyConfigResult *proxyConfigResult `json:"proxy_config_result,omitempty"`
	Error             *apiError          `json:"error,omitempty"`
}

func urlPort(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "invalid"
	}
	if port := parsed.Port(); port != "" {
		return port
	}
	switch parsed.Scheme {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return "missing"
	}
}

func wireGuardPort(config *proxyConfigResult) int {
	if config == nil || config.WgConfig == nil {
		return 0
	}
	return config.WgConfig.ProxyPort
}

type removeResult struct {
	Error *apiError `json:"error,omitempty"`
}

type apiClient struct {
	baseURL string
	client  *http.Client
}

type protocolProbe func(context.Context) error
type protocolProbeFactory func(*proxyConfigResult) map[string]protocolProbe

// httpsRequestTrace records the last completed transport milestone. Callbacks
// can arrive from transport goroutines, so snapshots are safe for concurrent
// use while an HTTP request is being canceled.
type httpsRequestTrace struct {
	stateLock sync.Mutex
	started   time.Time
	phase     string
	gotConn   bool
	reused    bool
}

// Builds the net/http trace that advances one request's diagnostic phase.
func (self *httpsRequestTrace) clientTrace() *httptrace.ClientTrace {
	setPhase := func(phase string) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.phase = phase
	}
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { setPhase("resolving_target") },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if info.Err != nil {
				setPhase("resolving_target_failed")
			} else {
				setPhase("target_resolved")
			}
		},
		ConnectStart: func(_, _ string) { setPhase("connecting_tunnel") },
		ConnectDone: func(_, _ string, err error) {
			if err != nil {
				setPhase("connecting_tunnel_failed")
			} else {
				setPhase("tunnel_connected")
			}
		},
		TLSHandshakeStart: func() { setPhase("tls_handshake") },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err != nil {
				setPhase("tls_handshake_failed")
			} else {
				setPhase("tls_complete")
			}
		},
		GotConn: func(info httptrace.GotConnInfo) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.phase = "sending_request"
			self.gotConn = true
			self.reused = info.Reused
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err != nil {
				setPhase("sending_request_failed")
			} else {
				setPhase("waiting_for_response_headers")
			}
		},
		GotFirstResponseByte: func() { setPhase("reading_response") },
	}
}

// Adds identity-free timing and phase evidence to a transport error.
func (self *httpsRequestTrace) wrap(err error, finished time.Time) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	connection := "not_established"
	if self.gotConn {
		connection = "new"
		if self.reused {
			connection = "reused"
		}
	}
	return fmt.Errorf(
		"request started %s; elapsed %s; phase %s; connection %s: %w",
		self.started.UTC().Format(time.RFC3339Nano),
		finished.Sub(self.started).Round(time.Millisecond),
		self.phase,
		connection,
		err,
	)
}

type runDependencies struct {
	credentials *credentials
	httpClient  *http.Client
	probes      protocolProbeFactory
	tracker     hostedDeviceTrackerFactory
}

type runner struct {
	opts         Options
	api          *apiClient
	creds        credentials
	redactor     *redactor
	newProbes    protocolProbeFactory
	newTracker   hostedDeviceTrackerFactory
	progressLock sync.Mutex
}

// progressf emits one redacted milestone without making diagnostics part of
// the result contract. Overlapping campaigns report from several goroutines;
// serialize cleaning and delivery so every milestone remains one complete
// line and non-concurrent callbacks are safe.
func (r *runner) progressf(format string, args ...any) {
	if r.opts.Progress == nil {
		return
	}
	r.progressLock.Lock()
	defer r.progressLock.Unlock()
	message := fmt.Sprintf(format, args...)
	if r.redactor != nil {
		message = r.redactor.clean(message)
	}
	r.opts.Progress(message)
}

// Run executes all three protocols for every requested repetition. It always
// returns exactly one result per supported protocol so orchestration can
// distinguish a failed check from a runner that did not report it.
func Run(ctx context.Context, opts Options) []Result {
	return runWithDependencies(ctx, opts, runDependencies{})
}

func runWithDependencies(ctx context.Context, opts Options, deps runDependencies) []Result {
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = defaultProbeTimeout
	}
	if opts.SoakDuration == 0 {
		opts.SoakDuration = defaultSoakDuration
	}
	if opts.SoakInterval == 0 {
		opts.SoakInterval = defaultSoakInterval
	}
	if err := validateOptions(opts); err != nil {
		return failedResults(err.Error())
	}

	var creds credentials
	var err error
	if deps.credentials != nil {
		creds = *deps.credentials
	} else {
		creds, err = readCredentials(opts.CredentialsPath)
		if err != nil {
			return failedResults(fmt.Sprintf("credentials: %v", err))
		}
	}

	httpClient := deps.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 45 * time.Second}
	}
	r := &runner{
		opts:       opts,
		api:        &apiClient{baseURL: strings.TrimRight(opts.APIURL, "/"), client: httpClient},
		creds:      creds,
		redactor:   newRedactor(creds.user, creds.password),
		newProbes:  deps.probes,
		newTracker: deps.tracker,
	}
	if r.newProbes == nil {
		r.newProbes = r.productionProbes
	}
	if r.newTracker == nil && opts.TrackHostedDevice {
		r.newTracker = r.productionHostedDeviceTracker
	}

	passes := map[string]int{}
	failures := map[string][]string{}
	for repetition := 1; repetition <= opts.Repeat; repetition++ {
		r.progressf("repetition %d/%d started at %s", repetition, opts.Repeat, time.Now().UTC().Format(time.RFC3339Nano))
		iteration := r.runIteration(ctx)
		for _, name := range protocolNames {
			if err := iteration[name]; err != nil {
				failures[name] = append(failures[name], fmt.Sprintf("repetition %d: %v", repetition, err))
			} else {
				passes[name]++
			}
		}
		r.progressf("repetition %d/%d finished at %s", repetition, opts.Repeat, time.Now().UTC().Format(time.RFC3339Nano))
		if ctx.Err() != nil {
			for remaining := repetition + 1; remaining <= opts.Repeat; remaining++ {
				for _, name := range protocolNames {
					failures[name] = append(failures[name], fmt.Sprintf("repetition %d: %v", remaining, ctx.Err()))
				}
			}
			break
		}
	}

	labels := map[string]string{
		"socks":     "SOCKS5",
		"http":      "HTTP CONNECT",
		"wireguard": "userspace WireGuard",
	}
	results := make([]Result, 0, len(protocolNames))
	for _, name := range protocolNames {
		if len(failures[name]) == 0 && passes[name] == opts.Repeat {
			successfulRequestsPerCampaign := 1 + int(opts.SoakDuration/opts.SoakInterval)
			results = append(results, Result{
				Case:   name,
				Status: "PASS",
				Detail: fmt.Sprintf(
					"%d/%d sustained campaigns succeeded through %s (%d HTTPS requests each over %s)",
					passes[name],
					opts.Repeat,
					labels[name],
					successfulRequestsPerCampaign,
					opts.SoakDuration,
				),
			})
			if opts.OverlapProtocols {
				results[len(results)-1].Detail += "; the same campaign also passed while all proxy protocols overlapped"
			}
			continue
		}
		detail := strings.Join(failures[name], "; ")
		if detail == "" {
			detail = fmt.Sprintf("only %d/%d repetitions completed", passes[name], opts.Repeat)
		}
		results = append(results, Result{Case: name, Status: "FAIL", Detail: r.redactor.clean(detail)})
	}
	return results
}

func validateOptions(opts Options) error {
	if opts.APIURL == "" || opts.TargetURL == "" || opts.CredentialsPath == "" || opts.Repeat < 1 {
		return errors.New("API URL, target URL, credentials path, and a positive repeat are required")
	}
	if opts.ProbeTimeout < 0 {
		return errors.New("probe timeout cannot be negative")
	}
	if opts.SoakDuration < 0 {
		return errors.New("soak duration cannot be negative")
	}
	if opts.SoakInterval <= 0 {
		return errors.New("soak interval must be positive")
	}
	if opts.SoakDuration < opts.SoakInterval {
		return errors.New("soak duration must include at least one soak interval")
	}
	for label, raw := range map[string]string{"API": opts.APIURL, "target": opts.TargetURL} {
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return fmt.Errorf("%s URL must be an absolute HTTP(S) URL", label)
		}
	}
	return nil
}

func (r *runner) runIteration(ctx context.Context) map[string]error {
	result := map[string]error{}
	jwt, err := r.login(ctx)
	if err != nil {
		return errorsForEveryProtocol(fmt.Errorf("password login: %w", err))
	}
	r.redactor.add(jwt)
	if ctx.Err() != nil {
		return errorsForEveryProtocol(ctx.Err())
	}

	// Client creation is mutating. Once its request starts, let it finish even
	// if the campaign is interrupted so the response's client ID remains
	// available for deterministic cleanup instead of becoming ambiguous.
	provisionCtx, provisionCancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	provisioned, provisionErr := r.provision(provisionCtx, jwt)
	provisionCancel()
	if provisioned.ClientID != "" {
		r.redactor.add(provisioned.ClientID)
	}
	if provisioned.ByClientJWT != "" {
		r.redactor.add(provisioned.ByClientJWT)
	}
	if provisioned.ProxyConfigResult != nil {
		r.redactor.addProxyConfig(provisioned.ProxyConfigResult)
	}

	var tracker hostedDeviceTracker
	if provisionErr != nil {
		result = errorsForEveryProtocol(fmt.Errorf("provision proxy client: %w", provisionErr))
	} else if provisioned.ProxyConfigResult == nil {
		result = errorsForEveryProtocol(errors.New("provision proxy client returned no proxy configuration"))
	} else if ctx.Err() != nil {
		result = errorsForEveryProtocol(ctx.Err())
	} else {
		r.progressf("temporary client assigned to proxy host %s", provisioned.ProxyConfigResult.ProxyHost)
		r.progressf(
			"temporary client public ports http=%s socks=%s wireguard=%d",
			urlPort(provisioned.ProxyConfigResult.HTTPProxyURL),
			urlPort(provisioned.ProxyConfigResult.SocksProxyURL),
			wireGuardPort(provisioned.ProxyConfigResult),
		)
		if r.newTracker != nil {
			tracker, err = r.newTracker(ctx, provisioned)
			if err != nil {
				r.progressf("hosted device diagnostics unavailable: %v", err)
			} else {
				r.progressf("hosted device diagnostics started")
			}
		}
		probes := r.newProbes(provisioned.ProxyConfigResult)
		// Establish an isolated baseline first. HTTP stays first because it opens
		// and warms a newly placed hosted device; the optional concurrent pass
		// below then proves the paths do not interfere with one another.
		for _, name := range []string{"http", "socks", "wireguard"} {
			probe := probes[name]
			if probe == nil {
				result[name] = errors.New("runner did not configure this protocol")
				continue
			}
			started := time.Now()
			r.progressf("%s campaign started at %s", name, started.UTC().Format(time.RFC3339Nano))
			result[name] = probe(ctx)
			if result[name] != nil {
				result[name] = withHostedDeviceDiagnostics(result[name], tracker)
				r.progressf("%s campaign failed after %s: %v", name, time.Since(started).Round(time.Millisecond), result[name])
			} else {
				r.progressf("%s campaign passed after %s", name, time.Since(started).Round(time.Millisecond))
			}
		}

		if r.opts.OverlapProtocols && ctx.Err() == nil {
			r.progressf("overlapping HTTP CONNECT, SOCKS5, and WireGuard campaigns started")
			var overlapGroup sync.WaitGroup
			var overlapLock sync.Mutex
			overlapErrors := map[string]error{}
			for _, name := range []string{"http", "socks", "wireguard"} {
				probe := probes[name]
				if probe == nil {
					overlapErrors[name] = errors.New("runner did not configure this protocol")
					continue
				}
				overlapGroup.Add(1)
				go func(name string, probe protocolProbe) {
					defer overlapGroup.Done()
					started := time.Now()
					r.progressf("%s overlapping campaign started at %s", name, started.UTC().Format(time.RFC3339Nano))
					err := probe(ctx)
					if err != nil {
						err = withHostedDeviceDiagnostics(err, tracker)
					}
					overlapLock.Lock()
					overlapErrors[name] = err
					overlapLock.Unlock()
					if err != nil {
						r.progressf("%s overlapping campaign failed after %s: %v", name, time.Since(started).Round(time.Millisecond), err)
					} else {
						r.progressf("%s overlapping campaign passed after %s", name, time.Since(started).Round(time.Millisecond))
					}
				}(name, probe)
			}
			overlapGroup.Wait()
			for _, name := range protocolNames {
				if overlapErr := overlapErrors[name]; overlapErr != nil {
					result[name] = errors.Join(result[name], fmt.Errorf("overlapping protocols: %w", overlapErr))
				}
			}
			r.progressf("overlapping proxy protocol campaigns finished")
		}
	}
	if tracker != nil {
		tracker.Close()
	}

	if provisioned.ClientID != "" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		cleanupErr := r.remove(cleanupCtx, jwt, provisioned.ClientID)
		cancel()
		if cleanupErr != nil {
			for _, name := range protocolNames {
				result[name] = errors.Join(result[name], fmt.Errorf("remove temporary client: %w", cleanupErr))
			}
		}
		if cleanupErr == nil {
			r.progressf("temporary client cleanup completed")
		}
	}

	for _, name := range protocolNames {
		if _, ok := result[name]; !ok {
			result[name] = errors.New("protocol did not run")
		}
	}
	return result
}

func (r *runner) login(ctx context.Context) (string, error) {
	var result loginResult
	err := r.api.post(ctx, "/auth/login-with-password", map[string]any{
		"user_auth": r.creds.user,
		"password":  r.creds.password,
	}, "", &result)
	if err != nil {
		return "", err
	}
	if result.Error != nil {
		return "", errors.New(result.Error.Message)
	}
	if result.VerificationRequired != nil {
		return "", errors.New("configured data-plane account unexpectedly requires verification")
	}
	if result.Network == nil || result.Network.ByJWT == "" {
		return "", errors.New("response contained no network JWT")
	}
	return result.Network.ByJWT, nil
}

func (r *runner) provision(ctx context.Context, jwt string) (provisionResult, error) {
	var result provisionResult
	err := r.api.post(ctx, "/network/auth-client", map[string]any{
		"description": "URnetwork proxy acceptance",
		"device_spec": runtime.GOOS + "/" + runtime.GOARCH,
		"proxy_config": map[string]any{
			"lock_caller_ip": false,
			"enable_wg":      true,
			"initial_device_state": map[string]any{
				"country_code": "us",
			},
		},
	}, jwt, &result)
	if err != nil {
		return result, err
	}
	if result.Error != nil {
		return result, errors.New(result.Error.Message)
	}
	if result.ClientID == "" {
		return result, errors.New("response contained no client ID")
	}
	return result, nil
}

func (r *runner) remove(ctx context.Context, jwt, clientID string) error {
	var result removeResult
	if err := r.api.post(ctx, "/network/remove-client", map[string]any{"client_id": clientID}, jwt, &result); err != nil {
		return err
	}
	if result.Error != nil && result.Error.Message != "Client does not exist." {
		return errors.New(result.Error.Message)
	}
	return nil
}

func (a *apiClient) post(ctx context.Context, path string, body any, jwt string, output any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Client-Version", "1.0.0-proxy-acceptance")
	if jwt != "" {
		request.Header.Set("Authorization", "Bearer "+jwt)
	}
	response, err := a.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxAPIResponseBytes {
		return fmt.Errorf("%s response exceeded %d bytes", path, maxAPIResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var envelope struct {
			Error *apiError `json:"error"`
		}
		_ = json.Unmarshal(data, &envelope)
		if envelope.Error != nil && envelope.Error.Message != "" {
			return fmt.Errorf("%s returned HTTP %d: %s", path, response.StatusCode, envelope.Error.Message)
		}
		return fmt.Errorf("%s returned HTTP %d", path, response.StatusCode)
	}
	if err := json.Unmarshal(data, output); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

func (r *runner) productionProbes(config *proxyConfigResult) map[string]protocolProbe {
	return map[string]protocolProbe{
		"http": func(ctx context.Context) error {
			if config.HTTPProxyURL == "" || config.AuthToken == "" {
				return errors.New("HTTP proxy URL or authentication token is missing")
			}
			proxyURL, err := url.Parse(config.HTTPProxyURL)
			if err != nil || proxyURL.Host == "" {
				return errors.New("HTTP proxy URL is invalid")
			}
			proxyURL.User = url.UserPassword(config.AuthToken, "acceptance")
			transport := &http.Transport{Proxy: http.ProxyURL(proxyURL)}
			defer transport.CloseIdleConnections()
			_, err = probeHTTPSCampaign(
				ctx,
				"HTTP CONNECT",
				r.opts.TargetURL,
				transport,
				r.opts.ProbeTimeout,
				r.opts.SoakDuration,
				r.opts.SoakInterval,
				waitForProbeInterval,
				r.progressf,
			)
			return err
		},
		"socks": func(ctx context.Context) error {
			if config.SocksProxyURL == "" || config.AuthToken == "" {
				return errors.New("SOCKS proxy is unavailable; verify that the acceptance account includes the SOCKS feature")
			}
			proxyURL, err := url.Parse(config.SocksProxyURL)
			if err != nil || proxyURL.Host == "" {
				return errors.New("SOCKS proxy URL is invalid")
			}
			dialer, err := xproxy.SOCKS5("tcp", proxyURL.Host, &xproxy.Auth{User: config.AuthToken, Password: "acceptance"}, xproxy.Direct)
			if err != nil {
				return fmt.Errorf("create SOCKS5 dialer: %w", err)
			}
			contextDialer, ok := dialer.(xproxy.ContextDialer)
			if !ok {
				return errors.New("SOCKS5 dialer does not support request cancellation")
			}
			transport := &http.Transport{DialContext: contextDialer.DialContext}
			defer transport.CloseIdleConnections()
			_, err = probeHTTPSCampaign(
				ctx,
				"SOCKS5",
				r.opts.TargetURL,
				transport,
				r.opts.ProbeTimeout,
				r.opts.SoakDuration,
				r.opts.SoakInterval,
				waitForProbeInterval,
				r.progressf,
			)
			return err
		},
		"wireguard": func(ctx context.Context) error {
			if config.WgConfig == nil {
				return errors.New("WireGuard configuration is unavailable; verify that the acceptance account includes the WireGuard feature")
			}
			transport, closeClient, err := newWireGuardTransport(ctx, config.ProxyHost, config.WgConfig)
			if err != nil {
				return err
			}
			defer func() {
				transport.CloseIdleConnections()
				closeClient()
			}()
			_, err = probeHTTPSCampaign(
				ctx,
				"WireGuard",
				r.opts.TargetURL,
				transport,
				r.opts.ProbeTimeout,
				r.opts.SoakDuration,
				r.opts.SoakInterval,
				waitForProbeInterval,
				r.progressf,
			)
			return err
		},
	}
}

type targetHTTPStatusError struct {
	statusCode int
}

func (e *targetHTTPStatusError) Error() string {
	return fmt.Sprintf("target returned HTTP %d", e.statusCode)
}

type probeIntervalWait func(context.Context, time.Duration) error

func probeHTTPSCampaign(
	ctx context.Context,
	protocol string,
	target string,
	transport http.RoundTripper,
	retryWindow time.Duration,
	soakDuration time.Duration,
	soakInterval time.Duration,
	wait probeIntervalWait,
	progress func(string, ...any),
) (int, error) {
	if wait == nil {
		wait = waitForProbeInterval
	}
	probeCtx, cancel := context.WithTimeout(ctx, retryWindow)
	defer cancel()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	var lastErr error
	for {
		lastErr = probeHTTPSRequest(probeCtx, client, target)
		if lastErr == nil {
			break
		}
		var statusErr *targetHTTPStatusError
		if errors.As(lastErr, &statusErr) && statusErr.statusCode == http.StatusTooManyRequests {
			return 0, fmt.Errorf("%s readiness was rate limited: %w", protocol, lastErr)
		}
		if probeCtx.Err() != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("%s path did not reach the HTTPS target within %s: %w", protocol, retryWindow, lastErr)
		}
		if err := wait(probeCtx, readinessRetryInterval); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, fmt.Errorf("%s path did not reach the HTTPS target within %s: %w", protocol, retryWindow, lastErr)
		}
	}

	successfulRequests := 1
	sustainedRequests := int(soakDuration / soakInterval)
	if progress != nil {
		progress("%s readiness request passed; starting %d sustained requests", protocol, sustainedRequests)
	}
	for requestIndex := 1; requestIndex <= sustainedRequests; requestIndex++ {
		if err := wait(ctx, soakInterval); err != nil {
			return successfulRequests, err
		}
		if err := probeHTTPSRequest(ctx, client, target); err != nil {
			return successfulRequests, fmt.Errorf(
				"%s sustained request %d/%d failed after %d successful requests: %w",
				protocol,
				requestIndex,
				sustainedRequests,
				successfulRequests,
				err,
			)
		}
		successfulRequests++
		if progress != nil && (requestIndex == sustainedRequests || requestIndex%12 == 0) {
			progress("%s sustained progress %d/%d", protocol, requestIndex, sustainedRequests)
		}
	}
	return successfulRequests, nil
}

func probeHTTPSRequest(ctx context.Context, client *http.Client, target string) error {
	requestTrace := &httpsRequestTrace{started: time.Now(), phase: "starting_request"}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), requestTrace.clientTrace()))
	request.Header.Set("Accept", "text/plain, */*")
	request.Header.Set("User-Agent", "urnetwork-proxy-acceptance/1")
	request.Close = true
	response, err := client.Do(request)
	if err != nil {
		return requestTrace.wrap(err, time.Now())
	}
	requestTrace.stateLock.Lock()
	requestTrace.phase = "reading_response_body"
	requestTrace.stateLock.Unlock()
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxProbeResponseBytes))
	closeErr := response.Body.Close()
	if readErr != nil {
		return requestTrace.wrap(readErr, time.Now())
	}
	if closeErr != nil {
		return requestTrace.wrap(closeErr, time.Now())
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &targetHTTPStatusError{statusCode: response.StatusCode}
	}
	return nil
}

func waitForProbeInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func readCredentials(path string) (credentials, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return credentials{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return credentials{}, errors.New("credentials path must be a regular file, not a symlink")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return credentials{}, errors.New("credentials file must be owner-only (mode 0600)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return credentials{}, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || strings.TrimSpace(lines[0]) == "" || strings.TrimSpace(lines[1]) == "" {
		return credentials{}, errors.New("credentials file must have exactly two non-empty lines")
	}
	return credentials{user: strings.TrimSpace(lines[0]), password: strings.TrimSpace(lines[1])}, nil
}

// WriteResults atomically writes the strict TSV contract consumed by the root
// orchestrator. The file is private because failure details can contain host
// and account-adjacent diagnostics even after explicit secret redaction.
func WriteResults(path string, results []Result) error {
	if path == "" {
		return errors.New("result path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".proxy-results-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	for _, result := range results {
		detail := cleanField(result.Detail)
		if _, err := fmt.Fprintf(temporary, "%s\t%s\t%s\t%s\n", Platform, cleanField(result.Case), cleanField(result.Status), detail); err != nil {
			temporary.Close()
			return err
		}
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Failed reports whether any required protocol failed.
func Failed(results []Result) bool {
	for _, result := range results {
		if result.Status != "PASS" {
			return true
		}
	}
	return false
}

func errorsForEveryProtocol(err error) map[string]error {
	result := map[string]error{}
	for _, name := range protocolNames {
		result[name] = err
	}
	return result
}

func failedResults(detail string) []Result {
	result := make([]Result, 0, len(protocolNames))
	for _, name := range protocolNames {
		result = append(result, Result{Case: name, Status: "FAIL", Detail: cleanField(detail)})
	}
	return result
}

type redactor struct {
	values []string
}

func newRedactor(values ...string) *redactor {
	r := &redactor{}
	for _, value := range values {
		r.add(value)
	}
	return r
}

func (r *redactor) add(value string) {
	if value == "" {
		return
	}
	for _, existing := range r.values {
		if existing == value {
			return
		}
	}
	r.values = append(r.values, value)
	sort.Slice(r.values, func(i, j int) bool { return len(r.values[i]) > len(r.values[j]) })
}

func (r *redactor) addProxyConfig(config *proxyConfigResult) {
	if config == nil {
		return
	}
	r.add(config.AuthToken)
	r.add(config.SocksProxyURL)
	r.add(config.HTTPProxyURL)
	r.add(config.APIBaseURL)
	r.add(config.InstanceID)
	if config.WgConfig != nil {
		r.add(config.WgConfig.ClientPrivateKey)
		r.add(config.WgConfig.Config)
	}
}

func (r *redactor) clean(value string) string {
	for _, secret := range r.values {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	return cleanField(value)
}

func cleanField(value string) string {
	value = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ").Replace(value)
	value = strings.Join(strings.Fields(value), " ")
	const maxDetailBytes = 1000
	if len(value) > maxDetailBytes {
		value = value[:maxDetailBytes] + "..."
	}
	return value
}
