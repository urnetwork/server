package oauth

// The discovery documents.
//
// Clients read these to find the authorization server, its endpoints, and the
// keys that verify its tokens. The mcp spec requires the protected resource
// metadata on the resource server, and at least one of rfc 8414 or openid
// connect discovery on the authorization server -- while requiring clients to
// support both, so both are published.

import (
	"fmt"
)

// rfc 8414 authorization server metadata, which is also the openid connect
// discovery document: the two share a schema, and every field openid connect
// additionally requires is populated here.
type AuthorizationServerMetadata struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint,omitempty"`
	RevocationEndpoint    string `json:"revocation_endpoint,omitempty"`
	UserinfoEndpoint      string `json:"userinfo_endpoint,omitempty"`
	JwksUri               string `json:"jwks_uri"`

	ScopesSupported                            []string `json:"scopes_supported"`
	ResponseTypesSupported                     []string `json:"response_types_supported"`
	ResponseModesSupported                     []string `json:"response_modes_supported,omitempty"`
	GrantTypesSupported                        []string `json:"grant_types_supported"`
	CodeChallengeMethodsSupported              []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported          []string `json:"token_endpoint_auth_methods_supported"`
	SubjectTypesSupported                      []string `json:"subject_types_supported,omitempty"`
	IdTokenSigningAlgValuesSupported           []string `json:"id_token_signing_alg_values_supported,omitempty"`
	ClaimsSupported                            []string `json:"claims_supported,omitempty"`
	ClientIdMetadataDocumentSupported          bool     `json:"client_id_metadata_document_supported"`
	AuthorizationResponseIssParameterSupported bool     `json:"authorization_response_iss_parameter_supported"`
	// rfc 8707
	ResourceIndicatorsSupported bool `json:"resource_indicators_supported"`
}

// rfc 9728 protected resource metadata, served by the mcp server.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported,omitempty"`
	BearerMethodsSupported []string `json:"bearer_methods_supported,omitempty"`
	ResourceDocumentation  string   `json:"resource_documentation,omitempty"`
}

func AuthorizeEndpoint() string {
	return AuthorizationEndpoint()
}

func TokenEndpoint() string {
	return fmt.Sprintf("%s/oauth/token", Issuer())
}

func RegistrationEndpoint() string {
	return fmt.Sprintf("%s/oauth/register", Issuer())
}

func RevocationEndpoint() string {
	return fmt.Sprintf("%s/oauth/revoke", Issuer())
}

func UserinfoEndpoint() string {
	return fmt.Sprintf("%s/oauth/userinfo", Issuer())
}

func JwksUri() string {
	return fmt.Sprintf("%s/.well-known/jwks.json", Issuer())
}

func ServerMetadata() *AuthorizationServerMetadata {
	return &AuthorizationServerMetadata{
		Issuer:                Issuer(),
		AuthorizationEndpoint: AuthorizeEndpoint(),
		TokenEndpoint:         TokenEndpoint(),
		RegistrationEndpoint:  RegistrationEndpoint(),
		RevocationEndpoint:    RevocationEndpoint(),
		UserinfoEndpoint:      UserinfoEndpoint(),
		JwksUri:               JwksUri(),

		ScopesSupported: SupportedScopes(),
		// oauth 2.1: authorization code only, no implicit
		ResponseTypesSupported: []string{"code"},
		ResponseModesSupported: []string{"query"},
		GrantTypesSupported:    []string{"authorization_code", "refresh_token"},
		// oauth 2.1 removes `plain`
		CodeChallengeMethodsSupported: []string{PkceMethodS256},
		// every client here is public: a native or browser client cannot keep
		// a secret, and cimd clients have none by construction
		TokenEndpointAuthMethodsSupported: []string{"none"},
		SubjectTypesSupported:             []string{"public"},
		IdTokenSigningAlgValuesSupported:  []string{signerAlgEs256},
		ClaimsSupported: []string{
			"sub", "iss", "aud", "exp", "iat", "nonce", "auth_time",
			"network_id", "network_name",
		},
		ClientIdMetadataDocumentSupported: true,
		// rfc 9207: we emit `iss` on authorization responses, so clients can
		// defend against mix-up attacks
		AuthorizationResponseIssParameterSupported: true,
		ResourceIndicatorsSupported:                true,
	}
}

// The metadata the mcp server publishes. `offline_access` is deliberately
// absent: per the mcp spec it is not a resource requirement.
func McpProtectedResourceMetadata(resource string) *ProtectedResourceMetadata {
	return &ProtectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{Issuer()},
		ScopesSupported:        McpResourceScopes(),
		BearerMethodsSupported: []string{"header"},
		ResourceDocumentation:  "https://ur.io/docs/mcp",
	}
}
