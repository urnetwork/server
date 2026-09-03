package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/server"
)

// Sign in with Google for the apps that have no native Google flow (windows,
// linux). Android signs in with play services and ur.io runs Google Identity
// Services in the page; the desktop apps used to borrow the ur.io page through
// the /sso bridge. Now they open Google's authorize page in the system browser
// with this endpoint as the redirect, the same shape as Sign in with Apple:
//
//	https://accounts.google.com/o/oauth2/v2/auth?client_id=<web client id>
//	    &redirect_uri=<api>/auth/google/callback&response_type=code
//	    &scope=openid%20email%20profile&state=<state>&nonce=<nonce>
//	    &prompt=select_account
//
// Google does not hand an identity token to a server redirect (no form_post),
// so the app asks for an authorization code. Google sends the browser here
// with `code` and `state`; this endpoint exchanges the code for the tokens at
// Google's token endpoint (the web client's secret is the credential) and hands
// the identity token straight back to the app through the app's own scheme:
//
//	<scheme>://oauth/google?state=<state>&id_token=<jwt>
//	<scheme>://oauth/google?state=<state>&error=<message>
//
// Nothing is stored. The app checks that `state` is the attempt it started and
// that the identity token carries the nonce it minted, then signs in with the
// token through /auth/login, which verifies the signature and the audience
// (the web client id is in google.yml `client_id`).
//
// `state` is opaque except for the optional `platform` claim shared with the
// Apple callback (oauthSchemeForState): windows and linux redirect to
// `urnetwork://`, android to `ur://`, and the android scheme is the default.
//
// google.yml (vault):
//
//	sign_in_oauth:
//	  client_id: "<the ur.io web sign-in client id>"
//	  client_secret: "<that client's secret>"
//	  # optional; the callback url as registered with Google when the public
//	  # api origin is not the one in the request (a proxy without forwarding
//	  # headers). Default: https://<request host>/auth/google/callback
//	  redirect_uri: "https://api.bringyour.com/auth/google/callback"
//
// Without the section the endpoint answers the app with `error=not_configured`.

const googleOAuthReturnPath = "oauth/google"

const googleOAuthCallbackPath = "/auth/google/callback"

const googleOAuthExchangeTimeout = 10 * time.Second

// googleOAuthTokenUrl is Google's token endpoint; tests point it at a fake.
var googleOAuthTokenUrl = "https://oauth2.googleapis.com/token"

// GoogleOAuthClient is the web sign-in client the code exchange authenticates
// with, plus the registered callback url when it must be pinned.
type GoogleOAuthClient struct {
	ClientId     string
	ClientSecret string
	RedirectUri  string
}

// googleOAuthClientFunc yields the client from the vault; tests replace it.
var googleOAuthClientFunc = func() (*GoogleOAuthClient, bool) {
	return googleOAuthClientFromVault()
}

var googleOAuthClientFromVault = sync.OnceValues(func() (*GoogleOAuthClient, bool) {
	c := server.Vault.RequireSimpleResource("google.yml").Parse()
	section, ok := c["sign_in_oauth"].(map[string]any)
	if !ok {
		return nil, false
	}
	str := func(key string) string {
		value, _ := section[key].(string)
		return strings.TrimSpace(value)
	}
	client := &GoogleOAuthClient{
		ClientId:     str("client_id"),
		ClientSecret: str("client_secret"),
		RedirectUri:  str("redirect_uri"),
	}
	if client.ClientId == "" || client.ClientSecret == "" {
		return nil, false
	}
	return client, true
})

// GoogleOAuthCallback is the raw handler for GET /auth/google/callback.
func GoogleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}
	params := r.URL.Query()
	state := strings.TrimSpace(params.Get("state"))
	if state == "" {
		http.Error(w, "Missing state.", http.StatusBadRequest)
		return
	}

	values := url.Values{}
	values.Set("state", state)
	if errorMessage := strings.TrimSpace(params.Get("error")); errorMessage != "" {
		// google's own error (access_denied when the user backs out), passed
		// through untouched like the apple callback does
		values.Set("error", errorMessage)
	} else if code := strings.TrimSpace(params.Get("code")); code == "" {
		values.Set("error", "Google did not return an authorization code.")
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), googleOAuthExchangeTimeout)
		defer cancel()
		idToken, err := googleOAuthExchangeCode(ctx, code, googleOAuthRedirectUri(r))
		if err != nil {
			values.Set("error", err.Error())
		} else {
			values.Set("id_token", idToken)
		}
	}

	location := oauthSchemeForState(state) + "://" + googleOAuthReturnPath + "?" + values.Encode()
	http.Redirect(w, r, location, http.StatusFound)
}

// googleOAuthRedirectUri is the callback url the app registered the code
// against: the pinned one from the vault, else this request's own origin (the
// forwarding headers of the front proxy win over the socket-level ones).
func googleOAuthRedirectUri(r *http.Request) string {
	if client, ok := googleOAuthClientFunc(); ok && client.RedirectUri != "" {
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
	return scheme + "://" + host + googleOAuthCallbackPath
}

// googleOAuthExchangeCode trades the authorization code for Google's tokens
// and returns the identity token. The error is what the app shows the user, so
// it names the failure without leaking the client secret or the code.
func googleOAuthExchangeCode(ctx context.Context, code string, redirectUri string) (string, error) {
	client, ok := googleOAuthClientFunc()
	if !ok {
		return "", errors.New("not_configured")
	}

	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", client.ClientId)
	form.Set("client_secret", client.ClientSecret)
	form.Set("redirect_uri", redirectUri)

	type tokenResponse struct {
		IdToken          string `json:"id_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	response, err := server.HttpPostForm(
		ctx,
		googleOAuthTokenUrl,
		form,
		server.NoCustomHeaders,
		func(httpResponse *http.Response, body []byte) (*tokenResponse, error) {
			parsed := &tokenResponse{}
			// google answers a 400 with a json error for a used or expired
			// code; anything else unparseable is reported by status
			if jsonErr := json.Unmarshal(body, parsed); jsonErr != nil && httpResponse.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("Google's token endpoint answered %d.", httpResponse.StatusCode)
			} else if jsonErr != nil {
				return nil, errors.New("Google's token endpoint answered with an unreadable body.")
			}
			if parsed.Error != "" {
				if parsed.ErrorDescription != "" {
					return nil, fmt.Errorf("Google rejected the sign-in: %s (%s).", parsed.Error, parsed.ErrorDescription)
				}
				return nil, fmt.Errorf("Google rejected the sign-in: %s.", parsed.Error)
			}
			if httpResponse.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("Google's token endpoint answered %d.", httpResponse.StatusCode)
			}
			return parsed, nil
		},
	)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", errors.New("Google's token endpoint did not answer in time.")
		}
		return "", err
	}
	if response.IdToken == "" {
		return "", errors.New("Google did not return an identity token.")
	}
	return response.IdToken, nil
}
