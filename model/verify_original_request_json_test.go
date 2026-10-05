// SQL indexes project request fields without rewriting canonical signed originals.
package model

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/urnetwork/server"
)

// The real producer's schema includes a NUL domain separator. Bytea custody,
// verifier and server signatures, and all indexed request fields must survive it.
func TestVerifyOriginalRequestCanonicalDomainRoundTrip(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		original, key := testVerifySignedOriginal(t)
		if !bytes.Contains(original.Body, []byte(`\u0000`)) {
			t.Fatal("canonical producer omitted its signed NUL domain")
		}
		body, err := ValidateVerifyOriginal(original, key.Public().(ed25519.PublicKey))
		if err != nil {
			t.Fatal(err)
		}
		retained := RetainVerifyOriginal(t.Context(), original)
		if !bytes.Equal(retained.Body, original.Body) || !bytes.Equal(retained.Signature, original.Signature) {
			t.Fatal("SQL projection changed signed original bytes")
		}
		request := VerifyOriginalRequest{Scope: body.Scope, ClientId: body.Trail.ClientId, Message: body.RequestMessage, Signature: body.RequestSignature}
		for _, read := range []*VerifyOriginalTransition{
			GetVerifyOriginalRequest(t.Context(), request),
			GetLatestVerifyOriginal(t.Context(), body.Trail.TrailId),
			RetainVerifyOriginal(t.Context(), verifyOriginalRequestTestAlternative(t, original, key)),
		} {
			if read == nil || !bytes.Equal(read.Body, original.Body) || !bytes.Equal(read.Signature, original.Signature) {
				t.Fatal("fresh reader or retry replaced the retained signature authority")
			}
			if _, err := ValidateVerifyOriginal(read, key.Public().(ed25519.PublicKey)); err != nil {
				t.Fatal(err)
			}
		}
		server.Db(t.Context(), func(conn server.PgConn) {
			var clientId server.Id
			var scope string
			var message, signature []byte
			server.Raise(conn.QueryRow(t.Context(), `SELECT client_id,scope_json::text,request_message,request_signature
				FROM verify_original_request_lookup WHERE trail_id=$1 AND previous_depth=$2`, body.Trail.TrailId, body.PreviousDepth).
				Scan(&clientId, &scope, &message, &signature))
			if clientId != request.ClientId || scope != "null" || !bytes.Equal(message, request.Message) || !bytes.Equal(signature, request.Signature) {
				t.Fatal("projection changed exact client, nil scope, request or verifier signature")
			}
		})
		changed := request
		changed.Signature = bytes.Clone(request.Signature)
		changed.Signature[0] ^= 1
		if got := GetVerifyOriginalRequest(t.Context(), changed); got != nil {
			t.Fatal("changed request signature borrowed the original")
		}
	})
}
