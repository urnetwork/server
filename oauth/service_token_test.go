package oauth

// Service tokens and the key sets (service_token.go, keys.go; §7.8). Keys are
// the test's own, one per set, installed in place of auth.yml's; the issuer is
// the local vault's.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A new signer key of the set.
func newTestSignerKey(t testing.TB, set SignerKeySet) *SignerKey {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	thumbprint, err := SignerKid(&privateKey.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return &SignerKey{Kid: set.KidPrefix() + thumbprint, Alg: signerAlgEs256, Set: set, PrivateKey: privateKey}
}

// The test's issuer, in place of auth.yml's.
const testIssuer = "https://auth.example"

func installTestIssuer(t testing.TB) {
	t.Helper()
	t.Cleanup(Testing_SetIssuer(testIssuer))
}

// One key of every set, installed for the test.
type testSignerKeys struct {
	mcp     *SignerKey
	admin   *SignerKey
	service *SignerKey
	id      *SignerKey
}

func installTestSignerKeys(t testing.TB) *testSignerKeys {
	t.Helper()
	installTestIssuer(t)
	keys := &testSignerKeys{
		mcp:     newTestSignerKey(t, SignerKeySetMcp),
		admin:   newTestSignerKey(t, SignerKeySetAdmin),
		service: newTestSignerKey(t, SignerKeySetService),
		id:      newTestSignerKey(t, SignerKeySetId),
	}
	t.Cleanup(Testing_SetSignerKeys(keys.service, keys.mcp, keys.admin, keys.id))
	return keys
}

func newTestServiceTokenArgs() *MintServiceTokenArgs {
	sidecarId := server.NewId()
	return &MintServiceTokenArgs{
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		SidecarId:   &sidecarId,
		Audience:    "evs",
		Scopes:      []string{"repo:" + server.NewId().String() + ":r"},
		ProjVersion: 7,
	}
}

func newTestServiceTokenVerifier(t testing.TB) *ServiceTokenVerifier {
	t.Helper()
	verifier, err := issuerServiceTokenVerifier()
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

// A minted token verifies for its audience with every claim it was minted with,
// under an svc- kid, for 15 minutes.
func TestServiceTokenRoundTrip(t *testing.T) {
	keys := installTestSignerKeys(t)
	args := newTestServiceTokenArgs()
	memberId := server.NewId()
	args.MemberId = &memberId
	args.Act = &ServiceTokenActor{Subject: "agent"}

	token, expiry, err := MintServiceToken(args)
	connect.AssertEqual(t, err, nil)
	if d := expiry.Sub(server.NowUtc().Add(ServiceTokenDuration)); d < -time.Minute || time.Minute < d {
		t.Fatalf("expiry %s", expiry)
	}
	parsed, _, err := gojwt.NewParser().ParseUnverified(token, &ServiceTokenClaims{})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.Header["kid"], keys.service.Kid)

	claims, err := newTestServiceTokenVerifier(t).Verify(token, "evs")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, claims.NetworkId, args.NetworkId)
	connect.AssertEqual(t, claims.ClientId, args.ClientId)
	connect.AssertEqual(t, *claims.SidecarId, *args.SidecarId)
	connect.AssertEqual(t, *claims.MemberId, memberId)
	connect.AssertEqual(t, claims.SubjectClientId == nil, true)
	connect.AssertEqual(t, claims.Audience(), "evs")
	connect.AssertEqual(t, claims.Scopes(), args.Scopes)
	connect.AssertEqual(t, claims.ProjVersion, uint64(7))
	connect.AssertEqual(t, claims.Act.Subject, "agent")
	connect.AssertEqual(t, claims.Issuer, Issuer())

	// the process's verifier, on the issuer, is the issuer's own set
	claims, err = VerifyServiceToken(token, "evs")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, claims.ClientId, args.ClientId)
}

// The claims' JSON is §7.8's, held to a golden, so a renamed or retyped claim
// fails here before a service misreads it.
func TestServiceTokenClaimsVector(t *testing.T) {
	networkId, _ := server.ParseId("01900000-0000-7000-8000-000000000001")
	clientId, _ := server.ParseId("01900000-0000-7000-8000-000000000002")
	subjectClientId, _ := server.ParseId("01900000-0000-7000-8000-000000000003")
	sidecarId, _ := server.ParseId("01900000-0000-7000-8000-000000000004")
	memberId, _ := server.ParseId("01900000-0000-7000-8000-000000000005")
	iat := time.Unix(1767225600, 0).UTC()
	claims := &ServiceTokenClaims{
		Scope:           "repo:01900000-0000-7000-8000-000000000006:rw recovery:01900000-0000-7000-8000-000000000005",
		ProjVersion:     4096,
		NetworkId:       networkId,
		ClientId:        clientId,
		SubjectClientId: &subjectClientId,
		SidecarId:       &sidecarId,
		MemberId:        &memberId,
		Act:             &ServiceTokenActor{Subject: "01900000-0000-7000-8000-000000000007"},
		RegisteredClaims: gojwt.RegisteredClaims{
			Issuer:    "https://auth.example",
			Subject:   clientId.String(),
			Audience:  gojwt.ClaimStrings{"gkm"},
			IssuedAt:  gojwt.NewNumericDate(iat),
			NotBefore: gojwt.NewNumericDate(iat),
			ExpiresAt: gojwt.NewNumericDate(iat.Add(ServiceTokenDuration)),
			ID:        "01900000-0000-7000-8000-000000000008",
		},
	}
	encoded, err := json.Marshal(claims)
	connect.AssertEqual(t, err, nil)
	golden, err := os.ReadFile("testdata/service_token_claims.json")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, string(encoded), strings.TrimSpace(string(golden)))

	// and it reads back as the same claims
	decoded := &ServiceTokenClaims{}
	connect.AssertEqual(t, json.Unmarshal(golden, decoded), nil)
	connect.AssertEqual(t, decoded.ProjVersion, uint64(4096))
	connect.AssertEqual(t, *decoded.MemberId, memberId)
	connect.AssertEqual(t, decoded.Audience(), "gkm")
}

// The sets are disjoint: a service token verifies nowhere an mcp access token,
// an id token or an admin token is expected, and a token signed by any other
// set's key verifies nowhere a service token is.
func TestSignerKeySetsAreDisjoint(t *testing.T) {
	keys := installTestSignerKeys(t)
	verifier := newTestServiceTokenVerifier(t)

	serviceToken, _, err := MintServiceToken(newTestServiceTokenArgs())
	connect.AssertEqual(t, err, nil)
	_, err = VerifyAccessToken(serviceToken, "evs")
	connect.AssertEqual(t, err != nil, true)
	_, err = VerifyAccessTokenAnyResource(serviceToken)
	connect.AssertEqual(t, err != nil, true)
	_, err = VerifyIdToken(serviceToken, "evs")
	connect.AssertEqual(t, err != nil, true)

	// the same claims under each other set's key
	for _, key := range []*SignerKey{keys.mcp, keys.admin, keys.id} {
		claims := &ServiceTokenClaims{
			NetworkId: server.NewId(),
			ClientId:  server.NewId(),
			RegisteredClaims: gojwt.RegisteredClaims{
				Issuer:    Issuer(),
				Audience:  gojwt.ClaimStrings{"evs"},
				IssuedAt:  gojwt.NewNumericDate(server.NowUtc()),
				ExpiresAt: gojwt.NewNumericDate(server.NowUtc().Add(time.Minute)),
			},
		}
		token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
		token.Header["kid"] = key.Kid
		signed, err := token.SignedString(key.PrivateKey)
		connect.AssertEqual(t, err, nil)
		if _, err := verifier.Verify(signed, "evs"); err == nil {
			t.Fatalf("a %q key's token verified as a service token", key.Set)
		}
		// and a service key's kid under another key's signature
		token.Header["kid"] = keys.service.Kid
		signed, err = token.SignedString(key.PrivateKey)
		connect.AssertEqual(t, err, nil)
		if _, err := verifier.Verify(signed, "evs"); err == nil {
			t.Fatalf("a %q key's signature verified under an svc- kid", key.Set)
		}
	}

	// an mcp access token, minted by today's signer, is no service token
	accessToken, _, err := MintAccessToken(&MintAccessTokenArgs{
		UserId:    server.NewId(),
		NetworkId: server.NewId(),
		ClientId:  "https://client.example/mcp",
		Audience:  "evs",
		Scopes:    []string{ScopeMcpRead},
	})
	connect.AssertEqual(t, err, nil)
	_, err = verifier.Verify(accessToken, "evs")
	connect.AssertEqual(t, err != nil, true)

	// each set's lookups find only its own keys
	connect.AssertEqual(t, VerificationKey(keys.service.Kid) == nil, true)
	connect.AssertEqual(t, VerificationKey(keys.mcp.Kid) != nil, true)
	connect.AssertEqual(t, verificationKeyOf(SignerKeySetService, keys.mcp.Kid) == nil, true)
	connect.AssertEqual(t, verificationKeyOf(SignerKeySetService, keys.admin.Kid) == nil, true)
	connect.AssertEqual(t, len(Jwks().Keys), 1)
	connect.AssertEqual(t, Jwks().Keys[0].Kid, keys.mcp.Kid)
	connect.AssertEqual(t, PublicJwks(SignerKeySetService).Keys[0].Kid, keys.service.Kid)
}

// A verifier takes only svc- keys whose kid is their thumbprint: a document that
// mixes in another set's key, or misnames a key, verifies nothing.
func TestServiceTokenVerifierTakesOnlyServiceKeys(t *testing.T) {
	service := newTestSignerKey(t, SignerKeySetService)
	admin := newTestSignerKey(t, SignerKeySetAdmin)
	mcp := newTestSignerKey(t, SignerKeySetMcp)
	jwk := func(key *SignerKey) *Jwk { return NewSignerJwk(key.Kid, key.Alg, &key.PrivateKey.PublicKey) }

	_, err := NewServiceTokenVerifier(&PublicJwksDocument{Issuer: "https://auth.example", Keys: []*Jwk{jwk(service)}})
	connect.AssertEqual(t, err, nil)

	misnamed := jwk(service)
	misnamed.Kid = SignerKeySetService.KidPrefix() + strings.TrimPrefix(admin.Kid, SignerKeySetAdmin.KidPrefix())
	for name, document := range map[string]*PublicJwksDocument{
		"an admin key":   {Issuer: "https://auth.example", Keys: []*Jwk{jwk(service), jwk(admin)}},
		"an mcp key":     {Issuer: "https://auth.example", Keys: []*Jwk{jwk(mcp)}},
		"a misnamed key": {Issuer: "https://auth.example", Keys: []*Jwk{misnamed}},
		"no issuer":      {Keys: []*Jwk{jwk(service)}},
		"no key":         {Issuer: "https://auth.example"},
		"nil":            nil,
	} {
		if _, err := NewServiceTokenVerifier(document); err == nil {
			t.Fatalf("%s: a verifier was made", name)
		}
	}
}

// Rotation: a new svc- key listed first signs, and a token the previous key
// signed keeps verifying until the previous key leaves the set.
func TestServiceKeyRotationKeepsOldTokensVerifying(t *testing.T) {
	installTestIssuer(t)
	older := newTestSignerKey(t, SignerKeySetService)
	mcp := newTestSignerKey(t, SignerKeySetMcp)
	popOlder := Testing_SetSignerKeys(older, mcp)
	olderToken, _, err := MintServiceToken(newTestServiceTokenArgs())
	connect.AssertEqual(t, err, nil)
	popOlder()

	newer := newTestSignerKey(t, SignerKeySetService)
	defer Testing_SetSignerKeys(newer, older, mcp)()
	newerToken, _, err := MintServiceToken(newTestServiceTokenArgs())
	connect.AssertEqual(t, err, nil)
	parsed, _, err := gojwt.NewParser().ParseUnverified(newerToken, &ServiceTokenClaims{})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.Header["kid"], newer.Kid)

	verifier := newTestServiceTokenVerifier(t)
	for _, token := range []string{olderToken, newerToken} {
		if _, err := verifier.Verify(token, "evs"); err != nil {
			t.Fatal(err)
		}
	}

	// once the older key leaves, its tokens stop verifying
	defer Testing_SetSignerKeys(newer, mcp)()
	verifier = newTestServiceTokenVerifier(t)
	_, err = verifier.Verify(olderToken, "evs")
	connect.AssertEqual(t, err != nil, true)
}

// Every registered claim is checked: the audience, the issuer, the exp on the
// verifier's clock, and the claims §7.8 requires.
func TestServiceTokenVerifierChecksEveryClaim(t *testing.T) {
	keys := installTestSignerKeys(t)
	verifier := newTestServiceTokenVerifier(t)
	token, expiry, err := MintServiceToken(newTestServiceTokenArgs())
	connect.AssertEqual(t, err, nil)

	_, err = verifier.Verify(token, "gkm")
	connect.AssertEqual(t, err != nil, true)
	_, err = verifier.Verify(token, "api")
	connect.AssertEqual(t, err != nil, true)
	_, err = verifier.Verify(token, "")
	connect.AssertEqual(t, err != nil, true)

	verifier.Testing_SetNow(func() time.Time { return expiry.Add(time.Second) })
	_, err = verifier.Verify(token, "evs")
	connect.AssertEqual(t, err != nil, true)
	verifier.Testing_SetNow(server.NowUtc)

	otherIssuer, err := NewServiceTokenVerifier(&PublicJwksDocument{
		Issuer: "https://other-issuer.example",
		Keys:   PublicJwks(SignerKeySetService).Keys,
	})
	connect.AssertEqual(t, err, nil)
	_, err = otherIssuer.Verify(token, "evs")
	connect.AssertEqual(t, err != nil, true)

	sign := func(claims *ServiceTokenClaims) string {
		token := gojwt.NewWithClaims(gojwt.SigningMethodES256, claims)
		token.Header["kid"] = keys.service.Kid
		signed, err := token.SignedString(keys.service.PrivateKey)
		connect.AssertEqual(t, err, nil)
		return signed
	}
	good := func() *ServiceTokenClaims {
		return &ServiceTokenClaims{
			NetworkId: server.NewId(),
			ClientId:  server.NewId(),
			RegisteredClaims: gojwt.RegisteredClaims{
				Issuer:    Issuer(),
				Audience:  gojwt.ClaimStrings{"evs"},
				IssuedAt:  gojwt.NewNumericDate(server.NowUtc()),
				ExpiresAt: gojwt.NewNumericDate(server.NowUtc().Add(time.Minute)),
			},
		}
	}
	_, err = verifier.Verify(sign(good()), "evs")
	connect.AssertEqual(t, err, nil)

	zero := server.Id{}
	repoScopes := []string{}
	for i := 0; i <= ServiceTokenMaxRepoScopeCount; i += 1 {
		repoScopes = append(repoScopes, "repo:"+server.NewId().String()+":r")
	}
	for name, edit := range map[string]func(claims *ServiceTokenClaims){
		"no network":      func(claims *ServiceTokenClaims) { claims.NetworkId = server.Id{} },
		"no client":       func(claims *ServiceTokenClaims) { claims.ClientId = server.Id{} },
		"a zero sidecar":  func(claims *ServiceTokenClaims) { claims.SidecarId = &zero },
		"no iat":          func(claims *ServiceTokenClaims) { claims.IssuedAt = nil },
		"no exp":          func(claims *ServiceTokenClaims) { claims.ExpiresAt = nil },
		"two audiences":   func(claims *ServiceTokenClaims) { claims.RegisteredClaims.Audience = gojwt.ClaimStrings{"evs", "gkm"} },
		"65 repo: scopes": func(claims *ServiceTokenClaims) { claims.Scope = FormatScope(repoScopes) },
	} {
		claims := good()
		edit(claims)
		if _, err := verifier.Verify(sign(claims), "evs"); err == nil {
			t.Fatalf("%s: verified", name)
		}
	}

	// alg none and a symmetric key named by an svc- kid
	none := gojwt.NewWithClaims(gojwt.SigningMethodNone, good())
	none.Header["kid"] = keys.service.Kid
	signed, err := none.SignedString(gojwt.UnsafeAllowNoneSignatureType)
	connect.AssertEqual(t, err, nil)
	_, err = verifier.Verify(signed, "evs")
	connect.AssertEqual(t, err != nil, true)
}

// A mint refuses what no verifier would take: no service audience, the api's, a
// 65th repo: scope, and a set with no key.
func TestMintServiceTokenRefusesWhatNoVerifierTakes(t *testing.T) {
	installTestSignerKeys(t)
	for name, edit := range map[string]func(args *MintServiceTokenArgs){
		"the api's audience": func(args *MintServiceTokenArgs) { args.Audience = "api" },
		"no audience":        func(args *MintServiceTokenArgs) { args.Audience = "" },
		"no client":          func(args *MintServiceTokenArgs) { args.ClientId = server.Id{} },
		"65 repo: scopes": func(args *MintServiceTokenArgs) {
			args.Scopes = []string{}
			for i := 0; i <= ServiceTokenMaxRepoScopeCount; i += 1 {
				args.Scopes = append(args.Scopes, "repo:"+server.NewId().String()+":r")
			}
		},
	} {
		args := newTestServiceTokenArgs()
		edit(args)
		if _, _, err := MintServiceToken(args); err == nil {
			t.Fatalf("%s: minted", name)
		}
	}

	defer Testing_SetSignerKeys(newTestSignerKey(t, SignerKeySetMcp))()
	_, _, err := MintServiceToken(newTestServiceTokenArgs())
	connect.AssertEqual(t, err != nil, true)
}
