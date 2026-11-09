package oauth

// rev/ objects' signatures (revocation_object.go; §7.8): signed under the svc-
// set, verified by the svc- verifier alone, never confused with a service
// token either way, and read back by sidecar.RevocationChecker's format.

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func newTestRevocationPayload(t testing.TB) []byte {
	t.Helper()
	payload, err := sidecar.EncodeRevocationPayload(&sidecar.RevocationRecord{
		NetworkId: sidecar.Id(server.NewId()),
		Kind:      sidecar.RevocationKindClient,
		SubjectId: sidecar.Id(server.NewId()),
		NotBefore: time.Unix(1767225600, 0),
		Reason:    sidecar.RevocationReasonRemoved,
		Time:      time.Unix(1767225601, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// A signed object verifies to its payload's exact bytes, with the svc- kid and
// the rev/ type in its header.
func TestRevocationObjectRoundTrip(t *testing.T) {
	keys := installTestSignerKeys(t)
	payload := newTestRevocationPayload(t)
	object, err := SignRevocation(payload)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := base64.RawURLEncoding.DecodeString(strings.Split(string(object), ".")[0])
	if expected := `{"alg":"ES256","kid":"` + keys.service.Kid + `","typ":"ur-rev+jwt"}`; string(header) != expected {
		t.Fatalf("header %s", header)
	}
	verified, err := newTestServiceTokenVerifier(t).VerifyRevocation(object)
	if err != nil {
		t.Fatal(err)
	}
	if string(verified) != string(payload) {
		t.Fatalf("payload %s", verified)
	}
	t.Cleanup(Testing_SetServiceTokenVerifier(newTestServiceTokenVerifier(t)))
	verified, err = VerifyRevocation(object)
	if err != nil || string(verified) != string(payload) {
		t.Fatalf("the process's verifier: %v", err)
	}
	if _, err := sidecar.ParseRevocationPayload(verified); err != nil {
		t.Fatal(err)
	}
}

// Disjoint sets and types: an object signed by another set's key, an object
// with a service token's type, a service token, and a tampered object all
// fail; and a rev/ object never verifies as a service token.
func TestRevocationObjectRefusals(t *testing.T) {
	keys := installTestSignerKeys(t)
	verifier := newTestServiceTokenVerifier(t)
	payload := newTestRevocationPayload(t)

	for _, otherKey := range []*SignerKey{keys.admin, keys.id, keys.mcp} {
		object, err := signRevocationWith(otherKey, payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.VerifyRevocation(object); !errors.Is(err, ErrInvalidRevocationObject) {
			t.Errorf("signed by %q's key: %v", otherKey.Set, err)
		}
	}

	object, err := SignRevocation(payload)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(object), ".")
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(append([]byte(" "), payload...)) + "." + parts[2]
	if _, err := verifier.VerifyRevocation([]byte(tampered)); !errors.Is(err, ErrInvalidRevocationObject) {
		t.Errorf("a tampered payload: %v", err)
	}
	jwtHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"ES256","kid":"` + keys.service.Kid + `","typ":"JWT"}`))
	if _, err := verifier.VerifyRevocation([]byte(jwtHeader + "." + parts[1] + "." + parts[2])); !errors.Is(err, ErrInvalidRevocationObject) {
		t.Errorf("a JWT's type: %v", err)
	}

	token, _, err := MintServiceToken(newTestServiceTokenArgs())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.VerifyRevocation([]byte(token)); !errors.Is(err, ErrInvalidRevocationObject) {
		t.Errorf("a service token read as a rev/ object: %v", err)
	}
	if _, err := verifier.Verify(string(object), "evs"); !errors.Is(err, ErrInvalidServiceToken) {
		t.Errorf("a rev/ object verified as a service token: %v", err)
	}
	if _, err := verifier.VerifyRevocation([]byte(strings.Repeat("a", sidecar.RevocationObjectMaxByteCount+1))); !errors.Is(err, ErrInvalidRevocationObject) {
		t.Errorf("an oversized object: %v", err)
	}
}

// A rotation keeps objects signed by the older svc- key verifying while it is
// listed, as service tokens do.
func TestRevocationObjectSurvivesRotation(t *testing.T) {
	keys := installTestSignerKeys(t)
	payload := newTestRevocationPayload(t)
	object, err := SignRevocation(payload)
	if err != nil {
		t.Fatal(err)
	}
	newer := newTestSignerKey(t, SignerKeySetService)
	t.Cleanup(Testing_SetSignerKeys(newer, keys.service, keys.mcp, keys.admin, keys.id))
	if _, err := newTestServiceTokenVerifier(t).VerifyRevocation(object); err != nil {
		t.Fatalf("an older svc- key's object after a rotation: %v", err)
	}
	rotated, err := SignRevocation(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustDecodeHeader(t, rotated)), newer.Kid) {
		t.Fatal("a new object is not signed by the newest key")
	}
}

func mustDecodeHeader(t testing.TB, object []byte) []byte {
	t.Helper()
	header, err := base64.RawURLEncoding.DecodeString(strings.Split(string(object), ".")[0])
	if err != nil {
		t.Fatal(err)
	}
	return header
}

// End to end on T2's store: api's writer signs with the svc- set, and evs's
// checker, verifying with VerifyRevocation, refuses a removed client's token
// once its 5 s cache reads again, and honors one issued after not_before.
func TestRevocationObjectFromApiWriterVoidsAtEvs(t *testing.T) {
	installTestSignerKeys(t)
	// the process's verifier, read once, holds whichever keys a test before
	// this one installed
	t.Cleanup(Testing_SetServiceTokenVerifier(newTestServiceTokenVerifier(t)))
	ctx := context.Background()
	clock := sidecartest.NewFakeClock(time.Unix(1767225600, 0))
	blobs := sidecartest.NewFakeBlobs(clock, nil, nil, nil)
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	writerSettings := model.DefaultSidecarRevSettings()
	writerSettings.Clock = clock
	writer := model.NewSidecarRevStore(cluster, SignRevocation, writerSettings)
	checkerSettings := sidecar.DefaultRevocationSettings()
	checkerSettings.Clock = clock
	checker := sidecar.NewRevocationChecker(blobs, VerifyRevocation, checkerSettings)

	networkId := server.NewId()
	clientId := server.NewId()
	principal := &sidecar.Principal{
		NetworkId: [16]byte(networkId),
		ClientId:  [16]byte(clientId),
		Audience:  "evs",
		IssuedAt:  clock.Now(),
	}
	if err := checker.Check(ctx, principal); err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Second)
	if err := writer.Write(ctx, &model.SidecarRevocation{
		NetworkId: networkId,
		Kind:      sidecar.RevocationKindClient,
		SubjectId: clientId,
		NotBefore: sidecar.RevocationNotBefore(clock.Now()),
		Reason:    sidecar.RevocationReasonRemoved,
	}); err != nil {
		t.Fatal(err)
	}
	clock.Advance(checkerSettings.CacheTimeout)
	if err := checker.Check(ctx, principal); !errors.Is(err, sidecar.ErrRevocationVoidsToken) {
		t.Fatalf("a removed client's token at evs: %v", err)
	}
	principal.IssuedAt = clock.Now()
	if err := checker.Check(ctx, principal); err != nil {
		t.Fatalf("a token issued after not_before: %v", err)
	}
}
