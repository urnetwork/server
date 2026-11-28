package api

import (
	"github.com/urnetwork/server/v2026/oauth"
	"github.com/urnetwork/server/v2026/router"
)

// The authorization server routes (IDP.md). Served on the issuer host.
//
// The table is here rather than in oauth so that oauth does not import
// router. model imports oauth (a credential rotation revokes the user's OAuth
// grants in its transaction, oauth.RevokeUserGrantsInTx), and router's tests
// import model, so an oauth that imports router makes router's test build an
// import cycle.
func oauthRoutes() []*router.Route {
	return []*router.Route{
		router.NewRoute("GET", "/\\.well-known/oauth-authorization-server", oauth.ServerMetadataHandler),
		router.NewRoute("GET", "/\\.well-known/openid-configuration", oauth.ServerMetadataHandler),
		router.NewRoute("GET", "/\\.well-known/jwks\\.json", oauth.JwksHandler),
		router.NewRoute("POST", "/oauth/token", oauth.TokenHandler),
		router.NewRoute("POST", "/oauth/register", oauth.RegisterHandler),
		router.NewRoute("POST", "/oauth/revoke", oauth.RevokeHandler),
		router.NewRoute("GET", "/oauth/userinfo", oauth.UserinfoHandler),
		// called by the consent page on the ur.io origin
		router.NewRoute("POST", "/oauth/authorize", oauth.AuthorizeHandler),
		router.NewRoute("POST", "/oauth/consent", oauth.ConsentHandler),
	}
}
