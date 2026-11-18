package server

// Google Postmaster Tools' daily spam rate, one of the measures §21.5's breakers read (ABUSE-M9-03; D-ABUSE-2,
// R-ABUSE-4, I-13): of the API only v1's domains.trafficStats.get, for one domain and one UTC day, and of its answer
// only userReportedSpamRatio.
//
// The answer is a rate or unknown, never zero for want of an answer. Unknown is every case the API gave no ratio
// for the day asked: no token source, a token the source could not give, an unreachable API, any status but 200
// (a day not reported yet answers 404, and Postmaster Tools reports a day late), an answer past the size bound or not
// JSON, one that names another day, and one with no userReportedSpamRatio. The last is deliberate: the API's JSON
// drops a field that is zero, so a day with a ratio of exactly zero reads unknown here, which holds a breaker where it
// is rather than counting a day toward clearing on a field that was not sent.
//
// The domain is the platform's own <root>, in the path. The token rides the Authorization header, and no error or
// log line here carries it, the url or the answer (I-13). The credential is external: a deployment gives a token
// source, and with none the client answers unknown. Safe for concurrent use.
//
// The deployment's token source is NewPostmasterToolsTokenSource over vault's abuse-postmaster-tools.yml
// (QD-1069): an OAuth 2 client and the refresh token of the account Postmaster Tools verified <root> for, traded at
// the token endpoint for an access token kept until shortly before it expires.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

// The API's names: its path's parts and its day form.
const (
	postmasterToolsDomainsPath      = "/v1/domains/"
	postmasterToolsTrafficStatsPart = "/trafficStats/"
	postmasterToolsDayLayout        = "20060102"
)

// The API gave no usable answer for the day: see the file header.
var ErrPostmasterToolsSource = errors.New("postmaster tools: the source gave no usable answer")

// Tunables of the client and its token source.
type PostmasterToolsClientSettings struct {
	// The API's origin: https, with no path.
	BaseUrl string
	// One request's deadline.
	RequestTimeout time.Duration
	// The most bytes one answer is read to.
	ResponseMaxByteCount int64
	// The OAuth 2 token endpoint the refresh token is traded at: https.
	TokenUrl string
	// How long before an access token's expiry the source trades for the next.
	TokenExpiryMargin time.Duration
}

func DefaultPostmasterToolsClientSettings() *PostmasterToolsClientSettings {
	return &PostmasterToolsClientSettings{
		BaseUrl:              "https://gmailpostmastertools.googleapis.com",
		RequestTimeout:       30 * time.Second,
		ResponseMaxByteCount: 1024 * 1024,
		TokenUrl:             "https://oauth2.googleapis.com/token",
		TokenExpiryMargin:    time.Minute,
	}
}

// The vault file holding the deployment's credential (QD-1069).
const PostmasterToolsCredentialFileName = "abuse-postmaster-tools.yml"

// abuse-postmaster-tools.yml: an OAuth 2 client and the refresh token of the account Postmaster Tools verified
// <root> for, granted the API's read-only scope. Every field is a credential.
type PostmasterToolsCredential struct {
	ClientId     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	RefreshToken string `yaml:"refresh_token"`
}

// The credential is not one a token source can use. The text names the part, never a value.
var ErrPostmasterToolsCredential = errors.New("postmaster tools: the credential is not usable")

// Reads abuse-postmaster-tools.yml. A file that does not parse or lacks a field is refused.
func ParsePostmasterToolsCredential(data []byte) (*PostmasterToolsCredential, error) {
	credential := &PostmasterToolsCredential{}
	// a decode error can quote the line it failed on, so only the verdict is kept
	if err := yaml.Unmarshal(data, credential); err != nil {
		return nil, fmt.Errorf("%w: the file does not parse", ErrPostmasterToolsCredential)
	}
	credential.ClientId = strings.TrimSpace(credential.ClientId)
	credential.ClientSecret = strings.TrimSpace(credential.ClientSecret)
	credential.RefreshToken = strings.TrimSpace(credential.RefreshToken)
	if credential.ClientId == "" || credential.ClientSecret == "" || credential.RefreshToken == "" {
		return nil, fmt.Errorf("%w: client_id, client_secret and refresh_token are each needed", ErrPostmasterToolsCredential)
	}
	return credential, nil
}

// The token endpoint's answer, the part the source reads.
type postmasterToolsTokenAnswer struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

// A token source over credential: it trades the refresh token for an access token at settings' TokenUrl, keeps it
// until TokenExpiryMargin before it expires, and trades again after. A trade that fails answers an error, never a
// stale token, and the next call trades again. transport nil dials directly; now nil reads the wall clock. No error
// carries the credential, the token or the answer. Safe for concurrent use.
func NewPostmasterToolsTokenSource(
	credential *PostmasterToolsCredential,
	transport http.RoundTripper,
	settings *PostmasterToolsClientSettings,
	now func() time.Time,
) PostmasterToolsTokenSource {
	if settings == nil {
		settings = DefaultPostmasterToolsClientSettings()
	}
	if transport == nil {
		transport = &http.Transport{Proxy: nil}
	}
	if now == nil {
		now = time.Now
	}
	httpClient := &http.Client{
		Transport: transport,
		// a redirect would carry the credential elsewhere
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	var stateLock sync.Mutex
	var token string
	var refreshTime time.Time
	return func(ctx context.Context) (string, error) {
		stateLock.Lock()
		defer stateLock.Unlock()
		if token != "" && now().Before(refreshTime) {
			return token, nil
		}
		token = ""
		if credential == nil {
			return "", fmt.Errorf("%w: none", ErrPostmasterToolsCredential)
		}
		tokenUrl, err := url.Parse(settings.TokenUrl)
		if err != nil || tokenUrl.Scheme != "https" || tokenUrl.Host == "" {
			return "", fmt.Errorf("%w: no usable token endpoint", ErrPostmasterToolsSource)
		}
		form := url.Values{}
		form.Set("grant_type", "refresh_token")
		form.Set("client_id", credential.ClientId)
		form.Set("client_secret", credential.ClientSecret)
		form.Set("refresh_token", credential.RefreshToken)
		requestCtx, cancel := context.WithTimeout(ctx, settings.RequestTimeout)
		defer cancel()
		request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, tokenUrl.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return "", fmt.Errorf("%w: no token request", ErrPostmasterToolsSource)
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Accept", "application/json")
		requestTime := now()
		response, err := httpClient.Do(request)
		if err != nil {
			return "", fmt.Errorf("%w: the token endpoint is unreachable", ErrPostmasterToolsSource)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return "", fmt.Errorf("%w: the token endpoint's status %d", ErrPostmasterToolsSource, response.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, settings.ResponseMaxByteCount+1))
		if err != nil || settings.ResponseMaxByteCount < int64(len(body)) {
			return "", fmt.Errorf("%w: a token answer past its bound", ErrPostmasterToolsSource)
		}
		answer := &postmasterToolsTokenAnswer{}
		if err := json.Unmarshal(body, answer); err != nil || answer.AccessToken == "" || answer.ExpiresIn <= 0 ||
			!strings.EqualFold(answer.TokenType, "bearer") {
			return "", fmt.Errorf("%w: a token answer that is not the endpoint's", ErrPostmasterToolsSource)
		}
		token = answer.AccessToken
		refreshTime = requestTime.Add(time.Duration(answer.ExpiresIn)*time.Second - settings.TokenExpiryMargin)
		return token, nil
	}
}

// Gives an OAuth 2 access token with the API's read-only scope, as the deployment's credential allows.
type PostmasterToolsTokenSource func(ctx context.Context) (token string, err error)

// What NewPostmasterToolsClient takes.
type PostmasterToolsClientArgs struct {
	// nil answers every day unknown.
	TokenSource PostmasterToolsTokenSource
	// How the client reaches the API. nil dials directly, with a transport of the client's own.
	Transport http.RoundTripper
	// nil takes the defaults.
	Settings *PostmasterToolsClientSettings
}

// See the file header.
type PostmasterToolsClient struct {
	tokenSource PostmasterToolsTokenSource
	httpClient  *http.Client
	settings    *PostmasterToolsClientSettings
}

func NewPostmasterToolsClient(args *PostmasterToolsClientArgs) *PostmasterToolsClient {
	settings := args.Settings
	if settings == nil {
		settings = DefaultPostmasterToolsClientSettings()
	}
	transport := args.Transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil}
	}
	return &PostmasterToolsClient{
		tokenSource: args.TokenSource,
		httpClient: &http.Client{
			Transport: transport,
			// the API answers in place; a redirect is not followed with the token
			CheckRedirect: func(request *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		settings: settings,
	}
}

// The part of trafficStats this client reads.
type postmasterToolsTrafficStats struct {
	Name                  string   `json:"name"`
	UserReportedSpamRatio *float64 `json:"userReportedSpamRatio"`
}

// The domain's user-reported spam rate for the UTC day of day, in parts per million of the mail delivered to the
// inbox, and whether it is known (see the file header).
func (self *PostmasterToolsClient) SpamRate(ctx context.Context, domain string, day time.Time) (partsPerMillion int64, known bool) {
	partsPerMillion, err := self.spamRate(ctx, domain, day)
	if err != nil {
		return 0, false
	}
	return partsPerMillion, true
}

func (self *PostmasterToolsClient) spamRate(ctx context.Context, domain string, day time.Time) (int64, error) {
	if self.tokenSource == nil {
		return 0, fmt.Errorf("%w: no credential", ErrPostmasterToolsSource)
	}
	domain = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(domain), "."))
	if domain == "" || strings.ContainsAny(domain, "/?#% ") {
		return 0, fmt.Errorf("%w: no domain", ErrPostmasterToolsSource)
	}
	base, err := url.Parse(self.settings.BaseUrl)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return 0, fmt.Errorf("%w: no usable origin", ErrPostmasterToolsSource)
	}
	dayText := day.UTC().Format(postmasterToolsDayLayout)
	resourceName := "domains/" + domain + "/trafficStats/" + dayText
	requestUrl := base.Scheme + "://" + base.Host + postmasterToolsDomainsPath + url.PathEscape(domain) + postmasterToolsTrafficStatsPart + dayText

	requestCtx, cancel := context.WithTimeout(ctx, self.settings.RequestTimeout)
	defer cancel()
	token, err := self.tokenSource(requestCtx)
	if err != nil || token == "" {
		return 0, fmt.Errorf("%w: no token", ErrPostmasterToolsSource)
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, requestUrl, nil)
	if err != nil {
		return 0, fmt.Errorf("%w: no request", ErrPostmasterToolsSource)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	response, err := self.httpClient.Do(request)
	if err != nil {
		// the transport's error names the url
		return 0, fmt.Errorf("%w: unreachable", ErrPostmasterToolsSource)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%w: status %d", ErrPostmasterToolsSource, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, self.settings.ResponseMaxByteCount+1))
	if err != nil || self.settings.ResponseMaxByteCount < int64(len(body)) {
		return 0, fmt.Errorf("%w: an answer past its bound", ErrPostmasterToolsSource)
	}
	stats := &postmasterToolsTrafficStats{}
	if err := json.Unmarshal(body, stats); err != nil {
		return 0, fmt.Errorf("%w: an answer that is not the API's", ErrPostmasterToolsSource)
	}
	if stats.Name != resourceName {
		return 0, fmt.Errorf("%w: an answer for another day", ErrPostmasterToolsSource)
	}
	if stats.UserReportedSpamRatio == nil {
		return 0, fmt.Errorf("%w: no ratio for the day", ErrPostmasterToolsSource)
	}
	ratio := *stats.UserReportedSpamRatio
	if math.IsNaN(ratio) || ratio < 0 || 1 < ratio {
		return 0, fmt.Errorf("%w: a ratio out of range", ErrPostmasterToolsSource)
	}
	return int64(math.Round(ratio * 1000 * 1000)), nil
}
