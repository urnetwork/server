package mcp

// The mcp server as an oauth 2.1 resource server (IDP.md §10).
//
// Every request carries an access token minted by our authorization server for
// THIS resource. The token is verified for signature, issuer, expiry, and
// audience; a token minted for anything else is refused, which is what rfc 8707
// audience binding is for.
//
// This is a hard cut: network ByJwts and `urn_` api keys are no longer accepted
// here. The mcp spec is explicit that a server must only accept tokens issued
// by its own authorization server and must not accept or transit any others,
// and a platform-wide ByJwt is exactly such an "other" token.
//
// Scopes gate tools: `mcp:read` for lookups, `mcp:fetch` for the egress tool
// that provisions billed clients. An insufficient scope is a 403 with a
// challenge naming what is missing, so a client can step up.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
)

// The canonical resource identifier of this server, and the audience every
// access token must carry. Overridable for tests, which serve on an ephemeral
// port.
var McpResource = "https://mcp.bringyour.com"

const protectedResourceMetadataPath = "/.well-known/oauth-protected-resource"

func ProtectedResourceMetadataUrl() string {
	return McpResource + protectedResourceMetadataPath
}

// Verifies an access token minted for this resource.
//
// The audience is passed explicitly, so a token for another resource cannot
// satisfy this check. The scopes come back on the TokenInfo for the per-tool
// gate below.
func verifyAccessToken(ctx context.Context, tokenStr string, req *http.Request) (*auth.TokenInfo, error) {
	claims, err := oauth.VerifyAccessToken(tokenStr, McpResource)
	if err != nil {
		if glog.V(1) {
			glog.Infof("[mcp]token rejected = %s\n", err)
		}
		// the sdk maps this to a 401 with the resource metadata challenge
		return nil, fmt.Errorf("%w: %s", auth.ErrInvalidToken, err)
	}

	expiry, err := claims.GetExpirationTime()
	if err != nil || expiry == nil {
		return nil, fmt.Errorf("%w: no expiry", auth.ErrInvalidToken)
	}

	return &auth.TokenInfo{
		Scopes:     oauth.ParseScope(claims.Scope),
		Expiration: expiry.Time,
		// prevents session hijacking in the sdk's stateful paths; harmless
		// under stateless, and correct if that ever changes
		UserID: claims.Subject,
		Extra: map[string]any{
			"network_id": claims.NetworkId,
			"principal":  claims.Principal,
			"roles":      claims.Roles,
			"client_id":  claims.ClientId,
		},
	}, nil
}

// Wraps a handler so every mcp request must present a valid access token.
// `mcp:read` is the floor: it is the least any tool needs, so a token without
// it cannot do anything here and is better refused at the door with a
// challenge that tells the client what to ask for.
func requireAccessToken(handler http.Handler) http.Handler {
	return auth.RequireBearerToken(
		verifyAccessToken,
		&auth.RequireBearerTokenOptions{
			ResourceMetadataURL: ProtectedResourceMetadataUrl(),
			Scopes:              []string{oauth.ScopeMcpRead},
			// tokens from an authorization server whose clock runs slightly
			// fast should not be refused at the boundary
			ClockSkew: 30 * time.Second,
		},
	)(handler)
}

// Serves the rfc 9728 document that points clients at our authorization
// server. This is what turns a 401 into a discoverable login.
func protectedResourceMetadataHandler() http.Handler {
	metadata := oauth.McpProtectedResourceMetadata(McpResource)

	return auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               metadata.Resource,
		AuthorizationServers:   metadata.AuthorizationServers,
		ScopesSupported:        metadata.ScopesSupported,
		BearerMethodsSupported: metadata.BearerMethodsSupported,
	})
}

// Builds the client session for a tool call from the verified access token.
//
// The model layer reads `session.ClientSession.ByJwt` throughout
// (`AuthNetworkClient` needs the network, `FindProviderLocations` needs the
// session), so the token's claims are turned into an in-memory ByJwt. It is
// never signed and never leaves the process: it is a carrier for the identity
// the token already proved, not a credential.
func clientSessionFromToken(ctx context.Context, req *mcpsdk.CallToolRequest) (*session.ClientSession, error) {
	tokenInfo := auth.TokenInfoFromContext(ctx)
	if tokenInfo == nil {
		return nil, fmt.Errorf("no access token")
	}

	userId, err := server.ParseId(tokenInfo.UserID)
	if err != nil {
		return nil, fmt.Errorf("the token subject is not a user id")
	}

	networkId, _ := tokenInfo.Extra["network_id"].(server.Id)
	if networkId == (server.Id{}) {
		return nil, fmt.Errorf("the token carries no network")
	}

	clientSession := session.NewLocalClientSession(ctx, clientAddress(req), nil)

	principal, _ := tokenInfo.Extra["principal"].(string)
	roles, _ := tokenInfo.Extra["roles"].([]string)

	byJwt := jwt.NewByJwt(
		networkId,
		userId,
		// the network name is not in the token; the model layer that needs it
		// looks it up, and nothing authorizes on it
		"",
		// a guest network can never authorize (IDP.md §2), so a token always
		// represents a full network
		false,
		false,
	)
	byJwt.Principal = principal
	byJwt.Roles = roles
	clientSession.ByJwt = byJwt

	return clientSession, nil
}

// Reports whether the verified token carries a scope.
func tokenHasScope(ctx context.Context, scope string) bool {
	tokenInfo := auth.TokenInfoFromContext(ctx)
	if tokenInfo == nil {
		return false
	}
	return oauth.HasScope(tokenInfo.Scopes, scope)
}

// Stable, non-secret identity binding for caller-threaded state. The digest
// keeps raw subject/client identifiers out of AEAD additional-data traces.
func tokenStateBinding(ctx context.Context) (string, error) {
	tokenInfo := auth.TokenInfoFromContext(ctx)
	if tokenInfo == nil || tokenInfo.UserID == "" {
		return "", fmt.Errorf("no access token identity")
	}
	networkId, _ := tokenInfo.Extra["network_id"].(server.Id)
	clientId, _ := tokenInfo.Extra["client_id"].(string)
	if networkId == (server.Id{}) || clientId == "" || McpResource == "" {
		return "", fmt.Errorf("access token identity is incomplete")
	}
	return identityStateBinding(tokenInfo.UserID, networkId, clientId, McpResource), nil
}

func identityStateBinding(userId string, networkId server.Id, clientId string, resource string) string {
	binding := fmt.Sprintf("v1\x00%s\x00%s\x00%s\x00%s", userId, networkId, clientId, resource)
	digest := sha256.Sum256([]byte(binding))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// The tool result for a token that authenticated but lacks the scope this tool
// needs. A tool call cannot carry an http status, so the challenge is stated in
// the result text instead: the client is told exactly which scope to request,
// which is what the step-up flow needs.
func insufficientScopeResult(scope string) *mcpsdk.CallToolResult {
	return fetchErrorResult(fmt.Sprintf(
		"This tool requires the %q scope. Re-authorize requesting %q in addition to any scopes already granted.",
		scope,
		scope,
	))
}

func clientAddress(req *mcpsdk.CallToolRequest) string {
	var header http.Header
	if req.Extra != nil {
		header = req.Extra.Header
	}
	if header == nil {
		header = http.Header{}
	}

	clientAddress := header.Get("X-UR-Forwarded-For")
	if clientAddress == "" {
		clientIpStr := header.Get("X-Forwarded-For")
		clientPortStr := header.Get("X-Forwarded-Source-Port")
		if clientIpStr != "" && clientPortStr != "" {
			clientAddress = fmt.Sprintf("%s:%s", clientIpStr, clientPortStr)
		}
	}
	if clientAddress == "" {
		clientAddress = header.Get("X-UR-Remote-Addr")
	}
	if clientAddress == "" {
		clientAddress = "127.0.0.1:0"
	}
	return clientAddress
}
