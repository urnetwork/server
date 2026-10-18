package model

// Signed proxy ids under a proxy.yml whose secrets include blank entries. Each
// test pushes synthetic secrets; nothing reads a deployment vault.

import (
	"crypto/hmac"
	"crypto/sha1"
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/server"
)

const syntheticProxySigningSecret = "synthetic-proxy-signing-secret"

// Pushes proxy.yml for the rest of the test and makes the memoized secrets
// load it again.
func setProxySecretsForTest(tb testing.TB, resource string) {
	tb.Helper()
	pop := server.Vault.PushSimpleResource("proxy.yml", []byte(resource))
	previousSecrets, previousSecret := proxySigningSecrets, proxySigningSecret
	proxySigningSecrets = sync.OnceValue(loadProxySigningSecrets)
	proxySigningSecret = sync.OnceValue(loadProxySigningSecret)
	tb.Cleanup(func() {
		proxySigningSecrets, proxySigningSecret = previousSecrets, previousSecret
		pop()
	})
}

// A signed proxy id for proxyId under secret, as SignProxyId forms one.
func proxyIdSignedUnder(proxyId server.Id, secret string) string {
	h := hmac.New(sha1.New, []byte(secret))
	h.Write(proxyId.Bytes())
	return base32Encoder().EncodeToString(append(proxyId.Bytes(), h.Sum(nil)...))
}

// A blank entry was a verification key, so anyone who knew a proxy id could
// sign it under the empty key and pass as that proxy's client.
func TestParseSignedProxyIdRefusesIdSignedUnderBlankSecret(t *testing.T) {
	for _, blank := range []string{"", "   "} {
		setProxySecretsForTest(t, "secrets:\n  - '"+blank+"'\n  - "+syntheticProxySigningSecret+"\n")
		proxyId := server.NewId()
		if _, err := ParseSignedProxyId(proxyIdSignedUnder(proxyId, blank)); err == nil {
			t.Fatalf("a proxy id signed under the blank secret %q was accepted", blank)
		}
		parsedProxyId, err := ParseSignedProxyId(proxyIdSignedUnder(proxyId, syntheticProxySigningSecret))
		if err != nil || parsedProxyId != proxyId {
			t.Fatalf("a proxy id signed under the configured secret did not verify: %v", err)
		}
	}
}

// A blank first entry was the signing key, so every issued id was one anyone
// could have signed.
func TestSignProxyIdSkipsBlankSecret(t *testing.T) {
	setProxySecretsForTest(t, "secrets:\n  - ''\n  - "+syntheticProxySigningSecret+"\n")
	proxyId := server.NewId()
	signedProxyId := SignProxyId(proxyId)
	if signedProxyId != proxyIdSignedUnder(proxyId, syntheticProxySigningSecret) {
		t.Fatal("the proxy id was not signed under the first non-blank secret")
	}
	parsedProxyId, err := ParseSignedProxyId(signedProxyId)
	if err != nil || parsedProxyId != proxyId {
		t.Fatalf("a signed proxy id did not verify: %v", err)
	}
}

func TestSignProxyIdRefusesWithoutNonblankSecret(t *testing.T) {
	setProxySecretsForTest(t, "secrets:\n  - ''\n  - '   '\n")
	signed := func() (signedProxyId string, refusal any) {
		defer func() {
			refusal = recover()
		}()
		return SignProxyId(server.NewId()), nil
	}
	signedProxyId, refusal := signed()
	if refusal == nil || !strings.Contains(refusal.(error).Error(), "no non-blank secret") {
		t.Fatalf("a proxy id was signed without a non-blank secret: %q %v", signedProxyId, refusal)
	}
}

// A signature compared byte by byte, returning at the first difference, tells
// a caller timing the answers how much of its forgery was right. Timing is not
// a deterministic observation, so this reads the comparison the source makes.
func TestParseSignedProxyIdComparesSignaturesInConstantTime(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "network_client_proxy_model.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, decl := range file.Decls {
		function, ok := decl.(*ast.FuncDecl)
		if !ok || function.Name.Name != "ParseSignedProxyId" {
			continue
		}
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := selector.X.(*ast.Ident); ok {
						calls = append(calls, pkg.Name+"."+selector.Sel.Name)
					}
				}
			}
			return true
		})
	}
	if !slices.Contains(calls, "hmac.Equal") {
		t.Fatalf("ParseSignedProxyId does not compare signatures with hmac.Equal: %v", calls)
	}
	for _, variableTime := range []string{"slices.Equal", "bytes.Equal", "reflect.DeepEqual"} {
		if slices.Contains(calls, variableTime) {
			t.Fatalf("ParseSignedProxyId compares with %s", variableTime)
		}
	}
}

// Every proxy request verifies a signed proxy id, against the memoized secrets.
func BenchmarkParseSignedProxyId(b *testing.B) {
	setProxySecretsForTest(b, "secrets:\n  - "+syntheticProxySigningSecret+"\n  - synthetic-proxy-previous-secret\n")
	signedProxyId := SignProxyId(server.NewId())
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseSignedProxyId(signedProxyId); err != nil {
			b.Fatal(err)
		}
	}
}
