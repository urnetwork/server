package server

// The internal gate's credential scheme (QD-027, QD-055): a bearer verified by its digest to its class, current
// and previous alike, and every file that cannot be used refused with a reason that names no bearer.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// A fresh bearer in its wire form, and its digest in hex.
func newTestInternalGateBearer(t testing.TB) (string, string) {
	t.Helper()
	raw := make([]byte, DefaultInternalGateSettings().BearerByteCount)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	bearer := internalGateBearerEncoding.EncodeToString(raw)
	digest := sha256.Sum256([]byte(bearer))
	return bearer, hex.EncodeToString(digest[:])
}

func TestInternalGateVerifierNamesEachBearersClass(t *testing.T) {
	evsCurrent, evsCurrentDigest := newTestInternalGateBearer(t)
	evsPrevious, evsPreviousDigest := newTestInternalGateBearer(t)
	tlsedge, tlsedgeDigest := newTestInternalGateBearer(t)
	stranger, _ := newTestInternalGateBearer(t)
	verifier, err := NewInternalGateVerifier([]byte(
		"evs: {class: evs, sha256: [" + evsCurrentDigest + ", " + evsPreviousDigest + "]}\n" +
			"tlsedge-1: {class: tlsedge, sha256: [" + tlsedgeDigest + "]}\n",
	))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		authorization string
		caller        InternalGateCaller
		ok            bool
	}{
		{authorization: "Bearer " + evsCurrent, caller: InternalGateCallerEvs, ok: true},
		// a rotation: the previous bearer verifies until the file drops it
		{authorization: "Bearer " + evsPrevious, caller: InternalGateCallerEvs, ok: true},
		{authorization: "Bearer " + tlsedge, caller: InternalGateCallerTlsedge, ok: true},
		{authorization: "Bearer " + stranger},
		{authorization: ""},
		{authorization: "Bearer "},
		{authorization: "Basic " + evsCurrent},
		{authorization: "bearer " + evsCurrent},
		// a bearer's digest is no bearer
		{authorization: "Bearer " + evsCurrentDigest},
	}
	for _, c := range cases {
		request := httptest.NewRequest("GET", InternalGatePathPrefix+"over-limit", nil)
		if c.authorization != "" {
			request.Header.Set("Authorization", c.authorization)
		}
		caller, ok := verifier.Authenticate(request)
		if ok != c.ok || caller != c.caller {
			t.Errorf("%q: %q %t, want %q %t", c.authorization, caller, ok, c.caller, c.ok)
		}
	}
}

func TestInternalGateVerifierRefusesFilesItCannotUse(t *testing.T) {
	_, digest := newTestInternalGateBearer(t)
	files := []string{
		"",
		"evs: {class: dns, sha256: [" + digest + "]}\n",
		"evs: {class: evs, sha256: []}\n",
		"evs: {class: evs}\n",
		"evs: {class: evs, sha256: [" + digest[:20] + "]}\n",
		"evs: {class: evs, sha256: [not-hex]}\n",
		"evs: {class: evs, sha256: [" + digest + "], extra: 1}\n",
		"- evs\n",
	}
	for _, file := range files {
		if _, err := NewInternalGateVerifier([]byte(file)); !errors.Is(err, ErrInternalGateCredentialRefused) {
			t.Errorf("%q: %v", file, err)
		}
	}
}

func TestInternalGateCredentialSendsItsCurrentBearer(t *testing.T) {
	current, _ := newTestInternalGateBearer(t)
	previous, _ := newTestInternalGateBearer(t)
	bearer, err := ParseInternalGateCredential([]byte("current: " + current + "\nprevious: " + previous + "\n"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, bearer, current)
	bearer, err = ParseInternalGateCredential([]byte("current: " + current + "\n"))
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, bearer, current)

	short := internalGateBearerEncoding.EncodeToString(make([]byte, DefaultInternalGateSettings().BearerByteCount-1))
	refused := []string{
		"",
		"previous: " + previous + "\n",
		"current: " + short + "\n",
		"current: " + current + "\nprevious: " + short + "\n",
		"current: " + current + "=\n",
		"current: " + strings.ReplaceAll(current, current[:1], "+") + "\n",
		"current: " + current + "\nnext: " + previous + "\n",
	}
	for _, file := range refused {
		bearer, err := ParseInternalGateCredential([]byte(file))
		if !errors.Is(err, ErrInternalGateCredentialRefused) || bearer != "" {
			t.Errorf("%q: %q %v", file, bearer, err)
			continue
		}
		// the reason names the part, never a bearer
		if strings.Contains(err.Error(), current) || strings.Contains(err.Error(), previous) {
			t.Errorf("a refusal quoted a bearer: %v", err)
		}
	}
}

// The class list is the gate's own: each class it lists, once each and none named as the verify file, has an entry
// api's verifier takes as that class's and a file of its own name its services load, and a name it does not list
// has neither. The edge and platform listener classes, which sidecarctl's bearer keygen draws first, are listed.
func TestInternalGateCallersAreTheClassesTheGateTakes(t *testing.T) {
	callers := InternalGateCallers()
	for _, caller := range []InternalGateCaller{InternalGateCallerEdge, InternalGateCallerMxedgePlatform} {
		if !slices.Contains(callers, caller) {
			t.Errorf("%q is not listed", caller)
		}
	}
	fileNames := map[string]bool{}
	for _, caller := range callers {
		fileName := InternalGateCredentialFileName(caller)
		if fileNames[fileName] || fileName == InternalGateVerifyFileName {
			t.Errorf("%q is listed twice or named as the verify file", caller)
		}
		fileNames[fileName] = true
		current, digest := newTestInternalGateBearer(t)
		verifier, err := NewInternalGateVerifier([]byte(string(caller) + ": {class: " + string(caller) + ", sha256: [" + digest + "]}\n"))
		if err != nil {
			t.Fatalf("%s: %v", caller, err)
		}
		request := httptest.NewRequest("GET", InternalGatePathPrefix+"over-limit", nil)
		request.Header.Set("Authorization", "Bearer "+current)
		if verified, ok := verifier.Authenticate(request); !ok || verified != caller {
			t.Errorf("%s: verified as %q %t", caller, verified, ok)
		}
		pop := Vault.PushSimpleResource(fileName, []byte("current: "+current+"\n"))
		bearer, err := LoadInternalGateCredential(caller)
		pop()
		if err != nil || bearer != current {
			t.Errorf("%s: its file does not load: %v", caller, err)
		}
	}
	for _, stranger := range []InternalGateCaller{"", "dns", "verify", "EDGE", "../pg"} {
		if stranger.known() {
			t.Errorf("%q is known", stranger)
		}
		_, digest := newTestInternalGateBearer(t)
		if _, err := NewInternalGateVerifier([]byte("stranger: {class: \"" + string(stranger) + "\", sha256: [" + digest + "]}\n")); !errors.Is(err, ErrInternalGateCredentialRefused) {
			t.Errorf("%q: an entry of it answered %v", stranger, err)
		}
	}
}

// A file that does not parse is refused with the line the decoder names and none of its words, which quote the start
// of a value: a bearer written alone, where the file's format wants a map, or as an entry of the verify file.
func TestInternalGateRefusalsQuoteNoValue(t *testing.T) {
	bearer, digest := newTestInternalGateBearer(t)
	_, err := ParseInternalGateCredential([]byte(bearer + "\n"))
	if !errors.Is(err, ErrInternalGateCredentialRefused) || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("a bearer alone answered %v", err)
	}
	if strings.Contains(err.Error(), bearer[:7]) {
		t.Fatal("the refusal quotes the bearer's start")
	}
	for _, file := range []string{
		"evs: " + bearer + "\n",
		"evs: {class: evs, sha256: [" + digest + "]}\ngkm: " + bearer + "\n",
	} {
		_, err := NewInternalGateVerifier([]byte(file))
		if !errors.Is(err, ErrInternalGateCredentialRefused) || !strings.Contains(err.Error(), "line ") {
			t.Fatalf("a bearer as an entry answered %v", err)
		}
		if strings.Contains(err.Error(), bearer[:7]) {
			t.Fatal("the refusal quotes the bearer's start")
		}
	}
	// a file that is not YAML at all is refused alike
	if _, err := ParseInternalGateCredential([]byte("current: [" + bearer + "\n")); !errors.Is(err, ErrInternalGateCredentialRefused) || strings.Contains(err.Error(), bearer[:7]) {
		t.Fatalf("a file that is not YAML answered %v", err)
	}
}

// A process reads its own file from its vault, and a vault without one holds no credential.
func TestInternalGateFilesLoadFromTheVault(t *testing.T) {
	current, digest := newTestInternalGateBearer(t)
	if _, err := LoadInternalGateCredential(InternalGateCallerGkm); !errors.Is(err, ErrInternalGateCredentialRefused) {
		t.Fatalf("no file answered %v", err)
	}
	if _, err := LoadInternalGateVerifier(); !errors.Is(err, ErrInternalGateCredentialRefused) {
		t.Fatalf("no verify file answered %v", err)
	}
	popCredential := Vault.PushSimpleResource(InternalGateCredentialFileName(InternalGateCallerGkm), []byte("current: "+current+"\n"))
	defer popCredential()
	popVerify := Vault.PushSimpleResource(InternalGateVerifyFileName, []byte("gkm: {class: gkm, sha256: ["+digest+"]}\n"))
	defer popVerify()
	bearer, err := LoadInternalGateCredential(InternalGateCallerGkm)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, bearer, current)
	verifier, err := LoadInternalGateVerifier()
	connect.AssertEqual(t, err, nil)
	request := httptest.NewRequest("GET", InternalGatePathPrefix+"over-limit", nil)
	request.Header.Set("Authorization", "Bearer "+bearer)
	caller, ok := verifier.Authenticate(request)
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, caller, InternalGateCallerGkm)
	// a name that is no caller reads no file
	if _, err := LoadInternalGateCredential(InternalGateCaller("../pg")); !errors.Is(err, ErrInternalGateCredentialRefused) {
		t.Fatalf("a stranger's name answered %v", err)
	}
}
