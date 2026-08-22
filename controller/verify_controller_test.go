package controller

// verify_controller_test.go — pure-logic unit tests for the `/verify`
// controller: the parts exercisable without redis/pg. The redis-backed behavior
// (V1 poison/real EXTEND parity, V3 concurrent-EXTEND lock, V4/V5 rate meters)
// needs the pg+redis `test.sh` harness to validate end-to-end.

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

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

// TestVerifySeedRejectsMissingSignatureBeforeState is the regression for the
// fail-open shape reported in RaoFoundation/bittensor#3392. A missing
// signature must fail before rate counters, provider lookup, validator-key
// lookup, or any other Redis/PostgreSQL state is consulted.
func TestVerifySeedRejectsMissingSignatureBeforeState(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	args := &VerifyArgs{
		ClientId: server.NewId(), Vpk: public,
		ClientNonce: make([]byte, connect.VerifyNonceSize), SeedSig: nil, M: connect.VerifyMMin,
	}
	clientSession := session.NewLocalClientSession(context.Background(), "127.0.0.1:40000", nil)
	if _, err := verifySeed(args, clientSession); err == nil || !strings.Contains(err.Error(), "seed_sig must be 64 bytes") {
		t.Fatalf("missing seed signature did not fail closed at input shape: %v", err)
	}
}

func TestVerifyEvidenceRangeIsBounded(t *testing.T) {
	to := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	from, gotTo, limit, err := verifyEvidenceRange(&GetVerifyEvidenceArgs{To: to}, 100, 1000)
	if err != nil || !gotTo.Equal(to) || !from.Equal(to.Add(-31*24*time.Hour)) || limit != 100 {
		t.Fatalf("from=%s to=%s limit=%d err=%v", from, gotTo, limit, err)
	}
	if _, _, _, err := verifyEvidenceRange(&GetVerifyEvidenceArgs{From: to.Add(-94 * 24 * time.Hour), To: to}, 1, 10); err == nil {
		t.Fatal("unbounded public evidence range accepted")
	}
	if _, _, _, err := verifyEvidenceRange(&GetVerifyEvidenceArgs{From: to.Add(-time.Hour), To: to, Limit: 11}, 1, 10); err == nil {
		t.Fatal("oversize public evidence limit accepted")
	}
}

func TestVerifyKeyRotationSignsNewestAndPublishesHistoricalKeys(t *testing.T) {
	saved := verifyServerKeysInstance
	defer SetVerifyServerKeys(saved)
	oldKey := makeTestVerifyKey(0, 0x31)
	newKey := makeTestVerifyKey(1, 0x32)
	SetVerifyServerKeys([]*VerifyServerKey{newKey, oldKey})

	if verifySigningKey() != newKey || verifyServerKeyById(0) != oldKey || verifyServerKeyById(1) != newKey || verifyServerKeyById(99) != newKey {
		t.Fatal("verify key rotation selection did not preserve newest/historical semantics")
	}
	result, err := GetVerifyKeys(nil)
	if err != nil || len(result.Keys) != 2 || result.Keys[0].ServerKeyId != 1 || result.Keys[1].ServerKeyId != 0 {
		t.Fatalf("published keys = %+v, %v", result, err)
	}
	if string(result.Keys[0].PublicKey) != string(newKey.PrivateKey.Public().(ed25519.PublicKey)) || string(result.Keys[1].PublicKey) != string(oldKey.PrivateKey.Public().(ed25519.PublicKey)) {
		t.Fatal("published rotation keys do not match the configured signing keys")
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
// subsystem's soundness rests on. Forwarded headers are honored only for an
// explicitly trusted immediate peer; nginx also force-overwrites them at the
// edge, giving attribution two independent boundaries.
func TestVerifySourceIpPrecedence(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.9.9.9/32")}
	newReq := func() *http.Request {
		req := httptest.NewRequest("POST", "/verify", nil)
		req.RemoteAddr = "10.9.9.9:5555"
		return req
	}
	addr := func(req *http.Request) (string, error) {
		return session.ResolveClientAddress(req, trusted)
	}
	mustAddr := func(req *http.Request) string {
		address, err := addr(req)
		if err != nil {
			t.Fatal(err)
		}
		return address
	}

	// 1. X-UR-Forwarded-For wins over everything
	req := newReq()
	req.Header.Set("X-UR-Forwarded-For", "203.0.113.1:1111")
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	req.Header.Set("X-Forwarded-Source-Port", "2222")
	if got := mustAddr(req); got != "203.0.113.1:1111" {
		t.Fatalf("X-UR-Forwarded-For must win, got %q", got)
	}

	// 2. without X-UR-Forwarded-For: X-Forwarded-For + X-Forwarded-Source-Port
	req = newReq()
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	req.Header.Set("X-Forwarded-Source-Port", "2222")
	if got := mustAddr(req); got != "198.51.100.2:2222" {
		t.Fatalf("X-Forwarded-For+port expected, got %q", got)
	}

	// 3. A trusted proxy sending X-Forwarded-For WITHOUT the source port is the
	// ordinary case, not an error: stock nginx
	// (proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for), ALB,
	// CloudFront and Cloudflare all send a bare ip and no port header. It must
	// resolve to the CLIENT.
	//
	// This case used to assert the opposite -- that a bare X-Forwarded-For is
	// rejected -- on the reasoning that rejecting is safer than silently
	// re-attributing the request to the proxy. The premise was right and the
	// conclusion was wrong: the rejection is an ERROR, and router.wrap /
	// router.wrapWithInput turn a session construction failure into HTTP 500 on
	// every endpoint, so "reject" meant the api was down for any deployment
	// that enumerated a normal proxy. Attribution is preserved by reading the
	// client out of the header, which is what this now checks. The port is
	// genuinely unknown, and 0 is how that is spelled; every /verify hash keys
	// on the ip alone (model.ParseVerifyEgressIp, server.ClientIpHash).
	req = newReq()
	req.Header.Set("X-Forwarded-For", "198.51.100.2")
	if got := mustAddr(req); got != "198.51.100.2:0" {
		t.Fatalf("bare X-Forwarded-For from a trusted proxy resolved as %q, want 198.51.100.2:0", got)
	}

	// 3b. The safety half the old assertion was protecting is unchanged: a
	// header the service genuinely cannot read is never re-attributed to
	// anything the caller chose. It falls back to the immediate peer -- the
	// most restrictive answer available -- and the running service emits an
	// operator report naming that peer (pinned in
	// session.TestUnreadableForwardingHeaderFromATrustedPeerDegradesAndIsReported,
	// which can reach the log seam from inside that package).
	req = newReq()
	req.Header.Set("X-Forwarded-For", "not-an-address")
	if got := mustAddr(req); got != "10.9.9.9:5555" {
		t.Fatalf("an unreadable forwarding header resolved as %q, want the peer 10.9.9.9:5555", got)
	}

	// 3c. And a chain is read as a chain: the entry the trusted hop appended,
	// never the entry the client prepended.
	req = newReq()
	req.Header.Set("X-Forwarded-For", "6.6.6.6, 198.51.100.2")
	if got := mustAddr(req); got != "198.51.100.2:0" {
		t.Fatalf(
			"a client behind the trusted proxy prepended X-Forwarded-For: 6.6.6.6 and "+
				"/verify attributed the request to %q; it just chose its own egress "+
				"identity",
			got,
		)
	}

	// 4. no forwarding headers → RemoteAddr
	if got := mustAddr(newReq()); got != "10.9.9.9:5555" {
		t.Fatalf("RemoteAddr fallback expected, got %q", got)
	}

	// 5. an untrusted direct caller cannot spoof either forwarding form.
	req = newReq()
	req.RemoteAddr = "198.18.0.4:4444"
	req.Header.Set("X-UR-Forwarded-For", "203.0.113.99:1")
	if got, err := session.ResolveClientAddress(req, trusted); err != nil || got != "198.18.0.4:4444" {
		t.Fatalf("untrusted forwarding spoof resolved as %q, %v", got, err)
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
