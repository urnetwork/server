package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// Sign in with Microsoft for every app and ur.io (support inbox 7721: phones
// without Google services). Microsoft has no SDK every platform shares, so
// every client signs in through the browser, and no client carries the client
// id: the app opens
//
//	<api>/auth/microsoft/start?state=<state>&nonce=<nonce>
//
// and this server answers a redirect to Microsoft's authorize page with its
// own client (microsoft.yml), the authorization code flow and this server's
// callback as the redirect:
//
//	https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=<client id>
//	    &response_type=code&redirect_uri=<api>/auth/microsoft/callback&response_mode=query
//	    &scope=openid%20email%20profile&state=<state>&nonce=<nonce>&prompt=select_account
//
// Microsoft sends the browser to the callback with `code` and `state`; the
// callback exchanges the code at Microsoft's token endpoint (the client secret
// is the credential) and hands the identity token back to where the attempt
// returns:
//
//	<scheme>://oauth/microsoft?state=<state>&id_token=<jwt>
//	<scheme>://oauth/microsoft?state=<state>&error=<message>
//	<web return url>#state=<state>&id_token=<jwt>        (platform "web")
//
// Nothing is stored. The app checks that `state` is the attempt it started and
// that the identity token carries the nonce it minted (sdk oauth_microsoft.go),
// then signs in with the token through /auth/login or adds it through
// /auth/add-auth, which verify its signature, audience, tenant and email
// (session/microsoft_jwt.go).
//
// `state` is opaque except for its optional `platform` claim, shared with the
// Apple and Google callbacks (oauthSchemeForState). The "web" platform returns
// to ur.io's callback page (sign_in_oauth.web_return_url) with the result in
// the fragment, which never reaches a server.
//
// microsoft.yml (vault):
//
//	# the audiences /auth/login accepts (session/sso_jwt.go)
//	client_id: "<the application (client) id>"
//	sign_in_oauth:
//	  client_id: "<the application (client) id>"
//	  client_secret: "<a client secret value>"
//	  # optional; the callback url as registered with Microsoft when the
//	  # public api origin is not the one in the request. Default:
//	  # https://<request host>/auth/microsoft/callback
//	  redirect_uri: "https://api.bringyour.com/auth/microsoft/callback"
//	  # optional; default https://ur.io/auth/microsoft/callback
//	  web_return_url: "https://ur.io/auth/microsoft/callback"
//
// Without the section both endpoints answer the app with
// `error=not_configured`.

const microsoftOAuthReturnPath = "oauth/microsoft"

const microsoftOAuthCallbackPath = "/auth/microsoft/callback"

const microsoftOAuthAuthorizeUrl = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize"

const microsoftOAuthScope = "openid email profile"

const microsoftOAuthDefaultWebReturnUrl = "https://ur.io/auth/microsoft/callback"

// the state claim of an attempt that returns to ur.io's callback page
const microsoftOAuthWebPlatform = "web"

const microsoftOAuthExchangeTimeout = 10 * time.Second

// microsoftOAuthTokenUrl is Microsoft's token endpoint; tests point it at a
// fake.
var microsoftOAuthTokenUrl = "https://login.microsoftonline.com/common/oauth2/v2.0/token"

// An attempt's state and nonce: the base64url alphabet with '.' (the apps'
// base64url json, ur.io's mode prefix), bounded so a request cannot carry a
// payload through Microsoft.
var microsoftOAuthTokenPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

const (
	microsoftOAuthMaxStateLength = 1024
	microsoftOAuthMinNonceLength = 16
	microsoftOAuthMaxNonceLength = 256
)

func microsoftOAuthValidState(state string) bool {
	return len(state) <= microsoftOAuthMaxStateLength && microsoftOAuthTokenPattern.MatchString(state)
}

func microsoftOAuthValidNonce(nonce string) bool {
	return microsoftOAuthMinNonceLength <= len(nonce) && len(nonce) <= microsoftOAuthMaxNonceLength && microsoftOAuthTokenPattern.MatchString(nonce)
}

// MicrosoftOAuthClient is the sign-in client the start and the code exchange
// use, plus the registered callback url and the web return when pinned.
type MicrosoftOAuthClient struct {
	ClientId     string
	ClientSecret string
	RedirectUri  string
	WebReturnUrl string
}

// microsoftOAuthClientFunc yields the client from the vault; tests replace it.
var microsoftOAuthClientFunc = func() (*MicrosoftOAuthClient, bool) {
	return microsoftOAuthClientFromVault()
}

var microsoftOAuthClientFromVault = sync.OnceValues(func() (*MicrosoftOAuthClient, bool) {
	resource, err := server.Vault.SimpleResource("microsoft.yml")
	if err != nil {
		return nil, false
	}
	c, err := resource.ParseE()
	if err != nil {
		return nil, false
	}
	section, ok := c["sign_in_oauth"].(map[string]any)
	if !ok {
		return nil, false
	}
	str := func(key string) string {
		value, _ := section[key].(string)
		return strings.TrimSpace(value)
	}
	client := &MicrosoftOAuthClient{
		ClientId:     str("client_id"),
		ClientSecret: str("client_secret"),
		RedirectUri:  str("redirect_uri"),
		WebReturnUrl: str("web_return_url"),
	}
	if client.ClientId == "" || client.ClientSecret == "" {
		return nil, false
	}
	return client, true
})

// Where an attempt's result goes: the app's scheme, or ur.io's callback page
// with the result in the fragment for the web platform.
func microsoftOAuthReturnLocation(state string, values url.Values) string {
	if oauthPlatformForState(state) == microsoftOAuthWebPlatform {
		webReturnUrl := microsoftOAuthDefaultWebReturnUrl
		if client, ok := microsoftOAuthClientFunc(); ok && client.WebReturnUrl != "" {
			webReturnUrl = client.WebReturnUrl
		}
		return webReturnUrl + "#" + values.Encode()
	}
	return oauthSchemeForState(state) + "://" + microsoftOAuthReturnPath + "?" + values.Encode()
}

// MicrosoftOAuthStart is the raw handler for GET /auth/microsoft/start.
func MicrosoftOAuthStart(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	params := r.URL.Query()
	state := strings.TrimSpace(params.Get("state"))
	if !microsoftOAuthValidState(state) {
		http.Error(w, "Missing or malformed state.", http.StatusBadRequest)
		return
	}
	nonce := strings.TrimSpace(params.Get("nonce"))
	answer := func(errorMessage string) {
		values := url.Values{}
		values.Set("state", state)
		values.Set("error", errorMessage)
		http.Redirect(w, r, microsoftOAuthReturnLocation(state, values), http.StatusFound)
	}
	if !microsoftOAuthValidNonce(nonce) {
		answer("invalid_request")
		return
	}
	client, ok := microsoftOAuthClientFunc()
	if !ok {
		answer("not_configured")
		return
	}
	query := url.Values{}
	query.Set("client_id", client.ClientId)
	query.Set("response_type", "code")
	query.Set("redirect_uri", microsoftOAuthRedirectUri(r))
	query.Set("response_mode", "query")
	query.Set("scope", microsoftOAuthScope)
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("prompt", "select_account")
	http.Redirect(w, r, microsoftOAuthAuthorizeUrl+"?"+query.Encode(), http.StatusFound)
}

// MicrosoftOAuthCallback is the raw handler for GET /auth/microsoft/callback.
func MicrosoftOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	params := r.URL.Query()
	state := strings.TrimSpace(params.Get("state"))
	if !microsoftOAuthValidState(state) {
		http.Error(w, "Missing or malformed state.", http.StatusBadRequest)
		return
	}

	values := url.Values{}
	values.Set("state", state)
	if errorCode := strings.TrimSpace(params.Get("error")); errorCode != "" {
		// microsoft's own error (access_denied when the person backs out),
		// passed through as the google callback does; its description stays
		// in the browser history only
		values.Set("error", errorCode)
	} else if code := strings.TrimSpace(params.Get("code")); code == "" {
		values.Set("error", "Microsoft did not return an authorization code.")
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), microsoftOAuthExchangeTimeout)
		defer cancel()
		idToken, err := microsoftOAuthExchangeCode(ctx, code, microsoftOAuthRedirectUri(r))
		if err != nil {
			values.Set("error", err.Error())
		} else {
			values.Set("id_token", idToken)
		}
	}
	http.Redirect(w, r, microsoftOAuthReturnLocation(state, values), http.StatusFound)
}

// microsoftOAuthRedirectUri is the callback url the code was issued for: the
// pinned one from the vault, else this request's own origin (the front proxy's
// forwarding headers win over the socket-level ones), as for google.
func microsoftOAuthRedirectUri(r *http.Request) string {
	if client, ok := microsoftOAuthClientFunc(); ok && client.RedirectUri != "" {
		return client.RedirectUri
	}
	scheme := "https"
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwarded != "" {
		scheme = strings.ToLower(strings.Split(forwarded, ",")[0])
	} else if r.TLS == nil && (strings.HasPrefix(r.Host, "localhost") || strings.HasPrefix(r.Host, "127.")) {
		scheme = "http"
	}
	host := strings.TrimSpace(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	host = strings.TrimSpace(strings.Split(host, ",")[0])
	return scheme + "://" + host + microsoftOAuthCallbackPath
}

// microsoftOAuthExchangeCode trades the authorization code for Microsoft's
// tokens and returns the identity token. The error is what the app shows, so
// it names the failure without the client secret or the code.
func microsoftOAuthExchangeCode(ctx context.Context, code string, redirectUri string) (string, error) {
	client, ok := microsoftOAuthClientFunc()
	if !ok {
		return "", errors.New("not_configured")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", client.ClientId)
	form.Set("client_secret", client.ClientSecret)
	form.Set("redirect_uri", redirectUri)
	form.Set("scope", microsoftOAuthScope)

	type tokenResponse struct {
		IdToken string `json:"id_token"`
		Error   string `json:"error"`
	}
	response, err := server.HttpPostForm(
		ctx,
		microsoftOAuthTokenUrl,
		form,
		server.NoCustomHeaders,
		func(httpResponse *http.Response, body []byte) (*tokenResponse, error) {
			parsed := &tokenResponse{}
			// microsoft answers a 400 with a json error for a used or expired
			// code; anything else unparseable is reported by status. Its
			// error_description carries trace ids and is not passed on.
			if jsonErr := json.Unmarshal(body, parsed); jsonErr != nil && httpResponse.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("Microsoft's token endpoint answered %d.", httpResponse.StatusCode)
			} else if jsonErr != nil {
				return nil, errors.New("Microsoft's token endpoint answered with an unreadable body.")
			}
			if parsed.Error != "" {
				return nil, fmt.Errorf("Microsoft rejected the sign-in: %s.", parsed.Error)
			}
			if httpResponse.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("Microsoft's token endpoint answered %d.", httpResponse.StatusCode)
			}
			return parsed, nil
		},
	)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", errors.New("Microsoft's token endpoint did not answer in time.")
		}
		return "", err
	}
	if response.IdToken == "" {
		return "", errors.New("Microsoft did not return an identity token.")
	}
	return response.IdToken, nil
}
