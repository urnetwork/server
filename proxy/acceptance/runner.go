// Package acceptance drives the deployed proxy control plane and each public
// proxy data plane without changing the host's routes or proxy settings.
package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	Platform              = "server/proxy"
	defaultProbeTimeout   = 120 * time.Second
	maxAPIResponseBytes   = 1024 * 1024
	maxProbeResponseBytes = 64 * 1024
)

var protocolNames = []string{"socks", "http", "wireguard"}

// Options identifies one production acceptance campaign.
type Options struct {
	APIURL          string
	TargetURL       string
	CredentialsPath string
	Repeat          int
	ProbeTimeout    time.Duration
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
	ProxyConfigResult *proxyConfigResult `json:"proxy_config_result,omitempty"`
	Error             *apiError          `json:"error,omitempty"`
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

type runDependencies struct {
	credentials *credentials
	httpClient  *http.Client
	probes      protocolProbeFactory
}

type runner struct {
	opts      Options
	api       *apiClient
	creds     credentials
	redactor  *redactor
	newProbes protocolProbeFactory
}

// Run executes all three protocols for every requested repetition. It always
// returns exactly one result per supported protocol so orchestration can
// distinguish a failed check from a runner that did not report it.
func Run(ctx context.Context, opts Options) []Result {
	return runWithDependencies(ctx, opts, runDependencies{})
}

func runWithDependencies(ctx context.Context, opts Options, deps runDependencies) []Result {
	if err := validateOptions(opts); err != nil {
		return failedResults(err.Error())
	}
	if opts.ProbeTimeout == 0 {
		opts.ProbeTimeout = defaultProbeTimeout
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
		opts:      opts,
		api:       &apiClient{baseURL: strings.TrimRight(opts.APIURL, "/"), client: httpClient},
		creds:     creds,
		redactor:  newRedactor(creds.user, creds.password),
		newProbes: deps.probes,
	}
	if r.newProbes == nil {
		r.newProbes = r.productionProbes
	}

	passes := map[string]int{}
	failures := map[string][]string{}
	for repetition := 1; repetition <= opts.Repeat; repetition++ {
		iteration := r.runIteration(ctx)
		for _, name := range protocolNames {
			if err := iteration[name]; err != nil {
				failures[name] = append(failures[name], fmt.Sprintf("repetition %d: %v", repetition, err))
			} else {
				passes[name]++
			}
		}
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
			results = append(results, Result{
				Case:   name,
				Status: "PASS",
				Detail: fmt.Sprintf("%d/%d HTTPS requests succeeded through %s", passes[name], opts.Repeat, labels[name]),
			})
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
	if provisioned.ProxyConfigResult != nil {
		r.redactor.addProxyConfig(provisioned.ProxyConfigResult)
	}

	if provisionErr != nil {
		result = errorsForEveryProtocol(fmt.Errorf("provision proxy client: %w", provisionErr))
	} else if provisioned.ProxyConfigResult == nil {
		result = errorsForEveryProtocol(errors.New("provision proxy client returned no proxy configuration"))
	} else if ctx.Err() != nil {
		result = errorsForEveryProtocol(ctx.Err())
	} else {
		probes := r.newProbes(provisioned.ProxyConfigResult)
		// HTTP runs first because it also causes the hosted proxy device to open;
		// the SOCKS and WireGuard checks then exercise the same ready resident.
		for _, name := range []string{"http", "socks", "wireguard"} {
			probe := probes[name]
			if probe == nil {
				result[name] = errors.New("runner did not configure this protocol")
				continue
			}
			result[name] = probe(ctx)
		}
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
			return probeHTTPS(ctx, "HTTP CONNECT", r.opts.TargetURL, transport, r.opts.ProbeTimeout)
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
			return probeHTTPS(ctx, "SOCKS5", r.opts.TargetURL, transport, r.opts.ProbeTimeout)
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
			return probeHTTPS(ctx, "WireGuard", r.opts.TargetURL, transport, r.opts.ProbeTimeout)
		},
	}
}

func probeHTTPS(ctx context.Context, protocol, target string, transport http.RoundTripper, retryWindow time.Duration) error {
	probeCtx, cancel := context.WithTimeout(ctx, retryWindow)
	defer cancel()
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	var lastErr error
	for {
		request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "text/plain, */*")
		request.Header.Set("User-Agent", "urnetwork-proxy-acceptance/1")
		request.Close = true
		response, err := client.Do(request)
		if err == nil {
			_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxProbeResponseBytes))
			closeErr := response.Body.Close()
			switch {
			case readErr != nil:
				lastErr = readErr
			case closeErr != nil:
				lastErr = closeErr
			case response.StatusCode < 200 || response.StatusCode >= 300:
				lastErr = fmt.Errorf("target returned HTTP %d", response.StatusCode)
			default:
				return nil
			}
		} else {
			lastErr = err
		}
		if probeCtx.Err() != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s path did not reach the HTTPS target within %s: %w", protocol, retryWindow, lastErr)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-probeCtx.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%s path did not reach the HTTPS target within %s: %w", protocol, retryWindow, lastErr)
		case <-timer.C:
		}
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
