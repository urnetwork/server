package controller

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

// Sign in with Apple for the apps that have no native Apple flow (android,
// windows, linux). Apple has no SDK for those platforms, so the app opens
// Apple's authorize page in the system browser (a Custom Tab on android):
//
//	https://appleid.apple.com/auth/authorize?client_id=<services id>
//	    &redirect_uri=<api>/auth/apple/callback&response_type=code%20id_token
//	    &response_mode=form_post&scope=name%20email&state=<state>&nonce=<nonce>
//
// Apple POSTs the result to `redirect_uri` as a form, and this endpoint hands
// it straight back to the app through the app's own scheme:
//
//	<scheme>://oauth/apple?state=<state>&id_token=<jwt>&code=<code>&user=<json>
//	<scheme>://oauth/apple?state=<state>&error=<message>
//
// Nothing is stored or verified here beyond the shape: the app checks that
// `state` is the attempt it started and that the identity token carries the
// nonce it minted, then signs in with the token through /auth/login, which
// verifies the signature and the audience (the Apple Services ID).
//
// `state` is opaque to Apple and to this endpoint, except for one optional
// claim: when it is the base64url encoding of a JSON object with a `platform`
// key, that key picks the redirect scheme. Without it the android scheme is
// used.
//
//	android (default)  ur://oauth/apple
//	windows, linux     urnetwork://oauth/apple

const appleOAuthCallbackMaxBodyBytes = 64 * 1024

const appleOAuthReturnPath = "oauth/apple"

// appleOAuthSchemes is the redirect scheme per platform claim.
var appleOAuthSchemes = map[string]string{
	"android": "ur",
	"windows": "urnetwork",
	"linux":   "urnetwork",
}

const appleOAuthDefaultScheme = "ur"

// AppleOAuthCallback is the raw handler for POST and GET /auth/apple/callback.
func AppleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	var params url.Values
	switch r.Method {
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, appleOAuthCallbackMaxBodyBytes)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad request.", http.StatusBadRequest)
			return
		}
		params = r.PostForm
	case http.MethodGet:
		params = r.URL.Query()
	default:
		http.Error(w, "Method not allowed.", http.StatusMethodNotAllowed)
		return
	}

	location, ok := AppleOAuthReturnLocation(params)
	if !ok {
		http.Error(w, "Missing state.", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, location, http.StatusFound)
}

// AppleOAuthReturnLocation builds the app redirect for Apple's callback
// parameters. False when there is no state to hand back.
func AppleOAuthReturnLocation(params url.Values) (string, bool) {
	state := strings.TrimSpace(params.Get("state"))
	if state == "" {
		return "", false
	}
	scheme := appleOAuthSchemeForState(state)

	values := url.Values{}
	values.Set("state", state)
	if errorMessage := strings.TrimSpace(params.Get("error")); errorMessage != "" {
		values.Set("error", errorMessage)
	} else {
		if idToken := strings.TrimSpace(params.Get("id_token")); idToken != "" {
			values.Set("id_token", idToken)
		}
		if code := strings.TrimSpace(params.Get("code")); code != "" {
			values.Set("code", code)
		}
		// the name and email Apple sends on the first authorization only
		if user := strings.TrimSpace(params.Get("user")); user != "" {
			values.Set("user", user)
		}
		if !values.Has("id_token") {
			values.Set("error", "Apple did not return an identity token.")
		}
	}
	return scheme + "://" + appleOAuthReturnPath + "?" + values.Encode(), true
}

// appleOAuthSchemeForState reads the optional `platform` claim of the state.
func appleOAuthSchemeForState(state string) string {
	platform := appleOAuthPlatformForState(state)
	if scheme, ok := appleOAuthSchemes[platform]; ok {
		return scheme
	}
	return appleOAuthDefaultScheme
}

func appleOAuthPlatformForState(state string) string {
	var payload []byte
	for _, encoding := range []*base64.Encoding{
		base64.RawURLEncoding,
		base64.URLEncoding,
		base64.RawStdEncoding,
		base64.StdEncoding,
	} {
		decoded, err := encoding.DecodeString(state)
		if err == nil {
			payload = decoded
			break
		}
	}
	if payload == nil {
		return ""
	}
	var claims struct {
		Platform string `json:"platform"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(claims.Platform))
}
