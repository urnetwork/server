package controller

// Signed-byte admission is checked on the production seed path before its
// settings/state boundary. A local sentinel joins the call without services.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Fixed owned key/nonce bytes make the signature alias independent of randomness
// or configuration. The same JSON decoder as the router retains the requested
// integer; conversion here deliberately constructs malformed signed requests.
func newVerifySeedAdmissionArgs(t *testing.T, depth int) *VerifyArgs {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	nonce := make([]byte, connect.VerifyNonceSize)
	for index := range nonce {
		nonce[index] = byte(index + 31)
	}
	message, err := connect.BuildVerifySeedMessage(publicKey, nonce, byte(depth))
	if err != nil {
		t.Fatal(err)
	}
	args := &VerifyArgs{ClientId: server.Id{7}, Vpk: publicKey, ClientNonce: nonce, SeedSig: connect.SignVerifyMessage(privateKey, message), M: depth}
	raw := verifySeedAdmissionRequestBytes(t, args)
	var decoded VerifyArgs
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.M != depth || !bytes.Equal(raw, verifySeedAdmissionRequestBytes(t, &decoded)) || !connect.VerifyVerifyMessageSignature(decoded.Vpk, message, decoded.SeedSig) {
		t.Fatalf("JSON request lost its exact M%d integer or genuine signature", depth)
	}
	return &decoded
}

// Observe exact caller-owned request bytes as well as the unchanged integer and
// signature. No production helper is allowed to normalize the signed request.
func verifySeedAdmissionRequestBytes(t *testing.T, args *VerifyArgs) []byte {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A real M4 signature also verifies the byte-truncated M260 message, even
// though the controller's effective depths differ. M260 must never enter state.
func TestVerifySeedAdmissionRejectsSignedDepthAlias(t *testing.T) {
	args := newVerifySeedAdmissionArgs(t, 4)
	message, err := connect.BuildVerifySeedMessage(args.Vpk, args.ClientNonce, byte(args.M))
	if err != nil || !connect.VerifyVerifyMessageSignature(args.Vpk, message, args.SeedSig) {
		t.Fatalf("legal signature fixture failed: %v", err)
	}
	signature := bytes.Clone(args.SeedSig)
	args.M = 260
	raw := verifySeedAdmissionRequestBytes(t, args)
	var decoded VerifyArgs
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	args = &decoded
	alias, err := connect.BuildVerifySeedMessage(args.Vpk, args.ClientNonce, byte(args.M))
	if err != nil || args.M != 260 || !bytes.Equal(signature, args.SeedSig) || !bytes.Equal(message, alias) || !connect.VerifyVerifyMessageSignature(args.Vpk, alias, args.SeedSig) || verifyClampM(4) != 4 || verifyClampM(args.M) != 16 {
		t.Fatalf("fixture did not establish genuine M4/M260 signed-byte alias: %v", err)
	}
	before := verifySeedAdmissionRequestBytes(t, args)
	entered := false
	stop := errors.New("test-owned seed admission stop")
	clientSession := session.NewLocalClientSession(context.Background(), "127.0.0.1:40000", nil)
	t.Cleanup(clientSession.Cancel)
	result, err := verifySeedWithAdmission(args, clientSession, func() error { entered = true; return stop })
	if result != nil || (entered && err != stop) || !bytes.Equal(before, verifySeedAdmissionRequestBytes(t, args)) {
		t.Fatalf("admission fixture did not preserve the exact request and sentinel: %v", err)
	}
	if entered {
		t.Fatal("oversize signed-depth alias crossed admission before configuration or state")
	}
	if err == nil || !strings.Contains(err.Error(), "M must be at most 255") {
		t.Fatalf("signed-depth alias did not receive its shape refusal: %v", err)
	}
}

// The whole out-of-byte range must refuse, not just the demonstrated alias or
// one wrapping boundary. These requests are genuinely signed after truncation.
func TestVerifySeedAdmissionRejectsAllOversizeDepths(t *testing.T) {
	var admittedDepths []int
	for _, depth := range []int{256, 260, 511, 512, int(^uint(0) >> 1)} {
		args := newVerifySeedAdmissionArgs(t, depth)
		before := verifySeedAdmissionRequestBytes(t, args)
		entered := false
		stop := errors.New("test-owned seed admission stop")
		clientSession := session.NewLocalClientSession(context.Background(), "127.0.0.1:40000", nil)
		t.Cleanup(clientSession.Cancel)
		result, err := verifySeedWithAdmission(args, clientSession, func() error { entered = true; return stop })
		if result != nil || (entered && err != stop) || !bytes.Equal(before, verifySeedAdmissionRequestBytes(t, args)) {
			t.Fatalf("depth %d admission fixture changed request or sentinel: %v", depth, err)
		}
		if entered {
			admittedDepths = append(admittedDepths, depth)
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "M must be at most 255") {
			t.Errorf("depth %d did not receive its shape refusal: %v", depth, err)
		}
	}
	if len(admittedDepths) != 0 {
		t.Fatalf("oversize seed depth crossed admission before configuration or state: M=%v", admittedDepths)
	}
}

// Byte-sized requests remain legal shapes even above the effective M16 clamp.
// In particular requested0 and17..255 are not silently rewritten before crypto.
func TestVerifySeedAdmissionPreservesLegalByteDepths(t *testing.T) {
	for _, test := range []struct{ requested, effective int }{
		{requested: 0, effective: connect.VerifyMDefault},
		{requested: 1, effective: 4},
		{requested: 3, effective: 4},
		{requested: 4, effective: 4},
		{requested: 8, effective: 8},
		{requested: 16, effective: 16},
		{requested: 17, effective: 16},
		{requested: 116, effective: 16},
		{requested: 255, effective: 16},
	} {
		args := newVerifySeedAdmissionArgs(t, test.requested)
		before := verifySeedAdmissionRequestBytes(t, args)
		entries := 0
		stop := errors.New("test-owned seed admission stop")
		clientSession := session.NewLocalClientSession(context.Background(), "127.0.0.1:40000", nil)
		t.Cleanup(clientSession.Cancel)
		result, err := verifySeedWithAdmission(args, clientSession, func() error {
			entries++
			message, err := connect.BuildVerifySeedMessage(args.Vpk, args.ClientNonce, byte(args.M))
			if err != nil || args.M != test.requested || message[len(message)-1] != byte(test.requested) || !connect.VerifyVerifyMessageSignature(args.Vpk, message, args.SeedSig) || verifyClampM(args.M) != test.effective {
				return errors.New("legal request bytes or effective clamp changed")
			}
			return stop
		})
		if result != nil || err != stop || entries != 1 || !bytes.Equal(before, verifySeedAdmissionRequestBytes(t, args)) {
			t.Fatalf("legal byte-sized M%d failed exact admission: entries=%d err=%v", test.requested, entries, err)
		}
	}
}

// Negative integers retain the existing shape refusal rather than entering the
// same truncation path. All signed/caller-owned bytes remain unchanged.
func TestVerifySeedAdmissionRetainsNegativeDepthRefusal(t *testing.T) {
	for _, depth := range []int{-1, -256, -int(^uint(0)>>1) - 1} {
		args := newVerifySeedAdmissionArgs(t, depth)
		before := verifySeedAdmissionRequestBytes(t, args)
		entered := false
		clientSession := session.NewLocalClientSession(context.Background(), "127.0.0.1:40000", nil)
		t.Cleanup(clientSession.Cancel)
		result, err := verifySeedWithAdmission(args, clientSession, func() error { entered = true; return errors.New("unexpected negative-depth admission") })
		if result != nil || entered || err == nil || !strings.Contains(err.Error(), "M must be non-negative") || !bytes.Equal(before, verifySeedAdmissionRequestBytes(t, args)) {
			t.Fatalf("negative M%d did not retain shape refusal: %v", depth, err)
		}
	}
}

// Every fixed-width field refuses before the same observer. A sentinel is
// supplied even on these negative paths so fixture errors never reach services.
func TestVerifySeedAdmissionRejectsMalformedMessageWidths(t *testing.T) {
	for _, field := range []struct {
		name  string
		width int
		set   func(*VerifyArgs, []byte)
	}{
		{name: "vpk", width: ed25519.PublicKeySize, set: func(args *VerifyArgs, raw []byte) { args.Vpk = raw }},
		{name: "client_nonce", width: connect.VerifyNonceSize, set: func(args *VerifyArgs, raw []byte) { args.ClientNonce = raw }},
		{name: "seed_sig", width: ed25519.SignatureSize, set: func(args *VerifyArgs, raw []byte) { args.SeedSig = raw }},
	} {
		for _, width := range []int{0, field.width - 1, field.width + 1} {
			args := newVerifySeedAdmissionArgs(t, 4)
			field.set(args, make([]byte, width))
			before := verifySeedAdmissionRequestBytes(t, args)
			entered := false
			clientSession := session.NewLocalClientSession(context.Background(), "127.0.0.1:40000", nil)
			t.Cleanup(clientSession.Cancel)
			result, err := verifySeedWithAdmission(args, clientSession, func() error { entered = true; return errors.New("unexpected malformed-message admission") })
			if result != nil || entered || err == nil || !strings.Contains(err.Error(), field.name+" must be") || !bytes.Equal(before, verifySeedAdmissionRequestBytes(t, args)) {
				t.Fatalf("malformed %s width%d did not refuse before admission: %v", field.name, width, err)
			}
		}
	}
}

// Existing fixed-width errors retain precedence even when the requested depth
// is also malformed. No new integer guard may hide a missing signature.
func TestVerifySeedAdmissionPreservesShapeErrorPrecedence(t *testing.T) {
	for _, depth := range []int{-1, 260} {
		for _, malformed := range []struct {
			field string
			clear func(*VerifyArgs)
		}{
			{field: "vpk", clear: func(args *VerifyArgs) { args.Vpk, args.ClientNonce, args.SeedSig = nil, nil, nil }},
			{field: "client_nonce", clear: func(args *VerifyArgs) { args.ClientNonce, args.SeedSig = nil, nil }},
			{field: "seed_sig", clear: func(args *VerifyArgs) { args.SeedSig = nil }},
		} {
			args := newVerifySeedAdmissionArgs(t, depth)
			malformed.clear(args)
			before := verifySeedAdmissionRequestBytes(t, args)
			entered := false
			clientSession := session.NewLocalClientSession(context.Background(), "127.0.0.1:40000", nil)
			t.Cleanup(clientSession.Cancel)
			result, err := verifySeedWithAdmission(args, clientSession, func() error { entered = true; return errors.New("unexpected malformed-shape admission") })
			if result != nil || entered || err == nil || !strings.Contains(err.Error(), malformed.field+" must be") || !bytes.Equal(before, verifySeedAdmissionRequestBytes(t, args)) {
				t.Fatalf("M%d with malformed %s did not retain shape-error precedence: %v", depth, malformed.field, err)
			}
		}
	}
}

// Requests sharing an effective clamp still sign their different requested
// byte values. Default0 must likewise not acquire M8's signed-message identity.
func TestVerifySeedAdmissionKeepsRequestedDepthSignature(t *testing.T) {
	for _, test := range []struct{ first, second int }{
		{first: 0, second: 8}, {first: 1, second: 3}, {first: 16, second: 17},
		{first: 17, second: 116}, {first: 116, second: 255},
	} {
		first := newVerifySeedAdmissionArgs(t, test.first)
		second := newVerifySeedAdmissionArgs(t, test.second)
		firstMessage, firstErr := connect.BuildVerifySeedMessage(first.Vpk, first.ClientNonce, byte(first.M))
		secondMessage, secondErr := connect.BuildVerifySeedMessage(second.Vpk, second.ClientNonce, byte(second.M))
		if firstErr != nil || secondErr != nil || bytes.Equal(firstMessage, secondMessage) || verifyClampM(first.M) != verifyClampM(second.M) {
			t.Fatalf("distinct requested-byte fixture failed: %v/%v", firstErr, secondErr)
		}
		if !connect.VerifyVerifyMessageSignature(first.Vpk, firstMessage, first.SeedSig) || !connect.VerifyVerifyMessageSignature(second.Vpk, secondMessage, second.SeedSig) || connect.VerifyVerifyMessageSignature(first.Vpk, secondMessage, first.SeedSig) || connect.VerifyVerifyMessageSignature(second.Vpk, firstMessage, second.SeedSig) {
			t.Fatalf("requested M%d and M%d lost separate signed identities", first.M, second.M)
		}
	}
}
