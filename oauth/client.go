package oauth

// Client registration: client id metadata documents, dynamic registration, and
// pre-registered clients.
//
// CIMD is the mechanism the spec prefers and the one that makes a public
// authorization server workable: the client id IS an https url, and the
// metadata is fetched from it. That means this server fetches an arbitrary
// caller-supplied url, so the fetch is hardened the same way the mcp fetch tool
// is -- private, loopback, and link-local targets refused, redirects and body
// size bounded, and the result cached so a client id is not refetched on every
// authorization.
//
// Dynamic registration is supported for backwards compatibility and is
// deprecated by the spec.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

var (
	ErrUnknownClient     = errors.New("unknown client")
	ErrInvalidRedirect   = errors.New("invalid redirect_uri")
	ErrInvalidClientMeta = errors.New("invalid client metadata")
)

const (
	ClientTypeCimd          = "cimd"
	ClientTypeDynamic       = "dynamic"
	ClientTypePreregistered = "preregistered"

	ApplicationTypeWeb    = "web"
	ApplicationTypeNative = "native"
)

const (
	// a cimd document is re-fetched after this, so a client can rotate its
	// redirect uris without us pinning a stale copy forever
	cimdCacheDuration = 6 * time.Hour
	cimdFetchTimeout  = 10 * time.Second
	cimdMaxBodyBytes  = 256 * 1024
	cimdMaxRedirects  = 3
)

// Allows the integration tests to point a client id at a loopback document.
// Production leaves this false; see the ssrf note in the file header.
var cimdAllowPrivateTargets = false

type Client struct {
	ClientId        string
	ClientType      string
	ClientName      string
	ClientUri       string
	LogoUri         string
	ApplicationType string
	RedirectUris    []string
	Scopes          []string
}

// Reports whether a redirect uri was registered by this client.
//
// Compared exactly, per oauth 2.1: no prefix or wildcard matching, because a
// loose match is how an open redirector turns into token exfiltration. Native
// clients are the one relaxation -- a loopback redirect may vary its port,
// which rfc 8252 requires the server to allow since the client cannot reserve
// one in advance.
func (self *Client) ValidRedirectUri(redirectUri string) bool {
	for _, registered := range self.RedirectUris {
		if registered == redirectUri {
			return true
		}
	}

	if self.ApplicationType == ApplicationTypeNative {
		for _, registered := range self.RedirectUris {
			if loopbackRedirectMatch(registered, redirectUri) {
				return true
			}
		}
	}

	return false
}

// rfc 8252 section 7.3: for a loopback redirect, everything but the port must
// match.
func loopbackRedirectMatch(registered string, redirectUri string) bool {
	registeredUrl, err := url.Parse(registered)
	if err != nil {
		return false
	}
	redirectUrl, err := url.Parse(redirectUri)
	if err != nil {
		return false
	}

	if registeredUrl.Scheme != "http" || redirectUrl.Scheme != "http" {
		return false
	}
	if !isLoopbackHost(registeredUrl.Hostname()) || !isLoopbackHost(redirectUrl.Hostname()) {
		return false
	}
	return registeredUrl.Hostname() == redirectUrl.Hostname() &&
		registeredUrl.Path == redirectUrl.Path
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsLoopback()
}

// Resolves a client id to a client. An https client id is a client id metadata
// document url and is fetched (or served from the cached row); anything else
// must already be registered.
func GetClient(ctx context.Context, clientId string) (*Client, error) {
	if isCimdClientId(clientId) {
		return getCimdClient(ctx, clientId)
	}

	client := loadClient(ctx, clientId)
	if client == nil {
		return nil, ErrUnknownClient
	}
	return client, nil
}

// A client id metadata document id is an https url, which is what
// distinguishes it from an opaque registered id.
func isCimdClientId(clientId string) bool {
	clientUrl, err := url.Parse(clientId)
	if err != nil {
		return false
	}
	return clientUrl.Scheme == "https" && clientUrl.Host != ""
}

func getCimdClient(ctx context.Context, clientId string) (*Client, error) {
	// a cached document that has not gone stale
	if client := loadClient(ctx, clientId); client != nil && client.ClientType == ClientTypeCimd {
		if !cimdExpired(ctx, clientId) {
			return client, nil
		}
	}

	client, err := fetchCimdDocument(ctx, clientId)
	if err != nil {
		// fall back to a stale cached copy rather than failing an
		// authorization because the client's document host is briefly down
		if cached := loadClient(ctx, clientId); cached != nil && cached.ClientType == ClientTypeCimd {
			return cached, nil
		}
		return nil, err
	}

	saveClient(ctx, client, server.NowUtc().Add(cimdCacheDuration))
	return client, nil
}

// The subset of rfc 7591 client metadata this server uses.
type clientMetadataDocument struct {
	ClientId        string   `json:"client_id"`
	ClientName      string   `json:"client_name"`
	ClientUri       string   `json:"client_uri"`
	LogoUri         string   `json:"logo_uri"`
	ApplicationType string   `json:"application_type"`
	RedirectUris    []string `json:"redirect_uris"`
	Scope           string   `json:"scope"`
	GrantTypes      []string `json:"grant_types"`
}

func fetchCimdDocument(ctx context.Context, clientId string) (*Client, error) {
	documentUrl, err := url.Parse(clientId)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidClientMeta, err)
	}
	if err := validateCimdUrl(documentUrl); err != nil {
		return nil, err
	}

	fetchCtx, cancel := context.WithTimeout(ctx, cimdFetchTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, clientId, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")

	response, err := cimdHttpClient().Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidClientMeta, err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: status %d", ErrInvalidClientMeta, response.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(response.Body, cimdMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if cimdMaxBodyBytes < len(body) {
		return nil, fmt.Errorf("%w: document too large", ErrInvalidClientMeta)
	}

	var document clientMetadataDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidClientMeta, err)
	}

	// the document must claim the id it was fetched from, or one client could
	// publish a document impersonating another
	if document.ClientId != clientId {
		return nil, fmt.Errorf("%w: client_id does not match the document url", ErrInvalidClientMeta)
	}
	if len(document.RedirectUris) == 0 {
		return nil, fmt.Errorf("%w: no redirect_uris", ErrInvalidClientMeta)
	}

	applicationType := document.ApplicationType
	if applicationType == "" {
		applicationType = inferApplicationType(document.RedirectUris)
	}

	if err := validateRedirectUris(document.RedirectUris, applicationType); err != nil {
		return nil, err
	}

	return &Client{
		ClientId:        clientId,
		ClientType:      ClientTypeCimd,
		ClientName:      document.ClientName,
		ClientUri:       document.ClientUri,
		LogoUri:         document.LogoUri,
		ApplicationType: applicationType,
		RedirectUris:    document.RedirectUris,
		Scopes:          ParseScope(document.Scope),
	}, nil
}

// Registers a client dynamically (rfc 7591). Deprecated by the spec in favor
// of cimd, kept for clients that do not support it.
func RegisterClient(ctx context.Context, document *clientMetadataDocument) (*Client, error) {
	if len(document.RedirectUris) == 0 {
		return nil, fmt.Errorf("%w: no redirect_uris", ErrInvalidClientMeta)
	}

	applicationType := document.ApplicationType
	if applicationType == "" {
		applicationType = inferApplicationType(document.RedirectUris)
	}

	if err := validateRedirectUris(document.RedirectUris, applicationType); err != nil {
		return nil, err
	}

	client := &Client{
		// opaque: a dynamically registered id must not be confusable with a
		// cimd url
		ClientId:        fmt.Sprintf("urn_client_%s", server.NewId()),
		ClientType:      ClientTypeDynamic,
		ClientName:      document.ClientName,
		ClientUri:       document.ClientUri,
		LogoUri:         document.LogoUri,
		ApplicationType: applicationType,
		RedirectUris:    document.RedirectUris,
		Scopes:          ParseScope(document.Scope),
	}

	saveClient(ctx, client, time.Time{})
	return client, nil
}

// A native client may register a loopback redirect; a web client must use
// https. This is what lets claude desktop and the cli work without allowing a
// web client to redirect a code to localhost.
func validateRedirectUris(redirectUris []string, applicationType string) error {
	for _, redirectUri := range redirectUris {
		redirectUrl, err := url.Parse(redirectUri)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrInvalidRedirect, redirectUri)
		}
		if redirectUrl.Fragment != "" {
			return fmt.Errorf("%w: fragment not allowed", ErrInvalidRedirect)
		}

		switch redirectUrl.Scheme {
		case "https":
		case "http":
			if applicationType != ApplicationTypeNative || !isLoopbackHost(redirectUrl.Hostname()) {
				return fmt.Errorf("%w: http is only allowed for native loopback", ErrInvalidRedirect)
			}
		default:
			// a private uri scheme is how a native app receives a redirect
			if applicationType != ApplicationTypeNative {
				return fmt.Errorf("%w: unsupported scheme %s", ErrInvalidRedirect, redirectUrl.Scheme)
			}
		}
	}
	return nil
}

func inferApplicationType(redirectUris []string) string {
	for _, redirectUri := range redirectUris {
		redirectUrl, err := url.Parse(redirectUri)
		if err != nil {
			continue
		}
		if redirectUrl.Scheme != "https" {
			return ApplicationTypeNative
		}
	}
	return ApplicationTypeWeb
}

// The client id is supplied by the caller, so fetching it is an ssrf vector in
// the authorization server. Refuse anything that is not a public https target.
func validateCimdUrl(documentUrl *url.URL) error {
	if documentUrl.Scheme != "https" {
		return fmt.Errorf("%w: client_id must be https", ErrInvalidClientMeta)
	}
	if documentUrl.Fragment != "" {
		return fmt.Errorf("%w: client_id must not have a fragment", ErrInvalidClientMeta)
	}

	if cimdAllowPrivateTargets {
		return nil
	}

	host := documentUrl.Hostname()
	if isLoopbackHost(host) {
		return fmt.Errorf("%w: refusing a loopback client_id", ErrInvalidClientMeta)
	}
	// a hostname is resolved by the dialer below, which re-checks the address
	if addr, err := netip.ParseAddr(host); err == nil {
		if !publicAddr(addr) {
			return fmt.Errorf("%w: refusing a private client_id", ErrInvalidClientMeta)
		}
	}

	return nil
}

func publicAddr(addr netip.Addr) bool {
	return !addr.IsLoopback() &&
		!addr.IsPrivate() &&
		!addr.IsLinkLocalUnicast() &&
		!addr.IsLinkLocalMulticast() &&
		!addr.IsMulticast() &&
		!addr.IsUnspecified()
}

// The dialer re-checks the resolved address, which closes the dns rebinding
// gap that a url-only check leaves open: a hostname that passes validation can
// still resolve to a private address.
func cimdHttpClient() *http.Client {
	dialer := &net.Dialer{Timeout: cimdFetchTimeout}

	return &http.Client{
		Timeout: cimdFetchTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network string, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				if !cimdAllowPrivateTargets {
					ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
					if err != nil {
						return nil, err
					}
					for _, ip := range ips {
						if !publicAddr(ip.Unmap()) {
							return nil, fmt.Errorf("refusing a private address for %s", host)
						}
					}
				}
				return dialer.DialContext(ctx, network, addr)
			},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if cimdMaxRedirects <= len(via) {
				return fmt.Errorf("too many redirects")
			}
			return validateCimdUrl(req.URL)
		},
	}
}

func loadClient(ctx context.Context, clientId string) *Client {
	var client *Client

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_type,
				client_name,
				client_uri,
				logo_uri,
				application_type,
				redirect_uris_json,
				scope
			FROM oauth_client
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var (
					clientType       string
					clientName       *string
					clientUri        *string
					logoUri          *string
					applicationType  string
					redirectUrisJson string
					scope            *string
				)
				server.Raise(result.Scan(
					&clientType,
					&clientName,
					&clientUri,
					&logoUri,
					&applicationType,
					&redirectUrisJson,
					&scope,
				))

				redirectUris := []string{}
				json.Unmarshal([]byte(redirectUrisJson), &redirectUris)

				client = &Client{
					ClientId:        clientId,
					ClientType:      clientType,
					ApplicationType: applicationType,
					RedirectUris:    redirectUris,
				}
				if clientName != nil {
					client.ClientName = *clientName
				}
				if clientUri != nil {
					client.ClientUri = *clientUri
				}
				if logoUri != nil {
					client.LogoUri = *logoUri
				}
				if scope != nil {
					client.Scopes = ParseScope(*scope)
				}
			}
		})
	})

	return client
}

func cimdExpired(ctx context.Context, clientId string) bool {
	expired := true

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT metadata_expire_time
			FROM oauth_client
			WHERE client_id = $1
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var expireTime *time.Time
				server.Raise(result.Scan(&expireTime))
				if expireTime != nil {
					expired = server.NowUtc().After(*expireTime)
				}
			}
		})
	})

	return expired
}

func saveClient(ctx context.Context, client *Client, metadataExpireTime time.Time) {
	redirectUrisJson, err := json.Marshal(client.RedirectUris)
	if err != nil {
		panic(err)
	}

	var expireTime *time.Time
	if !metadataExpireTime.IsZero() {
		expireTime = &metadataExpireTime
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO oauth_client (
				client_id,
				client_type,
				client_name,
				client_uri,
				logo_uri,
				application_type,
				redirect_uris_json,
				scope,
				create_time,
				update_time,
				metadata_expire_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, $10)
			ON CONFLICT (client_id) DO UPDATE
			SET
				client_type = $2,
				client_name = $3,
				client_uri = $4,
				logo_uri = $5,
				application_type = $6,
				redirect_uris_json = $7,
				scope = $8,
				update_time = $9,
				metadata_expire_time = $10
			`,
			client.ClientId,
			client.ClientType,
			client.ClientName,
			client.ClientUri,
			client.LogoUri,
			client.ApplicationType,
			string(redirectUrisJson),
			FormatScope(client.Scopes),
			server.NowUtc(),
			expireTime,
		))
	})
}

// Remembered consent for a (user, client) pair.
type Consent struct {
	UserId    server.Id
	ClientId  string
	NetworkId server.Id
	Scopes    []string
}

// Reports whether the user has already approved every requested scope for this
// client. Any scope outside the remembered set re-prompts.
func ConsentSatisfies(ctx context.Context, userId server.Id, clientId string, requestedScopes []string) bool {
	consent := GetConsent(ctx, userId, clientId)
	if consent == nil {
		return false
	}
	for _, requested := range requestedScopes {
		if !HasScope(consent.Scopes, requested) {
			return false
		}
	}
	return true
}

func GetConsent(ctx context.Context, userId server.Id, clientId string) *Consent {
	var consent *Consent

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT network_id, scope
			FROM oauth_consent
			WHERE user_id = $1 AND client_id = $2
			`,
			userId,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				var (
					networkId server.Id
					scope     string
				)
				server.Raise(result.Scan(&networkId, &scope))
				consent = &Consent{
					UserId:    userId,
					ClientId:  clientId,
					NetworkId: networkId,
					Scopes:    ParseScope(scope),
				}
			}
		})
	})

	return consent
}

// Records approval, unioning with anything already approved so a narrower
// later grant does not silently drop permissions the client still relies on.
func SaveConsent(ctx context.Context, consent *Consent) {
	scopes := consent.Scopes
	if existing := GetConsent(ctx, consent.UserId, consent.ClientId); existing != nil {
		for _, scope := range existing.Scopes {
			if !HasScope(scopes, scope) {
				scopes = append(scopes, scope)
			}
		}
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO oauth_consent (
				user_id,
				client_id,
				network_id,
				scope,
				create_time,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $5)
			ON CONFLICT (user_id, client_id) DO UPDATE
			SET
				network_id = $3,
				scope = $4,
				update_time = $5
			`,
			consent.UserId,
			consent.ClientId,
			consent.NetworkId,
			FormatScope(scopes),
			server.NowUtc(),
		))
	})
}

// Disconnects a client: the remembered consent is dropped and every refresh
// token the client holds is revoked. Access tokens already issued stay valid
// until they expire, which is why they are short.
func RevokeConsent(ctx context.Context, userId server.Id, clientId string) {
	familyIds := []server.Id{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT DISTINCT family_id
			FROM oauth_refresh_token
			WHERE user_id = $1 AND client_id = $2 AND revoke_time IS NULL
			`,
			userId,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var familyId server.Id
				server.Raise(result.Scan(&familyId))
				familyIds = append(familyIds, familyId)
			}
		})
	})

	for _, familyId := range familyIds {
		RevokeRefreshTokenFamily(ctx, familyId)
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM oauth_consent
			WHERE user_id = $1 AND client_id = $2
			`,
			userId,
			clientId,
		))
	})
}

// Strips any scope the server does not support, so an unknown scope is
// ignored rather than granted.
func FilterSupportedScopes(requestedScopes []string) []string {
	supported := SupportedScopes()
	scopes := []string{}
	for _, requested := range requestedScopes {
		if HasScope(supported, requested) && !HasScope(scopes, requested) {
			scopes = append(scopes, requested)
		}
	}
	return scopes
}

// The canonical resource identifier, per rfc 8707 / rfc 9728: no fragment, and
// no trailing slash so it matches the audience minted into tokens.
func CanonicalResource(resource string) (string, error) {
	resourceUrl, err := url.Parse(resource)
	if err != nil {
		return "", err
	}
	if resourceUrl.Scheme == "" || resourceUrl.Host == "" {
		return "", fmt.Errorf("resource must be an absolute uri")
	}
	if resourceUrl.Fragment != "" {
		return "", fmt.Errorf("resource must not have a fragment")
	}
	resourceUrl.Path = strings.TrimSuffix(resourceUrl.Path, "/")
	return resourceUrl.String(), nil
}
