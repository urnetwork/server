package server

// certs' client of §12.8's internal dns routes (TLS-M5-16; D-DNS-8, D-TLS-7;
// plan-TLS-internal-credentials): certsservice.CertsDns over HTTP, the only
// way certs writes DNS-01 in URnetwork zones.
//
//	POST /dns/internal/acme-challenge   the TXT values at a name, or none to remove them
//	GET  /dns/internal/ready?serial=s   whether every running NS serves s and the canary is clean
//
// Each request carries the internal-certs bearer, 256 bits from taskworker's
// vault (internal-certs.yml), which the dns api verifies by hash. A client
// with no bearer is not built: certs then runs on certsservice.ClosedCertsDns
// and reaches no zone. The bearer goes only to the configured origin: over
// https, or http on loopback for the local stack, and never along a redirect.
//
// A 503 is the zone restoring, which the solver waits out
// (certsservice.ErrCertsDnsRestoring); a 400 or 429 is a write the path
// refused (ErrCertsDnsWriteRefused); anything else is an error whose text
// names the status and never the bearer. Safe for concurrent use.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
)

var _ certsservice.CertsDns = (*CertsDnsClient)(nil)

// The credential file of the certs caller class in taskworker's vault.
const CertsDnsCredentialFileName = "internal-certs.yml"

var (
	// No bearer, or settings no client can run on. The text names the part.
	ErrCertsDnsClientRefused = errors.New("certs dns client refused")
	// The dns api refused the bearer: 401 or 403.
	ErrCertsDnsCredentialRefused = errors.New("dns refused the certs credential")
)

// Tunables of the client.
type CertsDnsClientSettings struct {
	// The dns api's origin, scheme and host with no path: certs.yml's dns_url.
	// No default: a production name, which the host sets.
	BaseUrl string
	// The root the names sit under, sent with each call. Empty leaves it out,
	// which the api takes with one root.
	Root string
	// How long a challenge write may take.
	RequestTimeout time.Duration
	// How long a ready read may take: past the route's own hold, which stays
	// under the lb's 30 s.
	ReadyTimeout time.Duration
	// The most bytes read of an answer.
	MaxResponseByteCount int64
}

func DefaultCertsDnsClientSettings() *CertsDnsClientSettings {
	return &CertsDnsClientSettings{
		RequestTimeout:       15 * time.Second,
		ReadyTimeout:         40 * time.Second,
		MaxResponseByteCount: 64 * 1024,
	}
}

// See the file header.
type CertsDnsClient struct {
	settings   *CertsDnsClientSettings
	baseUrl    *url.URL
	bearer     string
	httpClient *http.Client
}

// The certs bearer from this process's vault. An error names the file and
// the part, never the bearer.
func LoadCertsDnsCredential() (string, error) {
	resource, err := Vault.SimpleResource(CertsDnsCredentialFileName)
	if err != nil {
		return "", fmt.Errorf("%w: no %s: %v", ErrCertsDnsClientRefused, CertsDnsCredentialFileName, err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", ErrCertsDnsClientRefused, CertsDnsCredentialFileName, err)
	}
	bearer, err := dnsservice.LoadDnsInternalCredential(data)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %w", ErrCertsDnsClientRefused, CertsDnsCredentialFileName, err)
	}
	return bearer, nil
}

// A client on settings with bearer. No bearer, no origin, an origin with a
// path or credentials of its own, and cleartext to anything but loopback are
// refused, so a client that exists can only send its bearer where it should
// go. transport nil takes http.DefaultTransport.
func NewCertsDnsClient(settings *CertsDnsClientSettings, bearer string, transport http.RoundTripper) (*CertsDnsClient, error) {
	if settings == nil {
		return nil, fmt.Errorf("%w: no settings", ErrCertsDnsClientRefused)
	}
	if bearer == "" {
		return nil, fmt.Errorf("%w: no credential", ErrCertsDnsClientRefused)
	}
	if settings.RequestTimeout <= 0 || settings.ReadyTimeout <= 0 || settings.MaxResponseByteCount <= 0 {
		return nil, fmt.Errorf("%w: a timeout or the response bound is not positive", ErrCertsDnsClientRefused)
	}
	baseUrl, err := url.Parse(settings.BaseUrl)
	if err != nil || baseUrl.Host == "" || baseUrl.User != nil || baseUrl.RawQuery != "" || baseUrl.Fragment != "" ||
		(baseUrl.Path != "" && baseUrl.Path != "/") {
		return nil, fmt.Errorf("%w: BaseUrl is not an origin", ErrCertsDnsClientRefused)
	}
	switch baseUrl.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(baseUrl.Hostname())
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("%w: BaseUrl is cleartext off loopback", ErrCertsDnsClientRefused)
		}
	default:
		return nil, fmt.Errorf("%w: BaseUrl is not http or https", ErrCertsDnsClientRefused)
	}
	if transport == nil {
		transport = http.DefaultTransport
	}
	return &CertsDnsClient{
		settings: settings,
		baseUrl:  &url.URL{Scheme: baseUrl.Scheme, Host: baseUrl.Host},
		bearer:   bearer,
		httpClient: &http.Client{
			Transport: transport,
			// the bearer follows no redirect
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// A client on settings with the bearer from this process's vault.
func NewCertsDnsClientFromVault(settings *CertsDnsClientSettings) (*CertsDnsClient, error) {
	bearer, err := LoadCertsDnsCredential()
	if err != nil {
		return nil, err
	}
	return NewCertsDnsClient(settings, bearer, nil)
}

// One call: the answer's body for a 200, and the route's refusal otherwise.
func (self *CertsDnsClient) call(ctx context.Context, timeout time.Duration, method string, path string, query url.Values, body []byte) ([]byte, error) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	callUrl := *self.baseUrl
	callUrl.Path = path
	if self.settings.Root != "" {
		if query == nil {
			query = url.Values{}
		}
		query.Set("root", self.settings.Root)
	}
	callUrl.RawQuery = query.Encode()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(callCtx, method, callUrl.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+self.bearer)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := self.httpClient.Do(request)
	if err != nil {
		// a transport error's text holds the url, which holds no secret
		return nil, fmt.Errorf("dns %s: %w", path, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaxResponseByteCount+1))
	if err != nil {
		return nil, fmt.Errorf("dns %s: %w", path, err)
	}
	switch {
	case response.StatusCode == http.StatusOK:
		if self.settings.MaxResponseByteCount < int64(len(answer)) {
			return nil, fmt.Errorf("dns %s: an answer past %d bytes", path, self.settings.MaxResponseByteCount)
		}
		return answer, nil
	case response.StatusCode == http.StatusServiceUnavailable:
		return nil, certsservice.ErrCertsDnsRestoring
	case response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w: status %d", certsservice.ErrCertsDnsWriteRefused, response.StatusCode)
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return nil, fmt.Errorf("%w: status %d", ErrCertsDnsCredentialRefused, response.StatusCode)
	}
	return nil, fmt.Errorf("dns %s: status %d", path, response.StatusCode)
}

// Writes values at name's challenge, or removes it with none, and answers
// the serial the write took.
func (self *CertsDnsClient) putChallenge(ctx context.Context, name string, values []string) (uint64, error) {
	body, err := json.Marshal(&dnsservice.DnsChallengeRequest{Root: self.settings.Root, Name: name, Values: values})
	if err != nil {
		return 0, err
	}
	answerBytes, err := self.call(ctx, self.settings.RequestTimeout, http.MethodPost, dnsservice.DnsAcmeChallengePath, nil, body)
	if err != nil {
		return 0, err
	}
	answer := &dnsservice.DnsChallengeAnswer{}
	if err := json.Unmarshal(answerBytes, answer); err != nil {
		return 0, fmt.Errorf("dns %s: an answer that does not parse", dnsservice.DnsAcmeChallengePath)
	}
	return answer.Serial, nil
}

func (self *CertsDnsClient) WriteAcmeChallenge(ctx context.Context, name string, values []string) (uint64, error) {
	if len(values) == 0 {
		// none would remove the challenge
		return 0, fmt.Errorf("%w: no values", certsservice.ErrCertsDnsWriteRefused)
	}
	serial, err := self.putChallenge(ctx, name, values)
	if err != nil {
		return 0, err
	}
	if serial == 0 {
		return 0, fmt.Errorf("dns %s: a write answered no serial", dnsservice.DnsAcmeChallengePath)
	}
	return serial, nil
}

func (self *CertsDnsClient) RemoveAcmeChallenge(ctx context.Context, name string) error {
	_, err := self.putChallenge(ctx, name, nil)
	return err
}

func (self *CertsDnsClient) Ready(ctx context.Context, serial uint64) (certsservice.CertsDnsReadiness, error) {
	query := url.Values{"serial": {strconv.FormatUint(serial, 10)}}
	answerBytes, err := self.call(ctx, self.settings.ReadyTimeout, http.MethodGet, dnsservice.DnsReadyPath, query, nil)
	if err != nil {
		return "", err
	}
	answer := &dnsservice.DnsReadyAnswer{}
	if err := json.Unmarshal(answerBytes, answer); err != nil {
		return "", fmt.Errorf("dns %s: an answer that does not parse", dnsservice.DnsReadyPath)
	}
	switch readiness := certsservice.CertsDnsReadiness(answer.Answer); readiness {
	case certsservice.CertsDnsStale, certsservice.CertsDnsReady, certsservice.CertsDnsCanaryFailing, certsservice.CertsDnsWaiting:
		return readiness, nil
	}
	// an answer this release does not know is never read as ready
	return "", fmt.Errorf("dns %s: an answer this client does not know", dnsservice.DnsReadyPath)
}

// Whether §12.6's canary is clean at serial: Ready answers ready. Anything
// else, an error included, is not clean (plan-TLS-canary-signal).
func (self *CertsDnsClient) CanaryClean(ctx context.Context, serial uint64) (bool, error) {
	readiness, err := self.Ready(ctx, serial)
	if err != nil {
		return false, err
	}
	return readiness == certsservice.CertsDnsReady, nil
}
