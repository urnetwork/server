package controller

// verify_controller_test.go — pure-logic unit tests for the `/verify`
// controller: the parts exercisable without redis/pg. The redis-backed behavior
// (V1 poison/real EXTEND parity, V3 concurrent-EXTEND lock, V4/V5 rate meters)
// needs the pg+redis `test.sh` harness to validate end-to-end.

import (
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// makeTestVerifyKey builds a deterministic server signing key from a repeated
// seed byte, for the pure-logic tests (no vault).
func makeTestVerifyKey(serverKeyId byte, fill byte) *VerifyServerKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = fill
	}
	return &VerifyServerKey{
		ServerKeyId: serverKeyId,
		PrivateKey:  ed25519.NewKeyFromSeed(seed),
	}
}

// TestVerifySyntheticSeedId covers the V2 pure helper. The unresolved-source
// poison seed id must be:
//   - deterministic per source anchor (so seeding twice returns a STABLE
//     trail[0], closing the seed-twice-and-compare real-vs-poison oracle),
//   - a valid 16-byte UUID (indistinguishable in shape from a real provider id),
//   - distinct across distinct anchors, and
//   - dependent on the server signing seed (unguessable to an outside observer).
func TestVerifySyntheticSeedId(t *testing.T) {
	// swap in a deterministic signing key; restore afterward
	saved := verifyServerKeysInstance
	defer SetVerifyServerKeys(saved)
	SetVerifyServerKeys([]*VerifyServerKey{makeTestVerifyKey(0, 0x11)})

	const anchorA = "203.0.113.7"
	const anchorB = "198.51.100.9"

	// determinism: same anchor → same id across repeated calls
	idA1 := verifySyntheticSeedId(anchorA)
	idA2 := verifySyntheticSeedId(anchorA)
	if idA1 != idA2 {
		t.Fatalf("synthetic seed id not stable for the same anchor: %s != %s", idA1, idA2)
	}

	// distinctness: different anchor → different id
	if idB := verifySyntheticSeedId(anchorB); idA1 == idB {
		t.Fatalf("synthetic seed id collided across distinct anchors: %s", idA1)
	}

	// 16-byte, non-zero, valid UUID that round-trips through ParseId
	if got := len(idA1.Bytes()); got != 16 {
		t.Fatalf("synthetic seed id must be 16 bytes, got %d", got)
	}
	if idA1 == (server.Id{}) {
		t.Fatalf("synthetic seed id must be non-zero")
	}
	parsed, err := server.ParseId(idA1.String())
	if err != nil {
		t.Fatalf("synthetic seed id is not a valid UUID string %q: %s", idA1.String(), err)
	}
	if parsed != idA1 {
		t.Fatalf("synthetic seed id did not round-trip: %s != %s", parsed, idA1)
	}

	// secret dependence: a different signing seed yields a different id for the
	// same anchor, so the id cannot be precomputed without the server key
	SetVerifyServerKeys([]*VerifyServerKey{makeTestVerifyKey(0, 0x22)})
	if idOtherSecret := verifySyntheticSeedId(anchorA); idA1 == idOtherSecret {
		t.Fatalf("synthetic seed id did not depend on the server signing seed")
	}
}

// TestVerifySourceIpPrecedence pins the source-ip precedence the whole /verify
// subsystem's soundness rests on (V11). ClientAddress must resolve from
// X-UR-Forwarded-For, else X-Forwarded-For + X-Forwarded-Source-Port, else
// RemoteAddr. nginx force-overwrites these with $remote_addr:$remote_port at
// ingress (warp/warpctl/config.go), so a normal-ingress caller cannot spoof the
// source; this test guards the app-side precedence against silent regression.
// It does not (and cannot) test nginx — that stays a deploy/runbook check.
func TestVerifySourceIpPrecedence(t *testing.T) {
	newReq := func() *http.Request {
		req := httptest.NewRequest("POST", "/verify", nil)
		req.RemoteAddr = "10.9.9.9:5555"
		return req
	}
	addr := func(req *http.Request) string {
		cs, err := session.NewClientSessionFromRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		return cs.ClientAddress
	}

	// 1. X-UR-Forwarded-For wins over everything
	req := newReq()
	req.Header.Set("X-UR-Forwarded-For", "203.0.113.1:1111")
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	req.Header.Set("X-Forwarded-Source-Port", "2222")
	if got := addr(req); got != "203.0.113.1:1111" {
		t.Fatalf("X-UR-Forwarded-For must win, got %q", got)
	}

	// 2. without X-UR-Forwarded-For: X-Forwarded-For + X-Forwarded-Source-Port
	req = newReq()
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	req.Header.Set("X-Forwarded-Source-Port", "2222")
	if got := addr(req); got != "198.51.100.2:2222" {
		t.Fatalf("X-Forwarded-For+port expected, got %q", got)
	}

	// 3. X-Forwarded-For WITHOUT the source port does not take effect: the code
	// requires both, so it falls through to RemoteAddr rather than trusting a
	// partial spoofed header
	req = newReq()
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	if got := addr(req); got != "10.9.9.9:5555" {
		t.Fatalf("X-Forwarded-For without port must fall through to RemoteAddr, got %q", got)
	}

	// 4. no forwarding headers → RemoteAddr
	if got := addr(newReq()); got != "10.9.9.9:5555" {
		t.Fatalf("RemoteAddr fallback expected, got %q", got)
	}
}

// TestVerifyClampM covers the pure §5.5 depth clamp.
func TestVerifyClampM(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, connect.VerifyMDefault},
		{1, connect.VerifyMMin},
		{connect.VerifyMMin - 1, connect.VerifyMMin},
		{connect.VerifyMMin, connect.VerifyMMin},
		{10, 10},
		{connect.VerifyMMax, connect.VerifyMMax},
		{connect.VerifyMMax + 1, connect.VerifyMMax},
		{connect.VerifyMMax + 100, connect.VerifyMMax},
	}
	for _, c := range cases {
		if got := verifyClampM(c.in); got != c.want {
			t.Errorf("verifyClampM(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestVerifyCachedResponseRoundTrip covers the pure §4.3 cached-response
// envelope: an ASSIGN and a FINAL each survive encode→decode to their concrete
// wire shape.
func TestVerifyCachedResponseRoundTrip(t *testing.T) {
	assign := &connect.VerifyAssignResult{
		TrailId:     connect.Id(server.NewId()),
		ServerNonce: make([]byte, connect.VerifyNonceSize),
		Trail:       []connect.Id{connect.Id(server.NewId())},
		NextHop:     connect.Id(server.NewId()),
		M:           connect.VerifyMDefault,
		ServerKeyId: 0,
		AssignSig:   make([]byte, ed25519.SignatureSize),
	}
	decoded, err := verifyDecodeCachedResponse(
		verifyEncodeCachedResponse(&verifyCachedResponse{Assign: assign}),
	)
	if err != nil {
		t.Fatal(err)
	}
	gotAssign, ok := decoded.(*connect.VerifyAssignResult)
	if !ok {
		t.Fatalf("expected *connect.VerifyAssignResult, got %T", decoded)
	}
	if gotAssign.TrailId != assign.TrailId {
		t.Fatalf("assign trail id mismatch: %s != %s", gotAssign.TrailId, assign.TrailId)
	}

	final := &connect.VerifyFinalResult{Status: connect.VerifyStatusComplete}
	decodedFinal, err := verifyDecodeCachedResponse(
		verifyEncodeCachedResponse(&verifyCachedResponse{Final: final}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodedFinal.(*connect.VerifyFinalResult); !ok {
		t.Fatalf("expected *connect.VerifyFinalResult, got %T", decodedFinal)
	}
}
